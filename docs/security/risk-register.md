# Security Risk Register

Status date: 2026-08-20. The operator is the risk owner until a separate role
is assigned. Reassess on any architecture change, security incident, new
supplier, or at least quarterly.

| ID | Risk | Likelihood | Impact | Treatment / launch condition | Owner | Status |
|---|---|---:|---:|---|---|---|
| R-001 | Home host, disk, or ransomware loss destroys originals | Medium | Critical | Encrypted 3-2-1 backups, offline/immutable copy, successful restore drill | Operator | Open — P0 |
| R-002 | Admin account/host compromise exposes all family media | Medium | Critical | MFA, VPN-only admin, least privilege, hardened host, audit export, incident playbook | Operator | Open — P0 |
| R-003 | Cloudflare MVP terminates TLS and can observe plaintext | Low | High | Explicit family acceptance; migrate to VPS L4 passthrough/origin TLS when required | Operator | Accepted only for MVP |
| R-004 | Vulnerable dependency or image reaches production | Medium | High | Protected branch, review, secret/SAST/dependency/image scans, signed images and SBOM | Operator | Partial — P1 |
| R-005 | Credential stuffing or stolen refresh token accesses a library | Medium | High | Login limits, Argon2id, short access TTL, refresh rotation, MFA and device revocation | Operator | Partial — P0 |
| R-006 | Integrity mismatch or latent bit rot is unnoticed | Low | Critical | Existing upload rehash/quarantine; scheduled scrubs, signed manifests, restore checks | Operator | Partial — P1 |
| R-007 | Public gateway abuse exhausts bandwidth, disk, or workers | Medium | High | Existing PATCH limits; add quota/free-space floor, WAF/rate limits, alerts and capacity review | Operator | Partial — P0 |
| R-008 | No usable forensic trail after account/media event | Medium | High | Structured minimal audit events, off-host immutable export, tested alert routing | Operator | Partial — P0 |
