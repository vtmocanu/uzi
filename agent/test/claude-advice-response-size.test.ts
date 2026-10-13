// Issue #2828 — the Claude advice harness bounds its accumulated response text.
//
// ClaudeAdviceHarness used to append every assistant text chunk with no bound, so a runaway or
// looping stream could grow worker memory without limit. It now fails closed at 8 MiB of UTF-8
// (the Codex twin's MAX_ADVICE_TEXT_BYTES value): the call rejects with the bounded error and
// never returns truncated text. Pattern: claude-conformance-advice-ceiling.test.ts constructs
// the harness directly with a fake queryFn over scripted frames; no SDK, token or network.

import test from "node:test";
import assert from "node:assert/strict";
import os from "node:os";
import path from "node:path";

import { ClaudeAdviceHarness } from "../src/claude-advice-harness.js";
import type { AdviceResult, AdviceUsageSnapshot } from "../src/harness.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { nullLogger } from "./helpers.js";

const CAP = 8 * 1024 * 1024;
const OVERFLOW = /advice response exceeded the maximum size/;

const assistantText = (text: string, id?: string) => ({
  type: "assistant",
  message: {
    role: "assistant",
    content: [{ type: "text", text }],
    ...(id !== undefined ? { id, model: "claude-test", usage: { input_tokens: 7, output_tokens: 3 } } : {}),
  },
});
const successResult = () => ({ type: "result", subtype: "success", is_error: false });

async function runAdvice(frames: unknown[], usageObserver?: (s: AdviceUsageSnapshot) => void): Promise<AdviceResult> {
  const queryFn = (() =>
    (async function* () {
      for (const f of frames) yield f;
    })()) as unknown as SdkQueryFn;
  const abort = new AbortController();
  const harness = new ClaudeAdviceHarness({
    token: "dummy-advice-tok",
    homeDir: path.join(os.tmpdir(), "uzi-2828-advice-home-does-not-exist"),
    abort,
    queryFn,
    denyReason: "advice lane is read-only and runs no tools",
    log: nullLogger(),
  });
  return harness.run(
    {
      label: "review",
      systemPrompt: "sys",
      prompt: "p",
      output: { kind: "text" },
      signal: abort.signal,
      timeoutMs: 5000,
      ...(usageObserver ? { usageObserver } : {}),
    },
    { onTerminal() {} },
  );
}

test("advice size: a single chunk past 8 MiB rejects with the bounded error and returns no text", async () => {
  let result: AdviceResult | undefined;
  await assert.rejects(
    (async () => {
      result = await runAdvice([assistantText("a".repeat(CAP + 1)), successResult()]);
    })(),
    OVERFLOW,
  );
  assert.equal(result, undefined, "no (truncated) result may be returned");
});

test("advice size: text one byte below 8 MiB returns intact", async () => {
  const text = "b".repeat(CAP - 1);
  const result = await runAdvice([assistantText(text), successResult()]);
  assert.equal(result.text.length, CAP - 1);
  assert.equal(result.text, text);
});

test("advice size: text of exactly 8 MiB returns intact", async () => {
  const half = CAP / 2;
  const result = await runAdvice([assistantText("c".repeat(half)), assistantText("d".repeat(half)), successResult()]);
  assert.equal(Buffer.byteLength(result.text, "utf8"), CAP);
  assert.equal(result.text, "c".repeat(half) + "d".repeat(half));
});

test("advice size: many small chunks reject once their cumulative bytes exceed 8 MiB", async () => {
  const chunk = "e".repeat(64 * 1024);
  const frames: unknown[] = [];
  for (let i = 0; i < CAP / chunk.length; i++) frames.push(assistantText(chunk));
  frames.push(assistantText("f")); // the one byte past the cap
  frames.push(successResult());
  await assert.rejects(runAdvice(frames), OVERFLOW);
});

test("advice size: multibyte text under the cap in characters but over it in UTF-8 bytes rejects", async () => {
  // "€" is one UTF-16 code unit and three UTF-8 bytes: CAP/2 characters is 1.5 × CAP bytes.
  const text = "€".repeat(CAP / 2);
  assert.ok(text.length < CAP, "the character count stays below the cap");
  assert.ok(Buffer.byteLength(text, "utf8") > CAP, "the UTF-8 byte count exceeds the cap");
  await assert.rejects(runAdvice([assistantText(text), successResult()]), OVERFLOW);
});

test("advice size: usage observed before the overflow is still published", async () => {
  const seen: AdviceUsageSnapshot[] = [];
  await assert.rejects(
    runAdvice([assistantText("g", "msg-1"), assistantText("h".repeat(CAP), "msg-2"), successResult()], (s) => seen.push(s)),
    OVERFLOW,
  );
  const last = seen.at(-1);
  assert.ok(last, "usage was published before the throw");
  assert.equal(last["claude-test"]?.inputTokens, 14, "both messages' usage, including the overflowing frame's, was published");
});
