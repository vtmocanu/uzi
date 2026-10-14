# Issue #2971 investigation checkpoint

Refs #2971. Investigation in progress; the issue remains open. This document
records observed evidence, not authority to retry, release custody or delete a
source. M1 characterization is recorded below; M2 audit and M3 decision
comparison are pending. No consent implementation or production salvage is
authorized by this investigation.

## M1: permanent-block reproduction

Observed 2026-10-14 UTC against tree baseline
`150e442c43cf1f64bdcd3e24231ee2295a8a1f38` (starting HEAD
`a86fb127` has an identical tree). Added characterization:
`agent/test/runner-retained-episode.test.ts`,
“issue 2971 unique-content exhaustion stays blocked after a larger-cap restart”.

Command, from repository root:

```sh
(cd agent && node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-timeout=120000 --test-name-pattern='issue 2971 unique-content' test/runner-retained-episode.test.ts)
```

First focused run exited 0, one test passed, no failures/cancellations/skips.
Scratch log: `.uzi/scratch/gate-log.RHk6w5` (session-local, not published).
The fixture uses real Git, a real retained clone and unique 64 KiB public
content, plus the untracked literal `only local dirty work\n`. The initial
runner exhausted on its first closure check; repeated-read accounting is not
needed. The unique captured closure measured 66,492 decoded bytes.

| Observation | Initial execution | Recreated cache and runner |
| --- | --- | --- |
| Configured cap | 4,096 bytes | 1,048,576 bytes |
| Runner closure calls | 1 | 0 |
| Real model calls | 0 | 0 |
| Successor preparations | 0 | 0 |
| Custody release calls | 0 | 0 |
| Journal | attempts 1, backoff 1 ms, blocked / decoded_history_limit | byte-for-byte unchanged |
| Source and pre-existing episode pin | retained | retained |
| Dirty-work literal | preserved | preserved |

The initial exception was an actual `RecoveryClosureLimitError` with
`limit === 4096`, message:

> reachable recovery history exceeds the 4096 bytes decoded verification limit

The exact initial terminal report was:

> Retained recovery blocked: reachable recovery history exceeds the 4096 bytes decoded verification limit; local work and custody retained

The original journal timestamps were `startedAt=1791980473960`
(2026-10-14T12:21:13.960Z) and `deadline=1791980773960`
(2026-10-14T12:26:13.960Z). The test asserts the deadline remains in the
future at the initial block, equals startedAt + 300000, and that attempts,
backoff, source/attempt attribution and the entire journal survive restart
unchanged. It pre-establishes a real source-tip episode ref to check pin
preservation; this is not a claim that the failed capture created a restore
pin. The blocked episode has no restoreTip.

An independent fresh local closure verification under the larger cap succeeded
before the restarted runner was invoked. That proof does not authorize
execution. The restarted runner still reported exactly:

> Retained recovery blocked: previous decoded verification exhaustion; current cap 1048576 bytes; prior cap not recorded; local work and custody retained

The test is passing characterization of the permanent block. It is not an
observed recovery-authorization fix or the eventual recovery-positive red/green.
The fake API supplies positively fenced generation 2 ownership for this local
fixture. It does not exercise the server lifecycle of a terminal incident run,
a live worker, forge upload, owner consent or production custody.

### Code evidence and related work

Starting points verified in the baseline, not an exhaustive reader inventory:

- `agent/src/runner.ts:17269`:
  `if (source.recovery?.stage === "blocked") await fail();`
- `agent/src/git.ts:5900`:
  `if (old?.stage === "blocked") throw new Error("recovery episode is blocked");`
- `agent/src/git.ts:6048`:
  `if (journal.recovery?.stage === "blocked") return;`
- `agent/src/runner.ts:17396`:
  `if (error instanceof RecoveryClosureLimitError) { blocker = "decoded_history_limit"; await fail(); }`
- `adr/1197-transient-recovery-park.md:308–312` says an already blocked
  decoded-history journal stays blocked across restart/cap increase and reports
  “prior cap not recorded”.

PR #2976 and PR #2981 were both read through the authorized forge reader on
2026-10-14, approximately 12:18 UTC, and returned `state: opened`. The grant
returned no diff, head SHA or merge evidence. Checked-out
`GitCache.reserveRecoveryIteration` still uses the original startedAt +
300000 deadline and refuses blocked records; the existing cumulative
repeated-read test remains. The investigation preserves that code and
implements neither external change. Future resumes must re-read related status
and checked-out code before relying on these observations.

Recorded decisions read at `specs/human.md:877,886,1004,1303` retain custody,
require exact disposal authority and qualify checkpoint coverage. No conflict
was found with this characterization; this is not an exhaustive compatibility
claim. In particular, line 1004 states:

> The server recorded checkpoint <tip> before this hold. Current availability and the latest local edits are not verified.

## Bounded incident and surviving-source ledger

Observation date: 2026-10-14 UTC. Read issue #2971's captured description and
fresh comments, issue #2958 (including its correction comment), and issue
#2979. These returned no truncation/withheld/unknown-comment flags. All incident
descriptions remain reports; this run independently inspected **no production
journal**.

A bounded forge search requested opened issues with label `area::recovery`
updated after `2026-10-11T00:00:00Z` through the observation at approximately
12:22 UTC on October 14. It returned 31 issue summaries with `truncated:
false`; this is coverage of that query, not all runs/incidents. Full incident
content inspected is limited to the three named issues above. Closed,
unlabelled and older reports, unavailable operator evidence and worker-local
journals are outside that coverage. A tracked token search for the supplied run
prefixes found no matching incident journals (the unrelated numeric matches
are not evidence). A filename search for incident/operator/journal artifacts
likewise supplied no incident journal.

| Reported run / issue | Classification and blocking cap evidence | Safe worker / generation / attempt evidence | Source, pin, hold and checkpoint evidence |
| --- | --- | --- | --- |
| 21a4d826 / #2971 | Reported old 1 GiB exhaustion; failed Oct 11 08:58Z. No durable cap field inspected. | Worker/incarnation, generation, attempt unknown. | Holds reportedly discarded 09:13Z. Not counted as surviving. Source/pin existence, checkpoint availability and latest-edit coverage unverified. |
| 38b52617 / #2971 | Reported old-limit failure Oct 12 07:29Z; no journal or recorded cap inspected. | Worker/incarnation, generation, attempt unknown. | Issue groups it with manually discarded holds; exact discard time unavailable. Not counted as surviving. Source/pin/checkpoint availability and coverage unverified. |
| 1ab9349c / #2979 | Reported **fresh** 8 GiB exhaustion Oct 14 10:19:43Z, attempts 1, start 10:18:21Z, decoded_history_limit. Not evidence of an old-limit block. | Codex worker rc.14 reported; exact worker/incarnation, source/execution generations and attempt ID unknown. | Checkpoint prefix 3fcd6a21 reported on origin; no live availability/coverage verified. Current source/pin/hold status unknown. |
| 0d5a0f7c / #2958 | Reported expired episode clock, budget_exhausted, attempts 1; start 07:31:17.572Z, deadline 07:36:17.572Z Oct 14. Decoded cap not applicable to reported cause. | Codex ephemeral rc.14 reported; exact identities unknown. | Three holds reportedly left; current status/source/pins unknown. Later capture is reported but preservation not independently established. |
| 84ed3050 / #2958 | Reported expired clock, budget_exhausted, attempts 1; start 01:57:47.572Z, deadline 02:02:47.572Z Oct 14. | Claude persistent rc.11 reported; exact identities unknown. | Current hold/source/pin/checkpoint availability and latest-edit coverage unknown. |
| 63ea43d9 / #2958 comment | Reported stale deadline on new claim after OOM, failed Oct 14 10:56:06Z; start 10:46:41Z, deadline 10:51:41Z. Not old-cap exhaustion. | Codex ephemeral rc.14 reported; new claim asserted, exact fence/attempt unknown. | Comment asserts retained checkpoint/custody and “No work was lost”; this run has not verified complete latest-work preservation. |
| Local issue-2971 fixture | Independently reproduced 4096-byte exhaustion; recreated runner at 1048576 bytes remains blocked. | Synthetic claim generation 2, same retained run/source attribution asserted; no live worker. | Source, literal, existing pin and journal retained within isolated fixture; no production hold or checkpoint inference. |

The **current live configured cap is unknown for every incident**. The code
default is 8 GiB; it cannot establish an individual worker's configuration.
Reported historical limits are not durable recorded-cap evidence.

Current frequency and surviving-source status are **unverified**. There is no
eligible-run denominator, fleet inventory, complete incident interval, live
worker read-only grant, authenticated exact-source descriptor/pin inventory,
current owner hold listing or independently verified checkpoint byte coverage.
Two historical reports do not establish a rate, and discarded holds do not
establish surviving sources. Required next evidence is a dated read-only run
population for the same interval, exact worker/incarnation and source/execution
generation/attempt bindings, original journals and current cap configuration,
current source/pin/hold availability, and independent checkpoint availability
with a comparison against dirty/untracked latest bytes. No credential request,
live mutation, hold discard, limit increase or redispatch was performed.

## Validation status

Focused characterization passed as recorded above. Coder and researcher
delegations returned transport failures, not review results. Reviewer, tester
and auditor independently ran the focused characterization successfully on
`70bb9a4af0c26438bb819bb67c3849942c14241a`. Each reported the same
restart-successor-counter coverage gap; the repair instruments the recreated
cache separately. Reviewer and auditor then reported clean scoped static
repair review at `0b3afc33253b73fd77390cf7cdc19ba4eada9936`; tester's first
repair dispatch was cancelled with no result, then its retry independently
passed the named test on that SHA (1 pass, no failures/cancellations/skips).
The lead's repaired focused run also passed, scratch log
`.uzi/scratch/gate-log.jmw6oZ`; the tester retained
`.uzi/scratch/gate-log.jDRWUA`. No deferred review defect remains.

### Blocking broad-validation result

After every validator returned, the candidate was frozen at
`0b3afc33253b73fd77390cf7cdc19ba4eada9936` and the lead ran
`task gate:agent` alone in the foreground with a unique scratch log.
The runtime stopped it at its **60-minute deadline** and reaped its process
tree on 2026-10-14, approximately 13:49 UTC. It is **not a pass**.
The shell wrapper was interrupted before writing its numeric exit status;
status is runtime deadline termination, numeric exit unavailable. Log:
`.uzi/scratch/gate-log.2bvT9T` (session-local, not published).

The log reached deps-check, lint, dead code, typecheck and the unit suite.
Lint reported zero errors/warnings; no preceding task failure was reported.
The unit suite had not finished; no final unit summary or Codex M4 completion
was reached, and the new characterization was not shown in this partial log.
Its independent focused passes remain the evidence for that test.
Two named failures appeared:

- `runner-acknowledged-final-restart.test.ts`: “new report proof transport
  failure preserves existing physical archive authority within the same budget”.
- `runner-plan-cross-check.test.ts`: “forces the supported human disposition
  on submit checker_unavailable”.

There was no broad-suite final diagnostic block before termination. To avoid
attributing either failure to baseline without evidence, the lead selected the
two named tests on candidate and base, then the two whole files on candidate
and base. Commands used the same wrappers, Node imports, timeout and
concurrency, with matching-lockfile installed dependencies in the immutable
base scratch export. No Git-dependent gate ran inside an export.

From the respective `agent` directory:

```sh
../scripts/real-procfs-skip-check.sh ../scripts/tmpdir-leak-guard.sh node --import tsx --import ./test/setup/hermetic-proc.ts --test --test-concurrency=3 --test-timeout=120000 --test-name-pattern='new report proof transport failure|forces the supported human disposition on submit checker_unavailable' test/runner-acknowledged-final-restart.test.ts test/runner-plan-cross-check.test.ts
```

The whole-file command was identical with `--test-name-pattern` omitted.

| Selection / revision | Recorded outcome | Session-local scratch log |
| --- | --- | --- |
| Named two / candidate 0b3afc33 | exit 0, 2 passed, zero failed/cancelled/skipped | gate-log.zOgDZR |
| Named two / base a86fb127 | exit 0, 2 passed, zero failed/cancelled/skipped | gate-log.5cAmOH |
| Whole two files / candidate 0b3afc33 | exit 0, 105 passed, zero failed/cancelled/skipped | gate-log.i5i7GD |
| Whole two files / base a86fb127 | exit 1, 104 passed, 1 failed, zero cancelled/skipped | gate-log.gIO2Cu |

The base export was resolved to
`a86fb127d4cea4dfa8ea42b8f7f4331b3b1f9436` and has the original baseline
tree. Base failure reproduces the report-retirement assertion at line 866:
actual `{ retired: false }`, expected
`{ retired: true, authority: "existing_archive" }`. This is an observed
baseline test instability; its production root cause is not established.
It was reported as an off-task finding without a code fix.
The checker-unavailable failure passed all narrowed comparisons and remains
**inconclusive**, not attributed to baseline. Neither result converts the
stopped broad gate to green.

`task gate:repo` remains **unrun** because the required first gate did not
pass. No Go changes, gate:api, live E2E, LiveDB or production inspection occurred.
No full gate was repeated on an unchanged tree to retrieve output. Broad
validation still blocks M1 completion and any shipping claim. A successful
required gate execution is needed before M1 can be declared complete; no
baseline waiver or alternate gate equivalence is claimed.

M1 remains in progress with committed passing characterization and evidence.
M2 cap audit and M3 salvage/design checkpoint are not yet performed.

The eventual acceptance remains outstanding: owner-authorized recovery-positive
red/green, blocker/current-cap controls, one pass per consent, every custody and
integrity check, actual blocking-cap storage with explicit legacy unknown,
episode-policy ADR amendment if semantics change, and required serial gates.
