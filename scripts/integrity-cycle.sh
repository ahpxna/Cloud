#!/usr/bin/env bash
# Run a full-byte scrub before producing and verifying a new immutable signed inventory.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
# shellcheck source=scripts/lib/operator-lock.sh
source "$repo_root/scripts/lib/operator-lock.sh"
load_dotenv "$repo_root/.env"

: "${MANIFEST_SIGNING_KEY_ID:?set MANIFEST_SIGNING_KEY_ID}"
key_path="${MANIFEST_KEY_HOST_PATH:-$repo_root/.data/secrets/manifest-ed25519.pem}"
public_key_path="${MANIFEST_PUBLIC_KEY_HOST_PATH:-$repo_root/.data/secrets/manifest-ed25519-public.pem}"
output_dir="${MANIFEST_OUTPUT_DIR:-$repo_root/.data/manifests}"
[[ -f "$key_path" ]] || { echo "manifest key missing: $key_path" >&2; exit 2; }

# The public key is not secret. Generate it once from the configured private key
# when upgrading an older installation that did not persist a separate trust file.
if [[ ! -f "$public_key_path" ]]; then
  command -v openssl >/dev/null 2>&1 || { echo "openssl is required to derive the manifest public key" >&2; exit 2; }
  mkdir -p "$(dirname "$public_key_path")"
  openssl pkey -in "$key_path" -pubout -out "$public_key_path"
  chmod 0644 "$public_key_path" 2>/dev/null || true
fi

mkdir -p "$output_dir"
chmod 700 "$output_dir" 2>/dev/null || true
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
manifest_name="manifest-$stamp.json"

operator_lock_acquire
trap 'operator_lock_release' EXIT INT TERM

# Refuse to sign a new inventory if even one stored original fails a full read.
docker compose --profile integrity run --rm scrub -output "/reports/scrub-$stamp.json"
docker compose --profile integrity run --rm manifest \
  -output "/manifests/$manifest_name" \
  -object-key "manifests/$manifest_name"

# Independently verify the signature with the public trust key. This catches a
# damaged output file or a signing/verification key mismatch before the cycle is
# reported as successful.
docker compose --profile integrity run --rm manifest-verify \
  -mode verify \
  -input "/manifests/$manifest_name" \
  -expected-key-id "$MANIFEST_SIGNING_KEY_ID"

operator_lock_release
trap - EXIT INT TERM
printf 'PASS integrity cycle %s\n' "$stamp"
