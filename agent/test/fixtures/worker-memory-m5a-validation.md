# Worker memory M5a — required real-process lane

## Accepted candidate and boundary (2026-10-15)

Implementation commits are `7001785edb71273c602f45ed4c3fd329d699889e`
(required-lane enrollment) and `b64fc729e7748a32f694b5284a1adff069e602a0`
(report bounds and ownership-verified cleanup). The latter is the executed candidate.
M1 and M4 evidence remains historical at its named revisions; this record adds the
previously unexecuted real command fixture. M5b calibration and final gates remain open.

The published floor `a7f721c0fba78725a70563979b363bccc9a296a9` and approved
merge target `4103fad73896f103bf7cebd743f009b0b6bde671` remain ancestors.
The branch still lacks PR #3039, as explicitly authorized by the human deviation;
landing must merge current main including that change. Its three files were not edited.

## Delivered harness behavior

The existing required task retains the original 38 source-derived leaves in five
agent test files and the real root-start worker entrypoint/capability transition.
A separate serial container runs the explicit `.mts` command proof as UID/GID 10003,
with all five capability sets empty and no-new-privileges. It mounts source, package
metadata, supervisor inputs and image dependencies explicitly; the package metadata
preserves ESM resolution. The real supervisor and command sandbox execute.

The scoped compiler and literal `test(...)` inventory regression run through
`test:worker-uid-harness`, consumed by `test:agent:worker-uid`. Separate inventories,
JUnit files, exit statuses and checkers require positive execution of both lanes.
The existing CI trigger/aggregate job remains unchanged; no workflow file was edited.

Both report streams have an actual-write cap of 8,388,608 bytes. Overflow stops the
owned foreground producer and fails even if the retained XML parses. Standalone
parsing also bounds bytes, XML items, cases, depth, attributes and text and rejects DTDs.
Unpredictable invocation tokens label resources; a host-only attempted-request journal
covers lost acknowledgements. Cleanup requires matching labels and valid immutable
container IDs, refuses uncertain metadata and shares a 30-second budget. Metadata
failure before creation authorizes no removal. Every Docker call has an explicit
watchdog; provisioning and the command proof include bounded kill grace.

## Executed checks

Raw logs are retained under `.uzi/scratch/gate-log.<suffix>` in this clone. Scratch
is not a durable artifact or part of the committed source.

| Candidate/check | Actual result | Log suffix |
| --- | --- | --- |
| Pinned Task `test:worker-uid-harness` at the repaired tree | Exit 0: 22 checker tests, 15 wrapper tests, required-check truth table, command inventory regression, scoped `npx tsc` | `i6OgcI` |
| ShellCheck on `run.sh`; `git diff --check` | Exit 0 | Lead command output |
| First real expanded `run.sh` at b64fc729 | Exit 0: 38 worker leaves and one command proof, cleanup successful; 18:25:23–18:26:23 UTC | `fwk7VJ` |
| Full pinned Task `test:agent:worker-uid` at b64fc729 | Exit 0: all host checks/compiler and both real invocations; 18:32:28–18:34:58 UTC, 150 seconds | `dp4qT1` |

The full-task JUnit reports contain exactly 38 and one executed cases, occupying
12,411 and 446 bytes respectively, both with `EXIT=0` sidecars. The command case
ran for 7.542240 seconds (the earlier script invocation's case took 9.126349 seconds).
The checker rejected no missing, skipped, failed or duplicate required case.
Follow-up bounded Docker inspections returned explicit no-such-resource diagnostics
for all three exact journaled container names and the dependency volume. Those
expected inspect exit-1 results establish absence; they are not failed fixture checks.

The real command fixture positively exercises adopted detached descendants, verified
RSS ranking, protected/unattributed refusal, injected supervisor and primary start-tick
mismatch refusal before signaling, concurrent cleanup joining one disposal, sibling
and unenrolled-tree survival, confirmed drain and the exact structured pressure reply
through the affected broker result and output projection. It checks eventual cleanup
of its owned roots. Injected start-tick changes prove rejection logic, not real kernel
PID recycling. Injected pressure feedback is not a measured guard/cgroup intervention.
Starting directly at command UID does not prove a production worker-to-command
transition; the original worker lane separately retains its existing transition proofs.

## Offline environment and retained failures

Host: Linux 6.12.0-243.el10.x86_64, Node 24.20.0, Python 3.14.7, Go 1.26.8,
pinned Task 3.54.0. The locally constructed offline fixture image is
`mem2396-offline:complete`, ID
`sha256:22d5a16b260c8bb2359b392c4c86a923e22bf262bfb7e470aba4a95c5eff5478`.
It uses the cached Node 24.21.0 Bookworm image, local BusyBox/tini/musl loader,
local Go toolchain and locked agent dependencies, provided complete Codex 0.160.1,
and vendored, statically built supervisor helpers. This is a fixture image, not a
validation of the full production worker image. No package install, image pull,
networked RUN step or networked build fallback was executed. All fixture containers
used `--network none`; the required lane used `WORKER_UID_SKIP_BUILD=1`.

Initial offline image preparation succeeded (`8Mh4r9`), but a dependency COPY was
excluded by the existing Docker ignore file (`dlHDjm`, exit 1). A scratch-only ignore
override corrected that input (`zOkndo`, exit 0); package metadata was added offline
(`u5A624`). No unchanged stalled operation was retried. The first baseline script run
(`lweXyS`, exit 1, 17:53:19–17:53:42 UTC) failed existing ownership/planning cases.
Narrow diagnostic exports retained every assertion: `kly90S` and `8grya4` failed,
then `G52N2E` passed both named cases after completing local runtime prerequisites.
Diagnostics established directory mode 2700 versus required 700 with GNU chmod;
the image also lacked the pinned Codex executable and needed BusyBox kill behavior.
The corrective offline provisioning phase had an outer 115-second timeout plus
five-second grace and bounded Docker build (`535nVd`, exit 0). It copied provided
local inputs only. These were prerequisite failures, not demonstrated product defects.

The repair implementer was cancelled before its final report. Interrupted log
`qNFJMf` has no final exit result and is not counted green. The lead's fresh harness
and full required task above establish the committed repair's results. Earlier failed
repair regressions and diagnostic logs remain retained, not erased.

## Local timing, not hosted CI timing

Baseline source is default `4103fad73896f103bf7cebd743f009b0b6bde671`, exported
without another worktree. Its matching three supervisor binaries were built outside
the measurement, with the same Go version, `CGO_ENABLED=0`, `-mod=vendor` and
`-buildvcs=false` (`4jLSDm`, exit 0). Both runs used the same offline dependency
image, unchanged locked JS manifests, serial host execution and the existing 600-second
worker-lane wall bound. Image preparation is excluded from both measurements.
Candidate provisioning of its new dependency volume is inside its measured lane.

The baseline's real execution used a scratch operational wrapper: no-pull, bounded
CID-receipt cleanup, five-second kill grace and read-only mounts of revision-matched
helpers. Its test sources, assertions, selected 38 leaves and Node test flags were
unchanged. The export Taskfile called this wrapper only for the real invocation;
its stub regressions used the unmodified default script. An earlier whole-lane attempt
accidentally exposed the operational wrapper to those stubs (`kRsPlh`, exit 201), so
it failed instrumentation before Docker. Separating the wrapper repaired the setup;
the narrow seven default wrapper regressions passed (`gLDIEL`). This is not evidence
of a default product failure.

| Whole required-lane composition | UTC interval | Exit | Local seconds |
| --- | --- | --- | --- |
| Default baseline, operational wrapper as described | 18:30:58–18:31:48 | 0 | 50 |
| M5a candidate b64fc729, canonical task | 18:32:28–18:34:58 | 0 | 150 |

Baseline log is `GIqZWG`; it passed 20 checker tests, seven wrapper tests, the
required-check truth table and all 38 real leaves. The observed delta is +100 seconds
(+200%), at one-second wall-clock resolution. This includes different test inventories,
additional compiler/inventory checks, dependency provisioning and the command proof;
it does not isolate a production performance change. Host/cache variation is uncontrolled
beyond using the same host/image and strictly serial owned jobs. No queue or CI
critical-path measurement was made. M5b must record final composed-lane timing;
post-publication CI step/dependency-path acceptance remains lander-owned.

## Review and residual scope

Reviewer and tester reviewed 0371db71..7001785e. Tester verified unchanged ordered
38-leaf inventory and a swallowed-command-exit mutation. Auditor independently found
High unbounded report ingestion and Medium deletion without established ownership.
The b64fc729 repair received scoped reviewer, tester and auditor review. All found
no blocking introduced defect; the auditor independently closed both original findings.
Tester exercised raised-parser-cap, bypassed-ownership and uncapped-write mutations,
which failed their intended assertions. The lead inspected the repaired mechanisms
and executed the fresh canonical checks above.

At 7001785e the required secret scan's first 60-second attempt timed out (`Bg9Zwt`,
exit 124); the longer retry completed (`TzoOpv`, exit 0), detected both canaries and
reported zero tracked findings. The agent dependency audit reported zero vulnerabilities.
Scoped Semgrep found no applicable files, so it provides no coverage for this unit.
`govulncheck` was unavailable on PATH. The repair auditor did not have fresh scanner
binaries on PATH and reported that limit; no download was attempted. These scans
are separate from the canonical worker-UID task. Final repository/component scans
and gates required by M5b remain open.

Reviewer timing notes at 7001785e and b64fc729 are addressed locally by the measured
comparison above; hosted CI timing remains deferred per the approved plan. Tester
noted absent coverage/pre-commit tooling; no new coverage gate was added. Preserve
inherited optional dispositions in the M1/M4 records for final handoff.

Production sizing remains uncalibrated. No finite private-cgroup load, hard-crossing
bracket, measured reserve inequality or versioned two-size calibration report is
claimed here. Claude support remains deferred to #2471. No production memory setting,
concurrency default, migration, API authority or runtime privilege changed in M5a.
