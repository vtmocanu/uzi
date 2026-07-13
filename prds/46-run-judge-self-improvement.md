# PRD #46: Run retrospective (LLM judge) + self-improvement job

**GitLab Issue**: [#46](https://gitlab.example.com/vtmocanu/uzi/-/issues/46)
**Status**: Reviewed (design review + security audit + fact-check, 2026-07-12), ready to start
**Priority**: Medium
**Created**: 2026-07-12
**Depends on**: PRD #19 (app_settings), PRD #25 (Slack), PRD #32 (vault). Related: PRD #39 (chat run kind, in progress), PRD #40 (token usage, would enrich judge input), plan.md:64/69/91 (superseded by this PRD + `specs/human.md`)

## Problem

uzi never learns from its own runs. Nobody reviews how agents, tools, prompts, and
plans performed; missing worker tools surface only as buried `command not found`
tool errors; agent templates and repo agents drift without feedback; and
improvements to uzi itself depend on a human noticing them. plan.md queued this up
twice (line 91: LLM-judge session analysis; line 69: scheduled self-improvement)
with nowhere to live.

## Solution Overview

Two features in one PRD, phased — the judge ships first, the self-improvement job
second; they share the settings and inbox plumbing (user decision, 2026-07-12).

1. **Run retrospective (LLM judge)**: when enabled, every *finished* run is
   reviewed by an LLM on the **run owner's own Anthropic token**. The judge reads
   the run trace (agents, tools, prompts, plan, review cycles, delivery) and
   produces a verdict + structured recommendations: all good / enable an existing
   tool or skill / install a missing worker tool / adjust an agent template or
   prompt / improve an existing agent (including repo agents living in git) /
   propose a missing agent for a repo / improve uzi itself. Recommendation only —
   the judge never writes code.
2. **Notifications inbox**: a new in-app inbox (none exists today) — users see
   their own notifications, admins see all — plus delivery through the existing
   Slack notifier. The judge is the first tenant; the inbox is generic.
3. **Self-improvement scheduled job (admin-only)**: on a configurable interval
   (2-day default), an engine reviews uzi's own codebase plus accumulated
   "improve uzi" recommendations, picks **one top thing** (bug, feature, or whole
   refactor), and autonomously runs an agent team that delivers an MR — no
   approval gate, but the plan is inspectable. An already-open self-improvement
   MR is reused/extended so everything is tested together. Runs on the enabling
   admin's token, with room for a general/instance token later.

## Design Decisions

1. **The judge is a worker-executed run kind, not an API-side call.** The API
   never talks to Anthropic — the `@anthropic-ai/claude-agent-sdk` is imported
   only under `agent/src/`, and `api/` has no Anthropic HTTP client at all
   (every "anthropic" hit there is token custody); the OAuth token leaves the
   vault only inside a claim (`api/internal/workersvc/service.go:420-431` →
   `claim.go:114`). So "use the user's token" forces the judge onto the worker
   as a new `runs.kind = 'judge'` with PRD #39's slim-runner shape (no clone, no
   worktree, no push, no MR — a JudgeRunner that calls the model and returns
   structured output). **The repo-less-run machinery is load-bearing and owned
   by this PRD's M1** (review B1, 2026-07-12) — `ci_fix` is NOT a repo-less
   precedent (ci_fix runs clone): `runs.repo_id` is `NOT NULL` (`00020:32`),
   `GetRunClaimContext` INNER-JOINs runs→repos→forge_connections
   (`runtime.sql:185-188`), and `assembleClaim` unconditionally opens the bot
   PAT. M1 therefore includes: `repo_id DROP NOT NULL`, the `runs_kind_shape`
   rework (`judge` ⇒ repo/issue NULL + `target_run_id` NOT NULL), a forked
   claim-assembly branch (no repo/connection join, Anthropic-token-only, wire
   assertion that no `forge_pat` is present), and a NULL-`repo_id` audit of
   every runs query/DTO. PRD #39 (Draft) needs the same relaxation for `chat`
   runs — whichever lands first carries it; this PRD does not assume #39 lands
   first. Rejected: an API-side Anthropic client — new credential path, new
   egress, breaks the "only workers spend user tokens" custody line.

2. **Trigger on the committed terminal transition, not the notify goroutine.**
   The lifecycle notify seam (`runlifecycle/lifecycle.go:189`) is explicitly
   best-effort and may drop events — acceptable for comments, not for deciding
   token spend (review N2, 2026-07-12). Enqueue happens where the terminal
   state is *committed*: worker-reported `SetState` (rows>0), the sweeper's
   terminal sweeps, and server-side cancel/reject — deliberately including
   sweeper-failed (timed-out/worker-lost) runs, which are exactly the runs
   worth judging. Judged terminal statuses: **completed and failed only** —
   user-cancelled runs are not judged (the user aborted; don't spend on it;
   review N4). When a run of an
   eligible kind reaches a terminal state and (global toggle on + owner opted in
   + owner has an Anthropic token), the API enqueues a `judge` run targeting it.
   The judge run is owned by the same user as the judged run — never cross-user.
   Eligibility is an explicit **allowlist** `{issue, ci_fix}`, never a denylist —
   a future kind must opt in deliberately, and `judge`/`self_improve` stay
   outside it (no recursion, no self-feeding improvement loop; audit M4,
   2026-07-12). Mid-run judging is deferred (user decision, 2026-07-12).

3. **The worker fetches the trace; the claim stays small.** A judged run's trace
   is `run_messages` (`00020_workers_runs.sql:70-79`: gapless `seq`, `kind`
   incl. `tool_use`/`tool_result`, `agent`, `payload jsonb`) plus `plan_md`,
   `run_user_inputs` (steering log), iteration count, MR/pipeline outcome, and
   the `repo_agents` roster snapshot (`00052_run_agent_selection.sql`). That can
   be megabytes, so the claim carries only `target_run_id`; the worker pulls the
   trace through a new Bearer-authenticated worker endpoint
   (`GET /api/worker/runs/{id}/trace`, paginated via the existing
   `ListRunMessagesAfter` replay shape, `runtime.sql:382`). Authorization is
   **judge-run-scoped, not user-scoped** (audit H1, 2026-07-12): the existing
   `GetRunOwnedByWorker` pattern doesn't fit (the worker owns the *judge* run;
   `{id}` is the *target*), so the endpoint derives the target server-side —
   caller's worker must own a non-terminal `kind='judge'` run, its
   `target_run_id` must equal `{id}`, and `target.user_id == judge.user_id` is
   re-asserted independently (the enqueue invariant is necessary but not
   sufficient). Plain user-scoping is explicitly rejected (it would let any of a
   user's workers stream any of their traces at will). The endpoint also enforces
   the page/size budget server-side so a pathological run can't be replayed
   wholesale (audit L2). The judge claim carries **only** the Anthropic token —
   no ForgePAT, no forge-connection dependency: `assembleClaim`'s unconditional
   PAT open (`service.go:396`) is skipped for the judge lane, which both honors
   least privilege and keeps a judge from spuriously failing when the target's
   forge connection is gone (audit H2). Token/cost data joins the input when
   PRD #40 lands; until then the judge sees what the trace shows.

4. **The deterministic `command not found` scan is API-side input, not judge
   work** (plan.md:64). When enqueueing a judge run, the API scans the target's
   `tool_result` payloads for command-not-found / executable-missing patterns and
   passes the hit list in the claim as a structured signal. The judge interprets
   (which tool, which agent needed it); the regex never decides anything alone.
   This scan is also the fallback content when the judge itself fails: the
   deterministic findings still land in the inbox.

5. **Recommendations are structured rows, not prose blobs — and they are
   untrusted data.** New table `run_reviews` (one per judged run: verdict enum
   `ideal | ok | issues`, summary md, judge model, status) +
   `review_recommendations` (category enum: `enable_tool | install_worker_tool |
   adjust_template | improve_agent | add_agent | improve_uzi`, target
   (agent/tool/repo name), rationale md, confidence, **provenance**: producing
   run + user). The worker posts them through a new worker endpoint at judge-run
   completion (persist-first, then notify — same ordering discipline as
   `run_messages`). The review POST validates hard (audit C1/L4, 2026-07-12):
   category against the enum, length caps + control-char strip on
   target/rationale/summary (the `sanitizeSelfReported` pattern,
   `handler/worker_protocol.go:31`), and free text scrubbed through the Slack
   `ScrubSecrets` families before persistence — the worker is a user-controlled
   container, so these rows are attacker-suppliable regardless of any prompt
   hardening. The judge prompt additionally instructs never to quote raw
   file/command output verbatim, since the run-message redactor only scrubs the
   run's *own* secrets, not third-party secrets appearing in tool output (audit
   M1). Categories are the user's taxonomy verbatim (`specs/human.md`,
   2026-07-12) including agent improvement / missing-agent proposals for repo
   agents living in git. `improve_uzi` rows additionally feed the
   self-improvement job (decision 10).

6. **Inbox = new `notifications` table + bell surface; judge is tenant #1.**
   No in-app notification store exists (the WS hub is per-run; the activity feed
   is a per-run rendering of `run_messages`, "web-only" per PRD #38). New table
   `notifications` (user_id, kind, payload jsonb, run/review refs, read_at),
   REST list/mark-read, a bell + inbox page in the SPA; users see their own,
   admins see all (admin view includes owner). Scoping reuses the
   owner-or-admin `GetRunForViewer` pattern (`workersvc/service.go:1078`,
   non-owner → not-found): list is session-user-scoped, `?all=1` requires
   admin, mark-read verifies row ownership (audit M2). Pagination, a per-user
   cap, and retention/pruning ship with the table, not later — every finished
   run of every opted-in user writes here (audit M5). Slack delivery rides the
   existing notifier (`slacksvc/notifier.go:75`, per-user gating via
   `slack_notify` already in place) — and this is a real notifier extension,
   not a new string: the notifier is structurally run-state-only
   (`stateEvent{runID,status}` → `GetSlackRunContext` → "run on repo#iid"
   rendering), so judge/self-improvement notifications need a new event variant
   + render path (review N6, 2026-07-12). Conversely, judge and `self_improve`
   runs' own state transitions are **suppressed** from the run-state Slack path
   (no "judge run completed" DM noise; `GetSlackRunContext` would also error on
   a repo-less judge run). Every judge free-text field passes `EscapeMrkdwn` +
   `ScrubSecrets` (`slacksvc/redact.go:18,:45`) like existing untrusted dynamic
   fields (audit M1): inbox + Slack both, per user decision 2026-07-12. Realtime nudge via
   the existing WS hub is nice-to-have, not required for v1 (polling the badge
   on navigation is acceptable).

7. **Toggles follow the established two-level pattern.** Global kill-switch
   `judge_enabled` in `app_settings` (the PRD #19 infra whose migration comment
   reserved exactly this kind of key, `00036_app_settings.sql:8`) + per-user
   opt-in column `users.judge_enabled` (default **false**, opt-in — it spends
   the user's tokens) modeled on `autopilot_enabled` (`00037:28`, toggle route
   pattern `handler.go:239`). `PUT /api/me/judge` flips only the **session
   user's** flag — identity from the session, never the body, so nobody can opt
   another user into spending their tokens (audit H3). Admins can toggle any
   user's flag from the admin users surface (actor authorized via
   `RequireAdmin`, target from the path) — that covers "force-disable per
   user". Judge model: `app_settings` key `judge_model`, cheap default (haiku
   alias), reusing the model-alias validation from the worker-model work
   (PRD #17).

8. **Judge cost discipline.** One judge run per target run: enforced **at
   enqueue** by a partial unique index on `runs(target_run_id) WHERE
   kind='judge' AND status non-terminal` — the `run_reviews`
   UNIQUE(target_run_id) alone would fire only *after* a duplicate judge has
   already spent tokens (review N3). Re-judge is an explicit "re-run judge"
   action behind the per-user limiter pattern (`handler.go:369`); its review
   post is an UPSERT (replace semantics), otherwise the second review 23505s
   against the unique row (review N3; audit M3). `target_run_id` is FK
   `ON DELETE CASCADE` — deleting the judged run (e.g. repo disconnect cascades
   runs away) takes the judge run and review with it. The API-side
   command-not-found scan caps the bytes it inspects and runs off the hot
   request path (audit L1). Note on latency: today's worker runs one run at a
   time (`agent/src/worker.ts:58`), so a judge run may wait behind a long issue
   run — acceptable v1; PRD #42 (worker concurrency) would lift it and should
   account for judge runs if it lands (review N9). Judge runs claim through the
   normal
   queue and count toward worker capacity; they are small (one model
   round-trip over a compacted trace — the JudgeRunner truncates/samples the
   trace to a budget rather than shipping 100k-message pathologies verbatim).
   If the owner's vault is locked the judge run waits `queued` like any run
   (existing "waiting for vault unlock" semantics); a stale judge run is swept
   by the existing sweeper (no special lifecycle).

9. **Self-improvement engine = privcheck-shaped scheduler.** A new
   `selfimprove.Engine` cloned from the privilege-check sweep pattern
   (`privcheck/sweep.go:27` Boot + interval ticker, `0` disables, wired in
   `main.go`): on tick, if enabled and due, it creates the improvement run.
   Unlike privcheck's idempotent sweep, a tick is NOT idempotent, so "due" is
   durable — a persisted `selfimprove_last_run_at` setting, not an in-memory
   ticker that resets on every API restart (review B3, 2026-07-12). A tick
   skips (with an inbox notification, no silent stall) when: a self-improvement
   run is still active (also enforced by a partial unique index on `runs WHERE
   kind='self_improve' AND status non-terminal` — Boot re-runs and future
   multi-replica must not double-create onto the same fixed branch; review B3),
   the admin's vault is locked, or `selfimprove_repo` is disconnected / not
   owned by the enabling admin (review N5).
   Settings in `app_settings`: `selfimprove_enabled`, `selfimprove_interval`
   (default 48h), `selfimprove_repo` (the connected uzi repo),
   `selfimprove_last_run_at`, and the enabling
   admin's user id (`selfimprove_user_id`). Each admin can enable the job with
   *their* token — the enabling admin is the run owner, and
   `selfimprove_user_id` is always set to the **authenticated session admin**,
   never accepted from the request body (otherwise admin A schedules autonomous
   spend on admin B's token; audit H3). The consent copy states the standing
   nature explicitly: the vault DEK stays cached while the process lives, so
   the job runs on the admin's token during any logged-in window and produces
   autonomous code changes — not a one-time spend (audit L3). The token-source
   is behind a narrow accessor so a general/instance token can slot in later
   without redesign (user decision, 2026-07-12). If the admin's vault is locked
   at tick time, the tick is skipped and a notification says so (no silent
   stall).

10. **The improvement run is a normal, autonomous run against the uzi repo —
    guardrails fully intact.** The engine files (or reuses) a tracking issue on
    the uzi project, then creates an auto-approved run (autopilot-style
    `auto_approve`, `service.go:496` / claim `claim.go:39-45`) of kind
    `self_improve` (issue-shaped: full clone→plan→implement⇄review→MR pipeline).
    Creation goes through a **dedicated method shaped like `CreateCIFixRun`**
    (`workersvc/ci_fix.go:122`), not `createRun` — the normal path requires the
    issue to already be in the poller cache and to carry a PRD link, and a
    just-filed tracking issue satisfies neither (review B2, 2026-07-12); the
    engine snapshots title/description directly. The tracking issue must NOT
    carry the PRD/autopilot trigger labels — otherwise the poller enqueues a
    second, `kind='issue'` autopilot run on the same issue that the kind-scoped
    one-active index would not dedupe (review N1). Known side effects, accepted:
    the run flows through `runlifecycle`, so board-column moves and the
    auto-approve terminal comment land on the tracking issue (review N1).
    Its planning prompt gets: the accumulated unaddressed `improve_uzi`
    recommendations + repo state, and must pick **one top thing** — bug, feature,
    or whole refactor — not a list (prompt-enforced; user decision 2026-07-12:
    "iterate and self improve"). No approval gate blocks it, but `plan_md` is
    stored on the run like every run, so the plan is inspectable in the run view
    and linked from the notification (user decision, 2026-07-12). MR reuse is
    free: a fixed branch name (e.g. `uzi/self-improve`) + the worker's idempotent
    `createMergeRequest` (`agent/src/gitlab.ts:76`, reuses an existing MR for the
    branch; `git.ts:71 pushBranch` never forces) means an open self-improvement
    MR gets extended, everything tested together. The primary directive is
    untouched: Developer-role bot PAT, protected `main`, `guardrails.ts` deny
    hook, `settingSources: []` — a run on uzi's own repo is not special.
    **Injection/self-modification fences (audit C1, 2026-07-12)** — these gate
    the *merge* and fence the *input*, preserving the autonomous run:
    - The recommendations block in the planning prompt is wrapped in the
      existing untrusted-data framing (`UNTRUSTED_FRAME`, `agent/src/prompt.ts:16`)
      like issue fields and ci_fix evidence — recommendation text is LLM output
      over untrusted traces and potentially worker-forged; it is data, never
      instructions.
    - **Mandatory human merge**: the bot must not be able to merge to `main`
      (protected-branch merge rights = humans only), verified as part of setup
      docs and covered by the privilege-check sweep for the self-improve
      connection (audit M6). Never auto-merge — explicit success criterion.
    - A `self_improve` run cannot weaken its guardrails *at runtime* (the
      worker executes its own compiled `guardrails.ts` with
      `settingSources: []`; the checked-out copy never loads) — the risk is
      the merged, later-deployed artifact. Accordingly, MRs from this job that
      touch guard-critical paths (`agent/src/guardrails.ts`, auth middleware,
      `secretbox`, `vault`, `workersvc` claim/token paths, compose secret
      wiring) are flagged loudly in the MR description + notification for
      extra-careful human review (there is no CI or CODEOWNERS on this repo).
    - The runner runs the test suites (`go test ./...`, `npm test`,
      `npm run build`) and surfaces pass/fail in the MR description — with no
      CI, an autonomous MR must carry its own evidence.

11. **The job consumes judge output but does not require it.** Recommendation
    rows of category `improve_uzi` are marked `addressed_by` when an improvement
    run picks them up; the engine works fine with zero recommendations (pure
    code review). The judge recommends; only the job acts (user decision,
    2026-07-12).

12. **Migrations were draft-numbered `00080+`** during design to stay clear of
    ranges other open PRDs held (#45 drafts 00053, #39 00065, #41 00070, #42
    00075; live head was `00052` — PRD #37 — at draft time). At landing they were
    renumbered to the live head per convention: contiguous above PRD #45's OIDC
    head `00056`, as `00057`–`00060`.

## Technical Design

### API (api/)

- Migrations (landed `00057`–`00060`, contiguous above the OIDC head `00056`): `00057` `runs.repo_id`/`issue_iid` DROP NOT NULL + extend
  `runs.kind` CHECK + rework `runs_kind_shape` (`judge` ⇒ repo/issue NULL,
  `target_run_id` NOT NULL FK ON DELETE CASCADE; `self_improve` ⇒ issue-shaped)
  + partial unique indexes (one non-terminal judge per target; one non-terminal
  self_improve); `00058` `run_reviews` + `review_recommendations` (provenance
  cols); `00059` `notifications`; `00060` `users.judge_enabled`. sqlc regen +
  NULL-`repo_id` audit of every runs query/DTO.
- `workersvc`: enqueue-judge at committed terminal transitions (SetState,
  sweeper, server-side cancel paths); forked claim-assembly branch for `judge`
  (no repo/connection join, no PAT — target id + command-not-found signal +
  judge model only); dedicated `CreateSelfImproveRun` (CreateCIFixRun-shaped);
  worker endpoints `GET /api/worker/runs/{id}/trace` (judge-run-scoped authz,
  paginated, server-enforced budget) and `POST /api/worker/runs/{id}/review`
  (validate + scrub + UPSERT).
- `settings`: keys `judge_enabled`, `judge_model`, `selfimprove_enabled`,
  `selfimprove_interval`, `selfimprove_repo`, `selfimprove_user_id` + typed
  accessors + validation.
- `handler`: user toggle (`PUT /api/me/judge`), admin per-user toggle,
  notifications API (`GET /api/notifications`, `POST /api/notifications/{id}/read`,
  admin `?all=1`), re-run-judge action, run-view review payload.
- New `api/internal/selfimprove` engine (privcheck-shaped), wired in `main.go`.
- Slack: new notifier event ("judge review ready", "self-improvement MR
  opened/updated", "self-improvement tick skipped: vault locked").

### Worker (agent/)

- Claim-lane awareness of the two kinds (shares PRD #39's claim-shape work if it
  lands first; otherwise a minimal kind switch in `worker.ts`).
- `JudgeRunner`: fetch trace → compact to budget → single structured-output
  model call (judge model from claim) → post review. No git, no MR, no
  guardrail changes (the deny hook still applies).
- `SelfImproveRunner` = the existing issue runner with a fixed branch name and
  the recommendations block in the planning prompt (delta over `runner.ts`, not
  a fork).

### Web (web/)

- Bell + inbox page (users: own; admins: all), unread badge.
- Run page: verdict chip + recommendations panel + "re-run judge".
- Settings: user judge toggle; AdminSettings: global judge toggle, judge model,
  self-improvement enable/interval/repo (+ "uses your token" consent copy for
  the enabling admin).
- `mockApi` parity for the new endpoints.

### Docs + specs

- New `docs/judge.md` (`audience: user`, order 95): what the judge is, token
  cost, opt-in, reading recommendations, re-run.
- New `docs/self-improvement.md` (`audience: user`, order 75, admin-focused):
  enabling, schedule, token consent, MR reuse, inspecting plans, break-glass
  (disable + close MR).
- Updates: `docs/admin-settings.md`, `docs/slack.md`, `docs/worker-model.md`
  (judge model note), `docs/configuration.md` if any env surfaces.
- `specs/ai.md` decision entries; `specs/human.md` already carries the
  requirements (2026-07-12).

## Milestones

- [x] **M1 — Schema + settings groundwork**: migrations (nullable
      `repo_id`/`issue_iid` + kind-shape rework + partial unique indexes,
      `run_reviews`, `review_recommendations`, `notifications`,
      `users.judge_enabled`), sqlc + NULL-`repo_id` query/DTO audit, forked
      judge claim assembly (no-PAT wire assertion), settings
      keys/accessors/validation, user + admin toggle endpoints and UI switches.
      Tests for gating logic.
- [x] **M2 — Notifications inbox (generic)**: notifications API, bell + inbox
      page (user/admin views), Slack notifier event kind. Judge-independent;
      seeded via a test event.
- [x] **M3 — Judge end-to-end**: terminal-funnel enqueue with all gates,
      command-not-found scan, judge claim lane, worker trace fetch + JudgeRunner
      + review post, `run_reviews` persisted. Validated with the stub executor /
      fake trace.
- [x] **M4 — Judge surfacing**: run-page verdict + recommendations panel,
      re-run action, inbox + Slack delivery wired to review completion.
- [x] **M5 — Self-improvement engine**: settings (incl. durable
      `selfimprove_last_run_at`), privcheck-shaped scheduler with
      skip-if-active / vault-locked / repo-missing tick skips (+ notifications),
      tracking-issue reuse (no trigger labels), dedicated
      `CreateSelfImproveRun`, fixed-branch MR reuse, untrusted-framed
      `improve_uzi` recommendations folded into the planning prompt and marked
      addressed, guard-path flagging + test evidence in the MR.
- [x] **M6 — Tests green**: Go + web + agent suites covering the gating matrix
      (global off / user off / no token / vault locked / non-eligible kind / no
      recursion), trace-endpoint authz (wrong worker, terminal judge run,
      target/judge owner mismatch), review-POST validation (bad enum, oversize,
      control chars, secret-family scrub), toggle authz (body-supplied ids
      ignored), inbox visibility (user vs admin, cross-user mark-read), engine
      tick logic, MR-reuse path. `go test ./...`, `npm test` (web + agent).
- [x] **M7 — Docs + specs**: `docs/judge.md`, `docs/self-improvement.md`,
      updated pages, `specs/ai.md`; `npm run build` (check-docs) green.
      (fact-checked clean; `specs/ai.md` §195-204 added, `specs/human.md`
      verified complete.)
- [ ] **M8 — Live validation** (stub-validated GREEN; live capstone
      DEFERRED-TO-OPERATOR): judge path end-to-end passed on the isolated
      stub stack (`run-e2e.sh` PRD #46 section: funnel enqueue →
      repo-less/no-PAT claim → trace → review → persist-first notification →
      re-run-judge → `install_worker_tool jq` via the deterministic fallback,
      UPSERT to one row) — `./e2e/run-e2e.sh` 119/0 GREEN on the rebuilt image
      (incl. the baked hooksPath dir); MR-reuse + no-force-push proven against
      the forge-fake; bot=Developer / `main` Maintainers-only verified on the
      LIVE forge; nonce fence held 20/20. **Remaining, operator-only** (needs a
      real Anthropic token + scratch repo — must NOT be sourced by the team):
      a genuinely live LLM judge verdict, and one live self-improvement tick on
      a scratch repo.
- [x] **M9 — Real test evidence in self-improvement MRs**: thread the PRD #18
      provisioned tool env (`provision.ts` → `provision-run.ts` `toolEnv`,
      already exported into the SDK env) into `defaultCheckRunner`'s
      `execFile` (today it passes no env at all, so provisioned tools are
      invisible to the checks); `npm ci` pre-step in `web/` and `agent/` so
      `vitest`/`tsc` exist; honest-skip fallback retained for genuinely
      unavailable toolchains (missing prerequisite or exit 127 → `skipped`
      with a reason, never a false `failed`). Gating unknowns: whether `go`
      is provisionable via the run's tool packages, and whether the worker
      has npm-registry egress — whatever cannot be made real stays honestly
      skipped and is stated as such in the MR.
- [x] **M10 — Harden worker git env (pre-existing PAT-exfil vector)**: surfaced by the M9
      audit sweep, PoC-confirmed. `gitEnv` → scrubbed replacement env (worker
      join-token / API vars absent by construction; credential pairs
      preserved); `core.hooksPath` pinned to a ROOT-OWNED mode-0555 dir baked
      at image build (base template) so a non-root uid cannot plant a hook —
      PoC-confirmed ("Permission denied") + e2e-green; `filter.` added to the
      guardrail config-write deny. Auditor cleared (blocking finding resolved),
      reviewer APPROVE. Both worker templates (base + jvm) bake the stanza
      explicitly (jvm does NOT `FROM base`); reviewer confirmed af267db clean.

## Milestone dependency / parallelization

| Phase | Milestones | Depends on | Files touched |
|---|---|---|---|
| 1 | M1 | — | migrations, sqlc, settings, toggles |
| 2 (parallel) | M2, M3 | M1 | notifications API/UI/Slack · workersvc + agent runner |
| 3 | M4 | M2+M3 | run page, notifier wiring |
| 4 | M5 | M1 (M2 for its notifications) | selfimprove engine, runner delta |
| 5 | M6, M7 | M3–M5 | tests · docs |
| 6 | M8 | all | live stack |
| 7 | M9 | M8 | agent/src/{self-improve,runner}.ts, provision plumbing |

## Out of Scope

- Mid-run judging (finished runs only in v1).
- Judging `chat` runs, judge runs (recursion), or cross-user judging.
- Auto-acting on judge recommendations (only the self-improvement job acts, and
  only on `improve_uzi`; tool installs / template edits stay human-applied).
- A general/instance Anthropic token (accessor is shaped for it; implementing it
  is its own PRD).
- Self-improvement against repos other than uzi's own.
- Non-Anthropic judge models; per-user judge model override.
- Token/cost analytics in the judge input (arrives with PRD #40).
- WS-pushed realtime inbox updates (badge refresh on navigation suffices v1).

## Success Criteria

- With the feature on and a user opted in, finishing a run produces a verdict +
  recommendations on the run page, in the inbox, and on Slack, billed to that
  user's token; with any gate off, nothing fires and no tokens are spent.
- A run whose trace contains `command not found` yields an
  `install_worker_tool` recommendation naming the tool even if the LLM call
  fails (deterministic fallback).
- Judge recommendations can name agent-level fixes: improve a specific repo
  agent file or propose a missing agent for the repo.
- The self-improvement job, once enabled by an admin, autonomously produces (or
  extends) exactly one open MR on the uzi repo per cycle, with an inspectable
  plan, picking one top item — and never touches `main` (all four guardrail
  layers verified untouched). The MR is **never auto-merged**: the bot lacks
  merge rights on `main`, a human merges, and MRs touching guard-critical paths
  are flagged for extra review. Each MR carries its own test-suite pass/fail
  evidence.
- A forged or injected recommendation (hostile worker POST, malicious trace
  content) cannot become instructions: it is enum/length-validated, scrubbed,
  provenance-labeled at ingest, and rendered inside untrusted-data framing in
  the self-improvement planning prompt.
- Disabling either feature stops all related token spend immediately; the whole
  PRD is dormant when toggles are off (existing tests unaffected).

## Implementation notes (2026-07-12)

Approved deviations from the design above, found during implementation:

1. **The command-not-found scan runs at claim assembly, not at enqueue.**
   Decision 4 describes it as part of enqueueing the judge run; in practice it
   runs off that hot request path, at claim assembly (`assembleJudgeClaim` /
   `judgeSignal`, `api/internal/workersvc/judge.go`) — a separate worker poll,
   not the transition that creates the judge run. Accepted trade-off: a judge
   run that is never claimed (e.g. no worker online) yields no deterministic
   findings, only whatever the LLM call itself produces once it does run.
2. **Premise correction on migration numbering (Decision 12).** The PRD
   assumed the live head was `00052` (PRD #37). By the time this PRD landed,
   `issue_iid` was already nullable (`00043`, PRD #6's `ci_fix` runs) and
   `repo_id` was already nullable (`00053`, PRD #39's chat runs, which needed
   a repo-less run shape first). This PRD's M1 therefore *extended* those two
   already-relaxed columns into the repo-less `judge` shape and the `judge`/
   `self_improve` `runs_kind_shape` branches (`00057`), rather than dropping
   either NOT NULL itself — and the actual live head at this PRD's landing was
   `00055`, not `00052`. Migration numbers in a PRD draft are always
   collision-avoidance placeholders, renumbered to the real live head at
   landing (per `CLAUDE.md`'s migration-numbering convention) — this is a
   correction to the draft's stated *premise* (which prior PRDs already did
   the relaxing), not a new instance of the normal renumbering.
3. **Re-run judge needs no per-user opt-in.** Decision 7's per-user
   `judge_enabled` opt-in gates the *automatic* judge (Decision 2's
   terminal-funnel enqueue). The owner-initiated "re-run judge" action
   (Decision 8) does not additionally require that flag: an explicit,
   owner-only click is itself the consent to spend that run. It still runs
   behind its own dedicated per-user rate limiter (`JUDGE_RATE_LIMIT_MAX`/
   `_WINDOW`), separate from the chat limiter.
4. **Notification payload contents, made explicit.** The judge "review ready"
   notification (inbox row + Slack DM) carries the verdict, a scrubbed and
   280-rune-capped summary preview, and the recommendation count plus
   category list. Recommendation `target` and `rationale` free text are never
   copied into the notification — they stay on the run page behind the deep
   link (`buildReviewNotification`, `api/internal/handler/judge_worker.go`).
5. **The two Slack suppressions have different mechanisms.** Decision 6 says
   judge and self_improve runs' own state transitions are suppressed from the
   run-state Slack path. In the implementation this is structural for judge
   runs (they're repo-less, so `GetSlackRunContext`'s INNER JOIN on repos
   returns `ErrNoRows` before any suppression logic runs) but requires an
   explicit `rc.Kind == "judge" || rc.Kind == "self_improve"` guard for
   self_improve runs, which *are* repo-ful and would otherwise get a run-state
   DM (`Notifier.handle`, `api/internal/slacksvc/notifier.go`).
6. **M8 found the self-improvement MR's test evidence is false, not just
   missing (M9 added to fix it).** Decision 10's "the runner runs the test
   suites and surfaces pass/fail in the MR description" assumed the checks
   would either pass or genuinely fail. Live validation found a third case:
   the worker image has no Go toolchain, and a fresh clone has no
   `node_modules`, so `npm test` in `web`/`agent` exits 127
   ("vitest: command not found") — which `defaultCheckRunner` maps to
   `failed`, not `skipped`. A real self-improvement MR would therefore carry
   four false "failed" checks on perfectly good code, directly contradicting
   both `self-improve.ts`'s own contract ("a check that cannot run is
   reported skipped, never failed") and this PRD's success criterion that
   "each MR carries its own test-suite pass/fail evidence" — the only signal
   a human reviewer gets, since this repo has no CI. The fix is not a
   worker-image change: PRD #18's devbox tool provisioning
   (`provision.ts`/`provision-run.ts`) already runs per-claim and its
   `toolEnv` is already merged into the agent's own SDK env
   (`sdk-env.ts`/`sdk-executor.ts`); the gap is that `defaultCheckRunner`'s
   `execFile` call never receives that `toolEnv`, so provisioned tools are
   invisible to the checks even when provisioning succeeded. User approved
   landing the fix in this PRD as M9 rather than deferring it to a follow-up.
   **M9's real evidence is CONDITIONAL, not automatic:** `go`/`nodejs` reach the
   worker only when the connected uzi repo's `repo_tool_profiles` provisions them
   (they are on the seeded `tool_allowlist`, `00046`, but an empty profile
   provisions nothing). So a check produces real pass/fail only when its toolchain
   is provisioned; otherwise it stays honestly *skipped* (M8's fallback), never a
   false pass or fail. Operator step to get real evidence for every suite: add
   `go` + `nodejs` to the uzi repo's tool profile. `npm ci` runs `--ignore-scripts`
   and stays best-effort, so a hardened deploy that firewalls registry egress
   degrades to honest-skip rather than failing.
7. **M9 threading the toolEnv reopened, then closed, a token-exfil residual —
   and M5 had silently widened it.** Making the checks actually run means the
   worker executes model-authored code (the change's own test files,
   `package.json` scripts) as the worker uid, outside the SDK hook system. The
   M9 audit found `defaultCheckRunner`'s `execFile` had no `env`, so it would
   inherit the worker's full environment — the join token
   (`UZI_WORKER_TOKEN[_FILE]`) + `UZI_API_URL` → worker impersonation → claim →
   forge PAT + Anthropic token. M9 gives the check subprocess a **scrubbed
   replacement env** (`buildCheckEnv`, the `provision.ts` discipline: those vars
   absent by construction) and runs `npm ci --ignore-scripts` to drop the
   lifecycle-script entry path. Residual accepted for the MVP (documented loudly
   in `self-improve.ts` + `docs/self-improvement.md`): the subprocess still runs
   as the worker uid and the join-token *file* stays same-uid readable at a fixed
   path — the SAME residual class `provision.ts` documents for nix build hooks,
   but M5 widened it from admin-*vetted* packages to *model-authored* code
   without calling it out. Structural close = the k8s uid-split (agent under a
   distinct uid), deferred to the remote-worker phase.
8. **M10 — a pre-existing PAT-exfil vector in the git path, fixed in this
   branch.** The same audit sweep found `agent/src/git.ts` `gitEnv` built
   `{...process.env, …}` (full spread, leaking the join token + API URL) AND
   injected the forge PAT as `GIT_CONFIG_VALUE_n`; because `pushBranch` runs with
   `cwd=<bare>`, a git hook at the default path (`<bare>/hooks/pre-push`) fires
   as a worker child — outside the SDK hook system — inheriting both. Affects
   every run kind, not just self_improve. Fixed here (not caused by this PRD, but
   folded in per user decision): `gitEnv` → replacement env (worker/API vars
   absent by construction; `safe.directory` + the credential pairs preserved),
   `core.hooksPath` pinned on every invocation to a **root-owned `0555` empty
   dir baked into the worker image** (`agent/templates/base/Dockerfile`) — NOT a
   runtime-created dir, which would sit under the shared uzi uid and be
   agent-writable (that relocates the vector; audit follow-up). The uzi uid
   cannot create a hook inside a root-owned `0555` dir, so no planted hook can
   fire. `filter.` added to the guardrails
   git-config-write deny list (a second code-exec route). M9 + M10 together bring
   every worker subprocess (SDK, provision, checks, git) onto the
   replacement-env discipline.
