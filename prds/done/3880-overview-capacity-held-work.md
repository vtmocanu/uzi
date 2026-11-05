# PRD #3880: Overview preview, capacity rail and held work

**Status**: Implemented (AI-synced 2026-11-05). Child of epic #3873. Blocked by: #3878, satisfied by merged PR #3952 (`699e4425`). M1 (capacity rail) and M2 (Held work card, independent holds poll, held-work decision row and header count) are built on branch `agent/issue-3880`. In-run evidence: focused Vitest for the touched files and the mock scenarios, the `OverviewNext` browser cases (including 400 px and 320 px rows), `task check:web`, `npm run build`, `task deadcode:web`, `task check-docs:web`, `task check:changelog-entry`, `task check:token-literals`, `task scan:secrets` and `task sast:semgrep` all passed. Each milestone has a regression test watched failing on the pre-change code. Side-by-side screenshots at 1440 px and 400 px in Ember and Dawn, app and mock, are in `prds/mockups/3880/`. Pending exact-head PR CI (landing obligation, not verified in-run): full `test:web`, `test:web-browser`, `gate:repo` and the mock image build.

**Design brief**: `prds/3873-overview-redesign.md` (model-fidelity rules, data readiness, reuse map, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`, the Capacity and Held work cards and the held-work decision row. The brief's mock contract applies: implement the mock, list larger deviations under `Mock deviations` in the PR body with their reasons, and carry side-by-side screenshots.

## Problem

The preview's `WorkerLoadCard` lists only workers with resource samples, so free run capacity still takes arithmetic. Its `CustodyBoardAlert` self-hides when protection is healthy and combines custody capacity, open holds and owner decisions when it appears. The rail gives those separate places without changing admission or custody semantics.

## Outcome

On `/overview/next`, put a right-hand rail next to `OverviewRunsWidget`, stacking below it at narrow widths. Replace the preview's `WorkerLoadCard` and `CustodyBoardAlert` while keeping the current `/dashboard` unchanged.

- **Capacity (your fleet)**: "6 of 8 run slots in use" and a segmented bar for in-use, closed and open advertised slots, with a legend. List every owner worker, including offline workers and workers with no stats. Each row retains today's status, `WorkerRunBadge` and `WorkerCordonBadge`, plus its name. Put the existing cpu, mem and disk stat line behind a disclosure. A draining or cordoned worker keeps a neutral badge and says "Takes no new runs". Show "Rolls to vX once fully idle" only with a hosted worker's actual upgrading report, a nonempty `upgrade_target` and a drain; an outdated version or manual cordon alone is no rollout promise. Sanitize the reported target and worker name.
- **Held work (yours)**: always show custody slots used of the configured limit, total open holds and holds needing a decision as three separate figures. When `custodyAlertView` yields a state, keep its exact headline and severity: "Held work is blocking new runs" (danger), "Held work needs your attention" or "Custody capacity reached" (warning). Show Review held work only when an open hold exists and `decision_needed > 0`, matching the current alert's gate; link to the existing Workers held-work section.
- **Needs your decision** gains one held-work row when the owner aggregate reports `open_holds > 0` and `decision_needed > 0`. It links to the Workers held-work section with Review held work. If an open decision hold is present in the owner list, name its run and show the oldest matching hold's age; with several holds, state the aggregate count. If the aggregate says a decision is pending but the list cannot identify a hold, show a generic row rather than hiding the decision or inventing a run. The header's decision count adds the aggregate `decision_needed` only under the same gate, not one per display row. The command menu (#3875) later consumes the same owner decision source.

Acceptance examples:

- Blocked runs with `decision_needed = 0`: Held work shows the danger headline, but neither card offers Review held work and there is no held-work decision row.
- A draining worker with one spare advertised run slot: the spare slot is closed, not open, and the badge remains neutral. A manual cordon alone does not claim a roll is pending.
- With no custody hold open, Held work shows zero figures and no headline, and adds nothing to Needs your decision.
- With two decision holds, Needs your decision shows one held-work row with the count while the status sentence includes two custody decisions. An owner-scoped response never shows another user's hold.

## Out of scope

An admin fleet view (the Workers page has it), the issue-work line (#3879), usage (#3881), any API/DTO, migration or CLI change, and `.github/workflows/**` edits in implementation or validation. Custody admission, hold classification and release authority remain unchanged.

## Shipped seams and prior work

- PR #3952 shipped `web/src/pages/OverviewNext.tsx`: `useOverviewData({ includeAdminRuns: true })` supplies the viewer's own `data.runs` and `data.workers`; `overviewDecisions(ownRuns)` feeds `NeedsDecisionCard`; `OverviewRunsWidget` already owns the left column. The preview still renders `CustodyBoardAlert` with `ownRecoveryWaitCount(ownRuns)` and `WorkerLoadCard` with `data.workers`. `/dashboard` shares the hook but must keep both current sections. The mock scenarios are `overview-next` and `overview-calm` in `web/src/mocks/`; extend the former for capacity and custody without changing the calm contract.
- PRD #1349 and merged PR #2457 set the custody alert's current severity, headlines and no-action-when-no-decision behavior. PR #2993 added per-hold admission membership to the Workers held-work surface; this preview uses the existing owner aggregate, not a second admission classifier. PR #3170 made `custody_hold_limit` configurable; never hardcode the mock's eight slots. PR #2860 made `WorkerRunBadge.hideIdleCrossChecks` available; the new compact rows may reuse it, while dedicated cross-check capacity remains separate.
- Closed and merged work checked on 2026-11-05: no earlier Capacity rail or held-work decision row for `/overview/next`. #3878 carried over the old banner and load card; it did not implement this PRD's replacements.

## Modules and seams

- **`capacitySlots(workers)`** (pure, `web/src/lib`): return in-use, closed and open counts, over-cap excess and counts of workers excluded from the bar. `Worker.active_runs` and `max_concurrent_runs` (`web/src/lib/apiTypes.ts`, `Worker`) are the run-lane numerator and advertised cap. For an online worker with a known nonnegative cap, in-use is `min(active_runs, cap)` and over-cap excess is `max(active_runs - cap, 0)`; free slots are closed when `draining_since != null`, `manual_cordoned_at != null`, `effective_cordoned === true` or a hosted upgrade is in progress, otherwise open. Offline workers and workers with a null/unknown cap remain in the list but contribute no bar segment; label those exclusions. No segment is negative. Dedicated `active_cross_checks` / `max_cross_check_slots` are separate; legacy cross-checks still included in `active_runs` count toward the advertised run cap. "Open" means advertised spare capacity, not guaranteed schedulability.
- **`CapacityRail`**: reuse `WorkerRunBadge`, `WorkerCordonBadge` and `WorkerStatLine` (`web/src/components/WorkerStats.tsx`); the current load card's `hasStats` filter must not hide workers from this rail. Reuse `WorkerUpgradeBadge`'s typed `upgrade_status` and `upgrade_target` evidence for any roll copy. Keep worker names and reported upgrade text through `stripUnsafeChars` and demo masking where the preview already applies it.
- **Owner holds load**: `api.getRecoveryHolds()` calls owner-scoped `GET /api/recovery/holds?state=open` (`web/src/lib/api.ts`; `api/internal/handler/recovery.go:ListRecoveryHolds` under `RequireUser`). Add its `RecoveryCustodyHolds` result to the preview's `useOverviewData` path as an **independent**, 10 s visible poll with last-good data on failure. Do not put it in the personal first-load `Promise.all`, tie it to a possibly hung runs/workers poll, or make a holds error blank the page. Remove the preview's `CustodyBoardAlert` in the same change, so it does not fetch the endpoint a second time. `/dashboard` keeps the alert's own fetch.
- **`HeldWorkCard`**: use `recoveryCapacityUsed` for the custody slots (`admission_counted_holds ?? open_holds`) and the aggregate's `open_holds`, `decision_needed`, `blocked_runs` directly (`web/src/lib/recovery.ts:recoveryCapacityUsed`, `custodyAlertView`). `custodyAlertView` is nullable because the old alert self-hides; the new card still renders the three figures when it returns null. Reuse its headline, danger/warning tone and priority unchanged when present. The Review link uses the existing `/workers?tab=workers#recovery-holds` destination only when `custodyAlertView` is nonnull and its `decisionNeeded > 0`.
- **Held-work decision item**: extend `overviewDecisions` in `web/src/lib/overview.ts` and `NeedsDecisionCard` with a discriminated held-work item; do not fabricate a `RunSummaryItem` for a custody hold. Pass the full owner `RecoveryCustodyHolds` response so the aggregate gates the row (`open_holds > 0` and `decision_needed > 0`) and its open decision holds (as classified by `custodyHoldView(...).needsDecision`) can supply an optional run id and age. Keep the row visible on an aggregate/list mismatch; the count remains aggregate-authoritative. `statusSentence` adds `aggregate.decision_needed` under the same gate to its own-run decision count, avoiding both undercounting several holds and counting the display row twice.
- The preview replaces both old sections while retaining `HealthPlatformLine` and the current usage card. No data from `adminListRuns` or `getAdminUsage` enters custody or capacity: the rail and held-work card are labelled yours, including for admins.

## Testing decisions

- Unit tests for `capacitySlots`: draining, manual and effective cordon, hosted upgrade, offline, over-cap, null cap, active dedicated versus legacy cross-check lanes, zero workers and nonnegative segments; a roll note requires actual upgrade evidence, not only a manual cordon or an outdated badge.
- `HeldWorkCard` tests use the current `CustodyBoardAlert.test.tsx` states: blocked, capacity reached, decision needed, healthy, no open hold, inconsistent `open_holds = 0` with `decision_needed > 0`, and `admission_counted_holds = 0` with positive `open_holds`. The headline and tone match `custodyAlertView`, and the Review link follows its positive `decisionNeeded` gate.
- Hook and page tests prove independent holds loading, last-good data after a failed poll, no duplicate holds fetch on the preview, both old sections removed there, and `/dashboard` unchanged. A held-work row appears only for positive aggregate `decision_needed`, stays visible on aggregate/list mismatch, never includes admin/foreign rows, and the header count uses the aggregate count. Watch a focused regression fail on the pre-fix seam and pass after each milestone.
- Extend `?mock=overview-next` with owner workers and open custody holds; keep `?mock=overview-calm` free of owner decisions. Compare side-by-side screenshots against the mock at desktop and 400 px in Ember and Dawn; list meaningful deviations in the PR body. Add focused browser cases for the rail/disclosure and narrow layout.

## Milestones

### M1: the capacity rail

**Blocked by**: none inside this PRD; #3878 is merged.

`capacitySlots`, the Capacity card and worker rows with the disclosure; the preview drops only its `WorkerLoadCard`. Acceptance: slot tests, focused page/browser checks and mock parity for the Capacity card; `/dashboard` keeps its Worker load card.

### M2: Held work and its decision row

**Blocked by**: M1 for shared preview layout.

An independent holds read in `useOverviewData`, `HeldWorkCard`, the discriminated Needs your decision row and header count; the preview drops `CustodyBoardAlert` in the same change. Acceptance: every custody state keeps its current headline and tone, the three figures always render, no Review link without a pending decision, and mock parity; `/dashboard` keeps its custody alert.

## In-run checks and landing obligations

Run `task check:web`, touched and directly affected Vitest files, focused browser cases for changed UI, `cd web && npm run build`, `task docs:sync` then `task check-docs:web` when docs or mock-scenario guidance changes, `task check:changelog-entry`, and in-run `task scan:secrets` plus `task sast:semgrep` because worker names and upgrade text are untrusted render inputs. Keep `.github/workflows/**` out of the branch and validation files.

**Landing obligations (outside milestones):** full `test:web`, `test:web-browser` and `gate:repo`, plus the mock image build, green on exact-head PR CI. The PR carries desktop and 400 px screenshots in both themes.

## Decision Log

- **Owner-scoped data, labelled as such** ("your fleet", "yours"): `listWorkers` and the recovery holds response are the viewer's. Rejected: an admin fleet view here, which belongs on the Workers page.
- **Retire the custody banner only together with the Held work card**, so the preview never loses a custody state.
- **Draining is neutral**, never an attention colour (brief D5); an outdated version or manual cordon alone is not a pending-roll receipt.
- **One held-work display row, aggregate decision count**: avoid duplicating the row for multiple holds while the header counts all pending decisions. Use a listed decision hold for age/run context when present, and a generic row on a list/aggregate mismatch; no new API field or inferred custody authority.
