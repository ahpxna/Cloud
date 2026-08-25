# MVP API Contract

All JSON and TUS routes are served by the authenticated gateway. Responses
containing credentials or upload state use `Cache-Control: no-store`.

## Account flow

There is no public registration endpoint. Create family users locally with:

```bash
make create-user EMAIL=parent@example.com ROLE=member
```

`POST /v1/auth/login`

```json
{
  "email": "parent@example.com",
  "password": "user-entered password",
  "device_name": "Mom's iPhone"
}
```

When MFA is not enabled, the response contains a 15-minute HS256 access token
and a 30-day opaque refresh token. When confirmed MFA is enabled, a correct
password returns `202` with a one-time 5-minute `challenge` and **no access or
refresh token**. Complete `/v1/auth/mfa/verify` with either a current TOTP code
or one unused recovery code before tokens are issued. Store the refresh token
in iOS Keychain, never UserDefaults or the photo queue database. `POST /v1/auth/refresh` rotates the refresh token. Updated clients also send a client-generated UUID as `rotation_request_id` and persist that UUID until a successful response. For 30 seconds the server can return the exact encrypted successor only when both the old token **and the same request ID** are retried; a different request ID is treated as replay and revokes the live family. Older clients that omit the request ID still rotate normally but do not receive lost-response idempotency. Refresh sessions are token families: reuse of a revoked token outside the exact retry case revokes every live descendant and emits a security warning without revealing the account to the caller. `POST /v1/auth/logout` revokes its token and is idempotent.


### MFA lifecycle

All MFA state is server-side. TOTP secrets are encrypted with the independent
`MFA_ENCRYPTION_KEY_BASE64`; recovery codes are displayed once and persisted
only as SHA-256 hashes. TOTP verification accepts the adjacent ±1 30-second
window and stores the last accepted counter to reject replay.

- `POST /v1/auth/mfa/enroll` requires an active access-token session **and the
  current password** (`password`), then returns a new base32 secret plus
  `otpauth_uri`. It invalidates any unconfirmed previous enrollment.
- `POST /v1/auth/mfa/confirm` requires an access token and the current
  `totp_code`; on success it returns the one-time recovery-code set **and
  revokes every pre-MFA session family**, including the current device. The
  client must discard cached credentials and sign in again through MFA.
- `POST /v1/auth/mfa/verify` is the only unauthenticated MFA route. Send the
  password-login `challenge` plus exactly one of `totp_code` or `recovery_code`.
  A challenge has five attempts and is consumed on success. Challenge issuance
  is durably capped per user (12 per hour) so repeated correct-password logins
  cannot reset MFA brute-force budget indefinitely.
  Authenticated `enroll`, `confirm`, `recovery`, and `disable` mutations additionally use
  a durable per-user five-attempt/five-minute action budget; enrollment password
  checks also share the global Argon2 worker gate. Successful actions clear only
  their own budget. Edge rate limits are defense in depth only.
- `POST /v1/auth/mfa/recovery` requires an access token plus current TOTP and
  rotates every recovery code, showing the replacement set once. The accepted
  TOTP counter and replacement recovery-code set commit atomically.
- `POST /v1/auth/mfa/disable` requires an access token plus current TOTP,
  deletes TOTP/recovery state, consumes outstanding challenges, and revokes all
  sessions. Clients must delete cached access/refresh credentials and sign in
  again.

Recovery codes are high-entropy, single-use credentials. Store them offline;
do not screenshot them into the same photo library being protected.

`GET /v1/auth/sessions` requires an access token and lists active device
sessions. `DELETE /v1/auth/sessions/{session_id}` revokes only a session owned
by the caller; another account receives `404`.

## Create an upload session

`POST /v1/upload-sessions` requires `Authorization: Bearer <access-token>`.

```json
{
  "client_asset_id": "PhotoKit-local-identifier-or-generated-UUID",
  "original_filename": "IMG_0123.HEIC",
  "media_type": "image/heic",
  "expected_size": 4821931,
  "client_sha256": "64-lowercase-or-uppercase-hex-characters"
}
```

The `(owner, client_asset_id)` pair is an idempotency key. Repeating identical
immutable metadata returns the same session with `200`; changing it returns
`409`. The response provides `upload_endpoint`, `session_id_metadata`, a
recommended 32 MiB chunk size, and an `upload_token` capability valid only for
that upload session. Session creation is admission-controlled: it returns `507
Insufficient Storage` if either the owner's quota or the filesystem safety
reserve (including active-upload reservations) would be exceeded.
It returns `429 active_upload_limit` once the configured per-user count of
incomplete sessions is reached. New session identities are also protected by a
durable PostgreSQL per-user creation window (`upload_create_rate_limited`); an
idempotent retry of an existing non-expired identity does not consume that
window. These controls bound database/event growth from a compromised
authenticated account.

## TUS upload

Create the TUS resource by POSTing to `/v1/uploads/` with `Authorization:
Bearer <upload_token>` only. General access JWTs are intentionally rejected by
the TUS endpoint:

```text
Tus-Resumable: 1.0.0
Upload-Length: <exact expected size>
Upload-Metadata: session_id <base64(session UUID)>
```

The gateway replaces user metadata with the server-approved session ID and
forces that ID to be the TUS resource ID. Every later `HEAD` and `PATCH` checks
that the capability is scoped to that exact session; a different user receives
`404`, not an existence oracle. PATCH requires `Content-Length`, permits at
most 32 MiB, and returns `429 Retry-After: 2` when concurrency capacity is full.
Upload capabilities expire after at most 10 minutes (even though the upload
session may live longer); TUSKit fetches a fresh scoped capability before a
network request. They carry the originating device session and are rejected
immediately once that session is revoked.

TUSKit persists its applied custom headers. Therefore it must persist this
scoped upload capability, never the general 15-minute access token. If the app
has lost its local TUS metadata while an incomplete server resource is still
`uploading`, it calls `POST /v1/upload-sessions/{id}/restart` with its access
token. The gateway rejects stale PATCH requests, removes only that incomplete
staging resource, and returns the session in `created` state; the client starts
the same idempotent upload from byte zero. A complete resource is never
restartable.

On connection loss, keep the returned `Location`, issue authenticated `HEAD`,
read `Upload-Offset`, and resume exactly there. Do not mark the local queue item
complete after the last `204`: poll `GET /v1/upload-sessions/{id}` until it is
`available`. `quarantined` means the origin's byte count or SHA-256 did not
match and the client must not delete its source copy.

## Library viewing

`GET /v1/assets?limit=50&cursor=<opaque>` returns only the caller's verified,
non-deleted assets in a stable newest-first order. The response contains each
original's authenticated URL.

`GET` or `HEAD /v1/assets/{asset_id}/original` supports HTTP byte ranges for
video seeking and large original downloads. The gateway checks owner ID before
opening the file and returns `404` to a different account. The original's
storage path is never exposed in the API.

## Current omissions

Thumbnails, EXIF extraction, destructive deletion/retention semantics, external
alert delivery, and physical-device iOS acceptance remain unimplemented or
unproven. MFA source and iOS flows are implemented, but a real enrollment,
recovery-code custody test, and access-review record remain deployment evidence. They remain public launch blockers where marked P0. Scheduled
full-byte scrubs/signed manifests, encrypted-restic backup tooling, isolated
restore verification, audit export, private metrics/alerts, and synthetic probes
now exist as operator source, but they do not count as deployed evidence until
run against the real host/provider/public path. See `docs/runbooks/` and
`docs/audits/2026-08-24-proposal-implementation.md`.
