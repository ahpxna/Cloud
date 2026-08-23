BEGIN;

-- A lease alone is not ownership. Every claim gets a fresh fencing token and
-- every verifier transition checks it, so a worker revived after lease expiry
-- cannot advance state owned by its replacement.
ALTER TABLE upload_sessions
    ADD COLUMN verification_claim_token uuid;

CREATE INDEX upload_sessions_verification_claim_token
    ON upload_sessions (verification_claim_token)
    WHERE verification_claim_token IS NOT NULL;

COMMIT;
