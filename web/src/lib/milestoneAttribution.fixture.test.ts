import { describe, it, expect } from "vitest";
import cases from "../../../fixtures/milestone-attribution/cases.json";
import {
  effectiveMilestoneAgents,
  uniqueLiveMatchMilestoneId,
} from "./runBadge";
import type { Milestone, MilestoneAgent } from "./api";

// milestoneAttribution.fixture.test.ts is the TYPESCRIPT HALF of a cross-language golden for the
// per-milestone agent attribution join (PRD #1224). The Go half is
// api/cmd/uzi/milestone_attribution_fixture_test.go. Neither reads the other: each folds its OWN
// production join helpers over the SAME fixture (fixtures/milestone-attribution/cases.json), so a
// mismatch NAMES the drifting surface — the one thing the per-surface render tests (M5/M6) cannot
// catch, since they exercise web-TS and cmd/uzi-Go in isolation.
//
// This TS side drives effectiveMilestoneAgents + uniqueLiveMatchMilestoneId (runBadge.ts). Unlike
// the Go half (whose helper returns a map, so it asserts membership as a SET), this half asserts
// the returned array's id order IS the FROZEN milestone order — the M5 carry-forward: a direct unit
// test of runBadge's ordering, which no existing test pinned. It reddens a case under any of:
//   - flipping the output order from FROZEN (run.milestones) to milestones_in_progress order,
//   - dropping the D6 read-time stale re-filter (an entry whose id is no longer in progress),
//   - changing the duplicate-id rule (first-wins, so the first entry's agent decides the match).

describe("PRD #1224: per-milestone attribution cross-surface fixture (web half)", () => {
  for (const c of cases) {
    it(c.name, () => {
      const run = {
        milestones: c.milestones.map(
          (id): Milestone => ({ id, title: id }),
        ),
        milestones_in_progress: c.milestones_in_progress,
        milestones_agents: c.milestones_agents as MilestoneAgent[] | null,
      };

      // effective membership + ORDER: the returned array's ids equal expected.effective_ids IN
      // ORDER (frozen milestone order — milestones_in_progress is deliberately scrambled in the
      // fixture, so a frozen-order break surfaces here).
      const effective = effectiveMilestoneAgents(run);
      expect(effective.map((e) => e.id)).toEqual(c.expected.effective_ids);

      // unique live match: null on 0 or 2+ matches (a JSON null expected maps to null).
      const unique = uniqueLiveMatchMilestoneId(effective, c.activity_agent);
      expect(unique).toBe(c.expected.unique_match_id ?? null);
    });
  }
});
