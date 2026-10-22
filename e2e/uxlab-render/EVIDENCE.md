# Issue 3310: verification state

M1 and M2 are complete in the tracked record. M3 is **INCOMPLETE**.
The completed matrix at `ab970306` exited 1. All four exact containers and Nix volumes were removed and absence verified.
No final matrix/local success, rendering receipts, PNG acceptance or light/dark
visual acceptance are recorded. The off-mode database guidance correction is
not a formal criterion change, scope waiver or partial-delivery approval.

## Current recovered attempt

Recovery initially restored a clean `b6bfe35d` snapshot; all previous scratch
logs were gone. Historical receipts below survive as tracked prose, not
available logs or current-candidate verification.

Current attempt logs, supplied by the lead (not reread for this doc update):
`.uzi/scratch/m3-matrix-75829170.log` and `.uzi/scratch/uxlab-render.RoqjfW`.
The fixed image is
`sha256:6b788ff9e9feb73e1777f98654b290a561420f26b2f9e8df5264539eed856c41`;
production source digest is
`31d4cb2ca5e5ce1bb049573a52401f076f910412ca4ff4c5be16a21372066b57`.

Observed current-attempt receipts:

- Base Ruby provisioning: 54,101 ms, no warnings.
- Network-enabled command Go 1.27.3 prewarm: 118,956 ms, exit 0. This uses
  the uid-10003 command cache; it is not a local-runner cache or rendering proof.
- Off-mode big-lock O_RDWR open succeeded, causing a boundary criterion failure.
  The descriptor is closed without writing. This observation remains unaccepted;
  the big-lock check is strict in off mode.
- Exact owned container and volume cleanup was verified; cell duration was
  310 seconds. This is not a completed four-cell matrix result.

The probe now records and immediately closes an observed off-mode database
open, separately from sentinel-only off-mode calibration. Required-mode real-file
denials and off-mode big-lock denial remain strict. Formal criteria for the
off-mode database observation are pending; the guidance correction does not
satisfy the original denial criterion or approve a scope change.

The matrix continues through base/off, base/required, candidate/off and
candidate/required, retaining an aggregate nonzero status for any failed cell.
A candidate `rendering-complete` marker can permit artifact export despite a
separate boundary failure. Export preserves the failed status and does not
constitute a pass. No such final rendering receipt is recorded here.

Earlier recovered attempts `0158297a` and `6d52ad47` failed before provisioning:
the first on BusyBox timeout arguments, the second on GNU multicall basename
resolution. Their logs were retained by the lead, but neither is regression red.
The runner helper now uses detached spawn with at most 4 MiB of stdout bytes
and 4 MiB of stderr bytes, calling production `killRunnerGroupOnly` on exit or
deadline. It has no in-image GNU timeout dependency. Host Docker bounds still
use timeout. Terminal reports ASCII JSON-quote each row's first 2048 source
characters under `FIXTURE_LOG=`; genuine `FIXTURE_EXIT=` is separate and raw
logs are unchanged.

## Completed recovered matrix at ab970306

The lead read the four raw logs under
`.uzi/scratch/proof-candidate-ab970306/.uzi/scratch/`:
`uxlab-render.kXYEj1`, `uxlab-render.HRH6SH`,
`uxlab-render.UUvQYY` and `uxlab-render.pUqApQ`.
Outer log: `.uzi/scratch/m3-matrix-ab970306.log`, exit 1.

| Cell | Observed result |
| --- | --- |
| base/off | Go setup passed; sentinel calibrated; boundary failed; Devbox control hit 900-second command deadline before base preflight |
| base/required | Calibrated sentinel and both existing-file opens denied; exact missing-freeze diagnostic with identical seed bytes/inventory/nanosecond mtimes |
| candidate/off | Root packages provisioned without warnings; Devbox control hit its 900-second deadline before generation |
| candidate/required | Provisioned freeze 0.2.2 resolved at its run profile; existing-file and sentinel denials passed; full build reached its 900-second command deadline |

Both required-mode Devbox controls exited 1 with a Nix version-detection error,
not the required lock diagnostic. Those controls **failed acceptance**.
All four provisioning calls emitted the expected events with no warnings.
Go 1.27.3 setup passed through the real command cache/launcher in each cell.
Fresh uid-10003 HOME/TMPDIR checks passed for observed command starts.
No completed PNG set was exported or visually inspected.

Fresh image-copy-up store provisioning measurements:

| Mode | Base Ruby only | Candidate Ruby + freeze | Difference |
| --- | ---: | ---: | ---: |
| off | 49,859 ms | 38,509 ms | -11,350 ms |
| required | 29,491 ms | 38,089 ms | +8,598 ms |

Each call used a separate fresh container, image-copied named Nix volume,
empty tier 1, opt-in, and the same default network/cache policy. External
CDN/cache conditions were not controlled. These are fresh-store observations,
**not cold-fetch measurements**; their variation does not establish a general
freeze overhead. Creation, checkout copying, Go setup and rendering are excluded.

The current repair preserves the image toolchain profile and overrides only
Go/gofmt, rather than hiding other image tools. Its focused regression passes
and fails against the former whole-profile replacement. The Devbox control
gets a separate bounded deadline and byte-capped diagnostic file; thrown
control errors remain failures while independent checks can continue.
Failed builds record bounded output and frame/PNG counts. These repairs were committed and received focused review; further audit fixes and the failed real-image results are recorded below. No complete positive proof is claimed.

## Repaired cell evidence and acceptance conflict

At 4b6aacb9 the base/off cell reached the exact missing-freeze diagnostic
and verified unchanged seed inventory, SHA256 bytes and nanosecond mtimes.
It retained FAIL for the strict boundary failure and the timed-out Devbox
control. The cache was removed; exact container/volume absence was verified.
Duration: 370 seconds. Log: .uzi/scratch/m3-base-off-4b6aacb9.log;
raw log: .uzi/scratch/proof-candidate-4b6aacb9/.uzi/scratch/uxlab-render.34H7kP.

At 2be4b045 the candidate/required cell preserved the original image tools
and fonts while supplying Go 1.27.3. The version diagnostic captured Native
Nix 3.21.5 failing to read /proc/meminfo under Landlock (also /proc/self/fd
and /proc/stat). Devbox 0.17.5 therefore reported an empty Nix version,
rather than the required database-lock diagnostic. The actual unchanged
sandbox grants no /proc subtree. Granting it or changing Nix/runtime
behavior is outside this issue's frozen scope.

Real Go generation completed successfully in 2.027 seconds with 260 ANSI
frames. The full build reached its 900-second deadline after 63 PNG paths
appeared. Those paths are not a verified complete PNG set; no PNG artifact
export or visual acceptance is claimed. Captured output and counts are in
.uzi/scratch/m3-candidate-required-2be4b045.log and raw
.uzi/scratch/proof-candidate-2be4b045/.uzi/scratch/uxlab-render.T9oDBH.
The command cache, exact container and named volume were removed and
absence verified.

This leaves an acceptance conflict in addition to the off-mode lock-open
observation. The supplied correction covered only the off-mode database
open; it did not waive the lock-specific Devbox control or the remaining
rendering proofs. M3 remains incomplete. A formal owner scope/acceptance
decision is required; no production runtime or sandbox grant was changed.
The unfinished rendering, local Devbox and visual work remains worker-owned.

## Pending M3 proof

| Required evidence | Current recorded state |
| --- | --- |
| Final four-cell outcomes and cleanup | ab970306 completed with exit 1 and verified cleanup; repaired candidate not yet run |
| Base missing-freeze red, unchanged seed bytes/mtimes in both modes | required passed at ab970306; off passed at 4b6aacb9 |
| Candidate full generation/render and offline HOME comparison in both modes | required generated 260 frames then build timed out with 63 PNG paths; no complete artifact set |
| Command-side Devbox big-lock negative control | Failed: off control timed out; required stops at Nix version detection before lock |
| Real-file boundary and calibrated sentinel checks | Off observation unaccepted; formal database criterion decision pending |
| Runner-owned local nested Devbox build | No successful local receipt; command prewarm uses a separate cache |
| Complete base/candidate provisioning comparison by mode | Matched ab970306 observations above; repair changes setup, so updated comparison remains pending |
| Host PNG export, signatures, dimensions, hashes and strict freshness | No final artifact receipt |
| Light/dark visual inspection | Unperformed; no acceptance claimed |
| Current full repository gate | Not yet rerun; historical pass below is not current verification |

The lead owns collecting matrix/local receipts and the remaining verification.
This documentation-only update runs no gates/tests and performs no rendering or
visual review. Compatible Go is an explicit #3305 fixture workaround, not a
repair of #3305. A downloaded distribution may need executable bits on bin/
and pkg/tool/ binaries and readable/traversable mount directories; see the
[fixture instructions](README.md) and [UX lab prerequisites](../../api/cmd/uzi/uxlab/README.md).

## Current supplied repository receipts

`task --list` and `task --dry gate:repo` passed: `gate-log.3kJ3k8` and
`gate-log.aMZWjk`. The base parity check failed as expected on the missing root
freeze declaration, exit 1: `gate-log.8XvjEz`. These are supplied receipts under
`.uzi/scratch/`, not checks rerun by this documentation update.

Serial added-step timing on Node 24.20.0 (`gate-log.r3myAF`):

| Added target | Wall time |
| --- | ---: |
| check:uxlab-freeze | 220 ms |
| test:uxlab-freeze | 5,015 ms |
| test:uxlab-build | 1,285 ms |
| Total | 6,520 ms |

The base gate lacks these steps. This local serial measurement is not a hosted
CI job measurement; queue, cache and critical-path effects remain unmeasured.
Whole-tree scan overhead was not isolated. Provisioning, Go setup and manual
render attempts are separate costs.

## Historical receipts (prior scratch logs lost)

These receipts predate recovery and do not verify `ab970306`.
M1 canary and post-repair receipts remain in the [fixture README](README.md).

- At `96ea08643d732e291ac8502ec7b4d139ac0ec2ea`, frozen base
  `3fb1fdbfa4b1e2d2932ca74c6bbbc2f85528540b` retained both manifests and
  received candidate `build.sh` only. The image/source digests matched those
  above; three helper digests were recorded without overrides. Fresh image-copy-up
  Nix and disposable /data were used, runner uid 10002 and command uid 10003.
  Full export copied 7,396 files in 22,650 ms. Ruby 4.0.6 provisioning took
  26,990 ms without warnings; Go 1.27.3 setup took 67,814 ms, exit 0.
  The then-strict off-mode database probe stopped on an O_RDWR observation
  before sentinel/big-lock checks. No real-file write occurred. Cleanup was
  verified; cell/matrix exit 1, total 196 seconds. Former logs:
  `m3-matrix-96ea0864.log`, `uxlab-render.nuzqMi`.
- At `1a9d9d4d`, a CommonJS top-level-await helper failure stopped the attempt
  before provisioning. Cleanup was verified, exit 1, 43 seconds. This was a
  harness failure, not regression red (`m3-matrix-1a9d9d4d.log`).
- The separate local leg at `573ca948` provisioned Ruby 4.0.6 and freeze 0.2.2
  in 30,878 ms without warnings, versus the historical base's 26,990 ms:
  one pair differing by 3,888 ms, not a completed comparison or cold-fetch
  benchmark. Local command Go setup took 67,770 ms. Runner-owned
  `devbox run build` exited 126 on the generated build script; no frames/PNGs.
  Cleanup was verified, exit 1, 208 seconds (`m3-local-573ca948.log`,
  `uxlab-render.M9VdVr`). Fixture opt-in does not verify an instance setting.
- Isolated execution probes observed default /data tmpfs denying a correctly
  owned mode-0700 script (126), and `rw,exec,nosuid,nodev` permitting it (0),
  with verified cleanup (`gate-log.gBfv2X`, `gate-log.NPcwGK`). An earlier
  uninitialized-mount diagnostic did not reach execution. The fixture adopted
  executable /data; production mounts, uid and grants were unchanged.
- The local retry at `50b8da9a` hit its one-hour cell deadline: exit 1,
  3,655 seconds including verified cleanup, no artifact receipts. The wrapper
  **at that revision** exited before collecting Docker child output on timeout,
  so the stalled stage was not identified (`m3-local-50b8da9a.log`,
  `uxlab-render.qrtn5f`). The current wrapper attempts bounded timeout log
  collection before cleanup; the historical limitation is not a current claim.
- Focused verification recorded 21 TypeScript and 18 Python tests passing,
  syntax/ShellCheck/whitespace checks, copy-helper and PNG-size mutation
  failures, and a narrowed scanner red/green without suppression. Former logs:
  `gate-log.dWeaWi`, `gate-log.7PorJ9`, `gate-log.XAdmhG`, `gate-log.UYxLcQ`,
  `gate-log.ZZmPnj`. These are historical results, not current test execution.
- Task list/dry-run previously passed (`gate-log.cPEWdC`, `gate-log.LoBiS2`).
  The initial full gate at `6d98db0e` exited 201 after 36m29s on Git-config
  isolation/checkout trust (`gate-log.tMS65l`). The unchanged narrowed test
  failed likewise and passed with disposable-HOME trust limited to the exact
  checkout (`gate-log.P0yuLr`, `gate-log.hsb2R8`). No production trust or
  ownership changed; no faithful full baseline gate from an export was claimed.
- A full `task gate:repo` at `573ca948` passed in 1,628 seconds (27m08s),
  before the /data execution correction. Secret/Semgrep canaries were detected
  with no tracked/tree findings; actionlint used the existing local fail-open
  skip because it was absent. Former log `m3-gate-573ca948.log` is lost.
  This historical pass is not current verification or hosted CI acceptance.
- Earlier local serial added-step timing was 230/4,124/588 ms (4,942 ms total,
  Node 24.20.0, former `gate-log.sZEjjg`). The current supplied 6,520 ms
  measurement above supersedes it; neither measures hosted CI job duration.
