// @vitest-environment jsdom
//
// PRD #3250 M2: the web half of the shared start-option fixture. The Go handler test
// (api/internal/handler/run_start_options_livedb_test.go) iterates the same
// fixtures/run-start-options/cases.json against the real create route; this iterates it
// against the mock createRun, so the demo build refuses exactly what the server refuses
// and stamps the same four fields. Every case is asserted visited.
import { afterAll, describe, expect, it } from "vitest";
import { ApiError } from "../lib/apiError";
import type { Harness, RunStartBody } from "../lib/api";
import { mockApi } from "./mockApi";
import { state } from "./store";
import cases from "../../../fixtures/run-start-options/cases.json";

interface StartOptionCase {
  name: string;
  harness: Harness;
  model?: string;
  effort?: string;
  expect: { status: 201 | 400 | 422; code?: string; harness?: Harness };
}

const fixture = cases as { cases: StartOptionCase[] };

// A runnable seeded card whose prior runs are retired before each case, so the
// active-run and open-MR gates never answer for the option under test.
const REPO = "repo-uzi";
const IID = 31;
function retireRunsOnIssue() {
  for (const r of state.runs.values()) {
    if (r.repo_id === REPO && r.issue_iid === IID) r.status = "cancelled";
  }
}

const visited = new Set<string>();

describe("mock createRun enforces fixtures/run-start-options/cases.json", () => {
  it("has a non-trivial corpus with every status class", () => {
    expect(fixture.cases.length).toBeGreaterThanOrEqual(20);
    expect(new Set(fixture.cases.map((c) => c.expect.status))).toEqual(new Set([201, 400, 422]));
  });

  for (const c of fixture.cases) {
    it(`${c.name} → ${c.expect.status}`, async () => {
      visited.add(c.name);
      retireRunsOnIssue();
      const start: RunStartBody = { harness: c.harness };
      if (c.model !== undefined) start.model = c.model;
      if (c.effort !== undefined) start.effort = c.effort;
      const before = state.runs.size;
      if (c.expect.status === 201) {
        const { run } = await mockApi.createRun(REPO, IID, start, true);
        expect(run.harness).toBe(c.harness);
        // Only supplied values are stamped; an absent setting leaves its pair null.
        expect(run.model).toBe(c.model ?? null);
        expect(run.model_source).toBe(c.model !== undefined ? "run_override" : null);
        expect(run.effort).toBe(c.effort ?? null);
        expect(run.effort_source).toBe(c.effort !== undefined ? "run_override" : null);
        return;
      }
      const err = await mockApi.createRun(REPO, IID, start, true).then(
        () => null,
        (e: unknown) => e,
      );
      expect(err).toBeInstanceOf(ApiError);
      const apiErr = err as ApiError;
      expect(apiErr.status).toBe(c.expect.status);
      if (c.expect.status === 422) {
        // The code and harness only, never the remedy list (the fixture's contract).
        expect(apiErr.body).toMatchObject({ code: c.expect.code, harness: c.expect.harness });
      }
      // A refusal creates no run.
      expect(state.runs.size).toBe(before);
    });
  }

  afterAll(() => {
    expect([...visited].sort()).toEqual(fixture.cases.map((c) => c.name).sort());
  });
});

describe("mock createRun one-click defaults", () => {
  it("an empty start object stamps no model or effort", async () => {
    retireRunsOnIssue();
    const { run } = await mockApi.createRun(REPO, IID, {}, true);
    expect(run.model).toBeNull();
    expect(run.model_source).toBeNull();
    expect(run.effort).toBeNull();
    expect(run.effort_source).toBeNull();
    expect(run.credential_override).toBeNull();
  });
});
