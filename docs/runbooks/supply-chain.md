# Build, SBOM, Signing, and Provenance

Pull requests and `main` builds compile every runtime/admin target. The security
workflow runs secret scanning, filesystem/container vulnerability policy,
CodeQL, and emits a CycloneDX SBOM for every image target.

Tag pushes matching `v*` invoke `.github/workflows/release.yml`. The workflow:

1. builds the Internet-facing `gateway` image with source/revision OCI labels;
2. pushes it to GHCR and resolves the immutable digest;
3. generates a CycloneDX image SBOM;
4. creates a build provenance predicate tied to repository, commit, workflow,
   runner, and target;
5. keyless-signs the digest with GitHub Actions OIDC via cosign;
6. attaches SBOM and provenance attestations to the digest; and
7. verifies the keyless signature identity before the job can pass.

A committed workflow is not release evidence. The launch record must retain a
successful GitHub run and verify the exact production digest/attestations before
deployment.
