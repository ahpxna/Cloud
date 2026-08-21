# Public IP and CGNAT Runbook

## Determine the current network state

1. Open the home router's status page and record its WAN IPv4 and WAN IPv6.
2. From a device on that LAN, query an external address service and compare its
   observed IPv4 with the router WAN IPv4.
3. Treat WAN IPv4 addresses in `10.0.0.0/8`, `172.16.0.0/12`,
   `192.168.0.0/16`, or `100.64.0.0/10` as non-public.
4. If the addresses differ, confirm with the ISP whether another NAT layer or
   CGNAT is present. A double-NAT home router can also cause a difference and is
   fixable locally; ISP CGNAT is not.

Do not infer reachability from an address-check website alone. The router's WAN
address and an inbound TCP test are both required.

## Request a public address from the ISP

Ask specifically for one of:

- removal from CGNAT and a public dynamic IPv4 address;
- a public static IPv4 address; or
- a business/static-IP add-on if residential service cannot provide one.

A static address is convenient but not mandatory. A public dynamic address plus
DNS/DDNS is sufficient. Confirm whether the ISP blocks inbound TCP 443 and
whether bridge mode is supported before paying.

Public IPv6 is useful but is not the sole production path unless client network
coverage has been measured. Apply an IPv6 firewall; globally routable IPv6 does
not mean the host should accept every inbound connection.

## Direct-origin checklist

- DNS or DDNS points only at the intended public address.
- Router forwards TCP 443 only; TCP 80 is optional and avoided with DNS-01 ACME.
- UPnP is disabled.
- SSH, PostgreSQL, metrics, dashboards, and storage services are LAN/VPN-only.
- TLS uses a public CA and renews automatically.
- The actual path passes upload interruption, HTTP 104, IPv4, IPv6, and restore
  tests before Cloudflare is removed.

## When the ISP cannot help

Use a VPS with a stable public IP. Establish WireGuard from the home origin to
the VPS and perform HAProxy TCP passthrough on the VPS. Terminate TLS at home.
Benchmark throughput at multiple times of day; a geographically close VPS can
still have poor peering or an oversubscribed network.

