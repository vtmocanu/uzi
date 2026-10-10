---
title: Codex 0.160.0 coordinated candidate decision
audience: design
---

# Codex 0.160.0 coordinated candidate

Date: 2026-10-10. M1 evidence source: `a6904746802e87a1124aa95d73f1d70f426b472b` checkout and retained operator receipts. Historical CI statuses are maintainer-supplied provenance, not independently revalidated architecture/posture. Gated, reviewed code head: `bc746d4da06bcdc20e47f150ab0b46246d1f2d30`.

The human clarification authorizes publishing the reviewed narrow coordinated candidate with **exact-head real-lane acceptance PENDING**. Docker-free regression RED on the lock-only change and GREEN after coordination establish the revised local boundary; they do not establish actual workerUID GREEN or completed delivery.

- **M1: receipt/coordinate proof completed under the clarification.** Supplied workerUID control and lock-only RED establish the revised boundary; historical launch operation, errno and cause remain unproven.
- **M2: candidate implemented and reviewed.** Launcher, probe, active fixtures and M4 assertions match the eight-field pin; Docker-free RED/GREEN and the additive ADR-1106 source-delta amendment are recorded.
- **M3: local verification/handoff recorded; real-lane acceptance pending under the clarification.** This is not the original full real-lane GREEN milestone.

## Supplied CI and historical coordinate evidence

- [Real workerUID control 38019285046](https://github.com/vtmocanu/uzi/actions/runs/38019285046): `37e0a4e1`, package **0.159.3**, source commit `01fc69f4026735edfdf6789820549727a4867b11`; maintainer reports **passed**.
- [Exact lock-only RED 38019435933](https://github.com/vtmocanu/uzi/actions/runs/38019435933): `9ed69ac7`, package **0.160.0**, source commit `a956835d020762cb2b570053af06f643a11c0ecc`; maintainer reports **failed**.

At M1 source `a6904746`, `agent/codex/install-codex.sh:76` set `DEST="${PREFIX}/${CODEX_VERSION}"`, default `PREFIX=/opt/uzi-codex` at line 31: a 0.160.0 lock would select `/opt/uzi-codex/0.160.0`, while `agent/src/codex/launcher.ts:75` named `/opt/uzi-codex/0.159.3/bin/codex`. This historical path mismatch does not establish the historical failing operation, errno or cause. The current launcher names `/opt/uzi-codex/0.160.0/bin/codex`.

Both official architecture artifacts' manifests and SHA-256 hashes were independently verified at M1 against downloads and API receipts for tag `rust-v0.160.0`, commit `a956835d020762cb2b570053af06f643a11c0ecc`:

- amd64: `4fcc47ab57f52ff75363951a8761146cd10c8288bd86fed45487dbb204a16b71`
- arm64: `7f0fe42ff22ecfa3a47bc4a34f5b22c4218b431a4ec0aba51c7d98299f07900c`

## Historical local infrastructure evidence

The default Docker daemon was reachable (29.8.0, x86_64). The initial control-image build log stops at `RUN apk add`, without an `EXIT` record; no completed image or failed test is evidenced. A bounded connectivity probe on pinned `node:24-alpine` digest `e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf` recorded `wget` timeout, `EXIT=1`, after 13 seconds, requesting `dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/APKINDEX.tar.gz` although the base reported 3.24.1. The exact 3.24 endpoint probe timed out, `EXIT=1`, after 11 seconds (`.uzi/scratch/gate-log.1id1O4`). These are infrastructure/connectivity evidence only, not Codex RED or workerUID tests.

Unchanged Docker-free Python `check_test.py`, `run_test.py`, `gate_test.py` recorded `EXIT=0`, elapsed 0.189/4.415/0.646 seconds. Inventory recorded `EXIT=0`, 29 required leaves, none executed. Supporting retained logs: `.uzi/scratch/gate-log.N3L7dn`, `.uzi/scratch/gate-log.glZwN5`, `.uzi/scratch/gate-log.zuobhU`, `.uzi/scratch/gate-log.V9iMqW`, `.uzi/scratch/gate-log.djZeZi`, `.uzi/scratch/worker-uid-inventory.ZAYuFQ`. These historical checks were not rerun for this handoff; the full current inventory is still required.

## Docker-free coordinate regression

The candidate coordinates the production executable path, probe version/both archive digests and active M4 assertions/written evidence with the lock. Installer behavior and launch/isolation/refusal checks are unchanged. From `agent/`, `node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 test/codex-runtime-coordination.test.ts` recorded six assertion failures on the lock-only candidate (`EXIT=1`, 1.996 seconds test, two seconds wall), then six passes after coordination (`EXIT=0`, 1.772 seconds test, two seconds wall), zero skips/cancellations. RED exercised the old launcher path, probe version/both digests and both active M4 version assertions; GREEN also checks each written-evidence version. An earlier TypeScript instrument error is excluded.

The test imports production constants and parses active M4 source; it does not launch the real runtime. Focused probe/provider-plan fixtures recorded `EXIT=0` (33 tests, one second wall); inventory still recorded 29 required leaves, none executed locally. Logs: `.uzi/scratch/gate-log.kKvnFT` (RED), `.uzi/scratch/gate-log.ZxuC8j` (GREEN), `.uzi/scratch/gate-log.96WvKA` (fixtures), `.uzi/scratch/gate-log.ncJ25z` (inventory). The [ADR-1106 amendment](../adr/1106-codex-harness.md#amendment-coordinated-runtime-01600-candidate-2026-10-10) records the direct pinned-source review.

## Local verification and review handoff

Lead-supplied results below are **local ordinary/unit and M4 checks**, not actual workerUID-lane acceptance. Initial 45-file unit `2de28f06` had no reviewer blockers; the tester required stale template-version regex assertions to be fixed. The one-line fix at `bc746d4da06bcdc20e47f150ab0b46246d1f2d30` was reviewed clean by both. Optional duplicate parity cleanup is deferred.

| Command / code head | Recorded outcome |
|---|---|
| `task gate:agent` / `2de28f06` | Task `EXIT=201` (shell wrapper 1); unit 11,406 total, 11,401 pass, 2 fail, 3 skip, 0 cancel; two exact template-comment version assertions at `agent/test/templates-guardrails.test.ts:568`. Unit 1,743s, outer 1,762s; M4 not reached. |
| `task gate:agent` / `bc746d4` | `EXIT=0`; unit 11,406 total, 11,403 pass, 0 fail, 3 skip, 0 cancel, 2,308s; outer 2,474s. M4 183 pass, 0 fail/skip/cancel, 116s; strict completeness 37 clauses, 64 executed results, required `[claude/U,codex/U,codex/P]` satisfied. Log `.uzi/scratch/gate-log.taJCQg`. |
| Combined focused command below / `bc746d4` | `EXIT=0`, 72 pass, 0 fail/skip/cancel, 4.986s test; `.uzi/scratch/gate-log.pZuz69`. Synthetic installer/probe fixtures do not prove actual worker receipts. |
| `task test:codex-supervisor` / `2de28f06` | `EXIT=0`, fmt, vet and tests; `.uzi/scratch/gate-log.mqKZLL`. The `bc746d4` fix changes only the template assertion. |
| `task check-docs:web` and `go -C api test -race -count=1 ./internal/uzidocs -run '^TestEmbeddedDocsMatchSource$'` / `2de28f06` | Each `EXIT=0`; `.uzi/scratch/gate-log.NHTaal` and `.uzi/scratch/gate-log.ZwLWEz`. Rechecking this documentation update is lead-owned. |

Combined focused command, from repo root: `node --import ./agent/node_modules/tsx/dist/loader.mjs --import ./agent/test/setup/hermetic-proc.ts --test --test-timeout=120000 agent/test/templates-guardrails.test.ts agent/test/codex-pin-parity.test.ts agent/test/codex-runtime-coordination.test.ts agent/test/codex-runtime-probe-installer.test.ts`.

The unit gate reported three skips; no skip logic was changed. No wait, timeout or concurrency change was made. Full-unit timings include existing tests; real-UID before/after timings and distinct cache, queue or build timings are unavailable. Scratch logs are relative, uncommitted and not durable CI receipts.

## Pending exact-head real-lane acceptance

Maintainer ordinary CI must establish the actual **0.160.0 worker executable/package, installer receipt and probe capability**, execute the **full current workerUID inventory including the seven historical launch leaves**, and pass required checks on the **exact PR head**. Independently record each pending EACCES outcome:

- `real advice cwd disposal never deletes outside files during swaps`: **PENDING**; whether coordination clears it is unknown.
- `ordinary advice cwd disposal removes the worker-owned tree`: **PENDING**; whether coordination clears it is unknown.

Preserve the two-shard union, M4 once and required checks, privileges, timeouts and workflows. No CI trigger/result or actual workerUID GREEN is claimed. Actual-worker receipts/capability and arm64 execution/build receipts remain maintainer/CI pending; manifest/hash verification is not execution proof. #2532 remains open and maintainer-owned. Hold the PR unmerged if failures remain. Compare exact signatures with the supplied control and #2397. Launch fallout is a hypothesis only; neither EACCES case is classified as a separate upstream defect. Broader adaptation requires a new plan gate.
