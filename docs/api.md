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
in iOS Keychain, never UserDefaults or the photo queue database. `POST /v1/auth/refresh` rotates the refresh token. Updated clients also send a client-generated UUID as `rotation_request_id` and persist that UUID until a successful response. For 7 days (a phone often retries only when the app next wakes) the server can return the exact encrypted successor only when both the old token **and the same request ID** are retried and that successor is still live; `REFRESH_RETRY_ENCRYPTION_KEY_BASE64` is a dedicated persistent 32-byte key so that retry capsule remains decryptable across a gateway crash/restart; a different request ID is treated as replay and revokes the live family. Older clients that omit the request ID still rotate normally but do not receive lost-response idempotency. Refresh sessions are token families: reuse of a revoked token outside the exact retry case revokes every live descendant and emits a security warning without revealing the account to the caller. `POST /v1/auth/logout` revokes the entire refresh-token family for that device and is idempotent.


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

`expires_at` is the deadline for transfer admission only. `created`, `uploading`,
and `failed` sessions become `410 Gone` after that deadline, but durable states
such as `received`, `verifying`, `available`, and `quarantined` remain readable
for client reconciliation and are returned without a new upload capability.

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
token. The gateway takes tusd's per-upload lock (interrupting a stalled PATCH
for that upload), removes only the incomplete staging resource, and returns the
session in `created` state; the client starts the same idempotent upload from
byte zero. A complete resource is never restartable. `409 upload_busy` with
`Retry-After` means another request still held the upload; retry shortly.

On connection loss, keep the returned `Location`, issue authenticated `HEAD`,
read `Upload-Offset`, and resume exactly there. A new `HEAD` or `PATCH` for the
same upload asks a stalled earlier PATCH to release its lock, so resuming after
a network change takes about a second rather than waiting for a server timeout. Do not mark the local queue item
complete after the last `204`: poll `GET /v1/upload-sessions/{id}` until it is
`available`. `quarantined` means the origin's byte count or SHA-256 did not
match and the client must not delete its source copy.

## Library viewing

`GET /v1/assets` lists what the caller may see, with an opaque `next_cursor`:

| Query | Meaning |
| --- | --- |
| `view` | `library` (default), `photos`, `videos`, `live`, `selfies`, `screenshots`, `panoramas`, `favorites`, `recent`, `hidden`, `trash`, `on_this_day` (with `month`, `day`, `tzoffset` minutes) |
| `album`, `place`, `ids` | an album's items, a place cell's items, or up to 200 comma-separated IDs (own items in any state, or items in albums shared with the caller) |
| `sort` | `taken_desc` (default), `taken_asc`, `added_desc`, `size_desc`, `name_asc`, `favorites_first`; Recently Deleted is ordered by deletion time and an album by its own sort order |
| `media` | `photo` or `video` |
| `q` | search file name, caption, place name, camera model or date (`2026-09`) |
| `limit`, `cursor`, `tickets=1` | page size 1–100, the previous page's `next_cursor`, and view tickets |

The timeline is ordered by **date taken** (`taken_at`: the capture time read
from EXIF/QuickTime metadata, or the upload time when the file has none).
Library views exclude hidden and binned items. The motion video of a Live Photo
is attached to its still as `live_video` instead of being listed separately
(paired by Apple's content identifier). Each item carries capture metadata
(`captured_at`, `width`, `height`, `duration_ms`, `subtype`, `latitude`,
`longitude`, `place_cell`, `place_name`), curation state (`favorite`, `hidden`,
`trashed_at`, `purge_at`, `caption`), ownership (`owner_id`, `mine`,
`owner_name`) and, in a shared album, `added_by_name`, `like_count`, `liked`,
`comment_count`.

`GET /v1/assets/{id}?tickets=1` returns one item with camera `details` (make,
model, lens, aperture, exposure, ISO, focal length) and the albums it is in.
`PATCH /v1/assets/{id}` sets `caption`, `favorite` or `hidden` (owner only).

`GET` or `HEAD /v1/assets/{asset_id}/original` supports HTTP byte ranges for
video seeking and large original downloads. Only the owner, or a member of an
album the item is shown in, can open it; everyone else gets `404`. The
original's storage path is never exposed in the API.

`GET /v1/assets?tickets=1` also returns a `view_url` per asset: the original
URL plus a 10-minute view ticket scoped to that one asset and to the caller's
device session. Browser `<img>`/`<video>` elements, which cannot send an
`Authorization` header, use it. A ticket cannot list the library, open another
asset, or act as an access or upload token, and it stops working when the
device session is revoked. Originals are served byte-for-byte; nothing is
resized, transcoded or recompressed.

### Recently Deleted

`POST /v1/assets/batch` `{"ids": [...], "action": "favorite" | "unfavorite" |
"hide" | "unhide" | "trash" | "restore" | "purge"}` changes up to 1000 of the
caller's own items (the other half of a Live Photo is included) and returns
`{"changed": n}`. `trash` moves items to Recently Deleted; they disappear from
the library and albums and are **permanently deleted 30 days later** by the
gateway (hourly). `purge` deletes binned items now; `POST
/v1/assets/trash/empty` and `POST /v1/assets/trash/restore` act on the whole
bin. Uploading a binned photo again restores it.

A purge tombstones the asset row (`deleted_at`), removes it from every album,
records an append-only `asset_events` entry, and removes the original and its
preview. The original is first renamed into `.purging/` inside the database
transaction and deleted only after it commits; on startup the gateway deletes
leftovers of committed purges and puts back bytes of purges that never
committed. Purges and upload commits of the same content are serialised, so a
purge can never delete bytes a concurrent re-upload just committed. Older
restic snapshots keep purged files until they are pruned.

### Cutting a video and downloading several items

`GET /v1/assets/{id}/clip?start=3.2&end=9.7` (access token, or the item's
original view ticket) streams part of a QuickTime/MP4 video. The cut is
lossless: samples from the keyframe at or before `start` are copied unchanged
and an edit list makes playback begin exactly at `start`, so no frame is
re-encoded and the original is untouched. The response has an exact
`Content-Length` and the original's media type. `422 clip_unsupported` is
returned for layouts the cutter does not handle (for example fragmented MP4).

`POST /v1/assets/download` `{"ids": [...]}` (up to 1000) prepares a ZIP and
returns `{"url", "count", "total_bytes"}`; `GET` that URL (it carries a
30-minute download ticket bound to the caller's device session) streams an
uncompressed ZIP whose entries are the originals byte-for-byte, Live Photo
videos included.

### Albums, folders and sharing

| Request | Effect |
| --- | --- |
| `GET /v1/albums` | own and shared-with-me albums (cover, count, owner, `shared`, `can_add`) plus own folders |
| `POST /v1/albums` `{"name", "folder_id"?, "asset_ids"?, "member_ids"?}` | create, optionally with items and shared with family members |
| `GET` / `PATCH` / `DELETE /v1/albums/{id}` | details with members; owner changes `name`, `folder_id`, `cover_asset_id`, `sort_order` (`newest_first`, `oldest_first`, `added`), `members_can_add`; delete keeps the photos |
| `POST /v1/albums/{id}/assets` and `/assets/remove` `{"ids"}` | add own items (owner, or members when allowed); the owner removes any item, members their own |
| `PUT /v1/albums/{id}/members` `{"user_ids"}` | owner sets who the album is shared with; removing someone also removes the photos they added |
| `POST /v1/albums/{id}/leave` | a member leaves (their photos leave with them) |
| `POST /v1/albums/{id}/likes` `{"asset_id", "liked"}` | like or unlike an item |
| `GET` / `POST /v1/albums/{id}/comments`, `DELETE /v1/albums/{id}/comments/{comment_id}` | read and write comments; authors and the album owner can delete |
| `GET /v1/activity` | what others added or commented in shared albums in the last 90 days |
| `POST /v1/album-folders` `{"name", "parent_id"?}`, `PATCH` / `DELETE /v1/album-folders/{id}` | folders nest up to 5 levels; deleting one moves its contents up |
| `GET /v1/people`, `PUT /v1/people/me` `{"display_name"}` | family members on this server, and the caller's display name |

An album item is a reference: nobody gets a copy. The database enforces that
whoever adds an item owns it. Hidden or binned items are not shown to anyone.

### Places and summary

`GET /v1/places` groups the caller's located photos into ~5 km grid cells
(`cell`, average position, count, cover). `PUT /v1/places/{cell}`
`{"name": "Nhà bà nội"}` names a cell; `DELETE` removes the name. No location
ever leaves the server: there is no online reverse geocoding.

`GET /v1/library/summary` returns smart-album counts and covers, storage usage
(photos, videos, Recently Deleted, quota, free disk) and the available sorts.

### Capture metadata

The gateway reads metadata in the background after each commit (and once for
existing assets): EXIF/maker notes from JPEG, HEIC/HEIF/AVIF, PNG and TIFF/DNG,
and `mvhd`/`tkhd`/Apple `mdta` keys and `udta` atoms from QuickTime/MP4. Only
headers and metadata boxes are read, with bounded reads; the parser is fuzzed.
`PHOTO_TIMEZONE` (IANA name, default the container's zone) interprets capture
times written without an offset. JPEG and PNG previews are rendered on the
server; HEIC and video previews still come from the owner's browser.

## Thumbnails

iPhone originals are mostly HEIC, which the server cannot decode in pure Go,
so the owner's browser renders a preview (Safari decodes HEIC and video
frames) and uploads it once with `PUT /v1/assets/{id}/thumbnail`
(`Content-Type: image/jpeg`, access token, at most 512 KiB and 640×640). The
server decodes and re-encodes it, dropping any metadata, and stores it under
`thumbnails/<owner>/` on the media volume. `GET /v1/assets?tickets=1` then adds
a `thumbnail_url` whose ticket is identical for an hour so browsers cache it.
Thumbnail and original tickets are separate kinds: neither opens the other.
Thumbnails are derived data, outside the signed integrity manifest, and can be
deleted and regenerated at any time.

## Upload progress

`GET /v1/upload-sessions?limit=50` (access token) lists the caller's most
recently updated uploads with `state`, `expected_size`, `received_size`
(bytes on disk, also for single-request uploads), timestamps and `asset_id`.
For an unfinished upload it adds what the bytes received so far reveal:
`captured_at`, `duration_ms`, `width`, `height`, `camera`, a `preview_url`
(the first ≤32 MiB, with a 10-minute upload-preview ticket, enough for a
video's first frame or the top of a photo), a `thumbnail_url` when the JPEG
embeds an EXIF preview, `same_as_asset_id` when an item with the same capture
time (or name) is already backed up, and `cancellable`.

`DELETE /v1/upload-sessions/{id}` stops an unfinished upload: a request still
sending it is interrupted through tusd's lock-release protocol, the partial
bytes are deleted, and the session becomes `expired` with
`error_code: "cancelled"`. Sending the same file again restarts it. An upload
whose bytes all arrived returns `409 upload_already_received`.

## Single-request uploads (iOS Shortcut and web app)

`POST /v1/direct-uploads` takes the whole file as the request body. `POST /app/`
is an alias, so a Shortcut can use the same address the family opens to watch
progress (`https://<host>/app/#uploads`; the fragment is never sent):

| Header | Value |
| --- | --- |
| `Authorization` | `Bearer <upload key>` (Shortcut) or `Bearer <access token>` (web app) |
| `Content-Length` | required, at most `TUS_MAX_UPLOAD_BYTES` |
| `X-Content-SHA256` | hex SHA-256 the client computed |
| `X-File-Name` | file name, optionally percent-encoded; an extension is added if missing |
| `X-Media-Type` / `Content-Type` | optional; otherwise inferred from the extension or the file's first bytes |

The request runs the same state machine as TUS: admission control, tusd's
filestore under its per-upload lock, `received`, then server-side SHA-256
verification before the asset becomes visible. `202` means received and being
verified, `200` means already backed up (`duplicate: true`), `409` means
previously rejected or busy, `429` carries `Retry-After`. Sending the same file
again under the same name is idempotent; a digest mismatch is quarantined.

Upload keys (`fpcu_…`) are long-lived and **upload-only**: they cannot read the
library, list uploads or change the account. Manage them with an access token:
`GET /v1/auth/upload-keys`, `POST /v1/auth/upload-keys` `{"name": "iPhone của mẹ"}`
(returns the key once), `DELETE /v1/auth/upload-keys/{id}`. At most 10 are
active per account. `revoke-sessions`, `reset-password`, `reset-mfa`,
`disable-user` and `delete-user` revoke them too.

## MFA status

`GET /v1/auth/mfa` (access token) returns `{"available", "enabled", "pending"}`.
`POST /v1/auth/mfa/enroll` now also returns `qr_png_base64`, a QR code of the
`otpauth://` URI for enrolling from a computer. On an iPhone, opening the
`otpauth://` link adds the code to the built-in Passwords app, which then
autofills it in Safari. Confirming or disabling MFA signs out every device.

## Web app

`/app/` serves a same-origin web app (Vietnamese UI) modelled on the iPhone
Photos app: a timeline grouped by day with sort, filter, search and grid size;
multi-select (tap, drag across tiles, per-day and select-all); a viewer with
swipe, pinch and double-tap zoom, press-and-hold Live Photos, slideshow, info
panel with camera data, captions and map link; an iPhone-style trimmer (yellow
handles, filmstrip) to save part of a video; albums in nested folders, smart
albums, places, memories ("Ngày này năm xưa"), Recently Deleted and Hidden;
shared albums with likes, comments and an activity feed; saving to the Photos
app through the share sheet and ZIP downloads; uploads with drag-and-drop,
folder upload, preview and ✕ to stop a stuck upload; display name, storage
usage, Shortcut keys and two-step sign-in. It is served with `script-src
'self'` and no inline code. Credentials live in the browser's local storage,
which is acceptable only because the app is reachable solely inside the
tailnet (or behind Cloudflare) with that strict CSP.

## Current omissions

Face recognition, memories beyond "on this day", photo editing and online
reverse geocoding are intentionally out of scope. External alert delivery and
physical-device iOS acceptance remain unproven. MFA source and iOS flows are
implemented, but a real enrollment, recovery-code custody test, and
access-review record remain deployment evidence. They remain public launch
blockers where marked P0. Scheduled full-byte scrubs/signed manifests,
encrypted-restic backup tooling, isolated restore verification, audit export,
private metrics/alerts, and synthetic probes now exist as operator source, but
they do not count as deployed evidence until run against the real
host/provider/public path. See `docs/runbooks/` and
`docs/audits/2026-08-24-proposal-implementation.md`.
