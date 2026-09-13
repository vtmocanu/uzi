# PRD #1331 — Onboarding checklist: derive "Bring a worker online" from setup state, not liveness

**Issue**: #1331
**Priority**: Medium
**Status**: Complete
**Base at authoring**: `d56436f756179a3c387deb6ba7f2d618b8414dc5` (line anchors below are as of this commit; re-derive symbol-first, lines drift)

## Problem

The Dashboard's "Get the factory running" card (`web/src/pages/Dashboard.tsx`) marks step 4, "Bring a worker online", as done only while `workersOnline > 0` (`:326`), and `workersOnline` counts workers whose `status === "online"` right now (`:126` on first load, `:171` on the 10s poll). The card's own visibility, `ready` (`:228-229`), is derived from the same four booleans, so it is recomputed every poll.

A hosted worker roll replaces the pod: the old pod stops heartbeating, the sweeper flips the row to `offline` (`MarkStaleWorkersOffline`, `api/internal/store/queries/runtime.sql:354`), and the new pod is `online` only after it pulls its image and registers. In that gap every worker reads `status: "offline"` with `upgrade_status: "upgrading"`. Observed on the live instance on 2026-09-13 during the v0.83.0-rc.1 fleet roll: token, forge and repo done, and the card came back at **3/4** with step 4 open and the hint "Generate a join token and start the uzi-agent container with it", for a user who has two joined hosted workers. Any transient outage (node drain, network blip past the heartbeat window) produces the same screen.

The step conflates two questions:

- **Setup**: "have you brought a worker online?" (one-time, durable, what an onboarding checklist asks)
- **Health**: "is a worker online right now?" (live, already answered by the "Workers online N/M" tile beside the card and by the Workers page badges)

Steps 1-3 (token exists, forge connected, repo enabled) are all setup facts. Step 4 is the odd one out.

## Solution

Derive step 4 from a durable setup fact, client-side, from data the API already sends:

```
workerJoined = workers.some(w => w.status === "online" || w.last_heartbeat_at != null)
```

- `last_heartbeat_at` (`web/src/lib/apiTypes.ts`, `string | null`) is stamped `now()` at register (`runtime.sql:225`) and on every heartbeat (`:298`, the `UPDATE workers SET status = 'online' … last_heartbeat_at = now()` statement) and is **never cleared**: the offline sweep at `:354` writes `status`, `online_since` and `updated_at`, not `last_heartbeat_at`, and no other statement in `api/` nulls it (the only two writes are the `= now()` stamps). So a worker that ever registered and heartbeat keeps it for life; a worker whose join token was minted but never used has `null`.
- A worker created-but-never-started therefore still leaves step 4 open (correct: the user has not brought one online). Deleting every worker reopens the step (correct: derived state self-corrects; a stored flag would not).
- The "Workers online 0/2" stat tile is untouched: it is the health surface and is accurate during a roll.

Compute `workerJoined` once at render time from `data.workers` (the full fleet is already on the `Overview` state, `:39`) and use it for both the step's `done` and the `steps` array (`:228`), so the "N/4 done" counter and the strike-through cannot disagree. Do not add it to the `Overview` shape or the two `setData` sites; one render-time derivation replaces the liveness read.

No API, DTO, migration, CLI or mock-fixture change. Copy unchanged.

## Decision log

- **D1 — Derived, not stored.** A persisted "onboarding completed" flag (browser or server) can drift from reality: stamped mid-roll, never unset when the user deletes their forge or workers, racy across tabs. Every other step is derived; this one becomes derived too. A "Dismiss" affordance is a separate feature, not requested.
- **D2 — Client-side, not a server-computed `onboarding` object on `/me`.** All four steps are client-derived from four list calls today; moving one to the server makes it the odd one out again, and no second consumer exists (the CLI and TUI render no checklist). Revisit only if a second surface wants the checklist.
- **D3 — `last_heartbeat_at`, not `version`, as the "ever registered" signal.** `version` is also written only at register, but it is `null` for an unstamped local image, so it would keep step 4 open forever on a dev stack. `last_heartbeat_at` is written on every heartbeat by every worker kind.
- **D4 — Keep the offline sweep as is.** It must keep clearing `status`/`online_since` only; the fix depends on `last_heartbeat_at` surviving it. No api change is needed or wanted.
- **D5 — Hint copy unchanged.** "Generate a join token and start the uzi-agent container with it" is still the right instruction for a user with no joined worker (hosted-first wording is PRD #1063's concern, not this one's).

## Scope / non-goals

- No server-side onboarding payload, no dismiss button, no `localStorage`.
- No change to the "Workers online" tile, the Worker load card, or the Workers page.
- No change to `api/cmd/uzi/`: the CLI has no checklist (checked per the root `CLAUDE.md` CLI-parity rule).
- No `docs/*.md` or `specs/human.md` mention this card: "factory running" and "Bring a worker online" have zero hits at the base commit; the "checklist" / "onboarding" hits (`docs/run-activity.md`, `docs/run-completion-hold.md`, `docs/cli.md`) are the TUI milestone checklist and the CLI docs corpus, unrelated. No docs or spec sync is owed.
- No `.github/workflows/**` change in implementation or validation (`.claude/rules/prds.md`).

## Milestones

- [x] **M1 — Regression tests, written first and shown failing on the unfixed predicate (web).** In `web/src/pages/Dashboard.test.tsx`, beside the PRD #60 checklist test (`describe("Dashboard onboarding checklist (PRD #60)")`, `:259`), using the existing `aWorker` fixture (`:112`, whose default is `status: "online"`, `last_heartbeat_at: null`) and `renderDashboard` (`:150`). Keep steps 1-3 open (the `beforeEach` defaults) so the card renders and the step is assertable:
  - **Fleet mid-roll counts as joined**: `listWorkers` resolves two workers with `status: "offline"`, `upgrade_status: "upgrading"`, `last_heartbeat_at` set (any ISO timestamp) → "Bring a worker online" carries `line-through`, and the counter reads `1/4 done`. This is the bug's exact screen; it must **fail** on the base commit.
  - **Never-joined worker stays open**: one worker `status: "offline"`, `last_heartbeat_at: null` → the step has no `line-through`, counter `0/4 done`. This passes on the base commit too; it pins the boundary so the fix cannot over-reach to "any worker row exists".
  - **Card hides on a registered-but-offline fleet once steps 1-3 are done**: mock `listSecrets` with a full `SecretMeta` whose `kind` is `"anthropic_token"` (`hasAnthropicToken` in `web/src/lib/hasToken.ts` keys on that; the type in `apiTypes.ts` needs id/kind/label/is_default/auto_eligible and the rest, build a complete literal), `listConnections` non-empty, `listRepos` with one `enabled: true` repo, workers as in the first case → `queryByText("Get the factory running")` is null **and**, as the positive control against a vacuous negative (`.claude/rules/web.md`, "Copy changes disarm negative assertions"), the stat tile still renders `0/2` for Workers online. Must fail on the base commit (the card would render at 3/4).
  - The "N/4 done" counter (`:298`) renders as two adjacent text nodes; assert it with `container.textContent` containing `"1/4 done"` (the suite's existing convention for split text, e.g. `:457`), not `getByText`.
  - Evidence in the run: the vitest output for this file on the unfixed tree (two red) and after M2 (all green). Run it as `cd web && npx vitest run src/pages/Dashboard.test.tsx`.
- [x] **M2 — The predicate (web).** In `Dashboard.tsx`: derive `workerJoined` once at render from `data.workers` per the Solution, **null-safe** (`data` is null in the skeleton state, `:98`/`:239`; derive as `(data?.workers ?? []).some(...)` or inside the existing `data ? … : []` branch); use it in the `steps` array (`:228`) and as step 4's `done` (`:326`); leave `workersOnline`/`workersTotal` and the tile alone. **Keep both disjuncts**: the existing PRD #60 test (`:264`) uses `aWorker({ status: "online" })` with the fixture default `last_heartbeat_at: null`, so a predicate reduced to `last_heartbeat_at != null` alone reddens it. Add a two-line comment on the derivation naming the roll case (offline + upgrading) and why `last_heartbeat_at` is the durable signal (never cleared by the offline sweep). Gate: `task gate:web` green (lint, knip, typecheck, tests). Then `cd web && npm run build` once by hand, since the gate omits `vite build` (`.claude/rules/web.md`).

## Success criteria

- During a fleet roll (all workers `offline` + `upgrading`), a user with steps 1-3 done sees no "Get the factory running" card; the "Workers online" tile reads `0/N`.
- A user who minted a join token but never started the container still sees step 4 open with the join-token hint.
- Deleting every worker reopens step 4.
- The three M1 tests pass; the two designed to fail on the base commit were shown failing there.
- `task gate:web` green; no change outside `web/src/pages/Dashboard.tsx` and `web/src/pages/Dashboard.test.tsx`.

## Risks

- **Over-reach to "any worker row"**: a join token minted and abandoned would mark the step done. Mitigated by the never-joined test in M1 and by D3.
- **Test asserts on copy that later changes**: the tests key on the step title and the "N/4 done" counter. Both are the card's own copy; a future copy change must repoint them (the web rule's copy-change sweep).
