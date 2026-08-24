# Security Risk Register

Status date: 2026-08-24. The operator is the risk owner until a separate role
is assigned. Reassess on any architecture change, security incident, new
supplier, or at least quarterly.

| ID | Risk | Likelihood | Impact | Treatment / launch condition | Owner | Status |
|---|---|---:|---:|---|---|---|
| R-001 | Home host, disk, or ransomware loss destroys originals | Medium | Critical | Encrypted 3-2-1 backups, offline/immutable copy, successful restore drill | Operator | Open — P0 |
| R-002 | Admin account/host compromise exposes all family media | Medium | Critical | TOTP MFA, VPN-only admin, least privilege, hardened host, audit export, incident playbook | Operator | Partial — P0; MFA source ready, host/access evidence pending |
| R-003 | Cloudflare MVP terminates TLS and can observe plaintext | Low | High | Explicit family acceptance; migrate to VPS L4 passthrough/origin TLS when required | Operator | Accepted only for MVP |
| R-004 | Vulnerable dependency or image reaches production | Medium | High | Protected branch, review, secret/SAST/dependency/image scans, signed images and SBOM | Operator | Partial — P1; release signing/SBOM workflow ready, successful release evidence pending |
| R-005 | Credential stuffing or stolen refresh token accesses a library | Medium | High | Durable identity throttle + global Argon2 worker budget, short access TTL, token-family replay revocation, device revocation, TOTP MFA and edge rate limits | Operator | Partial — P0; backend controls source-ready, real MFA enrolment and edge deployment evidence pending |
| R-006 | Integrity mismatch or latent bit rot is unnoticed | Low | Critical | Existing upload rehash/quarantine; scheduled scrubs, signed manifests, restore checks | Operator | Partial — P1; scrub/schedule source ready, host evidence pending |
| R-007 | Public gateway abuse exhausts bandwidth, disk, or workers | Medium | High | PATCH concurrency/size bounds, durable per-user upload-session admission, quota/free-space reservation, expiry reconciliation, Cloudflare WAF/rate rules, alerts and capacity review | Operator | Partial — P0; source controls ready, real edge apply/capacity evidence pending |
| R-008 | No usable forensic trail after account/media event | Medium | High | Structured minimal audit events, off-host immutable export, tested alert routing | Operator | Partial — P0; export/metrics/alert source ready, off-host routing evidence pending |
