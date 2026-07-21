// Issue #105: the resume preflight.
//
// Two properties are load-bearing and both are pinned here.
//
// SCOPE. The check must mirror the CLI's own lookup, which is NOT a scan of every
// project dir — it resolves `<HOME>/.claude/projects/<encoded-cwd>/<sid>.jsonl`. A
// transcript in some OTHER project dir is invisible to the CLI, so answering "present"
// for it would keep a resume the CLI then fails on, leaving the bug exactly as it was.
// Over-broad is not conservative here; it silently un-fixes the bug.
//
// FAIL-OPEN, asymmetric. "The file is not there" (ENOENT/ENOTDIR) is an answer and
// drops the resume. Anything undeterminable — unreadable dir, unparseable id, a cwd
// whose encoding we cannot reproduce — keeps it and degrades to the CLI's loud failure.
// Keeping a dead resume costs the run but is visible; dropping a live one discards
// recoverable context, which is the failure class the fix exists to prevent.
//
// The layout, the encoding rule, the scoping, and the 200-character truncation boundary
// were each measured by running the CLI shipped with @anthropic-ai/claude-agent-sdk
// 0.3.201 (2026-07-21) — see the ASSUMPTIONS block in src/sdk-session.ts.

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { encodeCwd, sessionTranscriptResolvable } from "../src/sdk-session.js";

const SID = "11111111-2222-3333-4444-555555555555";
const CWD = "/data/runner/repo/issue-7";

let home: string;

beforeEach(() => {
  home = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sdksession-"));
});

afterEach(() => {
  fs.chmodSync(home, 0o700); // a test below drops permissions; restore so rm works
  fs.rmSync(home, { recursive: true, force: true });
});

/** Plant a transcript the way the CLI writes one: one dir per cwd, `<sid>.jsonl`. */
function plant(projectDir: string, sessionId: string): void {
  const dir = path.join(home, ".claude", "projects", projectDir);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
}

describe("encodeCwd — the CLI's project-dir naming", () => {
  it("replaces every non-alphanumeric character with a dash", () => {
    assert.equal(encodeCwd("/data/runner/repo/issue-7"), "-data-runner-repo-issue-7");
    // A leading `/-` collapses to `--`; dots and underscores are dashes too. Measured
    // against real dirs the CLI created, not inferred from the slash case alone.
    assert.equal(encodeCwd("/private/tmp/-Users-x/a.b_c"), "-private-tmp--Users-x-a-b-c");
  });
});

describe("sessionTranscriptResolvable (issue #105)", () => {
  it("finds a transcript in the dir the cwd encodes to", async () => {
    plant(encodeCwd(CWD), SID);
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), true);
  });

  it("reports absent when the HOME has no .claude/projects at all — the cross-worker case", async () => {
    // Exactly what a different worker sees: `agent-home/<runId>` never existed here.
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), false);
  });

  it("reports absent when the project dir exists but holds a DIFFERENT session", async () => {
    plant(encodeCwd(CWD), "99999999-8888-7777-6666-555555555555");
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), false);
  });

  it("reports absent for a transcript in ANOTHER project dir — the CLI would not find it either", async () => {
    // The whole point of scoping. A glob across project dirs answered "present" here,
    // which kept a resume the CLI fails on and left the run dying as before.
    plant(encodeCwd("/some/entirely/different/cwd"), SID);
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), false);
  });

  it("reports absent for a sibling dir that merely shares a prefix", async () => {
    plant(`${encodeCwd(CWD)}-child`, SID);
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), false);
  });

  it("distinguishes two cwds under one HOME (a same-machine cwd change breaks resume too)", async () => {
    const other = "/data/runner/repo/issue-8";
    plant(encodeCwd(CWD), SID);
    assert.equal(await sessionTranscriptResolvable(home, CWD, SID), true);
    assert.equal(await sessionTranscriptResolvable(home, other, SID), false);
  });

  it("FAILS OPEN on a session id that is not UUID-shaped (never joins it onto a path)", async () => {
    // Keeping the resume yields the CLI's loud, honest failure. Silently starting fresh
    // on an id we merely failed to parse would be the worse answer.
    for (const bogus of ["", "../../etc/passwd", "not-a-uuid", `${SID}/..`]) {
      assert.equal(await sessionTranscriptResolvable(home, CWD, bogus), true, bogus);
    }
  });

  it("FAILS OPEN past the CLI's 200-character verbatim boundary (we do not guess its hash)", async () => {
    // At 201+ encoded characters the CLI truncates to 200 and appends a base36 hash of
    // its own making. Reproducing that would risk a false "absent", i.e. discarding a
    // live session — so the CLI is left to answer instead.
    const long = "/" + "a".repeat(200);
    assert.equal(encodeCwd(long).length, 201);
    assert.equal(await sessionTranscriptResolvable(home, long, SID), true);
    // …and exactly at the boundary the encoding is still verbatim, so the check applies.
    const atLimit = "/" + "a".repeat(199);
    assert.equal(encodeCwd(atLimit).length, 200);
    assert.equal(await sessionTranscriptResolvable(home, atLimit, SID), false);
  });

  it("FAILS OPEN when the projects dir cannot be read (an error is not an answer)", async (t) => {
    if (process.getuid?.() === 0) return t.skip("root ignores directory permissions");
    plant(encodeCwd(CWD), SID);
    fs.chmodSync(home, 0o000); // EACCES on the access(), not ENOENT
    try {
      assert.equal(await sessionTranscriptResolvable(home, CWD, SID), true);
    } finally {
      fs.chmodSync(home, 0o700);
    }
  });
});
