import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  CheckActivityRecorder, formatActivityLine, formatActivitySummary, ACTIVITY_PAYLOAD_MAX_BYTES,
} from "../src/cross-check-activity.js";

const CHECKOUT = "/work/checkout";
const HOME = "/work/home";

function rig(opts: { cap?: number } = {}) {
  const out: Record<string, unknown>[] = [];
  const rec = new CheckActivityRecorder((p) => { out.push(p); },
    { checkout: CHECKOUT, toolOutputDir: HOME, generation: 7, ...opts });
  const activity = () => out.filter((p) => p["event"] === "check_activity");
  const summary = () => out.find((p) => p["event"] === "check_activity_summary")!;
  return { out, rec, activity, summary };
}

it("coverage evidence includes only exact recorded successful reads and marks dropped capture incomplete", () => {
  const { rec } = rig({ cap: 2 });
  rec.start(); rec.modelStarted();
  rec.begin("ok", "read", CHECKOUT + "/ok.ts"); rec.settle("ok", { outcome: "ok" });
  rec.begin("search", "search", CHECKOUT + "/scope"); rec.settle("search", { outcome: "ok", matches: 1 });
  rec.begin("dropped", "read", CHECKOUT + "/dropped.ts"); rec.settle("dropped", { outcome: "ok" });
  rec.summary("codex", "code", true);
  assert.deepEqual([...rec.coverageEvidence().reads], ["ok.ts"]);
  assert.equal(rec.coverageEvidence().complete, false);
  const fresh = rig();
  fresh.rec.start(); fresh.rec.modelStarted();
  fresh.rec.begin("denied", "read", CHECKOUT + "/denied.ts"); fresh.rec.settle("denied", { outcome: "denied" });
  fresh.rec.begin("unsafe", "read", CHECKOUT + "/bad\u202e.ts"); fresh.rec.settle("unsafe", { outcome: "ok" });
  fresh.rec.begin("ok", "read", CHECKOUT + "/exact.ts"); fresh.rec.settle("ok", { outcome: "ok" });
  fresh.rec.summary("claude", "code", true);
  assert.deepEqual([...fresh.rec.coverageEvidence().reads], ["exact.ts"]);
  assert.equal(fresh.rec.coverageEvidence().complete, true);
});

describe("CheckActivityRecorder", () => {
  it("emits the marker and a complete summary for a settled session", () => {
    const { out, rec, summary } = rig();
    rec.start();
    rec.modelStarted();
    for (const [i, p] of ["a.ts", "b.ts", "c.ts", "d.ts"].entries()) {
      rec.begin("r" + i, "read", CHECKOUT + "/" + p);
      rec.settle("r" + i, { outcome: i === 3 ? "oversize" : "ok", lines: [1, 10] });
    }
    rec.summary("codex", "plan", true);
    assert.deepEqual(out[0], { event: "check_activity_start", v: 1, cap: 200, generation: 7 });
    assert.deepEqual(summary(), {
      event: "check_activity_summary", generation: 7, family: "codex", stage: "plan",
      reads: { ok: 3, denied: 0, oversize: 1, limit: 0, error: 0 },
      searches: { ok: 0, denied: 0, oversize: 0, limit: 0, error: 0 },
      dropped: 0, untracked: 0, unsettled: 0, complete: true,
    });
    assert.equal(formatActivitySummary(summary()), "4 reads, 0 searches, 0 dropped, complete");
  });

  it("is incomplete without a start marker or an ended session", () => {
    const a = rig();
    a.rec.summary("claude", "code", true);
    assert.equal(a.summary()["complete"], false);
    const b = rig();
    b.rec.start();
    b.rec.summary("claude", "code", false);
    assert.equal(b.summary()["complete"], false);
  });

  it("is incomplete until the model has started", () => {
    const a = rig();
    a.rec.start();
    a.rec.summary("claude", "code", true);
    assert.equal(a.summary()["complete"], false);
    const b = rig();
    b.rec.start();
    b.rec.modelStarted();
    b.rec.summary("claude", "code", true);
    assert.equal(b.summary()["complete"], true);
  });

  it("emits on settle only, relative paths with ranges, never absolute", () => {
    const { rec, out, activity } = rig();
    rec.start();
    rec.begin("a", "read", "api/x.go");
    assert.equal(activity().length, 0);
    rec.settle("a", { outcome: "ok", lines: [1, 200] });
    rec.begin("b", "read", CHECKOUT + "/web/../api/y.go");
    rec.settle("b", { outcome: "ok" });
    assert.deepEqual(activity().map((p) => p["path"]), ["api/x.go", "api/y.go"]);
    assert.deepEqual(activity()[0]!["lines"], [1, 200]);
    assert.equal(JSON.stringify(out).includes(CHECKOUT), false);
    assert.equal(formatActivityLine(activity()[0]!), "read api/x.go L1-200 · ok");
  });

  it("records a search by scope and match count, with no query anywhere", () => {
    const { rec, out, activity } = rig();
    rec.start();
    rec.begin("s", "search", "api/internal/forge");
    rec.settle("s", { outcome: "ok", matches: 3 });
    rec.begin("t", "search");
    rec.settle("t", { outcome: "ok", matches: 100, matchesTruncated: true });
    assert.deepEqual(activity()[0], { event: "check_activity", op: "search", outcome: "ok", path: "api/internal/forge", matches: 3 });
    assert.equal(activity()[1]!["path"], ".");
    assert.equal(activity()[1]!["matches_truncated"], true);
    assert.equal(formatActivityLine(activity()[0]!), "search api/internal/forge · 3 matches · ok");
    assert.equal(formatActivityLine(activity()[1]!), "search . · 100+ matches · ok");
    assert.equal(/query|pattern|glob/.test(JSON.stringify(out)), false);
    // The public API takes no query: begin/settle accept only an id, an op and a scope path.
    assert.equal(CheckActivityRecorder.prototype.begin.length, 3);
  });

  it("never emits a path outside the checkout", () => {
    const { rec, out, activity } = rig();
    rec.start();
    rec.begin("o", "read", "/etc/passwd");
    rec.settle("o", { outcome: "denied" });
    rec.begin("u", "read", "../../secret");
    rec.settle("u", { outcome: "denied" });
    rec.begin("t", "read", HOME + "/tool-results/abc.txt");
    rec.settle("t", { outcome: "ok", lines: [1, 5] });
    rec.begin("p", "read", CHECKOUT + "-evil/x");
    rec.settle("p", { outcome: "denied", category: "policy" });
    const a = activity();
    assert.deepEqual(a.map((p) => [p["path"], p["category"]]), [
      [undefined, "outside_checkout"], [undefined, "outside_checkout"],
      [undefined, "tool_output"], [undefined, "outside_checkout"],
    ]);
    assert.equal(JSON.stringify(out).includes("passwd"), false);
    assert.equal(formatActivityLine(a[0]!), "read denied (outside checkout)");
    assert.equal(formatActivityLine(a[2]!), "read (tool output) L1-5 · ok");
  });

  it("uses an explicit policy category, without the path, when a guard denies an inside path", () => {
    const { rec, activity } = rig();
    rec.begin("p", "read", ".git/config");
    rec.settle("p", { outcome: "denied", category: "policy" });
    assert.equal("path" in activity()[0]!, false, "a guard-denied attempt carries a category, never the path");
    assert.equal(activity()[0]!["category"], "policy");
  });

  it("never emits a path carrying a character the sanitizer would strip", () => {
    const { rec, activity } = rig();
    const inputs = ["a\u001b[31m\nb\u202ec\u200bd\u00ade.ts", "\u200b/etc/passwd",
      "\u0000/home/x/.codex/auth.json", "..\u200b/..\u200b/etc/shadow"];
    inputs.forEach((p, i) => { rec.begin("c" + i, "read", p); rec.settle("c" + i, { outcome: "denied" }); });
    assert.deepEqual(activity().map((p) => [p["path"], p["category"]]), inputs.map(() => [undefined, "policy"]));
  });

  it("refuses a path rather than truncating it, and keeps every payload within 1 KiB", () => {
    const { rec, out, activity } = rig();
    rec.start();
    rec.begin("l", "read", "d/".repeat(600) + "z.ts");
    rec.settle("l", { outcome: "ok", lines: [1, 2] });
    assert.deepEqual([activity()[0]!["path"], activity()[0]!["category"]], [undefined, "policy"]);
    rec.begin("q", "read", '"\\'.repeat(256));
    rec.settle("q", { outcome: "ok", lines: [1, 2] });
    for (const e of out) assert.ok(Buffer.byteLength(JSON.stringify(e), "utf8") <= ACTIVITY_PAYLOAD_MAX_BYTES);
    assert.equal(activity().length, 2);
  });

  it("counts and emits an outcome outside the closed set as error", () => {
    const { rec, activity, summary } = rig();
    rec.begin("o", "read", "a.ts");
    rec.settle("o", { outcome: "bogus" as never });
    rec.summary("codex", "plan", true);
    assert.equal(activity()[0]!["outcome"], "error");
    assert.equal((summary()["reads"] as Record<string, number>)["error"], 1);
  });

  it("caps emitted events at 200 and counts the excess as dropped", () => {
    const { rec, activity, summary } = rig();
    rec.start();
    rec.modelStarted();
    for (let i = 0; i < 250; i++) {
      rec.begin("id" + i, "read", "f" + i + ".ts");
      rec.settle("id" + i, { outcome: "ok" });
    }
    rec.summary("codex", "code", true);
    assert.equal(activity().length, 200);
    assert.equal(summary()["dropped"], 50);
    assert.equal((summary()["reads"] as Record<string, number>)["ok"], 250);
    assert.equal(summary()["complete"], true);
  });

  it("bounds pending calls at 256 and counts the rest untracked", () => {
    const { rec, summary } = rig();
    rec.start();
    for (let i = 0; i < 300; i++) rec.begin("id" + i, "read", "f.ts");
    rec.summary("codex", "plan", true);
    assert.equal(summary()["untracked"], 44);
    assert.equal(summary()["unsettled"], 256);
    assert.equal(summary()["complete"], false);
  });

  it("rejects non-string, empty and 129-byte ids; ignores duplicates and unknown settles", () => {
    const { rec, activity, summary } = rig();
    rec.start();
    rec.begin(42, "read", "a.ts");
    rec.begin("", "read", "a.ts");
    rec.begin("x".repeat(129), "read", "a.ts");
    rec.begin("é".repeat(65), "read", "a.ts"); // 130 bytes
    rec.begin("x".repeat(128), "read", "ok.ts");
    rec.begin("x".repeat(128), "read", "dup.ts");
    rec.settle("never-begun", { outcome: "ok" });
    rec.settle(7, { outcome: "ok" });
    rec.settle("x".repeat(128), { outcome: "ok" });
    rec.summary("codex", "plan", true);
    assert.equal(summary()["untracked"], 4);
    assert.deepEqual(activity().map((p) => p["path"]), ["ok.ts"]);
    assert.equal((summary()["reads"] as Record<string, number>)["ok"], 1);
    assert.equal(summary()["complete"], false);
  });

  it("counts calls pending at the summary as unsettled", () => {
    const { rec, summary } = rig();
    rec.start();
    rec.begin("a", "search", "api");
    rec.summary("claude", "plan", true);
    assert.equal(summary()["unsettled"], 1);
    assert.equal(summary()["complete"], false);
  });

  it("swallows an emit that throws and never throws into the check", () => {
    const rec = new CheckActivityRecorder(() => { throw new Error("boom"); }, { checkout: CHECKOUT, generation: 1 });
    rec.start();
    rec.begin("a", "read", "a.ts");
    rec.settle("a", { outcome: "ok" });
    rec.summary("codex", "plan", true);
  });

  it("emits the summary only once", () => {
    const { rec, out } = rig();
    rec.start();
    rec.summary("codex", "plan", true);
    rec.summary("codex", "plan", true);
    assert.equal(out.filter((p) => p["event"] === "check_activity_summary").length, 1);
  });
});

describe("activity wording", () => {
  it("treats the payload as untrusted when rendering", () => {
    assert.equal(formatActivityLine({ op: "write", outcome: "<b>", category: "constructor", path: "a\u202eb.ts" }), "read ab.ts · error");
    assert.equal(formatActivityLine({ op: "read", outcome: "denied", category: "toString" }), "read denied");
    assert.equal(formatActivityLine({ op: "search", outcome: "denied", category: "policy" }), "search denied (policy)");
  });
  it("uses singular nouns and the incomplete marker", () => {
    const p = { reads: { ok: 1, denied: 0, oversize: 0, limit: 0, error: 0 },
      searches: { ok: 1, denied: 0, oversize: 0, limit: 0, error: 0 }, dropped: 2, complete: false };
    assert.equal(formatActivitySummary(p), "1 read, 1 search, 2 dropped, incomplete");
  });
  it("renders a single match without a plural", () => {
    assert.equal(formatActivityLine({ op: "search", outcome: "ok", path: "api", matches: 1 }), "search api · 1 match · ok");
  });
});
