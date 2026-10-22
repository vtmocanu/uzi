# Issue 3310: verification state

M1 and M2 are complete. M3 is incomplete; the real-worker proof found a
baseline boundary condition that conflicts with its required denial check.
No PNG rendering or visual acceptance is claimed.

## Real-worker attempt

The latest attempted matrix used candidate `96ea08643d732e291ac8502ec7b4d139ac0ec2ea`
and frozen base `3fb1fdbfa4b1e2d2932ca74c6bbbc2f85528540b`.
The base checkout retained both base manifests and received only candidate
`build.sh`; the fixture ran outside that checkout. The image was
`sha256:6b788ff9e9feb73e1777f98654b290a561420f26b2f9e8df5264539eed856c41`
(`0.86.0-rc.18+g82d835b`). Production agent source digest was
`31d4cb2ca5e5ce1bb049573a52401f076f910412ca4ff4c5be16a21372066b57`.
The fixture recorded all three helper digests. There were no helper overrides.

The first base/off cell used a fresh named Nix volume with image copy-up,
disposable /data and the real production entrypoint. Checkout/provisioning ran
as runner 10002; command effects ran as 10003. Both observed command effects
verified different empty HOME/TMPDIR paths, owner 10003 and mode 0700.

Observed receipts:

- Full base export copied: 7,396 files, 22,650 ms.
- Real opted-in root provisioning: Ruby 4.0.6, 26,990 ms; all three expected
  events and no warnings. This is one fresh-store measurement, **not** a
  completed before/after comparison or an unqualified cold-fetch benchmark.
- Network-enabled Go setup through the production command cache/launcher:
  `/opt/uzi-toolchain/bin/go`, Go 1.27.3, 67,814 ms, exit 0. It downloaded
  modules and compiled the real CLI test package with no selected tests.
  This is setup, not ANSI generation or screenshot rendering.
- The compatible Go distribution was supplied explicitly from the official
  Go toolchain module. The fixture mounted it read-only outside /nix and
  repointed the existing toolchain symlink before executing the unchanged
  entrypoint. This is the approved #3305 proof workaround, not its repair.
- The safe database probe observed runner-owned `db.sqlite`, uid/gid
  10002/10002, mode **0664**. Command uid 10003 successfully opened it with
  **O_RDWR only**. The probe immediately closed the descriptor and failed;
  it did not create, truncate, lock or write the real database.
- Failure occurred during the off-mode calibration command before it reached
  the sentinel or big-lock probes. The cache holder was removed. The exact
  owned container and Nix volume were removed and absence verified.
- Cell and matrix exit 1; 196 seconds including creation/setup/cleanup.

Logs in the run worktree: `.uzi/scratch/m3-matrix-96ea0864.log` and
`.uzi/scratch/uxlab-render.nuzqMi`.

An earlier attempt at `1a9d9d4d` failed before provisioning because the mounted
copy helper used top-level await under CommonJS. That harness failure is not
regression red. The helper was repaired, reviewed and covered by a direct CLI
regression test before the latest attempt. Its container/volume cleanup was
also verified (`m3-matrix-1a9d9d4d.log`, 43 seconds, exit 1).

## Why the boundary requires a decision

This is not a fixture umask discrepancy: production `agent/src/main.ts:334-342`
intentionally applies `process.umask(0o002)` under the uid split and documents
inheritance into runner children. `agent/src/runner-uid.ts:50-58` documents
runner-cmd membership in group runner. The fixture preserves that posture.

The approved M3 criterion requires O_RDWR opens of the existing Nix database
and lock to be denied in both off and required modes, and explicitly makes
an unexpected successful open fail the boundary check. The observed off-mode
database open violates that criterion on the untouched base manifests.
The probe remains strict; no check was weakened or bypassed.

The approved scope excludes provisioning, uid, permission, environment and
sandbox runtime changes. Restoring a denial by changing the production umask
or chmodding the database would exceed that scope. A formal owner design or
criterion decision is required before proceeding with the blocked proof.
`git diff --exit-code 3fb1fdbf -- agent/src agent/codex agent/templates`
passed at `96ea0864`: those production paths are unchanged.

Still unperformed: missing-freeze base red in either mode; candidate rendering
in either mode; command-side Devbox lock control; complete database/lock denial
and calibrated Landlock sentinel evidence; runner-owned local Devbox build;
matched provisioning timing; PNG signatures/freshness correspondence and
light/dark visual inspection. The successful Go setup does not satisfy these.

## Focused verification and repository checks

The checked implementation has 21 passing TypeScript fixture tests and 18
passing Python archive tests. Syntax, scoped ShellCheck and diff whitespace
checks passed. The copy CLI test fails when the old CommonJS-incompatible
helper is restored. The PNG size test now kills the demonstrated mutation
that moved its size check after file creation. No surviving reported mutation
was accepted. The README scanner false positive was removed by rewording the
sentence; a narrowed default-rules scan failed on the previous text and passed
on the new text, without an allowlist or suppression.

Evidence logs: `gate-log.dWeaWi`, `gate-log.7PorJ9`, `gate-log.XAdmhG`,
`gate-log.UYxLcQ` and `gate-log.ZZmPnj` under `.uzi/scratch/`.
M1/M2 receipts and reviews remain in their earlier committed fixture guidance.

`task --list` and `task --dry gate:repo` passed (`gate-log.cPEWdC`,
`gate-log.LoBiS2`); Task wiring has not changed since M2. The initial full
`task gate:repo` at `6d98db0e` exited 201 in the existing Git-config isolation
test after 36m29s (`gate-log.tMS65l`). That test drops inherited GIT_CONFIG
variables, losing command-side trust for the runner-owned checkout. The
unchanged narrowed test failed with the same dubious-ownership error and
passed with trust limited to this exact checkout in a disposable HOME
(`gate-log.P0yuLr`, `gate-log.hsb2R8`). The test source is byte-identical at the
base revision. An isolated full baseline gate was not claimed: Git-dependent
gates cannot run faithfully from the permitted exports without Git metadata.
No production trust settings or ownership were changed.

A full repository gate after the fixture repairs remains to be recorded.

Serial added-step timing on Node 24.20.0 with existing tools/caches:

| Added target | Wall time |
| --- | ---: |
| check:uxlab-freeze | 230 ms |
| test:uxlab-freeze | 4,124 ms |
| test:uxlab-build | 588 ms |
| Total | 4,942 ms |

The base gate has none of these three steps, so the local serial added cost
is about 4.9 seconds (`gate-log.sZEjjg`). This estimates lint-repo step cost;
actual hosted CI queue and critical-path impact remain unmeasured. Manual
container creation, provisioning, Go setup, failed attempts and future render
proof are separate costs and are not new CI jobs. The matched provisioning
cost comparison remains incomplete.
