---
title: Codex 0.160.0 worker UID blocker decision
audience: design
---

# Codex 0.160.0 worker UID blocker

Date: 2026-10-10. Evidence source: `a6904746802e87a1124aa95d73f1d70f426b472b` checkout and supplied operator receipts.

The human-approved Delivery boundary / M1 exit permits a blocker stop without a speculative fix, retaining **0.159.3**. This records dispositions, not milestone completion or completed diagnosis/upstream audit. No compatibility PR is authorized by this report.

- **M1: blocked, not completed.** Neither a real 0.159.3 positive control nor a lock-only 0.160.0 named-leaf RED is evidenced. The failing operation/errno and EACCES dispositions remain unproven.
- **M2: not started.** Cannot adapt before proof.
- **M3: not started.** Cannot verify or deliver an unproved candidate.

The default Docker daemon was reachable (29.8.0, x86_64). The initial control-image build log stops at `RUN apk add`, without an `EXIT` record; no completed image is evidenced. This is not an observed failed test.

A bounded operator connectivity probe on pinned `node:24-alpine` digest `e67514e5d0f6c46656005e1b693b2ec9d52e80b641307de684d4a015ba7a4eaf` recorded `wget` download timeout, `EXIT=1`, after 13 seconds. It requested `dl-cdn.alpinelinux.org/alpine/v3.23/main/x86_64/APKINDEX.tar.gz`, although the base reported 3.24.1. This establishes connectivity evidence only, not an exact current apk endpoint test or Codex RED.

Unchanged Docker-free Python checks `check_test.py`, `run_test.py`, and `gate_test.py` recorded `EXIT=0`, elapsed 0.189/4.415/0.646 seconds respectively. Inventory recorded `EXIT=0`, identifying 29 required leaves; none executed. No real-test before/after timings exist.

Both official candidate package SHA-256 hashes were independently verified against downloads and API receipts for tag `rust-v0.160.0`, commit `a956835d020762cb2b570053af06f643a11c0ecc`:

- amd64: `4fcc47ab57f52ff75363951a8761146cd10c8288bd86fed45487dbb204a16b71`
- arm64: `7f0fe42ff22ecfa3a47bc4a34f5b22c4218b431a4ec0aba51c7d98299f07900c`

The launcher’s old `/opt/uzi-codex/0.159.3/bin/codex` path versus the versioned installer destination is a hypothesis, not proven cause.

Resume M1 with a permitted builder having package connectivity, or maintainer receipts establishing exact source/image/posture, old-version positive control and genuine candidate RED. Then M2 requires a coordinated minimal fix, regression and pin; M3 requires candidate receipt/probe/native identity, all 29 leaves and CI proofs. Preserve required checks and privileges, timeouts and workflows; execute no runtime mutation under this decision.

Supporting retained logs only, not durable proof: `.uzi/scratch/gate-log.N3L7dn`, `gate-log.glZwN5`, `gate-log.zuobhU`, `gate-log.V9iMqW`, `gate-log.djZeZi`, and `worker-uid-inventory.ZAYuFQ` (last five also under `.uzi/scratch/`).
