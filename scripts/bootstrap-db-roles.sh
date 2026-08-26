#!/bin/sh
set -eu

mode="${1:-}"
: "${POSTGRES_DB:?POSTGRES_DB is required}"
: "${POSTGRES_USER:?POSTGRES_USER is required}"
: "${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"

psql_super() {
  PGPASSWORD="$POSTGRES_PASSWORD" psql \
    --host="${PGHOST:-postgres}" \
    --port="${PGPORT:-5432}" \
    --username="$POSTGRES_USER" \
    --dbname="$POSTGRES_DB" \
    --set=ON_ERROR_STOP=1 \
    "$@"
}

case "$mode" in
  bootstrap)
    : "${GATEWAY_DB_PASSWORD:?GATEWAY_DB_PASSWORD is required}"
    : "${ADMIN_DB_PASSWORD:?ADMIN_DB_PASSWORD is required}"
    : "${INTEGRITY_DB_PASSWORD:?INTEGRITY_DB_PASSWORD is required}"
    : "${READONLY_DB_PASSWORD:?READONLY_DB_PASSWORD is required}"
    : "${BACKUP_DB_PASSWORD:?BACKUP_DB_PASSWORD is required}"
    : "${SESSION_MAINTENANCE_DB_PASSWORD:?SESSION_MAINTENANCE_DB_PASSWORD is required}"
    psql_super \
      --set=gateway_password="$GATEWAY_DB_PASSWORD" \
      --set=admin_password="$ADMIN_DB_PASSWORD" \
      --set=integrity_password="$INTEGRITY_DB_PASSWORD" \
      --set=readonly_password="$READONLY_DB_PASSWORD" \
      --set=backup_password="$BACKUP_DB_PASSWORD" \
      --set=session_maintenance_password="$SESSION_MAINTENANCE_DB_PASSWORD" <<'SQL'
SELECT format(
  'CREATE ROLE photo_cloud_gateway LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'gateway_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_gateway') \gexec
SELECT format(
  'CREATE ROLE photo_cloud_admin LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'admin_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_admin') \gexec
SELECT format(
  'CREATE ROLE photo_cloud_integrity LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'integrity_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_integrity') \gexec
SELECT format(
  'CREATE ROLE photo_cloud_readonly LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'readonly_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_readonly') \gexec
SELECT format(
  'CREATE ROLE photo_cloud_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'backup_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_backup') \gexec
SELECT format(
  'CREATE ROLE photo_cloud_session_maintenance LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS PASSWORD %L',
  :'session_maintenance_password'
)
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'photo_cloud_session_maintenance') \gexec

-- Rotate passwords on every bootstrap so changing .env does not require a
-- manual ALTER ROLE on an existing installation.
SELECT format('ALTER ROLE photo_cloud_gateway PASSWORD %L', :'gateway_password') \gexec
SELECT format('ALTER ROLE photo_cloud_admin PASSWORD %L', :'admin_password') \gexec
SELECT format('ALTER ROLE photo_cloud_integrity PASSWORD %L', :'integrity_password') \gexec
SELECT format('ALTER ROLE photo_cloud_readonly PASSWORD %L', :'readonly_password') \gexec
SELECT format('ALTER ROLE photo_cloud_backup PASSWORD %L', :'backup_password') \gexec
SELECT format('ALTER ROLE photo_cloud_session_maintenance PASSWORD %L', :'session_maintenance_password') \gexec
SQL
    ;;
  finalize)
    psql_super <<'SQL'
-- Start from no ambient privileges. Runtime roles receive only the tables each
-- process actually touches; a compromised Internet-facing gateway therefore
-- cannot rewrite manifest/scrub evidence or schema migration history.
SELECT format(
  'REVOKE ALL PRIVILEGES ON DATABASE %I FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance',
  current_database()
) \gexec
SELECT format(
  'GRANT CONNECT ON DATABASE %I TO photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance',
  current_database()
) \gexec
REVOKE ALL ON SCHEMA public FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance;
-- Existing databases upgraded from older PostgreSQL releases can retain the
-- historical PUBLIC CREATE grant. Remove it explicitly before granting the
-- scoped runtime roles any schema access.
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance;

REVOKE ALL ON ALL TABLES IN SCHEMA public FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance;

-- Gateway: auth/session/MFA state machine + upload state machine + visible asset
-- reads. upload_events remains append-only at the privilege layer as well as by
-- trigger: the gateway can insert events but cannot update or delete them.
GRANT SELECT ON users TO photo_cloud_gateway;
GRANT UPDATE (auth_epoch) ON users TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE ON device_sessions TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE ON user_sessions TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON login_throttles TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa_totp TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa_recovery_codes TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON mfa_challenges TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE, DELETE ON mfa_action_throttles TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE ON upload_sessions TO photo_cloud_gateway;
GRANT SELECT, INSERT, UPDATE ON upload_session_throttles TO photo_cloud_gateway;
GRANT SELECT, INSERT ON assets TO photo_cloud_gateway;
GRANT SELECT, INSERT ON upload_events TO photo_cloud_gateway;

-- uuidv7() does not require sequence rights, but upload_events uses an identity
-- sequence. Grant only that generated sequence to the gateway.
GRANT USAGE, SELECT ON SEQUENCE upload_events_sequence_id_seq TO photo_cloud_gateway;

-- Admin one-shot tooling can provision users, but cannot read password hashes
-- or mutate upload/integrity/audit history.
GRANT INSERT ON users TO photo_cloud_admin;
GRANT SELECT (id) ON users TO photo_cloud_admin;

-- Integrity jobs can read the durable inventory and append their own evidence.
-- They cannot mutate uploads, users, assets, or upload_events.
GRANT SELECT ON assets, upload_sessions, signed_manifests, asset_integrity_checks TO photo_cloud_integrity;
GRANT INSERT ON signed_manifests, asset_integrity_checks TO photo_cloud_integrity;
GRANT USAGE, SELECT ON SEQUENCE asset_integrity_checks_id_seq TO photo_cloud_integrity;

-- Metrics/export processes are deliberately read-only and see only the
-- operational/integrity tables they query; auth secrets and session rows are
-- not exposed to observability credentials.
GRANT SELECT ON upload_sessions, assets, upload_events, asset_integrity_checks, signed_manifests TO photo_cloud_readonly;

-- Backup is a separate one-shot credential. It can read all application data
-- for pg_dump but cannot write anything or assume a runtime role.
GRANT SELECT ON ALL TABLES IN SCHEMA public TO photo_cloud_backup;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO photo_cloud_backup;

-- Session history retention is a one-shot maintenance identity, not a gateway privilege.
GRANT SELECT, DELETE ON user_sessions, device_sessions TO photo_cloud_session_maintenance;

-- Do not silently widen privileges on future migrations. New tables are denied
-- until this script is updated explicitly; this makes schema growth fail closed.
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON TABLES FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance',
  current_user
) \gexec
SELECT format(
  'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public REVOKE ALL ON SEQUENCES FROM photo_cloud_gateway, photo_cloud_admin, photo_cloud_integrity, photo_cloud_readonly, photo_cloud_backup, photo_cloud_session_maintenance',
  current_user
) \gexec

-- Fail startup if a future edit accidentally widens a sensitive runtime role.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_roles
    WHERE rolname IN ('photo_cloud_gateway', 'photo_cloud_admin', 'photo_cloud_integrity', 'photo_cloud_readonly', 'photo_cloud_backup')
      AND (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls)
  ) THEN
    RAISE EXCEPTION 'runtime database role unexpectedly has elevated role attributes';
  END IF;
  IF has_table_privilege('photo_cloud_gateway', 'signed_manifests', 'INSERT')
     OR has_table_privilege('photo_cloud_gateway', 'asset_integrity_checks', 'INSERT')
     OR has_table_privilege('photo_cloud_gateway', 'schema_migrations', 'UPDATE') THEN
    RAISE EXCEPTION 'gateway database role can mutate integrity/migration evidence';
  END IF;
  IF has_table_privilege('photo_cloud_readonly', 'users', 'SELECT')
     OR has_table_privilege('photo_cloud_readonly', 'user_sessions', 'SELECT')
     OR has_table_privilege('photo_cloud_readonly', 'device_sessions', 'SELECT') THEN
    RAISE EXCEPTION 'observability database role can read auth/session tables';
  END IF;
  IF has_table_privilege('photo_cloud_backup', 'users', 'UPDATE') THEN
    RAISE EXCEPTION 'backup database role is writable';
  END IF;
  IF has_table_privilege('photo_cloud_session_maintenance', 'user_sessions', 'UPDATE')
     OR has_table_privilege('photo_cloud_session_maintenance', 'users', 'SELECT') THEN
    RAISE EXCEPTION 'session maintenance role is over-privileged';
  END IF;
  IF has_schema_privilege('photo_cloud_gateway', 'public', 'CREATE')
     OR has_schema_privilege('photo_cloud_admin', 'public', 'CREATE')
     OR has_schema_privilege('photo_cloud_integrity', 'public', 'CREATE')
     OR has_schema_privilege('photo_cloud_readonly', 'public', 'CREATE')
     OR has_schema_privilege('photo_cloud_backup', 'public', 'CREATE') THEN
    RAISE EXCEPTION 'runtime database role can create objects in public schema';
  END IF;
END
$$;
SQL
    ;;
  *)
    echo "usage: $0 bootstrap|finalize" >&2
    exit 64
    ;;
esac
