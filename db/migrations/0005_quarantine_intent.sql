BEGIN;

-- Filesystem quarantine is a destructive mutation and must be fenced before
-- bytes move out of staging. `quarantining` records that intent durably while
-- retaining the verifier claim token until the terminal transition commits.
ALTER TABLE upload_sessions
    DROP CONSTRAINT upload_sessions_state_check;

ALTER TABLE upload_sessions
    ADD CONSTRAINT upload_sessions_state_check CHECK (state IN (
        'created', 'uploading', 'received', 'verifying', 'verified',
        'committing', 'available', 'failed', 'expired', 'quarantining',
        'quarantined'
    ));

-- The intent transition is append-only audit evidence. Keep the event-type
-- constraint in lock-step with the repository transition so PostgreSQL cannot
-- reject a correctly fenced quarantine before the filesystem move.
ALTER TABLE upload_events
    DROP CONSTRAINT upload_events_event_type_check;

ALTER TABLE upload_events
    ADD CONSTRAINT upload_events_event_type_check CHECK (event_type IN (
        'created', 'upload_started', 'bytes_acknowledged', 'interrupted',
        'resumed', 'received', 'verification_started', 'verified',
        'checksum_mismatch', 'quarantine_started', 'commit_started',
        'available', 'failed', 'expired', 'quarantined', 'reconciled'
    ));

DROP INDEX upload_sessions_reconciliation;
CREATE INDEX upload_sessions_reconciliation
    ON upload_sessions (state, updated_at)
    WHERE state IN ('received', 'verifying', 'verified', 'committing', 'quarantining');

DROP INDEX upload_sessions_verification_claim;
CREATE INDEX upload_sessions_verification_claim
    ON upload_sessions (updated_at, id)
    WHERE state IN ('received', 'verifying', 'verified', 'committing', 'quarantining')
      AND verification_lease_until IS NULL;

DROP INDEX upload_sessions_verification_lease_expiry;
CREATE INDEX upload_sessions_verification_lease_expiry
    ON upload_sessions (verification_lease_until, updated_at, id)
    WHERE state IN ('received', 'verifying', 'verified', 'committing', 'quarantining');

COMMIT;
