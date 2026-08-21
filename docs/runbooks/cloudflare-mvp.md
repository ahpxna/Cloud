# Cloudflare Tunnel MVP Runbook

Status: gateway implemented; real Tunnel and physical-device acceptance pending.

## Preconditions

- The public hostname belongs to the operator's Cloudflare zone.
- The gateway validates every API and tus method and enforces upload ownership.
- tusd is not directly reachable from the tunnel, host LAN, or another public
  route.
- Upload clients use a maximum 32 MiB request body.
- Health, metrics, PostgreSQL, and administrative interfaces remain private.

## Route

```text
photos-api.example.com -> Cloudflare Tunnel -> upload-gateway:8080
```

The connector uses a scoped tunnel token supplied at runtime. Never place the
token in Compose, Git, image layers, or shell history. Configure API responses
with `Cache-Control: no-store` and do not enable cache rules for upload or
authenticated download paths.

## Setup

1. Put the public hostname in a Cloudflare-managed DNS zone. A Tunnel route
   creates the DNS record; the home router needs no port-forward and no public
   IPv4 address.
2. Create a named Tunnel in Cloudflare Zero Trust and set its public-hostname
   service to `http://upload-gateway:8080`.
3. Store the scoped connector token only as `CLOUDFLARE_TUNNEL_TOKEN` in the
   ignored `.env` file. Generate `ACCESS_TOKEN_HMAC_KEY_BASE64` separately with
   `openssl rand -base64 32`.
4. Run `make edge-up`. The Compose topology lets cloudflared reach the gateway
   but not the PostgreSQL network.
5. Do not create a route to `tusd-lab`, PostgreSQL, Grafana, SSH, or a Docker
   socket. Disable caching for `/v1/*`.

Avoid interactive browser challenges on the mobile API hostname. Authentication
belongs to the product protocol; WAF and rate-limit responses must be machine
readable and retry behavior must distinguish `401/403`, `429`, and transient
`5xx` failures.

## Acceptance tests

- Upload a photo and a video larger than 100 MB using 32 MiB tus requests.
- Interrupt Wi-Fi/cellular at several offsets and prove only missing bytes are
  sent after resume.
- Kill cloudflared, the gateway, and tusd independently.
- Confirm the final client and server SHA-256 values match.
- Confirm staging objects cannot be downloaded or listed.
- Attempt cross-user HEAD, PATCH, DELETE, and download requests.
- Verify Cloudflare and origin logs contain no token, filename, EXIF, or body.
- Capture whether a synthetic HTTP 104 response survives the real Tunnel path;
  this is research evidence, not permission to exceed the 100 MB request cap.
