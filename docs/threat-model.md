# Initial Threat Model

Status: architecture baseline; update with implementation evidence.

## Protected assets

- Original photo and video bytes.
- Ownership, albums, timestamps, EXIF, and account metadata.
- Password verifiers, sessions, tunnel credentials, and signing keys.
- The append-only upload history and signed integrity manifests.
- Backups and the ability to restore them.

## Trust boundaries

1. iPhone app, Share Extension, App Group container, and Keychain.
2. Public Internet and Cloudflare edge for the MVP.
3. Cloudflare Tunnel connector and authenticated gateway.
4. Application network, PostgreSQL, staging, media, and backup filesystems.
5. Operator/admin access to the home host.

Cloudflare can see MVP plaintext because it terminates TLS. Family users are
isolated by application authorization, but the home-server operator can access
original bytes. This is an explicit consequence of not using application E2EE.

## Primary threats and controls

| Threat | Required control |
|---|---|
| Partial upload becomes visible | Verify-before-visible state machine; no reads from staging |
| Corruption during transfer or storage | TLS, client/server SHA-256, immutable originals, scrubs, second verified copy |
| Offset races corrupt an upload | tusd file locker plus one authenticated owner per resource |
| Cross-user upload or download | Derive owner from session; authorize every tus method and download Range request |
| Token theft | Keychain storage, short access TTL, rotating hashed refresh tokens, device revocation |
| Password database theft | Argon2id, unique salt, rate limiting, no password logs |
| Malicious filename or metadata | Treat metadata as untrusted; opaque storage keys; bounded lengths; content sniffing after upload |
| Staging exhaustion | Per-user quota reservation, global free-space floor, upload expiry, termination cleanup |
| Media tampering by host malware | Signed manifests shipped off-host, read-only backup snapshots, alert on mismatch |
| Database/file commit split | Explicit `committing` state and idempotent reconciler |
| Cloudflare or VPS compromise | MVP accepts Cloudflare plaintext risk; later VPS uses L4 passthrough and origin-held TLS key |
| Ransomware or disk loss | 3-2-1 backup direction, offline/off-site copy, restore drills; RAID alone is insufficient |
| Supply-chain compromise | Pinned dependencies/images, SBOM, signature/provenance verification, scheduled vulnerability scans |

## Privacy-sensitive logging

Do not log access tokens, cookies, original filenames, EXIF, GPS coordinates,
raw IP addresses beyond a short operational retention period, or photo bytes.
Audit events use user/upload IDs, bounded error codes, offsets, byte counts, and
request IDs.

## Required abuse tests

- Change `owner_id` and storage metadata in a client request.
- Reuse another user's upload URL for HEAD, PATCH, DELETE, and GET.
- Replay a completed upload creation request.
- Send overlapping and out-of-order offsets concurrently.
- Disconnect during every state transition and kill each server process.
- Fill staging and originals filesystems independently.
- Modify one stored byte, delete an original, and tamper with a manifest.
- Restore database and media from backups taken at different times.

