BEGIN;

-- Library curation: favourites, hidden items, a 30-day "Recently Deleted"
-- bin, captions, albums inside (nested) folders, albums shared with other
-- family accounts, and the capture metadata that the timeline, places and
-- smart albums sort and filter on.

-- A permanently deleted ("purged") asset keeps its row as a tombstone because
-- upload_sessions, integrity checks and audit events reference it. Content and
-- storage uniqueness therefore applies to live rows only, so the same photo
-- can be uploaded again after it was purged.
ALTER TABLE assets DROP CONSTRAINT assets_owner_id_storage_key_key;
ALTER TABLE assets DROP CONSTRAINT assets_owner_id_content_sha256_key;
CREATE UNIQUE INDEX assets_owner_storage_live
    ON assets (owner_id, storage_key) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX assets_owner_content_live
    ON assets (owner_id, content_sha256) WHERE deleted_at IS NULL;

-- Lets album rows prove in the database that whoever added a photo owns it.
ALTER TABLE assets ADD CONSTRAINT assets_id_owner_unique UNIQUE (id, owner_id);

ALTER TABLE assets
    ADD COLUMN favorite boolean NOT NULL DEFAULT false,
    ADD COLUMN hidden boolean NOT NULL DEFAULT false,
    ADD COLUMN caption text CHECK (caption IS NULL OR char_length(caption) <= 2000),
    -- Apple's content identifier joins a Live Photo's still and motion video.
    ADD COLUMN live_photo_id text CHECK (live_photo_id IS NULL OR char_length(live_photo_id) <= 128),
    ADD COLUMN subtype text CHECK (subtype IS NULL OR subtype IN ('screenshot', 'selfie', 'panorama')),
    ADD COLUMN latitude double precision CHECK (latitude IS NULL OR latitude BETWEEN -90 AND 90),
    ADD COLUMN longitude double precision CHECK (longitude IS NULL OR longitude BETWEEN -180 AND 180),
    -- Places group photos by a ~5 km grid cell; people can name a cell.
    ADD COLUMN place_cell text GENERATED ALWAYS AS (
        CASE WHEN latitude IS NULL OR longitude IS NULL THEN NULL
        ELSE round(round(latitude::numeric * 20) / 20, 2)::text || ','
             || round(round(longitude::numeric * 20) / 20, 2)::text
        END) STORED,
    ADD COLUMN metadata_version smallint NOT NULL DEFAULT 0 CHECK (metadata_version >= 0),
    -- The timeline is ordered by when a photo was taken; files without a
    -- capture date fall back to when they were uploaded.
    ADD COLUMN taken_at timestamptz GENERATED ALWAYS AS (COALESCE(captured_at, created_at)) STORED,
    ADD CONSTRAINT assets_trash_before_purge CHECK (deleted_at IS NULL OR trashed_at IS NOT NULL),
    ADD CONSTRAINT assets_location_pair CHECK ((latitude IS NULL) = (longitude IS NULL));

CREATE INDEX assets_library_taken
    ON assets (owner_id, taken_at DESC, id DESC)
    WHERE deleted_at IS NULL AND trashed_at IS NULL;
CREATE INDEX assets_library_added
    ON assets (owner_id, created_at DESC, id DESC)
    WHERE deleted_at IS NULL AND trashed_at IS NULL;
CREATE INDEX assets_trash
    ON assets (trashed_at, id)
    WHERE deleted_at IS NULL AND trashed_at IS NOT NULL;
CREATE INDEX assets_live_photo
    ON assets (owner_id, live_photo_id)
    WHERE deleted_at IS NULL AND live_photo_id IS NOT NULL;
CREATE INDEX assets_places
    ON assets (owner_id, place_cell)
    WHERE deleted_at IS NULL AND place_cell IS NOT NULL;

CREATE TABLE place_labels (
    owner_id uuid NOT NULL REFERENCES users(id),
    cell text NOT NULL CHECK (cell ~ '^-?[0-9]{1,3}\.[0-9]{2},-?[0-9]{1,3}\.[0-9]{2}$'),
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (owner_id, cell)
);
CREATE INDEX assets_metadata_pending
    ON assets (metadata_version, created_at)
    WHERE deleted_at IS NULL;

-- How family members see each other when sharing an album.
ALTER TABLE users
    ADD COLUMN display_name text CHECK (display_name IS NULL OR char_length(display_name) BETWEEN 1 AND 60);

CREATE TABLE album_folders (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id uuid NOT NULL REFERENCES users(id),
    parent_id uuid,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, owner_id),
    CHECK (parent_id IS NULL OR parent_id <> id),
    FOREIGN KEY (parent_id, owner_id) REFERENCES album_folders (id, owner_id)
);

CREATE INDEX album_folders_by_owner ON album_folders (owner_id, parent_id);

CREATE TABLE albums (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    owner_id uuid NOT NULL REFERENCES users(id),
    folder_id uuid,
    name text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    cover_asset_id uuid REFERENCES assets(id),
    sort_order text NOT NULL DEFAULT 'newest_first'
        CHECK (sort_order IN ('newest_first', 'oldest_first', 'added')),
    -- For a shared album: may members add their own photos?
    members_can_add boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (folder_id, owner_id) REFERENCES album_folders (id, owner_id)
);

CREATE INDEX albums_by_owner ON albums (owner_id, folder_id);

-- Family members an album is shared with. The owner is never listed.
CREATE TABLE album_members (
    album_id uuid NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES users(id),
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (album_id, user_id)
);

CREATE INDEX album_members_by_user ON album_members (user_id, album_id);

-- A photo appears in an album by reference; nobody gets a copy. Whoever adds
-- a photo must own it, which the composite key enforces.
CREATE TABLE album_assets (
    album_id uuid NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    asset_id uuid NOT NULL,
    added_by uuid NOT NULL,
    added_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (album_id, asset_id),
    FOREIGN KEY (asset_id, added_by) REFERENCES assets (id, owner_id)
);

CREATE INDEX album_assets_by_asset ON album_assets (asset_id);
CREATE INDEX album_assets_by_album_added ON album_assets (album_id, added_at, asset_id);

CREATE TABLE album_likes (
    album_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (album_id, asset_id, user_id),
    FOREIGN KEY (album_id, asset_id) REFERENCES album_assets (album_id, asset_id) ON DELETE CASCADE
);

CREATE TABLE album_comments (
    id uuid PRIMARY KEY DEFAULT uuidv7(),
    album_id uuid NOT NULL,
    asset_id uuid NOT NULL,
    user_id uuid NOT NULL REFERENCES users(id),
    body text NOT NULL CHECK (char_length(body) BETWEEN 1 AND 1000),
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (album_id, asset_id) REFERENCES album_assets (album_id, asset_id) ON DELETE CASCADE
);

CREATE INDEX album_comments_by_asset ON album_comments (album_id, asset_id, created_at);

-- Append-only history of what people did to their library. A purge records
-- its event in the same transaction that tombstones the asset.
CREATE TABLE asset_events (
    sequence_id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    asset_id uuid NOT NULL REFERENCES assets(id),
    owner_id uuid NOT NULL REFERENCES users(id),
    event_type text NOT NULL CHECK (event_type IN (
        'favorited', 'unfavorited', 'hidden', 'unhidden', 'trashed',
        'restored', 'purged', 'caption_changed'
    )),
    -- 'owner' for an action someone took, 'retention' for the 30-day purge,
    -- 'upload' when uploading the same photo again brings it back.
    actor text NOT NULL CHECK (actor IN ('owner', 'retention', 'upload')),
    occurred_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX asset_events_by_owner ON asset_events (owner_id, sequence_id DESC);

CREATE FUNCTION reject_asset_event_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'asset_events is append-only';
END;
$$;

CREATE TRIGGER asset_events_no_update
    BEFORE UPDATE OR DELETE ON asset_events
    FOR EACH ROW EXECUTE FUNCTION reject_asset_event_mutation();

COMMIT;
