# Cloudflare edge controls

This directory versions the edge controls that were previously only a launch-gate note.

1. Create a scoped API token with **Zone WAF Write** for the target zone and export it as `CLOUDFLARE_API_TOKEN`.
2. Set `zone_id` and the exact `api_hostname` in a local `.tfvars` file that is not committed.
3. Import any existing zone-level rulesets before applying. Cloudflare rulesets are authoritative resources; do not blindly apply over unmanaged rules.
4. Run `terraform init`, `terraform plan`, review the expressions, then `terraform apply`.
5. Exercise real TUS `OPTIONS`, `POST`, `HEAD`, and repeated `PATCH` through the public hostname. Do **not** add a low request-rate limit to `/v1/uploads/*`.
6. Confirm no transform rule strips `Tus-Resumable`, `Upload-Offset`, `Upload-Length`, or `Upload-Metadata`.

The application still enforces durable login/upload admission controls. Edge rules are defense in depth, not the sole authorization mechanism.
