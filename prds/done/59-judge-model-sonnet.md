# PRD #59: Default judge model → sonnet

**GitLab Issue**: [#59](https://github.com/vtmocanu/uzi/-/issues/59)
**Status**: Draft (created 2026-07-16)
**Priority**: Low
**Related**: PRD #46 (run judge + self-improvement — introduced `judge_model` with the haiku default this PRD flips; Decision 7 there stays as the historical record).

## Problem

The judge (PRD #46) defaults to the `haiku` alias (`DefaultJudgeModel`,
`api/internal/settings/settings.go:104`). Decision 7 picked "the cheapest
capable model" because a retrospective is a single compacted-trace round-trip
on the run owner's tokens. The verdict half of the job (`ideal | ok | issues`)
is constrained classification and haiku handles it; the recommendation half is
not. The judgment-heavy categories (`improve_agent`, `adjust_template`,
`improve_uzi`) feed self-improvement runs (Decision 9), and a shallow
retrospective produces shallow self-improvement PRs — the cost of a weak model
compounds downstream instead of staying contained in one summary.

## Solution Overview

Flip the compiled-in default `judge_model` from `haiku` to `sonnet` and align
everything that states or displays the default: the settings comment trail, UI
placeholder + helper copy, docs, web mocks, and tests that assert the default.
Opus stays available but is not the default: the judge is one single-turn
structured-output call per run, and opus-per-run on users' tokens buys little
over sonnet for that shape.

Nothing else changes:

- **Admin-set values are untouched.** `judge_model` set in `app_settings`
  always wins; the default only applies where the key is unset/blank
  (`settings.go:445` fallback).
- **The judge stays opt-in twice over** (global `judge_enabled` default OFF +
  per-user toggle), so the blast radius of the new default is only instances
  that enabled the judge without pinning a model.
- **Stored `run_reviews.judge_model` rows are historical** — they record what
  actually ran and are never rewritten.

## Design Decisions

1. **sonnet, not opus** (user + assistant assessment, 2026-07-16). The failure
   mode being fixed is recommendation quality feeding self-improvement, and
   sonnet closes most of that gap. Opus per judged run on user tokens is the
   wrong default cost point for a single-turn call; admins who want it set it.
2. **Upgrade behavior is silent and accepted, but documented.** An instance
   that enabled the judge and never pinned a model starts spending sonnet
   instead of haiku tokens after upgrade. Acceptable because the judge is
   opt-in and per-run cost stays small (one bounded-prompt call), but
   `docs/admin-settings.md` must state the default plainly so admins who care
   can pin `haiku` back.
3. **PRD #46 Decision 7 is not rewritten.** Done PRDs are the historical
   decision log; this PRD + a `specs/ai.md` entry record the supersession.
4. **e2e keeps its explicit pin.** `e2e/run-e2e.sh:1359` PUTs
   `judge_model: "haiku"` explicitly — it exercises the settings endpoint, and
   the stub executor never calls Anthropic, so the value is inert. Leaving it
   also keeps the e2e asserting that an explicit admin value survives
   independent of whatever the compiled default is.

## Touchpoints

- `api/internal/settings/settings.go`: `DefaultJudgeModel` (`:104`) + the
  Decision-7 rationale comment above it (`:100-102`) + the fallback doc
  comment (`:445`).
- `api/internal/settings/settings_test.go:107,205` (default assertions).
- `web/src/pages/AdminSettings.tsx:465,471` (placeholder + "the default
  (`haiku`) is usually right" copy — reword; sonnet is not "cheap").
- Web mocks/tests asserting the default: `web/src/mocks/mockApi.ts:99`,
  `web/src/mocks/data.ts:275`, `web/src/mocks/mockApi.test.ts:54`,
  `web/src/pages/AdminSettings.test.tsx:39`. (`data.ts:870,878` and
  `RunView.test.tsx:205` model historical review/run rows — leave.)
- `docs/admin-settings.md:40` ("a cheap alias like `haiku` by default") +
  sweep `docs/judge.md` for default-model copy.
- `specs/ai.md`: decision entry recording the flip and why.

## Milestones

- [ ] **M1 — API default flipped**: `DefaultJudgeModel = "sonnet"`, comment
      trail updated to the new rationale, settings tests green
      (`go test ./internal/settings`).
- [ ] **M2 — Web + mocks aligned**: AdminSettings placeholder/copy reworded,
      mock defaults flipped, `npm run typecheck` + `npm test` green.
- [ ] **M3 — Docs + specs**: `docs/admin-settings.md` states the sonnet
      default and how to pin haiku back; `specs/ai.md` decision entry added;
      `web/scripts/check-docs.mjs` (via `npm run build`) green.

## Success Criteria

- Fresh instance with the judge enabled and no `judge_model` set assembles
  judge claims with `sonnet`.
- An explicitly set `judge_model` (any alias or full model ID) still wins.
- No UI/docs surface still calls haiku the default.

## Out of Scope

- Per-user judge model overrides.
- Changing the judge prompt/compaction or the self-improvement engine.
- Touching the rate-limit probe model (`claude-haiku-4-5`, specs §240 — a
  deliberate cheapest-model choice, unrelated to the judge).
