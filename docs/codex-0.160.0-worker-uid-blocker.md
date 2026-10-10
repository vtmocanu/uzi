---
title: Codex 0.160.0 coordinated candidate decision
audience: design
---

# Codex 0.160.0 coordinated candidate

Date: 2026-10-10. Local evidence source: `a6904746802e87a1124aa95d73f1d70f426b472b` checkout and retained operator receipts; historical CI statuses below are maintainer-supplied provenance, not independently revalidated architecture/posture.

The human decision on 2026-10-10 revises acceptance and authorizes a narrow coordinated candidate: Docker-free regression RED on the lock-only change, then GREEN with launcher/probe/active M4 assertions matching the lock. Publish committed, reviewed candidate work with **exact-head real-lane acceptance pending**; this record does not establish a GREEN candidate or completed delivery.

- **M1: receipt/coordinate proof completed under the clarification.** Supplied real workerUID control and lock-only RED establish the revised boundary; historical launch operation/errno remains unavailable and unproven.
- **M2: candidate implemented.** Coordinates the launcher, probe, active fixtures and M4 assertions with the eight-field pin; includes observed Docker-free RED/GREEN and the additive ADR-1106 source-delta amendment. Review and integration checks are recorded separately from native acceptance.
- **M3: pending maintainer CI.** Acceptance requires the exact PR head and the evidence below.

## Supplied CI and static coordinate evidence

- [Real workerUID control 38019285046](https://github.com/vtmocanu/uzi/actions/runs/38019285046): `37e0a4e1`, package **0.159.3**, source commit `01fc69f4026735edfdf6789820549727a4867b11`; maintainer reports **passed**.
- [Exact lock-only RED 38019435933](https://github.com/vtmocanu/uzi/actions/runs/38019435933): `9ed69ac7`, package **0.160.0**, source commit `a956835d020762cb2b570053af06f643a11c0ecc`; maintainer reports **failed**.

Statically verified at the M1 evidence source `a6904746`: `agent/codex/install-codex.sh:76` sets `DEST="${PREFIX}/${CODEX_VERSION}"`, with default `PREFIX=/opt/uzi-codex` at line 31. The new 0.160.0 lock therefore installs into `/opt/uzi-codex/0.160.0`, while `agent/src/codex/launcher.ts:75` names `/opt/uzi-codex/0.159.3/bin/codex`. This coordinate defect is consistent with CI refusal; it does not prove historical `ForkExec` failed at `execve` with `ENOENT`, or an internal upstream failure.

Both official candidate package SHA-256 hashes were independently verified against downloads and API receipts for tag `rust-v0.160.0`, commit `a956835d020762cb2b570053af06f643a11c0ecc`:

- amd64: `4fcc47ab57f52ff75363951a8761146cd10c8288bd86fed45487dbb204a16b71`
- arm64: `7f0fe42ff22ecfa3a47bc4a34f5b22c4218b431a4ec0aba51c7d98299f07900c`

## Historical local infrastructure evidence

The default Docker daemon was reachable (29.8.0, x86_64). The initial control-image build log stops at `RUN apk add`, without an `EXIT` record; no completed image is evidenced. This is not an observed failed test.

A bounded operator connectivity probe on pinned `node:24-alpine` digest `e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf` recorded `wget` download timeout, `EXIT=1`, after 13 seconds. It requested `dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/APKINDEX.tar.gz`, although the base reported 3.24.1. This establishes connectivity evidence only, not an exact current apk endpoint test or Codex RED.

The exact 3.24 endpoint probe recorded download timeout, `EXIT=1`, after **11 seconds**, in `.uzi/scratch/gate-log.1id1O4`; it is connectivity evidence, not a workerUID test.

Unchanged Docker-free Python checks `check_test.py`, `run_test.py`, and `gate_test.py` recorded `EXIT=0`, elapsed 0.189/4.415/0.646 seconds respectively. Inventory recorded `EXIT=0`, identifying 29 required leaves; none executed. No real workerUID leaves ran locally, and no real-test before/after timings exist. The recorded inventory is historical; adding a regression may change the current inventory.

Supporting retained logs only, not durable proof: `.uzi/scratch/gate-log.N3L7dn`, `gate-log.glZwN5`, `gate-log.zuobhU`, `gate-log.V9iMqW`, `gate-log.djZeZi`, and `worker-uid-inventory.ZAYuFQ` (last five also under `.uzi/scratch/`).

## Docker-free coordinate regression

The candidate changes the production executable path to `/opt/uzi-codex/0.160.0/bin/codex` and coordinates the probe's version/both archive digests and active M4 assertions/evidence with the lock. Installer behavior and launch/isolation/refusal checks are unchanged.

From `agent/`, the command
`node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/codex-runtime-coordination.test.ts`
recorded six assertion failures on the lock-only candidate (`EXIT=1`, 1.996 seconds test duration; two seconds wall) and six passes after coordination (`EXIT=0`, 1.772 seconds test duration; two seconds wall), with zero skipped or cancelled tests. The RED failed on the actual old launcher path, probe version and both digests, and both active M4 version assertions. The GREEN also checks each M4 written-evidence version. An earlier TypeScript test-instrument error is excluded from this RED claim.

This test imports actual production constants and parses active M4 source; it neither launches nor substitutes for the real runtime. Focused probe/provider-plan fixture checks recorded `EXIT=0` (33 tests, one second wall). The current inventory still records 29 required workerUID leaves, none executed locally. There are no added waits, timeout changes, builds or queue costs in this regression's before/after timing.

Supporting scratch logs: `gate-log.kKvnFT` (RED), `gate-log.ZxuC8j` (GREEN), `gate-log.96WvKA` (focused fixtures), `gate-log.ncJ25z` (inventory), under `.uzi/scratch/`. These logs are not committed and are not durable CI receipts. The [ADR-1106 amendment](../adr/1106-codex-harness.md#amendment-coordinated-runtime-01600-candidate-2026-10-10) records the full direct pinned-source review.

## Pending exact-head acceptance

Maintainer CI must establish the actual **0.160.0 executable/package**, installer receipt and probe identity; execute the **full current workerUID inventory**, including the **seven affected launch leaves and both EACCES cases**; and pass the other required checks on the **exact PR head**. Preserve required checks, privileges, timeouts and workflows. No GREEN is recorded now.

Hold the PR unmerged if failures remain. Compare exact failure signatures with the supplied control and #2397; do not automatically attribute them upstream or classify EACCES as separate. Launch fallout is a hypothesis only: record whether coordination clears it. Broader adaptation requires a new plan gate.
