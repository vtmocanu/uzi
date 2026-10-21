---
title: Autopilot
order: 80
audience: user
---

# Autopilot

With autopilot on, adding the **autopilot label** (default `autopilot`, see
[Admin settings](./admin-settings.md)) alongside the **run-eligibility label**
(default `uzi`) — or assigning the issue to the uzi-bot account instead — to a
GitLab issue starts a run for you, unattended: no plan-approval step,
using your own credential for the selected harness. The plan is still
recorded in run history as an audit trail, and the merge request stays your human review
gate. Off by default, and opt-in per user — see below. If you also opt in to
[Plan cross-check](./cross-check.md), a plan must pass a check by the other
model family (Codex for a Claude lead, Claude for a Codex lead) before
implementation. Eligible changes requested go back to the lead
for bounded automatic revisions. Exhaustion, blockers and check failures
normally require a human decision; irrecoverable delivery losses fail the run.

## 1. Set your forge identity

uzi only knows your bot account's identity from its PAT, not yours. Open
**Settings → Forge → Your forge identity (for autopilot)** and enter your
own GitLab username for that connection. uzi checks it against the forge on
save; an unresolved username still saves, with a warning, since a forge
blip shouldn't block the save. See [GitLab bot setup](./gitlab-bot-setup.md) or
[Forgejo bot setup](./forgejo-bot-setup.md) for the rest of the Forge page.

Matching is exact and case-sensitive, per forge host: a case-variant of your
username is a different identity for this purpose and isn't deduplicated
against yours. Use the exact casing GitLab shows for your account — a
username value is claimed on a first-saved-wins basis, so entering someone
else's exact username first blocks their own later save with that value.

## 2. Opt in

Open **Settings → Autopilot** and check **Enable autopilot for my
account**. Off by default; attribution spends your own model credentials
for issues that trace back to you. Harness selection does not grant consent
or change forge-identity matching. Configure credentials for the selected
harness in Settings; Claude token bindings still follow
[Anthropic tokens](./anthropic-token.md).

## 3. Add the label in GitLab

Add the autopilot label to any issue that already carries the
run-eligibility (`uzi`) label **or** is assigned to the uzi-bot account —
bot assignment satisfies only the run-eligibility condition, so the
autopilot label is still required for an unattended run. uzi resolves *who
added it*: the label adder if mapped and opted in, else
the issue's author, else neither — and it stops there. A run only ever
starts for someone who has both set their forge identity and opted in, with
the repo connected under their own account. Closed issues are never
autopilot candidates, even freshly relabeled — an autopilot-only guard the
manual **Start run** button doesn't have.

**Bot-assigned issues are candidates too.** An issue is eligible for a run
whenever it carries the run-eligibility label above **or** is assigned to
the uzi-bot account (see [Admin settings → Run
eligibility](./admin-settings.md#run-eligibility)), so the poller also picks
up an autopilot-labelled issue that's bot-assigned but never labelled.
Consent and attribution still key on the **autopilot-label add event**, not
the assignment: an assignment has no adder to resolve, so it never bypasses
the who-added-it rule above — it only widens which issues become autopilot
candidates once the autopilot label lands.

## Which harness autopilot uses

After owner opt-in and forge-identity checks, uzi captures the consenting
candidate's current forge issue. Exact `harness::codex` or
`harness::claude` selects that harness explicitly and requires a usable
credential for it, without fallback. A Codex-only owner with a usable Codex
credential can therefore start a `harness::codex` issue without an
Anthropic token. Both recognized labels or another `harness::*` value are
invalid; duplicate copies of the same recognized label are allowed.

With **no harness label**, the legacy gate still requires an Anthropic
token to be present; actual harness selection later follows existing
automatic resolution (D11). Separate model defaults stay separate.
Issue harness labels affect automatic issue starts, not manual web/CLI
creation or chat `start_run`; see the
[scheduling precedence table](./scheduling.md#which-harness-a-schedule-runs-on).

Invalid labels or a refused selected-harness credential start no run.
The poller logs the static reason through `poller: autopilot create run`
and leaves the label-add event unrecorded for the next poll to retry.
There is no new issue comment for these refusals; the existing
**No eligible user** comment path is unchanged.

## What happens next

- **Eligible**: the issue moves to In Progress, a run starts, the plan is
  recorded, and on success a comment lands on the issue with the merge
  request link. With Plan cross-check enabled, a Claude or Codex lead proceeds
  after the latest exact-plan pass by the other family, with bounded automatic
  revisions on eligible changes requested. Exhaustion, blockers and check
  failures normally park for your plan decision. A Codex lead on a worker
  without `cross_check_codex_lead_v1` parks for a human. See [Cross-check](./cross-check.md) for
  terminal delivery exceptions.
- **The agent never stops to ask you something, either** — with one
  exception. If it would otherwise pause for an ordinary clarifying question
  (see [Answering a question](./run-activity.md#answering-a-question)), an
  autopilot run auto-resolves instead — "proceed on your best judgment" —
  and notes the assumption it made in the run feed rather than parking. A
  [structural completion hold](./run-completion-hold.md) is not
  auto-resolved this way: guessing past a frozen milestone the run couldn't
  actually finish is exactly the failure the interlock exists to catch, so
  an autopilot run still asks and, absent an answer, still parks like any
  other run would.
- **No eligible user**: one comment explaining why, and no run — never
  repeats on later polls, even across a full resync.
- **Failed run**: one comment with a link to the run, not the failure
  reason itself — the run may contain agent-supplied free text, so the
  comment points at the access-controlled run page for detail rather than
  repeating it in a member-visible GitLab comment.

## Semantic runs park instead of auto-approving (dormant)

**Dormant.** Production enrollment is off until [PRD #1231](https://github.com/vtmocanu/uzi/issues/1231): no production code creates a semantic run today, so every real autopilot run keeps the behavior described above (the *structural* profile, which makes zero coverage calls). This section describes the built-but-unreachable path that only tests exercise. Nothing here is live behavior.

A *semantic* run (see [Cross-check](./cross-check.md#semantic-contract-profile-dormant)) compares the issue and linked PRD against the proposed plan before it proceeds. On such a run autopilot auto-approves only when all of these hold:

- the server's single coverage authority reports coverage **complete**;
- when Plan cross-check is required, the checker's **APPROVE** is for the current candidate; and
- the candidate has **no maintainer criteria**.

Otherwise the run parks in `awaiting_approval` through the same plan-approval park other gates use. Notifications use the existing post-commit notification and the Slack `awaiting_approval` hook, and wall time is banked while it waits as at other approval parks. The run does not fail, does not start a replan and receives no synthetic checker REVISE; you decide at the gate.

The gate shows why autopilot stopped as a `semantic_park` reason (the web gate and a `SEMANTIC_PARK` row in `uzi run get`):

| Reason | Meaning |
|---|---|
| `coverage_incomplete` | The review found a source requirement missing or a criterion that is not atomic. |
| `coverage_uncertain` | The frozen source capture was not complete, so the review cannot give a complete verdict. |
| `coverage_unavailable` | No usable review: a transport failure, a provider safety refusal of the coverage pass (not a policy hold), or exhausted coverage budgets. |
| `maintainer_authorization` | Coverage is complete but the candidate lists maintainer criteria that need your explicit approval. |

The reason is stored only in the presented gate payload, never in the cross-check gate reason. A complete, maintainer-free candidate that is gated only by the checker keeps the existing cross-check gate reason or deadline-to-human path and carries no `semantic_park`.

Three further rules:

- **One-way latch.** Once a semantic run has taken the human gate in an execution, no later candidate in that execution auto-approves, including one you revise.
- **Exhaustion is a hard stop.** When the coverage budget is spent (at most three reviews and 15 minutes and 120,000 tokens per run, independent of the checker's allowance), the gate opens without another model call, whatever checker rounds remain, and shows the consumed and remaining budget.
- **No checker coverage fallback.** With no worker advertising `cross_check_semantic_plan_v1`, the check waits for its deadline and the lead takes the human gate; it never falls back to the structural check. See [Worker versions and upgrades](./worker-upgrades.md#semantic-completion-capabilities-dormant).

### Coverage transport limitation

Codex currently lacks an enforceable token cap. Claude's output cap alone does not prove a conservative full-call bound. Complete large sources can exceed the 40,000-token allowance. Unsupported or unbounded transports and inadmissibly large inputs yield unavailable without a model call. #1231 must establish enforceable bounds before activation.

The shipped workers supply no coverage transport at all, so even an enrolled run would park as `coverage_unavailable`. See [ADR-1225](../adr/1225-structural-completion-interlock-invariants.md#coverage-transport-limitation).

## PRD updates happen unattended

An issue run is asked to update the issue's linked `prds/*.md` file before it
finishes, and to move it to `prds/done/` if the PRD is now fully complete (see
[Agent skills](./skills.md)). An autopilot run does this with nobody in the
loop, the same way it writes code with nobody in the loop. The edit and the
`git mv` are ordinary commits on the run's branch, so they arrive as reviewable
file changes in the merge request, never as an out-of-band write.

Two things worth knowing before you label an issue:

- **A run using [repo agents](./repo-agents.md) can move a PRD to `prds/done/`
  unattended.** Autopilot defaults to the repo's roster when one is detected,
  and a repo-authored reviewer's sign-off is not uzi's own review. Read the PRD
  diff when you review the merge request, the same as the code.
- **The issue's own PRD link is corrected only after you merge.** If the run
  moved the file, uzi rewrites that link in the issue description once the
  merge request has merged, so your merge decision is what authorizes the edit.
  It only ever repoints a `prds/*.md` link the description already carried, and
  only one whose filename matches the file the run declared it moved, so a link
  to a different PRD is left alone. What that bound does not cover: the run's
  own declaration is what picks which link, and nothing verifies the file really
  moved, so an issue linking several PRDs can end up with the wrong one pointing
  at a path that does not exist. That costs a stale link on that one issue and
  nothing wider.

## Retry

Autopilot only reacts to a label being *added*; fix whatever blocked it
(map your username, opt in, shorten an oversized description), then
remove and re-add the autopilot label for a recorded refusal. A failed
run is retried the same way. Harness-label and selected-credential
refusals leave the event unrecorded: correct the harness label or repair
its credential and the next poll retries without re-adding autopilot.
A failed run itself does not automatically restart.

## Mid-run label changes

Removing the autopilot label while a run is active changes nothing — the
run keeps going. Re-adding it while a run is already active is swallowed
silently (no second run, no comment): the active run already covers it.

## If two people connect the same project

Each forge connection carries its own consent. If two uzi users both
connect the same GitLab project, adding the label can start a run under one
connection's owner while the other connection independently — and
correctly — reports "no eligible user". Each owner's opt-in only ever gates
their own connection; one person's label add never spends someone else's
tokens.
