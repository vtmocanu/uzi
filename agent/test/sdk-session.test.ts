// Issue #105: the resume preflight. These pin the FAIL-OPEN cut line, which is the
// whole safety argument — "the file is not there" (ENOENT/ENOTDIR) is an answer and
// drops the resume, anything else is a failure to look and must keep it, because a
// spurious "absent" silently discards a good session.
//
// The path layout asserted here (`$HOME/.claude/projects/<encoded-cwd>/<sid>.jsonl`)
// was verified against the real CLI shipped with @anthropic-ai/claude-agent-sdk
// 0.3.201, in both directions: an empty HOME makes `--resume` fail locally with
// "No conversation found with session ID: …" (exit 1, duration_api_ms 0 — it never
// reaches the API), and planting a transcript at exactly that path gets the same
// invocation past resolution and on to an auth failure.

import { describe, it, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { sessionTranscriptResolvable } from "../src/sdk-session.js";

const SID = "11111111-2222-3333-4444-555555555555";

let home: string;

beforeEach(() => {
  home = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-sdksession-"));
});

afterEach(() => {
  fs.chmodSync(home, 0o700); // a test below drops permissions; restore so rm works
  fs.rmSync(home, { recursive: true, force: true });
});

/** Plant a transcript the way the CLI writes one: one dir per cwd, `<sid>.jsonl`. */
function plant(encodedCwd: string, sessionId: string): void {
  const dir = path.join(home, ".claude", "projects", encodedCwd);
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, `${sessionId}.jsonl`), "{}\n");
}

describe("sessionTranscriptResolvable (issue #105)", () => {
  it("finds a transcript under its project dir", async () => {
    plant("-data-runner-repo-issue-7", SID);
    assert.equal(await sessionTranscriptResolvable(home, SID), true);
  });

  it("finds it regardless of WHICH project dir holds it (cwd encoding is not recomputed)", async () => {
    plant("-some-entirely-different-cwd", SID);
    assert.equal(await sessionTranscriptResolvable(home, SID), true);
  });

  it("reports absent when the HOME has no .claude/projects at all — the cross-worker case", async () => {
    // Exactly what a different worker sees: `agent-home/<runId>` never existed here.
    assert.equal(await sessionTranscriptResolvable(home, SID), false);
  });

  it("reports absent when the projects dir exists but holds a DIFFERENT session", async () => {
    plant("-data-runner-repo-issue-7", "99999999-8888-7777-6666-555555555555");
    assert.equal(await sessionTranscriptResolvable(home, SID), false);
  });

  it("ignores stray files next to the project dirs", async () => {
    const projects = path.join(home, ".claude", "projects");
    fs.mkdirSync(projects, { recursive: true });
    fs.writeFileSync(path.join(projects, `${SID}.jsonl`), "{}\n"); // not inside a project dir
    assert.equal(await sessionTranscriptResolvable(home, SID), false);
  });

  it("FAILS OPEN on a session id that is not UUID-shaped (never joins it onto a path)", async () => {
    // Keeping the resume yields the CLI's loud, honest failure. Silently starting
    // fresh on an id we merely failed to parse would be the worse answer.
    for (const bogus of ["", "../../etc/passwd", "not-a-uuid", `${SID}/..`]) {
      assert.equal(await sessionTranscriptResolvable(home, bogus), true, bogus);
    }
  });

  it("FAILS OPEN when the projects dir cannot be read (an error is not an answer)", async (t) => {
    if (process.getuid?.() === 0) return t.skip("root ignores directory permissions");
    const projects = path.join(home, ".claude", "projects");
    fs.mkdirSync(projects, { recursive: true });
    fs.chmodSync(home, 0o000); // EACCES on the readdir, not ENOENT
    try {
      assert.equal(await sessionTranscriptResolvable(home, SID), true);
    } finally {
      fs.chmodSync(home, 0o700);
    }
  });
});
