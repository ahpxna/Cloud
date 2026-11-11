# ThinkCentre: from bare server to the family library

Use this in order on a dedicated x86_64 Lenovo ThinkCentre Tiny, SFF or Tower.
The SSD holds Ubuntu, `/opt/family-photo-cloud`, Docker and PostgreSQL; the HDD
holds media. Tiny models may lack a 3.5-inch bay: check the exact model, power
and SATA connectors before buying. Prefer internal SATA and a CMR HDD, not SMR.
USB needs a reliable powered enclosure, cooling and SMART pass-through. A UPS
is useful. Size the HDD for the library, a full extra recovery staging copy,
and at least the configured 200 GiB free-space floor. Set BIOS **Power on after
AC loss**. The default below unlocks the media HDD without anyone typing after
a power cut. Keep its recovery passphrase offline with the owner.

## 1. Install and secure Linux

Install Ubuntu Server **24.04 LTS**, a named operator account, SSH, and a reserved
DHCP address. The default uses an SSD that boots unattended, with the HDD's
root-only unlock key on it. A passphrase-only encrypted SSD would block boot
before that key can be read: do not choose it for the parents' unattended host.
If encrypting the SSD too, separately arrange and test unattended root unlock.
Debian 12 uses the same storage/Compose steps and Docker's Debian apt repository.

From the operator's SSH session, after verifying public-key login in a second
terminal (keep the first open), run:

```bash
sudo apt-get update
sudo apt-get upgrade
sudo apt-get install unattended-upgrades openssh-server ufw chrony ca-certificates curl git make restic rclone smartmontools cryptsetup parted jq nano openssl
sudo dpkg-reconfigure -plow unattended-upgrades
sudo install -m 0644 /dev/stdin /etc/ssh/sshd_config.d/00-family-photo.conf <<'EOF'
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
EOF
sudo sshd -t
sudo systemctl reload ssh
read -r -p 'Management LAN CIDR (your actual LAN subnet): ' LAN_CIDR
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow from "$LAN_CIDR" to any port 22 proto tcp
sudo ufw allow 41641/udp
sudo ufw enable
sudo systemctl enable --now chrony
chronyc tracking
sudo systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
```

Do not forward SSH or the database from the router. Docker-published ports can
bypass ufw; this deployment publishes only the Tailscale peer UDP port to all
interfaces. The database and operator dashboards keep their loopback bindings.

## 2. Install Docker Engine and Compose

Use [Docker's apt repository](https://docs.docker.com/engine/install/ubuntu/),
not the distribution's `docker.io` or legacy `docker-compose` package. These
commands support Ubuntu 24.04 and Debian 12 by selecting the repository OS:

```bash
. /etc/os-release
case "$ID" in ubuntu|debian) ;; *) echo 'Use Ubuntu or Debian'; exit 1 ;; esac
sudo install -m 0755 -d /etc/apt/keyrings
sudo curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
sudo chmod a+r /etc/apt/keyrings/docker.asc
printf 'Types: deb\nURIs: https://download.docker.com/linux/%s\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\nArchitectures: %s\n' \
  "$ID" "${UBUNTU_CODENAME:-$VERSION_CODENAME}" "$(dpkg --print-architecture)" \
  | sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null
sudo apt-get update
sudo apt-get install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
sudo systemctl enable --now docker
sudo usermod -aG docker "$USER"
```

Log out and in again; `docker compose version` and `docker info` must succeed.
Membership of the docker group grants root-equivalent access. Do not install a
second host Tailscale daemon on UDP 41641; this repo runs it in a container.

## 3. Prepare the HDD (erases the selected disk)

Identify the empty HDD by **model, serial and size**, never by an assumed `/dev/sdb`.
The following formatting is destructive. Stop if the displayed disk contains
anything you need. Use its stable `/dev/disk/by-id/…` path, without `-part1`:

```bash
lsblk -o NAME,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS
ls -l /dev/disk/by-id/
read -r -p 'Stable by-id path of the EMPTY media HDD: ' MEDIA_DISK
lsblk -o NAME,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS "$MEDIA_DISK"
read -r -p 'Type ERASE to format that disk: ' CONFIRM
test "$CONFIRM" = ERASE || exit 1
sudo parted --script "$MEDIA_DISK" mklabel gpt mkpart primary 1MiB 100%
sudo udevadm settle
MEDIA_PART="${MEDIA_DISK}-part1"
# Enter a strong recovery passphrase; keep it offline with the owner.
sudo cryptsetup luksFormat --type luks2 "$MEDIA_PART"
sudo install -d -o root -g root -m 0700 /etc/cryptsetup-keys.d
sudo test ! -e /etc/cryptsetup-keys.d/family-photo-media.key || exit 1
sudo sh -c 'umask 077; dd if=/dev/urandom of=/etc/cryptsetup-keys.d/family-photo-media.key bs=64 count=1 status=none'
sudo chmod 0600 /etc/cryptsetup-keys.d/family-photo-media.key
# Adds the SSD key as another slot; keeps the recovery passphrase slot.
sudo cryptsetup luksAddKey "$MEDIA_PART" /etc/cryptsetup-keys.d/family-photo-media.key
sudo cryptsetup open --key-file /etc/cryptsetup-keys.d/family-photo-media.key "$MEDIA_PART" family-photo-media
sudo mkfs.ext4 -L family-photo-media /dev/mapper/family-photo-media
LUKS_UUID=$(sudo blkid -s UUID -o value "$MEDIA_PART")
MEDIA_UUID=$(sudo blkid -s UUID -o value /dev/mapper/family-photo-media)
printf 'family-photo-media UUID=%s /etc/cryptsetup-keys.d/family-photo-media.key luks,nofail,x-systemd.device-timeout=15s\n' "$LUKS_UUID" | sudo tee -a /etc/crypttab
sudo mkdir -p /srv/family-photo
printf '/dev/mapper/family-photo-media /srv/family-photo ext4 defaults,nofail,x-systemd.device-timeout=15s 0 2\n' | sudo tee -a /etc/fstab
sudo systemctl daemon-reload
sudo mount /srv/family-photo
sudo install -d -o "$(id -u)" -g "$(id -g)" -m 0700 /srv/family-photo/media
findmnt /srv/family-photo
```

Run the append commands once; inspect `/etc/crypttab` and `/etc/fstab` for
duplicates before repeating. The crypttab UUID identifies LUKS; fstab uses its
mapper device. Save `MEDIA_UUID`, the **ext4 UUID inside LUKS**, for preflight.
Keep ext4's `lost+found` at the mount root; the media subdirectory must be empty
for recovery. `nofail` lets Linux boot for SSH repair when the HDD is missing;
the Docker boot gate below still refuses to serve without the checked mount.
Preflight requires the actual mountpoint, ext4 UUID, active LUKS ancestry, free
space and `PHOTO_MEDIA_HOST_ROOT` below that mount. It never unlocks a disk.

After AC returns, BIOS starts the machine, the SSD boots, crypttab reads the
keyfile, and fstab mounts the unlocked mapper before Docker passes its gate.
Test a reboot without typing and a missing-HDD boot before handing over. This
protects an **HDD leaving the house alone** for warranty return, disposal or
drive theft. It does **not** protect against the whole machine being stolen:
the internal SSD carries the unlock key, database and configuration secrets.

Optional upgrade, **not verified on ThinkCentre hardware**: enroll a TPM2 slot
with `sudo systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7 "$MEDIA_PART"`.
After checking the installed systemd/TPM support, replace the crypttab keyfile
field with `none` and options with `luks,nofail,tpm2-device=auto,x-systemd.device-timeout=15s`.
The keyfile must also leave `/etc/cryptsetup-keys.d` (systemd can discover a
matching file there automatically); retire its LUKS slot only after a successful
TPM boot and a tested recovery passphrase. Keep recovery available across
firmware/Secure Boot changes; TPM policy and root-SSD encryption need their own
review before claiming protection against whole-machine theft. See systemd's
[crypttab](https://www.freedesktop.org/software/systemd/man/255/crypttab.html)
and [systemd-cryptenroll](https://www.freedesktop.org/software/systemd/man/255/systemd-cryptenroll.html).

## 4. Install this repository and configure the host

Use the owner's repository URL and reviewed revision containing these files:

```bash
read -r -p 'Repository clone URL: ' REPO_URL
sudo install -d -o "$USER" -g "$(id -gn)" /opt/family-photo-cloud
git clone "$REPO_URL" /opt/family-photo-cloud
cd /opt/family-photo-cloud
make env
make secrets
id -u
id -g
sudo blkid /dev/mapper/family-photo-media
nano .env
```

Set these values; use the numeric UID/GID printed above and actual filesystem UUID:

```dotenv
COMPOSE_FILE=compose.yaml:compose.linux-host.yaml
PHOTO_UID=1000
PHOTO_GID=1000
PHOTO_MEDIA_HOST_ROOT=/srv/family-photo/media
PHOTO_MEDIA_MOUNT=/srv/family-photo
PHOTO_MEDIA_DEVICE_UUID=REPLACE_WITH_EXT4_UUID
PHOTO_MEDIA_MIN_FREE_GIB=200
PHOTO_MIN_FREE_BYTES=214748364800
RESTORE_LIVE_DIR=/srv/family-photo/recovery-stage
```

Keep `COMPOSE_PROJECT_NAME=family-photo-cloud` on this dedicated production host.
Use paths without spaces in `.env` (also read by systemd). Set `PHOTO_TIMEZONE`
for files without a capture offset. `.data/postgres` remains on the SSD. Leave
the media, manifests, audit and public-keyring destinations empty for migration.

```bash
chmod 600 .env
# Docker must not auto-create this writable bind directory as root.
(
  source scripts/lib/load-env.sh
  load_dotenv .env
  sudo install -d -o "$PHOTO_UID" -g "$PHOTO_GID" -m 0700 "${INTEGRITY_REPORT_DIR:-.data/integrity-reports}"
)
sudo make host-preflight
make config
```

**Make the manual check a boot gate.** Compose has `restart: unless-stopped`;
preflight alone does not prevent Docker restarting the gateway over an absent
disk. On this dedicated server, gate Docker itself before it starts containers:

```bash
sudo mkdir -p /etc/systemd/system/docker.service.d
sudo tee /etc/systemd/system/docker.service.d/family-photo-media.conf >/dev/null <<'EOF'
[Unit]
RequiresMountsFor=/srv/family-photo

[Service]
ExecStartPre=/usr/bin/make -C /opt/family-photo-cloud host-preflight
EOF
sudo systemctl daemon-reload
```

This intentionally gates **all Docker workloads** on this dedicated machine.
At the next reboot, Docker must wait for the encrypted disk and a successful
preflight. Test that boot before handing it to parents; also test a planned
boot with the HDD disconnected, then reconnect/unlock, `sudo mount -a`,
`sudo make host-preflight`, and `sudo systemctl restart docker`. Inspect
`journalctl -u docker -b` for the gate. Do not start Docker manually around the
gate or hot-unplug the HDD while running. A disk removed after Docker started
is an incident: stop the stack and restore the mount before restarting it.

## 5. Freeze the Mac and move the library

Use an initialized encrypted restic repository reachable by both hosts: a local
USB repository is fastest for this move; Google Drive is described in
[backup-restore: Google Drive](backup-restore.md#google-drive-off-site-repository).
Do not copy `.data/postgres` from arm64 to x86_64. Recovery uses `pg_dump` and
`pg_restore`, with this repo's bootstrap/migration/grant path.

For a **new USB repository**, first mount an encrypted removable disk on the
Mac and choose absolute paths for `RESTIC_REPOSITORY` and
`RESTIC_PASSWORD_FILE` in its `.env`. Generate a strong password into that
protected file with mode 0600, keep an offline copy, then initialize once:

```bash
(
  source scripts/lib/load-env.sh
  load_dotenv .env
  restic init
)
```

Do not run `init` against an existing repository or replace its password.
On Linux the USB mount path may differ: adjust only
the repository/password paths, keeping the same repository and password.

On the **Mac**, in its existing repo directory, record the current revision,
user count and live asset count. Disable any host backup/integrity schedules
during the move. Stop Tailscale first, then the gateway, so no phone writes more
data and the node identity can never be active twice. PostgreSQL stays up for
the dump:

```bash
git rev-parse HEAD
docker compose --profile tailnet stop tailscale
docker compose --profile gateway stop upload-gateway
make list-users
docker compose exec -T postgres psql -U photo_cloud -d photo_cloud -At -c 'SELECT count(*) FROM assets WHERE deleted_at IS NULL'
make integrity-cycle
docker compose exec -T postgres psql -U photo_cloud -d photo_cloud -At -c "SELECT id, original_filename, encode(content_sha256,'hex') FROM assets WHERE deleted_at IS NULL ORDER BY id LIMIT 1"
```

For the new repository, create independent trust fingerprints now, before the
first backup. Set `MANIFEST_TRUST_FINGERPRINTS_FILE` in `.env` to a protected
absolute path outside the repository and restore stage, then run:

```bash
(
  source scripts/lib/load-env.sh
  load_dotenv .env
  (cd "${MANIFEST_PUBLIC_KEYRING_HOST_PATH:-./.data/secrets/manifest-public-keyring}" && sha256sum *.pem) \
    > "${MANIFEST_TRUST_FINGERPRINTS_FILE:?set a protected absolute path}"
  chmod 600 "$MANIFEST_TRUST_FINGERPRINTS_FILE"
)
```

Keep an offline copy. For an existing repository retain its independently
trusted fingerprints. Continue on the stopped Mac:

```bash
docker compose --profile audit build audit-export
make audit-export
make backup
# Load the same protected .env for direct restic commands (no eval):
(
  source scripts/lib/load-env.sh
  load_dotenv .env
  restic snapshots --tag family-photo-cloud
  read -r -p 'Snapshot ID just created: ' RESTORE_SNAPSHOT
  export RESTORE_SNAPSHOT
  make restore-drill
)
docker compose --profile gateway --profile tailnet down
```

Use the repo's configured DB/user if changed from `photo_cloud`. Do not run
`tailnet-up` on the Mac again during or after the move. Keep its stopped disk
intact until the ThinkCentre is proven; never operate both with the same node
state. `backup` restarts a gateway only if it was running when backup began.

The restic snapshot contains the dump, **all media** (including staging,
quarantine and originals), manifests, audit exports and public keyring. It
does **not** contain `.env`, the private signing key, standalone public key,
trust fingerprint file, or `.data/tailscale`. Make a separate protected recovery
kit on an encrypted removable disk, after the Mac is stopped. Never put these
files in Git or send them through chat. Keep the independent fingerprint file
outside restic and outside the restore staging directory.

Transfer over **SSH on the LAN**, after checking the server's SSH host-key
fingerprint at its console. On the ThinkCentre, create a private incoming
directory (run as its named operator, not root):

```bash
cd /opt/family-photo-cloud
test ! -e .migration-secrets || exit 1
install -d -m 0700 .migration-secrets
```

On the stopped Mac, make temporary copies only on an **encrypted** removable
volume, then stream them to that incoming directory. `.env.reference` is secret;
use it locally to edit the Linux configuration, never print it or paste it into
chat. The command copies the configured signing paths, password and trust file,
plus stopped Tailscale state; it does not copy PostgreSQL:

```bash
(
  set -euo pipefail
  umask 077
  source scripts/lib/load-env.sh
  load_dotenv .env
  read -r -p 'Mounted encrypted transfer volume directory: ' TRANSFER_ROOT
  TRANSFER_DIR=$(mktemp -d "$TRANSFER_ROOT/fpc-move.XXXXXX")
  trap 'rm -rf -- "$TRANSFER_DIR"' EXIT
  cp .env "$TRANSFER_DIR/.env.reference"
  cp "${MANIFEST_KEY_HOST_PATH:-.data/secrets/manifest-ed25519.pem}" "$TRANSFER_DIR/signing-private.pem"
  cp "${MANIFEST_PUBLIC_KEY_HOST_PATH:-.data/secrets/manifest-ed25519-public.pem}" "$TRANSFER_DIR/signing-public.pem"
  cp "${RESTIC_PASSWORD_FILE:?set RESTIC_PASSWORD_FILE}" "$TRANSFER_DIR/restic-password"
  cp "${MANIFEST_TRUST_FINGERPRINTS_FILE:?set MANIFEST_TRUST_FINGERPRINTS_FILE}" "$TRANSFER_DIR/manifest-trust.sha256"
  cp -R .data/tailscale "$TRANSFER_DIR/tailscale"
  chmod -R u+rwX,go-rwx "$TRANSFER_DIR"
  read -r -p 'ThinkCentre operator@LAN-address: ' SSH_DEST
  tar -C "$TRANSFER_DIR" -cf - . | ssh "$SSH_DEST" \
    'umask 077; cd /opt/family-photo-cloud/.migration-secrets && tar -xf - && chmod -R u+rwX,go-rwx .'
)
```

The temporary Mac copies are deleted on exit; retain a separate deliberate
offline recovery kit as listed below. Deletion is not guaranteed secure erasure
on SSDs: use encrypted transfer storage and never leave plaintext archives.

On the **ThinkCentre**, use the reviewed code revision compatible with that
snapshot. Keep its freshly generated **database passwords**. Securely edit its
`.env`, preserving the Mac's exact `ACCESS_TOKEN_HMAC_KEY_BASE64`,
`LOGIN_THROTTLE_HMAC_KEY_BASE64`, `MFA_ENCRYPTION_KEY_BASE64`,
`REFRESH_RETRY_ENCRYPTION_KEY_BASE64`, `MANIFEST_SIGNING_KEY_ID`,
`TAILSCALE_HOSTNAME` and `CANONICAL_HOST`. Losing the MFA encryption key locks
out enrolled users. Do not blindly replace the host `.env` with the Mac's paths.
Use `nano .env .migration-secrets/.env.reference` locally, then `chmod 600 .env`.

Copy the private signing key and standalone public key into the configured
`MANIFEST_KEY_HOST_PATH`/`MANIFEST_PUBLIC_KEY_HOST_PATH`. Copy the **contents**
of the stopped Mac's `.data/tailscale` into `/opt/family-photo-cloud/.data/tailscale`.
Keep state owned by `PHOTO_UID:PHOTO_GID`, directory mode 0700 and private key
mode 0600. Leave the **public-keyring destination empty**: restore-live restores
it from restic and checks it against the separately transferred fingerprints.
Put `RESTIC_PASSWORD_FILE` and `MANIFEST_TRUST_FINGERPRINTS_FILE` at their new
protected absolute paths, outside the restore stage. Set the same
`RESTIC_REPOSITORY` and the recorded `RESTORE_SNAPSHOT` ID in `.env`. For the
default signing paths and protected credential paths under `.data/secrets`:

```bash
(
  set -euo pipefail
  source scripts/lib/load-env.sh
  load_dotenv .env
  sudo install -d -o "$PHOTO_UID" -g "$PHOTO_GID" -m 0700 .data/secrets
  sudo install -o "$PHOTO_UID" -g "$PHOTO_GID" -m 0600 .migration-secrets/signing-private.pem "${MANIFEST_KEY_HOST_PATH:-.data/secrets/manifest-ed25519.pem}"
  sudo install -o "$PHOTO_UID" -g "$PHOTO_GID" -m 0600 .migration-secrets/signing-public.pem "${MANIFEST_PUBLIC_KEY_HOST_PATH:-.data/secrets/manifest-ed25519-public.pem}"
  sudo install -m 0600 .migration-secrets/restic-password "${RESTIC_PASSWORD_FILE:?set destination path}"
  sudo install -m 0600 .migration-secrets/manifest-trust.sha256 "${MANIFEST_TRUST_FINGERPRINTS_FILE:?set destination path}"
  test ! -e .data/tailscale
  sudo cp -R .migration-secrets/tailscale .data/tailscale
  sudo chown -R "$PHOTO_UID:$PHOTO_GID" .data/tailscale
  sudo find .data/tailscale -type d -exec chmod 0700 {} +
  sudo find .data/tailscale -type f -exec chmod 0600 {} +
)
```

Create protected parent directories first for any custom paths. `sudo install`
leaves the restic password/trust files root-owned: run restores, backups and
drills with `sudo make` on this host. After restore and phone checks pass,
delete the ThinkCentre's temporary copies with
`sudo rm -rf -- /opt/family-photo-cloud/.migration-secrets`; keep only the
intended runtime files and offline recovery kit. Never restart the Mac node.

**Keep offline for disaster recovery**, encrypted and separate from the server
and restic repository, in addition to the restic password:

| Item | If it is lost or replaced |
| --- | --- |
| `MFA_ENCRYPTION_KEY_BASE64` | Existing encrypted TOTP secrets cannot be decrypted; enrolled users cannot use their stored TOTP enrollment. Keep users' MFA recovery codes separately. See `internal/account/mfa.go` and `mfa_http.go`. |
| `ACCESS_TOKEN_HMAC_KEY_BASE64` | Existing access JWTs fail verification and must be renewed. Refresh-session hashes and Shortcut upload-key hashes are independent of this key; see `internal/auth/access_token.go`, `internal/account/http_api.go`, `upload_keys.go`. |
| `REFRESH_RETRY_ENCRYPTION_KEY_BASE64` | Stored successor refresh tokens from lost-response retries cannot be decrypted; affected devices may need to sign in again. It does not encrypt all refresh-session rows (`internal/account/http_api.go`). |
| `LOGIN_THROTTLE_HMAC_KEY_BASE64` | Persisted login-throttle identifiers no longer match; rate-limit history continuity is lost (`internal/account/http_api.go`). Preserve it on a move; a replacement does not decrypt user data. |
| Private signing PEM + `MANIFEST_SIGNING_KEY_ID`, public-key history and independently held trust fingerprints | Old signatures remain verifiable with their public keys. Losing the private key prevents signing under that identity; rotate deliberately, retain old public keys/trust, and keep the manifest history's backed-up DB/files (`cmd/manifest/main.go`). |
| LUKS recovery passphrase (optionally a header backup kept securely) | Without any working key slot/unlock key, the HDD cannot be decrypted. Do not rely solely on the SSD keyfile. |

The database dump already contains password hashes, refresh-session hashes and
Shortcut upload-key hashes; it contains no recoverable raw Shortcut key. Keep
that raw key on the phone/in its secure password store, or issue a replacement
and update the Shortcut if lost (`internal/account/upload_keys.go`). Database
role passwords may be newly generated for an **empty** restored database using
bootstrap/grants; they are not needed to decrypt the dump. Do not rotate them
casually on an initialized database.

Tailscale state is useful for preserving the node identity, not for decrypting
photos: if lost, authorize a new node (a new auth key can be issued), and possibly
choose a new name and update bookmarks/Shortcuts. Retire the old node in the
admin console. Preserve the stopped state for a seamless move; never run two
copies. `make tailnet-up` checks for its state or an auth key, and `compose.yaml`
mounts it at `TS_STATE_DIR`.

```bash
cd /opt/family-photo-cloud
sudo make host-preflight
make config
sudo make restore-live
```

It refuses any nonempty database/media/manifests/audit/keyring destination,
refuses a running stack, restores a logical PostgreSQL dump, runs the normal
migrations/grants, rehashes every live original, authenticates the restored
public keys, and verifies **every** signed manifest (including the newest).
It rehashes final media through `scrub` and starts a healthy gateway only after
verification. Look for `PASS live restore`. No force/overwrite option exists.
Before recovery starts, it rejects missing required gateway/database secrets,
signing identity/key files, restic password or independent trust file, naming
only the missing setting. It never transfers secrets from a backup or prints
their values; supply the independent kit first. Nonempty but incorrect keys
still require the post-move sign-in/integrity checks.
On FAIL it stops its PostgreSQL and retains partial data and staging evidence;
investigate, then choose fresh empty destinations rather than retry over them.
Staging needs a full extra library's free space **in addition to the configured
free-space floor**, and remains until you explicitly
remove the exact recovery-stage directory after acceptance (it contains a
plaintext database dump, so keep it on encrypted storage).

For a **new empty library**, skip recovery. With the default key path and no
existing key, create it once, then start the gateway and provision each parent:

```bash
install -d -m 0700 .data/secrets
test ! -e .data/secrets/manifest-ed25519.pem && (umask 077; openssl genpkey -algorithm ED25519 -out .data/secrets/manifest-ed25519.pem)
make gateway-up
read -r -p 'Parent account email: ' PARENT_EMAIL
make create-user EMAIL="$PARENT_EMAIL"
make integrity-cycle
```

Set a stable `MANIFEST_SIGNING_KEY_ID` in `.env` first; keep the private key in
the recovery kit. See [integrity-manifest](integrity-manifest.md). Do not run
these fresh-install steps before a move.

## 6. Start private access and check the move

For a transferred identity leave `TAILSCALE_AUTHKEY` blank; its existing state
is sufficient. For a new node use the one-time key and DNS/HTTPS setup in
[tailnet-family](tailnet-family.md). Then:

```bash
sudo make host-preflight
make tailnet-up
docker compose --profile tailnet exec -T tailscale tailscale --socket=/tmp/tailscaled.sock status
read -r -p 'Phone tailnet name or 100.x address: ' PHONE_PEER
docker compose --profile tailnet exec -T tailscale tailscale --socket=/tmp/tailscaled.sock ping "$PHONE_PEER"
sudo make integrity-cycle
make list-users
docker compose exec -T postgres psql -U photo_cloud -d photo_cloud -At -c 'SELECT count(*) FROM assets WHERE deleted_at IS NULL'
```

Compare users and count to the Mac, sign in from a phone, open `/app/`, play a
video and download an original. Check the original's SHA-256 against the Mac's
record. Same `.data/tailscale` state preserves the same HTTPS node name, so
parents' bookmarks and Shortcuts keep working. In the admin console disable
**key expiry** on the server node. Test both Wi-Fi and cellular with Tailscale
enabled on the phone, as described in the family runbook.

`status`/`ping` should show `direct <address>:<port>` once peers exchange traffic,
not only `via DERP`. The override retains non-internal `tunnel-egress`, so its
published UDP port works with Docker bridge networking. Allow outbound UDP,
DNS and HTTPS; if NAT prevents direct paths, reserve the server's LAN address
and forward **UDP 41641 only** at the router. Carrier-grade/symmetric NAT or
the phone's network can still require DERP; publishing a port cannot guarantee
a direct path. See [Tailscale's connection types](https://tailscale.com/docs/reference/connection-types).

## 7. Off-site backups and schedules

Complete [Google Drive setup and first backup/drill](backup-restore.md#google-drive-off-site-repository)
before enabling timers. Keep the restic password and manifest fingerprints
offline too. Then:

```bash
sudo make install-systemd
sudo systemctl enable --now family-photo-cloud-integrity.timer family-photo-cloud-backup.timer family-photo-cloud-session-maintenance.timer
systemctl list-timers 'family-photo-cloud-*'
systemctl status family-photo-cloud-backup.timer
sudo journalctl -u family-photo-cloud-backup.service -u family-photo-cloud-integrity.service -u family-photo-cloud-session-maintenance.service --since yesterday
```

Schedules use host local time: backup daily 02:10 (+0–15 min), integrity Sunday
03:20 (+0–20 min), session maintenance daily 04:10 (+0–15 min). Backup and
integrity share a lock; an overlap fails rather than producing mixed evidence.
Review failures the next morning, especially after large initial uploads.

## 8. Day 2 and parent handoff

Monthly: check `df -h / /srv/family-photo`, `sudo smartctl --scan-open`, then
`sudo smartctl -a "$MEDIA_DISK"` (re-enter the by-id path after a new shell).
Use `sudo smartctl -t short "$MEDIA_DISK"`, wait the reported duration and read
`sudo smartctl -l selftest "$MEDIA_DISK"`; a USB bridge may need `-d sat`.
Monitor reallocated/pending sectors, temperature and test failures. Investigate
read errors immediately. A SMART PASS does not replace backups.

For OS/Docker updates, first ensure a checked backup exists, then
`sudo apt-get update` and `sudo apt-get upgrade`; reboot in a planned window
and confirm automatic unlock. Keep the owner available with the recovery
passphrase if unlock fails. Check preflight, timers, `/app/` and a video
afterward. For a repo update: `git status --short`, record `git rev-parse HEAD`,
read release/migration notes, `git pull --ff-only`, review image version changes
in `.env`, then `make config` and `make tailnet-up` (rebuilds and migrates).
Do not run arbitrary `down -v` or disk/cache cleanup commands. A DB migration
may require backup recovery for rollback; an old binary alone is insufficient.

At the 200 GiB floor the gateway rejects new uploads/readiness; preserve
existing originals and add capacity rather than deleting files under `originals`.
A completely full SSD can also stop PostgreSQL/backups. Monitor both disks.
After power loss, PostgreSQL uses crash recovery and pending uploads reconcile;
confirm automatic unlock/mount, then inspect logs and run `sudo make integrity-cycle`.
Do not assume a successful reboot proves all stored bytes are sound.

Give parents the tested HTTPS bookmark and this short checklist: turn on
Tailscale, check Wi-Fi/cellular, retry the bookmark, check the server has power
and its disk is connected, then contact the operator. Do not unplug a running
disk, reset the server, change its name, or delete phone originals to fix access.
The operator checks `sudo make host-preflight`, `make status`,
`docker compose --profile tailnet logs --tail=100 tailscale upload-gateway postgres`,
disk space and `journalctl -u docker -b`. Keep phone originals until an off-site
backup **and** restore drill pass. Follow [go-live](go-live.md) for the remaining
alerts/account controls before handing over.
