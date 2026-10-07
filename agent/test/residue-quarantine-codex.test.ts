import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough, Writable } from "node:stream";

import {
  CodexHarness,
  type CodexLaunchRootResult,
  type CodexProviderConfig,
} from "../src/codex/codex-harness.js";
import { CodexAdviceHarness } from "../src/codex/codex-advice-harness.js";
import { launchCodexRoot, type CodexLaunchSpec, type LauncherDeps } from "../src/codex/launcher.js";
import { CodexTransportError, createCodexTransport, type CodexTransport } from "../src/codex/transport.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import type { CodexCallbackBroker } from "../src/codex/broker.js";
import type { AdviceRequest, HarnessEvent, RunTurnRequest } from "../src/harness.js";
import { ResidueQuarantinedError, latchResidueQuarantine } from "../src/residue-quarantine.js";
import { nullLogger } from "./helpers.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

// issue #2213 — no new Codex provider turn starts on a quarantined worker. Layer: the REAL
// CodexTransportImpl over in-memory streams (the dispatch funnel), the CodexHarness over it (the
// call sites, which also assert so a transport double cannot bypass the check), and
// launchCodexRoot (the app-server launch). The executor-level proofs, over the executor's own
// transport double, are in codex-executor.test.ts ("residue quarantine (issue #2213)").

const GATED = ["account/login/start", "thread/start", "thread/resume", "turn/start"] as const;

function latch(): void {
  latchResidueQuarantine({ cause: 'runner-uid pid 7 "x" could not be attributed', runId: "r1", site: "pre_clone" }, nullLogger());
}

/** Frames written to an outbound stream, decoded. */
function frameLog(outbound: PassThrough): Array<Record<string, unknown>> {
  const frames: Array<Record<string, unknown>> = [];
  let buf = "";
  outbound.setEncoding("utf8");
  outbound.on("data", (chunk: string) => {
    buf += chunk;
    let nl = buf.indexOf("\n");
    while (nl !== -1) {
      const line = buf.slice(0, nl);
      buf = buf.slice(nl + 1);
      if (line.trim().length > 0) frames.push(JSON.parse(line) as Record<string, unknown>);
      nl = buf.indexOf("\n");
    }
  });
  return frames;
}
const methodsOf = (frames: Array<Record<string, unknown>>): string[] => frames.map((f) => String(f.method));
const tick = (): Promise<void> => new Promise((resolve) => setImmediate(resolve));

describe("the real Codex transport refuses provider-turn requests while latched (issue #2213)", () => {
  let inbound: PassThrough;
  let outbound: PassThrough;
  let transport: CodexTransport;
  let frames: Array<Record<string, unknown>>;
  beforeEach(() => {
    inbound = new PassThrough();
    outbound = new PassThrough();
    frames = frameLog(outbound);
    transport = createCodexTransport({ inbound, outbound });
  });
  afterEach(async () => {
    await transport.close();
    inbound.destroy();
    outbound.destroy();
  });

  for (const method of GATED) {
    it(`${method}: a rejected promise carrying the ResidueQuarantinedError (never a CodexTransportError); no frame is written`, async () => {
      latch();
      await assert.rejects(transport.request(method, { x: 1 }, { deadlineMs: 300 }), (err: unknown) => {
        assert.ok(err instanceof ResidueQuarantinedError, String(err));
        assert.ok(!(err instanceof CodexTransportError));
        assert.equal(err.site, "provider_turn");
        return true;
      });
      await tick();
      await tick();
      assert.deepEqual(frames, [], "nothing reached the app-server");
    });
  }

  it("control: unlatched, every gated method is written; latched, a non-turn request (turn/interrupt, initialize) still goes out", async () => {
    for (const method of GATED) void transport.request(method, {}).catch(() => undefined);
    await tick();
    await tick();
    assert.deepEqual(methodsOf(frames), [...GATED]);
    frames.length = 0;
    latch();
    void transport.request("turn/interrupt", { threadId: "t" }).catch(() => undefined);
    void transport.request("initialize", {}).catch(() => undefined);
    await tick();
    await tick();
    assert.deepEqual(methodsOf(frames), ["turn/interrupt", "initialize"], "the latch gates turns, not the protocol");
  });
});

describe("launchCodexRoot refuses a provider app-server while latched (issue #2213)", () => {
  function spec(kind: "provider" | "command"): CodexLaunchSpec {
    return {
      ownedDataRoot: "/data/run/root-1",
      provider: { name: "uzi-codex", baseUrl: "http://127.0.0.1:9/v1", envKey: "CODEX_PROVIDER_KEY", credentialValue: "dummy-key" },
      model: "gpt-5-codex",
      codexBin: "/opt/uzi-codex/0.159.3/bin/codex",
      supervisorBin: "/usr/local/bin/uzi-codex-supervisor",
      kind,
      childArgv: kind === "provider" ? ["app-server"] : ["exec", "--", "echo"],
      cwd: "/work/repo",
    };
  }
  function deps(spawned: unknown[]): LauncherDeps {
    return {
      env: { UZI_UID_SPLIT: "1" },
      resolveRunnerUid: () => 10002,
      resolveCommandUid: () => 10003,
      assertNoUnexpectedSystemConfig: () => undefined,
      makeRunnerTrees: () => undefined,
      removeRunnerTree: () => undefined,
      reportRunnerTreeCleanupFailure: () => undefined,
      spawnSupervisor: (command, args) => {
        spawned.push({ command, args });
        throw new Error("spawn attempted"); // never reached for a refused launch; stops a control launch early
      },
      deadlines: { started: 100, snapshot: 100, dispose: 100, exit: 100 },
    };
  }

  it("a provider root is refused before the supervisor is spawned", async () => {
    latch();
    const spawned: unknown[] = [];
    await assert.rejects(launchCodexRoot(spec("provider"), deps(spawned)), ResidueQuarantinedError);
    assert.deepEqual(spawned, []);
  });

  it("control: while open the provider launch reaches the spawn; a command root is credential-free and reaches it even while latched", async () => {
    const open: unknown[] = [];
    await assert.rejects(launchCodexRoot(spec("provider"), deps(open)), /spawn attempted/);
    assert.equal(open.length, 1);
    latch();
    const cmd: unknown[] = [];
    await assert.rejects(launchCodexRoot(spec("command"), deps(cmd)), /spawn attempted/);
    assert.equal(cmd.length, 1, "a command root is not a provider turn");
  });
});

// ─── the harness over a real transport ─────────────────────────────────────────────────────

const provider: CodexProviderConfig = { name: "openai", baseUrl: "http://127.0.0.1:9/v1", envKey: "OPENAI_API_KEY", model: "gpt-6-astra" };
const fakeRoot: RegisteredRoot = { kind: "provider", reap: async () => ({ ok: true }), dispose: async () => {} };
const broker = { handleToolCall: async () => ({ ok: true, output: {} }) } as unknown as CodexCallbackBroker;

function request(): RunTurnRequest {
  return {
    prompt: "do the thing",
    systemPrompt: "you are the lead",
    signal: new AbortController().signal,
    model: "gpt-6-astra",
    phase: "implement",
    agents: {},
    leadSkills: [],
  };
}

interface LiveRig {
  harness: CodexHarness;
  frames: Array<Record<string, unknown>>;
  launches: () => number;
  close: () => Promise<void>;
}

/** A CodexHarness over the REAL transport; a pump answers each request and completes each turn. */
function liveRig(): LiveRig {
  const inbound = new PassThrough();
  const outbound = new PassThrough();
  const transport = createCodexTransport({ inbound, outbound });
  const frames = frameLog(outbound);
  let launches = 0;
  let buffered = "";
  const write = (frame: unknown): void => {
    inbound.write(`${JSON.stringify(frame)}\n`);
  };
  outbound.on("data", (chunk: string) => {
    buffered += chunk;
    let end: number;
    while ((end = buffered.indexOf("\n")) !== -1) {
      const frame = JSON.parse(buffered.slice(0, end)) as Record<string, unknown>;
      buffered = buffered.slice(end + 1);
      if (typeof frame.method !== "string" || frame.id === undefined) continue;
      const result =
        frame.method === "thread/start" ? { thread: { id: "th-1" } } : frame.method === "turn/start" ? { turn: { id: "tn-1" } } : {};
      write({ id: frame.id, result });
      if (frame.method === "turn/start") {
        setImmediate(() => {
          write({ method: "turn/started", params: { threadId: "th-1", turn: { id: "tn-1" } } });
          write({ method: "turn/completed", params: { threadId: "th-1", turn: { id: "tn-1", status: "completed" } } });
        });
      }
    }
  });
  const harness = new CodexHarness({
    registry: new ExecutionRegistry(newLocalExecutionEpoch(1)),
    launchRoot: async (): Promise<CodexLaunchRootResult> => {
      launches++;
      return { root: fakeRoot, transport, supervisorPid: 4321 };
    },
    broker,
    provider,
    workspace: "/work/repo",
    homeDir: "/work/.codex-state",
    log: nullLogger(),
    sessionInspect: async () => "unknown",
  });
  return {
    harness,
    frames,
    launches: () => launches,
    close: async () => {
      await harness.close();
      inbound.destroy();
      outbound.destroy();
    },
  };
}

async function collect(events: AsyncIterable<HarnessEvent>): Promise<HarnessEvent[]> {
  const out: HarnessEvent[] = [];
  for await (const ev of events) out.push(ev);
  return out;
}

describe("the Codex harness over a real transport (issue #2213)", () => {
  it("control: unlatched, a turn runs: thread/start then turn/start are written", async () => {
    const rig = liveRig();
    try {
      const events = await collect(rig.harness.startTurn(request()).events);
      assert.ok(events.some((e) => e.kind === "turn_finished"));
      assert.deepEqual(methodsOf(rig.frames).filter((m) => m === "thread/start" || m === "turn/start"), ["thread/start", "turn/start"]);
    } finally {
      await rig.close();
    }
  });

  it("fresh transport, latched before the epoch starts: no thread/start, turn/start or login frame; the error is the typed refusal", async () => {
    latch();
    const rig = liveRig();
    try {
      await assert.rejects(collect(rig.harness.startTurn(request()).events), ResidueQuarantinedError);
      await tick();
      const sent = methodsOf(rig.frames);
      for (const m of GATED) assert.ok(!sent.includes(m), `${m} was written: ${sent.join(",")}`);
    } finally {
      await rig.close();
    }
  });

  it("reused transport: turn 1 completes, the latch is set, and no second turn/start frame is written; typed, no resume remap", async () => {
    const rig = liveRig();
    try {
      const first = await collect(rig.harness.startTurn(request()).events);
      assert.ok(first.some((e) => e.kind === "turn_finished"));
      assert.equal(methodsOf(rig.frames).filter((m) => m === "turn/start").length, 1);
      latch();
      await assert.rejects(
        collect(rig.harness.startTurn(request()).events),
        (err: unknown) => {
          assert.ok(err instanceof ResidueQuarantinedError, String(err));
          assert.ok(!(err instanceof CodexTransportError), "never wrapped or remapped as a transport/resume fault");
          return true;
        },
      );
      await tick();
      assert.equal(methodsOf(rig.frames).filter((m) => m === "turn/start").length, 1, "no second turn/start was written");
      assert.equal(rig.launches(), 1, "the live transport was reused, not relaunched");
    } finally {
      await rig.close();
    }
  });

  it("a delegated child's thread/start and turn/start are refused with no frame, as a rejected promise", async () => {
    const rig = liveRig();
    try {
      await collect(rig.harness.startTurn(request()).events); // the root turn: the transport is live
      const before = rig.frames.length;
      latch();
      for (const method of ["thread/start", "turn/start"]) {
        await assert.rejects(rig.harness.requestOnTransport(method, { x: 1 }), ResidueQuarantinedError);
      }
      await tick();
      assert.equal(rig.frames.length, before, "no frame was written");
    } finally {
      await rig.close();
    }
  });
});

describe("the Codex advice lane over a real transport (issue #2213)", () => {
  function adviceRig(): { harness: CodexAdviceHarness; frames: Array<Record<string, unknown>>; close: () => void } {
    const inbound = new PassThrough();
    const outbound = new PassThrough();
    const transport = createCodexTransport({ inbound, outbound });
    const frames = frameLog(outbound);
    const harness = new CodexAdviceHarness({
      launchRoot: async () => ({ transport, cwd: "/isolated/advice-home/work", dispose: async () => {} }),
      provider,
      log: nullLogger(),
    });
    return { harness, frames, close: () => { inbound.destroy(); outbound.destroy(); } };
  }
  const adviceRequest = (): AdviceRequest => ({
    label: "review",
    systemPrompt: "you are the reviewer",
    prompt: "review this",
    model: "gpt-6-astra",
    output: { kind: "text" },
    signal: new AbortController().signal,
    timeoutMs: 5000,
  });

  it("a latched worker writes no thread/start or turn/start frame for an advice turn", async () => {
    latch();
    const rig = adviceRig();
    try {
      await assert.rejects(rig.harness.run(adviceRequest(), { onTerminal: () => {} }), ResidueQuarantinedError);
      await tick();
      const sent = methodsOf(rig.frames);
      assert.ok(!sent.includes("thread/start") && !sent.includes("turn/start"), sent.join(","));
    } finally {
      rig.close();
    }
  });
});

describe("the real Codex transport rechecks the latch when a queued gated frame is written (issue #2213)", () => {
  it("a turn/start queued behind backpressure while the worker latches is never written; its promise rejects with the ResidueQuarantinedError; later non-gated frames still go out", async () => {
    const written: string[] = [];
    let release: (() => void) | undefined;
    const outbound = new Writable({
      highWaterMark: 1,
      write(chunk: Buffer, _enc, cb) {
        written.push(chunk.toString("utf8"));
        // Hold the first write's callback: write() returns false (backpressure) until released.
        if (release === undefined) release = () => cb();
        else cb();
      },
    });
    const inbound = new PassThrough();
    const transport = createCodexTransport({ inbound, outbound });
    try {
      void transport.request("initialize", {}).catch(() => undefined);
      await tick();
      assert.equal(written.length, 1, "the first frame is written and stalls the stream");
      const gated = transport.request("turn/start", { x: 1 }, { deadlineMs: 500 });
      const gatedOutcome = gated.then(
        () => undefined,
        (err: unknown) => err,
      );
      void transport.request("turn/interrupt", { threadId: "t" }).catch(() => undefined);
      await tick();
      assert.equal(written.length, 1, "both later frames are queued behind the backpressure");
      latch();
      release?.();
      await tick();
      await tick();
      const err = await gatedOutcome;
      assert.ok(err instanceof ResidueQuarantinedError, String(err));
      assert.ok(!(err instanceof CodexTransportError));
      const methods = written.map((l) => (JSON.parse(l) as { method: string }).method);
      assert.deepEqual(methods, ["initialize", "turn/interrupt"], "the gated frame never reached the app-server");
    } finally {
      await transport.close();
      inbound.destroy();
      outbound.destroy();
    }
  });
});
