---
title: Cross-check
order: 86
audience: user
---

# Cross-check

Cross-check asks the other model family for a second opinion on a run.
**Plan cross-check** is a required gate on opted-in auto-approved plans.
**Code cross-check** is a planned, separate advisory stage before publication;
it is not available in this release.

## 1. Enable Plan cross-check

In **Settings → Run defaults → Cross-check**, enable **Plan cross-check**.
It is off by default and requires usable Claude and Codex credentials. The
setting applies to new eligible auto-approved runs, not existing runs or
human-gated plans. It covers issue, prompt, self-improvement, CI fix and merge
request rework plans that reach the auto-approval gate. Gateless tasks and
plans supplied at creation are outside this gate. Turning it off affects
future runs only.

## 2. Wait for the checked plan

Required runs wait for a worker advertising `cross_check_v1`. A **Claude
lead's plan is checked on Codex** in a separate, read-only checker run.
An APPROVE of the exact candidate permits implementation after the server
acknowledges the matching plan. Changes requested, blocked, timeout or a
failed check normally force a human plan gate. An unavailable checker,
interrupted attempt, refused candidate/planning diff or submit failure also
requires a human decision. An opted-in **Codex lead** parks with
`plan cross-check: not yet supported for a Codex lead`.

Checker runs use ordinary worker slots; the lead holds its slot while
waiting. The verdict deadline includes queue time. Waiting for the check is
excluded from the lead's wall budget. See [Configuration](./configuration.md#cross-check-settings)
for timeout defaults and bounds.

## 3. Read the evidence and decide

Open the run to see the current gate reason, checked-candidate outcome,
findings, checker-run link, recorded model/effort and reported tokens/cost.
Unknown or inconsistent outcomes show **Outcome unavailable**. Findings use
hardened Markdown with a display cap of 16,384 source characters and 20 items;
omitted text is disclosed. Missing cost shows unavailable; subscription usage
is labelled separately from metered spend. After a human revision,
**Earlier-plan evidence** does not certify the current plan. Approve, request
changes or reject the plan shown at the current gate; the original check stays
as history, without an automatic new checker round.

[CLI](./cli.md#plan-cross-check-evidence) shows the reason and findings;
[Slack](./slack.md#using-it) shows the reason without findings.

## Planning-diff refusals

When the worker cannot safely capture the planning diff it refuses, and the
run parks at a human gate with reason `planning_diff_refused`. The refusal
sub-code is shown with the gate reason, and only while that reason is
current: the run page shows `Refusal: <label>`, `uzi run get` appends the
sub-code with underscores shown as spaces (for example
`(unsupported entry)`) to the `PLAN_CROSS_CHECK` row, and the Slack gate
message carries it the same way. The feed line
(`plan cross-check: planning diff refused (<refusal>: <diagnostic>)`) and
the worker log's `refusal` and `diagnostic` fields use a fixed vocabulary;
raw helper stderr and filenames are never logged or shown.

| Sub-code | Meaning |
|----------|---------|
| `base_unavailable` | The base commit to diff against was not available. |
| `diff_failed` | Computing the diff failed. |
| `diff_too_large` | The diff or a capture budget exceeded its cap. |
| `too_many_untracked` | More untracked files than the capture allows. |
| `secret_detected` | The secret scan flagged the diff. |
| `scan_failed` | The secret scan could not complete. |
| `unsupported_entry` | The worktree, index or base holds an entry the capture does not support: a changed or untracked symlink, any submodule, a special file (FIFO, socket, device), or an unsupported file mode. |

An **unchanged tracked symlink** does not block the check: its raw link
target is compared with the base blob and is never opened or followed.
Any other symlink case (added, removed, retargeted, file to link or link to
file, or an untracked non-ignored symlink) refuses with `unsupported_entry`.
A symlinked `.gitignore` is treated as absent, as Git does. **Any submodule
gitlink also refuses** with `unsupported_entry`, changed or not. Submodule
support is deferred: equal base and index gitlink ids do not prove that
`git add -A` publishes nothing, because a moved submodule HEAD or a removed
directory is staged. Supporting it needs a proof that the worktree preserves
the base gitlink under publication.

Capture runs under two independent 128 MiB budgets: one for source reads
(worktree compare reads and verified object reads) and one for the
object-store snapshot copy, which is streamed in small chunks so cleanup can
run on SIGTERM. The snapshot is a temporary tree on the worker's disk,
removed when capture ends. Measured once on a clone of the uzi repository
(October 2026), capture used about a 74 MiB snapshot (the temporary tree
peaks near that size) and about 100 MiB of source reads, in under 7 s
against a 28 s deadline; a repository whose tracked text approaches
128 MiB will refuse with `diff_too_large`.

### Rollout

The refusal sub-code needs two phases, and nothing orders them
automatically. Deploy the api and its migration first and confirm it is
ready, then let the worker pin advance (keep `workers.image.tag` at the
previous release for the first deploy if the chart would move both). An older api rejects
`unsupported_entry` from a newer worker. The chart's `Recreate` strategy
orders pods within one Deployment only; it does not order the api against
the controller or the worker pin.

## Terminal delivery failures

Two delivery-loss exceptions fail the run instead of parking:
`plan cross-check: preparation receipts irrecoverably lost` and
`plan cross-check: human-presentation ACK unrecoverable`. After three
preparation attempts and an acknowledged forced human gate, unresolved ACKs
also fail with `plan cross-check: preparation ACKs unrecoverable`. These paths
do not retry indefinitely, enter `recovery_wait`, invent receipts or approve.
See the [architecture](../ARCHITECTURE.md#plan-cross-check) for the boundary.
