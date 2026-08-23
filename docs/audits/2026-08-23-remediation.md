# 2026-08-23 audit remediation

This records the disposition of the supplied independent code audit. It is an
engineering remediation record, not a production-launch approval.

## Remediated in source and regression tests

| Audit finding | Change | Evidence |
|---|---|---|
| Extension-dependent dedup paths could orphan blobs | Per-owner content-addressed path is now extensionless; `available` sessions reference either their newly inserted asset or the existing deduplicated asset | `TestProcessorDeduplicatesDifferentExtensionsToOneCanonicalObject`; PostgreSQL integration migration test |
| Login limiter had unbounded keys and unbounded Argon2 work | Expiring bounded cache evicts oldest entries when saturated, plus a global four-worker password verification gate | account limiter unit tests; `go test -race ./...` |
| iOS queue could crash before a transfer intent existed | Desired transfer is saved before TUS enqueue; recovery re-enqueues `created` sessions or resumes stored TUS context; unresolvable missing local context is explicit and retains the original | Swift source parse; full Xcode/device test remains required |
| iOS library ignored `next_cursor` | Cursor pagination and explicit Load More fallback are present | `LibraryPaginationTests` |
| Share Extension imported only one provider | It now attempts every selected image/movie provider, retaining each successful queue record | Swift source parse; full Xcode acceptance remains required |
| SHA-256 ran on MainActor | Hashing moved to a utility-priority detached worker | Swift source parse; device performance measurement remains required |
| Refresh race | `AuthenticationStore` has one shared refresh task | Swift source parse; XCTest pending full Xcode |
| Completed local payloads leaked | Available-only idempotent queue cleanup is implemented | `AppGroupQueueTests` |
| Final PATCH could be durable while in-memory completion event was lost | Periodic recovery inspects complete tusd sidecars, moves `uploading` to `received`, then claims verification | `TestCompletedTusUploadsFindsDurableFinalByteAfterEventLoss` |
| Startup recovery was capped/one-shot and could duplicate large hash jobs | Periodic reconciliation uses a durable PostgreSQL verification lease with renewal | PostgreSQL integration lease assertion |
| Expiry existed only in schema | Expiry state, staging cleanup, event, and retry of the same idempotency key added | `TestProcessorExpiresIncompleteSessionAndPermitsSameClientRetry` |
| Health endpoint was only liveness | `/livez` is liveness; `/readyz` checks media free reserve, staging write/sync/removal, and repository responsiveness | Compose healthcheck targets `/readyz` |
| No storage admission | Per-user unique-asset quota plus pending reservation and remaining-byte global free-space reservation with `507` response | PostgreSQL integration test |
| PostgreSQL init scripts could not upgrade an existing volume | Checksum-protected transactional migration runner gates dependent services | PostgreSQL integration migration-ledger assertion |
| Refresh replay lacked family revocation | Migration adds session family/parent/reuse fields; replay revokes active family descendants | account repository implementation; production migration evidence pending |
| CI scanned Docker's last stage instead of the public gateway | CI/security build and scan gateway, admin, manifest, and migration targets independently; Docker default is gateway | workflow matrix + Dockerfile target layout |
| Tracked compiled manifest binary | Removed and ignored local binary artifact | Git index change |
| Go patch level/config fallback | Go 1.26.7 used in module, Docker, CI; duration configuration now fails closed | race suite and Docker build |

## Intentionally still blocked outside source code

These findings need real authority, money, host hardware, identity policy, or
physical-device evidence. They are not marked complete merely because a
document exists:

- MFA enrollment/recovery policy and a tested client UX;
- Cloudflare edge IP/ASN/WAF rate rules and a real public-host test;
- disk encryption, firewall/VPN admin, SMART monitoring, and host evidence;
- encrypted off-site 3-2-1 backup, immutable/offline generation, and restore
  drill;
- off-host protected audit export, alert routing, and incident tabletop;
- full Xcode compile, TUSKit package build, Share Sheet, background/reboot,
  airplane mode, Wi-Fi/cellular, and large-video acceptance tests;
- PhotoKit bulk import, derivatives/thumbnails, metadata, timeline and other
  photo-product work.

Until those controls have dated evidence, this remains an alpha/private-lab
system. It may not be the sole copy of family originals or be exposed as a
general public service.
