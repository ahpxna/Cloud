package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PostgresRepository struct {
	pool *pgxpool.Pool
}

func NewPostgresRepository(pool *pgxpool.Pool) *PostgresRepository {
	return &PostgresRepository{pool: pool}
}

const sessionColumns = `
    id::text, owner_id::text, client_asset_id, original_filename, media_type,
    expected_size, received_size, client_sha256, server_sha256, state,
    COALESCE(transport_resource_id, ''), COALESCE(final_storage_key, ''), expires_at`

type rowScanner interface {
	Scan(...any) error
}

func scanSession(row rowScanner) (Session, error) {
	var session Session
	var clientHash []byte
	var serverHash []byte
	err := row.Scan(
		&session.ID, &session.OwnerID, &session.ClientAssetID,
		&session.OriginalFilename, &session.MediaType, &session.ExpectedSize,
		&session.ReceivedSize, &clientHash, &serverHash, &session.State,
		&session.TransportResource, &session.FinalStorageKey, &session.ExpiresAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	if len(clientHash) != 32 {
		return Session{}, fmt.Errorf("session %s has invalid client hash length", session.ID)
	}
	copy(session.ClientSHA256[:], clientHash)
	if len(serverHash) > 0 {
		if len(serverHash) != 32 {
			return Session{}, fmt.Errorf("session %s has invalid server hash length", session.ID)
		}
		var hash [32]byte
		copy(hash[:], serverHash)
		session.ServerSHA256 = &hash
	}
	return session, nil
}

func (r *PostgresRepository) CreateSession(ctx context.Context, input CreateSessionInput) (Session, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Session{}, false, err
	}
	defer tx.Rollback(ctx)

	row := tx.QueryRow(ctx, `
        INSERT INTO upload_sessions (
            owner_id, client_asset_id, original_filename, media_type,
            expected_size, client_sha256, transport, expires_at
        ) VALUES ($1::uuid, $2, $3, $4, $5, $6, 'tus-v1', $7)
        ON CONFLICT (owner_id, client_asset_id) DO NOTHING
        RETURNING `+sessionColumns,
		input.OwnerID, input.ClientAssetID, input.OriginalFilename, input.MediaType,
		input.ExpectedSize, input.ClientSHA256[:], input.ExpiresAt,
	)
	session, scanErr := scanSession(row)
	created := scanErr == nil
	if scanErr != nil && !errors.Is(scanErr, ErrNotFound) {
		return Session{}, false, scanErr
	}

	if !created {
		session, err = scanSession(tx.QueryRow(ctx, `
            SELECT `+sessionColumns+` FROM upload_sessions
            WHERE owner_id = $1::uuid AND client_asset_id = $2`,
			input.OwnerID, input.ClientAssetID,
		))
		if err != nil {
			return Session{}, false, err
		}
		if session.OriginalFilename != input.OriginalFilename ||
			session.MediaType != input.MediaType ||
			session.ExpectedSize != input.ExpectedSize ||
			!bytes.Equal(session.ClientSHA256[:], input.ClientSHA256[:]) {
			return Session{}, false, ErrConflict
		}
	} else {
		if _, err := tx.Exec(ctx, `
            INSERT INTO upload_events (upload_session_id, owner_id, event_type)
            VALUES ($1::uuid, $2::uuid, 'created')`, session.ID, session.OwnerID); err != nil {
			return Session{}, false, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, false, err
	}
	return session, created, nil
}

func (r *PostgresRepository) SessionByID(ctx context.Context, id string) (Session, error) {
	return scanSession(r.pool.QueryRow(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions WHERE id = $1::uuid`, id))
}

func (r *PostgresRepository) ClaimTusCreation(ctx context.Context, id, ownerID string, size int64) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'uploading', transport_resource_id = id::text,
                attempt_count = attempt_count + 1, updated_at = now()
            WHERE id = $1::uuid AND owner_id = $2::uuid
              AND expected_size = $3 AND state = 'created'
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type, attempt)
        SELECT id, owner_id, 'upload_started', 1 FROM changed`, id, ownerID, size)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) RecordProgress(ctx context.Context, id string, offset int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var ownerID string
	var previous, expected int64
	var state State
	err = tx.QueryRow(ctx, `
        SELECT owner_id::text, received_size, expected_size, state
        FROM upload_sessions WHERE id = $1::uuid FOR UPDATE`, id,
	).Scan(&ownerID, &previous, &expected, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if state != StateUploading || offset < previous || offset > expected {
		return ErrInvalidState
	}
	if offset == previous {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
        UPDATE upload_sessions SET received_size = $2, updated_at = now()
        WHERE id = $1::uuid`, id, offset); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO upload_events (
            upload_session_id, owner_id, event_type, offset_from, offset_to
        ) VALUES ($1::uuid, $2::uuid, 'bytes_acknowledged', $3, $4)`,
		id, ownerID, previous, offset); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkReceived(ctx context.Context, id string, size int64) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'received', received_size = $2, received_at = now(), updated_at = now()
            WHERE id = $1::uuid AND state = 'uploading' AND expected_size = $2
            RETURNING id, owner_id, received_size
        )
        INSERT INTO upload_events (
            upload_session_id, owner_id, event_type, offset_from, offset_to
        ) SELECT id, owner_id, 'received', 0, received_size FROM changed`, id, size)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) BeginVerification(ctx context.Context, id string) (Session, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)

	session, err := scanSession(tx.QueryRow(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return Session{}, err
	}
	if session.State == StateReceived {
		if _, err := tx.Exec(ctx, `
            UPDATE upload_sessions SET state = 'verifying', updated_at = now()
            WHERE id = $1::uuid`, id); err != nil {
			return Session{}, err
		}
		if _, err := tx.Exec(ctx, `
            INSERT INTO upload_events (upload_session_id, owner_id, event_type)
            VALUES ($1::uuid, $2::uuid, 'verification_started')`, id, session.OwnerID); err != nil {
			return Session{}, err
		}
		session.State = StateVerifying
	}
	switch session.State {
	case StateVerifying, StateVerified, StateCommitting, StateAvailable:
	default:
		return Session{}, ErrInvalidState
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (r *PostgresRepository) MarkVerified(ctx context.Context, id string, hash [32]byte) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'verified', server_sha256 = $2, verified_at = now(), updated_at = now()
            WHERE id = $1::uuid AND state = 'verifying'
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
        SELECT id, owner_id, 'verified' FROM changed`, id, hash[:])
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) MarkCommitting(ctx context.Context, id, storageKey string) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'committing', final_storage_key = $2, updated_at = now()
            WHERE id = $1::uuid AND state IN ('verified', 'committing')
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
        SELECT id, owner_id, 'commit_started' FROM changed`, id, storageKey)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) MarkAvailable(ctx context.Context, id, storageKey string, hash [32]byte) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	session, err := scanSession(tx.QueryRow(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions WHERE id = $1::uuid FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if session.State == StateAvailable {
		return tx.Commit(ctx)
	}
	if session.State != StateCommitting || !bytes.Equal(session.ClientSHA256[:], hash[:]) {
		return ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO assets (
            owner_id, upload_session_id, storage_key, original_filename,
            media_type, byte_size, content_sha256
        ) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
        ON CONFLICT (owner_id, content_sha256) DO NOTHING`,
		session.OwnerID, session.ID, storageKey, session.OriginalFilename,
		session.MediaType, session.ExpectedSize, hash[:]); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        UPDATE upload_sessions
        SET state = 'available', server_sha256 = $2, final_storage_key = $3,
            committed_at = now(), updated_at = now()
        WHERE id = $1::uuid`, id, hash[:], storageKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
        VALUES ($1::uuid, $2::uuid, 'available')`, id, session.OwnerID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkQuarantined(ctx context.Context, id string, hash [32]byte, errorCode string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var ownerID string
	err = tx.QueryRow(ctx, `
        UPDATE upload_sessions
        SET state = 'quarantined', server_sha256 = $2, last_error_code = $3, updated_at = now()
        WHERE id = $1::uuid AND state NOT IN ('available', 'quarantined')
        RETURNING owner_id::text`, id, hash[:], errorCode).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidState
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO upload_events (upload_session_id, owner_id, event_type, error_code)
        VALUES
            ($1::uuid, $2::uuid, 'checksum_mismatch', $3),
            ($1::uuid, $2::uuid, 'quarantined', $3)`, id, ownerID, errorCode); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkFailed(ctx context.Context, id, errorCode string) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'failed', last_error_code = $2, updated_at = now()
            WHERE id = $1::uuid AND state NOT IN ('available', 'quarantined')
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type, error_code)
        SELECT id, owner_id, 'failed', $2 FROM changed`, id, errorCode)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) PendingVerification(ctx context.Context, limit int) ([]Session, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions
        WHERE state IN ('received', 'verifying', 'verified', 'committing')
        ORDER BY updated_at, id
        LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sessions := make([]Session, 0, limit)
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

func (r *PostgresRepository) ListAssets(ctx context.Context, ownerID string, before *AssetCursor, limit int) ([]Asset, error) {
	var cursorCreatedAt any
	var cursorID any
	if before != nil {
		cursorCreatedAt = before.CreatedAt
		cursorID = before.ID
	}
	rows, err := r.pool.Query(ctx, `
        SELECT id::text, owner_id::text, storage_key, original_filename,
               media_type, byte_size, content_sha256, created_at
        FROM assets
        WHERE owner_id = $1::uuid AND deleted_at IS NULL
          AND ($2::timestamptz IS NULL OR (created_at, id) < ($2::timestamptz, $3::uuid))
        ORDER BY created_at DESC, id DESC
        LIMIT $4`, ownerID, cursorCreatedAt, cursorID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	assets := make([]Asset, 0, limit)
	for rows.Next() {
		asset, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (r *PostgresRepository) AssetByID(ctx context.Context, ownerID, assetID string) (Asset, error) {
	return scanAsset(r.pool.QueryRow(ctx, `
        SELECT id::text, owner_id::text, storage_key, original_filename,
               media_type, byte_size, content_sha256, created_at
        FROM assets
        WHERE id = $1::uuid AND owner_id = $2::uuid AND deleted_at IS NULL`, assetID, ownerID))
}

func scanAsset(row rowScanner) (Asset, error) {
	var asset Asset
	var hash []byte
	err := row.Scan(
		&asset.ID, &asset.OwnerID, &asset.StorageKey, &asset.OriginalFilename,
		&asset.MediaType, &asset.ByteSize, &hash, &asset.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, err
	}
	if len(hash) != 32 {
		return Asset{}, fmt.Errorf("asset %s has invalid content hash length", asset.ID)
	}
	copy(asset.ContentSHA256[:], hash)
	return asset, nil
}
