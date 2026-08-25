# Synthetic Upload and Resume Probes

Use a dedicated low-quota family account with no real photos. The synthetic
probe exercises the product path rather than the unsafe `tusd-lab`: login,
scoped upload capability, TUS resource creation, PATCH/HEAD resume, verifier
state transitions, private library lookup, authenticated download, and final
SHA-256 comparison.

```bash
make create-user EMAIL=probe@example.com ROLE=member
# put its password in a chmod-600 file, then:
make synthetic-probe \
  BASE_URL=https://photos.example.com \
  EMAIL=probe@example.com \
  PASSWORD_FILE=/protected/probe-password
```

Plain HTTP is rejected except when `-allow-http` is explicitly used with a
loopback host. If Docker Desktop/CI cannot publish the loopback gateway port,
start the gateway and use the isolated Compose-network fallback:

```bash
make synthetic-probe-docker \
  EMAIL=probe@example.com \
  PASSWORD_FILE=/absolute/protected/probe-password
```

That target authorizes HTTP only for the exact Compose DNS hostname
`upload-gateway`; it does not permit arbitrary RFC1918/plain-HTTP endpoints.

For a local gateway restart test:

```bash
PROBE_EMAIL=probe@example.com \
PROBE_PASSWORD_FILE=/protected/probe-password \
make chaos-resume
```

`chaos-resume` deliberately slows a 64 MiB upload, restarts the gateway, then
requires the client to recover by reading the authoritative server offset. It
refuses a non-loopback URL. Packet-loss/rate-limit `tc netem` tests still belong
in an isolated Linux test namespace/container with `NET_ADMIN`; do not grant
that capability to the production gateway.
