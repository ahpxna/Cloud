# ADR 0001: Use tus Before a Custom Upload Protocol

Status: accepted  
Date: 2026-08-20

## Context

The product must survive network loss, app suspension, retries, and process
restarts without accepting corrupt or duplicated assets. Implementing offset
negotiation, concurrent request locking, replay behavior, expiration, and client
persistence is a protocol project in its own right.

The Cloudflare MVP also imposes a 100 MB per-request limit. A typical iPhone
video exceeds that limit, so one raw multipart request is insufficient.

## Decision

Use tus protocol 1.0 for the MVP:

- `tusd` v2 is the pinned Go server implementation.
- TUSKit is the initial Swift client implementation.
- The Cloudflare path uses 32 MiB PATCH requests.
- The product gateway owns authentication, authorization, quota, expected
  SHA-256, audit events, verification, and durable commit.
- Both upstream integrations sit behind internal interfaces and contract tests.

Do not fork tusd or TUSKit initially. Patch upstream or replace an adapter if a
confirmed defect cannot be resolved.

## Alternatives considered

### Custom chunk API

Rejected for the MVP. It gives total control over metadata and background task
shape but duplicates mature offset, locking, retry, and client persistence work.
Integrity requirements make subtle protocol defects unusually expensive.

### Immich upload API

Rejected as the integrity transport. Immich is the strongest product benchmark
for self-hosted photo UX, but its upload API and mobile lifecycle are coupled to
that product. It does not provide the explicit verify-before-visible transaction
required here.

### Nextcloud WebDAV chunking

Rejected as the product core. It is mature and resumable but imports the larger
Nextcloud file-platform model and leaves the iOS photo-specific lifecycle to a
custom layer.

### IETF Resumable Uploads for HTTP only

Deferred, not rejected. It aligns with modern URLSession and PhotoKit background
resource upload, and tusd v2 already has interoperability work. The protocol is
still evolving and Cloudflare rejects a large initial request before the origin
can resume it. It becomes the preferred native transport after the public path
supports full asset sizes.

## Consequences

The MVP gets mature resumability quickly and remains below Cloudflare limits.
TUSKit's background chunk scheduling must be tested on physical iPhones; it is
not assumed to provide perfect automatic library backup. The transport can be
replaced without changing upload state or asset integrity contracts.

