BEGIN;
ALTER TABLE users ADD COLUMN library_lock_enabled boolean NOT NULL DEFAULT false;
ALTER TABLE device_sessions ADD COLUMN library_unlocked_until timestamptz;
ALTER TABLE device_sessions ADD COLUMN library_lock_version bigint NOT NULL DEFAULT 0;
COMMIT;
