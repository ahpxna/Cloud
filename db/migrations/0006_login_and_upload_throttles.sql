BEGIN;

CREATE TABLE login_throttles (
    identity_hash bytea PRIMARY KEY CHECK (octet_length(identity_hash) = 32),
    window_started_at timestamptz NOT NULL,
    attempt_count integer NOT NULL CHECK (attempt_count >= 0),
    blocked_until timestamptz,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX login_throttles_cleanup
    ON login_throttles (updated_at);

CREATE TABLE upload_session_throttles (
    owner_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    window_started_at timestamptz NOT NULL,
    create_count integer NOT NULL CHECK (create_count >= 0),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX upload_session_throttles_cleanup
    ON upload_session_throttles (updated_at);

-- Active-session admission already takes a transaction-level advisory lock.
-- This partial index keeps the per-owner count bounded as historical terminal
-- rows accumulate.
CREATE INDEX upload_sessions_owner_active
    ON upload_sessions (owner_id, state)
    WHERE state IN (
        'created', 'uploading', 'received', 'verifying', 'verified',
        'committing', 'quarantining'
    );

COMMIT;
