BEGIN;

-- Durable device identity is separate from refresh-token generations.  A
-- device_session row is the stable revocation/serialization point; rows in
-- user_sessions are short-lived refresh generations only.
ALTER TABLE users
    ADD COLUMN auth_epoch bigint NOT NULL DEFAULT 0 CHECK (auth_epoch >= 0);

CREATE TABLE device_sessions (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX device_sessions_active_by_user
    ON device_sessions (user_id, last_used_at DESC, id DESC)
    WHERE revoked_at IS NULL;

-- Preserve one stable identity per historical refresh family.  Using the old
-- family UUID as the new device UUID gives every existing family a deterministic
-- identity without inventing a second mapping table.
INSERT INTO device_sessions (
    id, user_id, device_name, created_at, last_used_at, expires_at, revoked_at
)
SELECT DISTINCT ON (session_family_id)
    session_family_id,
    user_id,
    device_name,
    min(created_at) OVER (PARTITION BY session_family_id),
    max(last_used_at) OVER (PARTITION BY session_family_id),
    max(expires_at) OVER (PARTITION BY session_family_id),
    CASE
        WHEN bool_or(revoked_at IS NULL AND expires_at > now()) OVER (PARTITION BY session_family_id)
            THEN NULL
        ELSE max(COALESCE(revoked_at, expires_at)) OVER (PARTITION BY session_family_id)
    END
FROM user_sessions
ORDER BY session_family_id, last_used_at DESC, id DESC;

ALTER TABLE user_sessions
    ADD COLUMN device_session_id uuid;

UPDATE user_sessions
SET device_session_id = session_family_id;

ALTER TABLE user_sessions
    ALTER COLUMN device_session_id SET NOT NULL,
    ADD CONSTRAINT user_sessions_device_session_id_fkey
        FOREIGN KEY (device_session_id) REFERENCES device_sessions(id) ON DELETE CASCADE;

DROP INDEX IF EXISTS user_sessions_active_by_user;
DROP INDEX IF EXISTS user_sessions_family_active;

CREATE INDEX user_sessions_device_active
    ON user_sessions (device_session_id, expires_at)
    WHERE revoked_at IS NULL;

-- parent_session_id remains useful for the bounded lost-response retry capsule,
-- but family/user/device identity now lives only in device_sessions.
ALTER TABLE user_sessions
    DROP COLUMN user_id,
    DROP COLUMN device_name,
    DROP COLUMN session_family_id;

ALTER TABLE user_sessions
    DROP CONSTRAINT user_sessions_parent_session_id_fkey,
    ADD CONSTRAINT user_sessions_parent_session_id_fkey
        FOREIGN KEY (parent_session_id) REFERENCES user_sessions(id) ON DELETE SET NULL;

-- MFA challenges are bound to the authentication configuration epoch that
-- created them.  Any enrollment/enable/disable transition increments the epoch
-- and makes already-issued challenges unusable.
ALTER TABLE mfa_challenges
    ADD COLUMN auth_epoch bigint NOT NULL DEFAULT 0;

COMMIT;
