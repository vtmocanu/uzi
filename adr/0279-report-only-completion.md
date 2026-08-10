# ADR-0279: A run can complete report-only — findings, no merge request

**Status**: Accepted
**Date**: 2026-08-10
**Deciders**: architect, coders, reviewers
**Issue**: GitLab issue [vtmocanu/uzi#279](https://gitlab.example.com/vtmocanu/uzi/-/issues/279)

## Decision (summary)

An issue run now has a first-class success-terminal that opens NO merge request. The lead
declares it by calling `signal_done` with a new issue-runs-only `report_only: true`
(`agent/src/signals.ts`); the worker records the run's findings and completes with no
push and no MR (`agent/src/runner.ts`), mirroring the existing `ci_fix` `not_code`
no-MR terminal. The findings ride a dedicated, server-scrubbed `runs.report_md` column
(migration `00114_run_report_only.sql`), surfaced as a neutral chip + escaped-text
Findings panel on web/CLI. Separately, an UNDECLARED issue run that reaches `signal_done`
with a confirmed-empty diff now FAILS with an actionable reason instead of opening an
empty MR.

## Context

uzi had exactly one success-terminal signal: the lead calling `signal_done`, which made
the worker fetch back the agent branch, push it, and open a merge request. A
verification/evidence issue run that legitimately produced ZERO code changes still ran
that path and opened an empty MR — observed as MR !242 on issue #78, closed by hand.
There was no sanctioned way to say "the deliverable is a report, not a diff," and no
guard distinguishing that intent from the failure mode it resembles — a run that meant to
commit code but committed nothing (forgot to commit, or the fetch-back brought nothing).
Those two zero-diff cases have opposite correct outcomes, so the system needed both a
declared success path and a guard for the undeclared ambiguity.

## The decision

### 1. Extend `signal_done`, not add a new terminal tool

`report_only` is an optional boolean param on the existing `signal_done` tool, schema-gated
to issue runs on the SAME `isIssueRun` discriminator that already gates `prd_done_path` and
`milestones` (`SignalServerOptions.reportOnly` in `agent/src/signals.ts`). `signal_done`
already carries schema-gated optional params by exactly this mechanism, and it is already
the run's single "done" latch (`scanSignals`, main-thread-only). A second terminal tool
would duplicate the stream-scan/latch machinery and create a second done-latch to keep
consistent — pure cost for no new capability. `signal_done` also now CAPTURES the
previously-dropped `summary` (`out.summary` in `scanSignals`); a plain `signal_done` still
scans to exactly `{ done: true }`.

### 2. An undeclared empty diff FAILS — it never auto-converts to report-only

The declared `report_only` flag is the ONLY sanctioned zero-diff success. The worker never
infers report-only from an empty diff: auto-labeling a zero-diff completion as report_only
would launder a real "committed nothing" bug into a green run. So the undeclared path
(`agent/src/runner.ts`, after `fetchAgentBranch`) is issue-kind-only and fails loudly with
a reason that names the remedy ("call signal_done with report_only: true"). The
discrimination is precise: `git.changedFiles` returning `null` means the diff could not be
computed — that keeps the normal push path (fail-open); only a CONFIRMED `[]` fails. A
declared `report_only` has already returned before this guard is reached.

### 3. A dedicated `report_md` column, not the transcript

`signal_done`'s tool_use is filtered out of the persisted run stream (`isSignalToolName`),
and its `summary` was never extracted, so the transcript is not a findings sink. The run
completes with `report_md: <summary>`, persisted to a dedicated column that mirrors the
existing `run_reviews.summary_md` (the judge's findings). `SetRunCompleted` plain-assigns
both new fields (`SetRunCompletedParams` in `api/internal/store/runtime.sql.go`).

### 4. `report_only boolean NOT NULL DEFAULT false`, not a nullable enum

The state is binary — a run either opened an MR or reported. `fix_verdict` is an enum only
because it has 3+ later-stamped states; `report_only` has two, so a boolean with a truthful
default is the smaller model and needs no CHECK or backfill (every existing row reads a
correct "normal completion"). `stop_kind` was deliberately NOT reused: it is stamped only
on stop/fail and carries a CHECK constraint, an unrelated lifecycle.

### 5. `report_md` is untrusted worker/model text, hardened server-side and on render

The agent may have seen repo secrets during the run, so `report_md` is treated exactly like
the judge's `summary_md` ingest (`api/internal/workersvc/report_only.go`): `clampWireReportOnly`
kind-gates the flag (drop-and-warn, never error on the terminal report); `clampWireReportMd`
stores the text ONLY when report_only was accepted, control/format-char stripped, bounded to
`ReviewSummaryMaxBytes` (8KB), then `secretscrub.Scrub`ed — sanitize-then-scrub, so the
scrubber sees whole runes. On render, web (`web/src/pages/RunView.tsx`) and CLI
(`api/cmd/uzi/run.go`) show `report_md` as ESCAPED plain text, never through `<Markdown>` —
the same rule the judge summary follows, because the ingest scrub does not cover
markdown/link injection.

## Consequences

**A verification/evidence issue run terminates cleanly with its findings + transcript and
opens NO merge request.** The empty-MR pathology (#242) cannot recur for a declared
report-only run, and an undeclared empty diff now fails loudly with an actionable reason
instead of opening an empty MR.

**Accepted edge — declared report_only + committed work.** The declared path returns BEFORE
`fetchAgentBranch`, so if a lead declares `report_only` on a run that DID commit code, that
commit is not pushed — it is discarded when the runner clone is torn down. There is no
inverse guard. This mirrors the `not_code` precedent, and the lead prompt frames report_only
as "no code change to land"; documented here, not guarded.

**Checkpoint/resume — now ENFORCED (issue #299), was an accepted edge.** A `report_only`
declared AFTER a mid-run checkpoint would leave the checkpoint refs published but un-landed
(no branch, no MR — see [ADR-0122](0122-checkpoint-push-broker.md)) — orphaned
`refs/uzi-checkpoints/*` on origin. This ADR originally rested that on the convention "a
genuine zero-code run never checkpoints" and left it unguarded. Issue #299 makes the
convention an enforced check at completion time: the `report_only` path in
`agent/src/runner.ts` now FAILS with an actionable reason (mirroring the undeclared-empty-diff
FAIL above) when the run published a checkpoint, so a report-only completion can no longer
orphan one. Detection is the UNION of two worker-side signals — `lastPublishedTip` (a
checkpoint THIS worker confirmably landed mid-run) OR `git.hasCheckpointRef` (origin's
`refs/uzi-checkpoints/<branch>`, mirrored into the worker bare at fetch time, catching a
checkpoint a PRIOR/cross-worker attempt landed). A genuine zero-code run trips neither
(nothing committed ⇒ no pack ⇒ no publish) and still completes report-only. No
checkpoint-ref *deletion* capability was added: the push broker only ever does a non-forced
push, and refusing loudly (converting a silent remote orphan into an actionable terminal) is
proportionate where a delete-ref RPC would be a new worker→api→forge trust-boundary
capability. Deliberately CONSERVATIVE: because `hasCheckpointRef` keys on the branch name and
nothing prunes `refs/uzi-checkpoints/<branch>` when a run's MR lands, the rare
branch-reuse-with-a-lingering-ref case can refuse a genuinely-empty report-only run — but the
ref it names is itself orphaned residue an operator should see, so erring toward surfacing it
is the intended direction.

**`report_md` rides the shared `RunDTO` embed.** `RunListItemDTO` embeds `RunDTO`
(`api/internal/apitypes/run.go`), so `report_only`/`report_md` appear on the runs LIST as
well as the detail view — consistent with `plan_md`/`fix_verdict`, and safe because the text
is already scrubbed at ingest; only the detail run view renders it.

**Do not weaken the undeclared-empty-diff guard when "simplifying."** Making the guard treat
`null` (diff-failure) as empty would fail runs on a transient git error; making it
auto-convert to report_only would re-open the exact laundering this decision rejects; dropping
the issue-kind gate would misfire on run kinds with their own terminal paths. The
null-vs-`[]` split, the fail-loud default, and the kind gate are each load-bearing and pinned
by the m1 empty-diff-guard tests.
