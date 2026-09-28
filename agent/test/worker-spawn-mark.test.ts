import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { recordRoot, statStartTime } from "../src/worker-spawn-mark.js";
import { realProcfsSkip } from "./real-procfs.js";

// issue #1783 (R0) — a recorded root is its pid AND its start time (field 22 of procfs `stat`).
// The comm (field 2) is agent-controllable (`prctl(PR_SET_NAME)`, the executable's name) and may
// itself contain ")" and spaces, so fields are counted after the LAST ")".

/** A `stat` line whose field 22 (starttime) is `start`, with `comm` verbatim inside the parens. */
function statLine(comm: string, start: number): string {
  // Fields 3..21, then 22 = start, then the tail (vsize, rss, …).
  const mid = "S 1 1234 1234 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0";
  return `1234 (${comm}) ${mid} ${start} 12345678 321 18446744073709551615\n`;
}

describe("statStartTime", () => {
  it("reads field 22 of a plain stat line", () => {
    assert.equal(statStartTime(statLine("sleep", 987654)), 987654);
  });

  it("counts fields after the LAST ')' when the comm contains ')' and spaces", () => {
    // Counting from the FIRST ")" would shift every field by the comm's extra tokens.
    assert.equal(statStartTime(statLine("a) b (c d", 987654)), 987654);
    assert.equal(statStartTime(statLine(") 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21", 555)), 555);
    assert.equal(statStartTime(statLine("sleep 300)", 42)), 42);
  });

  it("is undefined for an unparsable line", () => {
    assert.equal(statStartTime(""), undefined);
    assert.equal(statStartTime("1234 (no close paren S 1 2 3"), undefined);
    assert.equal(statStartTime("1234 (short) S 1 2 3\n"), undefined);
    assert.equal(statStartTime(statLine("x", 1).replace(" 1 12345678", " -1 12345678")), undefined, "a non-numeric start");
  });

  // Reads the REAL stat file: a sandbox that denies enumerating the proc root denies this read
  // too, and skips through the shared detector (issue #1863).
  const ownStatSkip: string | false = !fs.existsSync(path.join("/", "proc", "self", "stat"))
    ? "no procfs stat file (Linux only)"
    : realProcfsSkip("statStartTime: parses this process's own procfs stat");
  it("parses this process's own procfs stat (Linux)", { skip: ownStatSkip }, () => {
    const v = statStartTime(fs.readFileSync(path.join("/", "proc", "self", "stat"), "utf8"));
    assert.ok(v !== undefined && v > 0, String(v));
  });
});

describe("recordRoot", () => {
  it("binds the pid to the start time read at call time", () => {
    assert.deepEqual(recordRoot(4321, (p) => p + 1), { pid: 4321, startTime: 4322 });
  });

  it("records nothing for an implausible pid or an unreadable start time", () => {
    assert.equal(recordRoot(undefined, () => 1), undefined);
    assert.equal(recordRoot(1, () => 1), undefined);
    assert.equal(recordRoot(4321, () => undefined), undefined);
  });
});
