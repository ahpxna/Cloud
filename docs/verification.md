# Verification Status

Date: 2026-08-24

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
- The 2026-08 audit correctness fixes pass `go test -race ./...`: canonical
  deduplication across filename extensions, asset/session association, bounded
  login-limiter cardinality, Argon2 concurrency budget, expiry cleanup/retry,
  periodic reconciliation wiring, strict duration parsing, and readiness/
  admission-control source paths.
- `go test -tags=integration -count=1 ./internal/upload` passes after applying
  both PostgreSQL migrations.

## Added in the 2026-08-24 proposal bundle

The repository now contains source for full-byte integrity scrubs, scheduled
signed-manifest cycles, append-only audit export, a low-cardinality metrics
exporter, loopback Prometheus/Grafana/Alertmanager, synthetic end-to-end upload
and download integrity probes, a local restart/resume chaos harness, restic
backup/isolated restore-drill tooling, CycloneDX SBOM generation, and keyless
release image signing/provenance.

Validation executed while producing this bundle:

- `gofmt -l cmd internal`: clean.
- `bash -n` over every repository shell script: pass.
- YAML parse for Compose, iOS XcodeGen, GitHub Actions, Prometheus,
  Alertmanager, and Grafana provisioning: pass.
- Grafana dashboard JSON parse: pass.
- `swiftc -parse` over the iOS/Share Extension source: pass.
- A fresh test-only copy/slice was pinned to the sandbox Go 1.23.2 toolchain;
  production `go.mod` was not changed. `go test` passes for the upload
  crash/retry/quarantine processor slice, MFA core, integrity manifest package,
  and synthetic probe.
- The account HTTP/MFA handlers were also compiled and their HTTP/core tests run
  in a Go 1.23.2 test-only slice with minimal stubs replacing unavailable
  PostgreSQL/JWT/Argon2 dependencies. This validates source/API wiring but is not
  a substitute for production dependency integration.

The sandbox Go runtime is 1.23.2 while `go.mod` requires Go 1.26.7. Full
`go test ./...`/`go vet ./...` cannot execute because automatic 1.26.7 toolchain
download is network-blocked. Docker is also unavailable in this sandbox, so
Compose runtime, image build, restic/restore integration, Prometheus startup,
and restart-chaos execution remain Pending rather than being reported as
passes. Real backup/provider/public-path/alert and physical-device evidence also
remains Pending below.

## Pending

- Docker-image build and Compose runtime startup on this Mac.
- Physical-device TUSKit background test.
- Cloudflare Tunnel end-to-end test, including request limits and HTTP 104.
- Full Xcode iOS SDK/package build and physical-device security acceptance test.
- All P0 deployment/evidence gaps in `docs/security/control-matrix.md`: operator
  MFA, host/disk encryption, hardened host/VPN/firewall evidence, a genuinely
  off-site/immutable repository, successful timed restore drill, external alert
  delivery test, access review, and incident tabletop.

## Important scope boundary

The repository is ISO/IEC 27001:2022-aligned by design and maps technical
controls to NIST CSF 2.0, SP 800-53, and SSDF. It is **not ISO certified**.
Certification requires an operating ISMS with a defined organizational scope,
internal audit, management review, and an independent accredited audit.

The runtime checks could not pull public images because Docker Desktop's macOS
credential helper returned `Keychain Error (-67674)`, including when invoked
with an isolated empty Docker config. No Docker credential state was modified.
