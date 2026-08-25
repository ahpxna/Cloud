#!/usr/bin/env bash
# Shared consistency lock for operator jobs that must not overlap. Linux uses
# flock; other development hosts fall back to an atomic mkdir lock with stale
# PID reclamation so local drills remain usable.

operator_lock_acquire() {
  local lock_path="${PHOTO_OPERATOR_LOCK_PATH:-${TMPDIR:-/tmp}/family-photo-cloud-consistency.lock}"
  OPERATOR_LOCK_MODE=""
  OPERATOR_LOCK_PATH="$lock_path"

  if command -v flock >/dev/null 2>&1; then
    mkdir -p "$(dirname "$lock_path")"
    exec {OPERATOR_LOCK_FD}>"$lock_path"
    if ! flock -n "$OPERATOR_LOCK_FD"; then
      echo "another Family Photo Cloud consistency job holds $lock_path" >&2
      return 1
    fi
    OPERATOR_LOCK_MODE="flock"
    return 0
  fi

  local lock_dir="${lock_path}.d"
  if mkdir "$lock_dir" 2>/dev/null; then
    printf '%s\n' "$$" > "$lock_dir/pid"
    OPERATOR_LOCK_MODE="mkdir"
    OPERATOR_LOCK_PATH="$lock_dir"
    return 0
  fi
  if [[ -r "$lock_dir/pid" ]]; then
    local owner_pid
    owner_pid="$(cat "$lock_dir/pid" 2>/dev/null || true)"
    if [[ "$owner_pid" =~ ^[0-9]+$ ]] && ! kill -0 "$owner_pid" 2>/dev/null; then
      rm -rf "$lock_dir"
      if mkdir "$lock_dir" 2>/dev/null; then
        printf '%s\n' "$$" > "$lock_dir/pid"
        OPERATOR_LOCK_MODE="mkdir"
        OPERATOR_LOCK_PATH="$lock_dir"
        return 0
      fi
    fi
  fi
  echo "another Family Photo Cloud consistency job holds $lock_dir" >&2
  return 1
}

operator_lock_release() {
  case "${OPERATOR_LOCK_MODE:-}" in
    flock)
      flock -u "${OPERATOR_LOCK_FD}" 2>/dev/null || true
      exec {OPERATOR_LOCK_FD}>&- 2>/dev/null || true
      ;;
    mkdir)
      rm -rf "${OPERATOR_LOCK_PATH:-}" 2>/dev/null || true
      ;;
  esac
  OPERATOR_LOCK_MODE=""
}
