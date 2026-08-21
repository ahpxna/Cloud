# Verification Status

Date: 2026-08-20

## Passed

- `docker compose --env-file .env.example config --quiet`
- All images are pinned to explicit version tags; no production `latest` tag.
- PostgreSQL and tus protocol-lab ports bind to loopback only.
- The tus lab disables downloads and is separated behind an explicit profile.
- `go test -race ./...` passes for access-token validation, Argon2id password
  records, refresh-token rotation/replay rejection, upload-session idempotency,
  cross-user isolation, interrupted/resumed TUS upload, checksum quarantine,
  no-overwrite commit, and commit recovery.
- `go vet ./...` passes.
- `make ios-parse` passes Swift syntax parsing and validates the XcodeGen YAML.
  It does not type-check against iOS SDK or TUSKit; full Xcode is not installed.
- `cmd/upload-gateway` defaults to `127.0.0.1:8080` outside Compose and has
  bounded configurable read/write/idle timeouts. Its duration parsing has a
  unit test.
- `.github` workflow YAML and Dependabot configuration parse successfully.
  The CI template runs race/vet/Compose/PostgreSQL integration/image-build;
  the security template runs secret, filesystem, image, and Go CodeQL scans
  when the project is initialized as a Git repository and pushed to GitHub.
- No production image reference uses the floating `latest` tag.
- GitHub Actions CI runs unit/race/vet, Compose validation, an embedded
  PostgreSQL 18 integration test, and a clean Docker-image build on Linux.
- The authenticated gateway embeds pinned `tusd` and enforces 32 MiB PATCH,
  two active PATCH requests per user, and six globally.
- Cloudflared is isolated from PostgreSQL by separate ingress/database networks.
- The `pki-sentinel` working tree was not modified; its untracked reuse
  assessment remains present.

## Pending

- PostgreSQL execution of `db/migrations/0001_core.sql` on this Mac.
- Docker-image build and Compose runtime startup on this Mac.
- Physical-device TUSKit background test.
- Cloudflare Tunnel end-to-end test, including request limits and HTTP 104.
- Full Xcode iOS SDK/package build and physical-device security acceptance test.
- All P0 rows in `docs/security/control-matrix.md`: operator MFA, host/disk
  encryption, hardened host/VPN/firewall evidence, off-host audit export,
  encrypted 3-2-1 backup, restore drill, alert test, and incident tabletop.

## Important scope boundary

The repository is ISO/IEC 27001:2022-aligned by design and maps technical
controls to NIST CSF 2.0, SP 800-53, and SSDF. It is **not ISO certified**.
Certification requires an operating ISMS with a defined organizational scope,
internal audit, management review, and an independent accredited audit.

The runtime checks could not pull public images because Docker Desktop's macOS
credential helper returned `Keychain Error (-67674)`, including when invoked
with an isolated empty Docker config. No Docker credential state was modified.
