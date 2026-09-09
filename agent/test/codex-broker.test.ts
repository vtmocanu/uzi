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
import { buildCodexToolHandlers } from "../src/codex/codex-executor.js";
import { forgeToolNames } from "../src/forge-tools.js";
import { memoryToolNames } from "../src/memory-tools.js";
import { reportIncidentalIssueToolName } from "../src/findings-tools.js";
import type { Logger } from "../src/log.js";
import type { WorkerClient } from "../src/client.js";
import type { EmittedMessage } from "../src/executor.js";
import type { ClaimSkill } from "../src/protocol.js";

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
  private readonly responses: readonly FileopResponse[];
  private responseIndex = 0;

  constructor(response: FileopResponse | FileopResponse[] = { ok: true, size: 3 }) {
    this.responses = Array.isArray(response) ? response : [response];
  }
  op = async (request: FileopRequest): Promise<FileopResponse> => {
    this.calls.push(request);
    const index = Math.min(this.responseIndex, this.responses.length - 1);
    this.responseIndex += 1;
    return this.responses[index]!;
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

  it("denies an absolute cwd outside the worktree without spawning", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "pwd", cwd: "/etc" }, "root");
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
    if (r.ok) {
      assert.deepEqual(r.output, {
        size: 5,
        contentBase64: Buffer.from("hello").toString("base64"),
        truncated: false,
      });
    }
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

  it("never reflects a short unknown fileop code into model-visible output", async () => {
    const rawCode = "E_FORGED";
    const fileop = new FileopSpy({ ok: false, code: rawCode });
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "x" }, "root");
    assertDenied(r, "fileop_denied");
    assert.equal(r.message, "file operation denied (E_IO)");
    assert.equal(r.message.includes(rawCode), false);
  });

  it("never reflects an oversized fileop code into model-visible output", async () => {
    const rawCode = `E_IO_${"x".repeat(4096)}`;
    const fileop = new FileopSpy({ ok: false, code: rawCode });
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "x" }, "root");
    assertDenied(r, "fileop_denied");
    assert.equal(r.message, "file operation denied (E_IO)");
    assert.equal(r.message.includes(rawCode), false);
  });

  it("denies a file write during the plan phase (no fileop)", async () => {
    const h = makeBroker({ grants: grants({ phase: "plan" }) });
    const r = await h.broker.handleToolCall(rt(), "Write", { path: "src/x.ts", content: "x" }, "root");
    assertDenied(r, "write_denied_in_plan");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("routes an Edit (old_string/new_string) to the atomic staged apply op", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(
      rt(),
      "Edit",
      { path: "src/x.ts", old_string: "foo", new_string: "bar" },
      "root",
    );
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { applied: 1 });
    assert.equal(h.fileop.calls.length, 1);
    const req = h.fileop.calls[0]!;
    assert.equal(req.op, "apply");
    assert.equal(req.path, "src/x.ts");
    assert.equal(req.old, Buffer.from("foo", "utf8").toString("base64"));
    assert.equal(req.data, Buffer.from("bar", "utf8").toString("base64"));
  });

  it("allows an Edit whose new_string is empty (a deletion)", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(
      rt(),
      "Edit",
      { path: "src/x.ts", old_string: "delete me", new_string: "" },
      "root",
    );
    assert.equal(r.ok, true);
    assert.equal(h.fileop.calls.length, 1);
    assert.equal(h.fileop.calls[0]!.data, "");
  });

  it("routes a MultiEdit to an ORDERED sequence of apply ops", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(
      rt(),
      "MultiEdit",
      {
        path: "src/x.ts",
        edits: [
          { old_string: "a", new_string: "1" },
          { old_string: "b", new_string: "2" },
        ],
      },
      "root",
    );
    assert.equal(r.ok, true);
    if (r.ok) assert.deepEqual(r.output, { applied: 2 });
    assert.equal(h.fileop.calls.length, 2);
    assert.deepEqual(h.fileop.calls.map((c) => c.op), ["apply", "apply"]);
    assert.equal(h.fileop.calls[0]!.old, Buffer.from("a", "utf8").toString("base64"));
    assert.equal(h.fileop.calls[1]!.old, Buffer.from("b", "utf8").toString("base64"));
  });

  it("stops a MultiEdit at the FIRST failing apply and reports the denial", async () => {
    // The fileop spy fails every op; the batch must stop after the first, not apply the rest.
    const fileop = new FileopSpy({ ok: false, code: "E_NO_MATCH" });
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(
      rt(),
      "MultiEdit",
      { path: "src/x.ts", edits: [{ old_string: "a", new_string: "1" }, { old_string: "b", new_string: "2" }] },
      "root",
    );
    assertDenied(r, "fileop_denied");
    assert.match(r.message, /E_NO_MATCH/);
    assert.equal(fileop.calls.length, 1);
  });

  it("reports a successful first MultiEdit when the second apply fails", async () => {
    const fileop = new FileopSpy([{ ok: true, size: 3 }, { ok: false, code: "E_AMBIGUOUS" }]);
    const h = makeBroker({ fileop });
    const r = await h.broker.handleToolCall(
      rt(),
      "MultiEdit",
      { path: "src/x.ts", edits: [{ old_string: "a", new_string: "1" }, { old_string: "b", new_string: "2" }] },
      "root",
    );
    assertDenied(r, "fileop_denied");
    assert.equal(r.message, "file operation denied (E_AMBIGUOUS); 1 edit applied before failure");
    assert.equal(fileop.calls.length, 2);
  });

  it("maps the apply no-match / ambiguous codes to a neutral denial", async () => {
    for (const code of ["E_NO_MATCH", "E_AMBIGUOUS"]) {
      const fileop = new FileopSpy({ ok: false, code });
      const h = makeBroker({ fileop });
      const r = await h.broker.handleToolCall(
        rt(),
        "Edit",
        { path: "src/x.ts", old_string: "foo", new_string: "bar" },
        "root",
      );
      assertDenied(r, "fileop_denied");
      assert.match(r.message, new RegExp(code));
    }
  });

  it("denies an apply_patch/Edit with neither content nor an old_string/new_string pair", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Edit", { path: "src/x.ts", new_string: "bar" }, "root");
    assertDenied(r, "bad_args");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("denies a MultiEdit whose edits array is empty or malformed (fail-closed)", async () => {
    const h1 = makeBroker();
    const empty = await h1.broker.handleToolCall(rt(), "MultiEdit", { path: "src/x.ts", edits: [] }, "root");
    assertDenied(empty, "bad_args");
    assert.equal(h1.fileop.calls.length, 0);

    const h2 = makeBroker();
    const malformed = await h2.broker.handleToolCall(
      rt(),
      "MultiEdit",
      { path: "src/x.ts", edits: [{ old_string: "a", new_string: "1" }, { new_string: "no-old" }] },
      "root",
    );
    assertDenied(malformed, "bad_args");
    // Not even the first, well-formed edit runs: the whole batch fails closed on parse.
    assert.equal(h2.fileop.calls.length, 0);
  });

  it("denies an Edit's .git path with the jail and never issues a fileop", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "Edit", { path: ".git/config", old_string: "a", new_string: "b" }, "root");
    assertDenied(r, "path_denied");
    assert.equal(h.fileop.calls.length, 0);
  });

  it("denies an Edit during the plan phase (no fileop)", async () => {
    const h = makeBroker({ grants: grants({ phase: "plan" }) });
    const r = await h.broker.handleToolCall(rt(), "Edit", { path: "src/x.ts", old_string: "a", new_string: "b" }, "root");
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

  it("denies a signal callback whose payload parses to no signal", async () => {
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["report_progress"]) }),
    });
    const r = await h.broker.handleToolCall(rt(), "report_progress", {}, "root");
    assertDenied(r, "invalid_signal");
    assert.equal(h.registry.inFlightCallbackCount(), 0, "the denied callback still settles");
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

  it("bounds and sanitizes a delegated child failure", async () => {
    const delegate = new DelegateSpy({
      ok: false,
      code: "attacker_code",
      message: `failed\u001b[2J\n${"x".repeat(500)}`,
    });
    const h = makeBroker({ delegate: delegate.seam });
    const r = await h.broker.handleToolCall(
      rt(),
      "spawn_agent",
      { subagent_type: "reviewer" },
      "root",
    );
    assertDenied(r, "child_failed");
    assert.ok(!hasRawControlChars(r.message), `message carried a control char: ${JSON.stringify(r.message)}`);
    assert.ok(r.message.length <= 121, "the child message is bounded by the diagnostic cap plus ellipsis");
    assert.equal(delegate.calls.length, 1);
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

const SHELL_OUTPUT_CAP_BYTES = 64 * 1024;
const TRUNCATED_MARKER = "…[truncated]";

describe("CodexCallbackBroker: large shell output is bounded in O(n) (no O(n^2) hang)", () => {
  it("bounds 512 KiB of stdout FAST and marks it truncated", async () => {
    // 512 KiB of single-byte ASCII. On the OLD per-char shrink loop this took ~71s
    // (recomputing Buffer.byteLength over the whole shrinking string every iteration),
    // which blows past both the time bound below and, on 1 MiB, the 120s test timeout.
    const bigStdout = "x".repeat(512 * 1024);
    const h = makeBroker({
      spawnCommand: async (): Promise<SpawnCommandResult> => ({ code: 0, stdout: bigStdout, stderr: "" }),
    });

    const start = Date.now();
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo big" }, "root");
    const elapsedMs = Date.now() - start;

    assert.equal(r.ok, true);
    if (r.ok) {
      const out = r.output as { code: number; stdout: string; stderr: string };
      // The truncation marker is present...
      assert.ok(out.stdout.endsWith(TRUNCATED_MARKER), "expected the truncated marker");
      // ...and the pre-marker body is byte-bounded to the cap (single-byte chars, so
      // exactly the cap here). This alone fails the old code, which never returns.
      const body = out.stdout.slice(0, out.stdout.length - TRUNCATED_MARKER.length);
      assert.ok(
        Buffer.byteLength(body, "utf8") <= SHELL_OUTPUT_CAP_BYTES,
        `body ${Buffer.byteLength(body, "utf8")} exceeds cap ${SHELL_OUTPUT_CAP_BYTES}`,
      );
    }
    // O(n) completes in milliseconds; the old O(n^2) path takes tens of seconds. A
    // 5s ceiling cleanly separates the two without flaking under CI contention.
    assert.ok(elapsedMs < 5000, `boundedString was slow (${elapsedMs}ms) — O(n^2) regression?`);
  });

  it("leaves at-cap output unmarked and byte-exact", async () => {
    // Exactly the cap: no truncation, returned verbatim, no marker.
    const exact = "y".repeat(SHELL_OUTPUT_CAP_BYTES);
    const h = makeBroker({
      spawnCommand: async (): Promise<SpawnCommandResult> => ({ code: 0, stdout: exact, stderr: "" }),
    });
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo exact" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) {
      const out = r.output as { stdout: string };
      assert.equal(out.stdout, exact);
      assert.ok(!out.stdout.includes(TRUNCATED_MARKER));
    }
  });
});

describe("CodexCallbackBroker: file read forwards the helper's base64 verbatim + propagates truncated", () => {
  it("forwards a large body (> any broker cap) that round-trips, truncated=false", async () => {
    // 200 KiB of raw bytes -> ~266 KiB of base64, larger than the old 64 KiB broker
    // cap. The old code ran boundedString on the ENCODED text, cutting it at a raw-byte
    // boundary and appending a marker -> non-decodable garbage. Forwarding verbatim
    // must round-trip exactly.
    const raw = Buffer.alloc(200 * 1024, 0x41);
    const b64 = raw.toString("base64");
    const fileop = new FileopSpy({ ok: true, size: raw.length, data: b64, truncated: false });
    const h = makeBroker({ fileop });

    const r = await h.broker.handleToolCall(rt(), "Read", { path: "big.bin" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) {
      const out = r.output as { size?: number; contentBase64?: string; truncated?: boolean };
      assert.equal(out.contentBase64, b64, "base64 must be forwarded verbatim");
      assert.ok(out.contentBase64 !== undefined);
      // Round-trips cleanly and equals the original bytes (fails on the old truncation).
      assert.ok(Buffer.from(out.contentBase64!, "base64").equals(raw), "base64 must round-trip");
      assert.equal(out.truncated, false);
    }
  });

  it("propagates truncated=true from the helper while still round-tripping", async () => {
    const raw = Buffer.alloc(200 * 1024, 0x42);
    const b64 = raw.toString("base64");
    const fileop = new FileopSpy({ ok: true, size: raw.length, data: b64, truncated: true });
    const h = makeBroker({ fileop });

    const r = await h.broker.handleToolCall(rt(), "Read", { path: "big.bin" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) {
      const out = r.output as { contentBase64?: string; truncated?: boolean };
      assert.ok(out.contentBase64 !== undefined);
      assert.ok(Buffer.from(out.contentBase64!, "base64").equals(raw), "base64 must round-trip");
      assert.equal(out.truncated, true);
    }
  });
});

/** True if any code point in `s` is a C0/C1 control or DEL — a raw byte that could
 *  forge or terminal-rewrite a downstream log/report line. Scans by code point (no
 *  regex, which would trip oxlint's no-control-regex on the test file itself). */
function hasRawControlChars(s: string): boolean {
  for (const ch of s) {
    const cp = ch.codePointAt(0) ?? 0;
    if (cp <= 0x1f || cp === 0x7f || (cp >= 0x80 && cp <= 0x9f)) return true;
  }
  return false;
}

describe("CodexCallbackBroker: untrusted identifiers are sanitized in denial messages", () => {
  // ESC + a CSI "clear screen" + LF + CR: the classic terminal-injection payload.
  const CONTROL_PROBE = "evil\u001b[2Jtool\nrole\r";

  it("strips control chars from an unknown tool name while keeping the stable code", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), CONTROL_PROBE, {}, "root");
    assertDenied(r, "unknown_tool"); // stable machine code unchanged
    assert.ok(!hasRawControlChars(r.message), `message carried a control char: ${JSON.stringify(r.message)}`);
    // The visible, safe portion of the name is preserved.
    assert.match(r.message, /evil/);
  });

  it("strips control chars from an unknown delegation role while keeping the stable code", async () => {
    const h = makeBroker();
    const r = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: CONTROL_PROBE }, "root");
    assertDenied(r, "unknown_role");
    assert.ok(!hasRawControlChars(r.message), `message carried a control char: ${JSON.stringify(r.message)}`);
  });

  it("strips control chars from a denied skill name while keeping the stable code", async () => {
    const handlers = new Map([["Skill", async () => ({ ran: true })]]);
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]) }),
      toolHandlers: handlers,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: CONTROL_PROBE }, "root");
    assertDenied(r, "denied_skill");
    assert.ok(!hasRawControlChars(r.message), `message carried a control char: ${JSON.stringify(r.message)}`);
  });
});

describe("CodexCallbackBroker: root authority requires BOTH origin=root and grants.isRoot", () => {
  it("denies a signal when origin is root but grants.isRoot is false", async () => {
    const h = makeBroker({ grants: grants({ isRoot: false }) });
    const r = await h.broker.handleToolCall(rt(), "submit_plan", { plan_md: "p" }, "root");
    assertDenied(r, "signal_root_only");
  });

  it("denies delegation when origin is root but grants.isRoot is false, never running a child", async () => {
    const h = makeBroker({ grants: grants({ isRoot: false }) });
    const r = await h.broker.handleToolCall(rt(), "spawn_agent", { subagent_type: "reviewer" }, "root");
    assertDenied(r, "delegate_root_only");
    assert.equal(h.delegate.calls.length, 0);
  });
});

describe("CodexCallbackBroker: a throwing seam denies as broker_error with the reservation settled", () => {
  it("catches a throwing spawn seam, denies broker_error, and settles (no in-flight, no poison)", async () => {
    const h = makeBroker({
      spawnCommand: async (): Promise<SpawnCommandResult> => {
        throw new Error("seam blew up");
      },
    });
    const r = await h.broker.handleToolCall(rt(), "Bash", { command: "echo hi" }, "root");
    assertDenied(r, "broker_error");
    // The admitted reservation was settled, so nothing is left in flight and the
    // epoch is not poisoned (a clean quiesce would follow).
    assert.equal(h.registry.inFlightCallbackCount(), 0);
    assert.equal(h.registry.isPoisoned(), false);
  });
});

// PRD #1171 M3 (item 5) — the REAL executor-built tool-handler map wired through the
// broker. Not a hand-rolled `new Map([...])` (that is the generic routing tested above);
// this proves the map buildCodexToolHandlers produces resolves forge/memory/findings and
// the Codex-specific Skill delivery, and that a subagent never reaches the memory handler.
describe("CodexCallbackBroker: production tool-handler map (buildCodexToolHandlers)", () => {
  const NOOP_LOG: Logger = { debug() {}, info() {}, warn() {}, error() {}, addSecret() {}, removeSecret() {}, child() { return NOOP_LOG; } };
  const FORGE_GET_ISSUE = forgeToolNames()[0]!; // mcp__forge__get_issue
  const MEMORY_TOOL = memoryToolNames()[0]!;
  const FINDINGS_TOOL = reportIncidentalIssueToolName();

  /** A minimal WorkerClient with just the reads/writes the wired handlers call, recording
   *  what each saw so a test can prove the callback reached the real handler. */
  function toolClient(): { client: WorkerClient; forgeIssue: number[]; memory: string[]; finding: string[] } {
    const forgeIssue: number[] = [];
    const memory: string[] = [];
    const finding: string[] = [];
    const client = {
      async getForgeIssue(_runId: string, iid: number) { forgeIssue.push(iid); return { iid, title: "FORGE_ISSUE_CANARY" }; },
      async saveMemory(_runId: string, body: { title: string }) { memory.push(body.title); return { id: "mem-canary", title: body.title }; },
      async reportFinding(_runId: string, _body: unknown) { finding.push("find-canary"); return "find-canary"; },
    } as unknown as WorkerClient;
    return { client, forgeIssue, memory, finding };
  }

  function toolMap(skills: ClaimSkill[] = [{ name: "prd-lifecycle", description: "PRD lifecycle skill", body: "SKILL_BODY_CANARY" }]): {
    map: ReturnType<typeof buildCodexToolHandlers>;
    emitted: EmittedMessage[];
    calls: ReturnType<typeof toolClient>;
  } {
    const emitted: EmittedMessage[] = [];
    const calls = toolClient();
    const map = buildCodexToolHandlers({ client: calls.client, runId: "run-tools", log: NOOP_LOG, emit: (m) => emitted.push(m), skills });
    return { map, emitted, calls };
  }

  it("routes a ROOT forge callback to the real handler (ok, not denied_tool)", async () => {
    const { map, calls } = toolMap();
    const h = makeBroker({ grants: grants({ allowedTools: new Set([FORGE_GET_ISSUE]), isRoot: true }), toolHandlers: map });
    const r = await h.broker.handleToolCall(rt(), FORGE_GET_ISSUE, { iid: 9 }, "root");
    assert.equal(r.ok, true);
    assert.deepEqual(calls.forgeIssue, [9]);
    if (r.ok) assert.ok(JSON.stringify(r.output).includes("FORGE_ISSUE_CANARY"), "the forge evidence rode the callback output");
  });

  it("routes a ROOT memory callback to the real handler (ok, not denied_tool)", async () => {
    const { map, calls } = toolMap();
    const h = makeBroker({ grants: grants({ allowedTools: new Set([MEMORY_TOOL]), isRoot: true }), toolHandlers: map });
    const r = await h.broker.handleToolCall(rt(), MEMORY_TOOL, { title: "a durable fact", body: "the mechanism" }, "root");
    assert.equal(r.ok, true);
    assert.deepEqual(calls.memory, ["a durable fact"]);
  });

  it("routes a ROOT findings callback to the real handler and emits the finding card", async () => {
    const { map, emitted, calls } = toolMap();
    const h = makeBroker({ grants: grants({ allowedTools: new Set([FINDINGS_TOOL]), isRoot: true }), toolHandlers: map });
    const r = await h.broker.handleToolCall(rt(), FINDINGS_TOOL, { title: "leak", description: "d", location: "a/b.ts#f" }, "root");
    assert.equal(r.ok, true);
    assert.equal(calls.finding.length, 1);
    assert.ok(emitted.some((m) => m.kind === "finding"), "the finding card was emitted to the stream");
  });

  it("DENIES a memory callback to a subagent (render strips memory; the allowedTools gate denies denied_tool)", async () => {
    const { map, calls } = toolMap();
    // A subagent's grants never carry the memory tool (isSubagentForbidden), modelled here.
    const h = makeBroker({ grants: grants({ allowedTools: new Set(["Read"]), isRoot: false }), toolHandlers: map });
    const r = await h.broker.handleToolCall(rt(), MEMORY_TOOL, { title: "t", body: "b" }, "child");
    assertDenied(r, "denied_tool");
    assert.deepEqual(calls.memory, [], "the memory handler was never reached");
  });

  it("delivers a GRANTED skill's content through the Skill handler (ok, not denied)", async () => {
    const { map } = toolMap();
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]), isRoot: true }),
      toolHandlers: map,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: "prd-lifecycle" }, "root");
    assert.equal(r.ok, true);
    if (r.ok) assert.ok(JSON.stringify(r.output).includes("SKILL_BODY_CANARY"), "the granted skill's body rode the callback output");
  });

  it("still DENIES an ungranted skill at the broker's allowedSkills gate (denied_skill, handler never reached)", async () => {
    const { map } = toolMap();
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]), isRoot: true }),
      toolHandlers: map,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: "not-granted" }, "root");
    assertDenied(r, "denied_skill");
  });

  it("a granted-but-bodyless skill returns a bounded ack (never denied_tool, never a throw)", async () => {
    // Granted (in allowedSkills) but absent from ctx.skills → the handler's fallback ack.
    const { map } = toolMap([]);
    const h = makeBroker({
      grants: grants({ allowedTools: new Set(["Skill"]), allowedSkills: new Set(["prd-lifecycle"]), isRoot: true }),
      toolHandlers: map,
    });
    const r = await h.broker.handleToolCall(rt(), "Skill", { skill: "prd-lifecycle" }, "root");
    assert.equal(r.ok, true, "a granted-but-bodyless skill is an ok ack, not a deny");
    if (r.ok) assert.ok(JSON.stringify(r.output).includes("prd-lifecycle"), "the ack names the skill");
  });
});
