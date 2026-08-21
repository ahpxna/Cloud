package account

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrInvalidCredentials = errors.New("invalid credentials")

type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         string
}

type Repository interface {
	ActiveUserByEmail(context.Context, string) (User, error)
	CreateRefreshSession(context.Context, string, string, [32]byte, time.Time) (string, error)
	RotateRefreshSession(context.Context, [32]byte, [32]byte, time.Time) (User, string, error)
	RevokeRefreshSession(context.Context, [32]byte) error
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
	var deviceName string
	err = tx.QueryRow(ctx, `
        UPDATE user_sessions AS session
        SET revoked_at = now(), last_used_at = now()
        FROM users AS account
        WHERE session.user_id = account.id
          AND session.refresh_token_sha256 = $1
          AND session.revoked_at IS NULL AND session.expires_at > now()
          AND account.state = 'active' AND account.deleted_at IS NULL
        RETURNING account.id::text, account.email, account.password_hash,
                  account.role, session.device_name`, oldHash[:],
	).Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Role, &deviceName)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, "", ErrInvalidCredentials
	}
	if err != nil {
		return User{}, "", err
	}
	var sessionID string
	err = tx.QueryRow(ctx, `
        INSERT INTO user_sessions (user_id, device_name, refresh_token_sha256, expires_at)
        VALUES ($1::uuid, $2, $3, $4)
        RETURNING id::text`, user.ID, deviceName, newHash[:], newExpiresAt,
	).Scan(&sessionID)
	if err != nil {
		return User{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, "", err
	}
	return user, sessionID, nil
}

func (r *PostgresRepository) RevokeRefreshSession(ctx context.Context, tokenHash [32]byte) error {
	_, err := r.pool.Exec(ctx, `
        UPDATE user_sessions SET revoked_at = COALESCE(revoked_at, now()), last_used_at = now()
        WHERE refresh_token_sha256 = $1`, tokenHash[:])
	return err
}
