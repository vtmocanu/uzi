---
title: Cross-check
order: 86
audience: user
---

# Cross-check

Cross-check asks the other model family for a second opinion on a run.
**Plan cross-check** is a required gate on opted-in auto-approved plans: a
Claude lead is checked on Codex and a Codex lead on Claude, never on its own
family.
**Code cross-check** is a separate, opt-in advisory stage before publication:
the other family reads the committed code and its findings go into the merge
request, but it never blocks publication (see
[Code cross-check](#code-cross-check)).

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
substitute models, clamp effort or retry on a default. The Claude cell
configures the Claude checker that checks a Codex lead; at Default it follows
your Claude worker default.

## 2. Wait for the checked plan

Required runs wait for a worker advertising `cross_check_v1`. A **Claude
lead's plan is checked on Codex** and a **Codex lead's plan on Claude**, in a
separate, read-only checker run. The checker is always the opposite family of
the lead; it never falls back to the lead's own family.
On a worker advertising `cross_check_rounds_v1`, changes requested (REVISE)
go back to the lead automatically within the revision budget. Each revised
candidate gets a fresh check and deadline; an APPROVE of the latest exact
candidate permits implementation after the server acknowledges the matching
plan. Checker findings arrive as fenced, potentially wrong or hostile
advice. The revision prompt tells the lead to verify them against the code,
issue and uzi rules, and to decline conflicting advice.

The default budget allows four automatic revisions (five checked candidates),
with a configurable integer range of 0–4. Explicit choices remain authoritative;
existing sequences keep their immutable first-candidate enabled/limit snapshots,
so configuration changes affect new checks only. Older `.env` files explicitly
setting `PLAN_CROSS_CHECK_MAX_REVISIONS=2` keep 2 after upgrading until the
operator removes the line or sets it to 4; only installations without the line
take the new default. Superseded attempts count across generations too,
including recovery after worker loss; they do
not spend your human revision allowance. Exhaustion parks with
`plan cross-check: revisions exhausted`. A round-capable worker with a zero
budget exhausts on REVISE; an older worker keeps the changes-requested human
gate. BLOCK, timeout, failed or unavailable checker, refused candidate/diff
and submit failure retain their human fallback or terminal delivery exception.
A Codex lead consumes automatic REVISE rounds without spending its human
revision allowance, and implements from the approved canonical checked plan,
which remains authoritative. An approval may also carry non-gating advice
(see [Coverage advice](#coverage-advice)). Its worker must advertise
`cross_check_codex_lead_v1` to submit for a check. A Codex lead on a worker
without it, or on an api that does not signal support
(see [rolling out](#rolling-out-codex-lead-checks)), keeps the older park, with
`plan cross-check: not yet supported for a Codex lead`, and waits for a human.

Checker runs use a dedicated cross-check lane (one slot by default); the lead
holds its run slot while waiting. A worker supporting both families can check
its own Claude lead on Codex even with a run cap of 1. A Claude checker (for a Codex lead) additionally requires
`cross_check_codex_lead_v1` on the worker that claims it; without one it stays
queued. Code cross-check has its own, stricter placement
(see [Code cross-check](#code-cross-check)).

Plan and Code checker children (`kind = 'cross_check'`) in both harness families
are hold-free and exempt from the owner's recovery custody admission limit.
This is separate from the bounded continuation exemption for code-publishing
runs; judges and jobs gain no exemption. Owner identity, current parent,
generation and round, deadline, protocol, vault, credential, checker-slot and
worker eligibility requirements still apply.

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

A Claude checker also fails with `plan cross-check: checker unavailable`,
and the lead parks for a human, when your Claude credential is unavailable or
disabled at claim time, or the automatic Claude pool is empty. No credential is
delivered, and your non-pooled default is never spent. A locked credential
vault remains a transient wait. A pinned Claude model the account cannot use
(the SDK reports `model_not_found`) is treated the same way. A checker session
whose confinement cannot be confirmed at startup fails the check with
`confinement_failed`.

### Automatic plan revision contract

The complete automatically revised `plan_md` must include the exact heading
**Cross-check dispositions**, with one disposition for each item delivered by
the immediately preceding check: addressed (where and how in the plan), or
declined (rationale and inspectable evidence). Older entries are optional.
Round 1 does not require dispositions; missing or incomplete defect entries in
round 2 and later call for ordinary REVISE, never BLOCK solely for that omission.
Coverage notes use `Requirement gap:` or `Coverage uncertainty:`. Disposition
them where useful; a missing or declined prior coverage disposition alone
never justifies REVISE, BLOCK or another round. Defect disposition rules remain
unchanged; a supported decline cannot waive a mandatory defect requirement.
The checker engages with the rationale and accepts evidence-supported declines
as appropriate. An uninspectable instruction claim must be explicitly unverified;
it alone cannot establish an exception to an inspectable requirement. Actual
instruction precedence is unchanged.

Lead and checker plan prompts share a bounded, static worker fact: temporary
file-tool and shell artifacts, including test logs and review exports, belong
under `.uzi/scratch/` in the worktree. Environment overrides are evaluated by
actual effects, including inherited children; the plan must name the effects
it relies on. Relocating script logs and inherited Go build/test temporary files
to permitted scratch is not alone a bypass. Neither a variable name nor its
destination proves harmlessness: effects on configuration, credentials and test
behavior need inspectable evidence. Reject overrides that skip, select, narrow
or otherwise weaken required validation, or violate an applicable explicit
restriction.

This is a prompt contract: dispositions remain inside the exact candidate
digest as untrusted plan content, not authorization. It adds no fields, parser,
transport, history or authority. Code cross-check guidance is unchanged.
Deterministic fixtures and mock tests do not prove model convergence; live
hosted/model attempts, cost and latency are unmeasured, with no universal
convergence guarantee. The [configuration notes](./configuration.md#cross-check-settings)
explain the bounded additional wait and lead revision cost.

### Coverage advice

The checker brief asks both families to enumerate requirements and acceptance
criteria from the supplied issue snapshot (`issue_title` and `issue_body`) and
repository PRDs linked by the issue. Issue comments are not supplied. The
checker uses its existing read/search tools within the current deadline and
limits; it gains no URL fetching or additional tools.

- An omitted or partly covered requirement is a `warning` with the mandatory
  summary prefix `Requirement gap:`.
- A requirement verifiable only on hosted, live or maintainer-only surfaces is
  `info` with the mandatory summary prefix `Coverage uncertainty:`.
- Missing, external, unreadable or oversized requirement sources get a
  `Coverage uncertainty:` note in the summary and are never treated as covered.
  Finding no defect does not establish that a requirement is covered.
- An omitted requirement stated by the issue as a safety, security or
  data-integrity constraint is an `error` defect, with no coverage prefix,
  requiring REVISE under the existing defect rules.

Every coverage item uses one of these prefixes; defects never use either.
Coverage alone never justifies REVISE or BLOCK. Defects come first within the
20-item cap; coverage fills spare space. The summary counts overflow with the
literal form `N coverage notes omitted` (replacing N with the count). Coverage
items and the omitted count never displace a defect. These are instruction
contracts, not a parser that rewrites verdicts or proof of model convergence.

On an approving verdict, both Claude and Codex leads receive every delivered
item, including noncoverage warning/info items, and the complete bounded
summary as nonce-fenced, untrusted, non-gating advice. A summary-only approval
is delivered too. Absent or null findings, or an empty summary with no items,
leave the implementation prompt unchanged.

The lead must verify suggestions against the issue, code and uzi rules,
decline conflicts and explain the decision, and keep existing permissions and
constraints. The approved canonical plan remains authoritative. An omitted
requirement may be implemented only within the issue's stated scope, with the
addition named in the PR description. Code cross-check reviews the
implementation **when enabled**; scope, permissions and canonical-plan
constraints apply independently of that opt-in. Advice requires no extra gate,
re-plan or revision round.

Delivery is best-effort and worker-local: approval notes are lost on reclaim.
When the dormant semantic human-gate path drops the checker approval, it also
drops this advice. This adds no wire/API/schema fields, capability or
persistence. During mixed-worker rollout, an old lead drops approving findings;
an old checker supplies no new coverage account, and a new lead renders the
findings it receives. The code-stage revision prompt's bytes are unchanged.
See the [dated ADR amendment](../adr/2149-cross-check.md#advisory-plan-coverage-amendment-2026-10-23-3334--3168-m1)
and [PRD #3168](../prds/3168-plan-cross-check-coverage.md) for validation and
remaining replay scope.

## 3. Read the evidence and decide

Open the run to see distinct rounds in the feed, the current gate reason,
checked-candidate outcome,
findings, checker-run link, recorded model/effort, each field's source
(`pin` or `worker default`) and reported tokens/cost. Missing legacy sources
stay unknown; later settings changes do not rewrite historical evidence.
Unknown or inconsistent outcomes show **Outcome unavailable**. Findings use
hardened Markdown with a display cap of 16,384 source characters and 20 items;
omitted text is disclosed. Missing or unreported cost shows unavailable;
legacy `subscription` usage shows **No estimate**. Priced Codex checks use
uzi's pinned Standard table in either auth mode: API-equivalent cost is a
comparison, not a bill. See [run cost](./run-cost.md#metered-subscription-and-unreported-cost).
After a human revision,
**Earlier-plan evidence** does not certify the current plan. Approve, request
changes or reject the plan shown at the current gate; the original check stays
as history, without an automatic new checker round.

[CLI](./cli.md#plan-cross-check-evidence) shows the reason and findings;
[Slack](./slack.md#using-it) shows the reason without findings.

## Claude checker isolation

The Claude checker has only the `Read`, `Grep` and `Glob` tools: no shell, web,
nested-agent or write tools, and no repository settings, plugins, skills or MCP
servers. It runs with an isolated environment holding only your Anthropic token
(a Codex checker holds only Codex credentials), as the worker's runner user.
Direct reads are limited to its own checkout, plus its own SDK spill files for
oversized tool output. Glob and Grep patterns that are absolute, home-relative
or have a `..` path segment in any brace expansion are denied, as are patterns
with a backslash, unbalanced braces, or too many braces or expansions. That
pattern check is defense in depth; the runner user's operating-system
permissions are the boundary. See
[worker setup](./worker-setup.md#run-artifacts-and-the-sandbox) for the
tool-path policy and the [architecture](../ARCHITECTURE.md#plan-cross-check).

## Checker activity

A Plan or Code checker run records which repository paths it read and
searched, so a clean verdict can be judged against how much the checker
looked at. Both families and both stages record it. The record is bounded
telemetry: it never changes the verdict, and a failure to record it does not
fail the check.

**What an event holds**

- A **read** records the checkout-relative path and the inclusive line range
  returned, when known.
- A **search** records the scope path (`.` for the whole checkout) and a match
  count. A truncated count shows as `N+`.
- Never the search query, glob or pattern, tool arguments or file contents.
- An attempt outside the checkout is recorded as a category with no path:
  `outside checkout`. `policy` covers a path the recorder refuses to show
  (it carries control or bidirectional characters, or is over 512 bytes). It
  also covers a Codex policy denial (`E_DENIED`, `denied_tool` or
  `unknown_tool`), which can name a path inside the checkout.
  `tool output` is any path under the checker's own HOME directory, for both
  families (in practice the Claude SDK spill file for oversized tool output);
  its path is not shown either.
- Every event has an outcome: `ok`, `denied`, `oversize`, `limit` or `error`.
  The Codex checker reports `oversize` for the helper's oversize code. The
  Claude checker never reports `oversize`. A Claude call refused by the
  code-stage tool budget ends the session and fails the check
  (`confinement_failed`). The refused call is `limit` if its tool result
  arrives before the abort; otherwise it stays **unsettled** (see below).

**Caps.** A check records at most 200 events; further settled calls are
counted in `dropped` but still counted in the summary totals. At most 256
calls are tracked in flight, and a call id must be 1 to 128 bytes. A call the
recorder cannot track (a non-string or out-of-range id, or one past the
in-flight cap) is counted `untracked`; a call that never got a result by the
end of the session is counted `unsettled`.

**Marker and summary.** The runner emits a `check_activity_start` marker when
the checker run begins and one `check_activity_summary` when the session ends.
The summary carries the family, stage, per-outcome counts for reads and
searches, `dropped`, `untracked`, `unsettled` and `complete`. `complete` is true
only when the start marker was emitted, the checker's model session started
(the Claude session's init frame arrived, or the Codex turn start was
confirmed) and the check then ended (normally, or by failure or timeout), no
begun call is unsettled and none was untracked. A check that ends before its
model session starts (for example an invalid claim, an expired deadline, a
clone failure, a missing credential, an invalid pinned effort, a provider that
fails to start or authenticate, or a pinned model rejected at turn start)
reports `incomplete`, never "0 reads, 0 searches, 0 dropped, complete".
`dropped` does not affect `complete`: a
dropped event is still counted in the totals.

### Where it shows

- **Checker run page.** The run page of a checker child shows a
  **Checker activity** line, and the feed lists each event as plain text (for
  example `read api/x.go L1-200 · ok` or `search api/internal/forge · 3 matches
  · ok`).
- **`uzi run logs <checker>`** prints the same rows. See
  [CLI](./cli.md#checker-activity).
- **`uzi run get <checker>`** prints one `ACTIVITY` row with the summary.

The line reads `4 reads, 0 searches, 0 dropped, complete` (or `incomplete`, per the summary's `complete` flag) once the summary for
the run's current attempt has arrived. Before then, or when it never arrives:

| Line | Meaning |
|------|---------|
| `activity capture in progress` | The run is not finished and the current attempt has emitted its start marker. |
| `activity capture incomplete` | The run finished with a marker but no summary for the current attempt. |
| `activity capture unavailable` | The run started and finished but the current attempt emitted no marker. A missing marker does not prove an older worker; it can also mean capture failed early. |
| `earlier attempt (generation N): ...` | A reclaimed checker's earlier attempt left a summary. It is labelled as that attempt's, and shown after the current state. A newer generation than the run reports is ignored. |
| `historical; current capture unknown: ...` | The API predates the run's `claim_generation` field, so the newest summary cannot be bound to the current attempt. |

### Rolling out activity

- Web and CLI releases that predate this feature show an unknown activity
  event as `status: <event>` (web) or as the raw payload JSON row, sanitized
  and capped (CLI `uzi run logs`), so a worker that rolls out first is
  harmless.
- A checker on an older worker records nothing, so a finished check shows
  `activity capture unavailable`, never "read nothing".
- Activity and usage frames now share one batched channel, which is closed
  before the terminal state is posted. A usage frame that is not delivered
  still fails the check with `model_error`. A tombstoned or still-buffered
  activity frame does not by itself (though a usage frame queued behind it is
  then undelivered too), but a channel breaker trip (for example a fatal API
  rejection of any batch, or no usable outbox while the API is unreachable) or a
  tombstone-set overflow fails a would-be success with `model_error`
  whichever frame caused it. A usage post that fails transiently and is later
  delivered does not fail the check. A usage frame that was written to the
  worker's outbox (the API was unreachable) never counts as delivered, even
  after the outbox drains, because a replay can deliver a dropped-message
  marker in its place: a would-be success fails `model_error`.
- Activity lands on the checker child run, not the lead. The lead's
  stalled-health warning during a code-check wait is unchanged.

## Rolling out Codex-lead checks

Deploy the api and its migration before the workers. A new api adds
`plan_cross_check_codex_lead` to a Codex lead's claim. A new worker that does
not receive it (an older api) does not submit the check and parks the lead at
the human gate with `plan cross-check: not yet supported for a Codex lead`
(`codex_lead_unsupported`), the same park as a Codex lead on an older worker,
until the api is upgraded. A worker with `WORKER_CROSS_CHECK_SLOTS=0`
(hosted: `UZI_WORKER_CROSS_CHECK_SLOTS=0`) takes no checker children; if no
other worker of the user can, the check waits until its deadline and the lead
takes the human gate, for either family (see
[cross-check slots](./worker-setup.md#cross-check-slots)). Migration 00314
allows Claude checker runs; its Down
deletes Claude checker runs but keeps the check history, with the checker run
link empty. Run the Down only with the new api stopped. With ephemeral workers
off, the settings toggle warns when no online worker can run the checker,
including when none advertises `cross_check_codex_lead_v1`.

## Semantic contract profile (dormant)

**Dormant.** Production enrollment is off until #1231, no production code creates a
semantic run and only tests inject the path. A real run uses the *structural*
profile described everywhere else on this page.

| Profile | Behavior |
|---|---|
| Structural (legacy, today) | The frozen milestone checklist and exact-head permit. No source-coverage review: zero coverage model calls. |
| Semantic (dormant) | Adds server-issued criterion IDs of kind `worker` or `maintainer`, and an independent coverage review that compares the issue and linked PRD with the proposed plan. |

The coverage review is a bounded, tool-less model turn at the exact approval
decision point, run once per candidate that is about to proceed: after a
required Plan cross-check APPROVE and before auto-approval, or when the
candidate is shown at a human gate. Automatic checker REVISE rounds consume no
coverage review. Per verdict-bearing review there are at most two invocations
sharing 5 minutes and 40,000 tokens, and per run at most three reviews and 15
minutes and 120,000 tokens, independent of the checker's allowance. A changed
candidate, source or criteria set invalidates the verdict and any checker
approval bound to the old plan.

A semantic plan is checked under a distinct candidate digest (`semantic_v1`).
The plan-checker child needs a worker advertising `cross_check_semantic_plan_v1`
(see [Worker versions and upgrades](./worker-upgrades.md#semantic-completion-capabilities-dormant)); with none, the check takes the existing deadline-to-human
path and never falls back to the structural digest. A plan you revise at the gate
triggers no new cross-family checker pass, but that revised candidate gets its
own coverage report within the same non-resetting budgets. When coverage is
missing, uncertain or unavailable an autopilot run parks at the gate instead of
auto-approving; see [Autopilot](./autopilot.md#semantic-runs-park-instead-of-auto-approving-dormant).
Design record: [ADR-2149 amendment](../adr/2149-cross-check.md#1230-coverage-park-and-semantic-digest-amendment-2026-10-20).

### Coverage transport limitation

Codex currently lacks an enforceable token cap. Claude's output cap alone does not prove a conservative full-call bound. Complete large sources can exceed the 40,000-token allowance. Unsupported or unbounded transports and inadmissibly large inputs yield unavailable without a model call. #1231 must establish enforceable bounds before activation.

See [ADR-1225](../adr/1225-structural-completion-interlock-invariants.md#coverage-transport-limitation).

## Recovery before a human gate

A reclaimed lead can start a fresh round within the same budget when
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

Capture derives its budgets from the immutable base in the worker-held bare
repository. The helper reads base objects only from that bare's object
directory, with type, size and hash verification; it rejects bare alternates
and uses neutral Git scratch instead of the clone's object store or
configuration. Missing or detectably corrupt required objects refuse with
`base_unavailable`; a concurrent repack may instead produce `diff_failed`
(diagnostic `git_refused`). Capture holds no long-lived bare lock.

Source reads have a limit of per-path base blob bytes (duplicates included),
plus verified commit/tree bytes, plus 128 MiB. Clean or deleted paths fit
within base credit; changed files charge roughly their new size against the
allowance. Clone inputs grant no credit, and current regular files are read completely
through pinned descriptors without trusting clean index stats. Independent
index, path, walk and ignore budgets also scale from the verified base;
see the [counter definitions and bounds](../adr/2149-cross-check.md#planning-diff-capture-symlinks-gitlinks-and-budgets-2026-10-07-2410).

There is no object-store snapshot copy or snapshot budget. Base discovery
has per-object, depth and deadline bounds, with no aggregate base cap or
cache. No additional absolute ceiling applies to the relative totals. Limits remain: 4 MiB per file, 2 MiB per commit/tree object, depth 64,
64 KiB per ignore/exclude read, 512 KiB patch/output and 200 non-ignored
untracked files. The helper has 28 s and the outer capture 30 s; yielding
during discovery and reads allows cancellation cleanup within the production
2 s SIGTERM kill grace. Large clean repositories no longer meet a fixed
128 MiB source ceiling, but other bounds and deadlines can still refuse.

Hosted post-merge acceptance remains pending: a maintainer must measure
clean HEAD capture including base sizing with a reported sample count,
verify bare read/write posture on Docker and kube-native workers, and
observe an auto-approved scheduled run proceeding without a gate.
Single-UID operation still lacks OS write isolation; split-UID helpers can
read the bare but cannot write it. Capture refusals keep the human fallback.

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

## Code cross-check

**Code cross-check** is advisory. After the lead finishes its work, the
other model family reads the committed code in a read-only child and reports
findings. They are added to the merge request. A failed, timed-out or
unavailable check is recorded and publication goes ahead; no result blocks it.

### Enable it

In **Settings → Run defaults → Cross-check**, enable **Code cross-check ·
Advisory before publication**. The line under it reads "Adds the other model
family's findings to the merge request; never blocks it". It is off by
default, independent of **Plan cross-check**, and needs usable Claude and
Codex credentials (otherwise the server refuses it). The toggle is
`PUT /api/me/cross-check` with `{"code": true}` or `{"code": false}`; omitted
fields keep their value. The response warns when no online worker of yours
can check code on both model families, which needs `cross_check_code_v1`
(see [worker setup](./worker-setup.md#cross-check-slots)).

Each run takes the setting when it is created, so changing it does not affect
runs already created. It applies to issue, prompt, self-improvement, CI fix,
merge request rework and task runs, whether auto-approved or human-gated. An
interactive task is checked once, when it finalises, not at each park for a
follow-up. The **Code** row of the pin grid in Run defaults holds independent
model and effort pins for each checker family, beside the Plan row (see
[pins](./configuration.md#plan-cross-check-model-and-effort-pins)).

### How a check runs

1. When the lead finishes, the worker snapshots the committed head into a
   local-only ref, `refs/uzi-cross-check/<lead-run-id>`. The ref is never
   pushed. Uncommitted changes are not part of the check.
2. A read-only checker child on the opposite family reads that snapshot: a
   Claude lead is checked by Codex and a Codex lead by Claude.
3. The checker runs on the **lead's own worker**, on the cross-check lane, and
   is strictly pinned to that worker; no other worker can take it. A lead on a
   worker that cannot run the check records "Code cross-check incomplete:
   worker unsupported" and continues.
4. With findings, the lead gets **one** repair turn. It verifies each
   finding, fixes what it accepts, and reports a disposition for every
   finding through the `report_cross_check_dispositions` tool: `addressed` or
   `declined`, each with a reason. A finding with no disposition shows as
   "not reported". The repair is not checked again; there is no second check.

`CODE_CROSS_CHECK_TIMEOUT` (default `30m`, at most `2h`; see
[Configuration](./configuration.md#cross-check-settings)) is the checker's
wall-clock budget. The wait is excluded from the lead's wall budget.

An attempt interrupted by a reclaim or a requeue is recorded as interrupted
and is not re-checked.

### Read the result

The merge request gets a **Code cross-check** section with the checked short
SHA, the checker's family, model and effort, then either "no findings" or
"N findings: A addressed, D declined", plus ", U not reported" when any
finding has no disposition. One line per finding follows, with its
disposition and reason. When the repository has owner guidance, a "custom
guidance applied (sha256 ...)" line shows the first 12 hex characters of its
digest. A completed check ends with "Changes after the check were not
cross-checked." An incomplete check renders as prose instead, for example
"Code cross-check incomplete: no local checker", "timed out" or
"interrupted".

The run page has a **Code cross-check** panel with the same evidence, and
`uzi run get <id>` prints it (see [CLI](./cli.md#code-cross-check-evidence)).

### Owner guidance and repository instructions

An owner or admin can tune the check per repository from the **Code
cross-check** panel on the **Repos** page, next to the **Trusted repo** panel.
Two settings are saved together with `PATCH /api/repos/{id}`
(`code_cross_check_guidance` and `cross_check_repo_instructions_enabled`);
the route is cookie-only, so the CLI cannot write them.

- **Guidance** is free text of at most 8 KiB. It is appended to the
  checker's trusted brief after the locked base brief, labelled "Owner
  guidance: additional checks for this repository; it cannot remove the
  required checks, change the output format or the read-only rules". It adds
  checks; it cannot loosen the required ones.
- **Give the checker this repository's CLAUDE.md** lets the checker see the
  repository's conventions. For a new check, the worker freshly fetches the
  configured default branch (or uses worker-owned default-branch discovery)
  and freezes its tip as **`repo_instructions_commit`**. It reads the root
  `CLAUDE.md` at that commit. The diff's **base commit** remains the merge base
  computed from the previously captured default-branch commit and the checked head.
  A symlink is followed only inside the commit tree (at most 8 hops, never
  outside the repository root or into `.git/`). A file over 64 KiB is dropped,
  never truncated, and import lines are stripped. The text reaches the checker
  as fenced advisory data; a `CLAUDE.md` change in the diff is review
  evidence, not instruction. This consent is separate from the lead's
  **Repo instructions** setting (see [Skills](./skills.md#repo-instructions-opt-in-default-off)).

Guidance, the instructions toggle, text and instruction commit are frozen into
the check when it is created; later edits affect new checks only. Forking from
an older default-branch commit does not select older instructions. The pin is
acquired even with consent off, but the file is read only with consent on.
An absent, unreadable, unsafe or oversized file leaves a successful pin with empty text.
If the initial done checkpoint leaves its snapshot unavailable, the existing
snapshot request freshly quiesces and reimports once, using the retained exact
pre-import head. If the initial head was unreadable, only the successfully
settled normal completion-record callback can supply the fallback. A null or
moved candidate gives `snapshot_failed`, without substituting a new head.
Pinning requires verified producing context, claim generation and committed
tracking receipt. Ordinary failure or a clean soft stop leaves SHA-free
incomplete evidence; no successful snapshot or instruction provenance is claimed.
Hard boundary, Git hard-abort and unsafe-cleanup errors fail closed.

Both completion paths freshly quiesce the lead before the credentialed fetch
and durable local pin, within the worker's held boundary. Branch validation
and an explicit forced fetch refspec refresh the configured tracking ref even
with stale tracking data, custom refspecs or a force rewrite; the run does not
select the source branch. The fetched tip is an observation at acquisition,
not a promise to include later default-branch changes.

A definite HTTP 4xx refusal allows one consent re-read and at most one resubmit
with the same pin; it does not re-fetch or re-resolve the tip. Existing check
rows and child claims reuse frozen inputs without acquisition. A present pin
binds the candidate digest even with consent off or unchanged text; polling
and repair compare the pin explicitly. Status exposes nullable
`repo_instructions_commit` metadata, not instruction text. Legacy rows keep
NULL/absent provenance and their historical digests, without backfill.

The existing 64 KiB, 8-hop, repository-root, `.git/`, regular-blob, import
stripping and advisory-fencing protections remain in place. The earlier
acquisition-budget description predates the bounded reaped Codex done path.
Explicit reaped Codex done checkpoints with
`executor.safety` now have independent absolute 60 s soft / 90 s hard allowances
for the initial checkpoint and existing fresh snapshot request (up to nearly
180 s combined). The clocks include gate/queue waits, preliminary reads,
reconciliation, quiescence and actions; commands do not restart them.
Shutdown/lifecycle abort prevents fresh admission. Shared Claude and non-reaped
paths are unchanged. This adds no model turn, completion attempt, checker
invocation or repair rerun. Finalize independently imports work with its existing
guards and separate 50-minute boundary; it is not a snapshot retry. Storage
settlement can outlast an allowance; see [Checkpoint budgets](./configuration.md#checkpoints-two-tiers-prd-122-m8-prd-267).
These source/test boundaries do not establish hosted acceptance.

### Snapshot cleanup

The worker deletes its local snapshot ref once the checker has settled. After
a restart, a boot sweep deletes a leftover ref only on positive server
evidence that matches the lead run id and, when a check row exists, the
lead's claim generation and the head. For every candidate the lead must also be finished or not held by that
worker, and a ref with an open local reader is kept. When the server has no
check row for the lead, there is no generation or head to match, so the sweep
deletes the ref only if it was also never read. On any doubt the ref is kept.

### Rolling out Code cross-check

Deploy the api and its migrations (00324 to **00327** and **00329**) before any upgraded worker that
advertises `cross_check_code_v1`. A worker advertises it only with a
cross-check lane (`WORKER_CROSS_CHECK_SLOTS` above zero) and the Codex
harness. The Downs of 00324, 00325 and 00327 delete data: 00324 deletes
the code-stage checks and their checker runs and drops the new columns,
00325 deletes your Code pins, and 00327 drops the per-repository guidance
columns. The Down of 00326 deletes nothing (it only restores two functions).
Migration 00329 adds the nullable instruction commit and its immutability guard;
old workers may omit it and are accepted as legacy. A new worker's pin is
refused by an old strict api: bounded advisory submission handling fails open
for publication without dropping the pin to retry as legacy.

For a binary rollback, roll back workers **before** the api and retain migration
00329's column, stored pins and digests. Its Down removes provenance and is a
destructive schema operation, not normal rollback. Run a deliberate Down only
with the upgraded api stopped. PRD #2170's hosted acceptance is still pending.
