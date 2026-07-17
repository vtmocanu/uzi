# PRD #69: Judge mode (off / optional / enforced) + per-user judge model + opus default

**GitLab Issue**: [#69](https://gitlab.example.com/vtmocanu/uzi/-/issues/69) (to be created)
**Status**: Draft (created 2026-07-17)
**Priority**: Medium
**Supersedes**: PRD #59 (default judge model → sonnet). #59's single change (flip the compiled-in default) is folded in here, but the value is **opus**, not sonnet — see Decision 1, which reverses #59's Decision 1. Close #59 as superseded on merge.
**Related**: PRD #46 (run judge + self-improvement — introduced `judge_enabled`, `judge_model`, and the per-user `users.judge_enabled` opt-in this PRD extends). PRD #17 (per-user `default_model` — the layering pattern this PRD mirrors for the judge model).

## Problem

The judge (PRD #46) has two rigid points that admins and users have asked to
soften:

1. **Enablement is all-or-nothing per user.** The global `judge_enabled`
   kill-switch only makes the feature *available*; each user must then opt in
   themselves (`PUT /me/judge`, gate at `api/internal/workersvc/judge_enqueue.go:71`).
   An admin who wants the judge running across the whole factory has no way to
   turn it on for everyone — they must ask each user to flip their own toggle.

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
   here (Decision 1) is opus, with the new per-user override as the cost lever
   for anyone who wants to dial it back.

## Solution Overview

Three layered changes, each independently shippable:

1. **Instance judge mode.** Keep `judge_enabled` as the master kill-switch and
   add an admin `judge_enforce_all` boolean. The enqueue gate resolves to three
   effective modes:
   - **off** — `judge_enabled=false`. No judge for anyone (unchanged).
   - **optional** — `judge_enabled=true`, `judge_enforce_all=false`. Each user
     self-opts-in via `PUT /me/judge` (today's behavior, unchanged).
   - **enforced** — `judge_enabled=true`, `judge_enforce_all=true`. The judge
     runs on every eligible run for every user; the per-user `judge_enabled`
     flag is bypassed.

2. **Per-user judge model override.** A new nullable `users.judge_model`. NULL
   means inherit the instance `judge_model`; a set value overrides it for that
   user's judge runs only. The user sets it themselves through their own
   settings (`PUT /me/settings`, alongside `default_model` and `theme`),
   validated by the same `validateModelAlias` the admin setting uses. Resolution
   at judge-claim assembly is user-value-wins, else instance default — mirroring
   how `default_model` layers user-over-template (PRD #17).

3. **Instance default judge model → opus.** Flip the compiled-in
   `DefaultJudgeModel` from `haiku` to `opus` and align every surface that
   states or displays it (settings comments, admin UI copy, docs, web mocks,
   tests). Admin-set values still win; the default only applies where the key is
   unset/blank.

What does **not** change:

- **Spend never leaves the run owner's own account.** Enforced mode makes a
  user's *own* runs get judged on that user's *own* tokens — an admin still
  cannot redirect judge spend to anyone else (the PRD #46 invariant at
  `handler/judge.go:23` holds). The per-user model override is the user's cost
  lever: forced-on but wants it cheap → set `haiku`.
- **Stored `run_reviews.judge_model` rows are historical** — they record what
  actually ran and are never rewritten.
- **The judge stays off out of the box.** `judge_enabled` default is still
  `false`; `judge_enforce_all` defaults `false`. A fresh instance is unchanged.

## Design Decisions

1. **opus, not sonnet (reverses #59 Decision 1).** User decision (2026-07-17):
   the instance default judge model is `opus`. The recommendation half of the
   judge feeds self-improvement, and the strongest model is wanted by default;
   the new per-user override (Decision 4) is the escape hatch for anyone who
   finds opus-per-run too expensive on their tokens. **Cost note carried
   forward from #59:** the judge fires on *every* eligible completed run, so an
   opus default is the heaviest per-run cost point. This is accepted because (a)
   the judge is off by default and opt-in/enforced deliberately, (b) each user
   spends only their own tokens, and (c) the per-user override lets cost-
   conscious users drop to sonnet/haiku. Admins who want a cheaper floor pin
   `judge_model` in settings.

2. **`judge_enforce_all` is a separate boolean, not a tri-state enum replacing
   `judge_enabled`.** Keeping `judge_enabled` as-is and adding one flag is the
   lower-risk, lower-churn change: no migration of the existing setting, no
   rename rippling through tests/UI/docs, and the master kill-switch semantics
   stay exactly where every reader already expects them. The three modes are
   derived, not stored.

3. **Enforced mode is hard force-on (ignores per-user opt-out), not a soft
   default.** User decision: "enforce it for all." A user cannot opt out of an
   enforced judge; their lever is the model override, not the on/off toggle.
   Rationale: the admin owns the instance and wants the retrospective practice
   applied factory-wide; a soft "default-on but opt-out" mode can be added later
   if a real need appears, but is out of scope now (avoid a fourth mode nobody
   asked for). The per-user `judge_enabled` flag is preserved and still governs
   the **optional** mode — enforced simply short-circuits the gate above it.

4. **Per-user judge model layers user-over-instance, blank = inherit.** Mirrors
   PRD #17's `default_model`: `users.judge_model` NULL/blank inherits the
   instance `judge_model`; a set value wins for that user's judge runs. Unlike
   the instance setting (which must be concrete — `validateModelAlias` rejects
   blank, `settings.go:790`), the per-user field accepts blank as "inherit", so
   the write path trims-to-NULL like `default_model` does
   (`handler/user_settings.go:87`).

5. **Resolution happens at judge-claim assembly, keyed by the run owner.** The
   claim builder (`workersvc/judge.go:152`) currently reads only the instance
   `settings.JudgeModel(ctx)`. It gains a per-user lookup on `run.UserID`
   (`users.judge_model`) and uses that when set, else the instance value. The
   enqueue path already loads the owner at Gate 3
   (`judge_enqueue.go:66`), so the owner row is cheap to thread; the claim
   assembly resolves independently so a worker-side re-claim resolves the same
   way.

6. **PRD #46 and #59 are not rewritten in place.** #46 is a done PRD (historical
   decision log). #59 is superseded by this PRD and closed; its touchpoint
   analysis for the default flip is reused here with the value changed to opus.
   `specs/ai.md` records the supersession and the opus decision.

7. **e2e keeps its explicit `judge_model` pin.** `e2e/run-e2e.sh` PUTs an
   explicit `judge_model` — it exercises the settings endpoint and the stub
   executor never calls Anthropic, so the value is inert. Leaving an explicit
   value also keeps e2e asserting that an admin-set value survives independent
   of whatever the compiled default is. Add coverage that `judge_enforce_all`
   round-trips through the settings endpoint the same way.

## Data model

- **New column** `users.judge_model text` (nullable, no default) — migration
  `00067_user_judge_model.sql` (number is a draft; renamed to the next free
  number above the live head at merge, per the goose discipline in CLAUDE.md;
  head is currently `00066_hosted_workers.sql`). sqlc: extend `GetUserSettings`
  / add `SetUserJudgeModel`, regenerate.
- **New app setting** `judge_enforce_all` (text `"true"`/`"false"`, default
  `"false"`). Follows the PRD #46 no-seeded-row pattern: add to `Defaults`,
  `Known`, the boolean validation branch, and a typed `JudgeEnforceAll(ctx)`
  accessor. No migration (an absent row synthesizes from `Defaults`).

## Touchpoints

**M1 — judge mode (enforce-all):**
- `api/internal/settings/settings.go`: `KeyJudgeEnforceAll` const,
  `DefaultJudgeEnforceAll="false"`, `Defaults`/`Known` entries, bool validation
  branch (alongside `judge_enabled`), `JudgeEnforceAll(ctx)` accessor.
- `api/internal/workersvc/service.go`: extend the `JudgeSettings` reader
  interface (`:297`) with `JudgeEnforceAll`.
- `api/internal/workersvc/judge_enqueue.go`: Gate 3 (`:71`) — bypass the
  per-user `owner.JudgeEnabled` check when `JudgeEnforceAll` is true (mode =
  enforced). Global kill-switch (Gate 2) still governs.
- `api/internal/handler/settings.go` + `settings_test.go`: `judge_enforce_all`
  in the admin GET/PUT surface.

**M2 — per-user judge model:**
- `api/internal/store/migrations/00067_user_judge_model.sql` (new).
- `api/internal/store/queries/users.sql`: extend user-settings read, add
  `SetUserJudgeModel`; `sqlc generate`.
- `api/internal/handler/user_settings.go`: add `judge_model` to
  `userSettingsDTO`, GET, and the PATCH-like PUT (trim-to-NULL, validated by the
  shared model validator — reuse `validateModel`).
- `api/internal/workersvc/judge.go:152`: resolve owner override before falling
  back to instance `JudgeModel`.
- Tests: `user_settings` handler test, `workersvc/judge_*_test.go` claim-model
  resolution (user override wins; NULL inherits).

**M3 — default → opus (folds in #59):**
- `api/internal/settings/settings.go`: `DefaultJudgeModel="opus"` (`:109`) +
  the Decision-7 rationale comment (`:104-107`) + the fallback doc comment
  (`:470-471`).
- `api/internal/settings/settings_test.go:112,205` (default assertions).
- `web/src/pages/AdminSettings.tsx` (judge model placeholder + "the default is
  usually right" copy — reword; opus is not "cheap").
- Web mocks/tests asserting the default: `web/src/mocks/mockApi.ts`,
  `web/src/mocks/data.ts`, `web/src/mocks/mockApi.test.ts`,
  `web/src/pages/AdminSettings.test.tsx`. (Historical review/run rows — leave.)

**M4 — web for M1 + M2 + docs/specs:**
- `web/src/pages/AdminSettings.tsx`: `judge_enforce_all` toggle with copy
  explaining enforced vs optional and the own-token-spend implication.
- User settings page: a per-user judge-model card (blank = "use the instance
  default"), next to the existing worker-model card.
- `web/src/mocks/*`: `judge_enforce_all` + `users.judge_model` shapes.
- `docs/judge.md`, `docs/admin-settings.md`: document the three modes, the
  per-user override, and the opus default + how to pin a cheaper model back.
- `specs/ai.md`: decision entries (judge mode, per-user model, opus default
  superseding #59). `web/scripts/check-docs.mjs` green via `npm run build`.

## Milestones

Dependency-ordered; M1 and M2 touch disjoint files and can run as parallel
agents, M3 is independent of both, M4 depends on all three landing (it wires the
UI + docs for every change).

- [ ] **M1 — Judge mode (enforce-all)** *(API; parallel with M2/M3)*
  `judge_enforce_all` setting + typed accessor + enqueue gate bypass +
  admin-settings surface. `go test ./internal/settings ./internal/workersvc
  ./internal/handler` green.
- [ ] **M2 — Per-user judge model** *(API; parallel with M1/M3)*
  `users.judge_model` migration + sqlc + `/me/settings` read/write +
  claim-assembly resolution. `go test ./...` + `sqlc generate` clean.
- [ ] **M3 — Default judge model → opus** *(API + mocks; parallel with M1/M2)*
  `DefaultJudgeModel="opus"`, comment trail + settings tests + web mock defaults.
  `go test ./internal/settings`; `npm run typecheck` + `npm test`.
- [ ] **M4 — Web + docs + specs** *(depends on M1+M2+M3)*
  Admin enforce-all toggle, per-user judge-model card, docs for all three
  changes, `specs/ai.md` entries, close #59. `npm run build` (check-docs) green.

## Success Criteria

- **Mode off**: `judge_enabled=false` → no judge enqueued for anyone (unchanged).
- **Mode optional**: `judge_enabled=true`, `judge_enforce_all=false` → only
  users with their own `judge_enabled=true` get judged (unchanged).
- **Mode enforced**: `judge_enabled=true`, `judge_enforce_all=true` → every
  eligible run is judged regardless of the per-user flag; spend stays on each
  run owner's token.
- **Per-user model**: a user who sets `judge_model` gets their judge claims
  assembled with that model; NULL inherits the instance value; an admin-set
  instance value is the fallback, not an override of a set user value.
- **Opus default**: fresh instance with the judge enabled and no `judge_model`
  set assembles judge claims with `opus`; an explicitly set instance value still
  wins; no UI/docs surface still calls haiku (or sonnet) the default.

## Out of Scope

- A soft "default-on but user-opt-out" fourth mode (Decision 3 — add later only
  if needed).
- Admin cap/lock on the per-user judge model (own-token spend makes a ceiling
  unnecessary; explicitly rejected in the design discussion).
- Changing the judge prompt/compaction or the self-improvement engine.
- Per-user override of the *agent-run* `default_model` policy (already exists,
  PRD #17) or making the agent default opus (the `lead` builtin is already
  `opus` — `api/internal/agenttmpl/builtins/lead.md:4`).
- The rate-limit probe model (`claude-haiku-4-5` — a deliberate cheapest-model
  choice, unrelated to the judge).
