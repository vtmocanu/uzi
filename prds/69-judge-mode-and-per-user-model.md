# PRD #69: Judge mode (off / optional / enforced) + per-user judge model + opus default + spend guards + judge run cost/time visibility + judge accuracy (deterministic signals + pre-start gating)

**GitLab Issue**: [#69](https://gitlab.example.com/vtmocanu/uzi/-/issues/69)
**Status**: Draft (created 2026-07-17; revised 2026-07-17 after fact-check + architecture + risk review, then spend guards folded in per user request, then judge run cost/time visibility (M6) folded in per user request; then judge-accuracy work (M7: deterministic env/failure-class signals + gate off pre-start infra failures) folded in 2026-07-18 per user request — from issue #81, surfaced by the #78 provisioning misjudgment)
**Priority**: Medium
**Supersedes**: PRD #59 (default judge model → sonnet). #59's single change (flip the compiled-in default) is folded in here, but the value is **opus**, not sonnet — see Decision 1, which reverses #59's Decision 1. #59 is closed as superseded.
**Related**: PRD #46 (run judge + self-improvement — introduced `judge_enabled`, `judge_model`, and the per-user `users.judge_enabled` opt-in this PRD extends). PRD #17 (per-user `default_model` — the layering pattern this PRD mirrors for the judge model). PRD #98 (Judge menu — the downstream *output workbench*: this PRD is the judge control plane (mode/model/spend/accuracy/consent), #98 is where its recommendations get triaged. Cleanly separable, and no notification seam: #98 adds **no Slack digest** — it only retargets the existing `judge_review` deep-link and groups those rows in the *web* inbox (judge_review-scoped), so this PRD's M7a infra-skip notification is untouched).

## Revalidation (2026-08-15)

Re-checked against `main @ e5ed9161`, ~1 month after drafting. **Nothing here landed and nothing was obviated** — every milestone is unimplemented against real symbols, and the api even carries an in-code note that #69 "is still Draft" (`handler/judge.go`, the `setJudgeRequest` comment). But the judge subsystem moved under #104/#111/#121/#300 (landed) and is about to move again under #229/#123 (planned), which splits this PRD into a **build-now cluster** and a **coordinate-first layer**:

- **Build now: M1, M2, M3+M5, M6, M7a.**
  - **M3 and M5 MUST ship together.** An opus default without the spend guards recreates exactly the runaway-loop risk Decisions 1/9 exist to prevent. Do not land M3 alone. (`"opus"` still resolves to the top tier — aliases pass straight to the SDK, `agenttmpl/model.go` only validates; `builtins/lead.md` is already `model: opus` — so M3's alias story is unchanged.)
  - **M6 is still a genuine gap.** `foldRunUsage` already admits `kind='judge'`, but the runner delivers no result frame and `apitypes.ReviewDTO` has no usage/timing fields; the judge-menu family (#98/#94/#119/#294) built the workbench but never added cost. Uncontested.
  - **M1** is uncontested — the enqueue gate still has no enforce bypass.
  - **M2 is build-now, standalone (updated 2026-08-15).** PRD #229 is **parked** (owner: not landing anytime soon), so its "coordinate first" hold on M2 is lifted. The claim-assembly seam Decision 5 asked for is already proven and in place (PRD #104's `judgeSecretID(ctx, run.UserID)`), and PRD #300 built the identical nullable-model-override-reusing-`ValidateModel` pattern one layer up (and cites #69 as the sibling) — so a standalone `users.judge_model` is mechanically trivial to build now. Residual cost: if #229 *ever* lands it absorbs this as the model-half of a per-user judge `{provider, model}` — a trivial add-a-column migration, the same absorption it already owes #300's per-schedule model. No unique debt.
  - **M7a re-scoped against #123's shipped behavior (2026-08-15) — now plain build-now.** #123 landed (MR !296; PRD at `prds/done/123-provisioning-without-github-egress.md`) but shipped differently than planned, and the re-scope changed M7a's mechanism (folded into Decisions 11/12 and Touchpoints M7a below). The load-bearing change: **compute the trusted `failure_class` from the server-owned `status` + `iteration_count` axes and the terminal transition's origin — NOT by parsing `failure_reason` strings**, which the repo documents as never-parse free text in three places (`notifysvc/run_failure_notifier.go`, `workersvc/autostop.go`, `00050_run_stop_kind.sql`); the sanctioned precedent is `limitwait.go`'s `rate_limit_type` closed-enum coercion. The pre-start gate targets the **general** pre-start infra class (`status='failed' AND iteration_count=0`, provisioning/credential/guardrail origin), not the #78 egress case specifically — #123's tier-1 bake gate narrowed egress-denied provisioning to a rare `{kubectl, nodejs}` cold-worker tail. **M7b is now effectively dead** (host-naming retired in #123; #290 already classifies transient-vs-permanent), though still formally deferred.
- **Keep deferred: M7b.** Unchanged; #123 landing would retire it for good.

**Citations below have rotted — re-derive at implementation time, do not trust them.** As the judge subsystem absorbed #104/#111/#121/#232, roughly a dozen `file.go:NN` references drifted 20–700 lines (e.g. `DefaultJudgeModel` :109→:135, still `"haiku"`; `settings.go:748`→:911; `workersvc/judge.go:138`→:855; `service.go:295/297`→:663/665). One is a *mechanism* change, not just a moved line: Decision 1/5 describe the judge claim reading the owner's OAuth token directly at `judge.go:177`, but PRD #104 made that vault-aware (`openAnthropic`/`cred.Token`). Per the repo's re-derive-at-assertion convention, line numbers are re-resolved when a milestone is picked up, not maintained here.

## Build directive (2026-08-15)

This PRD is ready to implement as-is; an implementing run needs no external guidance — this section is the instruction. Build the milestones below, in this order, then stop.

**Scope — build every milestone EXCEPT M7b.** M7b is deferred and effectively dead (Decision 11 and the revalidation note above); do not build it and do not add its `WORKER_EGRESS_ALLOW_FQDNS` plumbing.

**Order** (from the Milestones dependency notes further down):
1. **M1 first** — it widens the `SettingsReader` interface and every fake that implements it (a package-wide compile event); every other milestone rebases on it.
2. **M2, M3, M6** — after M1 (M6 is independent of M1, but sequence it here in a single-branch run).
3. **M5** — after M2 (shares the enqueue surface and needs `sqlc generate`, like M2).
4. **M7a** — after M5 (shares `maybeEnqueueJudge`).
5. **M4 last** — consent surface + web + docs + specs; depends on M1+M2+M3+M5.

**Hard constraints:**
- **M3 and M5 MUST land together.** The opus default without the spend guards recreates the runaway-loop cost risk (Decisions 1 and 9). Never ship M3 without M5.
- **M7a follows the re-scoped Decisions 11/12:** compute the closed-enum `failure_class` from the server-owned trusted axes (`status` + `iteration_count` + the terminal transition's origin), mirroring `limitwait.go`'s `rate_limit_type` coercion — **never by parsing `failure_reason`** (documented never-parse free text). The pre-start gate targets the general pre-start infra class, not the #78 egress case.
- **Re-derive every `file.go:NN` citation against the current tree** as you implement each milestone; they have drifted (see the revalidation note) and are not maintained inline.
- Standard uzi guardrails apply: never touch `main`; land the work on an `agent/*` branch and open one MR.

## Problem

The run judge (PRD #46) has three rigid points that admins and users have asked
to soften:

1. **Enablement is all-or-nothing per user.** The global `judge_enabled`
   kill-switch only makes the feature *available*; each user must then opt in
   themselves (`PUT /me/judge`, session-scoped self-service, gate at
   `api/internal/workersvc/judge_enqueue.go:71`). An admin who wants the judge
   running across the whole factory has no way to turn it on for everyone.

2. **The judge model is instance-wide only.** `judge_model` is a single admin
   setting (`api/internal/settings/settings.go:58`); every user's judge runs on
   the one model. A user who wants a deeper (or cheaper) retrospective on their
   own runs, spending their own tokens, cannot choose.

3. **The default model is too shallow for the recommendation half.** It defaults
   to `haiku` (`DefaultJudgeModel`, `settings.go:109`). The verdict half
   (`ideal | ok | issues`) is fine on haiku; the judgment-heavy recommendation
   categories (`improve_agent`, `adjust_template`, `improve_uzi`) feed
   self-improvement runs (PRD #46 Decision 9), and a shallow retrospective
   produces shallow self-improvement work. #59 proposed sonnet; the decision
   here (Decision 1) is opus, with the new per-user override as the cost lever.

4. **The judge misclassifies failures it has no ground truth for.** It reasons
   only from the run transcript (`JUDGE_SYSTEM_PROMPT` — "reason only from the
   trace text; you have NO tools", `agent/src/judge-runner.ts:71`); `buildJudgePrompt`
   (`:293`) feeds it the header, steering log, a deterministic command-not-found
   pre-scan, and a sampled trace — but **no environment ground truth**. Real case
   (issue #78): a hosted run failed in tool provisioning because `api.github.com`
   is egress-denied on dev-cluster (its worker allowlist is three FQDNs,
   `deploy/values/dev-cluster.yaml`). The durable fix — pin nixpkgs so the github
   dependency disappears, or pre-seed the package into the image — is tracked in
   **issue #82** (a github-egress stopgap was tried and reverted to keep the
   minimal-egress guardrail). Given only "connection timed out,"
   the judge called it a **transient infrastructure failure** and its
   **high-confidence** recommendation was **retry with exponential backoff** — which
   cannot work against a policy block. It also spent an opus retrospective on a run
   that failed at 0 iterations, before the agent ever started. The judge failed from
   missing inputs, not weak reasoning — and the input it needed (which failures are
   permanent) is only partly derivable from data the API holds today (see Decision 11).

## Solution Overview

The changes (mode / model / opus are independently shippable; spend guards, cost
visibility, and accuracy build on the enqueue gate and judge paths the first adds):

1. **Instance judge mode.** Keep `judge_enabled` as the master kill-switch and
   add an admin `judge_enforce_all` boolean. The enqueue gate resolves to three
   effective modes:
   - **off** — `judge_enabled=false`. No judge for anyone (unchanged). The
     kill-switch always wins: `enabled=false, enforce_all=true` resolves to off.
   - **optional** — `judge_enabled=true`, `judge_enforce_all=false`. Each user
     self-opts-in via `PUT /me/judge` (today's behavior, unchanged).
   - **enforced** — `judge_enabled=true`, `judge_enforce_all=true`. The judge
     runs on every eligible run for every user *who has an Anthropic token*; the
     per-user `judge_enabled` flag is bypassed. Token-less users are still
     skipped silently (Gate 4, unchanged) — nothing to spend, no notification.

2. **Per-user judge model override.** A new nullable `users.judge_model`. NULL
   means inherit the instance `judge_model`; a set value overrides it for that
   user's judge runs only. The user sets it themselves through their own
   settings (`PUT /me/settings`, alongside `default_model` and `theme`),
   validated by the same model validator, blank = inherit. Resolution at
   judge-claim assembly is user-value-wins, else instance default — mirroring
   how `default_model` layers user-over-template (PRD #17).

3. **Instance default judge model → opus.** Flip the compiled-in
   `DefaultJudgeModel` from `haiku` to `opus` and align every surface that
   states or displays it. Admin-set values still win; the default only applies
   where the key is unset/blank.

4. **Judge accuracy (deterministic signals + pre-start gating).** Changes so the
   judge stops confidently misclassifying failures whose cause it cannot see
   (Problem 4), split by what the API can already do:
   - **M7a (the shippable core):** (a) the API computes a trusted, **closed-enum**
     failure-class signal — extending the existing command-not-found pre-scan — from the
     server-owned trusted axes (the target run's `status` + `iteration_count` + the
     terminal transition's origin), **not by parsing the free-text `failure_reason`**
     (re-scoped 2026-08-15, Decision 11), naming causes like `provisioning_failed` /
     `credential_unavailable` / `plan_rejected` / `rate_limited`, fed into the judge
     prompt as data the judge trusts over its own inference; plus one targeted
     `JUDGE_SYSTEM_PROMPT` addition (transient≠permanent; no retry for a
     policy/config-denied failure). (b) a **pre-start gate**: a run that failed before
     the agent started (`status='failed'` AND 0 iterations AND a pre-start infra origin)
     is routed to a deterministic infra notification instead of an opus retrospective —
     cheaper and more correct.
   - **M7b (deferred, now effectively dead):** enriching the signal to name the specific
     unreachable host and cross-check it against the deployment's egress allowlist.
     #123 (the #82 durable fix) landed 2026-08-15: it retired host-naming, narrowed
     egress-denied provisioning to a rare `{kubectl, nodejs}` cold-worker tail, and #290
     already classifies transient-vs-permanent retry — so M7b's motivation is gone.
     Documented, formally deferred, not to be built.
   Detailed in Decisions 11–12 and M7.

### Consent surface (enforced mode)

Because enforced mode spends a user's own tokens without their opt-in, the user
must be able to *see* that it is happening. The `/me` user payload gains two
effective fields — `judge_enforced_by_admin` (bool) and `effective_judge_model`
(the model their judge will actually run on, after the per-user→instance→default
resolution) — so a non-admin's settings card can state "Judge: enforced by your
admin, runs on your Anthropic token with model X" with the model override right
there. A non-admin cannot read `/api/admin/settings`, so this is the only way
the SPA can surface the mode (arch review finding 2; risk review R2).

### Spend guards (enforced-cost backstop)

Enforced mode + the opus default + the fact that *failed* runs are judged
(`judge_enqueue.go:46`) is the max-cost configuration: a systematically failing
setup would fire an opus retrospective per failure, in a loop, on the user's
token — worse on a subscription plan, where it eats the rate-limit quota the
user's real runs need (risk review R3 + R1). Two admin-tuned guards, checked
per-user at a new **Gate 5** in `maybeEnqueueJudge` *before* a judge is enqueued,
in every mode (a loop is bad even for an opted-in user):

- **`judge_cooldown_seconds`** (default `60`, on) — skip if this user had a judge
  enqueued within the last N seconds. Kills tight loops; safe on 60s because real
  runs take minutes, so sub-minute completions are almost always failures.
- **`judge_daily_budget`** (default `0` = unlimited, opt-in) — skip if this user
  already had ≥ N judge runs in the rolling last 24h. A hard total ceiling for
  admins who want one; `0` disables it.

Both are **count-based and best-effort**: on trip the judge is skipped silently
(no defer, no queue, no notification — identical to a Gate 3/4 miss), logged at
debug. They are blunt (a legitimate high-throughput burst also loses some
retrospectives), which the generous defaults keep to genuine runaways.

What does **not** change:

- **Spend never leaves the run owner's own account.** Enforced mode makes a
  user's *own* runs get judged on that user's *own* tokens — an admin still
  cannot redirect judge spend to anyone else (the PRD #46 invariant holds). The
  per-user model override is the user's cost lever: forced-on but wants it cheap
  → set `haiku`.
- **Stored `run_reviews.judge_model` rows are historical** — they record what
  actually ran (`api/internal/workersvc/judge_review.go:47`) and are never
  rewritten, so per-user overrides stay auditable with no schema change.
- **The judge stays off out of the box.** `judge_enabled` default is still
  `false`; `judge_enforce_all` defaults `false`. A fresh instance is unchanged.

## Design Decisions

1. **opus, not sonnet (reverses #59 Decision 1).** User decision (2026-07-17):
   the instance default judge model is `opus`. The recommendation half feeds
   self-improvement, and the strongest model is wanted by default; the per-user
   override (Decision 4) is the escape hatch. **Cost reality carried forward
   from #59 + risk review R1:** the judge fires on *every* eligible completed
   **and failed** run, so an opus default is the heaviest per-run cost point
   (opus ≈ 5–15× haiku for a single compacted-trace call). Accepted because (a)
   the judge is off by default and opt-in/enforced deliberately, (b) each user
   spends only their own tokens, and (c) the per-user override lets cost-
   conscious users drop to sonnet/haiku. **Subscription-token caveat (new, risk
   R1):** the claim carries the owner's Anthropic OAuth token
   (`workersvc/judge.go:177`); for users on a subscription plan, an opus judge
   consumes *plan/rate-limit quota*, not metered dollars — an enforced opus
   judge can eat the quota the user's real runs need. This is why the admin
   enforce toggle copy (M4) must name the effective default model and suggest
   pinning `judge_model` cheaper first, and `docs/judge.md` must document the
   subscription-quota interaction.

2. **`judge_enforce_all` is a separate boolean, not a tri-state enum replacing
   `judge_enabled`.** Keeping `judge_enabled` as-is and adding one flag is the
   lower-risk change: no migration of the existing setting, no rename rippling
   through e2e (`e2e/run-e2e.sh:1682` PUTs `judge_enabled`), web mocks, admin UI,
   and tests, and the master kill-switch semantics stay where every reader
   expects them. The three modes are derived, not stored. The one representable-
   but-meaningless combination (`enabled=false, enforce_all=true`) resolves to
   off (kill-switch wins), and M4's admin UI greys the enforce toggle when the
   judge is off. (Arch review finding 7 concurs this is not rationalizing.)

3. **Enforced mode is hard force-on (ignores per-user opt-out), not a soft
   default.** User decision: "enforce it for all." A user cannot opt out of an
   enforced judge; their lever is the model override, not the on/off toggle. Two
   consequences the PRD names explicitly:
   - **It silently overrides the admin per-user force-disable.**
     `SetUserJudgeEnabled` (`api/internal/handler/judge.go:57`, PRD #46's
     "force-disable per user" control) writes the same `users.judge_enabled`
     flag that enforced mode bypasses. An admin who force-disabled one user
     (e.g. to protect a scarce token) and then enables `enforce_all` silently
     overrides that decision. One boolean cannot distinguish admin-forced-off
     from user-opted-out, and a third state is not worth it. Accepted, but M4
     greys/annotates that per-user toggle on the admin Users page when
     `enforce_all` is on, so the admin sees it is inert (arch finding 1).
   - **The only true opt-out left is deleting your Anthropic token**, which also
     kills your own runs. `docs/judge.md` states this bluntly so nobody
     discovers it by surprise.
   A soft "default-on but user-opt-out" fourth mode is deferred: the modes are
   derived, so it needs no schema and can be added later at zero migration cost
   (YAGNI).

4. **Per-user judge model layers user-over-instance, blank = inherit.** Mirrors
   PRD #17's `default_model`: `users.judge_model` NULL/blank inherits the
   instance `judge_model`; a set value wins for that user's judge runs. Unlike
   the instance setting (which must be concrete — `validateModelAlias` rejects
   blank, `settings.go:792`), the per-user field accepts blank as "inherit", so
   the write path trims-to-NULL via the same `validateModel`
   (`handler/user_settings.go:87` → `handler/agent_templates.go:570-582`) that
   `default_model` uses.

5. **Resolution at judge-claim assembly, keyed by the run owner.** The claim
   builder `assembleJudgeClaim` (`workersvc/judge.go:138`) currently reads only
   the instance `settings.JudgeModel(ctx)` at `judge.go:154` (verified sole
   resolution site — the other `JudgeModel` references are the payload field and
   the historical `run_reviews` recording). It gains a per-user lookup on
   `run.UserID` and uses that when set, else the instance value. Notes for the
   coder so the seam is not reinvented:
   - Resolving here (not threading the owner from enqueue) is correct:
     re-claims after requeue/affinity-grace re-run `assembleJudgeClaim`, so
     fresh claims and re-claims resolve identically; the instance-value-via-TTL-
     cache vs user-value-via-fresh-read asymmetry is harmless (both read at the
     same claim instant, spend is the owner's).
   - **Error semantics:** on a user-row read error, fall back to the instance
     value best-effort with a log — never send an empty model to the SDK (the
     existing failure mode at `judge.go:152-157` silently drops the model on a
     settings error; keep the drop explicit + logged, risk R6).
   - The per-user override stays inside the same `s.settings != nil` guard as
     today (a nil-settings deployment never enqueues judges anyway).
   - The column likely comes for free: `GetUserByID`'s row gains `judge_model`
     after the migration + `sqlc generate`, so no dedicated query may be needed
     at claim time.

6. **`JudgeEnforceAll` accessor must default FALSE on malformed/absent values.**
   Mirror `SlackEnabled` (`settings.go:434`): a malformed row must never
   silently turn forced token spend on. This means `KeyJudgeEnforceAll` MUST be
   added to the `Validate` bool branch (`settings.go:748`) — the default branch
   falls through to `ValidateLabel`, which would accept `"yes"` and then read as
   false (the pitfall documented at `settings.go:874`). Explicit M1 test.

7. **PRD #46 and #59 are not rewritten in place, but the stale invariant comment
   is fixed.** #46 is a done PRD (historical log); #59 is superseded and closed.
   `handler/judge.go:26` currently says "nobody can opt another user into
   spending their tokens (audit H3)" — enforced mode makes that false at the
   instance level. This PRD updates that comment and records the *deliberate* H3
   weakening in `specs/ai.md` (risk R2 / arch finding 1), so a future audit does
   not read the invariant as intact. The per-user model write path keeps the H3
   discipline (target from session, never body — `user_settings.go`).

8. **Upgrade behavior: silent haiku→opus jump, documented in release notes.**
   User decision (2026-07-17): an existing instance with the judge enabled and
   no `judge_model` pinned starts spending opus after `docker compose pull`. No
   migration pins the old value. Accepted, but — unlike #59's smaller sonnet
   jump — this is a 5–15× change, so it is called out explicitly in the release
   notes **and** `docs/admin-settings.md`, not only docs, with the one-line
   remedy (pin `judge_model` to `haiku`/`sonnet`). (Risk R4 recommended a
   pin-migration; the user chose the simpler documented-jump path.)

9. **Spend guards are two count-based settings, checked in every mode, cooldown
   default-on + budget opt-in.** User decision (2026-07-17): both a per-user
   cooldown and a per-user daily budget. Design choices and why:
   - **Both, because they cover different axes and are not redundant.** Cooldown
     caps *rate* (kills a tight failure burst); budget caps *volume* (kills the
     slow drip a cooldown lets through — 1 fail/min under a 60s cooldown is still
     ~1,440 opus judges/day). Together they bound both.
   - **Count-based, not cost/token-based.** The gate runs at *enqueue* time,
     before the judge runs, so the about-to-run judge's cost is unknown, and
     `run_usage` (PRD #40, `00062_run_usage.sql`) is folded only *after* a run
     completes. Counting the user's judge runs over the window is the clean,
     lag-free signal; a cost-based budget is a possible future refinement (Out
     of Scope).
   - **Enforced at a new Gate 5 in `maybeEnqueueJudge`, in every mode**
     (optional too), not enforced-mode-only: a runaway loop is a footgun even
     for a user who opted in, and the generous defaults never trip on normal
     cadence. On trip the judge is *skipped* silently (no defer/queue/notify —
     identical to a Gate 3/4 miss), logged at debug.
   - **Defaults: `judge_cooldown_seconds=60` (on), `judge_daily_budget=0`
     (unlimited, opt-in).** 60s is safe because real runs take minutes, so
     sub-minute completions are almost always failures — the cooldown protects
     even an admin who flips enforce+opus without thinking about loops. The
     budget is a policy ceiling admins opt into (`0` disables). Both are blunt
     (a legitimate high-throughput burst also loses some retrospectives); the
     defaults keep that to genuine runaways. Follows the existing
     `health_nudge_cooldown_seconds` int-setting pattern (`settings.go:568`).

10. **Judge run time/token/cost are captured through the existing PRD #40 fold,
    not a new accounting path, and fold into the run owner's usage totals (user
    decision, 2026-07-17).** Today a judge run records its cost **nowhere**: the
    `JudgeRunner` posts no `run_messages` and discards the model call's terminal
    result-frame `modelUsage` (`agent/src/judge-runner.ts:259` keeps only the
    text), so `foldRunUsage` — which already does **not** exclude `kind='judge'`
    (it skips only chat, `workersvc/service.go:878`) — never runs for a judge
    run. PRD #46 deferred this explicitly ("Token/cost data joins the input when
    PRD #40 lands"); #40 has landed, so this closes that gap. The fix reuses the
    proven work-run path rather than inventing a judge-specific cost field:
    - **Capture:** when the single judge turn's terminal result frame arrives in
      `consumeModel`, the worker POSTs it as **one** `run_message` for the judge
      run via the existing `postMessages` endpoint. `AppendMessages` →
      `foldRunUsage` then writes a `run_usage` row keyed on the judge `run_id`
      (empty `session_id` is already tolerated), with the same GREATEST merge and
      untrusted-worker clamps (`nonNegTokens` / `numericUSD`) as work runs — **no
      new trust boundary** (the worker's reported usage was already trusted for
      work runs). The judge lane still posts no other messages; it is not made a
      streamed run.
    - **Folds into totals for free:** `SelfUsage` / `AdminUsageTotals` /
      `AdminUsagePerUser` already aggregate `run_usage_totals` over
      `kind <> 'chat'` (`runtime.sql:587/613/647`), so a judge `run_usage` row
      appears in the owner's lifetime / 7-day totals and the admin per-user /
      factory breakdown with **zero query change**. This is the user-chosen
      behavior: judge spend is the owner's own token spend and now shows up where
      the rest of it does, instead of being silently absent — with the opus
      default (Decision 1) the single most expensive per-run call was the one
      missing from the bill.
    - **Duration:** the `JudgeRunner` reports only the terminal state, so a judge
      run's `started_at` is NULL (`SetRunRunning` never fires) and only
      `claimed_at` / `finished_at` exist. Add one `reportState({status:"running"})`
      at the start of `execute()` so `started_at` is stamped like every other
      run and duration is the uniform `finished_at - started_at`; the
      claimed→running transition is unaffected by the awaiting_approval guard (a
      judge never enters that state).
    - **Surface (user pick: the 4-tile strip, mock option C):** the judge run
      stays hidden from the run lists (it is not a work run), but its time /
      tokens / cost surface on the **reviewed** run's `JudgePanel`, as a compact
      4-tile strip (Tokens in · Tokens out · Duration · Cost) mirroring the
      work-run `RunUsagePanel` directly above it, so judge cost reads as the same
      kind of thing. `run_reviews.judge_run_id` already links the judge run, so
      `reviewDTO` gains `judge_run_id`, the judge run's timing, and the five usage
      figures; a judge run predating the feature (no `run_usage` row) renders no
      strip, never a fabricated 0 (the pre-feature-run rule PRD #40 already uses).

11. **Feed the judge a deterministic, closed-enum failure-class signal; it trusts
    it over its own inference.** Extends an existing pattern: the API already hands
    the judge a trusted command-not-found pre-scan (`signalBlock`, assembled API-side).
    Add a **failure-class signal** over the *target* run, computed API-side as a
    **closed enum** (`provisioning_failed`, `credential_unavailable`, `plan_rejected`,
    `run_timeout`, `worker_lost`, `rate_limited`, `agent_failure`, …). The
    `JUDGE_SYSTEM_PROMPT` gains ONE targeted addition (the only prompt change this PRD
    makes — see the Out-of-Scope carve-out): a network timeout is not automatically
    transient, and retry/backoff must not be recommended for a policy- or config-denied
    failure. **Why signals over "make the model smarter":** the judge failed here from
    missing inputs, not weak reasoning — given the ground truth, "transient → retry" is
    not a reachable conclusion.
    - **Re-scoped 2026-08-15 (#123 landed) — derive from trusted AXES, never parse
      `failure_reason`.** #123's investigation surfaced a load-bearing repo invariant:
      `failure_reason` is documented as never-parse free text in three places
      (`notifysvc/run_failure_notifier.go`, `workersvc/autostop.go`, migration
      `00050_run_stop_kind.sql`), and structured signals (`stop_kind`, `rate_limit_type`)
      exist precisely so nobody string-matches it. So compute the class from the
      SERVER-OWNED trusted axes — `status` + `iteration_count` (both already on the judge
      claim, see Touchpoints M7a) plus the *origin* of the terminal transition (which
      claim-assembly sentinel fired — `errCredentialUnavailable` / `errToolPackagesRejected`
      / `errGuardrailBlockedClaim`, or which sweeper/limit path set the state) — and never
      from the reason string. The sanctioned precedent is `limitwait.go`'s `rate_limit_type`
      closed-enum coercion; this signal follows it. (This is the single biggest change
      from #69's original Decision 11, which assumed keying off `failure_reason` prefixes.)
    - **Trust boundary:** the signal is trusted because it is derived from server-owned
      axes, like the command-not-found scan, so it sits OUTSIDE the untrusted-trace
      fence. It is the enum value ONLY — no host token (host-naming was retired in #123)
      and no interpolated `failure_reason` text. `failure_reason` never enters the
      trusted zone.
    - **No cross-language string coupling (was Decision 11's biggest risk, now removed).**
      The class is authored entirely API-side from the transition origin + server axes,
      NOT by matching `failure_reason` prefixes across the TS/Go boundary, so a reworded
      agent constant cannot silently un-class a run. The one worker-origin pre-start case,
      `REASON_PROVISION_FAILED` (`agent/src/provision-run.ts:19`, always
      `iteration_count=0`), is recognized by the `status='failed' AND iteration_count=0`
      shape, not by its text.
    - **M7b (egress-allowlist / host cross-check) is now effectively dead, not merely
      deferred.** #123 retired host-naming (the reason carries no hostname), its tier-1
      bake gate (`toolseed.Covered()`) narrowed egress-denied provisioning to a rare
      `{kubectl, nodejs}` cold-standard-tier tail, and #290's `classifyDevboxError`
      already classifies transient-vs-permanent retry. So M7a's class-only signal plus the
      prompt rule is the full justified scope; M7b stays formally deferred but should not
      be built.

12. **Don't judge a run that never started; route pre-start infra failures to a
    deterministic notification.** A run that failed before the agent began
    (`status='failed'` **AND** `iteration_count=0` **AND** a pre-start infra/config
    origin — the conjunction matters: 0 iterations alone also matches an agent that
    started and crashed before its first iteration report, which is judgeable) has no
    agent behavior to retrospect — the judge's actual strength. **Re-scoped 2026-08-15:**
    the gate targets the GENERAL pre-start infra class — worker `REASON_PROVISION_FAILED`
    plus the API-side claim-assembly terminals (`errCredentialUnavailable`,
    `errToolPackagesRejected`, `errGuardrailBlockedClaim`) and `REASON_NO_TOKEN` — NOT
    the #78 github-egress case specifically (which #123 narrowed to a rare tail). The
    origin is read from trusted axes (Decision 11), never by parsing `failure_reason`.
    Gate it in `maybeEnqueueJudge` (a sibling of the M5 spend-guard Gate 5, after
    Gates 2–4, so a judge-disabled or token-less user gets nothing — this is a judge
    *replacement*, not a general failure notifier). This is both an **accuracy** fix (no
    hallucinated agent-quality verdict on an infra failure) and a **spend** fix (the
    single most expensive per-run call — Decision 1 — is not fired on a run that did
    nothing), so it composes with the M5 guards.
    - **Notifier dependency:** `workersvc` has no user-notification capability today
      (`notifysvc.Notify` is called from the handler layer; `selfimprove/engine.go` is
      the in-package precedent), so the gate needs a notifier **injected into the
      service** (copy the selfimprove wiring). Unlike the enqueue path, which is
      idempotent via the one-active-judge unique index (`judge_enqueue.go:94`), the
      notification has no such final guard — add its own idempotency (a per-run
      notification key or a stamped column, à la `health_notified_at`).
    - Judged-worthy failures — the agent ran and then failed — are unaffected.

## Data model

- **New column** `users.judge_model text` (nullable, no default) — migration
  `00067_user_judge_model.sql` (number is a draft; renamed to the next free
  number above the live head at merge, per the goose discipline in CLAUDE.md;
  live head is `00123_user_sidebar_tokens.sql` as of 2026-08-15 — it was
  `00066_hosted_workers.sql` when this was drafted, so treat `00067` as a dead
  draft number and take the next free number above the head at landing). sqlc: extend `GetUserSettings`
  / `GetUserByID` rows and add `SetUserJudgeModel`, regenerate.
- **New app setting** `judge_enforce_all` (text `"true"`/`"false"`, default
  `"false"`). Follows the PRD #46 no-seeded-row pattern: add to `Defaults`
  (auto-surfaces in `AdminView`/`All`, which range over `Defaults` —
  `settings.go:677/701`), the `Known` set, the `Validate` **bool** branch
  (`settings.go:748`), and a typed `JudgeEnforceAll(ctx)` accessor. No migration
  (an absent row synthesizes from `Defaults`).
- **Two new app settings** `judge_cooldown_seconds` (default `"60"`) and
  `judge_daily_budget` (default `"0"`), both integers. Same no-seeded-row
  pattern; registered in `Defaults`/`Known`, the `Validate` **int** branch
  (alongside `KeyHealthApprovalSeconds, KeyHealthNudgeCooldownSeconds` at
  `settings.go:755`), with `intSetting`-backed accessors (`settings.go:568`
  precedent). Bounds: cooldown `0` (off) or `[60, 86400]` reusing the health
  seconds bound; budget `0` (off) or a positive count.
- **Two new store queries** (read-only, count-based) over `runs`:
  `LastJudgeEnqueuedAt(user_id)` → `MAX(created_at) WHERE kind='judge' AND
  user_id=$1` (cooldown), and `CountJudgesSince(user_id, since)` →
  `COUNT(*) WHERE kind='judge' AND user_id=$1 AND created_at > $2` (budget). Both
  cheap; note a partial index `runs(user_id, created_at) WHERE kind='judge'` if
  either shows up hot (optional — the judge funnel is low-QPS).
- **`run_usage` now covers judge runs** (M6): **no schema change** — the fold
  already admits `kind='judge'`; the only change is that the judge lane now
  *delivers* its terminal result frame. One **new read query**
  `GetJudgeRunUsageForTarget(target_run_id)` returns the target's most-recent
  judge run's timing (`started_at`/`finished_at`) LEFT-joined to its
  `run_usage_totals` row (NULLs for a pre-feature judge), so the review panel can
  show time+token+cost without exposing the judge run in any run list.

## Touchpoints

**M1 — judge mode (enforce-all):**
- `api/internal/settings/settings.go`: `KeyJudgeEnforceAll` const,
  `DefaultJudgeEnforceAll="false"`, `Defaults`/`Known` entries, bool validation
  branch at `:748` (NOT the default/label branch), `JudgeEnforceAll(ctx)`
  accessor (default-false on junk, mirror `SlackEnabled` `:434`).
- `api/internal/workersvc/service.go`: extend the **`SettingsReader`** interface
  (declared `:295`, `JudgeModel` method `:297`) with `JudgeEnforceAll`. **This
  is a package-wide compile event** — every fake implementing `SettingsReader`
  across `workersvc` tests must gain the method; M1 owns all of those updates in
  its own commit (see Milestones).
- `api/internal/workersvc/judge_enqueue.go`: Gate 3 (`:71`) — bypass the
  per-user `owner.JudgeEnabled` check when `JudgeEnforceAll` is true. Global
  kill-switch (Gate 2, `:53-64`) and token presence (Gate 4, `:74-79`) still
  govern; `RerunJudge` (`workersvc/judge_read.go:82`) already bypasses the
  per-user opt-in by design and needs NO change in any mode.
- `api/internal/handler/settings.go` + `settings_test.go`: `judge_enforce_all`
  in the admin GET/PUT surface (mostly free — auto-surfaced from `Defaults`).

**M2 — per-user judge model:**
- `api/internal/store/migrations/00067_user_judge_model.sql` (new).
- `api/internal/store/queries/users.sql`: extend the user-settings read +
  `GetUserByID` row, add `SetUserJudgeModel` (mirror `SetUserDefaultModel`);
  `sqlc generate`.
- `api/internal/handler/user_settings.go`: add `judge_model` to
  `userSettingsDTO`, GET, and the PATCH-like PUT (trim-to-NULL via the shared
  `validateModel`).
- `api/internal/workersvc/judge.go:154`: resolve owner override before falling
  back to instance `JudgeModel` (error semantics + nil-guard per Decision 5).
- Tests: `user_settings` handler test, `workersvc/judge_*_test.go` claim-model
  resolution (user override wins; NULL inherits; user-row error falls back).

**M3 — default → opus (folds in #59):**
- `api/internal/settings/settings.go`: `DefaultJudgeModel="opus"` (`:109`) + the
  Decision-7 rationale comment (`:104-107`) + the fallback doc comment
  (`:470-471`).
- `api/internal/settings/settings_test.go`: `:112` default assertion (real);
  `:205` is the `KeyJudgeModel:"haiku"` sample in the writability map (not a
  default assertion, but a haiku line to update).
- `web/src/pages/AdminSettings.tsx:465,471` (judge model placeholder + "the
  default (`haiku`) is usually right" copy — reword; opus is not "cheap").
- Web mocks/tests asserting the default: `web/src/mocks/mockApi.ts:99`,
  `web/src/mocks/data.ts:275`, `web/src/mocks/mockApi.test.ts:54`,
  `web/src/pages/AdminSettings.test.tsx:39`. (Historical review/run rows —
  leave.)

**M4 — consent surface + web + docs + specs (depends on M1+M2+M3):**
- `api/internal/handler/handler.go`: add `judge_enforced_by_admin` +
  `effective_judge_model` to the `/me` user DTO (`:208-223`), resolving the same
  user→instance→default chain as the claim path.
- `web/src/pages/AdminSettings.tsx`: `judge_enforce_all` toggle with copy
  naming the effective default model (opus) + the own-token / subscription-quota
  cost warning; grey it when the judge is off. Add the `judge_cooldown_seconds`
  + `judge_daily_budget` inputs (0 = off). Grey/annotate the admin per-user
  judge toggle on the Users page when `enforce_all` is on.
- `web/src/pages/Settings.tsx` (user judge card, `:350`): render the enforced
  banner from the new `/me` fields + a per-user judge-model input (blank = "use
  the instance default").
- `web/src/mocks/*`: `judge_enforce_all`, `users.judge_model`, and the two `/me`
  effective fields.
- `handler/judge.go:26`: fix the stale audit-H3 invariant comment.
- `docs/judge.md`, `docs/admin-settings.md`: the three modes, the per-user
  override, the opus default + how to pin cheaper, the subscription-quota
  interaction, the delete-token-is-the-only-opt-out fact, and the upgrade-jump
  note. `specs/ai.md`: decision entries (judge mode, per-user model, opus
  default superseding #59, deliberate H3 weakening). Release notes: the
  haiku→opus upgrade jump. `web/scripts/check-docs.mjs` green via `npm run
  build`.

**M6 — judge run cost/time capture + surface** (depends only on the #46 judge;
independent of M1/M2/M3/M5 — touches disjoint files):
- `agent/src/judge-runner.ts`: in `execute()`, `reportState({status:"running"})`
  once at the start (stamps `started_at`); in `consumeModel`, on the terminal
  result frame, map it and `postMessages(judgeRunId, [resultFrame])` so the fold
  sees it. Everything else (deny-all tool hook, `settingSources:[]`, text
  collection, deterministic fallback) is unchanged.
- `agent/test/judge-runner.test.ts`: assert the result frame is posted (usage
  reaches the API) and a `running` report is sent.
- `api/internal/store/queries/judge.sql`: `GetJudgeRunUsageForTarget` (the
  target's most-recent judge run joined LEFT to `run_usage_totals`); `sqlc
  generate`. No fold change — `foldRunUsage` already folds `kind='judge'`.
- `api/internal/handler/judge.go`: `reviewDTO` gains `judge_run_id`, the judge
  run's `started_at`/`finished_at`, and a usage bundle (reuse `usageDTO`);
  `GetReviewForTarget` (`workersvc/judge_read.go`) returns them.
- `web/src/pages/RunView.tsx` (`JudgePanel`) + `web/src/lib/api.ts` (`RunReview`
  type): render the 4-tile strip (Tokens in · Tokens out · Duration · Cost)
  matching `RunUsagePanel`, reusing `lib/formatTokens.ts` + `formatDuration`;
  absent when the judge predates the feature.
- `web/src/mocks/*`: judge run usage + timing on the review fixtures.
- `docs/judge.md`: judge spend is the owner's own token spend and now appears in
  their usage totals and on the run's review panel.

**M7a — Judge accuracy (class signal + prompt rule + pre-start gate)** (folded from
issue #81; re-scoped 2026-08-15 against #123's shipped behavior — see Decisions 11/12.
Line refs below are current as of that date but re-derive at implement time):
- `api/internal/workersvc/judge.go`: compute a closed-enum `failure_class` over the
  **target** run and carry it on the claim beside the existing command-not-found signal
  (`JudgeSignal` struct at `judge.go:74-80`, assembled in the `judgeSignal` path at
  `judge.go:964`). **Derive it from the SERVER-OWNED trusted axes, NOT `failure_reason`**
  (never-parse, Decision 11): `assembleJudgeClaim` (`judge.go:855`) already puts `Status`
  and `IterationCount` on the `ClaimPayload` (built `:917-951`; `Status` `:922`,
  `IterationCount` `:929`, `JudgeSignal` `:925`), and the terminal transition's origin is
  known API-side — the claim-assembly sentinels `errCredentialUnavailable` /
  `errToolPackagesRejected` / `errGuardrailBlockedClaim` (`service.go:1074-1076`), the
  sweeper/limit paths, and worker `REASON_PROVISION_FAILED` recognized by the
  `status='failed' AND iteration_count=0` shape. Model it on `limitwait.go`'s
  `rate_limit_type` coercion. **Name collision:** `failure_class` is already a slog key in
  `autostop.go` (an unrelated `persistFailKind`) — this is a claim/DTO field, do not
  collide. (`failure_reason` stays off the claim; the worker can read target
  `status`/`iteration_count` via the trace DTO `handler/judge_worker.go:36-49`, TS mirror
  `agent/src/protocol.ts:615-628`, if any part is rendered worker-side.)
- New gate in `maybeEnqueueJudge` (`judge_enqueue.go:44`; Gate 0 = `status ∈
  {completed,failed}` at `:45-48`, and the incoming `store.Run` carries `IterationCount`
  and `FailureReason`), sibling to M5's Gate 5, after Gates 2–4: `status='failed' AND
  iteration_count=0 AND pre-start infra origin` → skip the judge and fire a deterministic
  infra notification. A run-failure notifier already exists (`notifysvc/run_failure_notifier.go`,
  which gates on `stop_kind`, never `failure_reason`) — route the notify half through it
  or the injected notifysvc seam (selfimprove is the in-`workersvc` precedent), with its
  own idempotency (per-run key / stamped column).
- `agent/src/judge-runner.ts`: `buildJudgePrompt` (`:348`) renders the enum
  `failure_class` in the trusted (pre-fence) header, adjacent to where it already prints
  `status` (`:355`), `failure_reason` (`:358`) and `iterations` (`:359`) — enum value
  only, no host token, never re-derived from the reason text; amend `JUDGE_SYSTEM_PROMPT`
  (`:67-98`, which already carries a credential-CLI policy block at `:80-89`) with the
  transient≠permanent / no-retry-for-policy rule. The trace fence and no-tools discipline
  are unchanged.
- Tests: `workersvc/judge_*_test.go` (class derivation from status+iterations+origin incl.
  a provisioning pre-start case; a 0-iteration infra run is notified not enqueued; an
  agent-crash-at-iteration-0 run is still judged) and `agent/test/judge-runner.test.ts`
  (the class reaches the prompt in the trusted block; the prompt carries the rule).
- `docs/judge.md`: the failure-class signal + the pre-start-skip behavior. `specs/ai.md`:
  the accuracy decision (and that the class is computed from trusted axes, not
  `failure_reason`).

**M7b — Egress-allowlist enrichment (DEFERRED, not built):** name the specific
unreachable host and cross-check it against the deployment's `allowFQDNs`. Blocked on
config plumbing (a chart-derived `WORKER_EGRESS_ALLOW_FQDNS` env — the API reads no Helm
value today) + a host source (worker-side enrichment or a run-stream scan). Low marginal
value: the durable fix (pin nixpkgs / pre-seed, issue #82) removes the github
dependency; the design is recorded here so a future need has it, but it is not built in
this PRD.

## Milestones

Dependency notes (corrected after arch review finding 3):

- **M1 owns the `SettingsReader` interface widening and every fake update** in
  its commit — the change won't compile the `workersvc` package until all fakes
  implement `JudgeEnforceAll`.
- **M2 depends on M1's interface commit** to compile its own judge tests
  (shared package), even though the two edit disjoint files. Rebase M2 on M1.
- **M1 and M3 both edit `settings.go` + `settings_test.go`** — no logical
  conflict, but a guaranteed textual rebase. Cheapest is to serialize M3 after
  M1 (or accept the trivial rebase).
- **M5 (spend guards) shares M1's files** (`settings.go`, `judge_enqueue.go`)
  and needs `sqlc generate` like M2 (its two new queries). It is not
  independent: land it after M1+M2 (serialized), not in parallel with them.
- **M4 depends on M1+M2+M3+M5** (it wires UI, the `/me` fields, the spend-guard
  admin inputs, and docs for everything).
- **M6 (judge cost/time) is independent of M1/M2/M3/M4/M5.** It touches the judge
  runner, the review read path, and the `JudgePanel` — none of the mode/model/
  spend-guard files — so it can land in parallel with M2/M3 (or any time), and
  needs `sqlc generate` for its one read query like M2 does.
- **M7a (judge accuracy) shares M1/M5's `judge_enqueue.go`/`maybeEnqueueJudge`** (its
  pre-start gate is a sibling of Gate 5), so it must land **after M5** (serialized on
  that function, not parallel with it), and it edits `judge-runner.ts` like M6.
  Independent of M2/M3/M4. M7b is deferred (not scheduled).

So the safe fan-out is M1 first, then M2 and M3 (and M6) in parallel on top of it,
then M5 after M2 (shared enqueue/sqlc surface), then M7a after M5 (same
`maybeEnqueueJudge`), then M4 last — not all-wide from a cold start. M7b is deferred.

- [ ] **M1 — Judge mode (enforce-all)**: `judge_enforce_all` setting +
  default-false-on-junk accessor + `SettingsReader` widening (+ all fakes) +
  enqueue gate bypass + admin-settings surface. `go test ./internal/settings
  ./internal/workersvc ./internal/handler` green.
- [ ] **M2 — Per-user judge model**: `users.judge_model` migration + sqlc +
  `/me/settings` read/write + claim-assembly resolution (user-wins, error
  fallback). `go test ./...` + `sqlc generate` clean.
- [ ] **M3 — Default judge model → opus**: `DefaultJudgeModel="opus"`, comment
  trail + settings tests + web mock defaults. `go test ./internal/settings`;
  `npm run typecheck` + `npm test`.
- [ ] **M5 — Per-user spend guards**: `judge_cooldown_seconds` +
  `judge_daily_budget` int settings + accessors, the two count queries (sqlc),
  Gate 5 in `maybeEnqueueJudge` (skip-not-defer, all modes). `go test
  ./internal/settings ./internal/workersvc` green, including a loop test (N rapid
  failures → judges throttled by cooldown, capped by budget).
- [x] **M6 — Judge run cost/time**: `JudgeRunner` posts its terminal result frame
  + one `running` report; `GetJudgeRunUsageForTarget` (sqlc); `reviewDTO` gains a
  nested `judge_run` (`judge_run_id` + claim/start/finish timing + `usage`);
  `JudgePanel` 4-tile strip (Tokens in · Tokens out · Duration · Cost), rendered only
  when usage is present (absent for a pre-feature judge, never a fabricated 0);
  `docs/judge.md`. Judge spend deliberately rolls into `SelfUsage`/`AdminUsage` (the
  `kind <> 'chat'` aggregates admit `judge`, no query change — Decision 10). Gates:
  `task gate:agent`, `task gate:api`, `task gate:web` + `cd web && npm run build` green;
  sqlc stable (only `judge.sql.go` regenerated). The `judge_run` field is nested (not the
  flat `judge_run_id`+timing the checklist sketched) to keep the timing/usage grouped
  under one omitempty key.
- [ ] **M7a — Judge accuracy (class signal + prompt rule + pre-start gate)**:
  closed-enum `failure_class` over the target run, computed from the trusted axes
  (`status` + `iteration_count` + transition origin, NOT parsed from `failure_reason` —
  Decision 11) on the judge claim + rendered in the trusted signal block (enum value
  only, no host token); `JUDGE_SYSTEM_PROMPT` transient≠permanent / no-retry-for-policy
  rule; pre-start-infra gate in `maybeEnqueueJudge` (`status='failed'` AND 0 iterations
  AND pre-start infra origin → skip judge → deterministic notification, idempotent);
  docs + specs. `go test ./internal/workersvc`, `cd agent && npm test` green, with a test
  proving a pre-start provisioning failure yields a non-"transient", non-retry
  recommendation, a 0-iteration infra failure is notified not judged, and an
  agent-crash-at-iteration-0 is still judged.
- [ ] **M7b — Egress-allowlist enrichment (DEFERRED)**: not built in this PRD. Requires
  a chart-derived `WORKER_EGRESS_ALLOW_FQDNS` env + a host source; low value because the
  durable fix (pin nixpkgs / pre-seed, issue #82) removes the underlying failure mode.
- [ ] **M4 — Consent surface + web + docs + specs**: `/me` effective fields,
  admin enforce toggle (with cost copy + greying) + the two spend-guard inputs,
  user judge card (enforced banner + per-user model input), stale-comment fix,
  docs for all four changes, `specs/ai.md` entries, release note, close #59.
  `npm run build` (check-docs) green.

## Success Criteria

- **Mode off**: `judge_enabled=false` → no judge enqueued for anyone, even with
  `enforce_all=true` (kill-switch wins).
- **Mode optional**: `judge_enabled=true`, `judge_enforce_all=false` → only
  users with their own `judge_enabled=true` get judged (unchanged).
- **Mode enforced**: `judge_enabled=true`, `judge_enforce_all=true` → every
  eligible run of every user *with an Anthropic token* is judged regardless of
  the per-user flag; token-less users are skipped silently; spend stays on each
  run owner's token.
- **Consent**: a non-admin under enforced mode sees, from the `/me` payload,
  that the judge is admin-enforced and which model it runs on, with the override
  reachable from the same card.
- **Per-user model**: a user who sets `judge_model` gets their judge claims
  assembled with that model; NULL inherits the instance value; an admin-set
  instance value is the fallback, not an override of a set user value; a
  user-row read error falls back to the instance value (logged), never an empty
  model.
- **Opus default**: fresh instance with the judge enabled and no `judge_model`
  set assembles judge claims with `opus`; an explicitly set instance value still
  wins; no UI/docs surface still calls haiku (or sonnet) the default.
- **Spend guards**: with `judge_cooldown_seconds=60`, a user whose runs fail
  faster than once per 60s gets at most one judge per 60s (the rest skipped);
  with `judge_daily_budget=N>0`, that user gets at most N judges per rolling 24h;
  `0`/`0` disables each guard; a skipped judge is silent and leaves no run.
- **Judge cost/time (M6)**: a completed judge run writes a `run_usage` row; its
  cost appears in the owner's usage totals (self + admin) and its time + tokens +
  cost render on the reviewed run's review panel as the 4-tile strip; a judge run
  predating the feature shows no strip (never a fabricated 0); the judge run
  itself remains hidden from every run list.
- **Judge accuracy (M7a)**: a pre-start provisioning failure is presented to the judge
  as a `provisioning_failed` class (computed from `status`+`iteration_count`+origin, in
  the trusted block, no raw reason text) and, with the prompt rule, does not yield a
  "transient / retry-with-backoff" recommendation; a run that failed at 0 iterations AND
  with a pre-start infra-class origin is not judged (no opus call) but produces a
  deterministic infra notification; an agent that started and crashed at iteration 0 is
  still judged. (M7b — the host/allowlist cross-check — is deferred and effectively dead;
  no criterion here.)

## Out of Scope

- A soft "default-on but user-opt-out" fourth mode (Decision 3 — deferrable at
  zero migration cost; add only if a real need appears).
- Admin cap/lock on the *per-user* judge model (own-token spend makes a ceiling
  unnecessary).
- **Cost/token-based spend guards and failure-signature dedup.** The M5 guards
  are count-based (cooldown + daily count budget). A budget denominated in
  `run_usage` dollars/tokens, and a "skip-after-N-identical-failure-signatures"
  refinement that suppresses only *repeated* failures while still judging diverse
  runs, are both deferrable improvements — the count-based guards bound the
  runaway case, and the surgical signature approach is materially harder. (M6
  makes each judge's actual cost available *after* the fact, so a cost-denominated
  budget becomes a smaller follow-up, but it stays out of scope here — the gate
  still runs at enqueue, before the about-to-run judge's cost is known.)
- Changing the judge **compaction** or the self-improvement engine.
  `self_improve` runs stay un-judged (allowlist, `judge_enqueue.go:19`) regardless
  of `enforce_all`. (M7 makes ONE targeted `JUDGE_SYSTEM_PROMPT` addition — the
  transient≠permanent / no-retry-for-policy rule — and adds a trusted failure-class
  signal; the broader prompt/compaction rework stays out.)
- Per-user override of the *agent-run* `default_model` (already exists, PRD #17)
  or making the agent default opus (the `lead` builtin is already `opus` —
  `api/internal/agenttmpl/builtins/lead.md:4`).
- The rate-limit probe model (`claude-haiku-4-5`,
  `api/internal/anthropic/client.go:55` — a deliberate cheapest-model choice,
  unrelated to the judge).
