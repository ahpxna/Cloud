package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAccountNotFound is returned by operator actions for an unknown email.
var ErrAccountNotFound = errors.New("account not found")

// AccountSummary is the operator view of an account. It never includes the
// password hash, MFA secret or refresh-token material.
type AccountSummary struct {
	ID            string
	Email         string
	Role          string
	State         string
	MFAEnabled    bool
	ActiveDevices int
	CreatedAt     time.Time
	DeletedAt     *time.Time
}

// AdminRepository implements the operator account lifecycle. It runs as the
// photo_cloud_admin database role, which can read account metadata and revoke
// sessions but cannot read password hashes or mint sessions.
type AdminRepository struct {
	pool *pgxpool.Pool
}

func NewAdminRepository(pool *pgxpool.Pool) *AdminRepository {
	return &AdminRepository{pool: pool}
}

func (r *AdminRepository) CreateUser(ctx context.Context, email, passwordHash, role string) (string, error) {
	if role != "member" && role != "admin" {
		return "", fmt.Errorf("invalid role %q", role)
	}
	normalized, ok := normalizeEmail(email)
	if !ok {
		return "", fmt.Errorf("invalid email %q", email)
	}
	var id string
	err := r.pool.QueryRow(ctx, `
        INSERT INTO users (email, password_hash, role, state)
        VALUES ($1, $2, $3, 'active')
        RETURNING id::text`, normalized, passwordHash, role).Scan(&id)
	return id, err
}

func (r *AdminRepository) ListUsers(ctx context.Context) ([]AccountSummary, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT account.id::text, account.email, account.role, account.state,
               EXISTS (SELECT 1 FROM user_mfa_totp AS mfa
                       WHERE mfa.user_id = account.id AND mfa.confirmed_at IS NOT NULL),
               (SELECT count(*) FROM device_sessions AS device
                WHERE device.user_id = account.id AND device.revoked_at IS NULL AND device.expires_at > now()),
               account.created_at, account.deleted_at
        FROM users AS account
        ORDER BY account.created_at, account.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := []AccountSummary{}
	for rows.Next() {
		var summary AccountSummary
		if err := rows.Scan(&summary.ID, &summary.Email, &summary.Role, &summary.State, &summary.MFAEnabled,
			&summary.ActiveDevices, &summary.CreatedAt, &summary.DeletedAt); err != nil {
			return nil, err
		}
		accounts = append(accounts, summary)
	}
	return accounts, rows.Err()
}

// Disable blocks sign-in and refresh and revokes every device immediately.
// Access tokens already issued stop working at the next request because the
// gateway checks the device session and account state on every call.
func (r *AdminRepository) Disable(ctx context.Context, email string) (int64, error) {
	var revoked int64
	err := r.withAccount(ctx, email, func(tx pgx.Tx, userID, state string) error {
		if state == "deleting" {
			return fmt.Errorf("account is being deleted")
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET state = 'disabled', updated_at = now() WHERE id = $1::uuid`, userID); err != nil {
			return err
		}
		var err error
		revoked, err = revokeAccountSessionsTx(ctx, tx, userID)
		return err
	})
	return revoked, err
}

func (r *AdminRepository) Enable(ctx context.Context, email string) error {
	return r.withAccount(ctx, email, func(tx pgx.Tx, userID, state string) error {
		if state == "deleting" {
			return fmt.Errorf("account is being deleted")
		}
		_, err := tx.Exec(ctx, `UPDATE users SET state = 'active', updated_at = now() WHERE id = $1::uuid`, userID)
		return err
	})
}

// ResetPassword replaces the password hash and signs every device out, so a
// stolen refresh token cannot outlive the reset.
func (r *AdminRepository) ResetPassword(ctx context.Context, email, passwordHash string) (int64, error) {
	var revoked int64
	err := r.withAccount(ctx, email, func(tx pgx.Tx, userID, state string) error {
		if state == "deleting" {
			return fmt.Errorf("account is being deleted")
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1::uuid`, userID, passwordHash); err != nil {
			return err
		}
		var err error
		revoked, err = revokeAccountSessionsTx(ctx, tx, userID)
		return err
	})
	return revoked, err
}

func (r *AdminRepository) RevokeSessions(ctx context.Context, email string) (int64, error) {
	var revoked int64
	err := r.withAccount(ctx, email, func(tx pgx.Tx, userID, _ string) error {
		var err error
		revoked, err = revokeAccountSessionsTx(ctx, tx, userID)
		return err
	})
	return revoked, err
}

// ResetMFA removes a lost authenticator and its recovery codes after the
// operator has verified the person out of band, and signs every device out.
func (r *AdminRepository) ResetMFA(ctx context.Context, email string) (int64, error) {
	var revoked int64
	err := r.withAccount(ctx, email, func(tx pgx.Tx, userID, _ string) error {
		for _, statement := range []string{
			`DELETE FROM user_mfa_recovery_codes WHERE user_id = $1::uuid`,
			`DELETE FROM user_mfa_totp WHERE user_id = $1::uuid`,
			`DELETE FROM mfa_action_throttles WHERE user_id = $1::uuid`,
		} {
			if _, err := tx.Exec(ctx, statement, userID); err != nil {
				return err
			}
		}
		var err error
		revoked, err = revokeAccountSessionsTx(ctx, tx, userID)
		return err
	})
	return revoked, err
}

// MarkDeleted starts account deletion: sign-in stops, every device is revoked
// and MFA material is destroyed. Originals and audit history are kept until the
// operator purge in docs/runbooks/account-lifecycle.md, because they also live
// in encrypted backups with their own retention.
func (r *AdminRepository) MarkDeleted(ctx context.Context, email string) (int64, error) {
	var revoked int64
	err := r.withAccount(ctx, email, func(tx pgx.Tx, userID, _ string) error {
		if _, err := tx.Exec(ctx, `
            UPDATE users SET state = 'deleting', deleted_at = COALESCE(deleted_at, now()), updated_at = now()
            WHERE id = $1::uuid`, userID); err != nil {
			return err
		}
		for _, statement := range []string{
			`DELETE FROM user_mfa_recovery_codes WHERE user_id = $1::uuid`,
			`DELETE FROM user_mfa_totp WHERE user_id = $1::uuid`,
			`DELETE FROM mfa_action_throttles WHERE user_id = $1::uuid`,
		} {
			if _, err := tx.Exec(ctx, statement, userID); err != nil {
				return err
			}
		}
		var err error
		revoked, err = revokeAccountSessionsTx(ctx, tx, userID)
		return err
	})
	return revoked, err
}

func (r *AdminRepository) withAccount(ctx context.Context, email string, action func(pgx.Tx, string, string) error) error {
	normalized, ok := normalizeEmail(email)
	if !ok {
		return ErrAccountNotFound
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userID, state string
	if err := tx.QueryRow(ctx, `
        SELECT id::text, state FROM users WHERE lower(email) = lower($1) FOR UPDATE`, normalized,
	).Scan(&userID, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAccountNotFound
		}
		return err
	}
	if err := action(tx, userID, state); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// revokeAccountSessionsTx revokes every device and refresh generation and
// advances the authentication epoch, which also voids in-flight MFA challenges.
func revokeAccountSessionsTx(ctx context.Context, tx pgx.Tx, userID string) (int64, error) {
	if _, err := tx.Exec(ctx, `
        UPDATE user_sessions SET revoked_at = now(), last_used_at = now()
        WHERE revoked_at IS NULL
          AND device_session_id IN (SELECT id FROM device_sessions WHERE user_id = $1::uuid)`, userID); err != nil {
		return 0, err
	}
	command, err := tx.Exec(ctx, `
        UPDATE device_sessions SET revoked_at = now(), last_used_at = now()
        WHERE user_id = $1::uuid AND revoked_at IS NULL`, userID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET auth_epoch = auth_epoch + 1, updated_at = now() WHERE id = $1::uuid`, userID); err != nil {
		return 0, err
	}
	return command.RowsAffected(), nil
}
