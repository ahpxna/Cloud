# Account Lifecycle Runbook

Accounts are invite-only and are created by an operator. Every lifecycle action
runs through the one-shot `admin` container as the `photo_cloud_admin` database
role, which can overwrite but never read password hashes, can revoke but never
mint sessions, and cannot touch upload, integrity or audit history.

Each action that revokes sessions also advances the account's authentication
epoch, which voids any MFA challenge already issued. The gateway checks the
device session **and** the account state on every request, so a revoked or
disabled account loses access at its next request rather than when its 15-minute
access token expires.

| Situation | Command | Effect |
| --- | --- | --- |
| New family member | `make create-user EMAIL=… [ROLE=member]` | Prompts for an initial password (minimum 12 characters). |
| Review accounts | `make list-users` | Email, role, state, MFA on/off and live device count. No secrets. |
| Lost or stolen phone | `make revoke-sessions EMAIL=…` | Signs every device out. The person signs in again on remaining devices. |
| Forgotten password | `make reset-password EMAIL=…` | Prompts for a new password and signs every device out. |
| Lost authenticator and recovery codes | `make reset-mfa EMAIL=…` | Removes TOTP and recovery codes and signs every device out. Verify the person out of band first. |
| Suspected account takeover | `make disable-user EMAIL=…` then follow `security-incident.md` | Blocks sign-in and refresh and revokes every device. |
| Re-admit after an incident | `make enable-user EMAIL=…` | Allows sign-in again. Devices revoked earlier stay revoked. |
| Account deletion request | `make delete-user EMAIL=… CONFIRM=…` | See below. |

Record the UTC time, the operator and the reason for every action in the
incident or operations log. Never record passwords, recovery codes or tokens.

## Login lockout

Five failed passwords for one email within ten minutes block that email for the
rest of the window, and Cloudflare blocks an IP after ten login attempts per
minute. Someone who knows a family member's email can therefore delay that
person's *new* sign-ins; devices that are already signed in keep working because
they refresh with their own token. If a lockout is reported, wait out the window
and check the gateway logs and Cloudflare security events for the source before
resetting anything.

## Deleting an account

`delete-user` is the first, immediate step:

1. The account moves to `deleting`, sign-in and refresh stop, every device is
   revoked and the MFA secret and recovery codes are destroyed.
2. Originals, upload history and the signed integrity manifests are **kept** at
   this point. They are shared evidence for other members' integrity history and
   they also exist in encrypted backups with their own retention.

Finish the purge within 30 days of the request:

1. Ask whether the person wants an export of their originals first; provide it
   through the app or a supervised copy, then confirm receipt.
2. Take and verify a backup (`make backup`) so the purge itself is recoverable
   if it was requested by mistake during the 30-day window.
3. Remove the account's originals from `originals/<user-id>/` on the media
   volume after the export is confirmed, then run `make integrity-cycle` so the
   next signed manifest no longer lists them.
4. Note the date the oldest backup containing the account ages out of the
   restic retention policy (`backup-restore.md`). The account is fully erased
   from backups only after that date; tell the person this date.
5. Close the request in the operations log with the UTC dates of each step.

The audit trail (`upload_events`) is append-only by design and keeps opaque IDs
and hashes, not photo content.
