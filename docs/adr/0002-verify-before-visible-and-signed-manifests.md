# ADR 0002: Verify Before Visible and Sign Integrity Manifests

Status: accepted  
Date: 2026-08-20

## Context

TLS protects traffic in motion but does not prove that a crash, disk error,
software defect, or later modification left the stored original intact. A
resumable protocol proves only its own offset contract unless the product also
verifies the completed representation.

`pki-sentinel` already contains a useful signed-baseline pattern: deterministic
sorting, SHA-256 identifiers, Ed25519 signatures, refusal to trust a modified
baseline, explicit drift events, and non-zero failure status.

## Decision

- Hash the exact original on both iOS and the origin using SHA-256.
- Keep completed upload data in staging until the full server hash matches the
  client-declared hash and byte count.
- Commit with file `fsync`, a same-filesystem no-replace hard link, directory
  `fsync`, staging unlink, and a database visibility transaction. Re-hash any
  pre-existing destination before deduplicating it.
- Make committed originals immutable and owner-scoped.
- Append every meaningful state transition to `upload_events`.
- Generate sorted nightly asset inventories and sign a versioned canonical
  payload with Ed25519. Manifest v1 is documented in
  [`docs/integrity-manifest-v1.md`](../integrity-manifest-v1.md).
- Run periodic full and sampled storage scrubs that re-read original bytes.

The signing key must not live beside the writable media library. Vault may hold
it later, but an offline or separately mounted key is sufficient for the first
single-host implementation.

## Consequences

An upload costs two full reads: one on the client and one on the server. That is
acceptable for the family scale and provides a strong integrity boundary.
Signed manifests detect inventory tampering but cannot repair damaged data;
repair requires a verified second copy and regular restore drills.
