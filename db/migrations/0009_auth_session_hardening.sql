BEGIN;

-- MFA enrollment performs password re-authentication and must share the same
-- durable per-user abuse budget as the other sensitive MFA mutations.
ALTER TABLE mfa_action_throttles
    DROP CONSTRAINT mfa_action_throttles_action_check;
ALTER TABLE mfa_action_throttles
    ADD CONSTRAINT mfa_action_throttles_action_check
    CHECK (action IN ('enroll', 'confirm', 'recovery', 'disable'));

-- Preserve the exact rotated refresh token only as short-lived authenticated
-- ciphertext so a client can safely retry after losing the successful HTTP
-- response. The retry is bound to a client-generated rotation request ID by a
-- SHA-256 digest; possession of the old refresh token alone is not enough to
-- claim the successor during the grace window.
ALTER TABLE user_sessions
    ADD COLUMN refresh_retry_request_sha256 bytea,
    ADD COLUMN refresh_retry_ciphertext bytea,
    ADD COLUMN refresh_retry_nonce bytea,
    ADD COLUMN refresh_retry_until timestamptz,
    ADD CONSTRAINT user_sessions_refresh_retry_shape CHECK (
        (
            refresh_retry_request_sha256 IS NULL
            AND refresh_retry_ciphertext IS NULL
            AND refresh_retry_nonce IS NULL
            AND refresh_retry_until IS NULL
        )
        OR
        (
            refresh_retry_request_sha256 IS NOT NULL
            AND octet_length(refresh_retry_request_sha256) = 32
            AND refresh_retry_ciphertext IS NOT NULL
            AND refresh_retry_nonce IS NOT NULL
            AND octet_length(refresh_retry_nonce) = 12
            AND refresh_retry_until IS NOT NULL
        )
    );

CREATE INDEX user_sessions_refresh_retry_cleanup
    ON user_sessions (refresh_retry_until)
    WHERE refresh_retry_until IS NOT NULL;

COMMIT;
