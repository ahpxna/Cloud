#!/usr/bin/env bash
# Restore the latest Family Photo Cloud restic snapshot into an isolated
# directory and disposable PostgreSQL container. This does not touch the live
# database or media tree.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
load_dotenv "$repo_root/.env"

: "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY}"
: "${RESTIC_PASSWORD_FILE:?set RESTIC_PASSWORD_FILE}"
command -v restic >/dev/null 2>&1 || { echo "restic is required" >&2; exit 2; }
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
target="${RESTORE_DRILL_DIR:-$repo_root/.data/restore-drills/$stamp}"
mkdir -p "$target"
chmod 700 "$target" 2>/dev/null || true

snapshot="${RESTORE_SNAPSHOT:-latest}"
echo "Restoring snapshot $snapshot into $target..."
restic restore "$snapshot" --tag family-photo-cloud --target "$target"

mapfile -t dumps < <(find "$target" -type f -name postgres.dump -print)
if [[ ${#dumps[@]} -ne 1 ]]; then
  echo "expected exactly one postgres.dump in restored snapshot, found ${#dumps[@]}" >&2
  exit 1
fi
dump="${dumps[0]}"

container="family-photo-cloud-restore-drill-${RANDOM}-${RANDOM}"
password="restore-drill-${RANDOM}-${RANDOM}-${RANDOM}"
image="${POSTGRES_IMAGE:-postgres:18.4-alpine}"
cleanup() { docker rm -f "$container" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

docker run -d --name "$container" \
  -e POSTGRES_DB=photo_cloud \
  -e POSTGRES_USER=photo_cloud \
  -e POSTGRES_PASSWORD="$password" \
  "$image" >/dev/null
for _ in $(seq 1 60); do
  if docker exec "$container" pg_isready -U photo_cloud -d photo_cloud >/dev/null 2>&1; then break; fi
  sleep 1
done
docker exec "$container" pg_isready -U photo_cloud -d photo_cloud >/dev/null
cat "$dump" | docker exec -i -e PGPASSWORD="$password" "$container" \
  pg_restore --clean --if-exists --no-owner --no-acl -U photo_cloud -d photo_cloud

asset_count="$(docker exec -e PGPASSWORD="$password" "$container" psql -At -U photo_cloud -d photo_cloud -c \
  "SELECT count(*) FROM assets WHERE deleted_at IS NULL")"
manifest_count="$(docker exec -e PGPASSWORD="$password" "$container" psql -At -U photo_cloud -d photo_cloud -c \
  "SELECT count(*) FROM signed_manifests")"

# Locate the restored host media root by finding an originals directory. The
# backup retains absolute path components under the restic restore target.
media_originals="$(find "$target" -type d -name originals -print -quit || true)"
if [[ -z "$media_originals" && "$asset_count" != "0" ]]; then
  echo "database restored but originals directory is missing" >&2
  exit 1
fi
media_root="${media_originals%/originals}"

checked=0
if [[ "$asset_count" != "0" ]]; then
  while IFS=$'\t' read -r storage_key expected_hex expected_size; do
    [[ -n "$storage_key" ]] || continue
    path="$media_root/$storage_key"
    [[ -f "$path" ]] || { echo "missing restored original: $storage_key" >&2; exit 1; }
    actual_size="$(stat -c %s "$path")"
    [[ "$actual_size" == "$expected_size" ]] || { echo "size mismatch: $storage_key" >&2; exit 1; }
    actual_hex="$(sha256sum "$path" | awk '{print $1}')"
    [[ "$actual_hex" == "$expected_hex" ]] || { echo "SHA-256 mismatch: $storage_key" >&2; exit 1; }
    checked=$((checked + 1))
  done < <(docker exec -e PGPASSWORD="$password" "$container" psql -At -F $'\t' -U photo_cloud -d photo_cloud -c \
    "SELECT storage_key, encode(content_sha256,'hex'), byte_size FROM assets WHERE deleted_at IS NULL ORDER BY id")
fi

report="$target/restore-drill-report.txt"
{
  printf 'completed_at=%s\n' "$stamp"
  printf 'snapshot=%s\n' "$snapshot"
  printf 'asset_rows=%s\n' "$asset_count"
  printf 'assets_rehashed=%s\n' "$checked"
  printf 'signed_manifests=%s\n' "$manifest_count"
  printf 'result=PASS\n'
} > "$report"
chmod 600 "$report"
printf 'PASS restore drill: %s assets restored and rehashed; report=%s\n' "$checked" "$report"
