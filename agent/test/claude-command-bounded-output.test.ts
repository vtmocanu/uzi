import assert from "node:assert/strict";
import { test } from "node:test";
import { BoundedOutput, INLINE_CAP_CHARS } from "../src/claude-command/bounded-output.js";

// Uppercase/digit secret: none of its characters occur in the lowercase filler or in the
// redaction placeholder letters, so any partial leak is detectable by a character class.
const SECRET = "Q9Z7W4X2-Q9Z7W4X2";
const FILLER_OK = /^[ab\n]*(?:[*REDACT]{0,14})[ab\n]*$/;

function feed(out: BoundedOutput, text: string, chunk = 4096): void {
  const bytes = Buffer.from(text, "utf8");
  for (let i = 0; i < bytes.length; i += chunk) out.write(0, bytes.subarray(i, i + chunk));
  out.end();
}
function split(text: string): { head: string; tail: string } {
  const open = text.indexOf("\n[... output truncated");
  const close = text.indexOf("...]\n", open);
  assert.ok(open >= 0 && close > open, "truncation marker present");
  return { head: text.slice(0, open), tail: text.slice(close + "...]\n".length) };
}

test("short output is returned verbatim and untruncated, with the byte count", { timeout: 10000 }, () => {
  const out = new BoundedOutput();
  feed(out, "hello\nworld\n");
  const r = out.render();
  assert.equal(r.text, "hello\nworld\n");
  assert.equal(r.truncated, false);
  assert.equal(out.totalBytes, 12);
});

test("output over the inline cap keeps head and tail within the total cap", { timeout: 10000 }, () => {
  const out = new BoundedOutput();
  feed(out, `HEAD-SENTINEL${"m".repeat(100_000)}TAIL-SENTINEL`);
  const r = out.render();
  assert.equal(r.truncated, true);
  assert.ok(r.text.length <= INLINE_CAP_CHARS, `inline text is ${String(r.text.length)} chars`);
  assert.ok(r.text.startsWith("HEAD-SENTINEL"));
  assert.ok(r.text.endsWith("TAIL-SENTINEL"));
  assert.match(r.text, /output truncated: 100026 bytes in total/);
});

test("exactly the cap is not truncated and one more character is", { timeout: 10000 }, () => {
  const exact = new BoundedOutput();
  feed(exact, "x".repeat(INLINE_CAP_CHARS));
  assert.equal(exact.render().truncated, false);
  assert.equal(exact.render().text.length, INLINE_CAP_CHARS);
  const over = new BoundedOutput();
  feed(over, "x".repeat(INLINE_CAP_CHARS + 1));
  const r = over.render();
  assert.equal(r.truncated, true);
  assert.ok(r.text.length <= INLINE_CAP_CHARS);
});

test("200 MiB of streamed output keeps retained worker memory bounded", { timeout: 60000 }, () => {
  const out = new BoundedOutput([SECRET]);
  const chunk = Buffer.alloc(64 * 1024, "y");
  chunk[0] = 0x0a;
  const total = 200 * 1024 * 1024;
  let max = 0;
  out.write(0, Buffer.from("FIRST-LINE\n"));
  for (let sent = 0; sent < total; sent += chunk.length) {
    out.write(sent % 2 === 0 ? 0 : 1, chunk);
    max = Math.max(max, out.retainedChars);
  }
  out.write(0, Buffer.from("\nLAST-LINE"));
  out.end();
  assert.equal(out.totalBytes, total + "FIRST-LINE\n".length + "\nLAST-LINE".length);
  // Head <= 15000 + margin; tail <= 2 * (15000 + margin); margin = len(secret) - 1.
  assert.ok(max <= 3 * (INLINE_CAP_CHARS / 2 + SECRET.length), `retained ${String(max)} chars`);
  const r = out.render();
  assert.ok(r.text.length <= INLINE_CAP_CHARS);
  assert.ok(r.text.startsWith("FIRST-LINE"));
  assert.ok(r.text.endsWith("LAST-LINE"));
  assert.match(r.text, new RegExp(`${String(out.totalBytes)} bytes in total`));
});

test("a secret straddling the head cut or the tail cut never leaks a fragment", { timeout: 60000 }, () => {
  // Longer than the slack the marker leaves, so only the redaction margin can protect the cut.
  const long = "Q9Z7W4X2-".repeat(25);
  for (let offset = 14_700; offset <= 15_050; offset++) {
    const out = new BoundedOutput([long]);
    feed(out, `${"a".repeat(offset)}${long}${"b".repeat(40_000)}`);
    const { head, tail } = split(out.render().text);
    assert.match(head, FILLER_OK, `head cut leaked at offset ${String(offset)}`);
    assert.match(tail, FILLER_OK);
  }
  for (let after = 14_700; after <= 15_050; after++) {
    const out = new BoundedOutput([long]);
    feed(out, `${"a".repeat(40_000)}${long}${"b".repeat(after)}`);
    const { head, tail } = split(out.render().text);
    assert.match(head, FILLER_OK);
    assert.match(tail, FILLER_OK, `tail cut leaked with ${String(after)} chars after the secret`);
  }
});

test("a secret fully inside the kept window is replaced, and one in the omitted middle is absent", { timeout: 10000 }, () => {
  const out = new BoundedOutput([SECRET]);
  feed(out, `start ${SECRET} ${"m".repeat(60_000)} ${SECRET} end`);
  const r = out.render();
  assert.ok(!r.text.includes(SECRET));
  assert.ok(!r.text.includes("Q9Z7"));
  assert.equal(r.text.split("***REDACTED***").length - 1, 2);
});

test("redaction shrinking a window never exposes a buffer edge", { timeout: 20000 }, () => {
  // Pure repeated secret: the redacted head and tail are shorter than their budget.
  const out = new BoundedOutput([SECRET]);
  feed(out, SECRET.repeat(200_000), 65536);
  const r = out.render();
  assert.ok(r.text.length <= INLINE_CAP_CHARS);
  assert.ok(!/[QZWX]/.test(r.text.replace(/bytes in total/g, "")), "no secret character survives");
});

test("control characters are neutralised, CRLF becomes LF, tab and newline are kept", { timeout: 10000 }, () => {
  const out = new BoundedOutput();
  feed(out, "a\u001b[31mred\u0007\u0000\r\nb\tc\rd e‮f");
  const r = out.render();
  assert.equal(r.text, "a�[31mred��\nb\tc\nd�e�f");
});

test("multibyte characters split across chunks and streams decode intact", { timeout: 10000 }, () => {
  const out = new BoundedOutput();
  const bytes = Buffer.from("café \u{1f600} 漢", "utf8");
  for (let i = 0; i < bytes.length; i++) out.write(0, bytes.subarray(i, i + 1));
  out.end();
  assert.equal(out.render().text, "café \u{1f600} 漢");
});

test("a cut through a surrogate pair never emits a lone surrogate", { timeout: 20000 }, () => {
  for (let shift = 0; shift < 2; shift++) {
    const out = new BoundedOutput();
    feed(out, `${"x".repeat(shift)}${"\u{1f600}".repeat(40_000)}${"x".repeat(shift)}`);
    const r = out.render();
    assert.ok(!/[\ud800-\udbff](?![\udc00-\udfff])|(?<![\ud800-\udbff])[\udc00-\udfff]/.test(r.text), "inline text is well-formed UTF-16");
    assert.ok(r.text.length <= INLINE_CAP_CHARS);
  }
});
