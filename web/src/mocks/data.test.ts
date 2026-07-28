import { describe, it, expect } from "vitest";
import { mockBoards, mockMyTokenRateLimits, mockSecrets } from "./data";

// The demo build is a shipped artifact, and these two fixtures describe the SAME
// tokens to two different parts of one settings row: the toggle comes from
// mockSecrets, the eligibility chip from mockMyTokenRateLimits.
//
// 🔴 THEY SHIPPED INVERTED. A cold load of /settings rendered `console-key` with a
// CHECKED toggle beside a chip reading "not in pool", deterministic across reloads,
// and the healthy "in pool" chip that the fixture's own comment calls "the point"
// rendered on no row at all. A browser pass found it; nothing in the suite did,
// because no test compared the two lists — re-inverting them today still leaves all
// 85 mock tests green, which is why this file exists.
describe("mock token fixtures agree with each other (PRD #111, web-ux F1)", () => {
  const anthropic = mockSecrets.filter((s) => s.kind === "anthropic_token");

  it("describes the same set of tokens in both lists", () => {
    expect(new Set(mockMyTokenRateLimits.map((t) => t.secret_id))).toEqual(
      new Set(anthropic.map((s) => s.id)),
    );
  });

  it("agrees on auto_eligible token for token", () => {
    for (const secret of anthropic) {
      const meter = mockMyTokenRateLimits.find((t) => t.secret_id === secret.id);
      expect(meter, `no meter fixture for ${secret.id}`).toBeDefined();
      expect(
        meter!.auto_eligible,
        `${secret.label}: mockSecrets says auto_eligible=${secret.auto_eligible} but its meter says ` +
          `${meter!.auto_eligible} — the settings row would draw the toggle from one and the chip ` +
          `from the other, asserting a contradiction about which account gets spent`,
      ).toBe(secret.auto_eligible);
    }
  });

  it("never pairs a pooled token with a not_pooled status, or the reverse", () => {
    for (const meter of mockMyTokenRateLimits) {
      if (meter.auto_eligible) {
        expect(meter.auto_status, `${meter.label} is pooled but its status says not_pooled`).not.toBe(
          "not_pooled",
        );
      } else {
        expect(meter.auto_status, `${meter.label} is not pooled, so its status must be not_pooled`).toBe(
          "not_pooled",
        );
      }
    }
  });

  // web-ux F2: four of the six states were unreachable in the demo, and they are the
  // four the feature exists for — a user could not see what "opted in but never
  // pickable" looks like without a live poller. Asserted as a COUNT of distinct
  // statuses rather than by name, so adding a fifth state to the demo is free while
  // dropping back to the two trivial ones is not.
  it("makes more than the two trivial eligibility states browsable", () => {
    const statuses = new Set(mockMyTokenRateLimits.map((t) => t.auto_status));
    expect(
      statuses.size,
      `the demo shows only ${[...statuses].join(", ")} — the states that matter ` +
        `(never polled, stale, no usage data, low headroom) cannot be seen without a live poller`,
    ).toBeGreaterThanOrEqual(3);
  });
});

// The demo board is the one build most people click, so a feature that is invisible
// there is a feature nobody sees (web-ux S7). Each of these pinned a fixture that was
// silently arguing against a shipped decision.
describe("the demo board exercises the board's features (PRD #102, web-ux S7)", () => {
  const boards = Object.entries(mockBoards);

  it("renders in ascending issue number, like a board nobody has dragged", () => {
    // The fixtures were authored DESCENDING, so `Manual` mode visibly was not issue
    // order on the demo board — contradicting on screen the safety argument Decision
    // 7a makes for shipping Manual as the default. The real server's untouched-board
    // clause is `board_position ASC NULLS LAST, forge_issue_iid ASC` with every
    // position NULL, which is exactly ascending iid.
    for (const [id, board] of boards) {
      const iids = board.cards.map((c) => c.iid);
      expect(iids, `${id} renders out of issue order`).toEqual([...iids].sort((a, b) => a - b));
    }
  });

  it("gives some card a content label, so the chips render on more than zero cards", () => {
    // Not one mock card carried a content label, so M4's label chips rendered nowhere
    // in the demo build. Workflow labels do not count — chipLabels excludes them, and
    // a fixture full of those would satisfy a naive "has labels" check while showing
    // no chip.
    //
    // Scoped to PRD cards ON PURPOSE. The non-PRD fixtures added for M6 carry content
    // labels too, so counting every card would let this pass while the DEFAULT board
    // — toggle off — still showed no chip anywhere. That weaker version passed against
    // the unfixed fixtures when it was first written.
    const workflow = new Set(["PRD", "PRDLESS", "autopilot", "uzi-self-improve"]);
    const chipworthy = boards.flatMap(([, b]) => {
      const columns = new Set(b.columns.map((col) => col.label_name));
      return b.cards
        .filter((c) => c.labels.includes("PRD"))
        .flatMap((c) => c.labels.filter((l) => !workflow.has(l) && !columns.has(l)));
    });
    expect(chipworthy.length).toBeGreaterThan(0);
  });

  it("ships open non-PRD cards, so the M6 toggle demonstrates something", () => {
    // Default-off plus no non-PRD cards is a control that visibly does nothing.
    const nonPRD = boards.flatMap(([, b]) =>
      b.cards.filter((c) => !c.closed && !c.labels.includes("PRD") && !c.labels.includes("uzi-self-improve")),
    );
    expect(nonPRD.length).toBeGreaterThan(0);
    // At least one with NO labels at all: the ordinary shape of a freshly filed issue,
    // and the shape whose labels used to reach the wire as JSON null.
    expect(nonPRD.some((c) => c.labels.length === 0)).toBe(true);
  });
});
