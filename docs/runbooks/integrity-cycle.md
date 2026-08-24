# Scheduled Integrity Cycle

The integrity cycle separates **byte verification** from **inventory signing**.
A signed inventory is produced only after every visible original can be read and
its SHA-256 matches PostgreSQL.

## One-time key setup

Keep the private signing key off Git and out of the gateway container:

```bash
mkdir -p .data/secrets
openssl genpkey -algorithm ED25519 -out .data/secrets/manifest-ed25519.pem
chmod 600 .data/secrets/manifest-ed25519.pem
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
its signature metadata in PostgreSQL.

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
