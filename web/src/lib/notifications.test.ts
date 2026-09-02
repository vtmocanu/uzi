import { describe, it, expect } from "vitest";
import {
  EARLY_LIMIT_RESET_KIND,
  groupNotifications,
  JUDGE_GROUP_WINDOW_MS,
  notificationBody,
  notificationLink,
  notificationTitle,
  type GroupableNotification,
} from "./notifications";

// PRD #98 M5 — the two pure pieces of the notification retarget + inbox grouping.

describe("notificationLink (Decision 4: kind-conditional, NOT a URL edit)", () => {
  it("sends a judge review to the Judge workbench, anchored to its run", () => {
    expect(notificationLink("judge_review", "run-7")).toBe("/judge?run=run-7");
  });

  // PRD #333 M7: a findings ping opens the per-repo Findings backlog filtered to the flagging
  // run — the same kind-conditional guard, a different destination from both the judge and the
  // generic run rows. The gate above (every OTHER kind → /runs) proves this did not leak.
  it("sends an incidental_finding ping to the Findings backlog, filtered to its run", () => {
    expect(notificationLink("incidental_finding", "run-7")).toBe("/findings?run=run-7");
    expect(notificationLink("incidental_finding", null)).toBeNull();
  });

  // THE GATE. Read the failure mode before touching this: the inbox linked
  // /runs/${run_id} for every kind, so "retarget the judge notification" reads as a
  // one-line URL change — and that change redirects EVERY other kind to the Judge page.
  // A test that only checks the judge row passes under exactly that bug, which is why the
  // load-bearing case here is the non-judge one, and why it enumerates several kinds
  // rather than one: a guard written as `kind !== 'run_failed'` would satisfy a single case.
  it("leaves every OTHER kind on the run page", () => {
    // selfimprove_started is the ONLY non-judge kind the server currently emits that carries a
    // run id (api/internal/schedsvc/self_improve.go, the emitter of selfimprove_started) — i.e.
    // the only one that reaches this branch in production today. The rest are deliberately fictional: this is a pure string comparison,
    // so unknown kinds exercise the same path, and several are here so a guard spelt
    // `kind !== 'run_failed'` cannot satisfy the case.
    for (const kind of ["selfimprove_started", "run_failed", "mr_merged", "some_future_kind", ""]) {
      expect(notificationLink(kind, "run-7")).toBe("/runs/run-7");
    }
  });

  it("offers no link at all when there is no run to open", () => {
    // Distinct from linking to the wrong place: null renders no anchor.
    expect(notificationLink("judge_review", null)).toBeNull();
    expect(notificationLink("run_failed", undefined)).toBeNull();
    expect(notificationLink("judge_review", "")).toBeNull();
  });
});

// PRD #1020 M5: the early-limit-reset inbox renderer. The kind is account-level (no
// run), so its only renderer seam is a fixed title — an unrendered kind would show the
// raw humanized string "early limit reset", and it must carry no deep link.
describe("early_limit_reset inbox renderer (PRD #1020 M5)", () => {
  it("gives the kind a fixed human title, not the raw humanized kind", () => {
    const title = notificationTitle(EARLY_LIMIT_RESET_KIND, {});
    expect(title).toBe("7-day rate limit reset early");
    // The failure this guards: falling through to kind.replace(/_/g, " ").
    expect(title).not.toBe("early limit reset");
  });

  it("prefers an explicit payload.title when present", () => {
    expect(
      notificationTitle(EARLY_LIMIT_RESET_KIND, { title: "Weekly limit reopened early" }),
    ).toBe("Weekly limit reopened early");
  });

  it("offers no link — the alert is account-level and carries no run", () => {
    expect(notificationLink(EARLY_LIMIT_RESET_KIND, null)).toBeNull();
    expect(notificationLink(EARLY_LIMIT_RESET_KIND, undefined)).toBeNull();
  });
});

describe("notificationBody (PRD #292 Decision 7: inbox row stays a collapsed one-liner)", () => {
  // The inbox reads ONLY payload.body — never payload.summary. Since M4 the judge's
  // payload.summary is MULTI-LINE (it feeds the Slack blockquote), while payload.body is
  // the collapsed one-liner reviewNotificationBody built. The inbox must show that
  // one-liner verbatim and must NOT fall back to the multi-line summary (SC4).
  it("returns the one-line payload.body verbatim, never the multi-line summary", () => {
    const payload = {
      body: "verdict: ok — 1 recommendation: a b c",
      summary: "line1\nline2\nline3",
    };
    const body = notificationBody(payload);
    expect(body).toBe("verdict: ok — 1 recommendation: a b c");
    expect(body).not.toContain("\n");
  });

  it("returns empty string when body is absent (never reaches for summary)", () => {
    expect(notificationBody({ summary: "line1\nline2" })).toBe("");
  });
});

describe("groupNotifications (Decision 5: render-only judge grouping)", () => {
  const at = (ms: number) => new Date(ms).toISOString();
  const n = (id: string, kind: string, created: string): GroupableNotification => ({
    id,
    kind,
    created_at: created,
    read_at: null,
  });
  const base = Date.parse("2026-07-20T12:00:00Z");

  it("collapses a run of adjacent judge rows into one group", () => {
    const out = groupNotifications([
      n("a", "judge_review", at(base)),
      n("b", "judge_review", at(base - 1000)),
      n("c", "judge_review", at(base - 2000)),
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("judge_group");
    expect(out[0].kind === "judge_group" && out[0].items.map((i) => i.id)).toEqual(["a", "b", "c"]);
  });

  it("a LONE judge row stays a plain row", () => {
    // A "1 review ready" header costs a click to reach what was already on screen.
    const out = groupNotifications([n("a", "judge_review", at(base))]);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("single");
  });

  it("a non-judge row BREAKS the run — grouping is strictly by kind", () => {
    // Decision 5 scopes grouping to judge_review precisely so a different judge-adjacent
    // notification (#69 M7a's infra-skip is the named example) never hides under a header
    // that claims to be about reviews.
    const out = groupNotifications([
      n("a", "judge_review", at(base)),
      n("b", "judge_review", at(base - 1000)),
      n("infra", "infra_skip", at(base - 2000)),
      n("c", "judge_review", at(base - 3000)),
      n("d", "judge_review", at(base - 4000)),
    ]);
    expect(out.map((g) => g.kind)).toEqual(["judge_group", "single", "judge_group"]);
  });

  it("a gap wider than the window breaks the run", () => {
    // The gaps are measured between ADJACENT rows, so `old` is placed a full window plus a
    // margin below `b` — not below `base`. An earlier draft of this test put it exactly one
    // window below `b`, which is the `>` boundary and therefore still groups; the test
    // failed and the implementation was right. Keeping the margin explicit so the next
    // reader does not re-derive the same off-by-a-boundary.
    const out = groupNotifications([
      n("a", "judge_review", at(base)),
      n("b", "judge_review", at(base - 1000)),
      n("old", "judge_review", at(base - 1000 - JUDGE_GROUP_WINDOW_MS - 5000)),
      n("older", "judge_review", at(base - 1000 - JUDGE_GROUP_WINDOW_MS - 6000)),
    ]);
    expect(out.map((g) => g.kind)).toEqual(["judge_group", "judge_group"]);
    expect(out[0].kind === "judge_group" && out[0].items.map((i) => i.id)).toEqual(["a", "b"]);
    expect(out[1].kind === "judge_group" && out[1].items.map((i) => i.id)).toEqual(["old", "older"]);
  });

  it("an unparseable timestamp breaks the run rather than folding in silently", () => {
    // NaN > window is false, so the arithmetic alone would GROUP the bad row. That is the
    // fail-open direction, hence the explicit !Number.isFinite check.
    //
    // The bad row breaks the run on BOTH sides — it is also the predecessor the next row is
    // measured against — so three rows come back as three singles, not two groups. That is
    // the intended cost and it is stated here rather than left to be discovered: one
    // unreadable timestamp ungroups its neighbours, which is visibly odd and therefore
    // reportable, where silently folding an unknown-age row under "3 reviews ready" is not.
    const out = groupNotifications([
      n("a", "judge_review", at(base)),
      n("bad", "judge_review", "not a date"),
      n("c", "judge_review", at(base - 1000)),
    ]);
    expect(out.map((g) => g.kind)).toEqual(["single", "single", "single"]);
  });

  // The invariant that makes "render-only" true rather than aspirational. Grouping must
  // never drop, duplicate or reorder a row — read-state, ids and the API's offset paging
  // all depend on the list being exactly what the server sent.
  it("is a pure partition: every row appears exactly once, in order", () => {
    const input = [
      n("a", "judge_review", at(base)),
      n("b", "run_failed", at(base - 1000)),
      n("c", "judge_review", at(base - 2000)),
      n("d", "judge_review", at(base - 3000)),
      n("e", "judge_review", at(base - JUDGE_GROUP_WINDOW_MS - 9000)),
      n("f", "mr_merged", at(base - JUDGE_GROUP_WINDOW_MS - 10000)),
    ];
    const flat = groupNotifications(input).flatMap((g) => (g.kind === "judge_group" ? g.items : [g.item]));
    expect(flat.map((i) => i.id)).toEqual(input.map((i) => i.id));
  });

  it("handles an empty inbox", () => {
    expect(groupNotifications([])).toEqual([]);
  });
});
