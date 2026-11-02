import assert from "node:assert/strict";
import test from "node:test";
import { JudgeEvidenceReducer } from "../src/judge-evidence.js";
import type { JudgeEvidence, JudgeFetchMeta } from "../src/judge-evidence.js";
import type { WorkerRunMessage } from "../src/protocol.js";

function fixture() {
  let seq = 0;
  const reducer = new JudgeEvidenceReducer();
  const events: WorkerRunMessage[] = [];
  function send(kind: string, payload: Record<string, unknown> | null, extra: Partial<WorkerRunMessage> = {}) {
    const event: WorkerRunMessage = { seq: ++seq, kind, agent: "lead", payload,
      created_at: "2026-11-02T12:00:00Z", ...extra };
    events.push(event); reducer.accept(event); return event;
  }
  const start = (id: string, extra: Partial<WorkerRunMessage> = {}) =>
    send("tool_use", { id, name: "Agent", input: { subagent_type: "reviewer", prompt: "review <scrubbed>" } }, extra);
  const end = (id: string, content: unknown = "finished", is_error: unknown = false, extra: Partial<WorkerRunMessage> = {}) =>
    send("tool_result", { tool_use_id: id, content, is_error }, extra);
  return { reducer, events, send, start, end };
}
function bounded(e: JudgeEvidence) {
  assert.ok(e.sections.coverage.length <= 2000);
  assert.ok(e.sections.ledger.length <= 18000);
  assert.ok(e.sections.aggregates.length <= 8000);
  assert.ok(e.sections.prose.length <= 58000);
  assert.ok(e.metrics.pending <= 128);
  assert.ok(e.metrics.history <= 256);
  assert.ok(e.metrics.rows <= 48);
  assert.ok(e.metrics.toolIdentities <= 64);
  assert.ok(e.metrics.pendingRows <= 128);
  assert.ok(e.metrics.failedPairs <= 6);
  assert.ok(e.metrics.reservoirEvents <= 72);
  assert.ok(e.metrics.stateStringChars <= 600000);
  assert.equal(e.counts.eligible, e.counts.retained + e.counts.omitted);
  for (const line of e.sections.prose.split("\n")) assert.ok(line.length <= 800);
}

test("repeated roles, interleaving and child replay keep parent and child tool identities independent", () => {
  const f = fixture();
  f.start("A"); f.start("B");
  f.send("tool_use", { id: "A", name: "Bash", input: { command: "task lint" } },
    { agent: "lead", agent_instance: "A" });
  f.end("A", "child output", false, { agent: "lead", agent_instance: "A" });
  assert.equal(f.reducer.render().counts.confirmed.success, 0);
  f.end("B", "review failed", true);
  f.end("A");
  const e = f.reducer.render();
  assert.deepEqual(e.counts.confirmed, { success: 1, failed: 1, interrupted: 0, unknown: 0 });
  assert.equal(e.counts.toolUses, 3);
  assert.equal(e.counts.toolResults, 3);
  assert.equal(e.counts.unmatchedResults, 1);
  assert.match(e.sections.aggregates, /"child":1/);
  assert.match(e.sections.ledger, /child_tool_uses=0/);
  assert.match(e.sections.ledger, /child_tool_uses=1/);
  bounded(e);
});

test("results before starts pair by exact lane; resume and seq replay do not fabricate outcomes", () => {
  const f = fixture();
  f.end("A");
  f.start("A");
  f.send("status", { event: "resume_continued", text: "resumed" });
  f.reducer.accept(f.events[0]!);
  const e = f.reducer.render();
  assert.equal(e.counts.confirmed.success, 1);
  assert.equal(e.counts.dispatches, 1);
  assert.match(e.sections.coverage, /ignoredSeqs=1/);
  assert.match(e.sections.ledger, /start=#2 end=#1/);
});

test("completed identity reuse retracts confirmed outcomes and taints retained rows", () => {
  const f = fixture();
  f.start("A"); f.end("A");
  assert.equal(f.reducer.render().counts.confirmed.success, 1);
  f.start("A"); f.end("A", "failed", true);
  const e = f.reducer.render();
  assert.equal(e.counts.collisions, 1);
  assert.deepEqual(e.counts.confirmed, { success: 0, failed: 0, interrupted: 0, unknown: 0 });
  assert.equal(e.counts.provisional, 2);
  assert.equal(e.sections.ledger.split("ambiguous").length - 1, 2);
});

test("pending eviction remembers X through 129 starts, reused X and late X results", () => {
  const f = fixture();
  f.start("X");
  for (let i = 0; i < 129; i++) f.start("other" + i);
  f.start("X"); f.end("X"); f.end("X");
  const e = f.reducer.render();
  assert.equal(e.counts.collisions, 1);
  assert.equal(e.counts.confirmed.success, 0);
  assert.equal(e.counts.provisional, 1);
  assert.equal(e.counts.extraResults, 1);
  assert.ok(e.counts.evictions >= 2);
  assert.equal(e.counts.incomplete, 130);
  bounded(e);
});

test("history exhaustion never confirms unremembered identities, even after pending entries close", () => {
  const f = fixture();
  for (let i = 0; i < 260; i++) { f.start("i" + i); f.end("i" + i); }
  f.start("i259"); f.end("i259", "failed", true);
  const e = f.reducer.render();
  assert.equal(e.metrics.history, 256);
  assert.equal(e.metrics.historySaturated, true);
  assert.equal(e.counts.confirmed.success, 256);
  assert.equal(e.counts.confirmed.failed, 0);
  assert.equal(e.counts.provisional, 5);
  bounded(e);
});

test("missing, malformed and unrecognized results stay unknown; extra results retract", () => {
  const f = fixture();
  f.start("missing");
  f.start("bad"); f.send("tool_result", { tool_use_id: "bad", is_error: false });
  f.start("shape"); f.end("shape", { arbitrary: "text" });
  f.start("flag"); f.end("flag", "text", "false");
  f.start("good"); f.end("good", [{ type: "text", text: "done" }]);
  f.end("good");
  f.end("orphan");
  const e = f.reducer.render();
  assert.equal(e.counts.incomplete, 1);
  assert.equal(e.counts.confirmed.success, 0);
  assert.equal(e.counts.provisional, 4);
  assert.equal(e.counts.extraResults, 1);
  assert.equal(e.counts.unmatchedResults, 2);
  assert.match(e.sections.ledger, /outcome=unknown/);
});

test("only exact recorded interruption strings qualify; no historical timeout or budget invented", () => {
  const f = fixture();
  const interruptions = [
    "delegation stopped by the run; child settlement not confirmed",
    "delegation still open at turn end; child settlement not confirmed",
    "delegation ended by a provider stream failure; child settlement not confirmed",
  ];
  interruptions.forEach((s, i) => { f.start("i" + i); f.end("i" + i, s, true); });
  f.start("generic"); f.end("generic", "timeout mentioned in a report", true);
  const e = f.reducer.render();
  assert.equal(e.counts.confirmed.interrupted, 3);
  assert.equal(e.counts.confirmed.failed, 1);
  assert.match(e.sections.ledger, /deadline=unknown cause=unknown/);
  assert.doesNotMatch(e.sections.ledger, /timeout|budget/);
});

test("persisted-event intervals require valid ordered timestamps", () => {
  const f = fixture();
  f.start("valid");
  f.end("valid", "done", false, { created_at: "2026-11-02T12:00:02Z" });
  f.start("bad", { created_at: "not a date" }); f.end("bad");
  f.start("missing", { created_at: "" }); f.end("missing");
  f.start("reverse"); f.end("reverse", "done", false, { created_at: "2026-11-02T11:59:59Z" });
  const s = f.reducer.render().sections.ledger;
  assert.match(s, /2000ms persisted-event interval/);
  assert.equal(s.split("unknown persisted-event interval").length - 1, 3);
  assert.doesNotMatch(s, /execution duration/);
});

test("zero-tool child text is retained and dispatch boundary resets only lead observations", () => {
  const f = fixture();
  f.start("A");
  f.send("text", { text: "review report without tools" }, { agent_instance: "A", agent: "reviewer" });
  f.end("A");
  f.send("tool_use", { id: "sh", name: "Bash", input: { command: "echo checked" } });
  assert.match(f.reducer.render().sections.aggregates, /after LAST observed dispatch=1/);
  f.start("B");
  f.send("tool_use", { id: "sh2", name: "Bash", input: {} }, { agent_instance: "B" });
  const e = f.reducer.render();
  assert.match(e.sections.aggregates, /after LAST observed dispatch=0 boundary=#5/);
  assert.match(e.sections.prose, /review report without tools/);
  assert.match(e.sections.ledger, /child_tool_uses=0/);
  assert.doesNotMatch(e.sections.aggregates, /approval|percentage|effort/);
});

test("tool identity overflow and oversized exact identities remain explicit", () => {
  const f = fixture();
  for (let i = 0; i < 70; i++) f.send("tool_use", { id: "t" + i, name: "tool" + i, input: {} });
  f.start("x".repeat(257)); f.end("x".repeat(257));
  f.send("tool_use", { id: "x", name: "Task", input: {} }, { agent_instance: "z".repeat(257) });
  const e = f.reducer.render();
  assert.equal(e.metrics.toolIdentities, 64);
  assert.equal(e.counts.oversizedKeys, 3);
  assert.equal(e.counts.toolUses, 72);
  assert.match(e.sections.aggregates, /overflow {"lead":7,"child":1,"unknown":0}/);
  bounded(e);
});

test("prose classes survive text floods, accounting excluded but result failures retained", () => {
  const f = fixture();
  f.send("status", { event: "draft_plan_capture", version: 1, plan_md: "middle draft" });
  f.send("plan", { plan_md: "latest plan" });
  f.send("plan_feedback", { feedback: "owner steering" });
  f.send("plan_revising", { round: 1 });
  f.send("user_message", { text: "middle request" });
  f.start("bad"); f.end("bad", "failed", true);
  f.send("text", { text: "available review report" }, { agent_instance: "bad", agent: "reviewer" });
  f.send("status", { event: "semantic_plan_coverage", missing: ["M1"] });
  f.send("error", { event: "result", subtype: "error_during_execution", errors: ["provider failed"] });
  f.send("progress_note", { text: "excluded note" });
  f.send("summary_usage", { pass: "intent", model_usage: {} });
  f.send("status", { event: "codex_response_usage", usage: {} });
  f.send("status", { event: "result", subtype: "success", usage: {} });
  for (let i = 0; i < 500; i++) f.send("text", { text: "flood" + i });
  const e = f.reducer.render();
  assert.equal(e.counts.excluded, 4);
  for (const s of ["middle draft", "latest plan", "owner steering", "middle request", "available review report",
    "semantic_plan_coverage", "provider failed", "flood499"]) assert.ok(e.sections.prose.includes(s), s);
  assert.doesNotMatch(e.sections.prose, /excluded note|codex_response_usage/);
  bounded(e);
});

test("payload/control floods are bounded, escaped and detached from raw payloads", () => {
  const f = fixture();
  const controls = "\n\t" + String.fromCharCode(27) + "[31m" + "\u202e";
  const cyclic: Record<string, unknown> = { text: controls + "x".repeat(1000000), secret: "<scrubbed>" };
  cyclic["self"] = cyclic;
  for (let i = 0; i < 150; i++) f.send("text", cyclic, { agent: controls, agent_instance: controls });
  cyclic["text"] = "mutated";
  const e = f.reducer.render();
  assert.doesNotMatch(e.sections.prose, /mutated/);
  assert.ok(e.sections.prose.includes("\\n\\t\\u001b"));
  assert.ok(e.sections.prose.includes("\\u202e"));
  for (const c of e.sections.prose) {
    const n = c.charCodeAt(0);
    assert.ok(n === 10 || n >= 32);
    assert.ok(n < 0x202a || n > 0x202e);
  }
  bounded(e);
});

test("failed pairs include starts despite unrelated error and text floods", () => {
  const f = fixture();
  for (let i = 0; i < 30; i++) f.send("error", { message: "early error" + i });
  f.start("paired-failure");
  for (let i = 0; i < 30; i++) f.send("text", { text: "between" + i });
  f.end("paired-failure", "the failed report", true);
  for (let i = 0; i < 30; i++) f.send("error", { message: "later error" + i });
  const e = f.reducer.render();
  assert.match(e.sections.prose, /"id":"paired-failure"/);
  assert.match(e.sections.prose, /the failed report/);
  bounded(e);
});

test("extra results preceding a start taint the later match", () => {
  const f = fixture();
  f.end("X"); f.end("X", "different result", true); f.start("X");
  const e = f.reducer.render();
  assert.equal(e.counts.confirmed.success, 0);
  assert.equal(e.counts.provisional, 1);
  assert.equal(e.counts.extraResults, 1);
  assert.match(e.sections.ledger, /ambiguous/);
});

test("status result facts beyond the accounting capsule are retained", () => {
  const f = fixture();
  f.send("status", { event: "result", subtype: "success", errors: ["failure fact"] });
  f.send("error", { event: "result", subtype: "success", errors: ["failed terminal"] });
  assert.equal(f.reducer.render().counts.excluded, 0);
  assert.match(f.reducer.render().sections.prose, /failure fact/);
});

test("exact delimiter-containing lanes never collide with parent delegation keys", () => {
  const f = fixture();
  f.start("a:b");
  f.end("b", "child result", false, { agent_instance: "a:" });
  assert.equal(f.reducer.render().counts.confirmed.success, 0);
  f.end("a:b");
  assert.equal(f.reducer.render().counts.confirmed.success, 1);
});

test("unknown attribution cannot settle a parent lead dispatch", () => {
  const f = fixture();
  f.start("X");
  f.end("X", "output", false, { agent: null });
  assert.equal(f.reducer.render().counts.confirmed.success, 0);
  f.end("X");
  assert.equal(f.reducer.render().counts.confirmed.success, 1);
});

test("invalid calendar timestamps stay unknown", () => {
  const f = fixture();
  f.start("X", { created_at: "2026-02-31T12:00:00Z" });
  f.end("X", "done", false, { created_at: "2026-03-04T12:00:00Z" });
  assert.match(f.reducer.render().sections.ledger, /unknown persisted-event interval/);
});

test("long malformed keys and payloads cannot exceed retained state budgets", () => {
  const f = fixture();
  const huge = "x".repeat(1000000);
  for (let i = 0; i < 400; i++) {
    f.start("i" + i, { agent_label: huge });
    f.send("tool_use", { id: "t", name: huge, input: { nested: { deep: { ignored: huge } } } });
  }
  bounded(f.reducer.render());
});

test("oversized identity roles and additional accounting failure facts stay bounded and visible", () => {
  const f = fixture();
  f.send("tool_use", { id: "x".repeat(257), name: "Agent", input: { subagent_type: "r".repeat(1000000) } });
  f.send("status", { event: "codex_response_usage", usage: {}, errors: ["distinct failure fact"] });
  f.send("text", { text: "\u200f\u2028\u061c" });
  const e = f.reducer.render();
  assert.equal(e.counts.excluded, 0);
  assert.match(e.sections.prose, /distinct failure fact/);
  for (const char of ["\u200f", "\u2028", "\u061c"]) assert.ok(!e.sections.prose.includes(char));
  bounded(e);
});

test("evicted result identities prevent later invented definite matches", () => {
  const f = fixture();
  f.end("X");
  for (let i = 0; i < 128; i++) f.end("orphan" + i);
  f.end("X", "a second result"); f.start("X");
  const e = f.reducer.render();
  assert.equal(e.counts.confirmed.success, 0);
  assert.equal(e.counts.provisional, 1);
  assert.match(e.sections.coverage, /orphanResultEvictions=2/);
  assert.match(e.sections.ledger, /outcome=unknown ambiguous/);
  bounded(e);
});

test("successful dispatch reports preserve middle review evidence without severity parsing", () => {
  const f = fixture();
  for (let i = 0; i < 100; i++) f.send("text", { text: "before" });
  f.start("review"); f.end("review", "REVIEW REPORT: unresolved validation finding", false);
  for (let i = 0; i < 100; i++) f.send("text", { text: "after" });
  assert.match(f.reducer.render().sections.prose, /unresolved validation finding/);
});

test("timestamps, known causes and representative successes remain visible under floods", () => {
  const f = fixture();
  const time = "2026-11-02T12:00:00.123456789+00:00";
  f.start("healthy", { created_at: time }); f.end("healthy", "done", false, { created_at: time });
  for (let i = 0; i < 100; i++) {
    f.start("bad" + i, { created_at: time });
    f.end("bad" + i, "delegation ended by a provider stream failure; child settlement not confirmed", true,
      { created_at: time });
  }
  const e = f.reducer.render({ pages: 200, complete: false, reason: "hard cap", cap: 100000 });
  assert.match(e.sections.ledger, /instance="healthy"/);
  assert.match(e.sections.ledger, /cause="provider stream failure; settlement unconfirmed"/);
  assert.ok(e.sections.ledger.includes(time));
  assert.ok(e.sections.prose.includes(time));
  assert.match(e.sections.coverage, /completion-known=no complete=false/);
  for (const line of e.sections.ledger.split("\n")) {
    assert.match(line, /deadline=unknown cause=/);
    assert.match(line, /persisted-event interval/);
  }
  bounded(e);
});

test("ledger priority, row and prose budgets, deterministic page-independent selection", () => {
  const f = fixture();
  for (let i = 0; i < 80; i++) { f.start("s" + i); f.end("s" + i); }
  f.start("failed"); f.end("failed", "failure", true);
  f.start("open");
  const meta: JudgeFetchMeta = { pages: 9, complete: false, reason: "fetch cap", cap: 1000 };
  const e = f.reducer.render(meta);
  assert.equal(e.metrics.rows, 48);
  assert.equal(e.counts.omittedRows, 34);
  assert.match(e.sections.ledger.split("\n")[0]!, /instance="failed"/);
  assert.match(e.sections.ledger, /instance="s0"/);
  assert.match(e.sections.coverage, /complete=false/);
  assert.match(e.sections.coverage, /final delivery unknown/);
  const second = new JudgeEvidenceReducer();
  for (let i = 0; i < f.events.length; i += 7) for (const event of f.events.slice(i, i + 7)) second.accept(event);
  assert.deepEqual(second.render(meta), e);
  assert.match(second.render().sections.coverage, /pages=unknown completion-known=no/);
  assert.match(second.render({ pages: 1, complete: true, reason: "exhausted" }).sections.coverage, /fetch complete/);
  bounded(e);
});
