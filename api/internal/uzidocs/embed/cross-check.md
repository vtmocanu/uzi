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

The **Plan cross-check** row has independent model and effort choices for
Claude and Codex. Select **Default · value (source)** to follow that family's
worker default, or choose a hard pin, including a custom model ID. A model
pin does not pin effort, and an effort pin does not pin model. Pins do not
substitute models, clamp effort or retry on a default. The Claude cell is
editable but inactive: **Used once Codex-lead runs are cross-checked**. Today
only a Codex checker checks a Claude lead; Codex leads remain unsupported.

## 2. Wait for the checked plan

Required runs wait for a worker advertising `cross_check_v1`. A **Claude
lead's plan is checked on Codex** in a separate, read-only checker run.
On a worker advertising `cross_check_rounds_v1`, changes requested (REVISE)
go back to the lead automatically within the revision budget. Each revised
candidate gets a fresh check and deadline; an APPROVE of the latest exact
candidate permits implementation after the server acknowledges the matching
plan. Checker findings arrive as fenced, potentially wrong or hostile
advice. The revision prompt tells the lead to verify them against the code,
issue and uzi rules, and to decline conflicting advice.

The default budget allows two automatic revisions (three checked candidates).
Superseded attempts count too, including recovery after worker loss; they do
not spend your human revision allowance. Exhaustion parks with
`plan cross-check: revisions exhausted`. A round-capable worker with a zero
budget exhausts on REVISE; an older worker keeps the changes-requested human
gate. BLOCK, timeout, failed or unavailable checker, refused candidate/diff
and submit failure retain their human fallback or terminal delivery exception.
An opted-in **Codex lead** still parks with
`plan cross-check: not yet supported for a Codex lead`.

Checker runs use a dedicated cross-check lane (one slot by default); the lead
holds its run slot while waiting. A worker supporting both families can check
its own Claude lead on Codex even with a run cap of 1. This does not enable
Codex-lead checking (follow-up #2460) or Code cross-check.

The lead's worker is preferred. Another eligible worker of the same user may
claim the child after `WORKER_AFFINITY_GRACE` (default 2 minutes). A cordoned
worker may still claim its own pinned child; maintenance fences and worker
quarantine remain authoritative. An ephemeral worker may check its bound lead
or the child it was provisioned for, but cannot take another lead's child.
An eligible own lane with space avoids provisioning another pod; a full or
unsupported lane follows the existing capability-gap and saturation policy.

During a mixed-image rollout, older workers with a NULL (unadvertised) slot cap and
no `cross_check_lane_v1` capability can still take plan checks on ordinary run
slots. Explicitly disabling the lane with `WORKER_CROSS_CHECK_SLOTS=0` does
not enable that fallback; incomplete lane-aware negotiation cannot fall back
either. See [worker setup](./worker-setup.md#cross-check-slots)
for capacity and isolation limits. Checker children remain hidden from the
Runs list; worker capacity shows runs and cross-checks separately.

A non-null Codex model or effort pin also requires
`cross_check_pins_v1`; a custom resolved Codex model independently requires
`codex_custom_model_v1`. Unpinned round-1 checks remain eligible on older
`cross_check_v1` workers; later rounds additionally require
`cross_check_rounds_v1` on the same worker. These requirements apply to both
dedicated-lane claims and negotiated legacy run-slot fallback. Missing
capabilities leave the child queued.

The verdict deadline includes queue time. Waiting for the check is
excluded from the lead's wall budget. See [Configuration](./configuration.md#cross-check-settings)
for timeout defaults and bounds. A claim-time snapshot freezes the delivered
model, effort and their independent sources for that attempt. A new round
resolves pins at its own claim time; earlier records remain unchanged.
Local syntax, family, effort and
capability checks precede credential delivery; a capability race requeues.
Recognized authenticated rejection of a pinned model at checker startup fails
the child with `plan cross-check: checker unavailable` and forces a human gate.
Other startup failures follow the existing cross-check failure path.

## 3. Read the evidence and decide

Open the run to see distinct rounds in the feed, the current gate reason,
checked-candidate outcome,
findings, checker-run link, recorded model/effort, each field's source
(`pin` or `worker default`) and reported tokens/cost. Missing legacy sources
stay unknown; later settings changes do not rewrite historical evidence.
Unknown or inconsistent outcomes show **Outcome unavailable**. Findings use
hardened Markdown with a display cap of 16,384 source characters and 20 items;
omitted text is disclosed. Missing cost shows unavailable; subscription usage
is labelled separately from metered spend. After a human revision,
**Earlier-plan evidence** does not certify the current plan. Approve, request
changes or reject the plan shown at the current gate; the original check stays
as history, without an automatic new checker round.

[CLI](./cli.md#plan-cross-check-evidence) shows the reason and findings;
[Slack](./slack.md#using-it) shows the reason without findings.

## Recovery before a human gate

A reclaimed Claude lead can start a fresh round within the same budget when
there is no established human gate or durably approved plan, and the latest
attempt was interrupted while pending strictly before its deadline, decided
REVISE, or APPROVE whose plan was never durably stored (`approved_not_stored`).
Lifecycle-settled failed/superseded attempts need proof of that pre-deadline
interruption. The first custody-invalidating transition supplies the clock;
a later sweep cannot grant recovery. Equality or ambiguous legacy evidence
fails closed to timeout. BLOCK, timeout and other check failures retain their
fallback even if budget is spent. An eligible recovery without budget parks
as exhausted. An established human gate keeps its presentation and revision
context, without adopting checker evidence or starting a fresh check; a
durably approved plan follows the existing resume path.

See the [recovery decision](../adr/2149-cross-check.md#automatic-rounds-extension-2026-10-07-2150)
for the server authority and delivery boundaries.

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
