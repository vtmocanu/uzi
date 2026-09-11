# fixtures/milestone-attribution

A **cross-language golden** for the per-milestone agent attribution join contract (PRD
#1224). `cases.json` is a hand-authored, hand-maintained array of NINE attribution cases,
asserted by BOTH surfaces that must agree on the join:

- **Go** (`api/cmd/uzi`): `TestMilestoneAttributionCrossSurfaceFixture` in
  `api/cmd/uzi/milestone_attribution_fixture_test.go`, driving the TUI/CLI helpers
  `effectiveMilestoneAgents` / `uniqueMilestoneAgentMatch` (`tui_detail_rail.go`).
- **TypeScript** (`web/src/lib`): `milestoneAttribution.fixture.test.ts`, driving the web
  helpers `effectiveMilestoneAgents` / `uniqueLiveMatchMilestoneId` (`runBadge.ts`).

Neither test reads the other: each folds its OWN production join helpers over the SAME
JSON, so a mismatch names the drifting surface. This is the one thing the per-surface
render tests (M5/M6) cannot catch — web-TS and cmd/uzi-Go silently diverging on the join
logic itself.

## What each case pins

Each case supplies a run's frozen `milestones` (ids only), `milestones_completed`,
`milestones_in_progress` (deliberately NOT in frozen order — it is a membership set, and
the output must come out in FROZEN order), `milestones_agents` (or `null`), and the live
`activity_agent`. `expected` carries:

- `effective_ids` — the ids of the entries whose milestone is genuinely in progress AND NOT
  already completed, after the D6 read-time re-filter, deduped first-wins, **in FROZEN order**.
  The Go half asserts this as a SET (its helper returns a map); the TS half asserts it as an
  ORDERED array (frozen order — the M5 carry-forward the render tests never pinned directly).
- `unique_match_id` — the single effective id whose declared `agent` byte-matches
  `activity_agent`, else `null` (on 0 or 2+ matches). `null` maps to `""` on the Go side.

## The mutations this fixture reddens

- Flipping the output order from frozen to `milestones_in_progress` order. TWO cases scramble
  the in-progress list against frozen order so a frozen-order break shows up in the TS ordered
  assertion: `two-in-progress-unique-match` (in-progress `["m3","m2"]`) and
  `frozen-order-scrambled-second` (three in-progress, `["m4","m2","m3"]`, expected out
  `["m2","m3","m4"]`).
- Dropping the D6 stale re-filter (`mixed-valid-and-stale-D6-refilter`: a stale entry whose
  id is not in the live in-progress set must NOT appear).
- Dropping the completed re-filter (`id-both-completed-and-in-progress-excluded`: a milestone
  reported in BOTH `milestones_completed` and `milestones_in_progress` — reachable because the
  server validates the two sets independently — must be excluded from the effective set, since
  the render marks it ✓ done and a done milestone never shows a live "now working" strip). This
  is now a **pinned cross-surface contract**: the Go helper (`tui_detail_rail.go`) already
  excluded completed ids, and the TS helper (`runBadge.ts`) now does too, so both surfaces
  suppress the strip on a done-but-still-reported-in-progress milestone identically.
- Changing the duplicate-id rule (`duplicate-id-first-valid-wins`: the first entry's agent,
  not the last, decides the unique match).
- Repeated-role / zero-match ambiguity collapsing to a wrong single match
  (`repeated-role-suppressed`, `zero-match`: `unique_match_id` is `null`).

## No `-update` flag

This golden is hand-maintained: `expected` is computed by hand from the join rules, never
regenerated. Change a case only when the join contract itself changes, and update BOTH
surfaces' expectations together.

## `-count=1` (Go only)

`fixtures/` sits ABOVE the `api/` module, so a fixture-only edit contributes nothing to the
`api/cmd/uzi` package's test-cache key: a bare `go test ./cmd/uzi/` can print `ok (cached)`
over a changed fixture. `task test:api` / `task gate:api` pass `-count=1`, which is what
makes the Go half live; run it explicitly with `-count=1` when validating a fixture change.
