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

The response contains a 15-minute HS256 access token and a 30-day opaque
refresh token. Store the refresh token in iOS Keychain, never UserDefaults or
the photo queue database. `POST /v1/auth/refresh` rotates the refresh token;
replaying the old token returns `401`. Refresh sessions are token families:
reuse of a revoked token revokes every live descendant and emits a security
warning without revealing the account to the caller. `POST /v1/auth/logout`
revokes its token and is idempotent.

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

## TUS upload

Create the TUS resource by POSTing to `/v1/uploads/` with `Authorization:
Bearer <upload_token>`:

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
network request. This limits the damage window after device-session revocation.

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

MFA, thumbnails, EXIF extraction, deletion, scheduled manifest generation,
encrypted off-site backup/restore, audit export, alert delivery, and
physical-device iOS acceptance are not yet implemented. They remain public
launch blockers. The administrative signed-manifest CLI is documented separately
in `docs/runbooks/integrity-manifest.md`.
