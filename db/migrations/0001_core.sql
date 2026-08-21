BEGIN;

CREATE TABLE users (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    email text NOT NULL,
    password_hash text NOT NULL,
    role text NOT NULL DEFAULT 'member'
        CHECK (role IN ('admin', 'member')),
    state text NOT NULL DEFAULT 'active'
        CHECK (state IN ('invited', 'active', 'disabled', 'deleting')),
    quota_bytes bigint CHECK (quota_bytes IS NULL OR quota_bytes >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);

CREATE UNIQUE INDEX users_email_unique_ci ON users (lower(email));

CREATE TABLE user_sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users(id),
    device_name text NOT NULL,
    refresh_token_sha256 bytea NOT NULL
        CHECK (octet_length(refresh_token_sha256) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz,
    UNIQUE (refresh_token_sha256)
);

CREATE INDEX user_sessions_active_by_user
    ON user_sessions (user_id, expires_at)
    WHERE revoked_at IS NULL;

CREATE TABLE upload_sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id uuid NOT NULL REFERENCES users(id),
    client_asset_id text NOT NULL,
    original_filename text NOT NULL,
    media_type text NOT NULL,
    expected_size bigint NOT NULL CHECK (expected_size >= 0),
    received_size bigint NOT NULL DEFAULT 0
        CHECK (received_size >= 0 AND received_size <= expected_size),
    client_sha256 bytea NOT NULL CHECK (octet_length(client_sha256) = 32),
    server_sha256 bytea CHECK (
        server_sha256 IS NULL OR octet_length(server_sha256) = 32
    ),
    transport text NOT NULL
        CHECK (transport IN ('tus-v1', 'ietf-resumable')),
    transport_resource_id text,
    state text NOT NULL DEFAULT 'created'
        CHECK (state IN (
            'created', 'uploading', 'received', 'verifying', 'verified',
            'committing', 'available', 'failed', 'expired', 'quarantined'
        )),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    last_error_code text,
    staging_key text,
    final_storage_key text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    received_at timestamptz,
    verified_at timestamptz,
    committed_at timestamptz,
    expires_at timestamptz NOT NULL,
    UNIQUE (owner_id, client_asset_id),
    UNIQUE (transport, transport_resource_id),
    CHECK (state NOT IN ('verified', 'committing', 'available') OR server_sha256 IS NOT NULL),
    CHECK (state <> 'available' OR final_storage_key IS NOT NULL)
);

CREATE INDEX upload_sessions_reconciliation
    ON upload_sessions (state, updated_at)
    WHERE state IN ('received', 'verifying', 'verified', 'committing');

CREATE INDEX upload_sessions_expiration
    ON upload_sessions (expires_at)
    WHERE state IN ('created', 'uploading', 'failed');

CREATE TABLE assets (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id uuid NOT NULL REFERENCES users(id),
    upload_session_id uuid NOT NULL UNIQUE REFERENCES upload_sessions(id),
    storage_key text NOT NULL,
    original_filename text NOT NULL,
    media_type text NOT NULL,
    byte_size bigint NOT NULL CHECK (byte_size >= 0),
    content_sha256 bytea NOT NULL CHECK (octet_length(content_sha256) = 32),
    captured_at timestamptz,
    width integer CHECK (width IS NULL OR width > 0),
    height integer CHECK (height IS NULL OR height > 0),
    duration_ms bigint CHECK (duration_ms IS NULL OR duration_ms >= 0),
    metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    trashed_at timestamptz,
    deleted_at timestamptz,
    UNIQUE (owner_id, storage_key),
    UNIQUE (owner_id, content_sha256)
);

CREATE INDEX assets_timeline
    ON assets (owner_id, captured_at DESC NULLS LAST, id DESC)
    WHERE deleted_at IS NULL;

CREATE TABLE upload_events (
    sequence_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    upload_session_id uuid NOT NULL REFERENCES upload_sessions(id),
    owner_id uuid NOT NULL REFERENCES users(id),
    event_type text NOT NULL CHECK (event_type IN (
        'created', 'upload_started', 'bytes_acknowledged', 'interrupted',
        'resumed', 'received', 'verification_started', 'verified',
        'checksum_mismatch', 'commit_started', 'available', 'failed',
        'expired', 'quarantined', 'reconciled'
    )),
    offset_from bigint CHECK (offset_from IS NULL OR offset_from >= 0),
    offset_to bigint CHECK (offset_to IS NULL OR offset_to >= 0),
    attempt integer CHECK (attempt IS NULL OR attempt >= 0),
    error_code text,
    request_id text,
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    CHECK (
        offset_from IS NULL OR offset_to IS NULL OR offset_to >= offset_from
    )
);

CREATE INDEX upload_events_by_session
    ON upload_events (upload_session_id, sequence_id);

CREATE TABLE asset_integrity_checks (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    asset_id uuid NOT NULL REFERENCES assets(id),
    expected_sha256 bytea NOT NULL CHECK (octet_length(expected_sha256) = 32),
    observed_sha256 bytea CHECK (
        observed_sha256 IS NULL OR octet_length(observed_sha256) = 32
    ),
    result text NOT NULL CHECK (result IN ('match', 'mismatch', 'missing', 'io_error')),
    error_code text,
    checked_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX asset_integrity_checks_latest
    ON asset_integrity_checks (asset_id, checked_at DESC);

CREATE TABLE signed_manifests (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    manifest_version integer NOT NULL CHECK (manifest_version > 0),
    object_key text NOT NULL UNIQUE,
    asset_count bigint NOT NULL CHECK (asset_count >= 0),
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    signature_algorithm text NOT NULL DEFAULT 'ed25519'
        CHECK (signature_algorithm = 'ed25519'),
    signing_key_id text NOT NULL,
    signature bytea NOT NULL CHECK (octet_length(signature) = 64),
    generated_at timestamptz NOT NULL DEFAULT now()
);

-- Product audit records are append-only. Corrections are represented by a new
-- event so sequence history remains independently inspectable.
CREATE FUNCTION reject_upload_event_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'upload_events is append-only';
END;
$$;

CREATE TRIGGER upload_events_no_update
    BEFORE UPDATE OR DELETE ON upload_events
    FOR EACH ROW EXECUTE FUNCTION reject_upload_event_mutation();

COMMIT;

