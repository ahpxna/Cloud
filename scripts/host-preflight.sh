#!/usr/bin/env bash
# Read-only launch gate for the Linux media host. It does not format, mount,
# encrypt, or alter disks. The media mount must resolve through an active
# dm-crypt/LUKS mapping; an unrelated /etc/crypttab entry is not evidence.
set -euo pipefail

failures=0
warnings=0

pass() { printf 'PASS  %s\n' "$*"; }
warn() { printf 'WARN  %s\n' "$*"; warnings=$((warnings + 1)); }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }

require_command() {
  if command -v "$1" >/dev/null 2>&1; then
    pass "command available: $1"
  else
    fail "missing command: $1"
  fi
}

if [[ "$(uname -s)" != "Linux" ]]; then
  fail "this preflight must run on the future Linux host, not $(uname -s)"
  exit 2
fi

media_mount="${PHOTO_MEDIA_MOUNT:-}"
media_uuid="${PHOTO_MEDIA_DEVICE_UUID:-}"
minimum_free_gib="${PHOTO_MEDIA_MIN_FREE_GIB:-200}"

if [[ -z "$media_mount" ]]; then
  fail "set PHOTO_MEDIA_MOUNT to the mounted media volume, e.g. /srv/family-photo"
fi
if [[ -z "$media_uuid" ]]; then
  fail "set PHOTO_MEDIA_DEVICE_UUID to the expected filesystem UUID"
fi
if ! [[ "$minimum_free_gib" =~ ^[0-9]+$ ]]; then
  fail "PHOTO_MEDIA_MIN_FREE_GIB must be a whole number"
fi

for tool in findmnt findfs df stat lsblk cryptsetup docker; do
  require_command "$tool"
done

if [[ -n "$media_mount" && -d "$media_mount" ]]; then
  source_device="$(findmnt --noheadings --output SOURCE --target "$media_mount" 2>/dev/null | xargs || true)"
  filesystem="$(findmnt --noheadings --output FSTYPE --target "$media_mount" 2>/dev/null | xargs || true)"
  if [[ -z "$source_device" ]]; then
    fail "$media_mount is not a mounted filesystem"
  else
    pass "$media_mount is mounted from $source_device"
  fi

  case "$filesystem" in
    ext4|xfs) pass "media filesystem is $filesystem" ;;
    *) fail "media filesystem must be ext4 or xfs; found ${filesystem:-unknown}" ;;
  esac

  if [[ "$media_mount" == "/" ]]; then
    fail "media volume must be separate from the operating-system root volume"
  fi

  if [[ -n "$media_uuid" ]]; then
    expected_source="$(findfs UUID="$media_uuid" 2>/dev/null || true)"
    if [[ -z "$expected_source" ]]; then
      fail "no local filesystem has UUID $media_uuid"
    elif [[ "$expected_source" != "$source_device" ]]; then
      fail "mounted source $source_device does not match UUID $media_uuid ($expected_source)"
    else
      pass "mounted filesystem UUID matches the expected UUID"
    fi
  fi

  if [[ "$minimum_free_gib" =~ ^[0-9]+$ ]]; then
    available_bytes="$(df -PB1 "$media_mount" | awk 'NR == 2 { print $4 }')"
    required_bytes=$((minimum_free_gib * 1024 * 1024 * 1024))
    if [[ -n "$available_bytes" && "$available_bytes" -ge "$required_bytes" ]]; then
      pass "free space is at least ${minimum_free_gib} GiB"
    else
      fail "free space is below ${minimum_free_gib} GiB"
    fi
  fi

  if findmnt --noheadings --output OPTIONS --target "$media_mount" | tr ',' '\n' | grep -qx 'ro'; then
    fail "media filesystem is read-only"
  else
    pass "media filesystem is writable"
  fi

  encryption_verified=0
  if [[ -n "$source_device" && "$source_device" == /dev/* ]] && command -v lsblk >/dev/null 2>&1 && command -v cryptsetup >/dev/null 2>&1; then
    while read -r ancestor ancestor_type ancestor_fstype; do
      [[ -n "$ancestor" ]] || continue
      if [[ "$ancestor_type" != "crypt" ]]; then
        continue
      fi
      crypt_name="$(basename "$ancestor")"
      status_output="$(cryptsetup status "$crypt_name" 2>&1 || true)"
      if ! grep -Eqi '^[[:space:]]*type:[[:space:]]*LUKS[12]?' <<<"$status_output"; then
        fail "dm-crypt ancestor $ancestor is not reported as an active LUKS mapping; run preflight with sufficient privilege"
        continue
      fi
      backing_device="$(lsblk -npo PKNAME "$ancestor" 2>/dev/null | head -n1 | xargs || true)"
      backing_fstype=""
      if [[ -n "$backing_device" ]]; then
        backing_fstype="$(lsblk -ndo FSTYPE "$backing_device" 2>/dev/null | head -n1 | xargs || true)"
      fi
      if [[ "$backing_fstype" != "crypto_LUKS" ]]; then
        fail "active crypt mapping $ancestor does not trace to a crypto_LUKS backing device (${backing_device:-unknown}, fstype=${backing_fstype:-unknown})"
        continue
      fi
      pass "media mount traces through active LUKS mapping $ancestor backed by $backing_device"
      encryption_verified=1
      break
    done < <(lsblk -s -n -p -o NAME,TYPE,FSTYPE "$source_device" 2>/dev/null || true)
  fi
  if [[ "$encryption_verified" -ne 1 ]]; then
    fail "could not prove that $media_mount is backed by an active LUKS dm-crypt mapping"
  fi
else
  [[ -n "$media_mount" ]] && fail "media mount path does not exist: $media_mount"
fi

if [[ -r /etc/crypttab ]] && grep -Eq '^[[:space:]]*[^#[:space:]]+' /etc/crypttab; then
  pass "/etc/crypttab declares at least one encrypted-volume mapping (informational; mount tracing above is authoritative)"
else
  warn "/etc/crypttab has no active mapping declaration; ensure encrypted media unlock is intentionally managed"
fi

if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
  pass "Docker Compose is available"
else
  fail "Docker Compose plugin is not available"
fi

if [[ "$failures" -gt 0 ]]; then
  printf 'Host preflight failed: %d failure(s), %d warning(s). Do not store family originals.\n' "$failures" "$warnings" >&2
  exit 1
fi

printf 'Host preflight passed with %d warning(s). Continue with disposable fixtures only.\n' "$warnings"
