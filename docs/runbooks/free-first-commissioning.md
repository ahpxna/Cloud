# Free-first commissioning checklist

This checklist is intentionally limited to work that requires no new paid
service. It is not a launch checklist and does not make the system a backup.

## Complete now

1. Keep the repository private and enable GitHub 2FA/passkeys for the operator.
   Do not put `.env`, tunnel tokens, private manifest keys, photos, or database
   dumps in Git.
2. Install full Xcode from the Mac App Store, then install XcodeGen with
   `brew install xcodegen`. Run `make ios-test` to generate the project and
   execute XCTest on a simulator. A free Apple ID can sign a development build
   to the operator's own iPhone; it cannot provide TestFlight/App Store
   distribution to parents.
3. On that iPhone, test one image and one video through Share Sheet import,
   airplane-mode interruption, app termination, relaunch, Wi-Fi/cellular
   change, server verification, and original download hash check. Record
   date, iOS version, result, and any failure in an evidence note outside the
   repository.
4. Run repository checks before every commit. `make ios-parse` is a syntax and
   project-spec check only; it is not an iOS SDK build.
5. Do not run `make edge-up`, publish a tunnel, or create family accounts for
   real originals yet.

## When the 20 TB disk arrives

1. Use a dedicated, supported Linux host. Install the disk with full-disk or
   volume encryption (LUKS) and format the mounted media volume as ext4 or XFS.
   Do not use exFAT/FAT for originals.
2. Mount it by UUID outside the repository (for example `/srv/family-photo`).
   Do not bind production data to `./.data/media`.
3. Set `PHOTO_MEDIA_MOUNT`, `PHOTO_MEDIA_DEVICE_UUID`, and
   `PHOTO_MEDIA_MIN_FREE_GIB` for the preflight, then run:

   ```bash
   make host-preflight
   ```

4. Copy only disposable test fixtures first. Demonstrate upload, server hash
   verification, manifest generation, a power/restart recovery scenario, and
   local readback hash verification.
5. Keep all phone originals. The disk becomes the primary copy only after the
   checks pass; it is not a backup.

## Spending order after the disk

1. Encrypted off-site backup storage and a successful isolated restore drill.
2. A domain plus Cloudflare Tunnel (or a direct/VPS origin path) after the host
   is hardened.
3. Apple Developer Program membership only when remote native distribution is
   needed. It does not improve data durability.

## Non-negotiable public launch gates

- Encrypted off-site backup with a separate credential and a measured restore
  drill.
- Host disk encryption, current patches, default-deny firewall, and VPN/LAN
  only administration.
- MFA/passkeys for the operator and tested account/device revocation.
- Protected off-host audit export and a tested alert route.
- Full Xcode physical-device testing of Share Sheet and upload resume.
