# Scheduled Integrity Cycle

The integrity cycle separates **byte verification** from **inventory signing**.
A signed inventory is produced only after every visible original can be read and
its SHA-256 matches PostgreSQL.

## One-time key setup

Keep the private signing key off Git and out of the gateway container:

```bash
mkdir -p .data/secrets
openssl genpkey -algorithm ED25519 -out .data/secrets/manifest-ed25519.pem
openssl pkey -in .data/secrets/manifest-ed25519.pem -pubout -out .data/secrets/manifest-ed25519-public.pem
chmod 600 .data/secrets/manifest-ed25519.pem
chmod 644 .data/secrets/manifest-ed25519-public.pem
printf '%s\n' 'MANIFEST_SIGNING_KEY_ID=home-manifest-2026-01' >> .env
```

Record the corresponding public key in protected operator documentation before
using the key to establish long-term integrity evidence.

## Manual cycle

```bash
make integrity-cycle
```

The command first runs `cmd/scrub`, which reads every non-deleted original and
appends a row to `asset_integrity_checks`. Any missing file, size mismatch,
SHA-256 mismatch, or read error is a hard failure. Only a clean scrub proceeds
to `cmd/manifest`, which writes a no-replace signed JSON inventory and records
its signature metadata in PostgreSQL. The cycle then independently verifies the
new file with the public key before reporting success. Backup and integrity jobs
share one operator lock and fail rather than overlap.

If the process crashes after the immutable file is created but before its DB row
is committed, verify/reconcile it without the private key:

```bash
docker compose --profile integrity run --rm manifest-verify \
  -mode reconcile \
  -input /manifests/manifest-YYYYMMDDTHHMMSSZ.json \
  -object-key manifests/manifest-YYYYMMDDTHHMMSSZ.json
```

Reconciliation inserts a missing row only after signature verification and fails
if an existing row disagrees with the signed file.

Never "repair" a mismatch before preserving the scrub report, relevant host
logs, and a copy of the affected object where possible.

## Scheduling

The repository includes a weekly systemd timer. The unit assumes the checkout
is deployed at `/opt/family-photo-cloud`; change `WorkingDirectory` and
`EnvironmentFile` before installing if the host uses another path.

```bash
sudo make install-systemd
sudo systemctl enable --now family-photo-cloud-integrity.timer
systemctl list-timers family-photo-cloud-integrity.timer
```

Timer installation is not evidence that a cycle succeeded. Retain dated scrub
reports and signed manifests off-host with backups.
