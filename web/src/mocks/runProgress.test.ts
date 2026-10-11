// The mock server's progress derivation must agree with the Go runToDTO for every recorded
// parity case (fixtures/run-progress/parity.json), so mock-mode screenshots show what a real
// server would send. It also proves the default mock scenario exhibits every board state.
import { describe, expect, it } from "vitest";
import parity from "../../../fixtures/run-progress/parity.json";
import type { Run } from "../lib/apiTypes";
import { deriveMockProgress } from "./runProgress";
import { state } from "./store";

describe("deriveMockProgress agrees with the server (PRD #2602)", () => {
  it.each((parity as Array<{ name: string; progress: unknown }>).map((c) => [c.name, c] as const))(
    "%s",
    (_name, c) => {
      // The fixture case carries exactly the Run fields the rules read.
      expect(deriveMockProgress(c as unknown as Run)).toEqual(c.progress);
    },
  );

  it("is null for a terminal run", () => {
    expect(deriveMockProgress({ status: "completed", kind: "issue" } as Run)).toBeNull();
  });
});

describe("mock-mode seed shows every board progress state", () => {
  it("covers percent, waiting, stalled, parked, queued, planning and none", () => {
    const states = new Set([...state.runs.values()].map((r) => r.progress?.state).filter(Boolean));
    for (const s of ["percent", "waiting", "stalled", "parked", "queued", "planning", "none"]) {
      expect(states, s).toContain(s);
    }
  });
});
