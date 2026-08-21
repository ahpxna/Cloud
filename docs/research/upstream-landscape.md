# Upstream Project and Protocol Survey

Reviewed: 2026-08-20

## Decision summary

Build the product-specific control plane and integrity boundary; reuse the
transport protocol and implementations. The selected MVP stack is `tusd` plus
TUSKit behind custom adapters. Immich remains the UX/feature benchmark, not the
storage transaction boundary.

## Relevant upstreams

| Project | Current useful capability | Fit | Decision |
|---|---|---|---|
| [Immich](https://github.com/immich-app/immich) | Strong self-hosted photo UX, mobile backup, multi-user libraries, thumbnails, metadata extraction, search | Best benchmark or fastest off-the-shelf deployment; mobile/server versions are tightly coupled and upload visibility is owned by Immich | Study UI, metadata, jobs, and operational lessons; do not depend on its multipart upload for the strict integrity contract |
| [Nextcloud](https://github.com/nextcloud/server) | Mature accounts, file sync, WebDAV, and [chunked upload](https://docs.nextcloud.com/server/latest/developer_manual/client_apis/WebDAV/chunking.html) | Excellent general file cloud; heavier and less photo-specific | Reference its resumable and quota behavior, not the product core |
| [Ente](https://github.com/ente-io/ente) | Mature private photo product with end-to-end encryption | Strong security reference, but server-side derivatives/search and easy family recovery conflict with the chosen UX | Reference threat modeling and clients only |
| [tus protocol](https://github.com/tus/tus-resumable-upload-protocol) | Stable resumable upload 1.0 contract with creation, offset, termination, and optional checksum extensions | Matches lossy networks and chunking below proxy limits | Selected MVP wire protocol |
| [tusd](https://github.com/tus/tusd) | Official Go reference server, disk/S3 storage, locks, hooks, metrics, IETF draft interoperability | Active, embeddable, avoids custom protocol correctness work | Pin v2.10.0 behind the product gateway |
| [TUSKit](https://github.com/tus/TUSKit) | Swift tus client, persistence, retry, URLSession background configuration | Best initial iOS adapter, but its docs warn against sequential chunking for ideal background behavior | Use for MVP and prove behavior on physical devices |
| [IETF resumable upload](https://datatracker.ietf.org/doc/draft-ietf-httpbis-resumable-upload/) | Offset discovery and append semantics integrated with modern HTTP | Long-term standards path and native Apple alignment; still an evolving draft | Implement behind an adapter after proxy and version-interop tests |
| [Apple PhotoKit background resource upload](https://developer.apple.com/documentation/photokit/uploading-asset-resources-in-the-background) | System-managed asset backup on iOS 26.1+; iOS 27 adds the async job extension | Best long-term background UX; requires full photo access, raw upload endpoint, `104`, and a path that accepts the asset size | Target after MVP and keep Share Extension for manual sharing |
| [Cloudflare Tunnel](https://github.com/cloudflare/cloudflared) | Outbound-only reachability through CGNAT | Fastest MVP ingress | Pin 2026.7.2; never route directly to unauthenticated tusd |

## Why not copy a complete cloud product?

Immich or Nextcloud would deliver more features sooner. They are valid if the
goal changes to “operate a family photo server now.” The current requirement is
stricter: client/server hashes, explicit interruption events, quarantine,
verify-before-visible commit, signed manifests, and an iOS app under this
project's control. Retrofitting those semantics inside a large upstream would
create a long-lived fork and make upgrades risky.

The design therefore adopts upstream components at stable seams:

```text
product app -> UploadTransport interface -> TUSKit now / PhotoKit later
product API -> upload handler interface  -> tusd now / IETF handler later
product integrity, accounts, metadata, audit, and commit -> owned here
```

## Important compatibility findings

- Cloudflare Free/Pro request bodies are limited to 100 MB. A 32 MiB tus request
  leaves room for headers and future policy changes.
- Cloudflare documents forwarding all origin 1xx responses as of July 2026,
  which is promising for HTTP 104, but the real Tunnel path still needs a
  captured integration test.
- Apple's native background resource upload sends an asset as a server request;
  it does not solve Cloudflare's maximum request body for large videos.
- Proxy behavior for HTTP 104 varies. An IETF HTTP working-group test found
  meaningful differences among HAProxy, Nginx, Caddy, and other intermediaries.
- TUSKit supports background URLSession but documents that chunking creates
  sequential-request scheduling constraints. “Works in foreground” is not
  accepted as proof of automatic backup reliability.

## Repository selection policy

An upstream is eligible when it has an explicit license, recent releases,
maintainer activity, tests, a security reporting path, and a replaceable adapter
boundary. Pin versions and review changelogs; never consume `latest` in a
production Compose file.

