# Issue #2397: partial worker-UID investigation

Cause diagnosis and fix are **not completed**. No flake fix yet; M2/M3 remain
incomplete. There is no local reproduction or cause regression test.

## Explicit human decision

On 2026-10-07, after the evidence question, the human supplied this decision:

> The detail you asked for does not exist anywhere retrievable.
>
> Finish M1 (the diagnostics that make the lane print the failing assertion or cancellation reason), commit it, and signal completion. The PR's own CI run of test-agent-worker-uid is the evidence lane from now on.
>
> Do not start M3 and do not claim a cause or fix. The PR must use "Refs #2397", not "Closes".

Milestone disposition under that decision:

- **M1 — diagnostics: completed.** This delivery is diagnostics-only; the quoted
  instruction records the human decision, not proof of a cause or fix.
- **M2 — investigation: partial/deferred; original M2 unmet.**
- **M3 — verified-fix: not started, deferred by explicit instruction; M3 unmet.**

The local environment reached the 900s image-build timeout before worker-UID
integration tests ran, as recorded below. Per the human/lander check, historical
JUnit existed only in runner `/tmp`, not in retrievable artifacts.

For #2390, the lander reported `[test:agent:worker-uid] context canceled` after wrapper
unit tests and before entrypoint output. This observation was absent in the
passing comparison and the #2377 failure; it does not establish a cause.

#2397 stays open. The PR uses `Refs #2397`; diagnosis and fix are deferred until
enhanced CI supplies a diagnostic and a controlled reproducer exists.

## Observed local attempt

At diagnostic-only `752f4c85` atop `d0b98093`:

- `docker info`: Docker 29.8.0, exit 0; no cached `wuid-2134-base:local` image.
- `npm --prefix agent ci --ignore-scripts`: exit 0.
- `timeout --kill-after=30s 900s task test:agent:worker-uid`: exit 124.
  Docker-free tests passed (11 checker, 7 wrapper, 1 truth-table); the inventory
  parser yielded 29 required leaves, including 7 production advice teardown leaves.
  The image build reached runtime step 3/31 (`apk add`) and stopped at the outer
  deadline. No completed image identity is available; **zero real worker-UID
  executions** occurred. The lane did not pass.

The supplied record cites `.uzi/scratch/gate-log.39IqrJ`, lines 56–62. Scratch is
ephemeral; the commands and results above do not depend on its persistence.

## CI evidence and supplied reports

These historical assertions were supplied, not locally reproduced:

- #2377: run `37542424541`, job `112538741355`, reportedly failed the symlink
  case; an unchanged-head rerun at `224890fc` passed. This supports classifying
  #2377 as intermittent; its underlying cause remains unresolved.
- #2390: run `37568169160`, job `112620705472`, failed at `2cb00271`;
  run `37569194788` passed at different head `62749048`. There is no unchanged-head
  proof. A shared cause versus a separate regression remains unresolved.
- A new human issue comment reports main run `37575030278`, job `112641997912`,
  at `876e3b5f` failing the single-UID case generically. This expands reported
  scope, without establishing a root cause.

Granted `get_pipeline_jobs(37542424541)` currently returns successful rerun job
`112543141140`, without the old assertion. Granted jobs for #2390 confirm failure
but supply no diagnostics. Original heads `224890fc`, `2cb00271` and `62749048`
are absent locally; merged commits are not controls at those exact heads.

## Source findings and unverified lead

This is a starting point, not an exhaustive production dependency enumeration.
In `agent/test/codex-executor.test.ts`, owner/symlink refusal tests at 6113/6133
and single-UID at 6191 use `runnerTeardownFixture`. The `makeFixture` import at 48
has calls at 946/1131 in unrelated decoded-usage-limits and approved-policy-flow
suites (940/1127). `pattern.mjs:6` anchors full inventory names, which exclude
those suites.

`agent/test/fixture-repo.ts` contains Node builtin imports and declarations, with
no import-time execution of `makeFixture`. That function (102) copies the local
environment (114) and filters `GIT_` keys (116–117). Merged #2390 helper diff
`e74ae641` changes that function body. No direct link to the selected fixtures
is established; this does **not** prove independence or absence of regression.

In `agent/src/codex/codex-executor.ts:1123`, disposal checks `outcome.clean`;
`rmRunnerTeardownTree` runs at 1129–1130 after confirmed clean disposal, otherwise
1134 warns that data is retained. Both refusal tests assert the cleanup-failed
warning. Setup, assertion or finalization failures can produce different failures.
This gate is an unverified investigation lead, not evidence of a timeout, order
or race cause. Preserve forensic retention.

## Next evidence

Accepted lander guidance is to use PR CI `test-agent-worker-uid` with enhanced
reporting. Capture the failing diagnostic, originating file, stack, type and
status, immutable head, image identity, runtime and inventory; then establish a
controlled reproducer before a behavioral fix. Repeated passes are insufficient.
