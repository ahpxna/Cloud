#!/usr/bin/env bash
# Recover into empty host storage only. Never removes or overwrites live data.
set -euo pipefail
umask 077
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
source "$repo_root/scripts/lib/load-env.sh"
source "$repo_root/scripts/lib/operator-lock.sh"
source "$repo_root/scripts/lib/restore.sh"
load_dotenv "$repo_root/.env"
[[ $# -eq 0 ]] || { echo 'usage: RESTORE_SNAPSHOT=<id> make restore-live (no overwrite option)' >&2; exit 2; }

for tool in docker restic jq realpath sha256sum; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 2; }
done
: "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY}"
: "${RESTIC_PASSWORD_FILE:?set RESTIC_PASSWORD_FILE}"

# Reject an incomplete recovery kit before locks, directories or containers.
# Report key names only; secret values never belong in recovery output.
for key in POSTGRES_PASSWORD GATEWAY_DB_PASSWORD ADMIN_DB_PASSWORD \
  INTEGRITY_DB_PASSWORD READONLY_DB_PASSWORD BACKUP_DB_PASSWORD \
  SESSION_MAINTENANCE_DB_PASSWORD ACCESS_TOKEN_HMAC_KEY_BASE64 \
  LOGIN_THROTTLE_HMAC_KEY_BASE64 MFA_ENCRYPTION_KEY_BASE64 \
  REFRESH_RETRY_ENCRYPTION_KEY_BASE64 MANIFEST_SIGNING_KEY_ID; do
  [[ -n "${!key:-}" && "${!key}" != replace-with-* && "${!key}" != change-me* ]] || {
    printf 'missing required recovery secret/config: %s; supply it securely before restore-live\n' "$key" >&2
    exit 2
  }
done
for key in RESTIC_PASSWORD_FILE MANIFEST_TRUST_FINGERPRINTS_FILE \
  MANIFEST_KEY_HOST_PATH MANIFEST_PUBLIC_KEY_HOST_PATH; do
  case "$key" in
    MANIFEST_KEY_HOST_PATH) path="${MANIFEST_KEY_HOST_PATH:-$repo_root/.data/secrets/manifest-ed25519.pem}" ;;
    MANIFEST_PUBLIC_KEY_HOST_PATH) path="${MANIFEST_PUBLIC_KEY_HOST_PATH:-$repo_root/.data/secrets/manifest-ed25519-public.pem}" ;;
    *) path="${!key:-}" ;;
  esac
  [[ -n "$path" && -f "$path" && -r "$path" && -s "$path" ]] || {
    printf 'missing or unreadable required recovery file: %s; transfer it securely before restore-live\n' "$key" >&2
    exit 2
  }
done

operator_lock_acquire
started_postgres=0
cleanup() {
  status=$?
  trap - EXIT
  if [[ $status -ne 0 ]]; then
    echo 'FAIL live restore: partial data retained; inspect before using fresh empty destinations.' >&2
    if [[ $started_postgres -eq 1 ]]; then
      docker compose --profile gateway stop upload-gateway postgres >/dev/null 2>&1 || true
    fi
  fi
  operator_lock_release
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

require_empty() {
  local path="$1" entries
  [[ ! -L "$path" ]] || { echo "refusing symlink destination: $path" >&2; exit 2; }
  if [[ -e "$path" ]]; then
    [[ -d "$path" ]] || { echo "refusing non-directory destination: $path" >&2; exit 2; }
    entries="$(find "$path" -mindepth 1 -maxdepth 1 -print -quit)" || {
      echo "cannot inspect destination: $path" >&2; exit 2;
    }
    [[ -z "$entries" ]] || {
      echo "refusing nonempty destination: $path" >&2; exit 2;
    }
  fi
}

database_dest="$repo_root/.data/postgres"
media_dest="${PHOTO_MEDIA_HOST_ROOT:-$repo_root/.data/media}"
manifest_dest="${MANIFEST_OUTPUT_DIR:-$repo_root/.data/manifests}"
audit_dest="${AUDIT_EXPORT_DIR:-$repo_root/.data/audit}"
keyring_dest="${MANIFEST_PUBLIC_KEYRING_HOST_PATH:-$repo_root/.data/secrets/manifest-public-keyring}"
destinations=("$database_dest" "$media_dest" "$manifest_dest" "$audit_dest" "$keyring_dest")
for destination in "${destinations[@]}"; do require_empty "$destination"; done
if [[ "$(id -u)" != 0 ]]; then
  [[ "$(id -u)" == "${PHOTO_UID:-1000}" && "$(id -g)" == "${PHOTO_GID:-1000}" ]] || {
    echo 'run as root or the configured PHOTO_UID:PHOTO_GID to set recovered ownership' >&2; exit 2;
  }
fi

# Validate the actual Compose bind sources before creating any storage.
model="$(docker compose --profile gateway config --format json)"
[[ "$(jq -r '.services.postgres.volumes[] | select(.target=="/var/lib/postgresql") | .source' <<<"$model")" == "$database_dest" ]] || {
  echo 'unexpected PostgreSQL mount; refusing restore' >&2; exit 2;
}
# realpath needs an existing parent on macOS; only empty directories are created.
for destination in "${destinations[@]}"; do mkdir -p "$destination"; done
media_dest="$(realpath "$media_dest")"
model_media="$(jq -r '.services["upload-gateway"].volumes[] | select(.target=="/srv/media") | .source' <<<"$model")"
[[ "$(realpath "$model_media")" == "$media_dest" ]] || {
  echo 'unexpected media mount; refusing restore' >&2; exit 2;
}
[[ -z "$(docker compose --profile '*' ps -q --status running)" ]] || {
  echo 'stop this entire Compose project before live restore' >&2; exit 2;
}

target="${RESTORE_LIVE_DIR:-$repo_root/.data/restore-live-$(date -u +%Y%m%dT%H%M%SZ)-$RANDOM}"
require_empty "$target"
mkdir -p "$target"
target="$(realpath "$target")"
# Every destination and the staging tree must be disjoint, including aliases.
canonical=("$target")
for destination in "${destinations[@]}"; do canonical+=("$(realpath "$destination")"); done
for ((i=0; i<${#canonical[@]}; i++)); do
  for ((j=i+1; j<${#canonical[@]}; j++)); do
    a="${canonical[i]}"; b="${canonical[j]}"
    [[ "$a" != "$b" && "$a" != "$b/"* && "$b" != "$a/"* ]] || {
      echo "overlapping restore paths: $a and $b" >&2; exit 2;
    }
  done
done
restore_snapshot
# Validate all captured directory mappings before initializing PostgreSQL.
sources=()
for key in media_root manifest_root audit_root manifest_public_keyring; do
  sources+=("$(restore_source_path "$key")")
done

docker compose up -d --wait postgres
started_postgres=1
docker compose run --rm --no-deps -T db-role-bootstrap
docker compose exec -T -e PGPASSWORD="${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD}" postgres \
  pg_restore --exit-on-error --no-owner --no-acl -U "${POSTGRES_USER:-photo_cloud}" \
  -d "${POSTGRES_DB:-photo_cloud}" < "$dump"
# Use the normal migration runner and runtime grants, including future upgrades.
docker compose run --build --rm --no-deps -T migrate
docker compose run --rm --no-deps -T db-runtime-grants
restore_query() {
  docker compose exec -T -e PGPASSWORD="$POSTGRES_PASSWORD" postgres \
    psql -v ON_ERROR_STOP=1 -At -F $'\t' -U "${POSTGRES_USER:-photo_cloud}" \
    -d "${POSTGRES_DB:-photo_cloud}" -c "$1"
}
restore_verify
[[ "$manifest_count" != 0 ]] || { echo 'no signed manifest: run integrity-cycle before backup' >&2; exit 1; }

# Copy every captured directory, including hidden files, staging and quarantine.
# Keep the restored dump/metadata as recovery evidence. No .env/private key/node
# identity is in these backups; those require the independent recovery kit.
for ((i=0; i<${#sources[@]}; i++)); do
  destination="${destinations[i+1]}"
  require_empty "$destination"
  cp -a "${sources[i]}/." "$destination/"
done
if [[ "$(id -u)" == 0 ]]; then
  chown -R "${PHOTO_UID:-1000}:${PHOTO_GID:-1000}" "$media_dest" "$manifest_dest" "$audit_dest" "$keyring_dest"
fi
# Re-hash the final serving paths as well as the staged snapshot.
docker compose --profile integrity run --build --rm -T scrub
docker compose --profile gateway up -d --build --wait upload-gateway
printf 'PASS live restore: %s assets rehashed, %s signed manifests verified; gateway healthy; evidence=%s\n' \
  "$checked" "$manifests_verified" "$target"
