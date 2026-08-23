# ADR-0006: Complete the free-first baseline before storage purchase

**Status:** Accepted  
**Date:** 2026-08-22  
**Deciders:** Family Photo Cloud operator

## Context

The current budget is reserved first for a 20 TB local media disk. Apple
Developer Program membership, a domain, a VPS, and a paid off-site backup
destination are deferred. The project must still make meaningful progress now,
without falsely presenting a local-only installation as a durable family
backup or a public product.

The custom gateway already owns the most important product semantics: account
isolation, resumable transfer, server-side SHA-256 verification, quarantine,
no-overwrite commit, audit events, and signed manifests. Replacing it with a
general file-cloud server before measuring it would discard that work and make
the integrity contract less explicit.

## Decision

Complete and test the following at zero new recurring cost before buying the
media disk:

1. Keep the gateway and its iOS source as the product core. Use the iOS app
   only on the operator's own phone via free Xcode provisioning; do not promise
   remote family distribution until an Apple Developer membership is available.
2. Keep the gateway bound to loopback. Do not start Cloudflare Tunnel or expose
   a public hostname until a real domain, host hardening evidence, and launch
   controls exist.
3. Add source-level regression tests, repository checks, a Linux host/media
   preflight, and an evidence checklist now.
4. Buy the 20 TB disk before any recurring service. On arrival, install it in
   a dedicated Linux host with LUKS and ext4/XFS, then run the preflight before
   copying irreplaceable originals.
5. Treat the first local disk as a staging/primary copy only. It must not cause
   users to delete phone copies. Public family use remains blocked until an
   encrypted off-site backup and measured restore drill exist.

## Options considered

### Option A: Buy Apple membership/domain first

| Dimension | Assessment |
|---|---|
| Immediate family UX | Better |
| Data durability | Poor |
| Cost priority | Wrong |
| Decision | Rejected |

It enables easier distribution but does not create a second copy of originals.

### Option B: Buy 20 TB storage first, complete free controls now (chosen)

| Dimension | Assessment |
|---|---|
| Immediate family UX | Local operator testing only |
| Data durability | Improves primary capacity, not backup durability |
| Cost priority | Correct |
| Decision | Accepted |

### Option C: Replace the gateway with a third-party cloud immediately

| Dimension | Assessment |
|---|---|
| Delivery speed | Moderate |
| Integrity ownership | Lower/less explicit |
| Migration effort | High |
| Decision | Deferred |

## Consequences

- A free Apple ID can install a development build on the operator's device but
  cannot distribute a stable remote build to parents through TestFlight/App
  Store.
- The app and Share Extension remain a tested source artifact until full Xcode
  is installed; the iOS code cannot be called release-ready yet.
- The 20 TB purchase unlocks local host commissioning, but not the P0 backup
  control. One disk is never a backup.
- A future client switch to Nextcloud or Immich remains possible, but must
  preserve separate accounts, verified original bytes, and backup/restore
  evidence.

## Action items

1. [x] Add pagination and available-state queue-cleanup regression coverage.
2. [x] Add host/media preflight and free-first commissioning checklist.
3. [ ] Install full Xcode and run generated-project XCTest plus own-device
   Share Sheet/resume acceptance tests.
4. [ ] Purchase and commission the 20 TB disk using the host preflight.
5. [ ] Choose and fund an encrypted off-site destination; run a restore drill.
6. [ ] Only then enable a public hostname and invite family users.
