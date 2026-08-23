package account

import (
	"context"
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
}

type Repository interface {
	ActiveUserByEmail(context.Context, string) (User, error)
	CreateRefreshSession(context.Context, string, string, [32]byte, time.Time) (string, error)
	RotateRefreshSession(context.Context, [32]byte, [32]byte, time.Time) (User, string, error)
	RevokeRefreshSession(context.Context, [32]byte) error
	ListDeviceSessions(context.Context, string) ([]DeviceSession, error)
	RevokeDeviceSession(context.Context, string, string) error
}

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
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
	oldHash, newHash [32]byte,
	newExpiresAt time.Time,
) (User, string, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return User{}, "", err
	}
	defer tx.Rollback(ctx)

	var user User
	var deviceName, sessionID, familyID string
	var revokedAt *time.Time
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
        SELECT account.id::text, account.email, account.password_hash,
               account.role, session.device_name, session.id::text,
               session.session_family_id::text, session.revoked_at, session.expires_at
        FROM user_sessions AS session
        JOIN users AS account ON account.id = session.user_id
        WHERE session.refresh_token_sha256 = $1
          AND account.state = 'active' AND account.deleted_at IS NULL
        FOR UPDATE`, oldHash[:],
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &deviceName, &sessionID, &familyID, &revokedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", err
	}
	if revokedAt != nil || !expiresAt.After(time.Now()) {
		if _, err := tx.Exec(ctx, `
            UPDATE user_sessions
            SET revoked_at = COALESCE(revoked_at, now()), reused_at = now(), last_used_at = now()
            WHERE session_family_id = $1::uuid AND revoked_at IS NULL`, familyID); err != nil {
			return User{}, "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE user_sessions SET reused_at = now(), last_used_at = now() WHERE id = $1::uuid`, sessionID); err != nil {
			return User{}, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return User{}, "", err
		}
		return User{}, "", ErrRefreshReplay
	}
	if _, err := tx.Exec(ctx, `UPDATE user_sessions SET revoked_at = now(), last_used_at = now() WHERE id = $1::uuid`, sessionID); err != nil {
		return User{}, "", err
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
		return User{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", err
	}
	return user, nextSessionID, nil
}

func (r *PostgresRepository) RevokeRefreshSession(ctx context.Context, tokenHash [32]byte) error {
	_, err := r.pool.Exec(ctx, `
        UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE refresh_token_sha256 = $1`, tokenHash[:])
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
	command, err := r.pool.Exec(ctx, `
        UPDATE user_sessions
        SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE id = $1::uuid AND user_id = $2::uuid AND revoked_at IS NULL`, sessionID, userID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidCredentials
	}
	return nil
}
