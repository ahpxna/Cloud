# Remaining Gap Remediation — 2026-08-25

This round follows the full race/vet/build/migration/integrity checks that passed
on the prior source. It addresses gaps that were previously blocked by macOS,
Docker Desktop port forwarding, unavailable full Xcode, or external deployment
evidence. Source changes do not claim that Linux/LUKS, Cloudflare, restic, or a
physical iPhone have been commissioned.

## Implemented in this round

- **MFA sensitive-action abuse control:** migration `0008` adds durable per-user
  budgets for `confirm`, `recovery`, and `disable`. Five attempts are accepted in
  a five-minute window; subsequent requests are rejected until the window
  expires. Successful actions clear only their own budget. Cloudflare receives
  a defense-in-depth rule for the same routes.
- **Atomic recovery-code rotation:** accepting the TOTP counter and replacing all
  recovery-code hashes now happens in one PostgreSQL transaction. Replacement
  failure rolls the TOTP counter back.
- **Real account/MFA PostgreSQL integration coverage:** the integration target now
  runs upload and account suites, including durable login/MFA throttles,
  challenge issuance, recovery-code one-use behavior, replay prevention, and
  the recovery-rotation rollback case.
- **Manifest verification and reconciliation:** `cmd/manifest` has sign, verify,
  and reconcile modes. Verification uses only an Ed25519 public key and can
  assert exact PostgreSQL evidence. Reconcile repairs only the narrow
  immutable-file-written/DB-row-missing crash window after signature
  verification.
- **Restore evidence:** restore drills rehash every live asset and verify every
  signed manifest, including version, asset count, canonical payload SHA-256,
  key ID, signature, and DB linkage.
- **Consistency-job serialization:** backup and integrity cycles use the same
  host-side lock so their evidence windows cannot overlap.
- **Linux encryption launch gate:** host preflight traces the configured media
  mount through its actual block-device ancestry and requires an active LUKS
  dm-crypt mapping backed by `crypto_LUKS`. An unrelated `/etc/crypttab` entry
  is informational only.
- **iOS verification recovery:** server verification pending state survives
  transient status failures, retries on exponential backoff, network recovery,
  and foreground activation instead of becoming indefinitely stuck after a
  fixed ~20-second polling window.
- **iOS Keychain update safety:** existing credentials are updated in place;
  `SecItemAdd` is used only when the item does not exist, removing the
  delete-before-add loss window.
- **Release supply chain:** every one of the eight deployable Docker targets is
  built with release-time immutable Dockerfile base refs, pushed by immutable
  digest, keyless-signed, and receives CycloneDX SBOM and provenance attestations
  on release tags. Operator helpers resolve/verify digest pins for upstream
  Compose images without silently editing `.env`.
- **Docker Desktop synthetic fallback:** an explicit in-network probe mode
  authorizes plain HTTP only for an exact operator-specified hostname. The Make
  target fixes that hostname to the private Compose service `upload-gateway`;
  generic private HTTP remains rejected.

## Evidence still external

The patch cannot prove a real Cloudflare ruleset apply, an encrypted Linux media
host, a configured immutable/off-site restic repository, external Alertmanager
delivery, full Xcode dependency build, or physical-device background transfer
behavior. Those remain commissioning evidence and must not be marked complete
from source inspection alone.
