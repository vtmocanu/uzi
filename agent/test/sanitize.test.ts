import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { MessageBatcher } from "../src/batcher.js";
import { makeRedactor, makeTextRedactor } from "../src/redact.js";
import { emptyCounts, countsTotal, sanitizePayload, sanitizeText } from "../src/sanitize.js";
import { recordingLogger } from "./helpers.js";
import type { WorkerClient } from "../src/client.js";
import type { OutgoingMessage } from "../src/protocol.js";

const NUL = "\u0000";
const LONE_HIGH = "\ud800";
const LONE_LOW = "\udc00";
const REPLACEMENT = "\ufffd";

function collector(): { client: WorkerClient; sent: OutgoingMessage[] } {
  const sent: OutgoingMessage[] = [];
  const client = {
    async postMessages(_runId: string, msgs: OutgoingMessage[]): Promise<void> {
      sent.push(...msgs);
    },
  } as unknown as WorkerClient;
  return { client, sent };
}

describe("sanitizeText (PRD #108, worker-side defense in depth)", () => {
  it("removes NUL and reports the count", () => {
    const c = emptyCounts();
    assert.strictEqual(sanitizeText(`a${NUL}b${NUL}c`, c), "abc");
    assert.strictEqual(c.nul, 2);
    assert.strictEqual(c.surrogate, 0);
  });

  it("replaces unpaired surrogates with U+FFFD, both halves", () => {
    const c = emptyCounts();
    assert.strictEqual(sanitizeText(`x${LONE_HIGH}y`, c), `x${REPLACEMENT}y`);
    assert.strictEqual(sanitizeText(`x${LONE_LOW}y`, c), `x${REPLACEMENT}y`);
    assert.strictEqual(c.surrogate, 2);
  });

  it("leaves well-formed surrogate PAIRS alone — every emoji is one", () => {
    const c = emptyCounts();
    const emoji = "ok \u{1F600} done";
    assert.strictEqual(sanitizeText(emoji, c), emoji);
    assert.strictEqual(countsTotal(c), 0, "a valid pair must not be counted or altered");
  });

  it("is the identity for ordinary text, including newlines, tabs and ANSI", () => {
    const c = emptyCounts();
    // Deliberately NOT stripped: legal in jsonb and load-bearing in tool output.
    const s = "line1\nline2\tcol\u001b[31mred\u001b[0m";
    assert.strictEqual(sanitizeText(s, c), s);
    assert.strictEqual(countsTotal(c), 0);
  });

  it("catches the realistic Node trigger: a string sliced mid-pair", () => {
    // JSON.stringify would emit a lone \udXXX escape for this, which jsonb rejects.
    const sliced = "hi \u{1F600}".slice(0, 4);
    assert.strictEqual(sliced.length, 4, "the slice really did cut the pair in half");
    const c = emptyCounts();
    const out = sanitizeText(sliced, c);
    assert.strictEqual(c.surrogate, 1);
    // The proof that matters: the result survives a stringify/parse round-trip.
    assert.strictEqual(JSON.parse(JSON.stringify(out)), out);
    assert.ok(!/[\ud800-\udfff]/.test(out) || /[\ud800-\udbff][\udc00-\udfff]/.test(out));
  });

  it("sanitizes object KEYS as well as values", () => {
    const c = emptyCounts();
    const out = sanitizePayload({ [`k${NUL}ey`]: `v${NUL}al`, nested: { arr: [`a${NUL}`] } }, c);
    assert.deepStrictEqual(out, { key: "val", nested: { arr: ["a"] } });
    assert.strictEqual(c.nul, 3, "a NUL in a key breaks the insert exactly like one in a value");
  });

  it("leaves structure, numbers, booleans and null untouched", () => {
    const c = emptyCounts();
    const input = { n: 1.5, big: 12345678901234, ok: true, nil: null, arr: [1, "x", false] };
    assert.deepStrictEqual(sanitizePayload(input, c), input);
    assert.strictEqual(countsTotal(c), 0);
  });
});

describe("MessageBatcher sanitizes BEFORE redacting (PRD #108, security)", () => {
  const SECRET = "super-secret-token-abcdef123456";

  it("a secret split by an embedded NUL is still redacted, not reconstituted in the clear", async () => {
    // The hole this ordering closes. The redactors are exact-substring matchers, so
    // `sec\0ret` does NOT match `secret`. Sanitize-then-redact means the redactor
    // sees the reassembled secret and scrubs it. Redact-then-sanitize would MISS it
    // and then reassemble it in the clear, on the wire and in the browser.
    const split = `${SECRET.slice(0, 10)}${NUL}${SECRET.slice(10)}`;
    const { logger } = recordingLogger();
    const { client, sent } = collector();
    const batcher = new MessageBatcher(
      client,
      "run-s",
      0,
      0,
      logger,
      makeRedactor([SECRET]),
      makeTextRedactor([SECRET]),
    );

    batcher.emit({ kind: "tool_result", agent: "lead", payload: { content: `token=${split}` } });
    await batcher.close();

    const wire = JSON.stringify(sent[0]);
    assert.ok(!wire.includes(SECRET), "the reassembled secret must not reach the wire");
    assert.ok(wire.includes("***REDACTED***"), "it was scrubbed, not merely broken up");
    assert.ok(!wire.includes("\\u0000"), "and the NUL is gone");
  });

  it("applies the same order to agent_label, which is worker-controlled text", async () => {
    const split = `${SECRET.slice(0, 12)}${NUL}${SECRET.slice(12)}`;
    const { logger } = recordingLogger();
    const { client, sent } = collector();
    const batcher = new MessageBatcher(
      client,
      "run-s2",
      0,
      0,
      logger,
      makeRedactor([SECRET]),
      makeTextRedactor([SECRET]),
    );

    batcher.emit({ kind: "tool_use", agent: "coder", agentLabel: `leak ${split} here`, payload: {} });
    await batcher.close();

    const label = sent[0]?.agent_label ?? "";
    assert.ok(!label.includes(SECRET));
    assert.ok(label.includes("***REDACTED***"));
    assert.ok(!label.includes(NUL), "Postgres `text` cannot hold a NUL either");
  });

  it("counts and logs every strip, so a NUL-emitting tool stays visible", async () => {
    const { logger, lines } = recordingLogger();
    const { client } = collector();
    const batcher = new MessageBatcher(client, "run-s3", 0, 0, logger);

    batcher.emit({ kind: "tool_result", agent: "lead", payload: { content: `a${NUL}b${LONE_HIGH}c` } });
    await batcher.close();

    const warn = lines.find(
      (l): l is Record<string, unknown> =>
        !!l && typeof l === "object" &&
        (l as { msg?: string }).msg === "stripped codepoints Postgres cannot store from a run message",
    );
    assert.ok(warn, "silently laundering a NUL means the emitting tool is never investigated");
    assert.strictEqual(warn["nul"], 1);
    assert.strictEqual(warn["unpaired_surrogate"], 1);
    assert.strictEqual(warn["seq"], 1);
  });

  it("does not log for an ordinary message", async () => {
    const { logger, lines } = recordingLogger();
    const { client } = collector();
    const batcher = new MessageBatcher(client, "run-s4", 0, 0, logger);
    batcher.emit({ kind: "text", agent: "lead", payload: { text: "nothing to strip here" } });
    await batcher.close();
    assert.ok(
      !lines.some((l) => !!l && typeof l === "object" && String((l as { msg?: string }).msg).startsWith("stripped")),
      "the common path must stay quiet",
    );
  });

  it("the incident's own payload shape survives sanitation and stringifies cleanly", async () => {
    // 84 NUL bytes inside a 25,418-byte tool_result — HarfBuzz spew from a headless
    // Chromium, which is what SQLSTATE 22P05 rejected on 2026-07-21.
    const spew = "x".repeat(300) + NUL.repeat(84) + "y".repeat(300);
    const { logger } = recordingLogger();
    const { client, sent } = collector();
    const batcher = new MessageBatcher(client, "run-s5", 0, 0, logger);
    batcher.emit({ kind: "tool_result", agent: "lead", payload: { content: spew } });
    await batcher.close();

    const first = sent[0];
    // Asserted BEFORE indexing into it (PRD #103 M3, oxlint
    // eslint(no-unsafe-optional-chaining)): the old form was
    // `(sent[0]?.payload as …).content`, which on an empty `sent` threw a
    // TypeError from the optional chain rather than failing an assertion. Same
    // coverage, a readable failure instead of a crash.
    assert.ok(first, "the batcher must have sent a frame");
    const wire = JSON.stringify(first);
    assert.ok(!wire.includes("\\u0000"), "no NUL escape reaches the api");
    assert.strictEqual((first.payload as { content: string }).content.length, 600);
  });
});
