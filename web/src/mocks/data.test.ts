import { describe, it, expect } from "vitest";
import { mockBoards, mockLimitWaitMessages, mockMyTokenRateLimits, mockRuns, mockSecrets } from "./data";

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

// PRD #35. The parked fixture is the ONLY way the run view's countdown, the board's
// warn pill, and the two new feed rows are reachable in mock mode — and mock mode is
// how this feature gets browser-validated at all, because a non-mock `vite dev` /
// `vite preview` of this repo proxies /api at whatever real stack is running.
//
// So the fixture is a test artefact, and these pin the properties that make it able
// to catch anything. Every one of them is satisfiable by a fixture that looks fine.
describe("the parked-run fixture can actually discriminate (PRD #35)", () => {
  const parked = mockRuns.find((r) => r.id === "run-limit-wait");

  it("exists and is the demo's only limit_wait run", () => {
    expect(parked, "no run-limit-wait fixture — nothing in mock mode renders a park").toBeDefined();
    expect(mockRuns.filter((r) => r.status === "limit_wait")).toHaveLength(1);
  });

  it("🔴 stamps retry_not_before EARLIER than limit_resets_at", () => {
    // Not a fudge and not an offset. The stamp is pool-aware: an owner whose second
    // credential still has headroom is promoted before the dead credential's window
    // rolls over, so earlier is the NORMAL case. A fixture with the two equal — or
    // with the reset first — lets a countdown wired to the wrong field look correct
    // in every browser pass anyone will ever do.
    const retry = Date.parse(parked!.retry_not_before!);
    const resets = Date.parse(parked!.limit_resets_at!);
    expect(Number.isFinite(retry) && Number.isFinite(resets)).toBe(true);
    expect(retry).toBeLessThan(resets);
  });

  it("puts both instants in the FUTURE, so the countdown is the state on screen", () => {
    // Everything else in data.ts is minsAgo. A park seeded in the past renders
    // "Resuming shortly" — a real state, but not the one anyone opens this to see.
    expect(Date.parse(parked!.retry_not_before!)).toBeGreaterThan(Date.now());
    expect(Date.parse(parked!.limit_resets_at!)).toBeGreaterThan(Date.now());
  });

  it("🔴 carries health 'ok', never a stale flag", () => {
    // The server CLEARS health on park entry, because its health detector's allowlist
    // never revisits a parked run and a flag live at park time would freeze for the
    // whole park. A fixture carrying "stalled" here would reproduce that bug in the
    // demo and look entirely plausible while doing it.
    expect(parked!.health).toBe("ok");
    expect(parked!.health_reason).toBeNull();
    expect(parked!.health_since).toBeNull();
  });

  it("has parked more than once, so the 'attempt N' clause is exercised", () => {
    // attempt 1 is deliberately suppressed as noise, so a count of 1 would leave that
    // branch invisible in the demo.
    expect(parked!.limit_wait_count).toBeGreaterThan(1);
  });

  it("is opted in and names a window this build knows", () => {
    expect(parked!.wait_on_limit).toBe(true);
    expect(parked!.rate_limit_type).toBe("five_hour");
  });

  it("has a board card, so the warn pill is reachable without opening the run", () => {
    const card = mockBoards["repo-uzi"]?.cards.find((c) => c.latest_run?.id === "run-limit-wait");
    expect(card, "the parked run has no card — runBadge's limit_wait arm renders nowhere").toBeDefined();
    expect(card!.latest_run!.status).toBe("limit_wait");
  });

  it("carries BOTH new message kinds in its stream", () => {
    // They render differently (warn vs. danger) and one run can only have parked, so
    // the limit_hit is a replayed death from an opted-out run — present precisely so
    // the danger variant is visible somewhere in the demo.
    const kinds = new Set(mockLimitWaitMessages.map((m) => m.kind));
    expect(kinds.has("limit_wait")).toBe(true);
    expect(kinds.has("limit_hit")).toBe(true);
  });

  it("carries a NUMERIC resets_at somewhere, exercising the seconds/millis arm", () => {
    // The wire carries the reset as an epoch number. A fixture that only ever used an
    // ISO string would leave parseFeedInstant's numeric promotion untested by the one
    // artefact a human actually looks at.
    const numeric = mockLimitWaitMessages.filter(
      (m) => typeof (m.payload as { resets_at?: unknown } | null)?.resets_at === "number",
    );
    expect(numeric.length).toBeGreaterThan(0);
  });
});
