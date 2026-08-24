#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
# shellcheck source=scripts/lib/load-env.sh
source "$repo_root/scripts/lib/load-env.sh"
load_dotenv "$repo_root/.env"
out_dir="${AUDIT_EXPORT_DIR:-$repo_root/.data/audit}"
mkdir -p "$out_dir"
chmod 700 "$out_dir" 2>/dev/null || true
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
docker compose --profile audit run --rm audit-export -output "/audit/upload-events-$stamp.jsonl"
