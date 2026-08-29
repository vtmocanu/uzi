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
  const due = mockRuns.find((r) => r.id === "run-limit-wait-due");

  it("exists alongside a second, DEGRADED parked fixture", () => {
    // Two, not one. The second exists because the first cannot reach the degraded
    // countdown states — expired stamp, multi-day window, suppressed attempt — and
    // the browser validator had to override Date.now() inside the page to see them.
    // A state reachable only by patching the clock regresses silently.
    expect(parked, "no run-limit-wait fixture — nothing in mock mode renders a park").toBeDefined();
    expect(due, "no run-limit-wait-due fixture — the expired-countdown branch is unreachable").toBeDefined();
    expect(mockRuns.filter((r) => r.status === "limit_wait")).toHaveLength(2);
  });

  it("🔴 the second fixture reaches the branches the first cannot", () => {
    // Each assertion here is one branch that would otherwise need a patched clock.
    // retry_not_before in the PAST -> "Resuming shortly" instead of a countdown.
    expect(Date.parse(due!.retry_not_before!)).toBeLessThan(Date.now());
    // A multi-day window -> formatCountdown's "Nd Nh" arm and the long-horizon reset.
    expect(due!.rate_limit_type).toBe("seven_day");
    expect(Date.parse(due!.limit_resets_at!) - Date.now()).toBeGreaterThan(4 * 86_400_000);
    // count == 1 -> the SUPPRESSED attempt clause, the opposite of the first fixture.
    expect(due!.limit_wait_count).toBe(1);
    // Still the pool-aware ordering, and by days rather than an hour.
    expect(Date.parse(due!.retry_not_before!)).toBeLessThan(Date.parse(due!.limit_resets_at!));
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

  it("🔴 models the SHIPPED payload: ISO resets_at, keys omitted, and NO attempt", () => {
    // The fixture is the demo's stand-in for the wire, so a shape the worker cannot
    // emit teaches the wrong thing to everyone who reads it — and would let a
    // renderer that only handles that shape look correct in a browser pass.
    //
    // `attempt` was in PRD Decision 10 and was dropped: the count is incremented
    // server-side AFTER this message is written, so any worker-supplied value is a
    // stale N-1. Asserted as an ABSENCE because re-adding it is the tempting mistake.
    for (const msg of mockLimitWaitMessages) {
      if (msg.kind !== "limit_wait" && msg.kind !== "limit_hit") continue;
      const payload = (msg.payload ?? {}) as Record<string, unknown>;
      expect(Object.keys(payload).sort()).toEqual(
        Object.keys(payload).filter((k) => k === "rate_limit_type" || k === "resets_at").sort(),
      );
      expect("attempt" in payload, `seq ${msg.seq} carries an attempt key the worker never sends`).toBe(false);
      if ("resets_at" in payload) {
        expect(typeof payload["resets_at"], `seq ${msg.seq}: resets_at must be an ISO string`).toBe("string");
        expect(Number.isFinite(Date.parse(payload["resets_at"] as string))).toBe(true);
      }
    }
  });

  it("covers the OMITTED-key shape, which is how 'unknown' arrives", () => {
    // Both keys are left out rather than sent as null, so "unknown" has exactly one
    // shape. A fixture without this case never renders the bare row.
    const bare = mockLimitWaitMessages.filter(
      (m) => m.kind === "limit_wait" && Object.keys((m.payload ?? {}) as object).length === 0,
    );
    expect(bare.length).toBeGreaterThan(0);
  });

  it("distinguishes its two parks by resets_at, the only field that can", () => {
    // With no count on the payload, resets_at is what stops two parks reading as
    // duplicate rows. It differs per park by construction; the fixture must not
    // accidentally reuse one value.
    const resets = mockLimitWaitMessages
      .filter((m) => m.kind === "limit_wait")
      .map((m) => (m.payload as { resets_at?: string } | null)?.resets_at)
      .filter((v): v is string => typeof v === "string");
    expect(resets.length).toBeGreaterThan(1);
    expect(new Set(resets).size).toBe(resets.length);
  });
});

// PRD #517. The follow-up park (awaiting_followup) is a first-class parked state, but
// unlike limit_wait / awaiting_approval / awaiting_input it had NO seeded fixture, so
// its whole web surface — the board follow-up strip, the run view's park announcement,
// SteerQueueCard's "resumes the run" chip — was unreachable under VITE_UZI_MOCK=1. This
// fixture makes it reachable; these pin the properties that make it able to catch
// anything.
describe("the follow-up-parked fixture can actually discriminate (PRD #517)", () => {
  const parked = mockRuns.find((r) => r.id === "run-awaiting-followup");

  it("exists in mockRuns at status awaiting_followup", () => {
    expect(parked, "no run-awaiting-followup fixture — nothing in mock mode renders a follow-up park").toBeDefined();
    expect(parked!.status).toBe("awaiting_followup");
  });

  it("has a board card, so runBadge's awaiting_followup arm is reachable without opening the run", () => {
    const card = mockBoards["repo-uzi"]?.cards.find((c) => c.latest_run?.id === "run-awaiting-followup");
    expect(card, "the follow-up-parked run has no card — runBadge's awaiting_followup arm renders nowhere").toBeDefined();
    expect(card!.latest_run!.status).toBe("awaiting_followup");
  });

  it("🔴 carries health 'ok', never a stale flag", () => {
    // The server CLEARS health on park entry (same invariant the limit_wait fixture pins):
    // a flag live at park time would freeze for the whole park, so a fixture carrying one
    // here would reproduce that bug in the demo and look entirely plausible while doing it.
    expect(parked!.health).toBe("ok");
    expect(parked!.health_reason).toBeNull();
    expect(parked!.health_since).toBeNull();
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
    // Scoped to `uzi` (runnable) cards ON PURPOSE (PRD #764). The non-uzi fixtures carry
    // content labels too, so counting every card would let this pass while the DEFAULT
    // board — toggle off — still showed no chip anywhere. `uzi` is the runnable marker,
    // shown as a highlighted chip, so it does not count as a genuine CONTENT label here.
    const workflow = new Set(["uzi", "autopilot", "uzi-self-improve"]);
    const chipworthy = boards.flatMap(([, b]) => {
      const columns = new Set(b.columns.map((col) => col.label_name));
      return b.cards
        .filter((c) => c.labels.includes("uzi"))
        .flatMap((c) => c.labels.filter((l) => !workflow.has(l) && !columns.has(l)));
    });
    expect(chipworthy.length).toBeGreaterThan(0);
  });

  it("ships open non-uzi cards, so the 'Show all issues' toggle demonstrates something", () => {
    // Default-off plus no non-uzi cards is a control that visibly does nothing (PRD #764).
    const nonUzi = boards.flatMap(([, b]) =>
      b.cards.filter((c) => !c.closed && !c.labels.includes("uzi") && !c.labels.includes("uzi-self-improve")),
    );
    expect(nonUzi.length).toBeGreaterThan(0);
    // At least one with NO labels at all: the ordinary shape of a freshly filed issue,
    // and the shape whose labels used to reach the wire as JSON null.
    expect(nonUzi.some((c) => c.labels.length === 0)).toBe(true);
  });
});
