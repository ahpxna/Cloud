# Build, SBOM, Signing, and Provenance

Pull requests and `main` builds compile every runtime/admin target. The security
workflow runs secret scanning, filesystem/container vulnerability policy,
CodeQL, and emits a CycloneDX SBOM for every image target.

Tag pushes matching `v*` invoke `.github/workflows/release.yml`. For each of the
eight deployable targets (`gateway`, `admin`, `manifest`, `scrub`,
`metrics-exporter`, `synthetic-probe`, `audit-export`, and `migrate`) the
workflow:

1. resolves the Dockerfile Go builder and Alpine runtime tags to immutable
   repository digests and builds with those exact base references;
2. builds the target with source/revision OCI labels;
3. pushes it to GHCR and resolves the immutable output digest;
4. generates a CycloneDX image SBOM;
5. creates build provenance tied to repository, commit, workflow, runner,
   target, and exact Dockerfile base-image digests;
6. keyless-signs the output digest with GitHub Actions OIDC via cosign;
7. attaches SBOM and provenance attestations to the digest; and
8. verifies the keyless signature identity before that matrix job can pass.

Compose also consumes upstream images such as PostgreSQL, cloudflared,
Prometheus, Alertmanager, Grafana, and the protocol-lab tusd image. Before a
production commissioning, resolve their reviewed tags to immutable refs:

```bash
make resolve-image-digests
# copy the reviewed VAR=image@sha256:... lines into .env
make verify-image-digests
```

The resolver is read-only and does not edit `.env`. Re-run the vulnerability
scan and review release notes before intentionally moving any pinned digest.

A committed workflow is not release evidence. The launch record must retain a
successful GitHub run and verify the exact production digest/attestations before
deployment.
