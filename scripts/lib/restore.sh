#!/usr/bin/env bash
# Shared snapshot layout and full-byte/signature checks. Caller supplies
# restore_query(SQL), target, and repo_root; failures propagate under set -e.

restore_snapshot() {
  : "${RESTIC_REPOSITORY:?set RESTIC_REPOSITORY}"
  : "${RESTIC_PASSWORD_FILE:?set RESTIC_PASSWORD_FILE}"
  snapshot="${RESTORE_SNAPSHOT:-latest}"
  echo "Restoring snapshot $snapshot into $target..."
  restic restore "$snapshot" --tag family-photo-cloud --target "$target"
  local found
  local dumps=()
  while IFS= read -r found; do dumps+=("$found"); done < <(find "$target" -type f -name postgres.dump -print)
  [[ ${#dumps[@]} -eq 1 ]] || { echo "expected exactly one postgres.dump, found ${#dumps[@]}" >&2; return 1; }
  dump="${dumps[0]}"
  metadata="$(dirname "$dump")/backup-metadata.txt"
  [[ -f "$metadata" ]] || { echo "backup metadata missing" >&2; return 1; }
}

restore_source_path() {
  local key="$1" value candidate found
  local matches=()
  value="$(sed -n "s/^${key}=//p" "$metadata")"
  [[ -n "$value" && "$value" != *$'\n'* ]] || { echo "missing/ambiguous backup path: $key" >&2; return 1; }
  if [[ "$value" == /* ]]; then
    candidate="$(realpath "$target/${value#/}")"
  else
    # Older backups recorded relative paths; accept only one matching directory.
    while IFS= read -r found; do matches+=("$found"); done < <(find "$target" -type d -name "$(basename "$value")" -print)
    [[ ${#matches[@]} -eq 1 ]] || { echo "ambiguous legacy backup path: $key" >&2; return 1; }
    candidate="$(realpath "${matches[0]}")"
  fi
  [[ "$candidate" == "$target/"* && -d "$candidate" ]] || { echo "missing/unsafe backup path: $key" >&2; return 1; }
  printf '%s\n' "$candidate"
}

restore_verify() {
  asset_count="$(restore_query "SELECT count(*) FROM assets WHERE deleted_at IS NULL")"
  manifest_count="$(restore_query "SELECT count(*) FROM signed_manifests")"

  checked=0
  if [[ "$asset_count" != "0" ]]; then
    media_root="$(restore_source_path media_root)"
    restore_query "SELECT storage_key, encode(content_sha256,'hex'), byte_size FROM assets WHERE deleted_at IS NULL ORDER BY id" > "$target/asset-rows.tsv"
    while IFS=$'\t' read -r storage_key expected_hex expected_size; do
      [[ -n "$storage_key" ]] || continue
      case "$storage_key" in
        originals/*) ;;
        *) echo "unsafe original storage key: $storage_key" >&2; return 1 ;;
      esac
      path="$(realpath "$media_root/$storage_key")"
      [[ "$path" == "$media_root/"* ]] || { echo "original escapes media root" >&2; return 1; }
      [[ -f "$path" ]] || { echo "missing restored original: $storage_key" >&2; exit 1; }
      actual_size="$(wc -c <"$path" | tr -d '[:space:]')"
      [[ "$actual_size" == "$expected_size" ]] || { echo "size mismatch: $storage_key" >&2; exit 1; }
      actual_hex="$(sha256sum "$path" | awk '{print $1}')"
      [[ "$actual_hex" == "$expected_hex" ]] || { echo "SHA-256 mismatch: $storage_key" >&2; exit 1; }
      checked=$((checked + 1))
    done < "$target/asset-rows.tsv"
  fi

  manifests_verified=0
  if [[ "$manifest_count" != "0" ]]; then
    keyring_path="$(restore_source_path manifest_public_keyring)"
    restored_manifest_root="$(restore_source_path manifest_root)"
    # A disaster drill must not silently depend on the live host's trust directory.
    # Restore the backed-up public keyring, then authenticate it against a small
    # operator-held fingerprint file that is deliberately kept outside restic.
    : "${MANIFEST_TRUST_FINGERPRINTS_FILE:?set MANIFEST_TRUST_FINGERPRINTS_FILE to an independent sha256sum file}"
    [[ -r "$MANIFEST_TRUST_FINGERPRINTS_FILE" ]] || { echo "manifest trust fingerprints are not readable: $MANIFEST_TRUST_FINGERPRINTS_FILE" >&2; exit 2; }
    trust_fingerprints="$(realpath "$MANIFEST_TRUST_FINGERPRINTS_FILE")"
    case "$trust_fingerprints" in
      "$(realpath "$target")"/*) echo "trust fingerprints must be independent of the restored snapshot" >&2; exit 2 ;;
    esac

    # sha256sum -c validates listed files but ignores unexpected extras. Require
    # the restored keyring to contain exactly the independently-approved set so a
    # tampered snapshot cannot smuggle in an untrusted signing key ID.
    if ! diff -u \
        <(awk '{name=$2; sub(/^\*/, "", name); print name}' "$trust_fingerprints" | LC_ALL=C sort -u) \
        <(find "$keyring_path" -maxdepth 1 -type f -name '*.pem' -exec basename {} \; | LC_ALL=C sort -u); then
      echo "restored manifest keyring differs from independent trust-anchor set" >&2
      exit 1
    fi
    (cd "$keyring_path" && sha256sum --strict --check "$trust_fingerprints")

    restored_manifest_count="$(find "$restored_manifest_root" -maxdepth 1 -type f -name 'manifest-*.json' -print | wc -l | tr -d ' ')"
    if [[ "$restored_manifest_count" != "$manifest_count" ]]; then
      echo "signed manifest file/DB count mismatch: files=$restored_manifest_count db=$manifest_count" >&2
      exit 1
    fi

    # A Mac snapshot can retain a different UID than the Linux runtime user.
    # Copy authenticated public inputs as the operator and run the verifier as
    # that same UID; do not relax permissions on the recovered library.
    verify_input="$(mktemp -d "$target/manifest-verify.XXXXXX")"
    mkdir "$verify_input/keyring"
    cp "$keyring_path/"*.pem "$verify_input/keyring/"
    chmod 700 "$verify_input/keyring"
    chmod 600 "$verify_input/keyring/"*.pem
    # Build once, without starting live DB dependencies. Each DB row is
    # asserted against the exact signed file.
    MANIFEST_PUBLIC_KEYRING_HOST_PATH="$verify_input/keyring" docker compose --profile integrity build manifest-verify >/dev/null
    restore_query "SELECT manifest_version, object_key, asset_count, encode(payload_sha256,'hex'), signing_key_id, encode(signature,'hex') FROM signed_manifests ORDER BY generated_at, id" > "$target/manifest-rows.tsv"
    while IFS=$'\t' read -r version object_key db_asset_count payload_hex key_id signature_hex; do
      [[ "$object_key" == manifests/* ]] || { echo "unexpected manifest object key: $object_key" >&2; exit 1; }
      relative="${object_key#manifests/}"
      [[ -n "$relative" && "$relative" != */* && "$relative" != "." && "$relative" != ".." ]] || {
        echo "unsafe manifest object key: $object_key" >&2
        exit 1
      }
      manifest_file="$restored_manifest_root/$relative"
      [[ -f "$manifest_file" ]] || { echo "restored manifest missing for DB row: $object_key" >&2; exit 1; }
      cp "$manifest_file" "$verify_input/$relative"
      chmod 600 "$verify_input/$relative"

      # -T and </dev/null: `docker compose run` would otherwise consume the
      # loop's stdin and silently end the loop after the first manifest.
      MANIFEST_PUBLIC_KEYRING_HOST_PATH="$verify_input/keyring" docker compose --profile integrity run --rm --no-deps -T \
        --user "$(id -u):$(id -g)" -v "$verify_input/$relative:/verify/$relative:ro" \
        manifest-verify \
        -mode verify \
        -input "/verify/$relative" \
        -object-key "$object_key" \
        -expected-version "$version" \
        -expected-asset-count "$db_asset_count" \
        -expected-payload-sha256 "$payload_hex" \
        -expected-key-id "$key_id" \
        -expected-signature-hex "$signature_hex" </dev/null
      manifests_verified=$((manifests_verified + 1))
    done < "$target/manifest-rows.tsv"
  fi

  # Never report PASS for a partial check.
  if [[ "$checked" != "$asset_count" ]]; then
    echo "rehashed $checked of $asset_count restored assets" >&2
    exit 1
  fi
  if [[ "$manifests_verified" != "$manifest_count" ]]; then
    echo "verified $manifests_verified of $manifest_count signed manifests" >&2
    exit 1
  fi

}
