#!/usr/bin/env bash
# Consistent encrypted off-site backup entry point.
# Requires a configured restic repository and password file. The gateway is
# stopped while the PostgreSQL dump and immutable-media snapshot are captured
# so database/media evidence belongs to one quiescent backup window.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
# shellcheck source=scripts/lib/operator-lock.sh
source "$repo_root/scripts/lib/operator-lock.sh"
load_dotenv "$repo_root/.env"

: "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY to an encrypted off-site repository}"
: "${RESTIC_PASSWORD_FILE:?set RESTIC_PASSWORD_FILE to a protected password file}"

command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }
command -v restic >/dev/null 2>&1 || { echo "restic is required" >&2; exit 2; }
[[ -r "$RESTIC_PASSWORD_FILE" ]] || { echo "RESTIC_PASSWORD_FILE is not readable" >&2; exit 2; }

media_root="${PHOTO_MEDIA_HOST_ROOT:-$repo_root/.data/media}"
manifest_root="${MANIFEST_OUTPUT_DIR:-$repo_root/.data/manifests}"
audit_root="${AUDIT_EXPORT_DIR:-$repo_root/.data/audit}"
manifest_keyring_root="${MANIFEST_PUBLIC_KEYRING_HOST_PATH:-$repo_root/.data/secrets/manifest-public-keyring}"
backup_root="${BACKUP_WORK_DIR:-$repo_root/.data/backup-work}"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
work="$backup_root/$stamp"
dump="$work/postgres.dump"
metadata="$work/backup-metadata.txt"
mkdir -p "$work" "$manifest_root" "$audit_root"
chmod 700 "$backup_root" "$work" 2>/dev/null || true

started_gateway=0
operator_lock_acquire
cleanup() {
  status=$?
  if [[ $started_gateway -eq 1 ]]; then
    docker compose --profile gateway up -d --no-deps upload-gateway >/dev/null 2>&1 || true
  fi
  operator_lock_release
  exit "$status"
}
trap cleanup EXIT INT TERM

if docker compose ps --status running --services 2>/dev/null | grep -qx 'upload-gateway'; then
  echo "Stopping upload-gateway for a consistent backup window..."
  docker compose stop -t 30 upload-gateway
  started_gateway=1
fi

# pg_dump runs inside the trusted database container; secrets remain in Compose
# environment and are not copied to the host command line.
echo "Creating PostgreSQL custom-format dump..."
docker compose exec -T postgres sh -ec \
  'PGPASSWORD="$POSTGRES_PASSWORD" pg_dump --format=custom --no-owner --no-acl --dbname="$POSTGRES_DB" --username="$POSTGRES_USER"' \
  > "$dump"
[[ -s "$dump" ]] || { echo "database dump is empty" >&2; exit 1; }
chmod 600 "$dump"

{
  printf 'created_at=%s\n' "$stamp"
  printf 'media_root=%s\n' "$media_root"
  printf 'manifest_root=%s\n' "$manifest_root"
  printf 'audit_root=%s\n' "$audit_root"
	printf 'manifest_public_keyring=%s\n' "$manifest_keyring_root"
  printf 'git_commit=%s\n' "$(git rev-parse HEAD 2>/dev/null || echo unavailable)"
  printf 'postgres_image=%s\n' "${POSTGRES_IMAGE:-postgres:18.4-alpine}"
} > "$metadata"
chmod 600 "$metadata"

paths=("$dump" "$metadata")
[[ -d "$media_root" ]] && paths+=("$media_root")
[[ -d "$manifest_root" ]] && paths+=("$manifest_root")
[[ -d "$audit_root" ]] && paths+=("$audit_root")
[[ -d "$manifest_keyring_root" ]] && paths+=("$manifest_keyring_root")

# restic authenticates and encrypts before data leaves the host.
echo "Writing encrypted restic snapshot..."
restic backup --tag family-photo-cloud --tag "backup-window:$stamp" "${paths[@]}"
restic check --read-data-subset="${RESTIC_READ_DATA_SUBSET:-1/100}"

# A local dump is useful only as staging for the encrypted repository. Remove it
# after restic confirms the snapshot to reduce duplicate sensitive state.
rm -rf "$work"
trap - EXIT INT TERM
if [[ $started_gateway -eq 1 ]]; then
  docker compose --profile gateway up -d --no-deps upload-gateway
fi
operator_lock_release
printf 'Backup completed: %s\n' "$stamp"
