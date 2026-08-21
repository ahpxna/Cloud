# Security Policy

## Scope

The in-scope system is the Family Photo Cloud iOS application, Share Extension,
upload gateway, PostgreSQL database, media storage, backups, Cloudflare Tunnel,
and the host used to run them. A family member should report a suspected issue
privately to the operator; do not place credentials, photo bytes, account
emails, IP addresses, or exploit details in a public issue.

## Reporting

Send a private report containing a minimal reproduction, affected version,
impact, and any safe mitigation. The operator acknowledges receipt within 72
hours, prioritizes confirmed account or photo exposure as P0, and keeps the
reporter informed until remediation is deployed.

## Disclosure handling

The operator records each report in the incident log, rotates exposed
credentials, preserves relevant audit evidence, and informs affected family
members when their account or originals may have been exposed. A fix is tested
before disclosure; no public advisory is needed for a private family deployment
unless a reusable dependency or configuration defect needs upstream disclosure.

## Security baseline

This repository maps its controls to ISO/IEC 27001:2022 and NIST guidance in
`docs/security/control-matrix.md`. That mapping is implementation evidence, not
an ISO certification claim. Certification requires an operating ISMS, a defined
scope, internal audit, management review, and an independent accredited audit.
