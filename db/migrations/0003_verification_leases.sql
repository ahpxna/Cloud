BEGIN;

-- The verifier queue is process-local, but ownership of a hash/commit job is
-- durable. A crashed worker is reclaimed after its lease; a live worker renews
-- it while hashing large videos, preventing periodic reconciliation from
-- enqueueing the same file repeatedly.
ALTER TABLE upload_sessions
    ADD COLUMN verification_worker_id text,
    ADD COLUMN verification_claimed_at timestamptz,
    ADD COLUMN verification_lease_until timestamptz;

CREATE INDEX upload_sessions_verification_claim
    ON upload_sessions (updated_at, id)
    WHERE state IN ('received', 'verifying', 'verified', 'committing')
      AND verification_lease_until IS NULL;

CREATE INDEX upload_sessions_verification_lease_expiry
    ON upload_sessions (verification_lease_until, updated_at, id)
    WHERE state IN ('received', 'verifying', 'verified', 'committing');

COMMIT;
