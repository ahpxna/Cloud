# Family Photo Cloud — Privacy Policy (DRAFT)

> **Draft for the operator.** Replace every `<…>` placeholder, have it reviewed,
> and publish it at a stable HTTPS URL before setting
> `PHOTO_CLOUD_PRIVACY_POLICY_URL`. It describes the data flows in this
> repository as of 2026-10-07; update it whenever they change. It is not legal
> advice.

**Operator:** `<name of the person or household running the server>`
**Contact:** `<operator email address>`
**Effective date:** `<YYYY-MM-DD>`

Family Photo Cloud is a private, invite-only service that backs up photos and
videos for members of one family to a server the family operates. There is no
public sign-up, no advertising and no analytics or tracking SDK.

## What is collected and why

| Data | Why | Where it is kept |
| --- | --- | --- |
| Email address, role and account state | Sign-in and account administration | Family server database |
| Password | Sign-in. Stored only as an Argon2id hash; the operator cannot read it | Family server database |
| Optional two-factor (TOTP) secret and recovery codes | Two-factor sign-in. The secret is encrypted; recovery codes are stored only as hashes | Family server database |
| Device name you enter and session times (created, last used, expiry) | Lets you see and sign out your devices | Family server database |
| Photos and videos you choose to upload, including the metadata inside the files (for example location, date and camera details) | Backup and viewing in the Library | Family server media disk and encrypted backups |
| File name, type, size and SHA-256 fingerprint of each upload, and upload progress events | Verifying that each original arrived intact and keeping an integrity audit trail | Family server database, signed integrity manifests and backups |
| Technical request data (IP address, time, request path) | Delivering and protecting the service | Cloudflare (network provider) and short-lived server logs |
| Upload diagnostic log | Troubleshooting. Kept on your device; it leaves the device only if you export it and choose a recipient | Your device |

The app only reads photos and videos you select or share to it. It does not scan
your whole photo library.

## Who processes the data

- **The operator** runs the server and can technically access stored originals.
  Connections are encrypted in transit, but storage is not end-to-end encrypted
  from the operator.
- **Cloudflare** carries traffic between the app and the server (Cloudflare
  Tunnel) and applies security filtering. See Cloudflare's privacy policy.
- **Backup storage provider:** `<provider name and region>` stores encrypted
  backups. The backup encryption key stays with the operator.

Data is not sold, shared for advertising, or used to train models.

## Retention and deletion

- Originals and their integrity records are kept while your account exists.
- Signed-out device sessions are pruned after `<180>` days.
- To delete your account, contact the operator. Sign-in stops immediately, your
  devices are signed out and two-factor data is destroyed. After offering you an
  export, the operator removes your originals within 30 days. Copies inside
  encrypted backups are removed when those backups expire, at most
  `<backup retention period>` later; the operator will tell you that date.
- The upload audit trail keeps opaque identifiers and fingerprints, not photo
  content.

## Your choices

You can choose which photos to upload, sign out any device from the app, turn
two-factor authentication on or off, and ask the operator for a copy of your
originals, a correction, or deletion of your account.

## Children

Accounts are created by the operator for family members. If a child uses the
app, a parent or guardian manages the account with the operator.

## Changes

The operator will tell members about material changes before they take effect
and update the effective date above.
