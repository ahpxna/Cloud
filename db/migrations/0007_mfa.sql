BEGIN;

CREATE TABLE user_mfa_totp (
    user_id uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    encrypted_secret bytea NOT NULL,
    nonce bytea NOT NULL CHECK (octet_length(nonce) = 12),
    confirmed_at timestamptz,
    last_used_counter bigint,
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE user_mfa_recovery_codes (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash bytea NOT NULL CHECK (octet_length(code_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    used_at timestamptz,
    UNIQUE (user_id, code_hash)
);

CREATE INDEX user_mfa_recovery_codes_unused
    ON user_mfa_recovery_codes (user_id, id)
    WHERE used_at IS NULL;

CREATE TABLE mfa_challenges (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    challenge_hash bytea NOT NULL UNIQUE CHECK (octet_length(challenge_hash) = 32),
    user_id uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    device_name text NOT NULL,
    expires_at timestamptz NOT NULL,
    attempts_remaining integer NOT NULL CHECK (attempts_remaining >= 0),
    consumed_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX mfa_challenges_active
    ON mfa_challenges (challenge_hash, expires_at)
    WHERE consumed_at IS NULL AND attempts_remaining > 0;

CREATE INDEX mfa_challenges_user_recent
    ON mfa_challenges (user_id, created_at DESC);

CREATE INDEX mfa_challenges_cleanup
    ON mfa_challenges (created_at);

COMMIT;
