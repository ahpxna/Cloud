# Integrity Manifest v1

Status: implemented canonical format; scheduled generator and storage scrubber pending.

The manifest is a signed inventory of assets that already reached `available`.
It detects an altered database inventory or unrecorded loss of originals; it
does not replace per-upload SHA-256 verification or a verified backup.

## Record fields

Each record contains:

- `asset_id`
- `owner_id`
- `storage_key`
- `byte_size`
- `content_sha256` (lowercase 64-character hexadecimal SHA-256)
- `verified_at`

The public JSON representation also has `manifest_version`, `generated_at`,
`signing_key_id`, `records`, and Base64URL-without-padding `signature`.

## Canonical payload

Ed25519 signs a custom byte payload, never JSON bytes. JSON serializers vary
across Go and Swift, while this length-prefixed format is deliberately simple
to implement independently.

1. Validate all fields and reject duplicate `asset_id` values.
2. Sort records by UTF-8 byte order of `owner_id`, then `asset_id`.
3. Emit the exact ASCII header:

```text
family-photo-cloud.asset-manifest/1\n
```

4. Emit manifest fields in this order: `manifest_version`, `generated_at`,
   `signing_key_id`, `record_count`.
5. For every record emit literal `record\n`, then `asset_id`, `owner_id`,
   `storage_key`, `byte_size`, `content_sha256`, and `verified_at` in that
   order.

Every field uses this exact byte form:

```text
<field-name><space><UTF-8-byte-length>:<value>\n
```

Dates are UTC `RFC3339Nano`, integers are base-10 without leading zeroes, and
the SHA-256 is lowercase hex. The field length means no delimiter ambiguity
even if a future string field permits a newline.

`internal/integrity/manifest.go` is the Go reference implementation. Its tests
prove order independence and signature rejection after any inventory change.

## Key handling

- Use Ed25519.
- Keep the private key outside the writable media mount and PostgreSQL volume.
- Give keys stable IDs such as `offline-ed25519-2026-01`.
- Keep the public key with backup/audit tooling.
- Rotate by issuing a new manifest with a new `signing_key_id`; retain old
  public keys for historical verification.

Do not claim that a local manifest proves storage is intact. A scrubber must
re-read originals, calculate their SHA-256, and compare against the manifest
and asset database; recovery requires an independently verified backup.
