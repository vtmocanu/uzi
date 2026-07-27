import { describe, it, expect } from "vitest";
import { mockMyTokenRateLimits, mockSecrets } from "./data";

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
