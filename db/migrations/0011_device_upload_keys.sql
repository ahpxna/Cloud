BEGIN;

-- Long-lived, upload-only credentials for an iOS Shortcut on one phone. They
-- can create uploads for their owner and nothing else: no library reads, no
-- account changes. Only the SHA-256 of the random key is stored. Revoking a
-- device session family, disabling the account or resetting its password also
-- revokes these keys.
CREATE TABLE device_upload_keys (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    key_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(key_sha256) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    revoked_at timestamptz
);

CREATE INDEX device_upload_keys_active_by_user
    ON device_upload_keys (user_id)
    WHERE revoked_at IS NULL;

COMMIT;
