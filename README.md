# Family Photo Cloud

Integrity-first family photo storage with an iOS app, Share Extension, reliable
background uploads, per-user libraries, and a self-hosted origin.

## Current status

This repository now contains the first backend vertical slice: invite-only
accounts, Argon2id password verification, rotating refresh sessions, short-lived
access tokens, an authenticated tus gateway, owner isolation, bounded upload
concurrency, SHA-256 verification, quarantine, crash recovery, and durable
no-overwrite commit. The standalone `tusd-lab` remains intentionally unsafe and
must never be routed from the Internet.

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
notes](ios/README.md), and [upstream survey](docs/research/upstream-landscape.md)
before implementing clients or more services.

The backend is not the finished product yet. The iOS source includes a Share
Extension queue, TUSKit-based upload transport, and private Library viewer, but
it is unbuilt on this machine because full Xcode is absent. Thumbnails,
scheduled manifest generation, backups, and physical-device background tests
remain before an App Store release.

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

Generate the gateway secret in the untracked `.env`, then start the gateway:

```bash
openssl rand -base64 32
# paste into ACCESS_TOKEN_HMAC_KEY_BASE64 in .env
make gateway-up
make create-user EMAIL=parent@example.com
```

After configuring a named Cloudflare Tunnel whose public hostname targets
`http://upload-gateway:8080`, add its scoped token to `.env` and run:

```bash
make edge-up
```

For isolated tus protocol experiments only:

```bash
make protocol-lab-up
```

The lab endpoint binds to `127.0.0.1:1080`; do not publish it.

## Project rules

- Originals are immutable after a verified commit.
- A file is never visible as an asset until client and server SHA-256 values
  match and the durable commit state machine completes.
- User ownership comes from the authenticated server session, never arbitrary
  upload metadata.
- No public route may bypass the authenticated gateway.
- A single storage disk is not a backup.
- The media filesystem must support hard links and durable directory `fsync`
  (use ext4 or XFS on Linux; do not use FAT/exFAT for originals).
