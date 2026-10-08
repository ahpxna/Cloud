# Go-Live Checklist

The order matters: each stage proves something the next one relies on. Tick a
box only with dated evidence (command output, email received, report file)
kept outside Git. Details live in the linked runbooks.

## 1. Source and CI (no hardware)

- [ ] `main` is green: Go race/integration tests, iOS XCTest, Trivy, CodeQL,
      gitleaks, rule tests, Terraform validation.
- [ ] Repository is private, or you have accepted that the code and commit
      metadata are public. Never commit `.env`, keys, photos or dumps.

## 2. Host and media disk ([production-host](production-host.md))

- [ ] Dedicated Linux host, current patches, default-deny firewall, admin only
      over LAN/VPN, Docker Engine with Compose v2.
- [ ] 20 TB media disk encrypted with LUKS, formatted ext4 or XFS, mounted by
      UUID (for example `/srv/family-photo`), owned by the operator user.
- [ ] Clone to `/opt/family-photo-cloud`, then in `.env`:
      `PHOTO_MEDIA_HOST_ROOT=/srv/family-photo/media`,
      `PHOTO_UID`/`PHOTO_GID` = `id -u`/`id -g`, keep
      `PHOTO_MIN_FREE_BYTES=214748364800`.
- [ ] `PHOTO_MEDIA_MOUNT=/srv/family-photo PHOTO_MEDIA_DEVICE_UUID=<uuid> make host-preflight` passes.

## 3. Secrets and stack

- [ ] `make secrets` (fresh values for this host; never copy a laptop `.env`).
- [ ] If this host already had a `.env`, its `*_IMAGE` lines override Compose
      defaults: update them to the versions in `.env.example`, or pin digests
      with `make resolve-image-digests` and `make verify-image-digests`.
- [ ] Manifest signing key created per [integrity-manifest](integrity-manifest.md),
      private key outside Git and outside the gateway.
- [ ] `make gateway-up` and `make observability-up` report healthy.
- [ ] `make create-user EMAIL=<probe account>` and `make synthetic-probe-docker`
      print `PASS` ([synthetic-probe](synthetic-probe.md)).

## 4. Alerts ([observability](observability.md))

- [ ] `ALERT_*` keys set in `.env`; `make alert-email` validated the config.
- [ ] `make observability-up && make alert-test`: FIRING and RESOLVED emails
      reached every recipient (check spam folders).

## 5. Backup and restore ([backup-restore](backup-restore.md))

- [ ] Off-site restic repository with provider-side retention/immutability and
      its own credential; `RESTIC_REPOSITORY`, `RESTIC_PASSWORD_FILE` and
      `MANIFEST_TRUST_FINGERPRINTS_FILE` set.
- [ ] `make integrity-cycle`, then `make backup`, then `make restore-drill`
      prints `PASS` with every asset rehashed and every manifest verified.
- [ ] `sudo make install-systemd`, then enable
      `family-photo-cloud-{integrity,backup,session-maintenance}.timer`.
- [ ] The next morning the `PhotoCloudBackupStale` alert is quiet.

## 6. Access from outside the home: choose one

**Free and private (recommended for a family):** follow
[tailnet-family](tailnet-family.md): `make tailnet-up`, Tailscale on each
phone, the web app at `/app/` and the iOS Shortcut. No domain, no Apple fee;
skip the rest of section 6 and the iPhone-app items in section 7.

**Public hostname (Cloudflare):**

- [ ] A domain you control, added to Cloudflare (a `*.workers.dev` or
      `*.trycloudflare.com` name cannot carry a stable named Tunnel; quick
      tunnels change URL on every restart, which breaks the app).
- [ ] Named Tunnel whose public hostname targets
      `http://upload-gateway:8080`; token in `CLOUDFLARE_TUNNEL_TOKEN`.
- [ ] From any machine with Internet access (not necessarily the server), with a
      Cloudflare API token scoped to Zone WAF edit: `terraform plan` reviewed
      and `terraform apply` done in `infra/cloudflare` for the WAF and
      rate-limit rules (import existing rulesets first).
- [ ] `make edge-up`; `make synthetic-probe BASE_URL=https://<hostname> …`
      passes through Cloudflare.

## 7. Native iPhone app (only with a public hostname or a paid Apple account)

- [ ] Set `PHOTO_CLOUD_API_BASE_URL` in `ios/project.yml` to
      `https://<hostname>` and your Apple team in `DEVELOPMENT_TEAM`; keep the
      App Group identifiers consistent for both targets.
- [ ] Physical-iPhone test: one photo and one video via Share Sheet and the
      picker, airplane-mode interruption, force-quit and relaunch,
      Wi-Fi↔cellular change mid-upload, item reaches *Available*, original
      downloads and its SHA-256 matches. Record iOS version and results.
- [ ] Family distribution, one of:
      - **Free (no Apple Developer Program):** sign with your Apple ID's
        *Personal Team* in Xcode and install over a cable onto each phone. The
        build stops launching after 7 days and must be reinstalled, every phone
        must be plugged into your Mac, and full Xcode needs tens of GB free.
        TestFlight is **not** available on this path. No privacy/support pages
        are required.
      - **TestFlight / App Store (Apple Developer Program, paid yearly):**
        publish `docs/legal/privacy-policy.md` and `docs/legal/support.md` with
        the placeholders filled (any HTTPS page works, including a
        `*.workers.dev` site), set `PHOTO_CLOUD_PRIVACY_POLICY_URL` and
        `PHOTO_CLOUD_SUPPORT_URL`, complete the privacy labels, give App Review
        a demo account, and check the archive with
        `python3 scripts/ios-release-check.py <App.app>`.

## 8. Family onboarding ([account-lifecycle](account-lifecycle.md))

- [ ] Create each member with `make create-user`; each signs in on their
      phone and enables MFA.
- [ ] Tell everyone: keep originals in Photos until the app shows *Available*.
- [ ] Rehearse `make revoke-sessions` and `make disable-user` once.
