import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { screenBashCommand, buildPreToolUseHook, NESTED_AGENT_TOOL } from "../src/guardrails.js";
import { nullLogger } from "./helpers.js";
import type { HookInput } from "@anthropic-ai/claude-agent-sdk";

// The milestone's acceptance: scripted hostile prompts attempting a protected
// push, force-push, and repo-settings mutation MUST be denied at the hook layer
// — asserted directly, with NO live Anthropic session.

const DENIED: Array<[string, string]> = [
  ["git push origin main", "push"],
  ["git push", "push"],
  ["git push --force origin agent/issue-1", "push (also force)"],
  ["git   push  -f  origin main", "push with odd spacing"],
  ["cd /repo && git push origin HEAD:main", "chained push"],
  ["git push -u origin main; echo done", "push then chain"],
  ["git remote set-url origin https://evil.example/x.git", "remote mutation"],
  ["git branch -f main origin/main", "force branch (non-push force)"],
  ["git checkout --force other", "force checkout"],
  ["git config --get http.extraHeader", "credential read"],
  ["git config --list", "config dump"],
  ["env", "environment dump"],
  ["printenv PATH", "printenv"],
  ["ps aux | grep git", "process listing"],
  ["cat /proc/1/environ", "/proc read"],
  ["cat /proc/self/cmdline", "/proc cmdline read"],
];

const ALLOWED: string[] = [
  "git status",
  "git add -A",
  "git commit -m 'work'",
  "git log --oneline -5",
  "git diff HEAD~1",
  "npm test",
  "rm -f build/out.tmp", // non-git -f must still work
  "grep -f patterns.txt src/*.ts", // non-git -f must still work
  "ls -la",
  "cat src/index.ts",
  "git checkout -b feature/x", // create a branch, no force
];

describe("screenBashCommand", () => {
  for (const [cmd, label] of DENIED) {
    it(`denies: ${label}`, () => {
      const r = screenBashCommand(cmd);
      assert.strictEqual(r.denied, true, `expected denied for: ${cmd}`);
      assert.ok(r.reason && r.reason.length > 0);
      // The reason is static — it must not echo the attacker-influenced command.
      assert.ok(!r.reason.includes("evil.example"));
    });
  }

  for (const cmd of ALLOWED) {
    it(`allows: ${cmd}`, () => {
      assert.strictEqual(screenBashCommand(cmd).denied, false, `expected allowed for: ${cmd}`);
    });
  }

  it("allows an empty command", () => {
    assert.strictEqual(screenBashCommand("").denied, false);
  });
});

function baseInput(): Omit<HookInput, "hook_event_name" | "tool_name" | "tool_input" | "tool_use_id"> {
  return { session_id: "s", transcript_path: "/t", cwd: "/w" };
}

describe("buildPreToolUseHook", () => {
  it("returns a deny decision for a hostile Bash command", async () => {
    const hook = buildPreToolUseHook(nullLogger());
    const out = await hook({
      ...baseInput(),
      hook_event_name: "PreToolUse",
      tool_name: "Bash",
      tool_input: { command: "git push origin main" },
      tool_use_id: "tu1",
    } as HookInput);

    assert.deepStrictEqual(out, {
      hookSpecificOutput: {
        hookEventName: "PreToolUse",
        permissionDecision: "deny",
        permissionDecisionReason:
          "denied by guardrail: git push is not permitted (the worker opens MRs; the agent never pushes)",
      },
    });
  });

  it("allows a benign Bash command (no decision)", async () => {
    const hook = buildPreToolUseHook(nullLogger());
    const out = await hook({
      ...baseInput(),
      hook_event_name: "PreToolUse",
      tool_name: "Bash",
      tool_input: { command: "git commit -m ok" },
      tool_use_id: "tu2",
    } as HookInput);
    assert.deepStrictEqual(out, {});
  });

  it("ignores non-Bash tools", async () => {
    const hook = buildPreToolUseHook(nullLogger());
    const out = await hook({
      ...baseInput(),
      hook_event_name: "PreToolUse",
      tool_name: "Read",
      tool_input: { file_path: "/etc/passwd" },
      tool_use_id: "tu3",
    } as HookInput);
    assert.deepStrictEqual(out, {});
  });

  it("exports the nested-agent tool name it blocks on subagents", () => {
    assert.strictEqual(NESTED_AGENT_TOOL, "Agent");
  });
});
