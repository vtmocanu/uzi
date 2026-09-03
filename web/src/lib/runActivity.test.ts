import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { activityAge, latestActivity } from "./runActivity";
import type { RunActivity, RunMessage } from "./apiTypes";

// The CROSS-MODULE drift guard (PRD #1064 R5). This asserts latestActivity against the
// SAME repo-root golden fixture the Go test (api/internal/runactivity/runactivity_test.go)
// asserts Latest+FromFrame against, so the two copies of the selection-and-fold rule
// cannot drift: change one and this file (or the Go test) reddens. Read the fixture at
// test time via node:fs, following apiContract.test.ts's exact pattern (the web bundle
// ships no @types/node — the single readFileSync signature is declared in node-fs.d.ts).
//
// The fixture is the SPEC, not a recording: it is hand-authored and owned by neither
// module. Its frame shape is the language-neutral Frame {kind, agent, agent_label,
// payload, created_at, seq}; RunMessage additionally carries agent_instance, so the
// bridge below adds `agent_instance: null` (latestActivity does not read it — the
// dispatch identity it folds comes from the payload, not the frame column).

interface FixtureFrame {
  kind: string;
  agent: string | null;
  agent_label: string | null;
  payload: unknown;
  created_at: string;
  seq: number;
}

interface FixtureCase {
  name: string;
  frames: FixtureFrame[];
  expected: RunActivity | null;
}

function loadCases(): FixtureCase[] {
  // import.meta.url is …/web/src/lib/runActivity.test.ts; the fixture is at the repo
  // root, three levels up (../../../), exactly as apiContract.test.ts resolves its
  // api-contract fixtures.
  const url = new URL("../../../fixtures/run-activity/cases.json", import.meta.url);
  const raw = readFileSync(url, "utf8");
  const parsed = JSON.parse(raw) as { cases: FixtureCase[] };
  return parsed.cases;
}

// bridgeFrame maps a language-neutral fixture Frame to the RunMessage shape
// latestActivity consumes.
function bridgeFrame(f: FixtureFrame): RunMessage {
  return {
    seq: f.seq,
    kind: f.kind,
    agent: f.agent,
    agent_instance: null,
    agent_label: f.agent_label,
    payload: f.payload,
    created_at: f.created_at,
  };
}

describe("latestActivity vs the shared golden fixture (PRD #1064 R5)", () => {
  const cases = loadCases();

  // A truncated or emptied fixture must redden, not silently pass with zero assertions —
  // the same false-green shape apiContract.test.ts guards against. The Go side lists the
  // required cases (D3); this floor keeps the two roughly in step without hard-coding the
  // exact count (a case added to the file is auto-exercised by the loop below).
  it("loads a non-empty case list from the repo-root fixture", () => {
    expect(Array.isArray(cases)).toBe(true);
    expect(cases.length).toBeGreaterThanOrEqual(15);
    // Every case has a unique name, so the every-case loop cannot silently collapse two.
    const names = new Set(cases.map((c) => c.name));
    expect(names.size).toBe(cases.length);
  });

  // EVERY case in the file is asserted (the every-case-exercised check): the loop is
  // driven by the file's own array, so a case added to cases.json is asserted here with
  // no code change, and one that is dropped stops being asserted — the drift guard.
  for (const c of cases) {
    it(`case: ${c.name}`, () => {
      const activity = latestActivity(c.frames.map(bridgeFrame));
      expect(activity).toEqual(c.expected);
    });
  }
});

describe("activityAge", () => {
  it("renders a compact elapsed token from the frame instant", () => {
    const at = "2026-09-03T12:00:00Z";
    const now = Date.parse(at) + 40_000;
    expect(activityAge(at, now)).toBe("40s");
    expect(activityAge(at, Date.parse(at) + 6 * 60_000)).toBe("6m");
  });

  it("returns '' for an unparseable instant rather than a fabricated token", () => {
    expect(activityAge("not-a-date", Date.now())).toBe("");
  });
});
