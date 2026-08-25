# Family Photo Cloud

Integrity-first family photo storage with an iOS app, Share Extension, reliable
background uploads, per-user libraries, and a self-hosted origin.

## Current status

This repository now contains the first backend vertical slice: invite-only
accounts, Argon2id password verification, rotating refresh sessions, short-lived
access tokens, TOTP MFA with one-time recovery codes and durable sensitive-action throttling, durable login throttling, an
authenticated tus gateway, owner isolation, bounded upload concurrency and
creation rates, SHA-256 verification, fenced quarantine, crash recovery, and
durable no-overwrite commit. It now also has content-addressed per-owner deduplication,
quota/free-space admission control, stale-upload expiry, periodic reconciliation,
readiness checks, bounded password-hash concurrency, and refresh-token-family
replay revocation. The standalone `tusd-lab` remains intentionally unsafe and
must never be routed from the Internet.

Schema changes run through the `migrate` one-shot service, which records a
version and SHA-256 checksum for every migration. PostgreSQL's first-boot init
directory is not used because it silently skips upgrades of existing volumes.

The accepted direction is:

- Cloudflare Tunnel for the MVP ingress.
- tus 1.0 with 32 MiB requests for resumable MVP uploads.
- `tusd` as an upstream library/service; no custom wire protocol.
- A custom Go gateway for authentication, ownership, state transitions, final
  SHA-256 verification, durable commit, and audit events.
- SwiftUI plus Share Extension on iOS, with TUSKit behind an internal transport
  interface.
- Native PhotoKit background resource upload and the IETF resumable-upload
  protocol as the long-term transport after the 100 MB Cloudflare constraint is
  removed through public IP or a VPS/WireGuard path.

Read [the architecture](docs/architecture.md), [MVP API contract](docs/api.md),
[integrity manifest format](docs/integrity-manifest-v1.md), [iOS scaffold
notes](ios/README.md), [operator runbooks](docs/runbooks/), and
[upstream survey](docs/research/upstream-landscape.md) before implementing
clients or more services.

The backend is not the finished product yet. The iOS source includes a Share
Extension queue, TUSKit-based upload transport, private Library viewer, TOTP MFA
enrolment/login/recovery controls, corrupt-queue recovery, and an exportable
background-transfer diagnostic log, but full Xcode/physical-device acceptance
is still external evidence. The operator
plane now includes full-byte scrubs, scheduled signed-manifest cycles,
append-only audit export, private Prometheus/Grafana/Alertmanager, synthetic
upload/download integrity probes, restart/resume chaos testing, encrypted
restic backup tooling, and an isolated restore drill. These are source-ready
controls, not proof that a real off-site repository, alert receiver, or iPhone
acceptance test has been completed.

## Operator integrity and recovery

After creating the manifest signing key and a real restic repository, the main
operator entry points are:

```bash
make scrub
make integrity-cycle
make audit-export
make observability-up
make backup
make restore-drill
```

Use a dedicated probe account for `make synthetic-probe` and local
`make chaos-resume`. If Docker Desktop cannot publish the loopback gateway port,
use `make synthetic-probe-docker` to exercise the same path over the isolated
Compose ingress network; its HTTP exception is exact-host and test-only. See the integrity, backup/restore, observability, synthetic
probe, and supply-chain runbooks before scheduling these jobs.

## Budget-first order

The current accepted path is documented in
[ADR-0006](docs/adr/0006-complete-free-first-before-storage-purchase.md):
complete source checks, local-only operator tests, and host preflight without
new recurring cost; buy the 20 TB media disk next; then fund encrypted off-site
backup and prove a restore before enabling public family use. A single 20 TB
disk is primary storage, not a backup. See the
[free-first commissioning checklist](docs/runbooks/free-first-commissioning.md).

## Production security gate

This repository is now being operated toward a NIST CSF 2.0 Tier 2 target and
an ISO/IEC 27001:2022-aligned ISMS. That does **not** mean it is ISO certified.
Before any family member treats it as their only backup, every P0 control in the
[control matrix](docs/security/control-matrix.md) must have dated evidence,
including encrypted off-site backups and a successful restore drill. Read the
[risk register](docs/security/risk-register.md) and [incident runbook](docs/runbooks/security-incident.md).

## Safe local start

```bash
make env
make config
make db-up
```

Generate three independent gateway secrets in the untracked `.env`, then start
the gateway. Never reuse a key across these purposes:

```bash
openssl rand -base64 32  # ACCESS_TOKEN_HMAC_KEY_BASE64
openssl rand -base64 32  # LOGIN_THROTTLE_HMAC_KEY_BASE64
openssl rand -base64 32  # MFA_ENCRYPTION_KEY_BASE64 (exactly 32 decoded bytes)
make gateway-up
make create-user EMAIL=parent@example.com
```

After configuring a named Cloudflare Tunnel whose public hostname targets
`http://upload-gateway:8080`, add its scoped token to `.env`. Review and apply
the versioned defense-in-depth rules in `infra/cloudflare/` with a scoped Zone
WAF token, then run:

```bash
make edge-up
```

For isolated tus protocol experiments only:

```bash
make protocol-lab-up
```

The lab endpoint binds to `127.0.0.1:1080`; do not publish it.

### Schema upgrades

For a fresh host, `migrate` runs automatically before the gateway/admin/manifest
services. Never edit a migration that has reached a host: the runner rejects a
checksum mismatch. Take a verified backup before any upgrade.

The only exception is a one-time upgrade from the old pre-ledger Compose
deployment. After independently confirming which SQL files were already
applied, set `PHOTO_MIGRATION_BASELINE_VERSION` to that exact version for one
run, then remove it. The runner fingerprints expected column types/nullability,
indexes, foreign keys and checks through schema v9 before recording a baseline; presence-only
lookalike schemas are refused. This is explicit operator acknowledgement, not
an automatic guess about a production schema.

## Project rules

- Originals are immutable after a verified commit.
- A file is never visible as an asset until client and server SHA-256 values
  match and the durable commit state machine completes.
- User ownership comes from the authenticated server session, never arbitrary
  upload metadata.
- No public route may bypass the authenticated gateway.
- Only one writable upload gateway may own the media state machine at a time;
  startup holds a session-level PostgreSQL advisory lock and a second gateway
  fails closed.
- A single storage disk is not a backup.
- The gateway refuses new uploads when quota or the media free-space reserve
  would be exceeded; `507` is a safety result, not a transient client error.
  Quota is visible unique assets plus unique pending content; free-space
  reservation is only bytes not yet persisted by active TUS transfers.
- A verifier gets a PostgreSQL lease before entering the in-memory worker queue
  and renews it while hashing large videos, avoiding reconciliation-induced
  duplicate work.
- Startup and periodic reconciliation inspect durable tusd `.info` sidecars.
  A full staged file stuck in database state `uploading` after an event-boundary
  crash moves idempotently to verification rather than being expired.
- `/livez` only proves process liveness. `/readyz` also requires storage above
  its reserve and a responsive repository; Compose uses `/readyz`.
- The media filesystem must support hard links and durable directory `fsync`
  (use ext4 or XFS on Linux; do not use FAT/exFAT for originals).
