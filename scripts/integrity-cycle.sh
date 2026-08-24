#!/usr/bin/env bash
# Run a full-byte scrub before producing a new immutable signed inventory.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
load_dotenv "$repo_root/.env"

: "${MANIFEST_SIGNING_KEY_ID:?set MANIFEST_SIGNING_KEY_ID}"
key_path="${MANIFEST_KEY_HOST_PATH:-$repo_root/.data/secrets/manifest-ed25519.pem}"
output_dir="${MANIFEST_OUTPUT_DIR:-$repo_root/.data/manifests}"
[[ -f "$key_path" ]] || { echo "manifest key missing: $key_path" >&2; exit 2; }
mkdir -p "$output_dir"
chmod 700 "$output_dir" 2>/dev/null || true
stamp="$(date -u +%Y%m%dT%H%M%SZ)"

# Refuse to sign a new inventory if even one stored original fails a full read.
docker compose --profile integrity run --rm scrub -output "/reports/scrub-$stamp.json"
docker compose --profile integrity run --rm manifest \
  -output "/manifests/manifest-$stamp.json" \
  -object-key "manifests/manifest-$stamp.json"
printf 'PASS integrity cycle %s\n' "$stamp"
