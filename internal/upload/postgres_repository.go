package upload

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

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
    COALESCE(transport_resource_id, ''), COALESCE(final_storage_key, ''), expires_at,
    COALESCE(asset_id::text, ''), COALESCE(verification_claim_token::text, '')`

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
		&session.AssetID,
		&session.VerificationClaim,
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

	// Serialize admission only, not TUS PATCH traffic. `pg_advisory_xact_lock`
	// keeps the reservation calculation correct across simultaneous creators.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(70420260823)`); err != nil {
		return Session{}, false, err
	}

	session, lookupErr := scanSession(tx.QueryRow(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions
        WHERE owner_id = $1::uuid AND client_asset_id = $2`, input.OwnerID, input.ClientAssetID))
	if lookupErr == nil {
		if session.OriginalFilename != input.OriginalFilename ||
			session.MediaType != input.MediaType ||
			session.ExpectedSize != input.ExpectedSize ||
			!bytes.Equal(session.ClientSHA256[:], input.ClientSHA256[:]) {
			return Session{}, false, ErrConflict
		}
		if session.State == StateExpired {
			if _, err := tx.Exec(ctx, `
                UPDATE upload_sessions
                SET state = 'created', received_size = 0, server_sha256 = NULL,
                    transport_resource_id = NULL, final_storage_key = NULL,
                    asset_id = NULL, last_error_code = NULL, expires_at = $2,
                    updated_at = now()
                WHERE id = $1::uuid`, session.ID, input.ExpiresAt); err != nil {
				return Session{}, false, err
			}
			if _, err := tx.Exec(ctx, `
                INSERT INTO upload_events (upload_session_id, owner_id, event_type)
                VALUES ($1::uuid, $2::uuid, 'resumed')`, session.ID, session.OwnerID); err != nil {
				return Session{}, false, err
			}
			session, err = scanSession(tx.QueryRow(ctx, `SELECT `+sessionColumns+` FROM upload_sessions WHERE id = $1::uuid`, session.ID))
			if err != nil {
				return Session{}, false, err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return Session{}, false, err
		}
		return session, false, nil
	}
	if !errors.Is(lookupErr, ErrNotFound) {
		return Session{}, false, lookupErr
	}

	var quotaBytes *int64
	if err := tx.QueryRow(ctx, `SELECT quota_bytes FROM users WHERE id = $1::uuid FOR UPDATE`, input.OwnerID).Scan(&quotaBytes); err != nil {
		return Session{}, false, err
	}
	if input.MaxActiveSessions > 0 {
		var activeCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM upload_sessions WHERE owner_id = $1::uuid AND state IN ('created','uploading','received','verifying','verified','committing')`, input.OwnerID).Scan(&activeCount); err != nil {
			return Session{}, false, err
		}
		if activeCount >= input.MaxActiveSessions {
			return Session{}, false, ErrSessionLimit
		}
	}
	// Quota represents visible unique assets plus unique content still on its
	// way to becoming an asset. Historical available upload sessions must not
	// consume quota a second time, and two in-flight retries of the same digest
	// reserve one eventual logical asset rather than N copies.
	var ownerUsed, ownerReserved int64
	if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(byte_size), 0)
			FROM assets
			WHERE owner_id = $1::uuid AND deleted_at IS NULL`, input.OwnerID).Scan(&ownerUsed); err != nil {
		return Session{}, false, err
	}
	if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(reserved_size), 0)
			FROM (
				SELECT MAX(session.expected_size) AS reserved_size
				FROM upload_sessions AS session
				WHERE session.owner_id = $1::uuid
				  AND session.state IN ('created', 'uploading', 'received', 'verifying', 'verified', 'committing')
				  AND NOT EXISTS (
					SELECT 1 FROM assets AS asset
					WHERE asset.owner_id = session.owner_id
					  AND asset.content_sha256 = session.client_sha256
					  AND asset.deleted_at IS NULL
				  )
				GROUP BY session.client_sha256
			) AS reservations`, input.OwnerID).Scan(&ownerReserved); err != nil {
		return Session{}, false, err
	}
	var inputAlreadyReserved bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM assets
			WHERE owner_id = $1::uuid AND content_sha256 = $2 AND deleted_at IS NULL
			UNION ALL
			SELECT 1 FROM upload_sessions
			WHERE owner_id = $1::uuid AND client_sha256 = $2
			  AND state IN ('created', 'uploading', 'received', 'verifying', 'verified', 'committing')
		)`, input.OwnerID, input.ClientSHA256[:]).Scan(&inputAlreadyReserved); err != nil {
		return Session{}, false, err
	}
	inputQuotaReservation := input.ExpectedSize
	if inputAlreadyReserved {
		inputQuotaReservation = 0
	}
	if quotaBytes != nil && inputQuotaReservation > *quotaBytes-ownerUsed-ownerReserved {
		return Session{}, false, ErrInsufficientStorage
	}

	// Files already received are reflected in Statfs free bytes. Only bytes
	// that have not yet been persisted need a second reservation. received_size
	// is updated by tus progress callbacks, so any lag is conservative rather
	// than allowing an overcommit.
	var activeReserved int64
	if err := tx.QueryRow(ctx, `
			SELECT COALESCE(SUM(expected_size - received_size), 0)
            FROM upload_sessions
			WHERE state IN ('created', 'uploading')`).Scan(&activeReserved); err != nil {
		return Session{}, false, err
	}
	if input.AvailableBytes < input.MinimumFreeBytes || input.ExpectedSize > input.AvailableBytes-input.MinimumFreeBytes-activeReserved {
		return Session{}, false, ErrInsufficientStorage
	}

	session, err = scanSession(tx.QueryRow(ctx, `
        INSERT INTO upload_sessions (
            owner_id, client_asset_id, original_filename, media_type,
            expected_size, client_sha256, transport, expires_at
        ) VALUES ($1::uuid, $2, $3, $4, $5, $6, 'tus-v1', $7)
        RETURNING `+sessionColumns,
		input.OwnerID, input.ClientAssetID, input.OriginalFilename, input.MediaType,
		input.ExpectedSize, input.ClientSHA256[:], input.ExpiresAt,
	))
	if err != nil {
		return Session{}, false, err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
        VALUES ($1::uuid, $2::uuid, 'created')`, session.ID, session.OwnerID); err != nil {
		return Session{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Session{}, false, err
	}
	return session, true, nil
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

func (r *PostgresRepository) ClaimVerification(ctx context.Context, workerID string, lease time.Duration, limit int) ([]Session, error) {
	if workerID == "" || lease <= 0 || limit <= 0 {
		return nil, ErrInvalidState
	}
	rows, err := r.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id, state AS previous_state
			FROM upload_sessions
			WHERE state IN ('received', 'verifying', 'verified', 'committing')
			  AND (verification_lease_until IS NULL OR verification_lease_until <= now())
			ORDER BY updated_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $3
		), claimed AS (
			UPDATE upload_sessions AS session
			SET state = CASE WHEN candidates.previous_state = 'received' THEN 'verifying' ELSE session.state END,
				verification_worker_id = $1,
				verification_claim_token = uuidv7(),
				verification_claimed_at = now(),
				verification_lease_until = now() + $2::bigint * interval '1 microsecond',
				updated_at = now()
			FROM candidates
			WHERE session.id = candidates.id
			RETURNING session.id, session.owner_id, candidates.previous_state
		), events AS (
			INSERT INTO upload_events (upload_session_id, owner_id, event_type)
			SELECT id, owner_id, 'verification_started' FROM claimed WHERE previous_state = 'received'
		)
		SELECT `+sessionColumns+` FROM upload_sessions
		WHERE id IN (SELECT id FROM claimed)
		ORDER BY verification_claimed_at, id`, workerID, lease.Microseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	claimed := make([]Session, 0, limit)
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, session)
	}
	return claimed, rows.Err()
}

func (r *PostgresRepository) RenewVerificationLease(ctx context.Context, id, workerID, claim string, lease time.Duration) error {
	if workerID == "" || claim == "" || lease <= 0 {
		return ErrInvalidState
	}
	command, err := r.pool.Exec(ctx, `
		UPDATE upload_sessions
		SET verification_lease_until = now() + $3::bigint * interval '1 microsecond', updated_at = now()
		WHERE id = $1::uuid AND verification_worker_id = $2 AND verification_claim_token::text = $4
		  AND state IN ('verifying', 'verified', 'committing')`, id, workerID, lease.Microseconds(), claim)
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
	if fence := VerificationFence(ctx); fence != "" && session.VerificationClaim != fence {
		return Session{}, ErrInvalidState
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
	fence := VerificationFence(ctx)
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'verified', server_sha256 = $2, verified_at = now(), updated_at = now()
			WHERE id = $1::uuid AND state = 'verifying'
			  AND ($3 = '' OR verification_claim_token::text = $3)
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
		SELECT id, owner_id, 'verified' FROM changed`, id, hash[:], fence)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) MarkCommitting(ctx context.Context, id, storageKey string) error {
	fence := VerificationFence(ctx)
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'committing', final_storage_key = $2, updated_at = now()
			WHERE id = $1::uuid AND state IN ('verified', 'committing')
			  AND ($3 = '' OR verification_claim_token::text = $3)
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
		SELECT id, owner_id, 'commit_started' FROM changed`, id, storageKey, fence)
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
		if fence := VerificationFence(ctx); fence != "" && session.VerificationClaim != fence {
			return ErrInvalidState
		}
		return tx.Commit(ctx)
	}
	if session.State != StateCommitting || !bytes.Equal(session.ClientSHA256[:], hash[:]) || (VerificationFence(ctx) != "" && session.VerificationClaim != VerificationFence(ctx)) {
		return ErrInvalidState
	}
	var assetID string
	err = tx.QueryRow(ctx, `
        INSERT INTO assets (
            owner_id, upload_session_id, storage_key, original_filename,
            media_type, byte_size, content_sha256
        ) VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7)
        ON CONFLICT (owner_id, content_sha256) DO NOTHING
        RETURNING id::text`,
		session.OwnerID, session.ID, storageKey, session.OriginalFilename,
		session.MediaType, session.ExpectedSize, hash[:]).Scan(&assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		var existingStorageKey string
		err = tx.QueryRow(ctx, `
            SELECT id::text, storage_key FROM assets
            WHERE owner_id = $1::uuid AND content_sha256 = $2 AND deleted_at IS NULL
	            FOR SHARE`, session.OwnerID, hash[:]).Scan(&assetID, &existingStorageKey)
		if err != nil {
			return err
		}
		if existingStorageKey != storageKey || storageKey != session.FinalStorageKey {
			return fmt.Errorf("deduplicated asset has inconsistent storage key: %w", ErrInvalidState)
		}
	} else if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
        UPDATE upload_sessions
        SET state = 'available', server_sha256 = $2, final_storage_key = $3,
            asset_id = $4::uuid, committed_at = now(), verification_worker_id = NULL,
            verification_claimed_at = NULL, verification_claim_token = NULL,
            verification_lease_until = NULL, updated_at = now()
	        WHERE id = $1::uuid AND ($5 = '' OR verification_claim_token::text = $5)`, id, hash[:], storageKey, assetID, VerificationFence(ctx))
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
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
        SET state = 'quarantined', server_sha256 = $2, last_error_code = $3,
            verification_worker_id = NULL, verification_claimed_at = NULL,
            verification_claim_token = NULL, verification_lease_until = NULL, updated_at = now()
        WHERE id = $1::uuid AND state NOT IN ('available', 'quarantined')
          AND ($4 = '' OR verification_claim_token::text = $4)
        RETURNING owner_id::text`, id, hash[:], errorCode, VerificationFence(ctx)).Scan(&ownerID)
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
	fence := VerificationFence(ctx)
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
        UPDATE upload_sessions
        SET state = 'failed', last_error_code = $2, verification_worker_id = NULL,
            verification_claimed_at = NULL, verification_claim_token = NULL,
            verification_lease_until = NULL, updated_at = now()
            WHERE id = $1::uuid AND state NOT IN ('available', 'quarantined')
              AND ($3 = '' OR verification_claim_token::text = $3)
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type, error_code)
		SELECT id, owner_id, 'failed', $2 FROM changed`, id, errorCode, fence)
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

func (r *PostgresRepository) ExpiredSessions(ctx context.Context, now time.Time, limit int) ([]Session, error) {
	rows, err := r.pool.Query(ctx, `
        SELECT `+sessionColumns+` FROM upload_sessions
        WHERE state IN ('created', 'uploading', 'failed') AND expires_at <= $1
        ORDER BY expires_at, id
        LIMIT $2`, now, limit)
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

func (r *PostgresRepository) MarkExpired(ctx context.Context, id string) error {
	command, err := r.pool.Exec(ctx, `
        WITH changed AS (
            UPDATE upload_sessions
            SET state = 'expired', updated_at = now()
            WHERE id = $1::uuid AND state IN ('created', 'uploading', 'failed')
            RETURNING id, owner_id
        )
        INSERT INTO upload_events (upload_session_id, owner_id, event_type)
        SELECT id, owner_id, 'expired' FROM changed`, id)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrInvalidState
	}
	return nil
}

func (r *PostgresRepository) ResetForRetry(ctx context.Context, id, ownerID string) (Session, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Session{}, err
	}
	defer tx.Rollback(ctx)
	session, err := scanSession(tx.QueryRow(ctx, `
		SELECT `+sessionColumns+` FROM upload_sessions
		WHERE id = $1::uuid AND owner_id = $2::uuid FOR UPDATE`, id, ownerID))
	if err != nil {
		return Session{}, err
	}
	if (session.State != StateCreated && session.State != StateUploading) || session.ReceivedSize >= session.ExpectedSize {
		return Session{}, ErrInvalidState
	}
	if _, err := tx.Exec(ctx, `
		UPDATE upload_sessions
		SET state = 'created', received_size = 0, transport_resource_id = NULL,
			last_error_code = NULL, updated_at = now()
		WHERE id = $1::uuid`, id); err != nil {
		return Session{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO upload_events (upload_session_id, owner_id, event_type)
		VALUES ($1::uuid, $2::uuid, 'resumed')`, id, ownerID); err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	return r.SessionByID(ctx, id)
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
