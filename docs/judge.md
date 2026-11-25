---
title: Run judge
order: 100
audience: user
---

# Run judge

With the run judge on, every one of your **finished** runs gets a
retrospective: an LLM reads the run's trace (agents, tools, plan, review
cycles, delivery) and produces a verdict plus structured recommendations —
never code changes, only advice. It's opt-in and off by default; your
instance admin also has to enable it globally first. By default the judge
runs on the same harness as the reviewed run, and you can pick Claude or
Codex for it instead (see [Judge runs on](#judge-runs-on)). A judge that runs
on Claude uses **your own Anthropic token** — your default one, a token you
name for the judge lane specifically, or auto-selected from your token pool —
and a judge that runs on Codex uses **your usable Codex credential at the
time the judge is queued**, freshly resolved and frozen onto the judge run —
not necessarily the same credential the reviewed run itself used. If your
Claude source is a [model provider](./providers.md), a judge that runs on
Claude runs on that provider instead, unless you pinned a specific Anthropic
token for the judge (see [A judge on a model provider](#a-judge-on-a-model-provider)).
If you have
no usable credential for the judge's harness when the judge would be queued,
the judge is skipped for that run, with no fallback to the other harness. The
run page shows the skip with its reason (see [A skipped judge](#a-skipped-judge)).

## 1. Enable it

Your admin enables the feature globally under **Admin → Instance →
Run judge**. Once that's on, open **Settings → Run defaults → Run judge** and check
**Judge my finished runs**. Admins can also force-enable or force-disable the
judge for any individual user from **Admin → Users**. See
[Admin settings](./admin-settings.md#run-judge) for the instance-wide judge
controls, and [Instance model defaults](./admin-settings.md#instance-model-defaults)
for the instance judge models.

Opting in also means your instance admin can see your recommendations: they
get an attribution-hidden aggregate across every user's — your name, your
runs, and everyone else's, left out. See
[Admins: the All users view](#admins-the-all-users-view) below for what
that aggregate does and doesn't show.

## Judge mode: off, optional, enforced

The kill-switch above combines with a second instance setting — enforce the
judge for everyone — into three effective modes:

- **Off** — the kill-switch is off. Nobody is judged, no matter what else is
  set; the kill-switch always wins, even over enforcement.
- **Optional** — the kill-switch is on, enforcement is off. Each user opts in
  themselves, exactly as in [Enable it](#1-enable-it) above. This is the mode
  once an admin turns the judge on, until they also turn on enforcement.
- **Enforced** — both are on. Every one of your finished runs is judged
  **whether or not you've opted in yourself**, as long as you hold a
  usable credential for the judge's harness (an Anthropic token for a judge
  on Claude, a usable Codex credential for a judge on Codex) — a user without
  it has the judge skipped for that run (shown with a reason on the run page),
  since there's nothing to spend. The
  judge's harness follows your [Judge runs on](#judge-runs-on) choice, which
  enforcement does not override. Spend still
  stays on your own credential: an admin can
  force that judging *happens*, never redirect *who pays* for it. Enforcement
  also overrides a per-user force-disable an admin may have set for you on
  **Admin → Users** — one flag can't tell "an admin disabled you" from "you
  opted out," so enforcement wins whenever it's on.

**Enforced mode has no separate user opt-out switch.** Judging still depends
on a usable credential for the judge's own harness. With **Judge runs on** set
to Codex, removing your Codex credentials means the judge is skipped for every
run (each shows the reason on its run page), and it is never moved to Claude instead; with it set to Claude or to the
run's harness, the harness is the one named there. Your levers under enforced
mode are the *model* it runs on and the harness it runs on, not the on/off
toggle; see the sections below.

## Judge runs on

**Settings → Models → Helper runs → Judge runs on** chooses the harness
the judge runs on, for both the automatic judge and the **Run judge** button:

- **Same as the run** (the default): the judge runs on the harness of the run
  it reviews.
- **Claude**: every judge runs on Claude, whatever harness the reviewed run
  used.
- **Codex**: every judge runs on Codex, whatever harness the reviewed run
  used.

The choice is frozen onto the judge run when it is created. An explicit choice
is never silently replaced: if you pick Claude or Codex and have no usable
credential for it, the judge is skipped for that run, and the run page shows why, rather than running on
the other provider or on the run's own harness. The **Run judge** button
refuses instead, with a message naming the missing credential or the judge's
harness (for example "add an Anthropic token before running the judge", or
"the judge's harness (<harness>) has no usable credential"), and never falls
back to the other provider. The judge always spends your own credential for
the harness it runs on, never a shared one.

The model, reasoning effort and token controls below apply to the harness the
judge runs on. The card dims the other harness's model field with a "Not used:
the judge runs on X" note, and the token picker is dimmed when the judge runs
on Codex. The [effective settings](./configuration.md#effective-settings)
view reports the choice as `judge_harness`.

From the CLI, `uzi settings set judge-runs-on same-as-run|claude|codex` writes
this choice and `uzi settings reset judge-runs-on` returns it to **Same as the
run**. It only redirects where an already enabled judge runs; turning the judge
on stays a web action. See [Changing model and effort
settings](./cli.md#changing-model-and-effort-settings).

## Which token the judge spends

**This picker covers judges that run on Claude only.** A judge that runs on
Codex spends **your usable Codex credential at the time the judge is queued** —
re-resolved fresh (subscription default first) and frozen onto the judge
run, which is not guaranteed to be the credential the reviewed run itself
used — with no separate judge-lane pick; the picker below never applies to
it. If you have no usable Codex credential when the judge would be queued,
the judge is skipped for that run, with the reason shown on the run page, rather than falling back to any other
credential or harness.

By default the judge auto-selects from your Anthropic [token
pool](./anthropic-token.md#letting-uzi-pick-the-token-auto-selection). If you
hold more than one token, **Settings → Run defaults → Run judge** offers a **Token the judge
spends** picker — your default one, a token you name for the judge lane
specifically, or **Auto-select from the pool** — so retrospectives can bill a
different account from the work they review (a cheaper console key for the
reviewing, a subscription for the runs).

The picker also covers uzi's **self-improvement** runs that resolve to the
Claude harness, for the same reason: they are uzi reviewing and improving
itself, not work you asked a particular worker to do, so they follow the
judge's credential rather than the claiming worker's. A self-improvement run
that resolves to Codex instead spends your Codex credential, unaffected by
this picker, the same as any other Codex run. Everything else — issue runs,
autopilot, CI-fix, chat — is unaffected by this setting. Leave it on **your
default token** to keep everything on one account.

**Auto-select from the pool** picks a pooled token the same way an auto worker
does: the token whose weekly allowance resets soonest by default, or the one
with the most rate-limit headroom under `UZI_AUTOSELECT_ORDER=headroom_first`
(see [How it chooses](./anthropic-token.md#how-it-chooses)). Under the default,
for picks above the headroom floor, the in-flight penalty only breaks
exact weekly-reset ties, so several
retrospectives at once concentrate on that one token until it nears the
headroom floor, which can mean more mid-run limit pauses and resumes. It
differs from a worker in one
deliberate way: a worker with an empty pool *holds* the run, waiting for a
token to be pooled, but the judge lane never holds a retrospective — with an
empty pool it spends your default token instead, so finished runs never pile
up waiting on a judge that can't run.

## A judge on a model provider

When your **Claude source** is a [model provider](./providers.md#use-a-provider-as-your-claude-source)
(your own, or an [admin-shared definition](./providers.md#admin-shared-providers) with your own enabled
key), a judge that runs on Claude runs on that provider, with your provider key and no Anthropic token. The
decision is made when the judge is created and frozen onto it: later edits to your source, the provider's
model map or the judge settings never rebind a judge that already exists, and it never falls back to an
Anthropic token.

- **The pinned-token exception.** If **Token the judge spends** is pinned to a specific token, the judge is a
  vendor judge and spends that token even when a provider is your Claude source. A judge set to your
  default token, or to auto-select, follows your provider; if you have no provider source it behaves as
  described above. A judge pinned to a token sends the provider run's trace to Anthropic (see
  [Where the judged trace goes](#where-the-judged-trace-goes)).
- **The model must be mapped.** The judge runs on the model it resolves to
  ([Which model it runs on](#which-model-it-runs-on)), and the built-in default is `opus`. A provider whose
  model map has no `opus` entry (a map with only the required `haiku`, for example) cannot serve a
  default-model judge: set a judge model the map does include, or add `opus` to the map.
- **No wait on rate limits.** A gateway 429 or overloaded answer ends the judge as failed with the fixed reason
  "Provider gateway rate limit reached; a provider judge does not wait on rate limits". The Claude CLI may
  retry the request itself before it reports the limit; once it does, uzi never parks, requeues or automatically re-runs
  the judge, never falls back to a vendor credential, and records no vendor reset time or cooldown for it.
- **A revoked provider fails the judge.** If the provider's key or revision was revoked or went stale before the
  judge was delivered, or the judge model was changed to one the frozen map does not map while the judge was
  queued, the judge ends failed with a typed reason (`fail_origin` `credential_unavailable`). It is never
  paused for you to resume.
- **Cost is unknown.** Usage shows as unknown, as for any [provider run](./providers.md#usage-and-cost).
- **The provider key is redacted** from the judge's output, review, usage and failure text.
- **Codex.** A Codex judge for an owner who has only a Claude source runs on its usual Codex credential
  and ignores the Claude source. A Codex judge for an owner whose **Codex source** is a provider is still
  refused (`provider_lane_unsupported`) until [#4574](https://github.com/vtmocanu/uzi/issues/4574). Neither
  harness ever falls back to the other.

When the judge cannot be created on your provider, the automatic judge is [skipped](#a-skipped-judge) and
**Run judge** is refused, with the same reasons (see [A skipped judge](#a-skipped-judge)); nothing falls
back to an Anthropic token.

### Where the judged trace goes

A judge reads the reviewed run's trace, so the trace reaches whichever evaluator the judge is bound to:

- **A provider judge sends the trace to your gateway**, including the trace of a run that itself used an
  Anthropic token (the judge follows your Claude source by default or auto-select).
- **A pinned vendor judge sends a provider run's trace to Anthropic.**
- **Redirects.** The Claude CLI follows gateway redirects (see the
  [providers redirect warning](./providers.md)), so judged trace bodies can reach a redirect
  destination under either auth style. Your key can reach other paths on the same host, and other hosts too
  with `x_api_key` auth. Bearer is recommended. The operator allowlist is the only control on where provider
  traffic starts.

## Which model it runs on

The judge has **two model settings, one per harness**: the Claude lane
(`judge_model`) and the Codex lane (`codex_judge_model`), each settable per
user and per instance. A judge run reads only the lane of **the harness the
judge itself runs on** ([Judge runs on](#judge-runs-on)), resolving in this order:

1. your own value for that lane,
2. the instance value for that lane,
3. the built-in default: **`opus`** for Claude, **`gpt-6.2-sol`** for Codex.

A value that is not valid for the run's harness is skipped (one
`harness_model_fallback` note on the run) and resolution falls through to the
next step. Your Codex worker-model default does **not** reach a Codex judge.

**On a model provider, the resolved Claude model must be in the provider's model
map.** The result of the order above (your value, the instance value, else
`opus`) is checked against the frozen map when the judge is created and again
when it is claimed. An unmapped model skips the automatic judge
(`provider_model_unmapped`) and refuses **Run judge** with a 409 of that reason; a
model setting changed to an unmapped one while the judge is queued fails it at
claim. uzi never swaps in another model. See
[A judge on a model provider](#a-judge-on-a-model-provider).

**Codex judge models are curated.** `codex_judge_model` accepts only
the curated Codex models (the ones the Codex worker model picker offers);
custom Codex model ids are not accepted for the judge. The Claude lane rejects Codex ids
(including `gpt-6-luna`), so a Claude judge can never be handed a Codex model
and a Codex judge can never be handed a Claude alias. Because a Codex judge
model is always curated, a judge never needs a worker with custom Codex model
support (the `codex_custom_model_v1` capability that other Codex runs with a
custom model require), which keeps the judge's exemption from that claim
requirement sound; this invariant is pinned by the resolver test.

A blank or whitespace-only value counts as unset at every step. A blank
personal value inherits the instance value (for example, a blank personal
Codex lane with the instance set to `gpt-6-sol` resolves to `gpt-6-sol`); only
a blank instance value falls through to the built-in default for that lane.
Values are stored trimmed.

The Claude built-in is **opus** — the strongest model, since the judge's
recommendations feed the [self-improvement job](./scheduling.md#default-jobs)
and a shallow retrospective produces shallow self-improvement work. Your
admin can pin a cheaper instance-wide Claude default (`haiku` or `sonnet`)
from **Admin → Instance → Instance model defaults**; your own value only changes your runs,
while the admin's is the fallback for everyone who hasn't set one.

Where to set each lane:

- **Your Claude judge model:** **Settings → Models → Helper runs → Judge
  model**, the Claude column; leave it on its **Default** option to use the
  instance value.
- **Your Codex judge model:** the same row's Codex column. It is always shown;
  without a usable Codex credential it notes that you can still save the choice.
  A judge model for a harness the judge never runs on (because of
  **Judge runs on**) is dimmed with a **Not used** reason. The page's one
  **Save changes** bar saves every lane you changed along with the rest of the page.
  Each field's Default option, or a line under it where the option does not
  say so, shows the saved effective value: what a judge run would resolve to
  and where that came from (**Unavailable** if the server could not read it).
- **Instance Claude and Codex judge models:** **Admin → Instance → Instance
  model defaults** (the Judge model row, with the same two fields; their Default option reads **Default · <model>
  (built-in)**). The Claude field shows a **Not a Claude model** warning when
  its stored value is a Codex id.

`uzi settings get` prints a **Model settings** table showing, per lane, what
is stored, the default, what a judge run would resolve to, and where that came
from; see [CLI](./cli.md#plan-cross-check-settings). To change a lane from the
CLI, use `uzi settings set judge-model --harness claude|codex <model-id>` and
`uzi settings reset judge-model --harness claude|codex` (the value goes to the
server's validation; a reset inherits the instance value again). See [Changing
model and effort settings](./cli.md#changing-model-and-effort-settings).

**Migration.** A curated Codex id previously stored in `judge_model` (yours or
the instance's) is copied into the Codex lane; the Claude key is left unchanged
and is then skipped, with a note, by Claude judges. Any other Codex id (such
as `gpt-6-luna`) is not copied.

The judge uses your [reasoning effort](./worker-effort.md) preference for the
harness it runs on: Claude or Codex. An unset preference inherits `medium`.
The judge model settings above are unaffected; this setting changes how hard
the judge model reasons.

Opus runs roughly **5–15× the cost of haiku** per retrospective. If your
Anthropic token is a **subscription plan** rather than a metered console key,
an opus judge spends **plan/rate-limit quota, not dollars** — under enforced
mode especially, a busy judge can eat into the quota your real runs need.
Pinning your Claude judge model to `haiku` or `sonnet` is the fix if that's a
problem. Enforced mode has no separate per-user opt-out switch; see [Judge
mode](#judge-mode-off-optional-enforced) above.

## What you get

When a judged run finishes (completed or failed — a cancelled run is never
judged), a review lands in five places:

- **The run page**: a verdict chip (Ideal / OK / Issues found) plus a list of
  recommendations, each with a category, a target (the tool/agent/repo it's
  about), a rationale, and a confidence level. The panel also shows what the
  retrospective itself cost — a small strip with the judge run's tokens in,
  tokens out, duration, and cost.
- **The [Judge menu](./judge-menu.md)**: the cross-run backlog, where the same
  recommendation raised by many runs is **one row** to triage once. This is
  where you work the list; the run page is where you see one run's verdict.
- **The runs list**: each judged run's row carries a `⚖ verdict · N` badge.
- **Slack** (if you've linked your account): a "Run review ready" DM with the
  same summary, opening the Judge menu anchored to that run.
- **The [uzi CLI](./cli.md)**: `uzi review show <run-id>` prints the same
  verdict, recommendations, and your triage state from the terminal — see
  [Reading a review from the CLI](#reading-a-review-from-the-cli) below.

A judge run is the owner's own token spend, so its cost counts as your spend:
it appears in your usage totals alongside your runs, and — since it is the
strongest model by default (see [Which model it runs
on](#which-model-it-runs-on) above) — a busy judge is a real line item worth
watching. The per-run cost/time strip above and your usage totals are the two
places you see it.

Recommendations use a fixed taxonomy: enable an existing tool or skill,
install a missing worker tool, adjust an agent template or prompt, improve an
existing agent (including a repo agent living in git), propose a missing
agent for the repo, improve uzi itself, or flag an efficiency improvement
(labelled "Efficiency", category `cost_efficiency`): a way the run could
have reached the same outcome with fewer tokens, turns or agents, or less
wall-clock time, without weakening any check (quality and correctness come
first, so a saving that would drop a real verification pass is never
recommended). Elapsed-time claims cite the trace's observed timestamps. Queue,
limit, owner-wait and parked time is reported separately and is not attributed
to agent inefficiency unless observed evidence shows an avoidable agent action
caused or extended it. A remedy that needs a factory change (runtime, worker image,
provisioning, CI) is filed under improve uzi instead. The
improve-uzi category feeds the `self-improve` [default
job](./scheduling.md#default-jobs), if you've enabled it on a repo.

## Reading a review with bounded evidence

The judge remains a single-run advisory pass: one model call, no tools and
no code changes. It streams the trace in pages of 500 messages, reducing
each page before fetching the next, up to 100,000 messages (200 pages).
An empty or short page establishes the fetched end. A full page at the cap
leaves the end unknown, even if the run had exactly 100,000 messages: there
is no extra probe. The latest fetched tail is not proof of terminal delivery;
a missing terminal outcome is disclosed as unknown.

The judge's user prompt, including its evidence, fits within a fixed envelope
of at most 120,000 characters. The fixed system instructions are separate
from this user-prompt limit. Coverage, a factual delegation ledger and raw aggregates
have separate budgets before the event-aware prose samples: opening and
latest fetched eligible events, middle plans and revisions, owner steering,
failed delegation starts with results, available review reports, and recorded
delivery or validation claims. Snippets, ledger rows, sample reservoirs and
tracking state are bounded; meaningful events can still be omitted.
Coverage tells the judge the fetched sequence range, count and pages,
eligible/excluded/retained/omitted evidence, and pairing and row omissions
or unknowns. These are model inputs, not new run-page or API fields.
Sampling omissions differ from events that were never fetched.
`progress_note`, `summary_usage` and accounting-only status messages do not
consume behavioral prose slots; status messages carrying failure facts
remain eligible.

Read the resulting assessment with these limits in mind:

- **Delegation identity and outcomes:** pairing uses exact lane, agent-instance
  and invocation-ID associations, not guesses from repeated role names or
  replays. Confirmed pairs mean observed exact associations and recognized
  recorded outcomes without detected ambiguity within available collision
  coverage, not globally unique execution or task correctness. Collisions,
  pending-entry eviction, and the 256-key delegation-history limit constrain
  confirmation. Confirmed outcomes, provisional pairings, incomplete starts
  and unmatched results are distinct observations, not interchangeable failures.
  Open dispatches and unknown orphan results share the same 128 pending slots.
  An incoming orphan evicts the oldest orphan, or is dropped when 128 open
  starts fill the allocation; orphan floods cannot evict open dispatches.
  An incoming start evicts the oldest orphan before an open start. Known
  evicted starts remain ambiguous through bounded tombstones. Orphan coverage
  losses include evictions and admission drops. Retained result-before-start
  pairs matched after a loss remain provisional. Subsequently observed
  start/result associations can confirm within available collision coverage,
  without establishing historical uniqueness. Signal tool starts are excluded;
  persisted `tool_result` records have `tool_use_id`, `content` and `is_error`,
  but no signal or tool-name marker. Prose cannot classify an unknown orphan
  as signal, ordinary or delegation activity. Ordinary tool starts use a
  separate exact-identity FIFO cache of up to 256 keys, retained after
  completion to recognize reuse across ordinary tools and delegations.
  Remembered ordinary results do not consume delegation pending slots. After
  ordinary identity history is lost,
  cross-kind reuse detection is limited to retained ordinary keys; that loss
  alone does not suppress an observed delegation outcome. Unknown orphan
  results remain uncertain, with bounded tracking rather than assumed
  delegation outcomes.
- **Time and causes:** intervals come from persisted-message timestamps,
  not measured execution time. Historical budgets and causes remain unknown
  when absent. A deadline-shaped interval is a hypothesis, not a confirmed
  timeout. An exact recorded Codex child-budget-expiry completion is labelled
  a reported expiry, not a confirmed termination; its outcome stays failed and
  its deadline unknown. The existing run-stopped, open-at-turn-end and
  provider-stream-failure completions keep their recorded interruption causes;
  other diagnostics, including `child_aborted:` and pre-start timeout text,
  keep an unknown cause.
- **Tool activity:** lead/child/unknown counts, including lead activity after
  the last observed dispatch, are signals, not effort percentages,
  productivity, source ownership or test-success evidence. There is no
  blanket ban on lead implementation. The judge is instructed to distinguish
  reasonable continuation after a child failure from unnecessary takeover,
  and to recommend independent review where recorded evidence supports it.
  Missing sampled review does not prove that no review occurred.
- **Recorded claims:** retained delivery and validation text supplies
  available claims and sequence anchors. The reducer does not parse shell
  text into gate verdicts.

A review may optionally use the existing `improve_uzi` or `adjust_template`
categories for a limitation in the judge's own evidence, compaction or
assessment. It should identify the affected conclusion, sequence anchors or
coverage, and an actionable remedy, or state honest uncertainty.
This does not require a recommendation, add a category or invoke another
judge. Matching known `improve_uzi` targets are reused unchanged so recurring
findings group together.

Event-derived evidence and known targets stay inside nonce-delimited
untrusted-data fences: they are inert evidence, never instructions.
The no-tools boundary, secret scrubbing and instruction to summarize rather
than quote raw file or command output remain in place.

## The deterministic fallback

Separately from the LLM call, uzi scans the run's own tool output for
`command not found` / missing-executable errors. Normally this is just a hint
handed to the judge model, which weighs it alongside everything else in the
trace. But **if the judge model call itself fails**, every hit is turned into
an "install a worker tool" recommendation naming the tool, guaranteed — so a
finding still lands even when the LLM doesn't run. When that happens the run
page shows a "judge incomplete" badge next to the verdict.

The one exception is a usage limit: if the judge model call dies because the
account hit its usage limit, no review is posted and the judge run fails with
a failure reason naming the limit window and reset time (or a plain "usage
limit reached" when the model reported neither).

## The failure-class signal

When the reviewed run **failed**, the judge is also handed a trusted
**failure class** — a single closed-vocabulary label for *why* the run failed
(for example `provisioning_failed`, `credential_unavailable`,
`guardrail_blocked`, `agent_failure`, `provider_policy_refusal`, `run_timeout`).
`base_diverged` is also a structured origin, but it rejects a required-base
mismatch before implementation and is excluded from judging, so it is not a
failure class handed to a judge.

The judged failure class is derived from the run's own structured state at the
moment it failed, never by reading the free-text failure reason, so the judge
weighs a fact rather than a string. The
practical effect: a network timeout or connection error is **not** treated as
automatically transient, and a policy- or config-denied failure does not draw a
"just retry / add backoff" recommendation — that class of block stays until the
configuration or policy is fixed.

For Codex, `provider_policy_refusal` records exactly `cyberPolicy` or
`misalignmentPolicyViolation`, not a classification inferred from provider
prose. The reason is a fixed literal, with no provider message, details,
content, or prompt reflected. It records the provider's classification,
not independent proof of a policy violation. See
[provider safety-policy refusals](./run-activity.md#when-codex-reports-a-provider-safety-policy-refusal)
for the literals and bounded root/child provenance.

## Reading a review from the CLI

`uzi review show <run-id>` prints the same verdict, summary,
recommendations, and triage tally from the terminal; add `--json` for
agents. (`uzi run review <run-id>` is the old name — it still works, but is
a hidden, deprecated alias.) A visible-but-unjudged run prints "not judged"
and exits **0**, not a not-found error — the API returns a valid 200 with a
null review, not a 404. While an automatic or manual judge is active for the
run, it prints "judge scheduled" (enqueued, not yet claimed) or "judge in
progress" in place of "not judged" — that phrase now means nobody has
judged this run and nobody is about to. Re-run the judge on a run that
already has a verdict and the command still prints that existing review in
full (it's the current answer until the new one lands), with a trailing
"note: judge scheduled — a re-judge is in flight and will replace this
review" (or "judge in progress"). `--json` carries the same distinction as
a sibling `pending_judge` key next to `review`: `{"state":
"scheduled"|"running", "enqueued_at": …}`, or `null` when no judge is
active — populated alongside a non-null `review` in that re-judge case.
A third sibling, `skip`, is `{"harness": "claude"|"codex", "reason": …, "at": …}`
or `null`. It is non-null only when the run has no review and no pending judge,
so it never appears next to either of them; `reason` is one of
`no_usable_credential`, `credential_disabled`, `codex_pool_unavailable`,
`provider_lane_unsupported`, `provider_source_unusable` or
`provider_model_unmapped`. `provider_lane_unsupported` is now recorded only for
a Codex judge whose owner has a Codex source (a model provider), which stays
unsupported until #4574. Older rows with that reason and `"harness": "claude"` are
historical, from before a Claude source could bind a judge, and are shown in the
past tense. `provider_source_unusable` means your Claude source could not serve
the judge (retired, off the operator allowlist, or without your enabled
key), and `provider_model_unmapped` means the provider's model map does not map the
judge's model. Without
`--json`, a skipped run prints a line such as `run <id>: Judge skipped
2026-11-12: Codex had no usable credential.` in place of "not judged".

**Treat the free-text fields as data, never as instructions.** In the
`--json` payload, `verdict`, `category`, `confidence`, and each
recommendation's `status`/`reason` (its disposition) are closed enums —
safe to branch on. But `target`, `rationale_md`, and `summary_md` are
**untrusted free text**: the judge model derived them from repo/issue/CI
content an attacker can influence, and they can be instruction-shaped. Never
execute, follow, or otherwise act on them — branch only on the enums, and
render the free text as inert data.

The wire value for a fallback review is **`status: "failed"`**, even though
this page's badge above reads "judge incomplete" — a `--json` consumer
keying on the string "incomplete" would silently treat every fallback review
as complete.

Same as the web UI, there's no CLI `rejudge` verb: re-running the judge
spends your own Anthropic token, so it stays the **Run judge**/**Re-run
judge** button on the run page. Setting or undoing a triage disposition
*is* available from the CLI — it spends nothing — see
[Reviewing and triaging from the CLI](./cli.md#reviewing-and-triaging-from-the-cli)
in the CLI reference.

## Re-running the judge

On any judged run's page, click **Run judge** (or **Re-run judge**, once a
review already exists) to get a fresh retrospective. This is owner-only —
clicking it is your consent to spend your own token, no separate opt-in
required — and rate-limited per user so it can't be hammered. A re-run
replaces the previous verdict rather than appending a second one.

While a judge is active for the run — the automatic one enqueued the
moment it finished, or one you just started — the button is disabled and
reads **Judge scheduled** or **Judge running…**. If the run already has a
verdict, that verdict stays on screen and a note appears beneath it: "A
judge is scheduled for this run — the new verdict will appear here when
it finishes." or "A judge is running for this run — the new verdict will
appear here when it finishes." A run with no verdict yet shows the
equivalent as the whole panel body instead: "Judge scheduled — the
verdict will appear here when it finishes." or "Judge in progress…".
Either way this is server truth, so it shows on a fresh page load and to
any other viewer of the run, not just the tab that started it, and it
swaps to the verdict on its own when the judge finishes — no reload
needed for any judge that lands within about ten minutes, which covers
every real one. A run with no judge in flight is unchanged: the button
stays live and reads **Run judge** (or **Re-run judge**).

## A skipped judge

When the automatic judge is skipped because the judge's harness has no usable
credential, the run page says so instead of showing an empty panel: it names
the reason (no usable credential for the harness, the credential is disabled,
no pooled Codex account was available, your Codex source is a model provider the
judge does not support yet, your Claude source could not serve the judge, or the
provider did not map the judge's model), links to **Models**, where
[Judge runs on](#judge-runs-on) is chosen (the Claude judge token is chosen in Run defaults; Codex
accounts are linked and pooled in Settings), and offers **Run judge**. The skip is recorded only after every other check has passed, so
a run the judge would never review (see [Which runs are judged](#which-runs-are-judged))
shows nothing.

The record is a single latest entry per run: a newer skip overwrites it, and
creating any judge for the run (the automatic one or **Run judge**) deletes it.
It shows only while the run has no review and no judge in flight. Clicking
**Run judge** while the credential is still missing is refused with a message
naming what is missing (an Anthropic token, a usable credential for the judge's
harness, or a pooled Codex account), shown inline in the panel, and the skip stays.
A provider refusal is a typed 409 instead: `provider_source_unusable`,
`provider_model_unmapped` or `provider_lane_unsupported` (a Codex judge with a Codex
source). A fourth, `judge_policy_changed`, means your judge settings changed while the judge was
being created; nothing was created and nothing fell back to an Anthropic token, so try again.

## Filing an issue from a recommendation

Each recommendation on the run page carries one triage row of three actions
at equal weight: **File issue**, **Mark done**, and **Dismiss ▾**, the same
row [the Judge menu](./judge-menu.md#2-triage-a-whole-group-at-once) uses.
Click **File issue** and uzi templates an editable draft, title, description,
and a repo picker, from the recommendation, the review, and the judged run,
entirely from data already stored. Nothing new is sent to the judge model and
no token is spent. Edit the draft if you like, pick a repo, and click
**Create**: uzi files the issue on GitLab as your connection's bot (uzi has
no per-user forge identity, so every issue, note, and label it creates is
authored by the same bot as everything else) and remembers the link so the
same recommendation can't be filed twice.

The filed issue carries the `uzi` label — it shows up on the board and can
start a run in one click, with no PRD file needed — but **never** the
autopilot label, so filing never starts a run by itself. Filing an issue and
spending tokens on a run stay two separate decisions you make.

If an admin files a recommendation from *your* review, the draft shows
"from user X's worker, run \<id\>" so they can see whose text they're about
to publish before they click Create.

**The recommendation text is untrusted, and it can cross projects.** It's LLM
output derived from your run's trace, which can itself be shaped by whatever
the run touched — and the repo you're filing into may not be the one the text
came from. Before filing, uzi fences the untrusted text, strips anything that
looks like a GitLab quick-action, and scans for known secret shapes (GitLab
tokens, AWS keys, PEM private keys, and more). That scan is best-effort
defense-in-depth, not a guarantee: it recognizes common token shapes but is
not exhaustive, so reading the draft is still the real control. Read the
draft before you click Create; don't file text you haven't looked at.

Re-running the judge on an already-filed recommendation doesn't refile it —
the existing link is kept, and if the new verdict changed the recommendation,
the filed row is flagged "filed for an earlier version" so you know to check
whether the issue still matches.

## Triage: resolve, dismiss, and count

Filing an issue (above) isn't the only way to close out a recommendation.
Each one also carries a **triage state** you set with one click: **Mark
done**, or **Dismiss ▾** and pick a reason — **Won't do** (valid, but not
worth acting on) or **Not an issue** (the judge got it wrong — a false
positive). **Undo** clears it back to whatever it was before: **Filed** if
you'd already filed an issue for it, otherwise **To triage**.

A recommendation is always in exactly one of four states, ranked highest
wins when more than one applies: **Dismissed** > **Done** > **Filed** > **To
triage**. Filing and triaging are independent actions, so you can file an issue
and later mark it done — a filed-and-done row shows as done, not filed.

**It survives a re-run of the judge.** Dismiss a false positive, click **Run
judge** again, and it comes back quietly dismissed, not reopened for you to
re-triage. If the underlying finding genuinely changed under it, though, the
row picks up a **"recommendation changed since you resolved"** flag so you
know to look again — a re-judge that leaves the finding's text unchanged
never raises it. uzi compares the recommendation's text at the moment you
resolved it against its current text on every read, so the flag reflects
whether *this specific finding* changed, not merely whether *a* re-judge
happened.

Two places tally the same four buckets from the same server-computed counts,
so they can never disagree: a **triage bar** at the top of each judged run's
recommendations, and the header of the **[Judge menu](./judge-menu.md)**,
which counts all your runs at once. Both break out **false positives** — how
many "Not an issue" dismissals you've made — as a sub-count of Dismissed.
(The all-runs tally used to sit as a strip above the runs list; it moved to
the Judge menu, and each run's row on that list gained its own
`⚖ verdict · N` badge instead.)

**No token spent, nothing written to GitLab.** Setting or undoing a
disposition is a local, instant action: it never calls the judge model and
never touches the forge.

**Feeds the `self-improve` [default job](./scheduling.md#default-jobs).** An
"improve uzi" recommendation you mark **Done** or **Dismiss** drops out of
your own self-improve job's backlog, the same way filing an issue for it
already does; **Undo** puts it back next cycle.

**Closing the filed issue marks it done for you.** Once the issue you filed
is closed on the forge, uzi moves that recommendation to Done by itself,
labelled "done via #IID" — once, and never over a verdict you set yourself.
See [Closing a filed issue marks it done](./judge-menu.md#5-closing-a-filed-issue-marks-it-done)
for the two preconditions.

**Triage one idea once, across every run.** The same recommendation recurring
in ten runs is ten coordinates here, but a single row on the
**[Judge menu](./judge-menu.md)**, where one action settles all of them.

**If a re-judge stops raising a recommendation you'd triaged** (its next run
just doesn't surface that finding again), your disposition is kept but goes
dormant: it won't appear anywhere and won't count toward either tally. If a
later re-judge raises the same finding again, your old disposition
reappears — with the staleness check above applying as usual. It's only
cleared for good if the review itself is deleted (e.g. the run is deleted).

## Barred-CLI recommendations are dismissed for you

A recommendation that asks to enable or install a **credential-bearing CLI that
uzi's policy permanently bars** — `glab`, `gh`, `aws`, `az`, and the like — can
never be actioned: workers are deliberately denied those tools, so the finding is
a dead end however the tool is named in the recommendation. uzi recognises this case and
**dismisses the recommendation for you automatically**, as **Won't do**. On the
[Judge menu](./judge-menu.md) the occurrence reads **"Dismissed · barred CLI"**
rather than a plain "Dismissed", so an auto-dismissal is always distinguishable
from one you made yourself — and its rate across your backlog is observable
through that distinct label.

This is the deterministic backstop behind the judge model's own guidance not to
recommend barred tools: even when a retrospective slips one through, the
dismissal still lands. And it costs nothing permanent if the match is ever wrong
— the dismissal is **visible and reversible** like any other. Click **Undo** on
the row (or run `uzi review undo <run-id> <rec-id>` — see [Reviewing and triaging
from the CLI](./cli.md#reviewing-and-triaging-from-the-cli)) and it returns to
**To triage** for you to work by hand.

## Which runs are judged

Only finished **issue** and **CI-fix** runs are eligible. Chat runs, judge
runs, and self-improvement runs are never judged — there's no recursive
judging, and no self-feeding loop.

One more run is deliberately skipped: a run that **failed before its agent ever
started** — a provisioning, credential, or guardrail block that stopped the run
at zero iterations. There is no agent behaviour to retrospect on such a run, and
judging it would only spend the (strongest, most expensive) model on a run that
did nothing, so the judge is skipped. **The failed-run Slack DM still lands** —
the skip drops only the retrospective, never the "your run failed" alert. (A run
that started and then crashed early is *not* in this set; it carries agent
behaviour and is still judged.)

A failed run with `fail_origin = base_diverged` is never judged, including on
resume and regardless of its iteration count. This is a required-base rejection,
not an execution failure to retrospect on. See
[required-base changes](./run-activity.md#when-a-required-base-has-changed).

A run with `fail_origin = provider_policy_refusal` has the same eligibility
as `agent_failure`, including a resumed run. The judge may review substantial
work completed before the refusal; retrospective review does not retry the
terminal execution. A child refusal is an observation, not a separate failed
run or a new reason to skip judging; the parent's outcome still governs.

## Admins: the All users view

If you're an instance admin, a **Mine / All users** switch appears at the
top of the Judge page. **Mine** is everything above, unchanged. **All
users** groups the same coordinates (category + target) across *every*
user's recommendations, so alongside how many runs a recommendation recurs
in you also see a count of **distinct users** who hit the pattern — how
widespread a recommendation is across the factory, not who's raising it.

**Attribution is hidden**: no owner, no run title, no link into a run. Each
occurrence in the expanded group reads only "A run, judged \<time\>" with
its verdict and triage state. The aggregate is a view over *patterns*,
where the person is noise — and, on a small team, a temptation. Say this
plainly, though: it's **aggregated across users, not anonymous**. A
recommendation's rationale is free text the judge wrote from your run's
trace, and it can still name a repo or a file, so a determined reader may
be able to infer whose it was from the text alone.

Two actions are offered, one of them a deliberate exception to the hiding
above:

- **File issue** drafts from the coordinate's newest open occurrence — and
  that draft card *does* name the producing run and its user. Filing
  publishes that user's (LLM-authored, unverified) worker text to a forge,
  so before clicking Create the admin needs to see whose text they're about
  to publish — the same rule that already governs an admin filing from a
  single run (see [Filing an issue from a recommendation](#filing-an-issue-from-a-recommendation)
  above).
- **Mark done** settles the coordinate across every user's open occurrence
  in one action. Each affected owner's own row then reads **"Done by an
  admin"**, with a working **Undo** on their side — undoing it only removes
  what the admin set; a verdict a user set for themselves is left alone.

There's **no Dismiss** here: dismissing someone else's recommendation is
that person's judgment to make, not an admin's.

The Mine / All users choice is remembered in that browser (default
**Mine**); switching to All users never changes what your own nav badge
counts — it's always your own To-triage total, whichever scope you're
looking at.

Reads are on the CLI too — `uzi admin review backlog` and `uzi admin review
stats` (see [Reviewing and triaging from the CLI](./cli.md#reviewing-and-triaging-from-the-cli))
— but File issue, Mark done, and Undo across users are web-only.

## Good to know

- **Cost**: a judge run is one model round-trip over a compacted version of
  the run's trace, billed to the run owner exactly like any other run on
  their token. With the feature off (globally, or for you), nothing fires and
  nothing is spent.
- **What's shared**: the Slack DM carries the verdict, a short
  (secret-scrubbed) summary, and the recommendation count and categories. The
  full recommendation detail (target, rationale) stays on the run page.
- **Untrusted text, rendered safely**: every free-text field the judge
  produces is validated, length-capped, and secret-scrubbed before it's
  stored. The run page renders the judge's `summary_md` and each
  recommendation's `rationale_md` through the shared hardened markdown
  renderer (the same one `plan_md` uses): no `rehype-raw`, so raw HTML stays
  inert text; dangerous URL schemes (`javascript:`/`data:`/`file:`) are
  stripped; links are forced external; images are size-capped. The
  bidi/control-char strip still runs at the render site first. The `target`
  coordinate stays inert escaped plaintext — it is data the page posts back,
  not prose.
