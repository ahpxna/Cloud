# Family Photo Cloud Architecture

Status: accepted baseline for implementation  
Date: 2026-08-20

## Requirements and constraints

The initial deployment serves three to five family members. Each person has a
private account and library, can upload from the iOS Share Sheet, and can browse
their verified originals and derivatives. Losing a connection must never make
partial bytes visible as an asset. The initial origin is a single home server
and existing media disk; Cloudflare Tunnel provides reachability through CGNAT.

The design favors correctness and operability over horizontal scale. PostgreSQL
and derivatives should live on SSD. Incoming staging and immutable originals
must share one media filesystem so the durable no-overwrite commit cannot cross
a mount boundary. Originals require an independent second copy.

## High-level design

```text
 iOS app / Share Extension
   |  TLS 1.3, bearer session, tus 1.0 (32 MiB PATCH requests)
   v
 Cloudflare edge + Tunnel                         MVP trust boundary
   |
   v
 Authenticated upload gateway (Go)
   |-- authenticate every POST/HEAD/PATCH/DELETE
   |-- derive owner_id from the session
   |-- embed/proxy pinned tusd
   |-- append upload events
   |
   +--> PostgreSQL (users, sessions, uploads, assets, events)
   |
   +--> staging filesystem -- verify SHA-256 --> immutable originals
                                                |
                                                +--> derivative worker
                                                +--> integrity scrubber
                                                +--> encrypted backup
```

No route from Cloudflare may terminate directly on tusd. Random upload IDs are
not an authorization boundary.

## Upload transports

### MVP: tus 1.0

Use the stable tus 1.0 protocol, the official `tusd` v2 implementation, and
TUSKit on iOS. Set the client request chunk to 32 MiB so every request stays
comfortably below Cloudflare's 100 MB request-body limit. The client persists
its upload URL and source file in the shared App Group container.

The Share Extension queues the selected resource and starts work when its time
budget allows. The main app reconciles queued and failed work on every launch.
Background URLSession behavior must be measured on physical devices; TUSKit's
own documentation warns that sequential chunk scheduling is constrained by iOS.

### Long term: PhotoKit/IETF resumable upload

On iOS 26.1+, the PhotoKit Background Resource Upload extension gives the
system responsibility for durable library backup. It uses the IETF Resumable
Uploads for HTTP draft and requires the server to return an informational HTTP
`104 Upload Resumption Supported` response.

This transport is enabled only after the public path accepts requests larger
than 100 MB and an integration test proves that every intermediary forwards
`104` correctly. The upload gateway exposes both transports behind one
`UploadTransport` contract so migration does not alter the product data model.

## Upload state machine

```text
CREATED -> UPLOADING -> RECEIVED -> VERIFYING -> VERIFIED
   |           |            |          |             |
   |           +----------> FAILED <---+             v
   |                                              COMMITTING
   |                                                  |
   +-------------------------------> EXPIRED          v
                                                  AVAILABLE
                                                      |
                         checksum mismatch ----------> QUARANTINED
```

An implementation may retry transient failures without leaving the current
state, but every attempt and offset change is appended to `upload_events`.

## Integrity and durable commit

1. The client hashes the exact original resource with SHA-256 and creates an
   upload session containing byte size, media type, client asset identifier,
   and expected hash.
2. tusd writes only to a staging directory. Concurrent operations on one upload
   use the upstream file locker.
3. Completion changes state to `RECEIVED`; it does not create a visible asset.
4. The verifier reads the stored original from disk and computes SHA-256 again.
5. A mismatch moves the data to quarantine and records `CHECKSUM_MISMATCH`.
6. A match causes the file and staging directory to be `fsync`ed.
7. The verifier creates a same-filesystem hard link at the immutable destination
   without replacing an existing object, `fsync`s the destination directory,
   then unlinks staging. If the destination already exists, its bytes are
   re-hashed before it is accepted.
8. A PostgreSQL transaction inserts the asset and changes the upload to
   `AVAILABLE`. If the database commit fails after the rename, a reconciler
   finds and repairs the orphan; files are never overwritten.

The storage key is scoped to the owner, for example:

```text
originals/<owner_uuid>/<first-two-hex>/<full-sha256>.<safe-extension>
```

Cross-user deduplication is deliberately excluded because it creates privacy
side channels and complicated deletion semantics.

Nightly manifests sort asset records deterministically, encode them with a
versioned canonical format, and sign them with Ed25519. The manifest is an
inventory-tamper signal, not a substitute for the upload verification above.

## Authentication and authorization

- Registration is invite/admin-only for the MVP.
- Passwords use Argon2id with parameters stored beside the hash.
- Access tokens are short lived; rotating refresh tokens are hashed at rest.
- iOS keeps refresh material in Keychain, shared only with the approved app
  extension access group.
- Every upload operation checks both the token and `upload.owner_id`.
- Storage paths and owner IDs supplied by clients are ignored.
- Downloads are served by the gateway using authorization and Range support;
  tusd downloads remain disabled.
- Login, device revocation, upload, verification, deletion, and restore actions
  emit structured audit events without EXIF or original filenames in logs.

## Concurrency and backpressure

For the first deployment, permit two active upload requests per user and six
globally. Reject chunked-transfer PATCH requests and PATCH bodies above the
configured 32 MiB chunk limit. Stream request bodies to disk; never buffer an
original in memory.
Run derivative generation outside the request path with one video transcode and
at most two image jobs. Pause new uploads below a configurable free-space floor
and return a retryable response.

The WAN remains the likely bottleneck. Upload traffic into the home uses the
minimum of the phone's uplink, the home's downlink, tunnel capacity, and disk
write rate. Remote viewing uses the home's uplink; clients should browse cached
thumbnails and request originals only on demand.

## Deployment evolution

### Phase 1: Cloudflare Tunnel

Cloudflared establishes an outbound-only tunnel. Only the authenticated gateway
is published. Upload requests are chunked below 100 MB. Cloudflare terminates
client TLS, so it belongs to the MVP trust boundary.

### Phase 2A: ISP public IP

Ask the ISP to remove CGNAT or provide a public dynamic/static IPv4 address.
Confirm that the router WAN address matches the externally observed address,
then use DNS/DDNS, forward only TCP 443, disable UPnP, and firewall all
administrative services to LAN/VPN.

### Phase 2B: VPS plus WireGuard

When a public IP is unavailable, a VPS provides the reachable address. HAProxy
on the VPS performs layer-4 TCP passthrough through WireGuard to the home
origin. The certificate private key and TLS termination stay at home, preferably
with DNS-01 certificate issuance.

This is end-to-origin encryption, not application E2EE. The VPS cannot read
photo plaintext but can observe IP addresses, timing, and traffic volume. Speed
depends on VPS peering and both ends' bandwidth and must be benchmarked.

## Failure handling and observability

Required metrics include active uploads, received bytes, retries, resume offset,
verification duration, checksum mismatches, quarantined assets, commit repair,
queue depth, free bytes, backup age, and last successful restore drill.

Chaos tests inject packet loss, delay, bandwidth limits, connection resets,
gateway termination, tusd restart, database restart, and power-loss-like process
stops. Every scenario must prove that the final SHA-256 matches, offsets never
move backwards after acknowledgement, and no partial asset becomes visible.

## What to revisit

- Replace filesystem storage only when one host or one disk can no longer meet
  availability needs; S3 compatibility alone is not a reason to add MinIO.
- Re-evaluate native PhotoKit transport after the supported iOS floor and proxy
  interoperability tests are known.
- Add Vault only when service-secret rotation justifies its memory and recovery
  burden.
- Add an off-site replica before allowing users to delete local phone copies.
