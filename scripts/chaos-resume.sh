#!/usr/bin/env bash
# Local-only recovery probe: start a deliberately slow TUS upload, restart the
# gateway mid-transfer, and require the probe to recover via HEAD/offset resume.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
load_dotenv "$repo_root/.env"

: "${PROBE_EMAIL:?set PROBE_EMAIL to a dedicated probe account}"
: "${PROBE_PASSWORD_FILE:?set PROBE_PASSWORD_FILE}"
base="${PROBE_BASE_URL:-http://127.0.0.1:${UPLOAD_GATEWAY_DEV_PORT:-8081}}"
bytes="${CHAOS_PROBE_BYTES:-67108864}"
chunk="${CHAOS_PROBE_CHUNK_BYTES:-1048576}"
delay="${CHAOS_PROBE_CHUNK_DELAY:-250ms}"

[[ "$base" == http://127.0.0.1:* || "$base" == http://localhost:* ]] || {
  echo "chaos-resume refuses non-loopback PROBE_BASE_URL" >&2; exit 2;
}

docker compose --profile gateway up -d --wait upload-gateway
(
  go run ./cmd/synthetic-probe \
    -base-url "$base" -allow-http \
    -email "$PROBE_EMAIL" -password-file "$PROBE_PASSWORD_FILE" \
    -bytes "$bytes" -chunk-bytes "$chunk" -chunk-delay "$delay" -timeout 5m
) &
probe_pid=$!
sleep "${CHAOS_RESTART_AFTER_SECONDS:-2}"
docker compose restart upload-gateway
wait "$probe_pid"
printf 'PASS gateway restart/resume chaos probe\n'
