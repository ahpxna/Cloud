# Cloud Proposal Implementation Bundle

Status date: 2026-08-24
Baseline: `Cloud-main.zip`, after the verifier claim-token and scoped-TUS hardening round.

This bundle closes the remaining **source-level** P0/P1 correctness/security
proposals discussed during the Cloud audits, then adds the agreed operational,
observability, failure-testing, backup, and supply-chain scaffolding. A source
patch is not treated as evidence that a provider, host, backup repository, or
iPhone has actually been configured or tested.

## Correctness and abuse-control remediations

| Proposal | Implementation | Source status |
|---|---|---|
| TUS expiry/restart crash window | Gateway inspects payload + `.info` under the same per-resource lock before deleting/resetting. A complete on-disk upload wins over stale DB state; malformed/inconsistent sidecars are isolated/preserved. | Implemented with regression tests |
| Verifier quarantine fencing | New `quarantining` DB state records intent under the verification claim token **before** filesystem mutation; recovery is idempotent and stale workers cannot terminal-transition a newer claim. | Implemented with regression tests |
| Durable login throttling | Per-normalized-identity HMAC state in PostgreSQL, process-wide token bucket/Argon2 gate, fail-closed in-memory fallback for unit fakes, indexed stale-row cleanup. Plain login identities are not persisted in throttle state. | Implemented |
| Upload-session creation throttling | Durable per-user PostgreSQL create window in the same transaction as admission; idempotent retries do not consume a new slot. | Implemented with regression test |
| Single writable gateway | Session-level PostgreSQL advisory lock is held on a dedicated pooled connection for process lifetime; a second writable gateway fails startup. | Implemented |
| Safe migration baseline | Pre-ledger baseline now fingerprints required column type/nullability, named indexes, foreign keys, and check constraints through schema v7; mismatches are refused. | Implemented |
| TOTP MFA | AES-256-GCM encrypted TOTP secret with separate key, five-minute one-time login challenge, ±1 TOTP window, replay counter, one-time hash-only recovery codes, durable per-user challenge issuance cap, and no refresh token before MFA. | Implemented backend + iOS UI; real enrolment/recovery evidence pending |
| Cloudflare edge rules | Terraform zone rulesets rate-limit login/MFA/refresh/session creation and enforce API/TUS method policy. TUS PATCH is deliberately not request-rate-limited at the edge. | Source ready; provider apply/public acceptance pending |
| iOS corrupt queue recovery | Malformed record metadata is isolated without deleting payload; an unambiguous image/video can be rebuilt under a **fresh UUID**, forcing byte-zero restart rather than unsafe resume. | Implemented with Swift regression tests |
| iOS background diagnostics | Bounded, rotated, token-redacted JSONL records queue/session/TUS IDs, offsets, app state, timestamps and failures; export UI is included. | Implemented; physical-device matrix pending |

## Operations and resilience proposals

| Proposal | Implementation in this bundle | Status after merge |
|---|---|---|
| Scheduled full-byte integrity scrub | `cmd/scrub`, append-only `asset_integrity_checks`, `scripts/integrity-cycle.sh`, systemd weekly timer | Source ready; first host run is evidence |
| Signed asset inventory kept outside gateway | Existing `cmd/manifest` wired into isolated `integrity` profile with key/output mounts | Source ready; key custody/evidence external |
| Synthetic upload/download SHA-256 probe | `cmd/synthetic-probe` exercises login, scoped TUS, resume, verification, library and download | Source ready; public-path run pending |
| Restart/resume chaos test | `scripts/chaos-resume.sh` restarts local gateway mid-transfer | Source ready; isolated netem loss/rate tests remain optional |
| Operational metrics | `cmd/metrics-exporter` exposes low-cardinality DB/storage/integrity metrics including `quarantining` backlog | Source ready |
| Prometheus/Grafana/Alertmanager | Loopback-only Compose profile, provisioned dashboard and alert rules | Source ready; external alert receiver/test pending |
| Protected audit export | `cmd/audit-export` creates immutable JSONL export + SHA-256 sidecar; backup includes exports | Source ready; off-host retention pending |
| Encrypted off-site backup | `scripts/backup-restic.sh` creates quiescent DB/media/evidence restic snapshot | Source ready; repository/provider not configured by code |
| Isolated restore verification | `scripts/restore-drill.sh` restores DB/media outside production and re-hashes every live asset | Source ready; successful dated drill pending |
| Scheduled backups | systemd daily backup timer | Source ready; do not enable before repository configuration |
| CI security matrix | Image/build/security jobs extended for the new commands and operational files | Source ready; GitHub execution pending |
| SBOM | Trivy CycloneDX artifact for every image target | Source ready; CI execution pending |
| Release image signing/provenance | Tag workflow pushes gateway digest, keyless-signs it with cosign, and attaches SBOM/provenance attestations | Source ready; first signed release pending |
| Private operator interface | Make targets for scrub, integrity, backup, restore, audit, observability and probes | Implemented |
| Kubernetes migration | Intentionally not added; unnecessary for the single-host family deployment | Deferred roadmap |
| Vault/Wazuh/OPA | Intentionally optional; no operational need demonstrated in this baseline | Optional |

## Controls this patch cannot truthfully mark complete

These remain deployment/acceptance gates rather than code TODOs:

- perform real TOTP enrolment, recovery-code recovery/rotation, MFA disable and
  access-review drills with the intended admin/member accounts;
- apply the versioned Cloudflare rulesets to the real zone and run the public
  TUS/login/MFA acceptance tests;
- verify encrypted Linux host/media/database volumes, VPN-only administration,
  firewall, patching, NTP, UPS and disk-health controls on the actual host;
- configure a genuinely off-site/immutable restic repository and complete a
  dated restore drill with measured RPO/RTO;
- connect/test an external Alertmanager receiver and retention policy;
- perform a full Xcode dependency/SDK build plus physical-iPhone Share Sheet,
  background URLSession, force-quit, reboot, lock, low-storage and network-
  transition matrix, using the exported diagnostics as evidence;
- run an incident tabletop and record the family notification/credential-
  rotation procedure.

Product roadmap changes such as thumbnails/EXIF derivatives or destructive
account/asset deletion are intentionally not hidden inside this security patch;
they alter retention and media-processing semantics and need separate designs.
