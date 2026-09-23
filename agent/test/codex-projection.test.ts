import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  MAX_PROJECTED_BYTES,
  boundJsonString,
  boundUtf8,
  newProjectionNonce,
  projectedId,
  projectToolInput,
  projectToolName,
  projectToolOutput,
} from "../src/codex/projection.js";

// Issue #1583 — the pure Codex tool projection: UTF-8 byte bounding, scrub-before-bound, the
// oversized-input shape and the namespaced id helper.

const LONE_SURROGATE = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
const REPLACEMENT = "\uFFFD";

function assertWellBounded(out: string, cap: number): void {
  assert.ok(Buffer.byteLength(out, "utf8") <= cap, `byteLength ${Buffer.byteLength(out, "utf8")} > cap ${cap}`);
  assert.ok(!LONE_SURROGATE.test(out), "no lone surrogate");
  assert.ok(!out.includes(REPLACEMENT), "no U+FFFD introduced");
}

function droppedOf(out: string): number {
  const m = /…\[truncated (\d+) bytes\]$/.exec(out);
  assert.ok(m, `marker present in ${JSON.stringify(out.slice(-40))}`);
  return Number(m[1]);
}

describe("codex projection: boundUtf8", () => {
  it("returns a string that fits unchanged", () => {
    const s = "héllo 😀 世界";
    assert.equal(boundUtf8(s, Buffer.byteLength(s, "utf8")), s);
    assert.equal(boundUtf8("", 0), "");
  });

  it("cuts 4-byte emoji straddling the cap on a code-point boundary, marker included in the cap", () => {
    const s = "😀".repeat(100); // 400 bytes, 200 UTF-16 units
    for (let cap = 30; cap <= 60; cap++) {
      const out = boundUtf8(s, cap);
      assertWellBounded(out, cap);
      const prefix = out.slice(0, out.indexOf("…"));
      assert.equal(droppedOf(out), 400 - Buffer.byteLength(prefix, "utf8"), "marker reports the dropped bytes");
      assert.equal(prefix.length % 2, 0, "only whole surrogate pairs are kept");
    }
  });

  it("cuts 3-byte CJK straddling the cap on a code-point boundary", () => {
    const s = "a" + "世".repeat(100); // 301 bytes
    for (let cap = 30; cap <= 40; cap++) {
      const out = boundUtf8(s, cap);
      assertWellBounded(out, cap);
      const prefix = out.slice(0, out.indexOf("…"));
      assert.equal(droppedOf(out), 301 - Buffer.byteLength(prefix, "utf8"));
    }
  });

  it("bounds a large mixed string to the projection cap", () => {
    const s = ("x😀世é").repeat(10_000);
    const out = boundUtf8(s, MAX_PROJECTED_BYTES);
    assertWellBounded(out, MAX_PROJECTED_BYTES);
    assert.ok(Buffer.byteLength(out, "utf8") > MAX_PROJECTED_BYTES - 64, "keeps close to the cap");
  });
});

describe("codex projection: tool input/output", () => {
  const scrub = (s: string): string => s.split("SEKRET-VALUE-42").join("***REDACTED***");

  it("copies a small input whole, scrubbing every nested string", () => {
    const out = projectToolInput({ command: "echo SEKRET-VALUE-42", nested: { list: ["SEKRET-VALUE-42", 1] } }, scrub);
    assert.deepEqual(out, { command: "echo ***REDACTED***", nested: { list: ["***REDACTED***", 1] } });
    assert.deepEqual(projectToolInput(undefined, scrub), {});
  });

  it("an oversized input keeps description/file_path + a bounded preview and truncated:true", () => {
    const args = {
      description: "patch the SEKRET-VALUE-42 file",
      file_path: "src/a.ts",
      content: "y".repeat(64 * 1024),
    };
    const out = projectToolInput(args, scrub) as Record<string, unknown>;
    assert.equal(out.description, "patch the ***REDACTED*** file");
    assert.equal(out.file_path, "src/a.ts");
    assert.equal(out.truncated, true);
    assert.equal(out.content, undefined);
    assert.equal(typeof out.preview, "string");
    assert.ok(!(out.preview as string).includes("SEKRET-VALUE-42"), "the preview is scrubbed");
    assert.ok(Buffer.byteLength(JSON.stringify(out), "utf8") <= MAX_PROJECTED_BYTES);
  });

  it("projects output scrubbed BEFORE bounding: a secret straddling the cut never leaks a fragment", () => {
    const pad = "z".repeat(MAX_PROJECTED_BYTES - 40);
    const out = projectToolOutput({ ok: true, output: pad + "SEKRET-VALUE-42" + "q".repeat(100) }, scrub);
    assert.ok(!out.includes("SEKRET"), "no fragment of the secret survives the cut");
    assertWellBounded(out, MAX_PROJECTED_BYTES);
    assert.equal(projectToolOutput({ ok: false, code: "denied", message: "no SEKRET-VALUE-42" }, scrub), "no ***REDACTED***");
    assert.equal(projectToolOutput({ ok: true, output: { code: 0 } }, scrub), '{"code":0}');
  });

  it("bounds and defaults the tool name", () => {
    assert.equal(projectToolName(undefined, scrub), "unknown");
    assert.equal(projectToolName("", scrub), "unknown");
    assert.equal(projectToolName("Bash", scrub), "Bash");
    assert.ok(Buffer.byteLength(projectToolName("n".repeat(5000), scrub), "utf8") <= 256);
  });

  it("strips control and bidi/format characters from the tool name", () => {
    const hostile = "Ba\u001b[31msh\n\r\t\u202eevil\u200b\u2028\u0085\ufeff";
    const name = projectToolName(hostile, scrub);
    assert.equal(name, "Ba[31mshevil");
    for (const bad of ["\u001b", "\n", "\r", "\t", "\u202e", "\u200b", "\u2028", "\u0085", "\ufeff"]) {
      assert.ok(!name.includes(bad), `stripped ${JSON.stringify(bad)}`);
    }
    assert.equal(projectToolName("\u001b\u202e\n", scrub), "unknown", "all-unsafe falls back to unknown");
  });

  it("an oversized input of backslashes/quotes stays within the cap once SERIALIZED", () => {
    for (const unit of ["\\", '"', "\u0001", "\n"]) {
      const B = unit.repeat(40_000);
      const out = projectToolInput({ description: B, file_path: B, path: B, skill: B, command: B }, scrub) as Record<string, unknown>;
      assert.equal(out.truncated, true);
      const bytes = Buffer.byteLength(JSON.stringify(out), "utf8");
      assert.ok(bytes <= MAX_PROJECTED_BYTES, `serialized ${bytes} > ${MAX_PROJECTED_BYTES} for ${JSON.stringify(unit)}`);
      assert.equal(typeof out.description, "string", "display fields survive");
    }
  });

  it("a backslash/quote/control-heavy output serializes within the cap (+2 quotes)", () => {
    for (const unit of ["\\", '"', "\u0001", "\n", "😀"]) {
      const out = projectToolOutput({ ok: true, output: unit.repeat(40_000) }, scrub);
      assert.ok(Buffer.byteLength(JSON.stringify(out), "utf8") <= MAX_PROJECTED_BYTES + 2, `for ${JSON.stringify(unit)}`);
      assert.ok(!LONE_SURROGATE.test(out));
      assert.match(out, /…\[truncated \d+ bytes\]$/);
    }
  });

  it("does not overflow the stack on deeply nested input, replacing the deep subtree with a marker", () => {
    let deep: unknown = [];
    for (let i = 0; i < 20_000; i++) deep = [deep];
    const out = projectToolInput({ command: "echo hi", x: deep }, scrub) as Record<string, unknown>;
    assert.equal(out.command, "echo hi");
    assert.ok(JSON.stringify(out).includes("[depth limit]"));
    let d: unknown = out.x;
    let levels = 0;
    while (Array.isArray(d)) {
      d = d[0];
      levels += 1;
    }
    assert.equal(d, "[depth limit]");
    assert.ok(levels <= 32, `walked ${levels} levels`);
  });

  it("does not throw on a cyclic object, cutting the cycle with a marker", () => {
    const a: Record<string, unknown> = { command: "SEKRET-VALUE-42" };
    a.self = a;
    const shared = { v: 1 };
    a.list = [shared, shared];
    const out = projectToolInput(a, scrub);
    assert.deepEqual(out, { command: "***REDACTED***", self: "[cycle]", list: [{ v: 1 }, { v: 1 }] });
  });
});

describe("codex projection: boundJsonString", () => {
  it("returns a string whose escaped form fits unchanged, and bounds the escaped form otherwise", () => {
    assert.equal(boundJsonString('a"b\\c', 7), 'a"b\\c');
    for (let cap = 30; cap <= 60; cap++) {
      const out = boundJsonString('\\"\u0000😀'.repeat(50), cap);
      assert.ok(Buffer.byteLength(JSON.stringify(out), "utf8") - 2 <= cap, `cap ${cap}`);
      assert.ok(!LONE_SURROGATE.test(out));
    }
  });
});

describe("codex projection: namespaced ids", () => {
  it("sanitises a hostile call id and caps it at 64 chars", () => {
    const hostile = "call\n\r\t\u0000\u001b[31m<script>" + "A".repeat(500);
    const id = projectedId("abcdef012345", 3, hostile, 1);
    assert.match(id, /^cx-abcdef012345-t3-[A-Za-z0-9_.:-]{1,64}$/);
    for (const bad of ["\n", "\r", "\t", "\u0000", "\u001b", "<", ">"]) assert.ok(!id.includes(bad));
    assert.equal(id, `cx-abcdef012345-t3-call31mscript${"A".repeat(64 - "call31mscript".length)}`);
  });

  it("falls back to n<counter> when the call id is empty after sanitising", () => {
    assert.equal(projectedId("abcdef012345", 1, "", 7), "cx-abcdef012345-t1-n7");
    assert.equal(projectedId("abcdef012345", 1, "\n\u0000 ", 2), "cx-abcdef012345-t1-n2");
  });

  it("a fresh nonce is 12 lowercase hex chars", () => {
    assert.match(newProjectionNonce(), /^[0-9a-f]{12}$/);
  });
});
