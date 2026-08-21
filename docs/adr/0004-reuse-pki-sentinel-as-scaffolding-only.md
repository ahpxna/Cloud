# ADR 0004: Reuse PKI Sentinel as Scaffolding Only

Status: accepted  
Date: 2026-08-20

## Context

The sibling `/Users/phanan/pki-sentinel` repository contains mature deployment,
security, observability, chaos-test, and signed-baseline work. It contains no
photo storage, upload protocol, family authentication, or iOS product code.

## Decision

Reuse these patterns:

- Compose health and initialization gates, version pinning, Make targets, and
  ignored runtime state.
- CI, secret scanning, vulnerability scanning, SBOM, signing, and provenance.
- Prometheus/Grafana/Alertmanager provisioning after metrics stabilize.
- `tc netem` cleanup and cancellation discipline for upload chaos tests.
- The truststore drift agent's SHA-256 plus Ed25519 signed-baseline design,
  adapted to canonical asset manifests.
- ADR, threat-model, runbook, and benchmark organization.

Do not copy private PKI roots, `.internal` routes, demo credentials, insecure
Traefik dashboard settings, unconditional `curl -k`, revocation logic, or Vault
AppRole as user authentication.

Vault, Wazuh, and OPA remain deferred optional profiles. Public iOS TLS uses a
publicly trusted CA and is independent of the PKI lab.

## Consequences

This materially reduces infrastructure and assurance work while preserving a
clean product boundary. It does not reduce the core work for upload ownership,
verification, durable storage, authentication, or iOS background behavior.

