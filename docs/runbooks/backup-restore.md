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

## Restore into an empty serving stack

Use `RESTORE_SNAPSHOT=<snapshot-id> make restore-live` on a stopped project
whose `.data/postgres`, media root, manifest directory, audit directory and
public-keyring directory are absent or empty. There is no overwrite/force flag.
Set `RESTORE_LIVE_DIR` to an empty, protected staging directory with room for
the whole snapshot. The dump and snapshot evidence remain there after recovery.
Do not point staging at any destination or at the restic repository.

The tool restores the custom-format dump into a newly initialized PostgreSQL,
uses `db-role-bootstrap`, `migrate` and `db-runtime-grants` from Compose, and
copies all captured media, manifests, audit exports and public-key history.
Shared drill/live verification rehashes every live asset and verifies every
signed manifest against its database row and independently pinned public keys.
Live recovery requires at least one signed manifest; run `make integrity-cycle`
before the source backup. The final media paths are scrubbed again before the
gateway starts. Success prints `PASS live restore`; failures retain evidence,
stop the PostgreSQL started by the restore and print `FAIL live restore`.
Partial restores cannot be resumed over nonempty directories.

The backup does **not** include `.env`, private signing key, standalone public
key or Tailscale state. Keep those and the independent trust fingerprints in
an encrypted recovery kit outside the repository. Preserve the original MFA,
token, login-throttle and refresh-retry keys when migrating existing accounts;
database-role passwords can be fresh. Never copy the architecture-specific
PostgreSQL data directory. Follow the ordered [Mac-to-ThinkCentre move](thinkcentre-host.md#5-freeze-the-mac-and-move-the-library).

## Google Drive off-site repository

Restic encrypts before uploading through its
[rclone backend](https://restic.readthedocs.io/en/stable/030_preparing_a_new_repo.html#other-services-via-rclone).
Drive stores ciphertext, not the working library. **Never use an rclone mount
of Drive as `PHOTO_MEDIA_HOST_ROOT`**: originals need a local filesystem with
hard links, atomic commit and directory fsync semantics.

Install `restic` and `rclone` on the server (`sudo apt-get install restic rclone`).
Enable Google account **2-step verification**. Configure a remote named
`gdrive` for the **same OS user that runs backups**. The supplied systemd service
runs `/usr/bin/make backup` as **root**, with working directory
`/opt/family-photo-cloud` and `EnvironmentFile=/opt/family-photo-cloud/.env`.
An operator's `~/.config/rclone/rclone.conf` is not automatically root's config.

Use an explicit config path to avoid depending on `HOME` under systemd:

```bash
sudo install -d -m 0700 /root/.config/rclone /root/.config/family-photo-cloud
sudo rclone --config /root/.config/rclone/rclone.conf config
```

Create `gdrive`, storage type `drive`, default client ID/secret unless you have
your own OAuth client, scope **`drive.file`** (files created/opened by rclone),
and no service account. Leave root-folder ID blank unless intentionally using
a dedicated folder. `drive.file` limits visibility to the application's files;
use rclone, rather than manually creating a pre-existing Drive folder, to
create this backup repository. See [rclone Drive scopes](https://rclone.org/drive/#scopes).

On the server without a browser, answer **No** to automatic browser login.
Use the exact `rclone authorize` command/options the wizard prints on another
trusted machine that has a browser and a compatible rclone version (install
rclone there first). It generally starts with `rclone authorize "drive"` and
may include an encoded configuration argument carrying the chosen scope.
Keep that argument: do not replace it with a broader default scope. Complete
Google consent there, then paste the returned token into the server wizard's
`config_token` prompt over SSH. Never paste tokens into chat or logs. Confirm
the remote and leave the wizard. See [headless authorization](https://rclone.org/remote_setup/).

```bash
sudo chmod 600 /root/.config/rclone/rclone.conf
sudo rclone --config /root/.config/rclone/rclone.conf lsd gdrive:
sudo sh -c 'umask 077; openssl rand -base64 48 > /root/.config/family-photo-cloud/restic-password'
```

Generate that password file **only for a new repository**. For a move/recovery,
securely install the existing password instead. In the host's `.env` set:

```dotenv
RESTIC_REPOSITORY=rclone:gdrive:family-photo-backup
RESTIC_PASSWORD_FILE=/root/.config/family-photo-cloud/restic-password
RCLONE_CONFIG=/root/.config/rclone/rclone.conf
MANIFEST_TRUST_FINGERPRINTS_FILE=/root/.config/family-photo-cloud/manifest-keyring.sha256
```

No unit or backend-script change is required: systemd exports `RCLONE_CONFIG`
from `.env`, and `load_dotenv` exports it for manual scripts. Apt installs
Docker, restic and rclone in `/usr/bin`, on systemd's normal executable PATH;
`make` invokes the script through `bash`. The explicit absolute config path
works whether or not root's `HOME` is set. Keep the rclone config writable by
root so refreshed OAuth tokens can be saved. For a different backup user,
change the config/password paths and ownership together. If binaries were
installed somewhere else, supply a reviewed systemd `Environment=PATH=...`
drop-in rather than relying on an interactive shell's PATH.

First ensure the signing keyring exists (`sudo make integrity-cycle` from the
repo). Create independent trust fingerprints, then initialize **once** and
test the first real backup as root, just like the timer:

```bash
cd /opt/family-photo-cloud
sudo bash <<'EOF'
set -euo pipefail
source scripts/lib/load-env.sh
load_dotenv .env
(cd "${MANIFEST_PUBLIC_KEYRING_HOST_PATH:-./.data/secrets/manifest-public-keyring}" && sha256sum *.pem) \
  > "$MANIFEST_TRUST_FINGERPRINTS_FILE"
chmod 600 "$MANIFEST_TRUST_FINGERPRINTS_FILE"
rclone lsd gdrive:
restic init
make backup
make restore-drill
restic snapshots --tag family-photo-cloud
EOF
```

For an existing repository, skip `restic init` and keep the independently held
fingerprint file; do not regenerate it from untrusted restored keys. Save the
first `Backup completed`, `restic check` and `PASS restore drill` outputs and
the restore report. Before enabling the timer, verify root can refresh OAuth
without a browser and the repository survives a reboot. Then run
`sudo systemctl start family-photo-cloud-backup.service` and inspect
`sudo journalctl -u family-photo-cloud-backup.service -n 100` to prove the
actual service environment. This Google Drive path has not been exercised in
the Mac development run; no Google sign-in was attempted there.

The script runs `restic check --read-data-subset=1/100` by default, but **does
not implement retention**. After reviewing the snapshot list and keeping an
offline copy, run this **manual** retention operation with no backup/drill in
progress (it deletes old snapshots and their unreferenced data):

```bash
sudo bash <<'EOF'
set -euo pipefail
source scripts/lib/load-env.sh
load_dotenv .env
restic forget --tag family-photo-cloud --group-by host --keep-daily 7 --keep-weekly 5 --keep-monthly 12 --prune
EOF
```

The owner's **2 TB** allowance belongs to the Google One subscription bundled
with their Gemini Pro plan; it is not permanent free Drive capacity. Verify
the actual plan in the account: current [Google One plans](https://one.google.com/about/plans)
may advertise a different AI Pro allowance. Without another paid plan, storage
returns to the free **15 GB** when the subscription ends. Gmail, Drive and
Google Photos share the quota; over-quota accounts cannot accept new backups
(see [Google storage](https://support.google.com/drive/answer/6374270)). Budget
for encrypted backup history as well as originals. Drive has an upload/copy
limit of about **750 GB per user per day**; an initial library can take several
days ([Google Drive limits](https://developers.google.com/workspace/drive/api/guides/limits)).

The restic password file is this setup's **only decryption key**: keep an
offline protected copy, plus the rclone recovery/config material and manifest
fingerprints. Losing the password means losing access even with the Google
account. Drive is the **second copy**, not immutable protection against a
compromised backup host; a local USB or second-disk restic repository, rotated
offline, is still advisable. Do not delete phone originals until backup and
recovery have both been demonstrated.
