#!/usr/bin/env bash
# Resolve mutable Compose image tags to immutable repository digests. This is a
# read-only helper: review its output, then copy the pinned refs into .env.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
[[ -f "$repo_root/.env" ]] && load_dotenv "$repo_root/.env"
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 2; }

resolve() {
  local variable="$1" tagged="$2" pinned
  docker pull "$tagged" >/dev/null
  pinned="$(docker image inspect --format '{{index .RepoDigests 0}}' "$tagged")"
  if ! [[ "$pinned" =~ @sha256:[0-9a-f]{64}$ ]]; then
    echo "could not resolve immutable digest for $tagged" >&2
    exit 1
  fi
  printf '%s=%s\n' "$variable" "$pinned"
}

resolve POSTGRES_IMAGE "${POSTGRES_IMAGE:-postgres:18.4-alpine}"
resolve TUSD_IMAGE "${TUSD_IMAGE:-ghcr.io/tus/tusd:v2.10.0}"
resolve CLOUDFLARED_IMAGE "${CLOUDFLARED_IMAGE:-cloudflare/cloudflared:2026.7.2}"
resolve PROMETHEUS_IMAGE "${PROMETHEUS_IMAGE:-prom/prometheus:v3.5.0}"
resolve ALERTMANAGER_IMAGE "${ALERTMANAGER_IMAGE:-prom/alertmanager:v0.28.1}"
resolve GRAFANA_IMAGE "${GRAFANA_IMAGE:-grafana/grafana:12.1.0}"
