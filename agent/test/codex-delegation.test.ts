import { describe, it } from "node:test";
import assert from "node:assert/strict";

import {
  CodexDelegationRunner,
  type ChildThreadController,
  type DelegationRole,
  type StartChildTurnSpec,
} from "../src/codex/delegation.js";
import type {
  ChildDelegationRequest,
  FileopRequest,
  FileopResponse,
  RunGrants,
  SpawnCommandOptions,
  SpawnCommandResult,
} from "../src/codex/broker.js";
import { ExecutionRegistry, newLocalExecutionEpoch } from "../src/codex/registry.js";
import type { CodexNotification } from "../src/codex/transport.js";

// PRD #1171 (M3, milestone 2) — the synchronous child-thread delegation runner. Every
// external effect is a FAKE: a scripted {@link ChildThreadController}, spy shell/file
// seams, and the REAL {@link ExecutionRegistry} + REAL child {@link CodexCallbackBroker}
// (constructed inside the runner) so the honest-origin/root-only/settlement policy is
// exercised end to end. No real Codex, no process, no file.

const WORKTREE = "/tmp/uzi-deleg-worktree";

/** A scripted child controller: yields `notes` in order then optionally HANGS (so an
 *  abort/deadline can be observed), recording respond/interrupt/close. */
class FakeController implements ChildThreadController {
  readonly threadId: string;
  readonly turnId: string;
  respondCalls: { requestId: number | string; reply: unknown }[] = [];
  interrupted = false;
  closed = false;
  private readonly notes: CodexNotification[];
  private readonly hangAfter: boolean;

  constructor(opts: { threadId?: string; turnId?: string; notes: CodexNotification[]; hangAfter?: boolean }) {
    this.threadId = opts.threadId ?? "child-thread";
    this.turnId = opts.turnId ?? "child-turn";
    this.notes = opts.notes;
    this.hangAfter = opts.hangAfter ?? false;
  }

  notifications(): AsyncIterableIterator<CodexNotification> {
    const notes = this.notes;
    const hangAfter = this.hangAfter;
    async function* gen(): AsyncGenerator<CodexNotification> {
      for (const n of notes) yield n;
      if (hangAfter) await new Promise<never>(() => {}); // never resolves → cancellable
    }
    return gen();
  }

  respond(requestId: number | string, reply: unknown): void {
    this.respondCalls.push({ requestId, reply });
  }
  async interrupt(): Promise<void> {
    this.interrupted = true;
  }
  async close(): Promise<void> {
    this.closed = true;
  }
}

function childGrants(overrides: Partial<RunGrants> = {}): RunGrants {
  return {
    role: "coder",
    phase: "implement",
    allowedTools: new Set(["Bash", "apply_patch", "Read"]),
    allowedSkills: new Set<string>(),
    isRoot: false,
    ...overrides,
  };
}

function role(overrides: Partial<DelegationRole> = {}): DelegationRole {
  return {
    grants: childGrants(),
    systemPrompt: "you are the coder subagent",
    model: "gpt-6-astra",
    effort: "medium",
    ...overrides,
  };
}

class SpawnSpy {
  calls: { argv: readonly string[]; opts: SpawnCommandOptions }[] = [];
  seam = async (argv: readonly string[], opts: SpawnCommandOptions): Promise<SpawnCommandResult> => {
    this.calls.push({ argv, opts });
    return { code: 0, stdout: "", stderr: "" };
  };
}

class FileopSpy {
  calls: FileopRequest[] = [];
  constructor(private readonly response: FileopResponse = { ok: true, size: 3, data: Buffer.from("hi").toString("base64") }) {}
  op = async (request: FileopRequest): Promise<FileopResponse> => {
    this.calls.push(request);
    return this.response;
  };
}

interface Built {
  runner: CodexDelegationRunner;
  registry: ExecutionRegistry;
  spawn: SpawnSpy;
  fileop: FileopSpy;
  startSpecs: StartChildTurnSpec[];
}

function makeRunner(opts: {
  controller?: ChildThreadController;
  roles?: Map<string, DelegationRole>;
  signal?: AbortSignal;
  childTurnDeadlineMs?: number;
  startImpl?: (spec: StartChildTurnSpec) => Promise<ChildThreadController>;
  spawnImpl?: (argv: readonly string[], o: SpawnCommandOptions) => Promise<SpawnCommandResult>;
} = {}): Built {
  const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
  const spawn = new SpawnSpy();
  const fileop = new FileopSpy();
  const startSpecs: StartChildTurnSpec[] = [];
  const roles = opts.roles ?? new Map<string, DelegationRole>([["coder", role()]]);
  const runner = new CodexDelegationRunner({
    registry,
    roles,
    startChildTurn: async (spec) => {
      startSpecs.push(spec);
      if (opts.startImpl) return opts.startImpl(spec);
      return opts.controller ?? new FakeController({ notes: [] });
    },
    spawnCommand: opts.spawnImpl ?? spawn.seam,
    fileop,
    worktreePath: WORKTREE,
    signal: opts.signal,
    childTurnDeadlineMs: opts.childTurnDeadlineMs,
  });
  return { runner, registry, spawn, fileop, startSpecs };
}

function delegReq(overrides: Partial<ChildDelegationRequest> = {}): ChildDelegationRequest {
  return {
    tool: "spawn_agent",
    role: "coder",
    args: { prompt: "do the task" },
    parent: { threadId: "root-thread", turnId: "root-turn", callId: "root-call" },
    ...overrides,
  };
}

/** A server→client tool-call note bound to the given child ids. */
function toolCallNote(
  ctrl: ChildThreadController,
  callId: string,
  tool: string,
  args: unknown,
  requestId: number | string = 7,
): CodexNotification {
  return {
    kind: "activity",
    method: "item/tool/call",
    requestId,
    params: { threadId: ctrl.threadId, turnId: ctrl.turnId, callId, tool, arguments: args },
  };
}

function agentMessageNote(ctrl: ChildThreadController, text: string): CodexNotification {
  return {
    kind: "activity",
    method: "item/completed",
    params: { threadId: ctrl.threadId, item: { type: "agentMessage", text } },
  };
}

function turnCompletedNote(ctrl: ChildThreadController, status: string): CodexNotification {
  return { kind: "turn_completed", method: "turn/completed", threadId: ctrl.threadId, turnId: ctrl.turnId, status, params: {} };
}

/** Read the `success` flag off a recorded respond call's `{ result: { success } }`. */
function respondSuccess(controller: FakeController, idx: number): boolean {
  const reply = controller.respondCalls[idx]!.reply as { result?: { success?: boolean } };
  return reply.result?.success === true;
}

/** Set a controller's scripted notes after construction (they reference the controller). */
function withNotes(controller: FakeController, notes: CodexNotification[]): FakeController {
  (controller as unknown as { notes: CodexNotification[] }).notes = notes;
  return controller;
}

describe("CodexDelegationRunner: admission", () => {
  it("denies an unknown role WITHOUT starting a child", async () => {
    const b = makeRunner();
    const res = await b.runner.run(delegReq({ role: "ghost" }));
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_denied");
    assert.equal(b.startSpecs.length, 0);
  });

  it("denies a role whose grants are root (never runs a root-graned child)", async () => {
    const roles = new Map<string, DelegationRole>([["boss", role({ grants: childGrants({ isRoot: true }) })]]);
    const b = makeRunner({ roles });
    const res = await b.runner.run(delegReq({ role: "boss" }));
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_denied");
    assert.equal(b.startSpecs.length, 0);
  });
});

describe("CodexDelegationRunner: happy path + honest attribution", () => {
  it("runs a child to completion, accumulates its text, and closes it", async () => {
    const controller = new FakeController({ threadId: "ct-1", turnId: "cu-1", notes: [] });
    withNotes(controller, [
      agentMessageNote(controller, "child result"),
      turnCompletedNote(controller, "completed"),
    ]);
    const b = makeRunner({ controller });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, true);
    if (res.ok) assert.deepEqual(res.output, { role: "coder", text: "child result" });
    assert.equal(controller.closed, true);
    assert.equal(controller.interrupted, false);
    // The child ran on its OWN honest prompt/model/task, not the parent's.
    assert.equal(b.startSpecs.length, 1);
    assert.equal(b.startSpecs[0]!.role, "coder");
    assert.equal(b.startSpecs[0]!.grants.isRoot, false, "the child thread receives only its immutable child grants");
    assert.equal(b.startSpecs[0]!.taskInput, "do the task");
    assert.equal(b.startSpecs[0]!.model, "gpt-6-astra");
    assert.deepEqual(b.startSpecs[0]!.parent, { threadId: "root-thread", turnId: "root-turn", callId: "root-call" });
  });

  it("routes a child effect through the child broker (origin child) and SETTLES it before resolving", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] });
    (controller as unknown as { notes: CodexNotification[] }).notes = [
      toolCallNote(controller, "cc-1", "Read", { path: "src/x.ts" }),
      turnCompletedNote(controller, "completed"),
    ];
    const b = makeRunner({ controller });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, true);
    // The child's Read ran through the fileop client under the child grants.
    assert.equal(b.fileop.calls.length, 1);
    assert.equal(b.fileop.calls[0]!.op, "read");
    // The child callback got a successful reply.
    assert.equal(respondSuccess(controller, 0), true);
    // The callback settled in the SHARED registry keyed by the CHILD's honest identity,
    // and no callback is left in flight before the parent callback resolves.
    assert.equal(b.registry.inFlightCallbackCount(), 0);
    assert.equal(b.registry.isPoisoned(), false);
  });
});

describe("CodexDelegationRunner: root-only + nested denial (honest child origin)", () => {
  it("denies a child submit_plan signal even if the child grant listed it (origin child)", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] });
    (controller as unknown as { notes: CodexNotification[] }).notes = [
      toolCallNote(controller, "cc-sig", "submit_plan", { plan_md: "sneaky" }),
      turnCompletedNote(controller, "completed"),
    ];
    // A deliberately-permissive child grant that even lists the signal tool: the ORIGIN
    // gate must still deny it because a subagent is never root.
    const roles = new Map<string, DelegationRole>([
      ["coder", role({ grants: childGrants({ allowedTools: new Set(["Read", "submit_plan"]) }) })],
    ]);
    const b = makeRunner({ controller, roles });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, true); // the child turn itself completed
    // The signal callback was REFUSED (success:false) — it never latched a workflow signal.
    assert.equal(respondSuccess(controller, 0), false);
  });

  it("denies a NESTED spawn_agent from the child even if its grant listed it (origin child)", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] });
    (controller as unknown as { notes: CodexNotification[] }).notes = [
      toolCallNote(controller, "cc-del", "spawn_agent", { subagent_type: "coder", prompt: "recurse" }),
      turnCompletedNote(controller, "completed"),
    ];
    const roles = new Map<string, DelegationRole>([
      ["coder", role({ grants: childGrants({ allowedTools: new Set(["Read", "spawn_agent"]) }) })],
    ]);
    const b = makeRunner({ controller, roles });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, true);
    // The nested delegation was refused (success:false) and NO second child was started.
    assert.equal(respondSuccess(controller, 0), false);
    assert.equal(b.startSpecs.length, 1);
  });

  it("refuses a non-tool-call child server request fail-closed without a broker call", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] });
    (controller as unknown as { notes: CodexNotification[] }).notes = [
      { kind: "activity", method: "unknown/serverRequest", requestId: 3, params: {} },
      turnCompletedNote(controller, "completed"),
    ];
    const b = makeRunner({ controller });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, true);
    const reply = controller.respondCalls[0]!.reply as { error?: { code: number } };
    assert.equal(reply.error?.code, -32601);
  });
});

describe("CodexDelegationRunner: terminal + cancellation", () => {
  it("returns child_failed on a non-completed terminal", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] });
    (controller as unknown as { notes: CodexNotification[] }).notes = [turnCompletedNote(controller, "error")];
    const b = makeRunner({ controller });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_failed");
    assert.equal(controller.closed, true);
  });

  it("returns child_failed on a clean EOF before any terminal", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [] }); // empty, no hang → ends
    const b = makeRunner({ controller });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_failed");
  });

  it("returns child_aborted and interrupts+closes the child when the run signal aborts", async () => {
    const parent = new AbortController();
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [], hangAfter: true });
    const b = makeRunner({ controller, signal: parent.signal });
    const p = b.runner.run(delegReq());
    await new Promise((r) => setTimeout(r, 15));
    parent.abort();
    const res = await p;
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_aborted");
    assert.equal(controller.interrupted, true);
    assert.equal(controller.closed, true);
  });

  it("returns child_timeout when the child never completes within the deadline", async () => {
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [], hangAfter: true });
    const b = makeRunner({ controller, childTurnDeadlineMs: 30 });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_timeout");
    assert.equal(controller.interrupted, true);
    assert.equal(controller.closed, true);
  });

  it("returns child_timeout (does NOT hang) when a child CALLBACK is wedged and the deadline fires", async () => {
    // Regression for the settlement-barrier hang: a child effect that never settles (e.g.
    // `sleep infinity`, which passes the bash screener) must not make runChild block
    // forever at the `finally` barrier. Before the fix the barrier did an UNCONDITIONAL
    // `await Promise.allSettled(pending)`, re-awaiting the very callback the abort race had
    // abandoned, so runChild never reached interrupt()/close(). The existing child_timeout
    // test above only wedges the NOTIFICATION stream (pending is empty at the barrier); this
    // one wedges an admitted CALLBACK, which is the case that broke.
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [], hangAfter: true });
    (controller as unknown as { notes: CodexNotification[] }).notes = [
      toolCallNote(controller, "cc-hang", "Bash", { command: "sleep infinity" }),
    ];
    const b = makeRunner({
      controller,
      childTurnDeadlineMs: 30,
      spawnImpl: () => new Promise<SpawnCommandResult>(() => {}), // never settles
    });
    const hung = new Promise<never>((_, reject) => {
      const t = setTimeout(
        () => reject(new Error("runChild hung: the settlement barrier re-awaited a wedged child callback on the abort path")),
        5000,
      );
      t.unref?.();
    });
    const res = await Promise.race([b.runner.run(delegReq()), hung]);
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_timeout");
    assert.equal(controller.interrupted, true);
    assert.equal(controller.closed, true);
  });

  it("denies before start when the run signal is already aborted", async () => {
    const parent = new AbortController();
    parent.abort();
    const controller = new FakeController({ threadId: "ct", turnId: "cu", notes: [], hangAfter: true });
    const b = makeRunner({ controller, signal: parent.signal });
    const res = await b.runner.run(delegReq());
    assert.equal(res.ok, false);
    assert.equal(res.code, "child_aborted");
    // Nothing was started; the pre-start abort short-circuits.
    assert.equal(b.startSpecs.length, 0);
  });
});
