# ADR-0005: Adopt NIST CSF 2.0 and ISO/IEC 27001:2022 ISMS baseline

**Status:** Accepted  
**Date:** 2026-08-20  
**Deciders:** Family Photo Cloud operator

## Context

The product stores irreplaceable family media and personal metadata. A secure
upload protocol is necessary but insufficient: the deployment also needs risk
ownership, host hardening, backup recovery, monitoring, incident handling, and
software-supply-chain controls. The operator wants a real system rather than a
learning project, while the first deployment remains a small, self-hosted
service with a limited budget.

ISO/IEC 27001:2022 defines requirements for an information-security management
system (ISMS). NIST CSF 2.0 organizes outcomes into Govern, Identify, Protect,
Detect, Respond, and Recover; NIST SP 800-53 supplies a control catalog, and
NIST SP 800-218 supplies secure-development practices. Neither framework is a
drop-in configuration or evidence of certification.

## Decision

Adopt a risk-informed NIST CSF 2.0 Target Profile, initially at Tier 2, and
operate an ISO/IEC 27001:2022-aligned ISMS for the defined family-cloud scope.
Use selected NIST SP 800-53 controls and NIST SSDF practices as technical
implementation references. Maintain a control matrix, risk register, evidence
index, security policy, incident runbook, restore evidence, and an annual
management review. No public statement may say “ISO certified” unless a scoped,
independent certification has been completed.

## Options Considered

### Option A: Security features only

| Dimension | Assessment |
|---|---|
| Complexity | Low initially |
| Cost | Low |
| Scalability | Poor |
| Assurance | Insufficient |

**Pros:** Fast to ship.  
**Cons:** No evidence, recovery discipline, ownership, or repeatable incident
response; does not satisfy the requested ISO/NIST operating model.

### Option B: ISO/NIST-aligned control program (chosen)

| Dimension | Assessment |
|---|---|
| Complexity | Moderate |
| Cost | Low software cost; ongoing operator time |
| Scalability | Good for a small self-hosted service |
| Assurance | Evidence-based and auditable |

**Pros:** Covers software, host, people, and recovery; provides a credible path
to future certification.  
**Cons:** Requires recurring reviews, tests, backup costs, and documented risk
acceptance.

### Option C: Immediate formal ISO certification

| Dimension | Assessment |
|---|---|
| Complexity | High |
| Cost | High relative to a family service |
| Scalability | Good |
| Assurance | Highest external assurance |

**Pros:** Independent attestation.  
**Cons:** Premature before the service, scope, evidence cycle, and operations
are stable; certification is organizational work, not a repository feature.

## Consequences

- Every material change needs a threat/risk review and control evidence.
- Family use is blocked until P0 controls in the matrix are implemented and
  tested, including encrypted off-site backups and a successful restore drill.
- CI scans, dependency updates, image provenance, audit-retention, and incident
  response become ongoing operational duties.
- A self-hosted single-person operation still has residual availability and
  insider-access risk; those risks are documented rather than hidden.

## Action Items

1. [x] Add the control matrix, risk register, security policy, and CI scanning.
2. [ ] Provision hardened host, encrypted volumes, restricted administrative
   VPN, and automatic security patching.
3. [ ] Implement MFA/device management and centralized immutable audit export.
4. [ ] Configure encrypted off-site backups and complete a measured restore
   drill before source deletion is enabled.
5. [ ] Perform internal audit and management review after 90 days of evidence.
