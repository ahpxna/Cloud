# Security Incident Runbook

Use this runbook for suspected account takeover, unexpected media access,
malware/ransomware, backup failure, integrity mismatch, secret exposure, or a
public gateway exploit. Preserve evidence and prioritize containment over normal
availability.

## First 30 minutes

1. Record UTC time, reporter, affected user/system, and observed facts. Do not
   place photo filenames, bytes, tokens, passwords, or full IP addresses in the
   incident note.
2. If active compromise is plausible, stop public ingress by disabling the
   Cloudflare public hostname/tunnel or firewalling TCP 443. Do not wipe disks.
3. Revoke affected refresh sessions, disable the account, rotate the gateway
   HMAC key and tunnel token if exposed, and preserve the old key only as sealed
   evidence when needed for investigation.
4. Snapshot PostgreSQL, media inventory, gateway logs, host logs, and active
   container/image digests to an encrypted evidence location. Label every copy
   with UTC time and SHA-256.

## Triage and recovery

1. Determine whether confidentiality, integrity, or availability was affected.
2. Verify the newest signed manifest and sample/targeted original SHA-256s.
3. Rebuild a host from a known-good, patched image rather than trusting a host
   with suspected administrator compromise.
4. Restore only tested database/media backup pairs. Run the restore drill steps
   before republishing ingress.
5. Force affected users to sign in again and tell family members what data may
   have been accessed, what was done, and what they should do next.

## Closure

Document root cause, timeline, affected scope, evidence hashes, corrective
actions, residual risk acceptance, and a test proving the fix. Update the risk
register and controls matrix; conduct a short tabletop follow-up within 30 days.
