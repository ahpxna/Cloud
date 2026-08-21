# Production Host Hardening Runbook

This runbook is a launch gate for a real family deployment. Use a dedicated,
supported mini PC/server with enough RAM and an SSD for PostgreSQL; a Raspberry
Pi is acceptable only after benchmark and restore tests prove it meets the same
controls. Do not use a desktop that also browses the web or runs unrelated
software.

## Host baseline

1. Install a supported Linux LTS release on a dedicated machine. Enable full
   disk encryption (LUKS) for OS, PostgreSQL, staging, originals, and local
   backup cache; keep recovery material offline.
2. Create a named non-root operator account. Disable password SSH and direct
   root SSH. Admin access must use Tailscale/WireGuard or a LAN management VLAN;
   do not expose SSH, PostgreSQL, Grafana, or Docker remotely.
3. Apply unattended security updates and reboot policy. Record OS release,
   kernel, Docker/Compose, disk serials, encrypted-volume status, and installed
   service versions in the monthly asset inventory.
4. Configure a default-deny host firewall. For Cloudflare MVP no inbound WAN
   rule is needed; permit only outbound DNS/NTP/HTTPS and the chosen encrypted
   backup endpoint. For direct/VPS ingress, expose TCP 443 only.
5. Store `.env`, manifest private key, Cloudflare token, backup credentials, and
   database credentials outside Git with mode `0600`, owned by the operator.
   Use separate credentials for gateway, backup, and manifest signing.

## Storage and containers

1. Use ext4 or XFS for the media volume. Confirm hard links and durable
   directory fsync work before storing originals; do not use FAT/exFAT/NFS for
   staging/originals.
2. Mount the media volume by UUID, fail closed if absent, and reserve a free
   space floor before accepting uploads. Monitor SMART/NVMe health and disk
   capacity.
3. Run `docker compose` only from the audited deployment directory. Keep the
   gateway non-root, root filesystem read-only, capabilities dropped, PostgreSQL
   private, and cloudflared unable to join the database network.
4. Verify every production image digest against the signed release provenance
   and record the exact digest in the change log. Never deploy a floating tag.

## Backups and recovery

1. Use encrypted backups with distinct credentials and an off-site destination.
   Preserve at least one immutable/offline generation. A RAID mirror is not a
   backup.
2. Back up PostgreSQL consistently with media and retain manifest public keys
   plus signed manifests in the off-host evidence location.
3. Run a restore drill on an isolated machine every quarter: restore database
   and originals, verify manifest signature, rehash a sampled set, log RPO/RTO,
   and obtain operator sign-off. Store the report for one year.
4. Do not permit phone-source deletion until an off-site backup and restore
   drill have both passed.

## Evidence before launch

- Completed host baseline checklist and asset inventory.
- Firewall/VPN proof and no-public-port scan result.
- CI security-scan artifacts, review of all high/critical findings, and image
  digest/provenance record.
- Backup job success plus an isolated restore-drill report.
- Account/access review, MFA proof for the operator, and incident tabletop.
