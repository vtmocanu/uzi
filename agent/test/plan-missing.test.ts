import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
  LEAD_MESSAGE_SECRET_MARGIN,
  LEAD_TEXT_TAIL_KEEP,
  MAX_LEAD_FINAL_MESSAGE_LEN,
  PLAN_MISSING_QUESTION,
  REASON_PLAN_MISSING,
  appendLeadTextTail,
  boundLeadFinalMessage,
  buildPlanMissingGuidancePrompt,
  isProseOnlyPlanTurn,
  resolvePlanMissing,
} from "../src/plan-missing.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";

// Issue #1593: the shared pieces of the prose-only planning-turn recovery. The executor
// wiring is covered in codex-executor / ask-user-executor tests; this pins the bounding,
// the prompt's trust boundary and the prose-only predicate.

const MARKER = "[truncated]…";

describe("boundLeadFinalMessage", () => {
  it("returns short text unchanged", () => {
    assert.equal(boundLeadFinalMessage("a short message\nwith\ttabs"), "a short message\nwith\ttabs");
  });

  it("keeps the TAIL of long text, capped at the limit with the marker prefixed", () => {
    const out = boundLeadFinalMessage("HEAD-" + "y".repeat(MAX_LEAD_FINAL_MESSAGE_LEN * 2) + "-TAILEND");
    assert.ok(out.length <= MAX_LEAD_FINAL_MESSAGE_LEN);
    assert.ok(out.startsWith(MARKER), JSON.stringify(out.slice(0, 20)));
    assert.ok(out.endsWith("-TAILEND"), "the end of the lead's message is what is shown");
    assert.ok(!out.includes("HEAD-"));
    assert.equal(boundLeadFinalMessage("z".repeat(MAX_LEAD_FINAL_MESSAGE_LEN)).length, MAX_LEAD_FINAL_MESSAGE_LEN, "exactly at the cap is not truncated");
    assert.ok(!boundLeadFinalMessage("z".repeat(MAX_LEAD_FINAL_MESSAGE_LEN)).startsWith(MARKER));
  });

  it("processes only a bounded tail window of an arbitrarily long input", () => {
    const seen: number[] = [];
    const redact = (s: string) => { seen.push(s.length); return s; };
    boundLeadFinalMessage("w".repeat(2_000_000), redact);
    assert.ok(seen.length > 0);
    assert.ok(Math.max(...seen) <= MAX_LEAD_FINAL_MESSAGE_LEN + LEAD_MESSAGE_SECRET_MARGIN,
      `the redactor saw ${Math.max(...seen)} chars`);
  });

  it("drops a secret split at the window's left edge, even when stripping shrinks the window", () => {
    const secret = "sekret-" + "leftedge-1593-" + "abcdefghijklmnop";
    const redact = (s: string) => s.split(secret).join("[REDACTED]");
    const window = MAX_LEAD_FINAL_MESSAGE_LEN + LEAD_MESSAGE_SECRET_MARGIN;
    // Only the secret's last 12 chars fall inside the window, and 2000 NULs (stripped) shrink
    // what is left, so without an explicit left-edge drop the unmatched suffix would sit inside
    // the cap.
    const suffix = secret.slice(-12);
    const tail = " the end";
    const nuls = "\u0000".repeat(2000);
    const text = "p".repeat(50) + secret + nuls + "f".repeat(window - suffix.length - nuls.length - tail.length) + tail;
    const out = boundLeadFinalMessage(text, redact);
    assert.ok(!out.includes(suffix), JSON.stringify(out.slice(0, 40)));
    assert.ok(out.endsWith(tail));
    assert.ok(out.startsWith(MARKER));
    // Degenerate: a window that is almost all stripped characters keeps only the marker.
    const allNul = "p".repeat(50) + secret + "\u0000".repeat(window);
    assert.equal(boundLeadFinalMessage(allNul, redact), MARKER);
  });

  it("redacts BEFORE the cut, so a secret straddling the cap leaves no suffix", () => {
    const secret = "sekret-" + "straddle-1593-abcdef";
    const redact = (s: string) => s.split(secret).join("[REDACTED]");
    // The secret ends just after where the tail cut lands, so a cut-then-redact would keep
    // its last few characters, which no longer match the full secret.
    const text = "q".repeat(100) + secret + "p".repeat(MAX_LEAD_FINAL_MESSAGE_LEN - MARKER.length - 5);
    const out = boundLeadFinalMessage(text, redact);
    assert.ok(out.length <= MAX_LEAD_FINAL_MESSAGE_LEN);
    assert.ok(!out.includes(secret.slice(-5)), `no secret suffix may survive: ${JSON.stringify(out.slice(0, 30))}`);
  });

  it("strips controls BEFORE redacting, so a secret split by an invisible char is still redacted", () => {
    const secret = "sekret-" + "split-1593-xyz";
    const redact = (s: string) => s.split(secret).join("[REDACTED]");
    for (const sep of ["\u200b", "\u0000", "\u2028", "\u2029", "\u202e"]) {
      const split = secret.slice(0, 6) + sep + secret.slice(6);
      assert.equal(boundLeadFinalMessage(`before ${split} after`, redact), "before [REDACTED] after", `split by U+${sep.charCodeAt(0).toString(16)}`);
    }
  });

  it("strips NUL, C0 (except \\n and \\t), DEL, C1, bidi/format controls and line/paragraph separators", () => {
    const nasty = "a\u0000b\u0007c\u001bd\u007fe\u0085f\u202eg\u2066h\u200bi\ufeffj\u2028k\u2029l\n\tm";
    assert.equal(boundLeadFinalMessage(nasty), "abcdefghijkl\n\tm");
  });

  it("never starts the kept tail on a lone low surrogate", () => {
    for (let pad = 0; pad < 4; pad++) {
      const out = boundLeadFinalMessage("\u{1F600}".repeat(MAX_LEAD_FINAL_MESSAGE_LEN) + "a".repeat(pad));
      const first = out.charCodeAt(MARKER.length);
      assert.ok(!(first >= 0xdc00 && first <= 0xdfff), `no lone low surrogate after the marker (pad ${pad})`);
      assert.ok(!out.includes("\ufffd"), "no replacement char from a split pair");
    }
  });
});

describe("appendLeadTextTail", () => {
  it("joins with newlines and keeps only the most recent LEAD_TEXT_TAIL_KEEP chars", () => {
    assert.equal(appendLeadTextTail(appendLeadTextTail("", "one"), "two"), "one\ntwo");
    let acc = "";
    for (let i = 0; i < 200; i++) acc = appendLeadTextTail(acc, `chunk-${i}-` + "x".repeat(1000));
    assert.ok(acc.length <= LEAD_TEXT_TAIL_KEEP, `held ${acc.length}`);
    assert.ok(acc.endsWith("chunk-199-" + "x".repeat(1000)));
    const big = appendLeadTextTail("", "y".repeat(LEAD_TEXT_TAIL_KEEP * 3) + "END");
    assert.ok(big.length <= LEAD_TEXT_TAIL_KEEP && big.endsWith("END"));
    assert.ok(LEAD_TEXT_TAIL_KEEP >= MAX_LEAD_FINAL_MESSAGE_LEN + LEAD_MESSAGE_SECRET_MARGIN,
      "the held tail covers the bounder's whole window");
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
