BEGIN;

CREATE TABLE mfa_action_throttles (
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    action text NOT NULL CHECK (action IN ('confirm', 'recovery', 'disable')),
    window_started_at timestamptz NOT NULL,
    attempt_count integer NOT NULL CHECK (attempt_count >= 0),
    blocked_until timestamptz,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (user_id, action)
);

CREATE INDEX mfa_action_throttles_cleanup
    ON mfa_action_throttles (updated_at);

COMMIT;
