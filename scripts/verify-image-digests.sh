#!/usr/bin/env bash
# Fail unless every externally pulled production Compose image is immutable.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
[[ -f "$repo_root/.env" ]] && load_dotenv "$repo_root/.env"

failed=0
check() {
  local variable="$1" value="$2"
  if [[ "$value" =~ @sha256:[0-9a-f]{64}$ ]]; then
    printf 'PASS  %s is digest-pinned\n' "$variable"
  else
    printf 'FAIL  %s is mutable: %s\n' "$variable" "$value" >&2
    failed=1
  fi
}
check POSTGRES_IMAGE "${POSTGRES_IMAGE:-postgres:18.4-alpine}"
check TUSD_IMAGE "${TUSD_IMAGE:-ghcr.io/tus/tusd:v2.10.0}"
check CLOUDFLARED_IMAGE "${CLOUDFLARED_IMAGE:-cloudflare/cloudflared:2026.7.2}"
check PROMETHEUS_IMAGE "${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0}"
check ALERTMANAGER_IMAGE "${ALERTMANAGER_IMAGE:-prom/alertmanager:v0.28.1}"
check GRAFANA_IMAGE "${GRAFANA_IMAGE:-grafana/grafana:12.1.0}"
exit "$failed"
