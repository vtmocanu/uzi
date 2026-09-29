import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { assembleAgents } from "../src/agents.js";
import {
  buildLeadSystemPrompt,
  CLAUDE_LONG_COMMAND_APPEND,
} from "../src/prompt.js";
import type { AgentTemplate } from "../src/protocol.js";

// Judge recommendations: gates backgrounded and then lost at the turn boundary, and
// `gh` / bare `tsc` calls that cannot succeed on a worker. The harness-neutral rule must
// reach the lead and every subagent on both harnesses; the Claude Bash recipe must reach
// only the Claude lead and Claude subagents (the Codex Bash tool has no timeout argument).
// The Codex subagent half lives in codex-render.test.ts beside its parity test.
const LIFETIME = "is stopped and its result is lost";
const TOOLBOX = "no forge CLI (`gh`, `glab`, `tea`)";

const tester: AgentTemplate = {
  name: "tester",
  description: "runs the gate",
  prompt_body: "Run the gate.",
  tools: ["Read", "Bash"],
};

describe("command-lifetime and worker-toolbox rules reach every agent", () => {
  it("the Claude lead gets the neutral rules and the Claude recipe", () => {
    const append = buildLeadSystemPrompt("LEAD BODY", { kind: "issue", harness: "claude" }).append;
    assert.ok(append.includes(LIFETIME));
    assert.ok(append.includes(TOOLBOX));
    assert.ok(append.includes(CLAUDE_LONG_COMMAND_APPEND));
  });

  it("a lead built without the Claude harness (the Codex executor) gets no Claude recipe", () => {
    const append = buildLeadSystemPrompt("LEAD BODY", { kind: "issue" }).append;
    assert.ok(append.includes(LIFETIME));
    assert.ok(append.includes(TOOLBOX));
    assert.ok(!append.includes("run_in_background"));
  });

  it("a Claude subagent gets the neutral rules and the Claude recipe", () => {
    const def = assembleAgents([tester]).subagents.tester;
    assert.ok(def, "tester assembled");
    assert.ok(def.prompt.includes(LIFETIME));
    assert.ok(def.prompt.includes(TOOLBOX));
    assert.ok(def.prompt.includes(CLAUDE_LONG_COMMAND_APPEND));
  });
});

// The recipe's two commands are what an agent will paste into two separate Bash calls;
// prove a failing command still records its status and a fresh shell reads it back.
describe("CLAUDE_LONG_COMMAND_APPEND start and poll commands", () => {
  const flat = CLAUDE_LONG_COMMAND_APPEND.replace(/\n/g, " ");
  const start = /`(rc=0; <command> .*?)`/.exec(flat)?.[1];
  const poll = /`(for i in \$\(seq \d+\); do .*?)`/.exec(flat)?.[1];
  const fill = (cmd: string, command: string) => cmd.replaceAll("<name>", "gate-t").replace("<command>", command);

  it("names a bounded poll that stays under the 600000 ms tool ceiling", () => {
    assert.ok(start, "start command present");
    assert.ok(poll, "poll command present");
    const m = /seq (\d+)\); .* sleep (\d+);/.exec(poll);
    assert.ok(m, "bounded seq + sleep");
    assert.ok(Number(m[1]) * Number(m[2]) < 600, "total sleep stays under 600 s");
    // Without an explicit timeout the poll call itself is backgrounded at the 2-minute default.
    assert.match(flat, /foreground calls, each with `timeout` 600000/);
  });

  it("a failing command under set -e still writes its status, and a fresh shell polls it back", () => {
    assert.ok(start && poll);
    const dir = mkdtempSync(join(tmpdir(), "cmdlife-"));
    try {
      execFileSync("mkdir", ["-p", join(dir, ".uzi", "scratch")]);
      // A failing gate whose output even contains a fake marker.
      execFileSync("bash", ["-e", "-c", fill(start, "sh -c 'echo EXIT=0; echo boom; exit 3'")], { cwd: dir });
      const started = Date.now();
      // A separate shell with a scrubbed env: nothing carries from the start call but the files.
      const out = execFileSync("bash", ["-c", fill(poll, "")], { cwd: dir, env: { PATH: process.env.PATH ?? "" }, encoding: "utf8" });
      assert.ok(Date.now() - started < 5_000, "did not sleep once the status exists");
      assert.equal(out.split("\n")[0], "3", "the real exit status, not the fake marker in the output");
      assert.match(out, /boom/);
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
