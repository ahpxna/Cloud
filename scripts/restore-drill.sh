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

manifests_verified=0
if [[ "$manifest_count" != "0" ]]; then
  # A disaster drill must not silently depend on the live host's trust directory.
  # Restore the backed-up public keyring, then authenticate it against a small
  # operator-held fingerprint file that is deliberately kept outside restic.
  : "${MANIFEST_TRUST_FINGERPRINTS_FILE:?set MANIFEST_TRUST_FINGERPRINTS_FILE to an independent sha256sum file}"
  [[ -r "$MANIFEST_TRUST_FINGERPRINTS_FILE" ]] || { echo "manifest trust fingerprints are not readable: $MANIFEST_TRUST_FINGERPRINTS_FILE" >&2; exit 2; }
  trust_fingerprints="$(realpath "$MANIFEST_TRUST_FINGERPRINTS_FILE")"
  case "$trust_fingerprints" in
    "$(realpath "$target")"/*) echo "trust fingerprints must be independent of the restored snapshot" >&2; exit 2 ;;
  esac

  mapfile -t restored_keyrings < <(find "$target" -type d -name manifest-public-keyring -print)
  if [[ ${#restored_keyrings[@]} -ne 1 ]]; then
    echo "expected exactly one restored manifest public keyring, found ${#restored_keyrings[@]}" >&2
    exit 1
  fi
  keyring_path="${restored_keyrings[0]}"
  # sha256sum -c validates listed files but ignores unexpected extras. Require
  # the restored keyring to contain exactly the independently-approved set so a
  # tampered snapshot cannot smuggle in an untrusted signing key ID.
  if ! diff -u \
      <(awk '{name=$2; sub(/^\*/, "", name); print name}' "$trust_fingerprints" | LC_ALL=C sort -u) \
      <(find "$keyring_path" -maxdepth 1 -type f -name '*.pem' -printf '%f\n' | LC_ALL=C sort -u); then
    echo "restored manifest keyring differs from independent trust-anchor set" >&2
    exit 1
  fi
  (cd "$keyring_path" && sha256sum --strict --check "$trust_fingerprints")

  mapfile -t restored_manifest_dirs < <(find "$target" -type d -name manifests -print)
  if [[ ${#restored_manifest_dirs[@]} -ne 1 ]]; then
    echo "expected exactly one restored manifests directory, found ${#restored_manifest_dirs[@]}" >&2
    exit 1
  fi
  restored_manifest_root="${restored_manifest_dirs[0]}"
  restored_manifest_count="$(find "$restored_manifest_root" -maxdepth 1 -type f -name 'manifest-*.json' -print | wc -l | tr -d ' ')"
  if [[ "$restored_manifest_count" != "$manifest_count" ]]; then
    echo "signed manifest file/DB count mismatch: files=$restored_manifest_count db=$manifest_count" >&2
    exit 1
  fi

  # Build once, then run the public-key-only verifier without starting live DB
  # dependencies. Each DB row is asserted against the exact signed file.
  MANIFEST_PUBLIC_KEYRING_HOST_PATH="$keyring_path" docker compose --profile integrity build manifest-verify >/dev/null
  while IFS=$'\t' read -r version object_key db_asset_count payload_hex key_id signature_hex; do
    [[ "$object_key" == manifests/* ]] || { echo "unexpected manifest object key: $object_key" >&2; exit 1; }
    relative="${object_key#manifests/}"
    [[ -n "$relative" && "$relative" != */* && "$relative" != "." && "$relative" != ".." ]] || {
      echo "unsafe manifest object key: $object_key" >&2
      exit 1
    }
    manifest_file="$restored_manifest_root/$relative"
    [[ -f "$manifest_file" ]] || { echo "restored manifest missing for DB row: $object_key" >&2; exit 1; }

    MANIFEST_PUBLIC_KEYRING_HOST_PATH="$keyring_path" docker compose --profile integrity run --rm --no-deps \
      -v "$manifest_file:/verify/manifest.json:ro" \
      manifest-verify \
      -mode verify \
      -input /verify/manifest.json \
      -expected-version "$version" \
      -expected-asset-count "$db_asset_count" \
      -expected-payload-sha256 "$payload_hex" \
      -expected-key-id "$key_id" \
      -expected-signature-hex "$signature_hex"
    manifests_verified=$((manifests_verified + 1))
  done < <(docker exec -e PGPASSWORD="$password" "$container" psql -At -F $'\t' -U photo_cloud -d photo_cloud -c \
    "SELECT manifest_version, object_key, asset_count, encode(payload_sha256,'hex'), signing_key_id, encode(signature,'hex') FROM signed_manifests ORDER BY generated_at, id")
fi

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
