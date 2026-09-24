import { describe, it } from "node:test";
import assert from "node:assert/strict";
import {
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
import { makeTextRedactor } from "../src/redact.js";

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

  it("hands the redactor the complete sanitized input, so a secret far from the tail is redacted", () => {
    const secret = "sekret-" + "fullinput-1593-" + "abcdefghijklmnop";
    const seen: number[] = [];
    const redact = (s: string) => { seen.push(s.length); return s.split(secret).join("[REDACTED]"); };
    const tail = " the end";
    const text = secret + "\u0000".repeat(10) + "w".repeat(200_000) + tail;
    const out = boundLeadFinalMessage(text, redact);
    assert.deepEqual(seen, [text.length - 10], "the redactor saw the whole input, NULs stripped");
    assert.ok(out.startsWith(MARKER));
    assert.ok(out.endsWith(tail));
    assert.ok(!out.includes("sekret-"));
  });

  it("redacts a 5000-char secret that starts left of the old bounded window", () => {
    // Regression: the bounder used to slice a tail window of cap + 4096 chars BEFORE
    // redacting, so a secret longer than that margin, starting left of the window, left an
    // unrecognisable suffix. Clearly synthetic: a deterministic base64url-ish sequence
    // assembled at runtime, so no token-shaped literal sits in source.
    const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    const body = (n: number, seed: number) =>
      Array.from({ length: n }, (_, i) => alphabet[(i * 37 + seed * 11 + ((i * i) % 61)) % 64]).join("");
    const secret = body(5000, 4);
    assert.equal(secret.length, 5000);
    const redact = makeTextRedactor([secret]);
    // The old implementation's window: the cap (4000) plus its 4096-char secret margin.
    const oldWindow = 4000 + 4096;
    const inside = 4500; // 4500 of the secret's chars fall inside the old window, 500 left of it
    const tail = " the end";
    // A long prefix left of the secret keeps the redacted text over the cap, so it is cut.
    const prefix = "p".repeat(1000);
    const text = prefix + secret + "~".repeat(oldWindow - inside - tail.length) + tail;
    assert.equal(text.length - (prefix.length + secret.length - inside), oldWindow);
    const out = boundLeadFinalMessage(text, redact);
    assert.ok(out.startsWith(MARKER));
    assert.ok(out.endsWith(tail));
    for (let i = 0; i + 16 <= secret.length; i++) {
      const frag = secret.slice(i, i + 16);
      assert.ok(!out.includes(frag), `a 16-char fragment of the secret survived at offset ${i}`);
    }
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
    assert.ok(LEAD_TEXT_TAIL_KEEP >= MAX_LEAD_FINAL_MESSAGE_LEN,
      "the held tail covers the status card's cap");
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
