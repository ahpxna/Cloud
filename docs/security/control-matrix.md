# Production Security Control Matrix

Status date: 2026-08-24
Target: NIST CSF 2.0 Tier 2 before family use; ISO/IEC 27001:2022-aligned ISMS.  
Scope: iOS app/Share Extension, gateway, PostgreSQL, media filesystem, backups,
Cloudflare Tunnel or later VPN relay, source repository, CI/CD, and host admin.

This is a control mapping, not an assertion of ISO certification or a claim to
implement every NIST SP 800-53 control. `Implemented` requires evidence;
`Partial` is not acceptable for P0 launch controls.

| Priority | NIST CSF 2.0 / 800-53 / SSDF reference | ISO/IEC 27001 ISMS area | Control and evidence | Status |
|---|---|---|---|---|
| P0 | GV.OC, GV.RM; PM-9 | Context, risk treatment | Scope, risk owner, risk register, annual review; evidence: ADR-0005 and signed review record | Partial |
| P0 | ID.AM; CM-8 | Asset inventory | Inventory host, disks, users, services, keys, domains, backups; evidence: monthly inventory export | Planned |
| P0 | PR.AA; AC-2, AC-3, AC-7, IA-2, IA-5 | Access control | Invite-only accounts, durable bounded password verification, token-family replay revocation, device/session revoke API, TOTP MFA with one-time recovery codes; evidence: enrolment/recovery drill and access review | Partial — source controls implemented; real enrolment/recovery and access-review evidence pending |
| P0 | PR.DS; SC-8, SC-13, SC-28 | Cryptography | TLS 1.3 public path, encrypted host/media/database/backup volumes, managed key rotation; evidence: config and restore test | Partial |
| P0 | PR.PS; CM-2, CM-6, CM-7 | Platform security | Hardened supported OS, automatic security patches, no public SSH/Postgres, VPN-only admin, host firewall; evidence: baseline scan | Runbook ready; deployment pending |
| P0 | PR.IR; CP-2, CP-4, CP-9, CP-10 | Resilience | 3-2-1 encrypted backups, immutable/offline copy, quarterly restore drill with RPO/RTO evidence | Partial — restic backup/isolated restore tooling ready; real off-site repository and dated drill pending |
| P0 | DE.CM; AU-2, AU-6, AU-9, AU-12, SI-4 | Logging and monitoring | Structured security events, protected off-host export, alert rules, time sync, retention and review; evidence: alert test | Partial — audit exporter, private metrics and alert rules ready; off-host retention/external alert test pending |
| P0 | RS.MA; IR-4, IR-5, IR-6 | Incident response | Severity playbook, evidence preservation, credential rotation, family notification rules; evidence: tabletop exercise | Planned |
| P1 | ID.RA; RA-3, RA-5, SI-2 | Vulnerability management | Weekly dependency/image/IaC scans, remediation SLA, exception log; evidence: CI artifacts and review | Implemented in CI template |
| P1 | SSDF PO, PS, PW, RV; SA-11, SA-15, SR-3 | Secure development and supply chain | Threat review, code review, secret scanning, SAST, SBOM, signed release image/provenance, dependency updates | Partial — workflow source ready; successful signed release evidence pending |
| P1 | PR.DS; SI-7 | Integrity | Client/server SHA-256, canonical dedup, durable no-overwrite commit, quarantine, signed manifests, scrub and restore verification | Partial — scheduled scrub/restore source ready; first dated host cycles pending |
| P1 | PR.AA; AC-6 | Separation of duties | Separate deploy, backup, manifest-signing, and database roles; no shared operator password | Planned |
| P1 | GV.SC; SR-2, SR-3, SR-5 | Supplier management | Review Cloudflare, VPS, registrar, Apple, GitHub, and backup-provider terms/data flow; evidence: supplier register | Planned |
| P2 | ID.IM; CA-2, CA-7 | Continual improvement | Quarterly control test, annual internal audit, management review, remediation tracking | Planned |

## Minimum launch gate

The system may accept family originals only when all P0 rows are `Implemented`
with dated evidence, and the risk owner explicitly accepts every remaining P1
risk. Do not let a family member delete a phone copy until encrypted off-site
backup and a successful restore drill have been evidenced.

## Evidence retention

- Keep security decisions, risk acceptance, access reviews, scan reports,
  incident records, backup reports, and restore-drill results for at least one
  year or the applicable legal requirement, whichever is longer.
- Keep raw security logs only as long as needed for detection/investigation;
  redact or avoid photo filenames, EXIF/GPS, bearer tokens, and full client IPs.
- Keep evidence in a separate encrypted backup location; a compromise of the
  home server must not permit silent deletion of all audit evidence.

## Sources

- [NIST CSF 2.0](https://www.nist.gov/publications/nist-cybersecurity-framework-csf-20)
  defines the Govern, Identify, Protect, Detect, Respond, and Recover outcomes.
- [NIST SP 800-53 Rev. 5 controls](https://csrc.nist.gov/Projects/risk-management/sp800-53-controls/downloads)
  provide selected implementation references.
- [NIST SP 800-218 SSDF](https://csrc.nist.gov/pubs/sp/800/218/final)
  guides secure software development.
- [ISO/IEC 27001:2022](https://www.iso.org/standard/27001) defines ISMS
  requirements; obtain the licensed standard and an accredited auditor for a
  certification program.
