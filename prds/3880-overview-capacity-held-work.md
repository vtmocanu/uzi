# PRD #3880: Overview preview, capacity rail and held work

**Status**: Draft, parked (`Later`). Child of epic #3873. Blocked by: #3878 (the preview page and its data hook).

**Design brief**: `prds/3873-overview-redesign.md` (model-fidelity rules, data readiness, reuse map, constraints, mock contract). This PRD wins where it is more specific.

**Design mock**: `prds/mockups/3873-overview-redesign-mock.html`, the "Capacity" and "Held work" cards and the held-work row in Needs your decision. The brief's mock contract applies: implement the mock, small consistency tweaks are fine, larger deviations go under `Mock deviations` in the PR body with a reason, and the PR carries side-by-side screenshots.

## Problem

Worker load is a wall of cpu, mem and disk text per worker, and the free capacity is left to arithmetic. Custody pressure is a banner whose three numbers (slots used, open holds, decisions needed) need interpretation.

## Outcome

On `/overview/next`, a right-hand rail next to the Runs widget (stacking under it at narrow widths), replacing the worker-load card and the custody banner the preview carried over from today:

- **Capacity (your fleet)**: "6 of 8 run slots in use" and a segmented bar of in-use, closed and open slots, with a legend. Then one row per worker: today's status, runs and cordon or draining badges and the name; a disclosure shows today's cpu, mem and disk stat line as meters. A draining or cordoned worker shows today's badge and "Takes no new runs"; the roll note ("Rolls to vX once it is fully idle") appears only when the worker reports a pending upgrade (`WorkerUpgradeBadge`'s evidence), because a manual cordon also drains without any rollout.
- **Held work (yours)**: custody slots used of the limit as pips, open holds and holds needing a decision as three separate figures, and today's alert states with today's headlines: "Held work is blocking new runs" (danger), "Held work needs your attention", "Custody capacity reached". The Review held work link shows only when a decision is pending.
- **Needs your decision** gains the held-work row ("Held work from run X needs a decision", Review held work) when `decision_needed > 0`, by extending #3878's `overviewDecisions` with the holds aggregate as a second input. The command menu (#3875) lands after this PRD and lists the same selector output.

Acceptance examples:

- Blocked runs with `decision_needed = 0`: the Held work card shows the danger headline, there is no Review link, and Needs your decision has no held-work row.
- A draining worker with one free slot: its badge is neutral, and the free slot counts as closed, not open.
- With no custody hold open, the Held work card shows the slots and zero figures and no headline, and nothing in Needs your decision.

## Out of scope

An admin fleet view (the Workers page has it), the issue-work line (#3879), usage (#3881). Custody semantics are unchanged: this PRD presents today's aggregate.

## Modules and seams

- **`capacitySlots(workers)`** (pure, `web/src/lib`): in use, closed and open counts plus workers with an unknown cap. In use is `active_runs`; capacity is `max_concurrent_runs` (`web/src/lib/apiTypes.ts:2609`); a cordoned or draining worker's free slots are closed; an upgrading (rolling) worker's free slots are closed; an offline worker's slots are neither open nor in use: it is listed with its status and left out of the bar, with a note ("1 worker offline"); a worker with a null cap is listed but left out of the bar, with a note. Over capacity (`active_runs > max_concurrent_runs`) counts the cap as in use and adds "+N over"; no segment is ever negative. Cross-check lanes are not run slots (`WorkerRunBadge` already separates them). "Open" means advertised spare capacity, not guaranteed schedulability (brief model-fidelity rules).
- **`CapacityRail`**: reuses `WorkerRunBadge`, `WorkerCordonBadge` and `WorkerStatLine` (`web/src/components/WorkerStats.tsx:262`).
- **`HeldWorkCard`**: this PRD adds the holds aggregate (`GET /api/recovery/holds`) to #3878's `useOverviewData`, replacing `CustodyBoardAlert`'s own fetch on the preview, and derives everything from `custodyAlertView` (`web/src/lib/recovery.ts:202`) and `recoveryCapacityUsed`. No new custody semantics.
- The preview page stops rendering `CustodyBoardAlert` and the worker-load card in the same change that adds these cards, so no custody state is ever uncovered.

## Testing decisions

- Unit tests for `capacitySlots`: draining, cordoned, offline, over capacity, unknown cap, cross-check lanes, zero workers; a test that the roll note needs upgrade evidence.
- Component tests for `HeldWorkCard` covering every `custodyAlertView` state (`web/src/components/CustodyBoardAlert.test.tsx` is the pattern): blocked, capacity reached, decision needed, healthy (no headline), and no Review link without a pending decision.
- A page test that the preview no longer renders `CustodyBoardAlert` or the worker-load card, and that the decision row appears only with `decision_needed > 0`.
- Screenshots against the mock (desktop, 400 px, Ember and Dawn) using #3878's mock-mode scenario extended with workers and holds.

## Milestones

### M1: the capacity rail

**Blocked by**: none inside this PRD (the PRD is blocked by #3878).

`capacitySlots`, the Capacity card and worker rows with the disclosure; the preview drops its worker-load card.

Acceptance: slot tests; mock parity for the Capacity card.

### M2: Held work and its decision row

**Blocked by**: none inside this PRD.

`HeldWorkCard`, the Needs your decision row, and the preview drops `CustodyBoardAlert`.

Acceptance: every custody state renders with today's headline; no Review link without a pending decision; mock parity.

## Decision Log

- **Owner-scoped data, labelled as such** ("your fleet", "yours"): `listWorkers` and the recovery holds aggregate are the viewer's. Rejected: an admin fleet view here, which belongs on the Workers page.
- **Retire the custody banner only together with the Held work card**, so the preview never loses a custody state.
- **Draining is neutral**, never an attention colour (brief D5).
