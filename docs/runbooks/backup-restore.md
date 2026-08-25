# Encrypted Backup and Restore Drill

This runbook provides source tooling for encrypted restic backups and isolated
restore verification. It does **not** turn an unconfigured host into a 3-2-1
backup system; repository credentials, provider retention/immutability, and
successful dated drills remain deployment evidence.

## Configure restic

Install `restic` on the Linux host and initialize a repository outside the
primary photo disk. Keep the repository password in a root-readable file, not
in `.env` or Git.

Required environment:

```text
RESTIC_REPOSITORY=<restic repository URI>
RESTIC_PASSWORD_FILE=/root/.config/family-photo-cloud/restic-password
MANIFEST_TRUST_FINGERPRINTS_FILE=/root/.config/family-photo-cloud/manifest-keyring.sha256
```

`MANIFEST_TRUST_FINGERPRINTS_FILE` is a tiny independent trust anchor and must
not live inside the restic repository or restore target. Create/update it after
each approved public-key rotation, for example:

```bash
( cd "$MANIFEST_PUBLIC_KEYRING_HOST_PATH" && sha256sum *.pem ) \
  > /root/.config/family-photo-cloud/manifest-keyring.sha256
chmod 600 /root/.config/family-photo-cloud/manifest-keyring.sha256
```

Keep a second protected copy of that fingerprint file with recovery material.

Backup and integrity cycles share one host-side consistency lock so manual or
scheduled runs cannot overlap. The backup script stops the upload gateway for a short quiescent window, writes
a PostgreSQL custom-format dump, snapshots the database dump plus media,
manifests, and audit exports into restic, runs a repository check, removes the
local dump, and restarts the gateway if it was running.

```bash
make backup
```

A backup is not accepted solely because `restic backup` exited zero. Configure
provider-side retention or an independent offline/immutable copy so compromise
of the home host cannot delete every snapshot.

## Isolated restore drill

```bash
make restore-drill
# or select a snapshot:
RESTORE_SNAPSHOT=<snapshot-id> make restore-drill
```

The drill restores into `.data/restore-drills/<timestamp>`, starts a disposable
PostgreSQL container, restores the dump there, locates restored originals, and
checks every live asset's byte size and SHA-256 against the restored database.
For every `signed_manifests` row it also requires the corresponding restored
manifest file. The drill now finds the **restored** public-key history from the
snapshot, authenticates every key against the independent SHA-256 fingerprint
file, then verifies Ed25519 signatures and asserts manifest version, asset count,
payload hash, signing key ID, and signature bytes against PostgreSQL. It no
longer silently uses the live host keyring. The private signing key is never
mounted in the restore verifier. It never connects to or overwrites the live
database/media path.

Keep `restore-drill-report.txt` as dated evidence. A quarterly production drill
should additionally record elapsed restore time, recovered snapshot age, RPO,
RTO, operator, repository/provider, and any corrective action.

## Scheduling

A daily systemd timer is included. As with the integrity timer, review the
`/opt/family-photo-cloud` paths before installation and do not enable it until a
real off-site repository has been initialized and tested.
