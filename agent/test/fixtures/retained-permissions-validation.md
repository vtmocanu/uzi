# Retained object permissions: validation record (#3059)

## M1: observed baseline red (2026-10-18)

Production code was unchanged from `1edf88d3b6051cfbf3c0c47ac8bb004b68af67f6`.
The test and inventory revision is the M1 commit containing this record.
No permission-readiness implementation existed during this proof.

The base worker image was built from this checkout's checked-in Dockerfile:
`docker build --progress=plain -f agent/templates/base/Dockerfile -t recovery-3059-base:local .`.
Build exit: 0. Image ID:
`sha256:5e48b49cc6f85d2f2fc7d75c7a5868685b338b2d3614e7f5208e4e50032a3d5f`.
The optional source-version build argument was not set, so baked BUILD_INFO says
`unknown`; source provenance here is the checkout and read-only source mounts,
not a claim that the image advertises the commit. The supervisor and entrypoint
came from the same unmodified production checkout. Image construction and export
were cold costs and are excluded from the proof-step comparison below.

Versioned required-lane command:

```sh
WORKER_UID_IMAGE=recovery-3059-base:local WORKER_UID_SKIP_BUILD=1 bash e2e/worker-uid/run.sh
```

The runtime used the production root-start entrypoint and hardened capability
drop. Fixture assertions established worker UID 10001, runner UID 10002, command
UID 10003, runner group 10002, actual owner-only directories 2700 and files 0400,
worker/runner EACCES and successful command reads, and command denial of both
worker-private and provider-private dummy files. Fresh commits and their unique
blob were absent from worker storage before recovery. Sources were self-contained;
there was no alternate through which a captured tip could mask unreadable source
objects. Resumed capture instead began with an already-valid bare tip and pin.

The six fresh-source positive leaves (loose, packed, and all four mixed
worker/runner hardlink variants) failed at source-read recovery:
`recovery capture: worktree status unreadable (cannot assert clean)`, then bounded
exhaustion without successor adoption. The captured-tip ordering leaf reached
successor adoption using the valid pre-existing bare restore tip, then failed its
`source normalized BEFORE resumed-tip verification` assertion (`false !== true`):
the source was still unreadable at the resumed verification seam. The
command-owned hardlink mutation case also failed because the new
`hardlinked_mutation` refusal did not yet exist.
The corrupted-source preservation case passed. None of the nine leaves skipped,
errored during startup, or timed out. All 38 existing worker leaves and both
existing command leaves passed.

Local retained logs (scratch is not durable across a fresh runner):
- Image build: `.uzi/scratch/gate-log.nN3iM1`, exit 0.
- Existing-lane baseline: `.uzi/scratch/gate-log.dJ3r3f`, exit 0.
- Final M1 required-lane red: `.uzi/scratch/gate-log.4VBy5x`, exit 1.
- Source JUnit inventory/result report: `.uzi/scratch/worker-uid.AurKLU/`.
- Final cheap agent/scoped-project checks: `.uzi/scratch/gate-log.rCVCgc`, exit 0.
- Shell lint, Docker-free harness checks and existing inventory check:
  `.uzi/scratch/gate-log.JBxlgm`, exit 0.

Local step timing with the same image, serial execution and read-only source mounts:

| Step | Before added leaves | M1 expected-red revision |
|---|---:|---:|
| Worker proof | 39 s, exit 0 | 142 s, exit 1 |
| Command proof | 26 s, exit 0 | 31 s, exit 0 |
| Image layout/preflight | 4 s | 3 s |

The worker-proof difference is +103 s on this local expected-red run, not a
green implementation measurement. Command-proof variation is +5 s on unchanged
proofs. No CI queue, rerun or critical-path timing was measured. M3 must record
the final green timing before claiming runtime impact.

Earlier failed attempts were fixture setup failures (unsafe group-writable bare
config, inaccessible shared scratch, and a worker-private fake process table).
They are not counted as the baseline red. The scoped empty process table contains
no provider roots; all real fixture command roots use the production supervisor
and require child exit plus clean disposal. Test-only scratch permissions are
restored. This does not replace production quiescence checks.

Positive fixtures stop only after the real `prepareRecoverySuccessor` has
verified and durably handed off attribution to a fresh successor. They then
inject a test stop before provider/model admission. Assertions require the
ready-for-model journal, retained predecessor, exact tip/tree/blob, closure and
hardlink preservation. Those post-repair assertions remain unproven at M1.
Negative mutation refusal and final non-skipped green are also M2/M3 work.

## M1 review and full-gate triage

Review of `1edf88d3..8b199f4a` identified missing positive assertions for retained
predecessor object bytes/identities, source refs and recovery pins. The following
repair records the original command-owned file snapshots and checks them after
adoption, allowing only group-read mode additions; it also checks source refs,
worktree bytes, directory modes and the exact retained episode pin. The same
repair corrects the resumed-tip baseline evidence above. These additional
post-adoption assertions require the M3 green proof; they are not claimed green
on the baseline.

`task gate:agent` on `8b199f4a` failed: recorded task exit 201, log
`.uzi/scratch/gate-log.kkWiVY`. Static stages passed; unit results were 16,086
passed, one failed, 15 skipped, zero cancelled, in 3,744 seconds. The sole failure
was the existing `wall-during-dispose settles eligible Bash and retires roots
before park/report` case in `codex-executor.test.ts`, reporting unconfirmed
command cleanup. M4 did not run after this unit failure.

The smallest selected test passed on both candidate `8b199f4a` (log
`.uzi/scratch/gate-log.0IuWHw`) and an isolated base `1edf88d3` export using the
same provisioned dependencies and hermetic preload (log
`.uzi/scratch/gate-log.tqvBba`), exit 0 and one test each, no skips. This comparison
is inconclusive: it does not establish that the full-gate failure is unrelated.
Final delivery still requires a successful full agent gate.

## M2: run-local terminal cleanup regression

The human selected a run-only terminal failure for unconfirmed readiness-helper
cleanup, including startup cleanup, and explicitly kept the worker quarantine
policy unchanged. The accepted interruption/persistence limit is recorded in
ADR-1197; there is no new durable pre-launch marker or command-UID scan.

The final regression file was copied into an isolated export of production
`59936bbc1d8e1a36490ce9891e3d72c88a4ad88b`, with the provisioned dependencies
linked unchanged. The primary worktree was not reset or replaced. Command:

```sh
cd agent
node --import tsx --test --test-timeout=120000 \
  --test-name-pattern='runner first readiness result then clean helpers' \
  test/git-retained-readiness.test.ts
```

Baseline log `.uzi/scratch/gate-log.PGfr6B`, exit 1: seven tests,
four failed and three passed, no skips/cancellations. Unclean disposal, throwing
disposal and unconfirmed startup cleanup each reached the injected fault and
failed because two helpers launched instead of one. Cleanup at the deadline
failed because its persisted blocker was `budget_exhausted`. Clean disposal,
verified startup cleanup and ordinary helper-refusal controls all passed.

After the fix, `.uzi/scratch/gate-log.NO5zkw`, exit 0: the entire readiness
file plus `git-recovery-claim-window.test.ts` passed all 92 tests, no skips or
cancellations. Terminal cases assert one helper launch, the parsed persisted
`readiness_cleanup_unconfirmed` blocker, no successor preparation/adoption or
model start, and preserved dirty bytes, source refs, journal attribution, pin and
custody. Positive controls complete the real successor preparation and inspect
its durable ready-for-model handoff before a controlled test stop. Further
focused cases cover cleanup overriding an ordinary refusal and preserving its
classification when an operation aborts during disposal. A persisted cleanup
blocker remains blocked across a later claim and deadline expiry.

These are host runner/fake-supervisor fault tests, not replacements for the
separate real distinct-UID permission and credential proofs. No surviving
process was manufactured or claimed. Final M3 delivery still requires its
complete integration gates and real-UID proof.

### M2 integration-gate fixture repair

`UZI_AGENT_TEST_CONCURRENCY=2 task gate:agent` on `01980dc7` failed,
exit 201, log `.uzi/scratch/gate-log.OLVErZ`: 16,154 passed,
three failed, 15 skipped and zero cancelled; unit duration 4,862 seconds.
M4 did not run. The failures were the retained release-unavailable reclaim
in `codex-executor.test.ts`, retained session-artifact adoption in
`runner-resume-preflight.test.ts`, and late native park ACK fencing in
`runner-worker-memory.test.ts`.

All three failed again in a narrow selection on `01980dc7` (log
`.uzi/scratch/gate-log.dflgqN`, exit 1). The equivalent selected command on
an isolated original-base `1edf88d3` export passed all three without skips
(log `.uzi/scratch/gate-log.3sbrub`, exit 0). Both used the hermetic preload,
skip/leak wrappers and an explicit Node concurrency-two flag. The wrapper's
diagnostic printed its default concurrency estimate because the environment
override was unset; the explicit Node flag was identical in both commands.

These were new readiness-dependency integration regressions, not classified
as unrelated gate failures. Their host-only synthetic-root fixtures now
explicitly satisfy readiness and assert its single call before successor
execution. The resume wrapper forwards the real executor's sandbox profile;
the memory successor fake declares the profile it models. Production guards,
timeouts and existing adoption/session/custody/fencing assertions are unchanged.
The separate real-UID proof has no injected launcher or readiness stub.

The repaired narrow selection passed all three, no skips, in 19 seconds
(log `.uzi/scratch/gate-log.mJDdXy`, exit 0). This run also sets the environment
concurrency override to two, so its wrapper diagnostic agrees with the Node flag.
A full gate repeat is still required after the scoped repair review.

The real-UID lane on `01980dc7` separately passed: log
`.uzi/scratch/gate-log.NV9cSL`, exit 0, report
`.uzi/scratch/worker-uid.88EBP0/results.xml`. All nine retained-permission
leaves passed without skips, failures or errors, plus all existing required
worker/command leaves. Image identity matched the original ID above.
Worker proof: 172 seconds; command proof: 19 seconds. These are local source-tree
timings, not measured CI critical-path results.

The full agent-gate repeat on `a1b58d6f` passed, log
`.uzi/scratch/gate-log.lC0dFB`, exit 0: 16,157 unit tests passed,
15 skipped, zero failed/cancelled (4,935 seconds), followed by all 183 M4 tests
passing without skips (105 seconds). The required claude/U, codex/U, codex/P
matrix was satisfied. Reviewer, auditor and read-only tester reviewed both the
run-only cleanup repair and its fixture integration repair without remaining
findings. M2 was checkpointed at that revision.

## M1 fixture correction: negative-source preservation and integrity proof

The test-only correction range starts at `a1b58d6f` and ends at the commit
containing this section. Both negative branches previously returned before
checking the saved source refs and every private object snapshot. They now
compare all original private objects and directories, preserving hashes,
device/inode identity, ownership, links and directory entries. Each mode must
be unchanged or have exactly the specified file group-read / directory
group-read-and-traverse addition. Both names of the multiply-linked mutation
candidate still require exact unchanged snapshots. Existing HEAD, worktree,
bare-ref, outside-content, custody and positive-recovery assertions remain.

The corrupt case now observes the real normalization, status and fetch methods,
calling their originals. It requires successful readiness and clean status
before each actual fetch error and a diagnostic identifying the deliberately
corrupt blob. An index assume-unchanged hint on the committed corrupt-case file
lets this scenario reach fetch integrity instead of a status re-read. This is
post-fix negative coverage, not permission-specific baseline-red evidence.

The unmutated corrected fixture passed the same-image UID lane, log
`.uzi/scratch/gate-log.EpQpGs`, exit 0: all nine retained leaves and all
47 worker / two command required leaves passed; worker proof 180 seconds,
command proof 18 seconds. Report: `.uzi/scratch/worker-uid.kSIKLw/results.xml`.
Static/lane type checks passed in `.uzi/scratch/gate-log.ea49jA`.

Sensitivity probes ran against disposable exports of unchanged production
`a1b58d6f`, comparing its old fixture with the corrected fixture. Image ID was
the original ID above in every invocation. No production file was mutated.

| Probe | Old assertions | Corrected assertions |
|---|---|---|
| Add a source tag after linked refusal; append bytes to an unrelated private tree object after corrupt refusal | exit 0, all nine retained leaves pass (`gate-log.EVE06i`) | exit 1, exactly the two targeted leaves fail: exact source-ref preservation and original private-object hash (`gate-log.juV61L`) |
| Substitute a generic fetch refusal, with a positively reached fault counter | exit 0, all nine retained leaves pass (`gate-log.CGTYJN`) | exit 1, only corrupt-source leaf fails: missing actual fetch-integrity evidence (`gate-log.Mz91Lx`) |

The generic-refusal comparison gives the old fixture the same index hint so
both variants positively reach the substituted fetch. It does not claim an
integrity failure. All comparisons had zero skipped or startup-error leaves;
the corrected unmutated fixture remained green. Probe code and reports live
under `.uzi/scratch/snap-m3-negative-*`; the four gate logs above are in
`.uzi/scratch/`. Initial probe attempts without a dependency link failed
inventory startup (MODULE_NOT_FOUND) and are retained but excluded from proof:
`gate-log.jvTJDl`, `gate-log.260P6e`, `gate-log.w4hGVE`,
`gate-log.wDuYdy`. Linking provisioned dependencies required no installation.

The original permission-specific same-image red evidence and all earlier
failed gates remain recorded above. No production, quarantine-policy,
workflow, assertion/deadline relaxation, or source cleanup was part of this
test correction.

## M3: bounded retained diagnostics

The diagnostic writer is `runGit` and its only metadata reader is
`formatRetainedRecoveryDiagnostic`. Metadata is held in a private WeakMap,
without raw enumerable error fields or a durable record; the public thrown
message remains compatible. The formatter bounds unsplit input to 64 KiB
before redacting it whole, normalizes line separators for split-token
redaction, sanitizes controls/bidi, and preserves bounded prefix/suffix cause
text. Final cause/context limits are 1024/256 characters including markers;
the warning and all free text fit 1536. Readiness failures produce fixed codes.

The runner emits the structured warning after lifecycle/control checks and
before deadline, third-attempt, cleanup or other terminal branches. Persisted
blockers and terminal reasons contain no transport text. Reservation,
renewal-ownership and blocker-persistence diagnostics use the same formatter.

Focused new tests passed: `.uzi/scratch/gate-log.jsace8`, exit 0, 14 tests,
zero skips/cancellations. They cover original-message compatibility, metadata
privacy, long argv/stderr paths with visible Permission denied suffix, all
field/aggregate bounds, UTF-8 byte overflow and whole-input omission, split
tokens across newline/separators/controls, bidi/astral handling, and actual
retained entry's final/third/deadline/cleanup reporting before terminal state.
Dummy provider-shaped token values are assembled from source fragments.
Selected existing readiness/control-precedence cases also passed:
`.uzi/scratch/gate-log.YYBLTG`, exit 0, 20 tests, no skips/cancellations.

Sensitivity: restoring the old warning position after terminal branches made
all four runner regression cases fail in
`.uzi/scratch/gate-log.b_60wrof`, exit 1. Third-attempt and generic cases
logged two warnings instead of three; deadline and cleanup cases logged none
instead of one. Correct placement was restored before subsequent checks.

Lead integration checks passed: `.uzi/scratch/gate-log.eEL82f`, exit 0,
226 focused diagnostic/readiness/retained-lifecycle tests, no skips or
cancellations. Static agent/lane types, lint, dead code, docs and fragment
validation passed in `.uzi/scratch/gate-log.ixEh5L`. The E2E library, forge
fake and Docker-free worker-UID harness checks passed in
`.uzi/scratch/gate-log.K2usyQ`, exit 0.

The changed retained warning/blocker identifiers were swept across
`e2e/phases/`, `e2e/lib.sh`, `e2e/lib.test.sh` and `e2e/forge-fake/`;
no owned stale literal expectations were found. Existing same-claim capture,
fresh-attempt and custody expectations remain applicable. Live compose phases
were not run; the retained/custody paths include phases 45, 52 and 72.

Focused evidence does not substitute for the final gates or same-image UID
proof. Delivery reports those results, tested revisions, and local step
timing separately; CI critical-path impact remains unmeasured until CI runs.

## Codex lost-refresh E2E fixture precondition

CI on validation PR #3126 (exact head `c2d98f1c`, run 39197954995) was red
only in `test-codex-refresh-lostreply-e2e`: all four variants stopped at the
resumed-attempt check. A local Linux single-target run of
`TestCodexRefreshLostReplyE2E` reproduced 4/4 failures on unmodified `c2d98f1c`;
a fixed-code diagnostic showed `retained_readiness_profile` three times, then
`budget_exhausted`. The fixture's test-double executor did not declare
`sandboxesCommands`, while its claim carries Codex secrets, so the runner gate
correctly refused retained recovery. Production Codex executors always declare
it; the gate is unchanged.

The fixture now declares `sandboxesCommands: true` and replaces only its own
GitCache readiness method with a recorder. This host has no worker/command UID
split, so the real helper cannot launch there; the real-UID lane remains the
permission proof (49 required leaves and both PSI calibrations passed in that
CI run). The fixture asserts no readiness call before the reclaim, then exactly
one call bound to the fixture's bare path, branch, run and predecessor clone,
made before the resumed executor ran. A failed resume now reports only known
fixed codes. All other fixture assertions and deadlines are unchanged.

Local evidence (Linux container, throwaway Postgres, caps `-p=2`,
`GOMAXPROCS=4`, concurrency 2, `go test -race -count=1`): the corrected fixture
passed 4/4 twice. Each probe failed 4/4: no `sandboxesCommands`
(`retained_readiness_profile` x3, `budget_exhausted`), zero recorded calls, two
calls, and readiness moved after the resumed executor started (runner call
removed, executor call added). Agent typecheck and lint passed, and
`TestStaleWorkerCancelE2E`, which CI does not run, passed 4/4.
