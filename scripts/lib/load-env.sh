#!/usr/bin/env bash
# Import a Compose-style KEY=VALUE file without eval. Existing environment
# variables win, so systemd/CI/operator overrides are preserved. Optional key
# arguments restrict imports; callers without them retain the full-file behavior.
load_dotenv() {
  local file="${1:-.env}" line key value first last wanted selected
  [[ $# -eq 0 ]] || shift
  [[ -f "$file" ]] || return 0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ -z "$line" || "$line" == \#* ]] && continue
    if [[ $# -gt 0 ]]; then
      selected=0
      for wanted in "$@"; do
        [[ "${line%%=*}" != "$wanted" ]] || selected=1
      done
      [[ $selected -eq 1 ]] || continue
    fi
    [[ "$line" == *=* ]] || { echo "invalid env line in $file" >&2; return 2; }
    key="${line%%=*}"
    value="${line#*=}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || { echo "invalid env key in $file" >&2; return 2; }
    [[ -n "${!key+x}" ]] && continue
    if (( ${#value} >= 2 )); then
      first="${value:0:1}"
      last="${value: -1}"
      if [[ ( "$first" == '"' && "$last" == '"' ) || ( "$first" == "'" && "$last" == "'" ) ]]; then
        value="${value:1:${#value}-2}"
      fi
    fi
    export "$key=$value"
  done < "$file"
}
