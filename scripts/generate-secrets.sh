#!/usr/bin/env bash
# Fill missing or placeholder secrets in .env without printing them, and add
# any keys that .env.example gained since this .env was created.
#
# Existing real values are never rotated: changing POSTGRES_PASSWORD after the
# volume is initialised locks the bootstrap role out, and rotating the gateway
# keys signs every device out and makes stored MFA secrets undecryptable.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
env_file="${1:-.env}"
example=".env.example"
command -v openssl >/dev/null 2>&1 || { echo "openssl is required" >&2; exit 2; }
[[ -f "$example" ]] || { echo "$example is missing" >&2; exit 2; }
umask 077
[[ -f "$env_file" ]] || cp "$example" "$env_file"
chmod 600 "$env_file"

password_keys=(
  POSTGRES_PASSWORD GATEWAY_DB_PASSWORD ADMIN_DB_PASSWORD INTEGRITY_DB_PASSWORD
  READONLY_DB_PASSWORD BACKUP_DB_PASSWORD SESSION_MAINTENANCE_DB_PASSWORD
)
key_keys=(
  ACCESS_TOKEN_HMAC_KEY_BASE64 LOGIN_THROTTLE_HMAC_KEY_BASE64
  MFA_ENCRYPTION_KEY_BASE64 REFRESH_RETRY_ENCRYPTION_KEY_BASE64
)

current_value() {
  # Last assignment wins, matching Compose's .env parsing.
  awk -v key="$1" 'index($0, key "=") == 1 { value = substr($0, length(key) + 2); found = 1 }
    END { if (found) print value }' "$env_file"
}

is_placeholder() {
  [[ -z "$1" || "$1" == replace-with-* || "$1" == change-me* ]]
}

set_value() {
  local key="$1" value="$2" tmp
  tmp="$(mktemp "${env_file}.XXXXXX")"
  if grep -q "^${key}=" "$env_file"; then
    KEY="$key" VALUE="$value" awk 'index($0, ENVIRON["KEY"] "=") == 1 { print ENVIRON["KEY"] "=" ENVIRON["VALUE"]; next } { print }' \
      "$env_file" >"$tmp"
  else
    cat "$env_file" >"$tmp"
    printf '%s=%s\n' "$key" "$value" >>"$tmp"
  fi
  chmod 600 "$tmp"
  mv "$tmp" "$env_file"
}

ensure_secret() {
  local key="$1" kind="$2" value
  value="$(current_value "$key")"
  if ! is_placeholder "$value"; then
    echo "kept      $key"
    return
  fi
  if [[ "$kind" == password ]]; then
    value="$(openssl rand -hex 32)"
  else
    value="$(openssl rand -base64 32)"
  fi
  set_value "$key" "$value"
  echo "generated $key"
}

# Add keys introduced in .env.example since this .env was copied.
if [[ -s "$env_file" && -n "$(tail -c1 "$env_file")" ]]; then
  printf '\n' >>"$env_file"
fi
while IFS= read -r line; do
  [[ "$line" =~ ^([A-Z0-9_]+)= ]] || continue
  key="${BASH_REMATCH[1]}"
  if ! grep -q "^${key}=" "$env_file"; then
    printf '%s\n' "$line" >>"$env_file"
    echo "added     $key (example default)"
  fi
done <"$example"

for key in "${password_keys[@]}"; do ensure_secret "$key" password; done
for key in "${key_keys[@]}"; do ensure_secret "$key" key; done

grafana_file="$(current_value GRAFANA_ADMIN_PASSWORD_FILE)"
grafana_file="${grafana_file:-./.data/secrets/grafana-admin-password}"
if [[ ! -s "$grafana_file" ]]; then
  mkdir -p "$(dirname "$grafana_file")"
  openssl rand -hex 24 >"$grafana_file"
  chmod 600 "$grafana_file"
  echo "generated $grafana_file"
fi
echo "secrets are in $env_file (mode 600); values were not printed"
