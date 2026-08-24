# Private Observability

The observability profile is intentionally unavailable to Cloudflare ingress.
It consists of a low-cardinality Go exporter, Prometheus, Alertmanager, and a
provisioned Grafana dashboard. All published ports bind to loopback.

## Prepare Grafana credential

```bash
mkdir -p .data/secrets
openssl rand -base64 32 > .data/secrets/grafana-admin-password
chmod 600 .data/secrets/grafana-admin-password
make observability-up
```

Default local endpoints are:

- metrics exporter: `127.0.0.1:9091`
- Prometheus: `127.0.0.1:9092`
- Alertmanager: `127.0.0.1:9093`
- Grafana: `127.0.0.1:3000`

The exporter intentionally omits filenames, EXIF/GPS, bearer tokens, refresh
tokens, and owner identifiers. Metrics cover durable upload state, verifier
backlog/age, asset count, storage free bytes, append-only event progress,
signed manifests, and the **latest** integrity result for each asset.

## Alerts

Prometheus ships rules for exporter loss, stale verification backlog, low media
free space, and current integrity failures. The committed Alertmanager receiver
is deliberately local-only and sends nothing externally. Configure and test a
real family-owned notification receiver before treating alert delivery as a P0
control, and never place notification API secrets in the repository.
