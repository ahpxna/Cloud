# ADR 0003: Cloudflare MVP, Then Direct or Private VPS Relay

Status: accepted  
Date: 2026-08-20

## Context

The home connection may be behind CGNAT. The MVP needs a public App Store-safe
HTTPS endpoint without waiting for an ISP network change. Long-term operation
should remove the request-size cap and reduce the number of parties able to
terminate TLS.

## Decision

Use Cloudflare Tunnel for the MVP and keep each tus request at 32 MiB. Treat
Cloudflare as a TLS termination and metadata trust boundary.

Prefer a direct ISP-provided public IP later. If unavailable, use a small VPS
with HAProxy layer-4 TCP passthrough over WireGuard. Keep the certificate key and
TLS termination on the home origin. Issue the public certificate with DNS-01 so
certificate renewal does not depend on exposing an ACME HTTP endpoint.

Before enabling native IETF resumable upload, run an integration test that
observes both the `104 Upload Resumption Supported` interim response and the
final response through every proxy in the path.

## Consequences

Cloudflare is fastest to operate but retains a 100 MB request limit and can see
plaintext at its edge. The VPS design removes that termination point but adds
monthly cost, a new availability dependency, and traffic-analysis visibility.
Neither design is “100% secure”; endpoint compromise, stolen credentials, and
origin compromise remain in scope.

