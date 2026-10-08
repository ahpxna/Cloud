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

Prometheus ships rules (with `promtool` unit tests in `rules_test.yml`) for:

| Alert | Fires when |
| --- | --- |
| `PhotoCloudGatewayDown` | the gateway's `/readyz` fails for 5 minutes (outage, low free space, read-only disk) |
| `PhotoCloudBackupStale` | no restic backup was written **and checked** in 36 hours, or ever |
| `PhotoCloudIntegrityCycleStale` | no signed integrity manifest in 8 days |
| `PhotoCloudIntegrityFailureRecorded` | the latest scrub found a mismatched, missing or unreadable original |
| `PhotoCloudVerificationBacklogStale` | an upload has waited more than 15 minutes for verification |
| `PhotoCloudStorageReserveLow` | the media disk has less than 200 GiB free |
| `PhotoCloudMetricsDown` | Prometheus cannot scrape the exporter |

The committed Alertmanager receiver is local-only and sends nothing. To email
alerts, set the `ALERT_*` keys in `.env` (recipients, sender, SMTP host:port,
SMTP login), then:

```bash
make alert-email        # renders + validates the config; prompts for the SMTP password
make observability-up   # applies it
make alert-test         # FIRING email within ~1 minute, RESOLVED ~5 minutes later
```

The SMTP password is typed at a hidden prompt and stored mode 600 in
`.data/alertmanager/config/`, never in `.env` or Git. Use
`make alert-email RESET_PASSWORD=1` to replace it. For Gmail, use
`smtp.gmail.com:587` with an app password (Google Account → Security → App
passwords, which requires 2-Step Verification), not the account password.
Alert delivery counts as a P0 control only after `make alert-test` has reached
every recipient's inbox, including spam-folder checks.
