#!/usr/bin/env bash
# Restore a Family Photo Cloud restic snapshot into an isolated directory and
# disposable PostgreSQL container. The drill verifies database/media hashes and
# every signed-manifest signature/linkage without touching live state.
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

target="$(realpath "$target")"
source "$repo_root/scripts/lib/restore.sh"
restore_snapshot

container="family-photo-cloud-restore-drill-${RANDOM}-${RANDOM}"
password="restore-drill-${RANDOM}-${RANDOM}-${RANDOM}"
image="${POSTGRES_IMAGE:-postgres:18.6-alpine}"
cleanup() { docker rm -fv "$container" >/dev/null 2>&1 || true; }
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
  pg_restore --exit-on-error --clean --if-exists --no-owner --no-acl -U photo_cloud -d photo_cloud

restore_query() {
  docker exec -e PGPASSWORD="$password" "$container" psql -v ON_ERROR_STOP=1 -At -F $'\t' -U photo_cloud -d photo_cloud -c "$1"
}
restore_verify

report="$target/restore-drill-report.txt"
{
  printf 'completed_at=%s\n' "$stamp"
  printf 'snapshot=%s\n' "$snapshot"
  printf 'asset_rows=%s\n' "$asset_count"
  printf 'assets_rehashed=%s\n' "$checked"
  printf 'signed_manifests=%s\n' "$manifest_count"
  printf 'manifests_verified=%s\n' "$manifests_verified"
  printf 'result=PASS\n'
} > "$report"
chmod 600 "$report"
printf 'PASS restore drill: %s assets rehashed, %s signed manifests verified; report=%s\n' "$checked" "$manifests_verified" "$report"
