# Signed Integrity Manifest Runbook

Status: tool, full-byte scrub, and scheduling source implemented; first host cycle remains deployment evidence.

Run this after the immutable-original backup succeeds, using a private key that
is not stored in the media mount or Docker environment of `upload-gateway`.

```bash
export DATABASE_URL='postgresql://photo_cloud_integrity:...@127.0.0.1:5432/photo_cloud'
export MANIFEST_SIGNING_KEY_ID='offline-ed25519-2026-01'
export MANIFEST_ED25519_PRIVATE_KEY_FILE='/secure/offline/manifest-ed25519.pem'

go run ./cmd/manifest \
  -output /secure/manifests/2026-08-20.json \
  -object-key manifests/2026-08-20.json
```

For the Compose deployment, prefer `make integrity-cycle`; it injects the
scoped `photo_cloud_integrity` credential. The bootstrap PostgreSQL credential
is only for migrations and role provisioning and must never be used by this
runtime integrity command.

The key file must contain a PKCS#8 Ed25519 private key PEM. The CLI can instead
read `MANIFEST_ED25519_PRIVATE_KEY_BASE64` (unpadded standard Base64, 64 raw
private-key bytes), but a read-only key file or secret manager is preferred.

The tool writes the JSON through a same-directory temporary file, file `fsync`,
atomic no-replace hard link, and directory `fsync`. It refuses both an existing
output path and an already-recorded `object_key`; historical manifests are
immutable. It then records the payload SHA-256 and signature in PostgreSQL. If
database recording fails after file creation, the file is a valid but
unregistered orphan: do not silently delete it; investigate and either record
it through a reconciliation tool or archive it as evidence.

This command signs metadata only. `cmd/scrub` separately opens each original,
re-hashes bytes, and appends `match`, `mismatch`, `missing`, or `io_error` to
`asset_integrity_checks`. Use `make integrity-cycle` so signing is refused when
the full-byte scrub fails; see `docs/runbooks/integrity-cycle.md`.
