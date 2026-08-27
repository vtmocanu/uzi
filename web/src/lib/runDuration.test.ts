import { describe, it, expect } from "vitest";
import type { LatestRun, Run } from "./api";
import { runDurationLabel, type RunDurationInput } from "./runDuration";

// A fixed clock so every anchor is built relative to a known NOW; the helper never
// reads Date.now, so the tokens are fully deterministic.
const NOW = Date.parse("2026-08-09T12:00:00Z");

// isoBefore returns an RFC3339 timestamp `minutes` minutes before NOW (fractional
// minutes allowed for the seconds cases).
function isoBefore(minutes: number): string {
  return new Date(NOW - minutes * 60_000).toISOString();
}

describe("runDurationLabel", () => {
  it("queued anchors on created_at", () => {
    expect(
      runDurationLabel(
        { status: "queued", created_at: isoBefore(4), updated_at: isoBefore(4) },
        NOW,
      ),
    ).toBe("queued 4m");
  });

  it("claimed anchors on claimed_at when present", () => {
    expect(
      runDurationLabel(
        {
          status: "claimed",
          created_at: isoBefore(10),
          updated_at: isoBefore(0.5),
          claimed_at: isoBefore(20 / 60), // 20 seconds
        },
        NOW,
      ),
    ).toBe("claimed 20s");
  });

  it("claimed falls back to created_at when claimed_at is null", () => {
    expect(
      runDurationLabel(
        {
          status: "claimed",
          created_at: isoBefore(3),
          updated_at: isoBefore(3),
          claimed_at: null,
        },
        NOW,
      ),
    ).toBe("claimed 3m");
  });

  it("running anchors on started_at when present", () => {
    expect(
      runDurationLabel(
        {
          status: "running",
          created_at: isoBefore(200),
          updated_at: isoBefore(1),
          started_at: isoBefore(90), // 1h 30m
        },
        NOW,
      ),
    ).toBe("running 1h 30m");
  });

  it("running falls back to created_at when started_at is null", () => {
    expect(
      runDurationLabel(
        {
          status: "running",
          created_at: isoBefore(5),
          updated_at: isoBefore(1),
          started_at: null,
        },
        NOW,
      ),
    ).toBe("running 5m");
  });

  it("awaiting_approval, awaiting_input, awaiting_followup, and limit_wait wait off updated_at", () => {
    // PRD #517: awaiting_followup joins the waiting arm — mutation guard: dropping it
    // makes runDurationLabel fall to the default "" and this assert reddens (the board
    // card would then show an empty duration token for a parked interactive run).
    for (const status of ["awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait"]) {
      expect(
        runDurationLabel(
          { status, created_at: isoBefore(30), updated_at: isoBefore(7) },
          NOW,
        ),
      ).toBe("waiting 7m");
    }
  });

  it("terminal completed reports the static ran-span", () => {
    const run: RunDurationInput = {
      status: "completed",
      created_at: isoBefore(200),
      updated_at: isoBefore(1),
      started_at: isoBefore(50),
      finished_at: isoBefore(8), // 50 - 8 = 42m
    };
    expect(runDurationLabel(run, NOW)).toBe("ran 42m");
  });

  it("terminal ran-span is independent of nowMs", () => {
    const run: RunDurationInput = {
      status: "completed",
      created_at: isoBefore(200),
      updated_at: isoBefore(1),
      started_at: "2026-08-09T10:00:00Z",
      finished_at: "2026-08-09T10:42:00Z",
    };
    const a = runDurationLabel(run, NOW);
    const b = runDurationLabel(run, Date.parse("2027-01-01T00:00:00Z"));
    expect(a).toBe("ran 42m");
    expect(b).toBe("ran 42m");
    expect(a).toBe(b);
  });

  it("terminal run that never started yields empty string", () => {
    for (const status of ["cancelled", "failed"]) {
      expect(
        runDurationLabel(
          {
            status,
            created_at: isoBefore(10),
            updated_at: isoBefore(2),
            started_at: null,
            finished_at: isoBefore(2),
          },
          NOW,
        ),
      ).toBe("");
    }
  });

  it("floors a future anchor (clock skew) to 0s", () => {
    expect(
      runDurationLabel(
        {
          status: "running",
          created_at: isoBefore(-1), // one minute in the future
          updated_at: isoBefore(0),
          started_at: isoBefore(-1),
        },
        NOW,
      ),
    ).toBe("running 0s");
  });

  it("returns empty string for an unknown status", () => {
    expect(
      runDurationLabel(
        {
          status: "banana",
          created_at: isoBefore(4),
          updated_at: isoBefore(4),
        },
        NOW,
      ),
    ).toBe("");
  });

  it("accepts a LatestRun-shaped object (no lifecycle fields) and degrades", () => {
    // Only status/created_at/updated_at present — the board's narrow projection. This
    // both exercises the fallback anchor and doubles as the structural-typing check.
    const board = {
      status: "running",
      created_at: isoBefore(12),
      updated_at: isoBefore(1),
    };
    expect(runDurationLabel(board, NOW)).toBe("running 12m");
  });

  it("type-checks against both Run and LatestRun (compile-time assertion)", () => {
    // These assignments only need to COMPILE; the runtime body is trivial. They pin
    // Decision 6: the optional-and-nullable fields accept Run (string | null) and
    // LatestRun (keys absent) alike.
    const asRun = (r: Run): RunDurationInput => r;
    const asLatest = (r: LatestRun): RunDurationInput => r;
    expect(typeof asRun).toBe("function");
    expect(typeof asLatest).toBe("function");
  });
});
