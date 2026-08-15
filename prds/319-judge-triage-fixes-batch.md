# PRD #319: Judge-triage fixes batch — Cf-strip untrusted Markdown, env-read allowlist, spec-keeper exclusion notification, lead prompt nudges

**Status**: draft
**Priority**: Medium
**Issue**: #319
**Delivery**: one bundled MR (Auto send-to-uzi; a human approves the plan gate and merges).

## Provenance

This PRD bundles fixes accepted during a `/judge-triage` pass over the `improve_uzi`
judge backlog (2026-08-15), each verified against the tree and then critiqued by two
reviewers before this draft. Anchors in **Touchpoints** are the verified locations **as of
that audit** — strong pointers, but the worker MUST re-derive the live location from the
quoted string/symbol (line numbers drift), not trust the number.

Everything here is an internal codebase change. **No open-web access is required** — the
worker clones the repo (forge egress) and re-derives every anchor locally. No milestone
depends on the open internet.

**One milestone is handled host-side, outside this MR.** The judge rec "review dispatch
worktree evidence" (a worktree-prune convention for the dev-team `.claude/agents/` roster)
is **not** in this worker MR: those files carry a generic body synced from the upstream
`vtmocanu/skills` role library, which the worker cannot reach and whose edits the sync would
revert. It is handled by the host session directly (repo-local, under each role's
`## For this repo (uzi)` section). See **Out of scope**.

## Problem

Four `improve_uzi` judge recommendations, plus one incidental security finding surfaced
during triage, each ask for a small, independent fix. They share no code and land together
only because separate MRs would be more ceremony than value:

1. **Untrusted `<Markdown>` sinks render without Unicode Cf/bidi stripping.** The judge
   review surfaces already strip control/bidi characters (`stripUnsafeChars`), but `plan_md`
   at the approval gate — the surface where a spoofed plan title is most dangerous — and
   several other untrusted sinks render raw. This is the Trojan-Source / bidi-spoofing class
   (cf. issue #124), on the surfaces an attacker can shape. **This finding resolves no judge
   rec** (see D1); it is the higher-value half of what triage found and is small.
2. **The tidy `printenv VAR` spelling of a diagnostic env read is blocked.** A run whose
   subject is PATH/environment propagation cannot read even a non-secret diagnostic variable
   with `printenv PATH`; it falls back to `echo "$PATH"` (which *is* allowed) or to
   inferring env facts from `$PATH` string-matching. Ergonomics, not capability (see D2).
3. **Steering can silently drop the `spec-keeper` guard role.** A run can exclude
   `spec-keeper` (the agent that guards `specs/human.md` / `specs/ai.md`) via
   `--exclude-agents` with no signal to the owner, even though that is the one change type
   the role exists to catch. There is no heads-up.
4. **The lead plan prompt re-verifies work it already confirmed**, and does not preserve
   exact literal tokens (e.g. NBSP vs ASCII space) from a PRD's technical scope, letting a
   character-level divergence slip into an approved plan.

## Solution Overview

One MR, four milestones, each independently testable and disjoint in files:

- **M1 (web, security)** — Strip Unicode Cf/bidi from every untrusted `<Markdown>` sink by
  **centralizing** the strip inside the `Markdown` component (reusing the existing
  `stripUnsafeChars` helper), so every sink — current and future — is covered by
  construction. Add a render test. Sweep the now-stale per-site comments.
- **M2 (agent)** — Allowlist `PATH` and `TMPDIR` for the `env`/`printenv` guardrail so a
  diagnostic read spelled `printenv PATH` is answerable, while bare `env`/`printenv`
  (enumeration) and any other/secret-bearing variable stay denied. Guardrail tests + ADR
  `0319` recording the decision.
- **M3 (api)** — When a run's approved selection **explicitly excludes a guard role**
  (`spec-keeper` today), emit a **notification** to the owner via the existing `notifysvc`,
  which surfaces in the in-app notification center **and** as a Slack DM (best-effort) in one
  call. No new wire field, no web-render change, no CLI/docs surface. Tests on the emit path.
- **M4 (builtins/lead.md)** — Two prompt nudges: reserve independent-verifier fan-out for
  genuinely uncertain or post-implementation claims (don't re-dispatch verifiers to re-read
  coordinates the lead already confirmed itself); and carry a PRD's exact literal tokens
  verbatim into the plan. Add a **phrase-pin test per nudge**, a CHANGELOG `[Unreleased]`
  entry, and keep the agenttmpl parse tests green.

## Design Decisions

**D1 — M1 resolves no judge recommendation; it is a standalone security fix.** The judge rec
"capture incidental findings before run teardown" asks for a first-class path to *file* an
incidental finding as a forge issue; that feature is **out of scope** here and stays in the
backlog (todo). M1 fixes the *finding itself* (the Cf-stripping gap) that rec's rationale
referenced, which is the higher-value half and is small. Bundled deliberately.

**D2 — M2 is ergonomics, and the real containment is the SDK env, not the screener.**
- `echo "$PATH"` / `echo "$TMPDIR"` are **already allowed** (base `echo`; shell-expansion
  indirection is a documented accepted residual). What is blocked is *enumeration* (bare
  `env`/`printenv`) and the tidy `printenv VAR` spelling. So this loosening is small.
- The guardrail is **not** the primary containment: the SDK subprocess env is a full
  replacement (`agent/src/sdk-env.ts` — "the SDK `env` option REPLACES the subprocess
  environment entirely"), emitting only `CLAUDE_CODE_OAUTH_TOKEN`, `HOME`, `PATH`, optional
  `TMPDIR`, and a few provisioned keys. So the allowlist is `PATH` + `TMPDIR` **only** —
  `UZI_RUNNER_PATH` and `UZI_UID_SPLIT` are worker-process vars **absent from the agent env
  by construction**; allowlisting them would only produce vacuous tests.
- Rule: allow an `env`/`printenv` call **iff it has ≥1 argument AND every argument is in the
  allowlist** — so `printenv PATH` allows, bare `printenv` and `printenv PATH ANTHROPIC_API_KEY`
  both deny. Bare `env`/`printenv` must stay denied because it would dump
  `CLAUDE_CODE_OAUTH_TOKEN`. The deny lives at two sites (`analyzeSimple`, `analyzeSegment`);
  the allowlist must be applied at both.
- Part (b) of the original rec — make a guardrail denial fail only the offending *segment* of
  a compound command — is **rejected as structurally impossible**: the `PreToolUse` hook
  returns a single allow/deny for the whole Bash call (`screenWithDepth` returns on the first
  deny; `buildPreToolUseHook` emits one decision). The ADR records this so it is not retried.

**D3 — the lead nudges are builtins-only; no propagation.** The `lead` template exists only
in `api/internal/agenttmpl/builtins/lead.md`. It is **not** in this repo's dev-team roster
(`.claude/agents/` has no `lead.md`) and **not** in the upstream `vtmocanu/skills`
`agent-team/roles.yaml`. So M4 is a single-file edit with **no** three-way sync, **no**
`npx skills update`, and **no** upstream push. (Verified 2026-08-15.)

**D4 — M3 fires on explicit exclusion of a guard role, not on mere absence.** The signal is
"a guard role is in the selection's exclusion list (and would otherwise be in the roster)" —
an active, deliberate drop by the owner. Firing on *any* roster that merely lacks
`spec-keeper` (e.g. a template roster that never allocated it, or a repo whose
`.claude/agents/` has none) was considered and **rejected as noisy** — it would fire on runs
where nothing was dropped. We also do **not** try to detect whether the change is
"spec-touching": at approve time only the plan exists, no diff, so that gate is not cheaply
knowable; excluding `spec-keeper` is itself a deliberate, uncommon action always worth a
heads-up. The guard set is a small extensible constant (`spec-keeper` today).

**D5 — M3 reuses `notifysvc`, which already fans out to Slack; no new wire field.**
`notifysvc.Service.Notify(ctx, Notification{UserID, Kind, Payload, RunID, Slack})` persists a
row to the `notifications` table (the in-app bell / `ListNotifications` /
`UnreadNotificationCount`) **and**, when `Slack` is set, enqueues a Slack DM — both
best-effort after the durable write. The web notification center renders **generically**
(`notificationTitle`/`notificationBody` read `payload.title`/`payload.body`, humanized-kind
fallback; `notificationLink` routes any run-bearing kind to `/runs/:id`), so a **new kind
needs no web change**. Slack degrades to a no-op if the owner has no Slack connected. Wiring:
`submitApproval` already resolves the roster (`rosterFor` + `validateSelection`); surface the
excluded-guard list on `SubmitInputResult` and have the approve **handler** (which holds
`h.notifier`) emit the notification — keeping `workersvc` notifier-free, as run-failure
notifications already are.

**D6 — M1 centralizes the strip; that is the better-practice fix, and it must land in
`Markdown.tsx` — never `MarkdownCore.tsx`.** Wrapping each call site (as the existing judge
surfaces do) leaves the next new sink unprotected. Stripping inside the `Markdown` component
covers every sink by construction. `Markdown` (untrusted policy) and `DocMarkdown` (trusted
docs) are **siblings** over the shared `MarkdownCore`; every `<Markdown content=…>` sink is
untrusted LLM/forge text, and docs render via `DocMarkdown`. So stripping in `Markdown` is
safe and does not touch docs; stripping in `MarkdownCore` **would** hit docs and is
forbidden. Cf/bidi code points have no legitimate place in rendered markdown, so the strip is
safe and idempotent (call sites that keep their own `stripUnsafeChars` wrap stay correct).
The strip runs on the content string before markdown parse, so it also strips inside code
fences — **kept deliberately**: a fenced block is exactly where a Trojan-Source payload would
sit. (`\r` is `\p{Cc}` outside the `\n`/`\t` carve-out, so CRLF bodies normalize to LF at
render — harmless.)

**D7 — builtin edits ship to users; CHANGELOG is mandatory, and it is not only M4.** A
`builtins/*.md` edit re-applies to pristine rows on the next boot
(`ReconcileBuiltinTemplates` / `RefreshPristineBuiltin`), so M4 requires a CHANGELOG
`[Unreleased]` line. M2 (guardrail behavior) and M3 (a new owner-facing notification) are
also user-visible worker/API changes and each get a CHANGELOG line.

## Touchpoints

Anchors are as-of 2026-08-15; **re-derive from the quoted string/symbol**, do not trust line
numbers.

### M1 — untrusted `<Markdown>` sinks (web/)
- Helper (reuse, do not reinvent): `stripUnsafeChars` in `web/src/lib/safeText.ts` (regex
  `/(?![\n\t])[\p{Cc}\p{Cf}]/gu`); its corpus test is `web/src/lib/safeText.test.ts`.
- Edit the component: `web/src/components/Markdown.tsx` — strip `content` here, **not**
  `web/src/components/MarkdownCore.tsx` (shared with `DocMarkdown` for trusted docs).
- Render test to extend: `web/src/components/Markdown.test.tsx` (jsdom +
  `@testing-library/react`). Assert a Cf/bidi-bearing string renders stripped.
- Sinks that must end up covered (centralizing covers all by construction — this list is the
  proof set, not a per-site task list): `run.plan_md` (`web/src/pages/RunView.tsx`, ~L1202,
  ~L1310); `feedback` (~L1002); superseded plan bodies `rev.priorPlans` (~L1271); question
  text `web/src/components/QuestionPanel.tsx` (~L189, ~L269); feedback/question/text
  `web/src/components/RunEvent.tsx` (~L1049, ~L1105, ~L1152, ~L1195); chat body
  `web/src/components/ChatMessages.tsx` (~L127).
- Stale-comment sweep (same-commit, fix-the-doc rule — centralizing falsifies these
  present-tense claims): `web/src/pages/RunView.tsx` (~L1831 "render through the SAME hardened
  `<Markdown>`"), `web/src/components/QuestionPanel.tsx` (~L22), `web/src/components/RunEvent.tsx`
  (~L1095, ~L1118), `web/src/lib/safeText.ts` (~L41-44 "a display-time transform, applied per
  render site"), and the test comments in `web/src/pages/RunView.test.tsx` (~L755) /
  `web/src/pages/IssueView.test.tsx` (~L133). Re-derive by content; correct each to reflect
  the centralized strip.
- Server ingest is NUL-only (`stripNULParam(req.PlanMd)`), so the render-side strip is
  load-bearing — do not rely on ingest.

### M2 — env-read guardrail (agent/)
- `agent/src/guardrails.ts`: deny sites `analyzeSimple` (~L479, `REASON_ENV`) and
  `analyzeSegment` (~L522, bare `env` after arg peel). Apply the allowlist at **both**.
- Test suite to extend: `agent/test/guardrails.test.ts` (env cases at ~L36/L37/L99/L291/L708;
  note `printenv PATH` is currently a **deny** fixture — this change **flips** it to allow, so
  update that fixture rather than adding a parallel allow case).
- ADR: `adr/0319-env-read-diagnostic-allowlist.md` — record the loosening, the `PATH`+`TMPDIR`
  scope and why (`sdk-env.ts` sparse replacement is the real containment), the ≥1-arg
  all-allowlisted rule, that bare `env`/`printenv` stays denied, and the rejection of
  per-segment failure (D2).

### M3 — guard-role exclusion notification (api/)
- Roster + exclusions resolve in `api/internal/workersvc/service.go` `submitApproval` (~L4333,
  via `rosterFor` + `validateSelection`); the validator is
  `api/internal/workersvc/agent_selection.go` (~L115). Surface the excluded-guard list on
  `SubmitInputResult` (~L4152).
- Emit from the approve **handler** (the layer holding `h.notifier`; `handler/workers.go`
  decodes exclusions via `DecodeExclusions`). Call `notifysvc.Service.Notify` with a new
  `Kind` (e.g. `guard_role_excluded`), `RunID` set, `Payload{title, body}` naming the dropped
  role, and `Slack: &SlackRender{Title, Body}` for the DM.
- Notification API: `api/internal/notifysvc/service.go` `Notify`; precedent for building one is
  `api/internal/handler/judge_worker.go` (`buildReviewNotification`) and
  `api/internal/notifysvc/run_failure_notifier.go`. Test precedent:
  `api/internal/notifysvc/service_test.go` / `run_failure_notifier_test.go` (fake store) and a
  `workersvc` approve-path test asserting the excluded-guard list is surfaced.
- Correct the record: the CLI (`api/cmd/uzi/run.go`, `approveSelection`) does **not** validate
  that an excluded name exists — the server validates against the live roster. No CLI change
  is needed for M3 (notifications carry no CLI surface).

### M4 — lead prompt (builtins/)
- `api/internal/agenttmpl/builtins/lead.md`. It already has cost-scaling guidance (~L121-126)
  and a plan-evidence procedure (~L38-49). Add: (a) reserve verifier fan-out for genuinely
  uncertain / non-self-checkable claims or post-implementation diffs — do not re-dispatch
  verifiers to re-read coordinates the lead already confirmed itself; (b) carry a PRD's exact
  literal tokens (e.g. NBSP vs ASCII space) verbatim into the plan.
- Phrase-pin tests (the parse tests pass with the nudges absent, so a pin per nudge is
  required): `api/internal/agenttmpl/render_test.go` (`TestLeadPlanCritiquePhrases` ~L494,
  `TestLeadParallelDispatchPhrases` ~L176). Caution: `recipient_test.go` (~L238) rules fire on
  any new sentence containing `SendMessage`/approval language; and `splitLeadRegions` region
  boundaries mean inserted text can move an existing pinned phrase into the wrong region —
  place the new lines carefully.
- CHANGELOG: `CHANGELOG.md` `## [Unreleased]` (currently empty) — add a `### Changed` entry
  per D7, trailing `(#319)`.
- Note: NBSP (U+00A0) is category `Zs`, so it survives `stripUnsafeChars` — M1 and M4 do not
  conflict.

## Milestones

- [ ] **M1** — Cf/bidi stripping is centralized in `Markdown.tsx` and covers every untrusted
  `<Markdown>` sink by construction; a web test asserts a Cf/bidi-bearing `plan_md` renders
  stripped; the stale per-site comments are corrected in the same commit. `task gate:web` green.
- [ ] **M2** — `printenv PATH` / `env TMPDIR` (allowlisted, ≥1 arg) are answerable; bare
  `env`/`printenv` and any non-allowlisted or secret-bearing variable stay denied; the
  existing `printenv PATH` deny fixture is flipped to allow. ADR `0319` added. `task gate:agent`
  green.
- [ ] **M3** — Approving a run whose selection **excludes** a guard role (`spec-keeper`) emits
  one `notifysvc` notification (in-app center + best-effort Slack DM) naming the role; a
  non-guard exclusion emits none. Tests cover both on the emit path. CHANGELOG line added.
  `task gate:api` green.
- [ ] **M4** — `builtins/lead.md` carries both nudges, each protected by a phrase-pin test;
  the agenttmpl parse tests pass; a CHANGELOG `[Unreleased]` line records the builtin change.
  `task gate:api` green.

## Success Criteria

1. A `plan_md` value containing bidi-override / Cf control characters renders with those
   characters removed; a web test proves it.
2. **No** untrusted `<Markdown>` sink renders raw Cf/bidi after M1 (class-scoped — satisfied by
   the centralized strip, and by construction covering the superseded-plan and feedback sinks).
3. `printenv PATH` succeeds; bare `printenv`/`env`, `printenv PATH ANTHROPIC_API_KEY`, and any
   secret-bearing variable are denied. Proven by guardrail tests at both deny sites.
4. Excluding a guard role on approve produces exactly one owner notification (bell + Slack when
   connected); excluding a non-guard role produces none. Proven by tests.
5. `builtins/lead.md` contains both nudges, each pinned by a phrase test; the agenttmpl tests
   pass.
6. CHANGELOG `[Unreleased]` records the M2 guardrail change, the M3 notification, and the M4
   prompt change (each `(#319)`).
7. `specs/ai.md` records the design decisions that touch spec-governed behavior (the guardrail
   allowlist and the new notification kind), so `spec-keeper` does not flag drift.
8. Every touched component's `task gate:<component>` is green.

## Out of scope

- **Worktree-prune convention for the dev-team roster** ("review dispatch worktree evidence"
  rec) — handled host-side by the session, repo-local under each role's `## For this repo (uzi)`
  section in `.claude/agents/{tester,reviewer,auditor}.md`; **not** in this worker MR, because
  those files' generic body is synced from the upstream `vtmocanu/skills` library the worker
  cannot reach (edits above the repo section would be reverted by the sync).
- A first-class path to **file** an incidental finding as a forge issue at run teardown (the
  actual ask of the "capture incidental findings" rec; `propose_issue` is chat-lane only).
  Stays todo. M1 fixes only the security finding that rec referenced.
- A **user-visible warning wire field / gating** on run create or approve. M3 deliberately
  reuses the notification channel instead; a blocking or create-time surface is a separate PRD.
- Per-segment (partial) failure of a compound Bash command on a guardrail denial — rejected as
  structurally impossible (D2).
- Firing M3 on a guard role merely absent from a roster (vs explicitly excluded) — rejected as
  noisy (D4).
- Any change to the upstream `vtmocanu/skills` role library, or crossing the two rosters (M4
  touches only `builtins/`; nothing here touches `.claude/agents/`).
- The other still-open `improve_uzi` recs left as `todo` in this triage pass (CI-fix
  cross-branch dedup, pre-seed/pre-approval reconciliation, toolchain freshness,
  migration-collision lint, memory freshness stamping, require-a-reject-rationale) — scoped
  separately.
