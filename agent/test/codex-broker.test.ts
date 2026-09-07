import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexCallbackBroker,
  MAX_ID_BYTES,
  MAX_TOOL_NAME_BYTES,
  type CallbackResult,
  type CallbackRuntimeId,
  type ChildDelegationRequest,
  type ChildDelegationResult,
  type CodexCallbackBrokerOptions,
  type FileopRequest,
  type FileopResponse,
  type RunGrants,
  type SpawnCommandOptions,
  type SpawnCommandResult,
} from "../src/codex/broker.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";

// PRD #1171 (M3, milestone 2) — the worker callback broker. Every seam is a fake;
// the ExecutionRegistry is the REAL one (it is pure logic) so admission/replay/poison
// are exercised end to end. No process is ever spawned and no file is ever touched.

const WORKTREE = "/tmp/uzi-broker-worktree";

/** A spy over the shell-effect seam. */
class SpawnSpy {
  calls: { argv: readonly string[]; opts: SpawnCommandOptions }[] = [];
  constructor(private readonly result: SpawnCommandResult = { code: 0, stdout: "ok-out", stderr: "" }) {}
  seam = async (argv: readonly string[], opts: SpawnCommandOptions): Promise<SpawnCommandResult> => {
    this.calls.push({ argv, opts });
    return this.result;
  };
}

/** A spy over the fileop client, returning a scripted response. */
class FileopSpy {
  calls: FileopRequest[] = [];
  constructor(private readonly response: FileopResponse = { ok: true, size: 3 }) {}
  op = async (request: FileopRequest): Promise<FileopResponse> => {
    this.calls.push(request);
    return this.response;
  };
}

/** A spy over the delegation seam. */
class DelegateSpy {
  calls: ChildDelegationRequest[] = [];
  constructor(private readonly result: ChildDelegationResult = { ok: true, output: { child: "settled" } }) {}
  seam = async (request: ChildDelegationRequest): Promise<ChildDelegationResult> => {
    this.calls.push(request);
    return this.result;
  };
}

function grants(overrides: Partial<RunGrants> = {}): RunGrants {
  return {
    role: "coder",
    phase: "implement",
    allowedTools: new Set(["Bash", "apply_patch", "Read", "spawn_agent", "submit_plan", "signal_done"]),
    allowedSkills: new Set<string>(),
    isRoot: true,
    ...overrides,
  };
}

interface Harness {
  broker: CodexCallbackBroker;
  registry: ExecutionRegistry;
  spawn: SpawnSpy;
  fileop: FileopSpy;
  delegate: DelegateSpy;
}

function makeBroker(opts: Partial<CodexCallbackBrokerOptions> = {}): Harness {
  const registry = opts.registry ?? new ExecutionRegistry(newLocalExecutionEpoch(1));
  const spawn = new SpawnSpy();
  const fileop = (opts.fileop as FileopSpy | undefined) ?? new FileopSpy();
  const delegate = new DelegateSpy();
  const broker = new CodexCallbackBroker({
    registry,
    spawnCommand: opts.spawnCommand ?? spawn.seam,
    fileop,
    worktreePath: opts.worktreePath ?? WORKTREE,
    grants: opts.grants ?? grants(),
    delegate: opts.delegate ?? delegate.seam,
    toolHandlers: opts.toolHandlers,
    allowedRoles: opts.allowedRoles ?? new Set(["coder", "reviewer"]),
    screenPolicy: opts.screenPolicy,
  });
  return { broker, registry, spawn, fileop, delegate };
}

let seq = 0;
function rt(): CallbackRuntimeId {
  seq += 1;
  return { threadId: "t", turnId: "u", callId: `c-${seq}` };
}

function assertDenied(r: CallbackResult, code: string): asserts r is { ok: false; code: string; message: string } {
  if (r.ok) assert.fail(`expected a denial with code ${code}, got ok`);
  assert.equal(r.code, code, `expected deny code ${code}, got ${r.code}: ${r.message}`);
}

describe("CodexCallbackBroker: ingestion bounds", () => {
  it("exposes the exact byte caps", () => {
    assert.equal(MAX_ID_BYTES, 512);
    assert.equal(MAX_TOOL_NAME_BYTES, 256);
  });

  it("denies an over-cap id WITHOUT poisoning or admitting", async () => {
    const h = makeBroker();
    const bigId = "a".repeat(MAX_ID_BYTES + 1);
    const r = await h.broker.handleToolCall(
      { threadId: bigId, turnId: "u", callId: "c" },
      "Bash",
      { command: "echo hi" },
      "root",
    );
    assertDenied(r, "ingestion_rejected");
    assert.equal(h.spawn.calls.length, 0);
    assert.equal(h.registry.inFlightCallbackCount(), 0);
    assert.equal(h.registry.isPoisoned(), false);
  });

  it("denies a non-string / empty id", async () => {
    const h = makeBroker();
    const r1 = await h.broker.handleToolCall({ threadId: "", turnId: "u", callId: "c" }, "Bash", { command: "echo" }, "root");
    assertDenied(r1, "ingestion_rejected");
    const r2 = await h.broker.handleToolCall(
      { threadId: "t", turnId: "u", callId: 5 as unknown as string },
      "Bash",
      { command: "echo" },
      "root",
    );
    assertDenied(r2, "ingestion_rejected");
  });

  it("denies an over-cap tool name", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "B".repeat(MAX_TOOL_NAME_BYTES + 1), {}, "root");
    assertDenied(r, "ingestion_rejected");
  });
});

describe("CodexCallbackBroker: shell (screenBashCommand integration)", () => {
  it("runs an allowed command through the spawn seam exactly once", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo hello" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { code: 0, stdout: "ok-out", stderr: "" });
    assert.equal(h.spawn.calls.length, 1);
    assert.deepEqual(h.spawn.calls[0]!.argv, ["/bin/sh", "-c", "echo hello"]);
    assert.equal(h.spawn.calls[0]!.opts.cwd, WORKTREE);
    // The fresh reservation is now settled.
    assert.equal(h.registry.inFlightCallbackCount(), 0);
  });

  it("denies a command the guardrail screens (reading a worker secret) and never spawns", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "cat /run/secrets/worker_token" }, "root");
    assertDenied(r, "shell_denied");
    assert.equal(h.spawn.calls.length, 0);
  });

  it("denies git push (guardrail) without spawning", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "git push origin main" }, "root");
    assertDenied(r, "shell_denied");
    assert.equal(h.spawn.calls.length, 0);
  });

  it("denies a Bash with no command string", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { notcommand: 1 }, "root");
    assertDenied(r, "bad_args");
    assert.equal(h.spawn.calls.length, 0);
  });

  it("denies a cwd outside the worktree", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo x", cwd: "../elsewhere" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.spawn.calls.length, 0);
  });
});

describe("CodexCallbackBroker: file effects through the fileop client", () => {
  it("routes an apply_patch (Write alias) to a fileop write with a worktree-relative path", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "hello" }, "root");
    assert.equal(r.ok, true);
    assert.equal(h.fileop.calls.length, 1);
    const req = h.fileop.calls[0]!;
    assert.equal(req.op, "write");
    assert.equal(req.path, "src/x.ts");
    assert.equal(req.data, Buffer.from("hello", "utf8").toString("base64"));
  });

  it("routes a Read to a fileop read", async () => {
    const fileop = new FileopSpy({ ok: true, size: 5, data: Buffer.from("hello").toString("base64") });
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(rt(), "Read", { path: "src/x.ts" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { size: 5, contentBase64: Buffer.from("hello").toString("base64") });
    assert.equal(fileop.calls.length, 1);
    assert.equal(fileop.calls[0]!.op, "read");
  });

  it("denies a .git path with the jail and never issues a fileop", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Write", { path: ".git/config", content: "x" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("denies a .GIT path case-insensitively (defense in depth) and never issues a fileop", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "sub/.GIT/config", content: "x" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("denies an outside-worktree path and never issues a fileop", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "../../etc/passwd", content: "x" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("denies a secret path (extraSecretPaths) and never issues a fileop", async () => {
    const h = makeBroker({ screenPolicy: { extraSecretPaths: [`${WORKTREE}/creds.env`] } });
    const r = await h.broker.handleToolCall(rt(), "Read", { path: "creds.env" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("maps a fileop error code to a neutral denial", async () => {
    const fileop = new FileopSpy({ ok: false, code: "E_ESCAPE" });
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "x" }, "root");
    assertDenied(r, "fileop_denied");
    assert.match(r.message, /E_ESCAPE/);
    assert.equal(fileop.calls.length, 1);
  });

  it("denies a file write during the plan phase (no fileop)", async () => {
    const h = makeBroker({ grants: grants({ phase: "plan" }) });
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "x" }, "root");
    assertDenied(r, "write_denied_in_plan");
    assert.equal(h.fileop.calls.length, 0);
  });
});

describe("CodexCallbackBroker: authority is bound to the grant, not the args", () => {
  it("denies an unknown tool with a diagnostic naming it", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "FabricateThings", { plausible: true }, "root");
    assertDenied(r, "unknown_tool");
    assert.match(r.message, /FabricateThings/);
  });

  it("denies a known tool absent from allowedTools even with plausible args", async () => {
    // A grant WITHOUT Bash: a perfectly valid command must not widen authority.
    const h = makeBroker({ grants: grants({ allowedTools: new Set(["apply_patch"]) }) });
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo hi" }, "root");
    assertDenied(r, "denied_tool");
    assert.equal(h.spawn.calls.length, 0);
  });
});

describe("CodexCallbackBroker: root-only signals", () => {
  it("lets the root latch a signal and returns the parsed signal", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "submit_plan", { plan_md: "the plan" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { plan: "the plan" });
  });

  it("denies a signal from a child origin even when isRoot and allowedTools would pass", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "submit_plan", { plan_md: "the plan" }, "child");
    assertDenied(r, "signal_root_only");
  });

  it("denies a signal from an unknown origin", async () => {
    const h = makeBroker({ grants: grants({ isRoot: false }) });
    const r = await h.broker.handleToolCall(rt(), "signal_done", { summary: "done" }, "unknown");
    assertDenied(r, "signal_root_only");
  });
});

describe("CodexCallbackBroker: delegation", () => {
  it("awaits a root delegation to a known role exactly once", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "reviewer", prompt: "look" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { child: "settled" });
    assert.equal(h.delegate.calls.length, 1);
    assert.equal(h.delegate.calls[0]!.role, "reviewer");
    assert.equal(h.delegate.calls[0]!.tool, "spawn_agent");
  });

  it("denies NESTED delegation (a child origin delegating) and never runs a child", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "reviewer" }, "child");
    assertDenied(r, "delegate_root_only");
    assert.equal(h.delegate.calls.length, 0);
  });

  it("denies delegation to an unknown role", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "intruder" }, "root");
    assertDenied(r, "unknown_role");
    assert.equal(h.delegate.calls.length, 0);
  });
});

describe("CodexCallbackBroker: MCP / skills pass-through", () => {
  it("routes an allowed MCP tool to its injected handler", async () => {
    const handlers = new Map([["mcp__memory__store", async () => ({ stored: true })]]);
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["mcp__memory__store"]) }),
      toolHandlers: handlers,
    });
    const r = await h.broker.handleToolCall(rt(), "mcp__memory__store", { note: "x" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { stored: true });
  });

  it("denies an allowed MCP tool with no wired handler", async () => {
    const h = makeBroker({ grants: grants({ allowedTools: new Set(["mcp__forge__get_issue"]) }) });
    const r = await h.broker.handleToolCall(rt(), "mcp__forge__get_issue", { iid: 1 }, "root");
    assertDenied(r, "denied_tool");
  });

  it("denies a skill not in allowedSkills", async () => {
    const handlers = new Map([["Skill", async () => ({ ran: true })]]);
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]) }),
      toolHandlers: handlers,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: "forbidden" }, "root");
    assertDenied(r, "denied_skill");
  });

  it("routes a granted skill to its handler", async () => {
    const handlers = new Map([["Skill", async () => ({ ran: true })]]);
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]) }),
      toolHandlers: handlers,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: "prd-lifecycle" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { ran: true });
  });
});

describe("CodexCallbackBroker: admission (registry idempotency + poison)", () => {
  it("replays a settled callback WITHOUT a second effect", async () => {
    const h = makeBroker();
    const id = rt();
    const first = await h.broker.handleToolCall(id, "Bash", { command: "echo once" }, "root");
    assert.equal(first.ok, true);
    assert.equal(h.spawn.calls.length, 1);

    // Identical tuple + identical payload: idempotent replay, NO re-execution.
    const second = await h.broker.handleToolCall(id, "Bash", { command: "echo once" }, "root");
    assert.equal(second.ok, true);
    if (second.ok) assert.deepEqual(second.output, { replay: true });
    assert.equal(h.spawn.calls.length, 1);
  });

  it("denies a changed-payload reuse of the same tuple AND poisons the epoch", async () => {
    const h = makeBroker();
    const id = rt();
    const first = await h.broker.handleToolCall(id, "Bash", { command: "echo A" }, "root");
    assert.equal(first.ok, true);
    assert.equal(h.spawn.calls.length, 1);

    // Same (thread,turn,call), DIFFERENT payload: replay/forgery signal.
    const second = await h.broker.handleToolCall(id, "Bash", { command: "echo B" }, "root");
    assertDenied(second, "changed_reuse");
    assert.equal(h.registry.isPoisoned(), true);
    assert.equal(h.spawn.calls.length, 1);
  });

  it("denies once admission has closed", async () => {
    const h = makeBroker();
    await h.registry.quiesceChildren(1000); // open -> closed, admission shut
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo x" }, "root");
    assertDenied(r, "admission_closed");
    assert.equal(h.spawn.calls.length, 0);
  });
});
