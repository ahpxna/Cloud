package account

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRefreshReplay      = errors.New("refresh token replay detected")
)

type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
}

type DeviceSession struct {
	ID         string    `json:"id"`
	DeviceName string    `json:"device_name"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Current    bool      `json:"current"`
}

type RefreshRotation struct {
	User                 User
	SessionID            string
	RetryTokenCiphertext []byte
	RetryTokenNonce      []byte
	Retried              bool
}

type Repository interface {
	ActiveUserByEmail(context.Context, string) (User, error)
	CreateRefreshSession(context.Context, string, string, [32]byte, time.Time) (string, error)
	RotateRefreshSession(context.Context, [32]byte, [32]byte, [32]byte, time.Time, []byte, []byte, time.Time) (RefreshRotation, error)
	RevokeRefreshSession(context.Context, [32]byte) error
	ListDeviceSessions(context.Context, string) ([]DeviceSession, error)
	RevokeDeviceSession(context.Context, string, string) error
	SessionActive(context.Context, string, string) (bool, error)
}

// LoginThrottleRepository persists identity throttling independently of the
// gateway process. identityHash must be an HMAC of the normalized login name;
// attacker-controlled plaintext identities are never stored.
type LoginThrottleRepository interface {
	RecordLoginAttempt(context.Context, [32]byte, time.Time, time.Duration, int) (bool, time.Duration, error)
	ClearLoginAttempts(context.Context, [32]byte) error
}

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

func (r *PostgresRepository) SessionActive(ctx context.Context, userID, sessionID string) (bool, error) {
	var active bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM user_sessions
        WHERE id = $1::uuid AND user_id = $2::uuid
          AND revoked_at IS NULL AND expires_at > now()
    )`, sessionID, userID).Scan(&active)
	return active, err
}

func (r *PostgresRepository) ActiveUserByEmail(ctx context.Context, email string) (User, error) {
	var user User
	err := r.pool.QueryRow(ctx, `
        SELECT id::text, email, password_hash, role
        FROM users WHERE lower(email) = lower($1) AND state = 'active' AND deleted_at IS NULL`, email,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	return user, err
}

func (r *PostgresRepository) CreateRefreshSession(
	ctx context.Context,
	userID, deviceName string,
	tokenHash [32]byte,
	expiresAt time.Time,
) (string, error) {
	var sessionID string
	err := r.pool.QueryRow(ctx, `
        INSERT INTO user_sessions (user_id, device_name, refresh_token_sha256, expires_at)
        VALUES ($1::uuid, $2, $3, $4)
        RETURNING id::text`, userID, deviceName, tokenHash[:], expiresAt,
	).Scan(&sessionID)
	return sessionID, err
}

func (r *PostgresRepository) RotateRefreshSession(
	ctx context.Context,
	oldHash, newHash, retryRequestHash [32]byte,
	newExpiresAt time.Time,
	retryCiphertext, retryNonce []byte,
	now time.Time,
) (RefreshRotation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RefreshRotation{}, err
	}
	defer tx.Rollback(ctx)

	// Expired retry ciphertext is useless and should not become a long-lived
	// encrypted archive of refresh secrets. Bound its database lifetime even on
	// active installations without a separate cleanup daemon.
	if _, err := tx.Exec(ctx, `
        UPDATE user_sessions
        SET refresh_retry_request_sha256 = NULL, refresh_retry_ciphertext = NULL,
            refresh_retry_nonce = NULL, refresh_retry_until = NULL
        WHERE refresh_retry_until IS NOT NULL AND refresh_retry_until <= $1`, now); err != nil {
		return RefreshRotation{}, err
	}

	var user User
	var deviceName, sessionID, familyID string
	var revokedAt, retryUntil *time.Time
	var expiresAt time.Time
	var storedRetryRequestHash, storedRetryCiphertext, storedRetryNonce []byte
	err = tx.QueryRow(ctx, `
        SELECT account.id::text, account.email, account.password_hash,
               account.role, session.device_name, session.id::text,
               session.session_family_id::text, session.revoked_at, session.expires_at,
               session.refresh_retry_request_sha256, session.refresh_retry_ciphertext,
               session.refresh_retry_nonce, session.refresh_retry_until
        FROM user_sessions AS session
        JOIN users AS account ON account.id = session.user_id
        WHERE session.refresh_token_sha256 = $1
          AND account.state = 'active' AND account.deleted_at IS NULL
		FOR UPDATE OF session`, oldHash[:],
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &deviceName, &sessionID, &familyID, &revokedAt, &expiresAt, &storedRetryRequestHash, &storedRetryCiphertext, &storedRetryNonce, &retryUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return RefreshRotation{}, ErrInvalidCredentials
	}
	if err != nil {
		return RefreshRotation{}, err
	}
	if revokedAt != nil || !expiresAt.After(now) {
		// A transport failure may hide the successful rotation response from the
		// client. During a very short grace window, return the exact successor
		// token rather than treating the retry as theft. The successor secret is
		// stored only as authenticated ciphertext on the revoked parent row.
		requestIDMatches := len(storedRetryRequestHash) == 32 && subtle.ConstantTimeCompare(storedRetryRequestHash, retryRequestHash[:]) == 1
		if revokedAt != nil && retryUntil != nil && retryUntil.After(now) && requestIDMatches && len(storedRetryCiphertext) != 0 && len(storedRetryNonce) != 0 {
			var nextSessionID string
			err := tx.QueryRow(ctx, `
                SELECT id::text
                FROM user_sessions
                WHERE parent_session_id = $1::uuid
                  AND revoked_at IS NULL AND expires_at > $2
                ORDER BY created_at DESC
                LIMIT 1`, sessionID, now).Scan(&nextSessionID)
			if err == nil {
				if _, err := tx.Exec(ctx, `UPDATE user_sessions SET last_used_at = $2 WHERE id = $1::uuid`, sessionID, now); err != nil {
					return RefreshRotation{}, err
				}
				if err := tx.Commit(ctx); err != nil {
					return RefreshRotation{}, err
				}
				return RefreshRotation{User: user, SessionID: nextSessionID, RetryTokenCiphertext: storedRetryCiphertext, RetryTokenNonce: storedRetryNonce, Retried: true}, nil
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return RefreshRotation{}, err
			}
		}
		if _, err := tx.Exec(ctx, `
            UPDATE user_sessions
            SET revoked_at = COALESCE(revoked_at, $2), reused_at = $2, last_used_at = $2
            WHERE session_family_id = $1::uuid AND revoked_at IS NULL`, familyID, now); err != nil {
			return RefreshRotation{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET reused_at = $2, last_used_at = $2 WHERE id = $1::uuid`, sessionID, now); err != nil {
			return RefreshRotation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return RefreshRotation{}, err
		}
		return RefreshRotation{}, ErrRefreshReplay
	}
	hasRetryRequest := retryRequestHash != ([32]byte{})
	if hasRetryRequest != (len(retryCiphertext) != 0 && len(retryNonce) != 0) {
		return RefreshRotation{}, errors.New("refresh retry request and ciphertext must be provided together")
	}
	if hasRetryRequest {
		if _, err := tx.Exec(ctx, `
            UPDATE user_sessions
            SET revoked_at = $2, last_used_at = $2,
                refresh_retry_request_sha256 = $3, refresh_retry_ciphertext = $4,
                refresh_retry_nonce = $5, refresh_retry_until = $6
            WHERE id = $1::uuid`, sessionID, now, retryRequestHash[:], retryCiphertext, retryNonce, now.Add(refreshRetryGrace)); err != nil {
			return RefreshRotation{}, err
		}
	} else if _, err := tx.Exec(ctx, `
        UPDATE user_sessions SET revoked_at = $2, last_used_at = $2 WHERE id = $1::uuid`, sessionID, now); err != nil {
		return RefreshRotation{}, err
	}
	var nextSessionID string
	err = tx.QueryRow(ctx, `
        INSERT INTO user_sessions (
            user_id, device_name, refresh_token_sha256, expires_at,
            session_family_id, parent_session_id
        ) VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6::uuid)
        RETURNING id::text`, user.ID, deviceName, newHash[:], newExpiresAt, familyID, sessionID,
	).Scan(&nextSessionID)
	if err != nil {
		return RefreshRotation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RefreshRotation{}, err
	}
	return RefreshRotation{User: user, SessionID: nextSessionID}, nil
}

func (r *PostgresRepository) RevokeRefreshSession(ctx context.Context, tokenHash [32]byte) error {
	_, err := r.pool.Exec(ctx, `
        WITH target_family AS (
            SELECT session_family_id
            FROM user_sessions
            WHERE refresh_token_sha256 = $1
        )
        UPDATE user_sessions
        SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE session_family_id IN (SELECT session_family_id FROM target_family)`, tokenHash[:])
	return err
}

func (r *PostgresRepository) ListDeviceSessions(ctx context.Context, userID string) ([]DeviceSession, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT id::text, device_name, created_at, last_used_at, expires_at
        FROM user_sessions
        WHERE user_id = $1::uuid AND revoked_at IS NULL AND expires_at > now()
        ORDER BY last_used_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sessions := []DeviceSession{}
	for rows.Next() {
		var session DeviceSession
		if err := rows.Scan(&session.ID, &session.DeviceName, &session.CreatedAt, &session.LastUsedAt, &session.ExpiresAt); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (r *PostgresRepository) RevokeDeviceSession(ctx context.Context, userID, sessionID string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// A refresh rotates one device into a new row. Find the stable family even
	// when the selected generation was already revoked, then revoke every live
	// descendant so a concurrent refresh cannot evade a device revoke.
	var familyID string
	if err := tx.QueryRow(ctx, `
        SELECT session_family_id::text
        FROM user_sessions
        WHERE id = $1::uuid AND user_id = $2::uuid`, sessionID, userID).Scan(&familyID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidCredentials
		}
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE user_sessions
        SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE user_id = $1::uuid AND session_family_id = $2::uuid AND revoked_at IS NULL`, userID, familyID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) RecordLoginAttempt(
	ctx context.Context,
	identityHash [32]byte,
	now time.Time,
	window time.Duration,
	limit int,
) (bool, time.Duration, error) {
	if window <= 0 || limit < 2 {
		return false, 0, errors.New("invalid login throttle configuration")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)

	// Serialize the first insert and all subsequent updates for one normalized
	// identity. Without this lock, two simultaneous first attempts can both
	// observe no row and race on the primary key, turning ordinary concurrency
	// into a fail-closed 503 instead of a counted authentication attempt.
	lockID := int64(binary.BigEndian.Uint64(identityHash[:8]))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockID); err != nil {
		return false, 0, err
	}

	// Keep attacker-controlled identity cardinality time-bounded. The process-wide
	// token bucket limits insertion rate; this indexed cleanup bounds retention.
	if _, err := tx.Exec(ctx, `
        DELETE FROM login_throttles
        WHERE updated_at < $1`, now.Add(-2*window)); err != nil {
		return false, 0, err
	}

	var windowStarted time.Time
	var count int
	var blockedUntil *time.Time
	err = tx.QueryRow(ctx, `
        SELECT window_started_at, attempt_count, blocked_until
        FROM login_throttles
        WHERE identity_hash = $1
        FOR UPDATE`, identityHash[:]).Scan(&windowStarted, &count, &blockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `
            INSERT INTO login_throttles (
                identity_hash, window_started_at, attempt_count, blocked_until, updated_at
            ) VALUES ($1, $2, 1, NULL, $2)`, identityHash[:], now)
		if err != nil {
			return false, 0, err
		}
		return true, 0, tx.Commit(ctx)
	}
	if err != nil {
		return false, 0, err
	}
	if blockedUntil != nil && blockedUntil.After(now) {
		return false, blockedUntil.Sub(now), tx.Commit(ctx)
	}
	if !now.Before(windowStarted.Add(window)) {
		_, err = tx.Exec(ctx, `
            UPDATE login_throttles
            SET window_started_at = $2, attempt_count = 1,
                blocked_until = NULL, updated_at = $2
            WHERE identity_hash = $1`, identityHash[:], now)
		if err != nil {
			return false, 0, err
		}
		return true, 0, tx.Commit(ctx)
	}

	count++
	var nextBlockedUntil *time.Time
	if count >= limit {
		until := windowStarted.Add(window)
		nextBlockedUntil = &until
	}
	_, err = tx.Exec(ctx, `
        UPDATE login_throttles
        SET attempt_count = $2, blocked_until = $3, updated_at = $4
        WHERE identity_hash = $1`, identityHash[:], count, nextBlockedUntil, now)
	if err != nil {
		return false, 0, err
	}
	return true, 0, tx.Commit(ctx)
}

func (r *PostgresRepository) ClearLoginAttempts(ctx context.Context, identityHash [32]byte) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM login_throttles WHERE identity_hash = $1`, identityHash[:])
	return err
}

func (r *PostgresRepository) MFAUserByID(ctx context.Context, userID string) (User, error) {
	var user User
	err := r.pool.QueryRow(ctx, `
        SELECT id::text, email, password_hash, role
        FROM users WHERE id = $1::uuid AND state = 'active' AND deleted_at IS NULL`, userID,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrInvalidCredentials
	}
	return user, err
}

func (r *PostgresRepository) TOTPForUser(ctx context.Context, userID string) (MFARecord, error) {
	var record MFARecord
	err := r.pool.QueryRow(ctx, `
        SELECT encrypted_secret, nonce, confirmed_at, last_used_counter
        FROM user_mfa_totp
        WHERE user_id = $1::uuid`, userID,
	).Scan(&record.EncryptedSecret, &record.Nonce, &record.ConfirmedAt, &record.LastUsedCounter)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFARecord{}, ErrMFANotConfigured
	}
	return record, err
}

func (r *PostgresRepository) SavePendingTOTP(ctx context.Context, userID string, encryptedSecret, nonce []byte) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `
        INSERT INTO user_mfa_totp (user_id, encrypted_secret, nonce, confirmed_at, last_used_counter, updated_at)
        VALUES ($1::uuid, $2, $3, NULL, NULL, now())
        ON CONFLICT (user_id) DO UPDATE
        SET encrypted_secret = EXCLUDED.encrypted_secret,
            nonce = EXCLUDED.nonce,
            confirmed_at = NULL,
            last_used_counter = NULL,
            updated_at = now()
        WHERE user_mfa_totp.confirmed_at IS NULL`, userID, encryptedSecret, nonce)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrMFAInvalid
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_mfa_recovery_codes WHERE user_id = $1::uuid`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = COALESCE(consumed_at, now()) WHERE user_id = $1::uuid AND consumed_at IS NULL`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ConfirmTOTP(ctx context.Context, userID string, counter int64, recoveryHashes [][32]byte) error {
	if len(recoveryHashes) == 0 {
		return ErrMFAInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var confirmedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT confirmed_at FROM user_mfa_totp WHERE user_id = $1::uuid FOR UPDATE`, userID).Scan(&confirmedAt); errors.Is(err, pgx.ErrNoRows) {
		return ErrMFANotConfigured
	} else if err != nil {
		return err
	}
	if confirmedAt != nil {
		return ErrMFAInvalid
	}
	if _, err := tx.Exec(ctx, `
        UPDATE user_mfa_totp
        SET confirmed_at = now(), last_used_counter = $2, updated_at = now()
        WHERE user_id = $1::uuid`, userID, counter); err != nil {
		return err
	}
	if err := replaceRecoveryCodesTx(ctx, tx, userID, recoveryHashes); err != nil {
		return err
	}
	// Enabling MFA changes the account authentication assurance level. Revoke
	// every refresh family issued before the second factor existed so a stolen
	// pre-MFA refresh token cannot keep minting access tokens indefinitely.
	if _, err := tx.Exec(ctx, `
        UPDATE user_sessions
        SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CreateMFAChallenge(
	ctx context.Context,
	userID, deviceName string,
	hash [32]byte,
	now, expiresAt time.Time,
	attempts int,
) error {
	if attempts <= 0 || deviceName == "" || !expiresAt.After(now) {
		return ErrMFAChallenge
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Serialize challenge issuance for a user. This prevents concurrent successful
	// password logins from racing past the durable per-user issuance bound.
	if err := lockUserTransaction(ctx, tx, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMFAChallenge
		}
		return err
	}

	if _, err := tx.Exec(ctx, `
        DELETE FROM mfa_challenges
        WHERE created_at < $1`, now.Add(-mfaChallengeRetention)); err != nil {
		return err
	}

	var recent int
	if err := tx.QueryRow(ctx, `
        SELECT count(*)
        FROM mfa_challenges
        WHERE user_id = $1::uuid AND created_at >= $2`,
		userID, now.Add(-mfaChallengeIssueWindow),
	).Scan(&recent); err != nil {
		return err
	}
	if recent >= mfaChallengeIssueLimit {
		return ErrMFARateLimited
	}

	if _, err := tx.Exec(ctx, `
        INSERT INTO mfa_challenges (
            challenge_hash, user_id, device_name, expires_at, attempts_remaining, created_at
        ) VALUES ($1, $2::uuid, $3, $4, $5, $6)`,
		hash[:], userID, deviceName, expiresAt, attempts, now,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MFAChallengeByHash(ctx context.Context, hash [32]byte, now time.Time) (MFAChallenge, error) {
	var challenge MFAChallenge
	err := r.pool.QueryRow(ctx, `
        SELECT account.id::text, account.email, account.password_hash, account.role,
               challenge.device_name, mfa.encrypted_secret, mfa.nonce,
               mfa.last_used_counter, challenge.expires_at, challenge.attempts_remaining
        FROM mfa_challenges AS challenge
        JOIN users AS account ON account.id = challenge.user_id
        JOIN user_mfa_totp AS mfa ON mfa.user_id = challenge.user_id
        WHERE challenge.challenge_hash = $1
          AND challenge.consumed_at IS NULL
          AND challenge.attempts_remaining > 0
          AND challenge.expires_at > $2
          AND mfa.confirmed_at IS NOT NULL
          AND account.state = 'active' AND account.deleted_at IS NULL`, hash[:], now,
	).Scan(
		&challenge.User.ID, &challenge.User.Email, &challenge.User.PasswordHash, &challenge.User.Role,
		&challenge.DeviceName, &challenge.EncryptedSecret, &challenge.Nonce,
		&challenge.LastUsedCounter, &challenge.ExpiresAt, &challenge.Attempts,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAChallenge{}, ErrMFAChallenge
	}
	return challenge, err
}

func (r *PostgresRepository) FailMFAChallenge(ctx context.Context, hash [32]byte, now time.Time) (int, error) {
	var remaining int
	err := r.pool.QueryRow(ctx, `
        UPDATE mfa_challenges
        SET attempts_remaining = GREATEST(attempts_remaining - 1, 0),
            consumed_at = CASE WHEN attempts_remaining <= 1 THEN $2 ELSE consumed_at END
        WHERE challenge_hash = $1 AND consumed_at IS NULL AND expires_at > $2 AND attempts_remaining > 0
        RETURNING attempts_remaining`, hash[:], now).Scan(&remaining)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrMFAChallenge
	}
	return remaining, err
}

func (r *PostgresRepository) CompleteMFATOTPChallenge(ctx context.Context, hash [32]byte, now time.Time, counter int64) (User, string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return User{}, "", err
	}
	defer tx.Rollback(ctx)

	var user User
	var deviceName string
	var lastCounter *int64
	err = tx.QueryRow(ctx, `
        SELECT account.id::text, account.email, account.password_hash, account.role,
               challenge.device_name, mfa.last_used_counter
        FROM mfa_challenges AS challenge
        JOIN users AS account ON account.id = challenge.user_id
        JOIN user_mfa_totp AS mfa ON mfa.user_id = challenge.user_id
        WHERE challenge.challenge_hash = $1
          AND challenge.consumed_at IS NULL
          AND challenge.attempts_remaining > 0
          AND challenge.expires_at > $2
          AND mfa.confirmed_at IS NOT NULL
          AND account.state = 'active' AND account.deleted_at IS NULL
        FOR UPDATE OF challenge, mfa`, hash[:], now,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &deviceName, &lastCounter)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrMFAChallenge
	}
	if err != nil {
		return User{}, "", err
	}
	if lastCounter != nil && counter <= *lastCounter {
		return User{}, "", ErrMFAReplay
	}
	if _, err := tx.Exec(ctx, `UPDATE user_mfa_totp SET last_used_counter = $2, updated_at = $3 WHERE user_id = $1::uuid`, user.ID, counter, now); err != nil {
		return User{}, "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = $2 WHERE challenge_hash = $1`, hash[:], now); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", err
	}
	return user, deviceName, nil
}

func (r *PostgresRepository) CompleteMFARecoveryChallenge(ctx context.Context, challengeHash, recoveryHash [32]byte, now time.Time) (User, string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return User{}, "", err
	}
	defer tx.Rollback(ctx)

	var user User
	var deviceName string
	err = tx.QueryRow(ctx, `
        SELECT account.id::text, account.email, account.password_hash, account.role, challenge.device_name
        FROM mfa_challenges AS challenge
        JOIN users AS account ON account.id = challenge.user_id
        JOIN user_mfa_totp AS mfa ON mfa.user_id = challenge.user_id
        WHERE challenge.challenge_hash = $1
          AND challenge.consumed_at IS NULL
          AND challenge.attempts_remaining > 0
          AND challenge.expires_at > $2
          AND mfa.confirmed_at IS NOT NULL
          AND account.state = 'active' AND account.deleted_at IS NULL
        FOR UPDATE OF challenge`, challengeHash[:], now,
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrMFAChallenge
	}
	if err != nil {
		return User{}, "", err
	}
	command, err := tx.Exec(ctx, `
        UPDATE user_mfa_recovery_codes
        SET used_at = $3
        WHERE user_id = $1::uuid AND code_hash = $2 AND used_at IS NULL`, user.ID, recoveryHash[:], now)
	if err != nil {
		return User{}, "", err
	}
	if command.RowsAffected() != 1 {
		return User{}, "", ErrMFAInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = $2 WHERE challenge_hash = $1`, challengeHash[:], now); err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", err
	}
	return user, deviceName, nil
}

func (r *PostgresRepository) RecordMFAActionAttempt(
	ctx context.Context,
	userID, action string,
	now time.Time,
	window time.Duration,
	limit int,
) (bool, time.Duration, error) {
	if window <= 0 || limit < 1 || (action != "enroll" && action != "confirm" && action != "recovery" && action != "disable") {
		return false, 0, ErrMFAInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)

	// Serialize first insertion and subsequent mutations by user so parallel
	// requests cannot race around the durable attempt budget.
	if err := lockUserTransaction(ctx, tx, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, 0, ErrInvalidCredentials
		}
		return false, 0, err
	}

	if _, err := tx.Exec(ctx, `
        DELETE FROM mfa_action_throttles
        WHERE updated_at < $1`, now.Add(-mfaActionRetention)); err != nil {
		return false, 0, err
	}

	var windowStarted time.Time
	var count int
	var blockedUntil *time.Time
	err = tx.QueryRow(ctx, `
        SELECT window_started_at, attempt_count, blocked_until
        FROM mfa_action_throttles
        WHERE user_id = $1::uuid AND action = $2
        FOR UPDATE`, userID, action).Scan(&windowStarted, &count, &blockedUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
            INSERT INTO mfa_action_throttles (
                user_id, action, window_started_at, attempt_count, blocked_until, updated_at
            ) VALUES ($1::uuid, $2, $3, 1, NULL, $3)`, userID, action, now); err != nil {
			return false, 0, err
		}
		return true, 0, tx.Commit(ctx)
	}
	if err != nil {
		return false, 0, err
	}
	if blockedUntil != nil && blockedUntil.After(now) {
		return false, blockedUntil.Sub(now), tx.Commit(ctx)
	}
	if !now.Before(windowStarted.Add(window)) {
		if _, err := tx.Exec(ctx, `
            UPDATE mfa_action_throttles
            SET window_started_at = $3, attempt_count = 1,
                blocked_until = NULL, updated_at = $3
            WHERE user_id = $1::uuid AND action = $2`, userID, action, now); err != nil {
			return false, 0, err
		}
		return true, 0, tx.Commit(ctx)
	}

	count++
	var nextBlockedUntil *time.Time
	if count >= limit {
		until := windowStarted.Add(window)
		nextBlockedUntil = &until
	}
	if _, err := tx.Exec(ctx, `
        UPDATE mfa_action_throttles
        SET attempt_count = $3, blocked_until = $4, updated_at = $5
        WHERE user_id = $1::uuid AND action = $2`, userID, action, count, nextBlockedUntil, now); err != nil {
		return false, 0, err
	}
	return true, 0, tx.Commit(ctx)
}

// lockUserTransaction serializes per-user state transitions while keeping the
// gateway role read-only on users. Advisory locks are transaction-scoped and
// are released automatically on both commit and rollback.
func lockUserTransaction(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, userID); err != nil {
		return err
	}
	var existing string
	return tx.QueryRow(ctx, `SELECT id::text FROM users WHERE id = $1::uuid`, userID).Scan(&existing)
}

func (r *PostgresRepository) ClearMFAActionAttempts(ctx context.Context, userID, action string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM mfa_action_throttles WHERE user_id = $1::uuid AND action = $2`, userID, action)
	return err
}

func (r *PostgresRepository) RotateRecoveryCodes(ctx context.Context, userID string, counter int64, hashes [][32]byte) error {
	if len(hashes) == 0 {
		return ErrMFAInvalid
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var lastUsed *int64
	err = tx.QueryRow(ctx, `
        SELECT last_used_counter
        FROM user_mfa_totp
        WHERE user_id = $1::uuid AND confirmed_at IS NOT NULL
        FOR UPDATE`, userID).Scan(&lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMFANotConfigured
	}
	if err != nil {
		return err
	}
	if lastUsed != nil && counter <= *lastUsed {
		return ErrMFAReplay
	}
	if _, err := tx.Exec(ctx, `
        UPDATE user_mfa_totp
        SET last_used_counter = $2, updated_at = now()
        WHERE user_id = $1::uuid`, userID, counter); err != nil {
		return err
	}
	if err := replaceRecoveryCodesTx(ctx, tx, userID, hashes); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func replaceRecoveryCodesTx(ctx context.Context, tx pgx.Tx, userID string, hashes [][32]byte) error {
	if _, err := tx.Exec(ctx, `DELETE FROM user_mfa_recovery_codes WHERE user_id = $1::uuid`, userID); err != nil {
		return err
	}
	for _, hash := range hashes {
		if _, err := tx.Exec(ctx, `INSERT INTO user_mfa_recovery_codes (user_id, code_hash) VALUES ($1::uuid, $2)`, userID, hash[:]); err != nil {
			return err
		}
	}
	return nil
}

func (r *PostgresRepository) DisableMFA(ctx context.Context, userID string, counter int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lastUsed *int64
	err = tx.QueryRow(ctx, `
        SELECT last_used_counter
        FROM user_mfa_totp
        WHERE user_id = $1::uuid AND confirmed_at IS NOT NULL
        FOR UPDATE`, userID).Scan(&lastUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrMFANotConfigured
	}
	if err != nil {
		return err
	}
	if lastUsed != nil && counter <= *lastUsed {
		return ErrMFAReplay
	}
	if _, err := tx.Exec(ctx, `UPDATE mfa_challenges SET consumed_at = COALESCE(consumed_at, now()) WHERE user_id = $1::uuid AND consumed_at IS NULL`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_mfa_recovery_codes WHERE user_id = $1::uuid`, userID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `DELETE FROM user_mfa_totp WHERE user_id = $1::uuid`, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrMFANotConfigured
	}
	if _, err := tx.Exec(ctx, `
        UPDATE user_sessions
        SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
