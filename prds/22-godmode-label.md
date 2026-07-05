# PRD #22: GODMODE Label — Run Issues Without a PRD Link

**GitLab Issue**: [#22](https://gitlab.example.com/vtmocanu/uzi/-/issues/22)
**Status**: Draft
**Priority**: Medium
**Created**: 2026-07-05

**Depends on**: PRD #19 M1 (`app_settings` infra, admin GET/PUT, Settings UI) for M1/M2 here; #19 M2 (bootstrap label delivery) for the web milestone (M3). The autopilot-parity item additionally depends on #19 M4 (autopilot run creation) and is gated as a follow-up, not on this PRD's critical path. No migration range needed (see Technical Design).

## Problem

Every run requires a `prds/*.md` link in the issue description. The gate is enforced server-side (`workersvc.CreateRun`, `api/internal/workersvc/service.go:584-586` → `ErrNoPRDLink`; handler 422 at `api/internal/handler/workers.go:296-297`) and surfaced in the web app as "no PRD link" warning badges (`web/src/pages/Board.tsx:547-549`, `web/src/pages/IssueView.tsx:121-126`).

That gate is the right default — the PRD is the agent's spec, and issues without one produce aimless runs. But for genuinely small work (a typo fix, a one-file tweak, a smoke test like issue #20's `vlad-test.txt` file) the gate forces authoring a throwaway `prds/*.md` file whose entire content restates the issue description. The workaround people actually use — a fake link to a nonexistent PRD file — is worse than an explicit escape hatch: it satisfies the regex (`forgesvc.prdLinkRe` matches the path shape, not file existence) while lying on the board.

## Solution Overview

An admin-controlled escape-hatch label, default name **`GODMODE`**:

1. **Two new `app_settings` keys** (infra from PRD #19): `godmode_enabled` (bool, default `true`) and `godmode_label` (default `"GODMODE"`). Both editable on the #19 admin Settings page: a toggle plus a name field (name field disabled while the toggle is off).
2. **Gate bypass**: when the feature is enabled and the issue carries the godmode label, `CreateRun` skips the `HasPrdLink` check. Everything else about the run is unchanged — same state machine, same planning turn, same approval gate, same guardrails.
3. **Web surfaces**: the "no PRD link" warning badge is replaced by a distinct `GODMODE` badge on cards/issue view when the bypass applies; the bootstrap payload (which #19 already extends with `prd_label`/`autopilot_label`) additionally carries `godmode_label` + `godmode_enabled`.

## Design Decisions

1. **On by default** (user, 2026-07-05). The escape hatch is available out of the box; an issue still only bypasses the gate if someone deliberately applies the `GODMODE` label in GitLab, so default-on does not weaken anything for unlabeled issues. Admins who want the strict PRD-only regime flip the toggle off.
2. **The label is checked on the forge-fresh snapshot, not only the cache.** The run-start handler already fetches the issue from the forge (`api/internal/handler/workers.go:272-287`) before calling `CreateRun`, and `forge.Issue.Labels` is on the snapshot. The bypass decision uses those fresh labels, so a just-added GODMODE label works immediately without waiting for a poller cycle. Matching is **exact** (case- and whitespace-sensitive), the same way board column labels match. The cached `issues.labels` column keeps board badges honest between polls.
3. **Policy in the handler, enforcement in the service** (review finding 1). `workersvc` today has no settings dependency and should not gain one. The handler — which already holds the fresh snapshot and needs the settings anyway to interpolate the label name into the 422 message — computes `allowWithoutPRD := godmodeEnabled && slices.Contains(issue.Labels, godmodeLabel)` and passes that single bool into `CreateRun`; the service keeps owning the `ErrNoPRDLink` enforcement. The #19 autopilot path computes the same bool from the just-synced cache labels when it lands (see Technical Design §2).
4. **The gate is evaluated once, at run creation** (review finding 5). Disabling godmode (or removing the label) neither stops nor re-gates an already queued/claimed/running/awaiting-approval run — the sweeper requeue paths and `Claim`/resume never re-check `HasPrdLink` today, and godmode inherits that: toggling off only blocks *new* runs. Documented, deliberate.
5. **Bypass is a gate exception, not a mode.** No new run flag, no claim-payload change, no prompt-template variant. The run proceeds exactly as a PRD-linked run does; the agent works from the issue description alone (which is already the run's `issue_description` snapshot). If the description is thin, the planning turn + human approval gate — both retained — are the safety net, which is precisely why godmode does NOT touch the approval flow.
6. **Quality gate, not a security boundary — and it stays that way.** Who can apply the label is bounded by GitLab (project members with at least Reporter can label). The bypass never weakens any of the four `main`-protection layers, and the human still clicks Start and approves the plan. Interaction with #19 autopilot: an issue carrying PRD + autopilot + GODMODE labels would run unattended with no PRD — that composition is allowed by design (both features are explicit admin/user opt-ins) but must be called out in docs.
7. **Label-name validation mirrors #19 Decision 8, extended.** Non-empty, ≤ 64 chars, no comma, and pairwise-distinct from **both** `prd_label` and `autopilot_label` (a godmode label equal to `prd_label` would exempt every issue; equal to `autopilot_label` would conflate "hands-off" with "spec-less"). Validation lives in the same settings PUT path #19 builds. Two edge cases pinned down (review finding 6): distinctness is enforced **regardless of the toggle state** — a disabled-but-colliding godmode label must be renamed before the conflicting `prd_label`/`autopilot_label` value can be saved (keeps re-enabling always safe; the error message says which key to rename). And a single PUT changing multiple keys validates the **post-merge set atomically**, never each key against stale stored values. Disabling the toggle does not clear the name — re-enabling restores the previous label.
8. **No forge-side label creation** (consistent with #19 Decision 8): admins create the `GODMODE` label in GitLab themselves; docs cover it.
9. **`has_prd_link` semantics are untouched.** The cached flag and the `HasPRDLink` regex keep meaning "description links a PRD file". The bypass is computed at decision points (`CreateRun`, badge rendering) as `hasPrdLink || (godmodeEnabled && labels.contains(godmodeLabel))` — no schema change, no cache invalidation when the setting flips, and the board converges instantly on toggle because badges derive from data already present client-side.

## Technical Design

### 1. Settings (api) — depends on #19 M1

- Keys `godmode_enabled` (`"true"`/`"false"` in the TEXT KV store, typed accessor in `api/internal/settings`) and `godmode_label`. Compiled-in defaults (`true`, `"GODMODE"`) — missing rows never break boot, so **no migration is required**; no seed rows either (matching #19's defaults-compiled-in posture; seeds there are belt-and-braces for the two label keys).
- Validation in the settings PUT handler per Decision 7. Both keys are **registered in #19's per-key validation switch** — #19 Decision 8 implies keyed validation, so new keys are not accepted "automatically" (review finding 4a).
- Absent-row behavior (review finding 4b): since no rows are seeded, `GET /api/admin/settings` may omit the godmode keys until first write. The admin UI and the bootstrap payload synthesize the compiled-in defaults (`true`, `"GODMODE"`) when a row is absent — absent-row is normal, never "misconfigured". If #19's GET instead returns all-known-keys-with-defaults, register the godmode defaults there and the synthesis moves server-side; pin this down against #19 M1's actual shape at implementation time.

### 2. Gate bypass (api)

- Per Decision 3: the handler computes `allowWithoutPRD := godmodeEnabled && slices.Contains(issue.Labels, godmodeLabel)` (exact match, fresh snapshot) and passes the bool into `CreateRun`; the service gate becomes `if !issue.HasPrdLink && !allowWithoutPRD { return ErrNoPRDLink }`. `workersvc` gains no settings dependency.
- The `CreateRun` signature change touches the existing no-PRD-link tests (`api/internal/workersvc/service_test.go:798-827`) — update them in M2, don't bolt on parallel ones.
- The 422 message (`workers.go:297`) is extended when godmode is enabled instance-wide: `"issue has no PRD link; add a prds/*.md link (or the GODMODE label) before starting a run"` — the label name interpolated from settings.
- **Autopilot parity is a follow-up gated on #19 M4** (review finding 3): the autopilot trigger detects issues from the just-synced *cache* (poller post-sync query, #19 finding B3), not a fresh `GetIssue` — so that path computes `allowWithoutPRD` from the cached `issues.labels` (no extra forge call; just-synced is fresh enough). Tracked as milestone M6 here, implementable only once #19 M4 exists.

### 3. Web

- Bootstrap payload: `godmode_label`, `godmode_enabled` alongside #19's labels.
- `Board.tsx` card badge and `IssueView.tsx` badge: when `!has_prd_link`, show the warning badge unless godmode is enabled and the card's labels include the godmode label — then show a distinct badge (e.g. tone `warning`→`accent`, text `GODMODE`, title "PRD-link gate bypassed by label").
- Board label-suggestion filter (from #19 M2's bootstrap-label work) also excludes `godmode_label` from column suggestions.
- Admin Settings page: toggle + name field per Solution Overview.

### 4. Docs + specs

- `docs/`: admin-settings page section (what godmode does, that it's a quality-gate bypass not a security change, the autopilot composition caveat, "create the label in GitLab yourself").
- `specs/ai.md`: decisions above. `specs/human.md` (user approval required): the godmode user story.

## Milestones

- [ ] **M1 — Settings keys + admin UI** *(needs #19 M1)*: accessors, key registration in the PUT validation switch, validation matrix tests (pairwise-distinct incl. while-disabled, post-merge atomic multi-key PUT), toggle + name field on the admin Settings page, absent-row default synthesis in GET/UI/bootstrap.
- [ ] **M2 — Gate bypass end-to-end**: handler-computed `allowWithoutPRD` → `CreateRun` enforcement, extended 422 message, unit tests (enabled/disabled × label present/absent × PRD link present/absent), existing no-PRD-link tests updated.
- [ ] **M3 — Web surfaces** *(needs #19 M2 bootstrap delivery)*: badges on Board + IssueView, label-suggestion exclusion, mock API + vitest coverage.
- [ ] **M4 — Docs + specs**: docs page section, `specs/ai.md` update, `specs/human.md` proposal for user approval.
- [ ] **M5 — E2E validation**: godmode disabled → 422; enabled + labeled → run starts without PRD link. Preconditions to build into the harness: the fake forge serves the godmode label on the test issue, and the scenario drives `PUT /api/admin/settings` in the isolated stack.
- [ ] **M6 — Autopilot parity** *(follow-up, blocked on #19 M4)*: autopilot trigger computes `allowWithoutPRD` from cached labels; composition test (PRD + autopilot + GODMODE, no PRD link → unattended run).

## Open Questions

1. **Badge look**: exact tone/wording of the GODMODE badge — pick during M3.

## Risks

- **Erosion of the PRD discipline**: the label must be deliberately applied per issue, the toggle is admin-only, and the plan-approval gate is retained; instances wanting the strict regime disable the toggle.
- **Label added by a low-trust GitLab member**: same trust model as board columns (labels already drive board moves); the human Start click remains the actual trigger.
- **Skew between fresh labels and cache**: badges may lag a poller cycle behind GitLab; the gate itself never lags because it reads the fresh snapshot (Decision 2).

## Decision Log

- 2026-07-05: PRD created (user request): configurable label name (default GODMODE) + on/off toggle in admin settings; label bypasses the PRD-link gate.
- 2026-07-05: Default state set to **enabled** (user) — per-issue label application is the deliberate act; the toggle exists for instances that want the strict PRD-only regime.
- 2026-07-05: Review round (reviewer + fact-checker agents). Fact-check: all codebase claims confirmed. Review fixes applied: bypass policy moved to the handler (`allowWithoutPRD` bool into `CreateRun`, no settings dep in `workersvc`); dependency corrected to #19 M1+M2 with autopilot parity split into M6 behind #19 M4 (cache labels, not fresh fetch, on that path); godmode keys registered in #19's keyed validation + absent-row default synthesis specified; one-time gate evaluation (toggle-off doesn't stop existing runs) made explicit; validation edge cases (distinct-while-disabled, atomic multi-key PUT) pinned; exact-match label semantics stated; e2e preconditions listed.
