# TOTP MFA Runbook

Status: source implemented; real enrollment/recovery evidence still required before the P0 access-control row can be closed.

## Key custody

Generate `MFA_ENCRYPTION_KEY_BASE64` independently from the access-token and login-throttle keys:

```bash
openssl rand -base64 32
```

The decoded value must be exactly 32 bytes. Store it with the host's protected secrets and encrypted backup/recovery material. Losing this key makes enrolled TOTP secrets undecryptable; exposing it weakens the database-at-rest separation.

## Enroll

1. Sign in normally on a trusted device while MFA is not yet enabled.
2. In the iOS Uploads tab, choose **Begin authenticator enrollment**.
3. Add the displayed secret/`otpauth://` URI to a separate authenticator.
4. Enter the current 6-digit code and confirm enrollment.
5. Save every displayed recovery code to protected offline recovery material. The server stores only hashes and the iOS app intentionally does not persist the displayed set.
6. Sign out/revoke the test session, sign in again, and prove that password login returns an MFA challenge with no refresh token before successful second-factor verification.

## Recovery test

Use one recovery code in place of TOTP on a fresh login challenge. Confirm that:

- the code succeeds once;
- reuse fails;
- a TOTP challenge still succeeds afterward;
- rotating recovery codes with a current TOTP invalidates every prior unused recovery code.

Do not consume the operator's only offline copy during a drill; maintain at least two protected recovery copies or immediately rotate after testing.

## Disable

Disabling MFA requires a valid current TOTP and an explicit destructive confirmation in the iOS UI. After disabling, test a fresh sign-in and record the reason/date in the access review. If MFA is disabled because the authenticator is lost, treat that as a recovery/security event and rotate affected session credentials as appropriate.

## Abuse-control verification

The server durably limits authenticated MFA `confirm`, `recovery`, and `disable`
mutations to five attempts per user/action in a five-minute window. A sixth
request must return `429` with `Retry-After`; a successful protected mutation
clears only that action's budget. The Cloudflare ruleset adds a broader per-IP
edge bound but is not the security boundary. PostgreSQL integration tests cover
this durable state and the atomic recovery-code rotation rollback path.

## Evidence to retain

Retain only non-secret evidence: date, account role, device, successful challenge/recovery outcomes, and reviewer. Never put TOTP secrets, live recovery codes, bearer tokens, or QR payloads in logs/screenshots retained as audit evidence.
