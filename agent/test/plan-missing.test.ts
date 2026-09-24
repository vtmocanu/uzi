import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  MAX_LEAD_FINAL_MESSAGE_LEN,
  PLAN_MISSING_QUESTION,
  REASON_PLAN_MISSING,
  boundLeadFinalMessage,
  buildPlanMissingGuidancePrompt,
  isProseOnlyPlanTurn,
  resolvePlanMissing,
} from "../src/plan-missing.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";

// Issue #1593: the shared pieces of the prose-only planning-turn recovery. The executor
// wiring is covered in codex-executor / ask-user-executor tests; this pins the bounding,
// the prompt's trust boundary and the prose-only predicate.

const MARKER = "…[truncated]";

describe("boundLeadFinalMessage", () => {
  it("returns short text unchanged", () => {
    assert.equal(boundLeadFinalMessage("a short message\nwith\ttabs"), "a short message\nwith\ttabs");
  });

  it("caps long text at the limit, marker included", () => {
    const out = boundLeadFinalMessage("y".repeat(MAX_LEAD_FINAL_MESSAGE_LEN * 2));
    assert.equal(out.length, MAX_LEAD_FINAL_MESSAGE_LEN);
    assert.ok(out.endsWith(MARKER));
    assert.equal(boundLeadFinalMessage("z".repeat(MAX_LEAD_FINAL_MESSAGE_LEN)).length, MAX_LEAD_FINAL_MESSAGE_LEN, "exactly at the cap is not truncated");
    assert.ok(!boundLeadFinalMessage("z".repeat(MAX_LEAD_FINAL_MESSAGE_LEN)).endsWith(MARKER));
  });

  it("redacts BEFORE the cut, so a secret straddling the cap leaves no prefix", () => {
    const secret = "sekret-" + "straddle-1593-abcdef";
    const redact = (s: string) => s.split(secret).join("[REDACTED]");
    // The secret starts just before where the cut lands, so a cut-then-redact would keep
    // its first few characters, which no longer match the full secret.
    const text = "p".repeat(MAX_LEAD_FINAL_MESSAGE_LEN - MARKER.length - 5) + secret + "q".repeat(100);
    const out = boundLeadFinalMessage(text, redact);
    assert.ok(out.length <= MAX_LEAD_FINAL_MESSAGE_LEN);
    assert.ok(!out.includes(secret.slice(0, 5)), `no secret prefix may survive: ${JSON.stringify(out.slice(-30))}`);
  });

  it("strips NUL, C0 (except \\n and \\t), DEL, C1 and bidi/format controls", () => {
    const nasty = "a\u0000b\u0007c\u001bd\u007fe\u0085f‮g⁦h​i﻿j\n\tk";
    assert.equal(boundLeadFinalMessage(nasty), "abcdefghij\n\tk");
  });

  it("never splits a surrogate pair at the cut", () => {
    const cut = MAX_LEAD_FINAL_MESSAGE_LEN - MARKER.length;
    const out = boundLeadFinalMessage("a".repeat(cut - 1) + "\u{1F600}".repeat(10));
    const last = out.charCodeAt(out.length - MARKER.length - 1);
    assert.ok(!(last >= 0xd800 && last <= 0xdbff), "no lone high surrogate before the marker");
  });
});

describe("buildPlanMissingGuidancePrompt", () => {
  it("carries the owner's guidance and never the lead's prose or the park's question", () => {
    const prose = "LEAD-PROSE: let me know what you think";
    const p = buildPlanMissingGuidancePrompt("use the server path");
    assert.match(p, /use the server path/);
    assert.match(p, /not plan approval/);
    assert.match(p, /submit_plan/);
    assert.ok(!p.includes(prose));
    assert.ok(!p.includes(PLAN_MISSING_QUESTION));
  });

  it("says so when the guidance is empty", () => {
    assert.match(buildPlanMissingGuidancePrompt("   "), /\(no guidance given\)/);
  });
});

describe("isProseOnlyPlanTurn", () => {
  const cases: Array<[Parameters<typeof isProseOnlyPlanTurn>[0], boolean]> = [
    [{ finalText: "prose" }, true],
    [{ plan: "", finalText: "prose" }, true],
    [{ plan: "  ", questions: [], finalText: "prose" }, true],
    [{ plan: "# plan", finalText: "prose" }, false],
    [{ questions: [{}], finalText: "prose" }, false],
    [{}, false],
    [{ finalText: "" }, false],
    [{ finalText: " \n " }, false],
  ];
  for (const [t, want] of cases) {
    it(`${JSON.stringify(t)} → ${want}`, () => assert.equal(isProseOnlyPlanTurn(t), want));
  }
});

describe("resolvePlanMissing", () => {
  const ctxWith = (askPlanMissing?: RunContext["askPlanMissing"]) => {
    const emitted: EmittedMessage[] = [];
    const ctx = { emit: (m: EmittedMessage) => emitted.push(m), askPlanMissing } as unknown as RunContext;
    return { ctx, emitted };
  };

  it("emits exactly one status card and fails REASON_PLAN_MISSING when unwired", async () => {
    const { ctx, emitted } = ctxWith();
    await assert.rejects(resolvePlanMissing(ctx, "prose"), (e: Error) => e.message === REASON_PLAN_MISSING);
    assert.equal(emitted.length, 1);
    assert.deepEqual(emitted[0]!.payload.event, "plan_missing");
    assert.equal(emitted[0]!.payload.lead_final_message, "prose");
  });

  it("returns guidance built from the owner's answers only", async () => {
    const { ctx, emitted } = ctxWith(async () => ({ kind: "answer", answers: ["one", "two"] }));
    const r = await resolvePlanMissing(ctx, "LEAD-PROSE");
    assert.equal(r.kind, "guidance");
    assert.ok(r.kind === "guidance" && r.prompt.includes("one\ntwo") && !r.prompt.includes("LEAD-PROSE"));
    assert.deepEqual(emitted.map((m) => m.kind), ["status", "answer"]);
    assert.equal(emitted[1]!.agent, "worker");
  });

  it("maps cancel and unattended", async () => {
    assert.deepEqual(await resolvePlanMissing(ctxWith(async () => ({ kind: "cancel" })).ctx, "p"), { kind: "cancel" });
    await assert.rejects(resolvePlanMissing(ctxWith(async () => ({ kind: "unattended" })).ctx, "p"),
      (e: Error) => e.message === REASON_PLAN_MISSING);
  });
});
