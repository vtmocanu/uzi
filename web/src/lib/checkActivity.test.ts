import { describe, expect, it } from "vitest";
import { activityStatusLine, checkActivityState } from "./checkActivity";

const counts = (ok: number) => ({ ok, denied: 0, oversize: 0, limit: 0, error: 0 });
const summary = (generation: number, over: Record<string, unknown> = {}) => ({
  kind: "status",
  payload: { event: "check_activity_summary", generation, family: "claude", stage: "plan",
    reads: counts(4), searches: counts(0), dropped: 0, untracked: 0, unsettled: 0, complete: true, ...over },
});
const marker = (generation: number) => ({ kind: "status", payload: { event: "check_activity_start", v: 1, cap: 200, generation } });

describe("activityStatusLine", () => {
  it("renders the start marker and the closed-shape lines", () => {
    expect(activityStatusLine({ event: "check_activity_start", v: 1, cap: 200, generation: 1 })).toBe("activity capture started");
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "ok", path: "api/x.go", lines: [1, 200] })).toBe("read api/x.go L1-200 · ok");
    expect(activityStatusLine({ event: "check_activity", op: "search", outcome: "ok", path: "api/internal/forge", matches: 3 })).toBe("search api/internal/forge · 3 matches · ok");
    expect(activityStatusLine({ event: "check_activity", op: "search", outcome: "ok", path: "p", matches: 1 })).toBe("search p · 1 match · ok");
    expect(activityStatusLine({ event: "check_activity", op: "search", outcome: "limit", path: "web", matches: 50, matches_truncated: true })).toBe("search web · 50+ matches · limit");
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "denied", category: "outside_checkout" })).toBe("read denied (outside checkout)");
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "denied" })).toBe("read denied");
  });
  it("renders summaries with singular and plural nouns", () => {
    expect(activityStatusLine(summary(1).payload)).toBe("activity summary: 4 reads, 0 searches, 0 dropped, complete");
    expect(activityStatusLine(summary(1, { reads: counts(1), searches: counts(1), dropped: 2, complete: false }).payload))
      .toBe("activity summary: 1 read, 1 search, 2 dropped, incomplete");
  });
  it("strips control and bidi characters from a path, including newlines", () => {
    const line = activityStatusLine({ event: "check_activity", op: "read", outcome: "ok", path: "a\u001b[31m\nb\u202Ec\td.go" });
    expect(line).toBe("read a[31mbcd.go · ok");
  });
  it("ignores a prototype-named category and returns undefined for out-of-set values", () => {
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "denied", category: "constructor" })).toBe("read denied");
    expect(activityStatusLine({ event: "check_activity", op: "constructor", outcome: "ok" })).toBeUndefined();
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "toString" })).toBeUndefined();
    expect(activityStatusLine({ event: "check_activity_summary", generation: 1 })).toBeUndefined();
    expect(activityStatusLine({ event: "init" })).toBeUndefined();
    expect(activityStatusLine(null)).toBeUndefined();
  });
});

describe("checkActivityState", () => {
  const run = (over: Record<string, unknown> = {}) => ({ status: "completed", claim_generation: 1, first_started_at: "2026-01-01T00:00:00Z", ...over }) as Parameters<typeof checkActivityState>[0];
  it("shows the current generation's summary", () => {
    expect(checkActivityState(run(), [marker(1), summary(1)])).toBe("4 reads, 0 searches, 0 dropped, complete");
    expect(checkActivityState(run(), [marker(1), summary(1, { complete: false })])).toBe("4 reads, 0 searches, 0 dropped, incomplete");
  });
  it("marker without summary is incomplete when terminal and in progress otherwise", () => {
    expect(checkActivityState(run(), [marker(1)])).toBe("activity capture incomplete");
    expect(checkActivityState(run({ status: "running" }), [marker(1)])).toBe("activity capture in progress");
  });
  it("nothing for the generation: unavailable once terminal and claimed, nothing otherwise", () => {
    expect(checkActivityState(run(), [])).toBe("activity capture unavailable");
    expect(checkActivityState(run({ status: "running" }), [])).toBeUndefined();
    expect(checkActivityState(run({ first_started_at: null }), [])).toBeUndefined();
  });
  it("never presents an older attempt's summary as current", () => {
    const msgs = [marker(1), summary(1), { kind: "status", payload: { text: "resumed" } }];
    expect(checkActivityState(run({ claim_generation: 2 }), msgs))
      .toBe("activity capture unavailable; earlier attempt (generation 1): 4 reads, 0 searches, 0 dropped, complete");
    expect(checkActivityState(run({ claim_generation: 2, status: "running" }), msgs))
      .toBe("earlier attempt (generation 1): 4 reads, 0 searches, 0 dropped, complete");
  });
  it("ignores a higher-generation summary or marker (a re-claim between reads)", () => {
    expect(checkActivityState(run(), [marker(2), summary(2)])).toBe("activity capture unavailable");
    expect(checkActivityState(run({ status: "running" }), [marker(2)])).toBeUndefined();
  });
  it("treats a null generation or null numbers as absent", () => {
    const nullGen = { kind: "status", payload: { ...(summary(0).payload as object), generation: null } };
    expect(checkActivityState(run({ claim_generation: 0 }), [nullGen])).toBe("activity capture unavailable");
    expect(activityStatusLine({ event: "check_activity", op: "search", outcome: "ok", path: "w", matches: null })).toBe("search w · ok");
    expect(activityStatusLine({ event: "check_activity", op: "read", outcome: "ok", path: "a", lines: [null, null] })).toBe("read a · ok");
  });
  it("labels the newest summary historical when claim_generation is absent", () => {
    const expected = "historical; current capture unknown: 4 reads, 0 searches, 0 dropped, complete";
    expect(checkActivityState(run({ claim_generation: undefined }), [summary(1)])).toBe(expected);
    expect(checkActivityState(run({ claim_generation: null }), [summary(1)])).toBe(expected);
    expect(checkActivityState(run({ claim_generation: undefined }), [marker(1)])).toBeUndefined();
  });
});
