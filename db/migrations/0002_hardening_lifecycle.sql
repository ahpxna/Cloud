BEGIN;

-- A duplicate upload session may resolve to the already-existing logical
-- asset for the same owner/content. Keeping that relationship prevents an
-- `available` session from pointing at a second, unreferenced blob.
ALTER TABLE upload_sessions
    ADD COLUMN asset_id uuid REFERENCES assets(id);

UPDATE upload_sessions AS session
SET asset_id = asset.id,
    final_storage_key = asset.storage_key
FROM assets AS asset
WHERE session.state = 'available'
  AND session.owner_id = asset.owner_id
  AND session.server_sha256 = asset.content_sha256;

ALTER TABLE upload_sessions
    ADD CONSTRAINT upload_sessions_available_asset
    CHECK (state <> 'available' OR asset_id IS NOT NULL);

CREATE INDEX upload_sessions_asset_id
    ON upload_sessions (asset_id)
    WHERE asset_id IS NOT NULL;

-- Refresh rotation is a family, not a chain of unrelated sessions. Reuse of a
-- revoked token revokes every live descendant of that family.
ALTER TABLE user_sessions
    ADD COLUMN session_family_id uuid,
    ADD COLUMN parent_session_id uuid REFERENCES user_sessions(id),
    ADD COLUMN reused_at timestamptz;

UPDATE user_sessions
SET session_family_id = id
WHERE session_family_id IS NULL;

ALTER TABLE user_sessions
    ALTER COLUMN session_family_id SET NOT NULL,
    ALTER COLUMN session_family_id SET DEFAULT uuidv7();

CREATE INDEX user_sessions_family_active
    ON user_sessions (session_family_id, expires_at)
    WHERE revoked_at IS NULL;

COMMIT;
