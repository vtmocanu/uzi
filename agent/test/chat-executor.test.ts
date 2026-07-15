import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions, SDKMessage, HookInput } from "@anthropic-ai/claude-agent-sdk";
import {
  ChatExecutor,
  buildChatSdkOptions,
  buildChatSystemPrompt,
  CHAT_BASE_TOOLS,
  UZI_SRC_DIR,
  type ChatContext,
  type ChatScheduler,
  type ChatTimerHandle,
  type SdkQueryFn,
} from "../src/chat-executor.js";
import type { EmittedMessage } from "../src/executor.js";
import { nullLogger } from "./helpers.js";

// The chat executor is exercised only up to — never across — the network boundary:
// `queryFn`/`spawn`/`kill` are faked and the lifecycle clocks are an injected
// scheduler, so the tool restriction, path-guard confinement, turn cap, idle
// completion, and per-turn wall-clock are all provable with dummy credentials, NO
// live Anthropic session, and NO real waits.

const OAUTH = "dummy-oauth-token-do-not-scan-0000";
const TOKEN_FILE = "/run/secrets/worker_token";

// --- scripted SDK messages ---------------------------------------------------

function assistantText(text: string, sessionId = "sess-1"): SDKMessage {
  return { type: "assistant", session_id: sessionId, message: { content: [{ type: "text", text }] } } as unknown as SDKMessage;
}
function resultSuccess(sessionId = "sess-1"): SDKMessage {
  return { type: "result", subtype: "success", is_error: false, num_turns: 1, session_id: sessionId } as unknown as SDKMessage;
}

interface Turn {
  options: SdkOptions;
  promptText?: string;
}

/** A fake `query` that replays one scripted stream per turn (invocation). */
function fakeTurns(scripts: Array<SDKMessage[] | ((signal: AbortSignal) => AsyncIterable<unknown>)>): {
  queryFn: SdkQueryFn;
  turns: Turn[];
} {
  const turns: Turn[] = [];
  let i = 0;
  const queryFn: SdkQueryFn = (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    const turn: Turn = { options: params.options };
    turns.push(turn);
    return (async function* () {
      for await (const p of params.prompt) {
        const rec = p as { message?: { content?: unknown } };
        const content = rec.message?.content;
        turn.promptText = typeof content === "string" ? content : JSON.stringify(content);
      }
      const s = typeof script === "function" ? script(params.options.abortController!.signal) : script;
      if (Array.isArray(s)) for (const m of s) yield m;
      else yield* s as AsyncIterable<SDKMessage>;
    })();
  };
  return { queryFn, turns };
}

function hangUntilAbort(signal: AbortSignal): AsyncIterable<unknown> {
  return {
    async *[Symbol.asyncIterator]() {
      await new Promise<void>((resolve) => {
        if (signal.aborted) return resolve();
        // A real hung SDK query has pending network I/O that keeps the event loop
        // alive; the executor's watchdog timers are unref'd, so without a ref'd
        // keep-alive here node 22 drains the loop before the watchdog fires
        // ("event loop has already resolved"). Cleared the moment the watchdog aborts.
        const keepAlive = setInterval(() => {}, 1_000);
        signal.addEventListener(
          "abort",
          () => {
            clearInterval(keepAlive);
            resolve();
          },
          { once: true },
        );
      });
    },
  };
}

/** A scheduler whose timers fire only when the test tells them to (no real waits). */
class FakeScheduler implements ChatScheduler {
  private seq = 0;
  readonly timers: { id: number; cb: () => void; ms: number; cleared: boolean }[] = [];
  set(cb: () => void, ms: number): ChatTimerHandle {
    const t = { id: ++this.seq, cb, ms, cleared: false };
    this.timers.push(t);
    return { clear: () => (t.cleared = true) };
  }
  /** Fire the most-recently-set, still-live timer whose ms matches (or any). */
  fire(ms?: number): void {
    const t = [...this.timers].reverse().find((x) => !x.cleared && (ms === undefined || x.ms === ms));
    if (!t) throw new Error(`no live timer to fire (ms=${ms ?? "any"})`);
    t.cleared = true;
    t.cb();
  }
}

const flush = (): Promise<void> => new Promise((r) => setImmediate(r));

/** Wait until the executor has armed a live timer of the given ms (it does real
 *  async I/O — fs.mkdir — before the loop, so a single microtask flush is too early). */
async function waitForTimer(sched: FakeScheduler, ms: number, tries = 200): Promise<void> {
  for (let i = 0; i < tries; i++) {
    if (sched.timers.some((t) => !t.cleared && t.ms === ms)) return;
    await new Promise((r) => setTimeout(r, 1));
  }
  throw new Error(`timer ms=${ms} was never armed`);
}

let homeDir: string;
beforeEach(() => {
  homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-chathome-"));
});
afterEach(() => {
  fs.rmSync(homeDir, { recursive: true, force: true });
});

interface CtxProbe {
  ctx: ChatContext;
  emits: EmittedMessage[];
  sessionIds: string[];
}

/**
 * A context whose user messages come from `queue`, consumed one per park:
 *   - a string       → resolves with that message,
 *   - `undefined`    → resolves undefined (source has no more input → idle-complete),
 *   - `null`         → returns a never-resolving promise (parks until ctx.signal aborts),
 *   - queue empty    → resolves undefined.
 * Idle is owned by the input source now (steering), so it surfaces here as an
 * `undefined` resolution, not a timer the executor fires.
 */
function makeCtx(queue: (string | undefined | null)[], overrides: Partial<ChatContext> = {}): CtxProbe {
  const emits: EmittedMessage[] = [];
  const sessionIds: string[] = [];
  const pending = [...queue];
  const ctx: ChatContext = {
    runId: "chat-1",
    oauthToken: OAUTH,
    sessionId: null,
    emit: (m) => emits.push(m),
    onSessionId: (s) => sessionIds.push(s),
    maxTurns: 50,
    turnTimeoutMs: 5000,
    nextUserMessage: () => {
      if (pending.length === 0) return Promise.resolve<string | undefined>(undefined);
      const v = pending.shift();
      if (v === null) return new Promise<string | undefined>(() => {});
      return Promise.resolve<string | undefined>(v);
    },
    ...overrides,
  };
  return { ctx, emits, sessionIds };
}

describe("buildChatSdkOptions confinement (PRD #39 Decision 6)", () => {
  const opts = (over: Partial<Parameters<typeof buildChatSdkOptions>[0]> = {}): SdkOptions =>
    buildChatSdkOptions({
      env: { CLAUDE_CODE_OAUTH_TOKEN: OAUTH, HOME: "/data/agent-home", PATH: "/usr/bin", ANTHROPIC_API_KEY: undefined, ANTHROPIC_AUTH_TOKEN: undefined },
      systemPrompt: buildChatSystemPrompt(),
      cwd: UZI_SRC_DIR,
      log: nullLogger(),
      secretPaths: [TOKEN_FILE],
      ...over,
    });

  it("restricts the tool set via `tools` (NOT allowedTools) to read-only investigation", () => {
    const o = opts();
    assert.deepStrictEqual(o.tools, [...CHAT_BASE_TOOLS]); // Read/Grep/Glob only
    assert.strictEqual(o.allowedTools, undefined, "must not use allowedTools — it does not confine");
    // No Bash/Write/Edit/WebFetch/WebSearch/Agent in the base set.
    for (const forbidden of ["Bash", "Write", "Edit", "WebFetch", "WebSearch", "Agent"]) {
      assert.ok(!(o.tools as string[]).includes(forbidden), `${forbidden} must not be available`);
    }
  });

  it("appends the M3 uzi MCP tool names to the base set when provided (seam)", () => {
    const o = opts({ extraTools: ["mcp__uzi__list_runs", "mcp__uzi__get_run"] });
    assert.deepStrictEqual(o.tools, ["Read", "Grep", "Glob", "mcp__uzi__list_runs", "mcp__uzi__get_run"]);
  });

  it("keeps settingSources [], bypassPermissions, and no partial messages; cwd is the baked source", () => {
    const o = opts();
    assert.deepStrictEqual(o.settingSources, []);
    assert.strictEqual(o.permissionMode, "bypassPermissions");
    assert.strictEqual(o.includePartialMessages, false);
    assert.strictEqual(o.cwd, UZI_SRC_DIR);
  });

  it("disallows the Agent + async-deferral tools even though they are not in the base set", () => {
    const o = opts();
    for (const t of ["Agent", "ScheduleWakeup", "CronCreate"]) {
      assert.ok((o.disallowedTools ?? []).includes(t), `${t} must be disallowed`);
    }
  });

  it("wires exactly two PreToolUse matchers: Bash and the file tools (no Agent guard — no subagents)", () => {
    const o = opts();
    const matchers = (o.hooks?.PreToolUse ?? []).map((m) => m.matcher);
    assert.deepStrictEqual(matchers, ["Bash", "Read|Edit|Write|MultiEdit|NotebookEdit|Glob|Grep"]);
  });

  // The path-guard hook of a built options object, plus a caller + decision reader.
  const pathHook = (o: SdkOptions) => o.hooks!.PreToolUse![1]!.hooks[0]!;
  const readPath = (o: SdkOptions, file: string, root: string = UZI_SRC_DIR) =>
    pathHook(o)(
      { hook_event_name: "PreToolUse", tool_name: "Read", cwd: root, tool_input: { file_path: file } } as unknown as HookInput,
      "tu",
      { signal: new AbortController().signal },
    );
  const decision = (r: unknown) => (r as { hookSpecificOutput?: { permissionDecision?: string } }).hookSpecificOutput?.permissionDecision;

  it("denies the join-token file via the outside-root deny — the LOAD-BEARING block, no param needed", async () => {
    // Root the guard at /opt/uzi-src and DO NOT pass the token in extraSecretPaths:
    // the token file lives outside the baked source, so the containment check alone
    // must deny it. This is the protection that survives if the param is ever dropped.
    assert.strictEqual(decision(await readPath(opts({ secretPaths: [] }), TOKEN_FILE)), "deny");
  });

  it("keeps the /proc deny intact and denies escaping the baked source", async () => {
    const o = opts();
    assert.strictEqual(decision(await readPath(o, "/proc/self/environ")), "deny", "/proc read still denied");
    assert.strictEqual(decision(await readPath(o, "/etc/passwd")), "deny", "outside-source read denied");
  });

  it("extraSecretPaths adds defense-in-depth: a secret file INSIDE the root is denied (and allowed without the param)", async () => {
    // A file inside the root passes the containment check, so the ONLY thing that can
    // deny it is the param — this proves the new parameter actually does work, on the
    // resolved path. Use a real temp dir so the guard's realpath resolution resolves.
    const realRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-src-"));
    const insideSecret = path.join(realRoot, "join-token");
    fs.writeFileSync(insideSecret, "tkn\n");
    try {
      assert.strictEqual(decision(await readPath(opts({ cwd: realRoot, secretPaths: [insideSecret] }), insideSecret, realRoot)), "deny", "denied by the param");
      assert.deepStrictEqual(await readPath(opts({ cwd: realRoot, secretPaths: [] }), insideSecret, realRoot), {}, "without the param the in-root file is allowed");
    } finally {
      fs.rmSync(realRoot, { recursive: true, force: true });
    }
  });

  it("allows an in-source read (no decision), proving the guard is rooted at the baked source", async () => {
    const realRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-src-"));
    fs.mkdirSync(path.join(realRoot, "api"));
    fs.writeFileSync(path.join(realRoot, "api", "main.go"), "package main\n");
    try {
      assert.deepStrictEqual(await readPath(opts({ cwd: realRoot }), path.join(realRoot, "api", "main.go"), realRoot), {});
    } finally {
      fs.rmSync(realRoot, { recursive: true, force: true });
    }
  });

  it("omits the model key entirely when no model resolves, and sets it when present", () => {
    assert.ok(!("model" in opts()), "model must be omitted, not undefined");
    assert.strictEqual(opts({ model: "sonnet" }).model, "sonnet");
  });
});

describe("buildChatSystemPrompt", () => {
  it("is the claude_code preset plus an append naming the baked source, BUILD_INFO, and honesty", () => {
    const sp = buildChatSystemPrompt();
    assert.strictEqual(sp.type, "preset");
    assert.strictEqual(sp.preset, "claude_code");
    assert.match(sp.append, /\/opt\/uzi-src/);
    assert.match(sp.append, /BUILD_INFO/);
    assert.match(sp.append, /UNTRUSTED/);
    assert.match(sp.append, /never invent/i);
  });
});

describe("ChatExecutor session env + assembly", () => {
  it("fails fast when no OAuth token is present", async () => {
    const { queryFn } = fakeTurns([[resultSuccess()]]);
    const probe = makeCtx(["hi"], { oauthToken: undefined });
    await assert.rejects(
      new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(probe.ctx),
      /no Anthropic OAuth token/,
    );
  });

  it("hands the SDK a sparse env (no worker secrets) and the read-only tool set every turn", async () => {
    const { queryFn, turns } = fakeTurns([[assistantText("answer"), resultSuccess()]]);
    const probe = makeCtx(["how does the plan gate work?", undefined]);
    await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler(), secretPaths: [TOKEN_FILE] }).run(probe.ctx);
    assert.strictEqual(turns.length, 1);
    const o = turns[0]!.options;
    assert.deepStrictEqual(new Set(Object.keys(o.env!)), new Set(["CLAUDE_CODE_OAUTH_TOKEN", "HOME", "PATH", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"]));
    assert.deepStrictEqual(o.tools, [...CHAT_BASE_TOOLS]);
    assert.strictEqual(o.cwd, UZI_SRC_DIR);
    // The user message rode the prompt stream as-is.
    assert.match(turns[0]!.promptText ?? "", /plan gate/);
  });

  it("persists the SDK session id once, then resumes it on the next turn", async () => {
    const { queryFn, turns } = fakeTurns([
      [assistantText("a", "sess-A"), resultSuccess("sess-A")],
      [assistantText("b", "sess-A"), resultSuccess("sess-A")],
    ]);
    const probe = makeCtx(["q1", "q2", undefined]);
    await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(probe.ctx);
    assert.deepStrictEqual(probe.sessionIds, ["sess-A"]); // reported exactly once
    assert.strictEqual(turns[0]!.options.resume, undefined); // fresh
    assert.strictEqual(turns[1]!.options.resume, "sess-A"); // resumed
  });
});

describe("ChatExecutor lifecycle clocks (Decision 3)", () => {
  it("caps the conversation at maxTurns and completes with a turn-cap notice", async () => {
    const { queryFn, turns } = fakeTurns([[assistantText("a"), resultSuccess()]]);
    // Endless supply of messages; the cap — not the input — stops the loop.
    const probe = makeCtx(["m1", "m2", "m3", "m4"], { maxTurns: 2 });
    const result = await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(probe.ctx);
    assert.strictEqual(result.turns, 2);
    assert.strictEqual(result.endReason, "turn_cap");
    assert.strictEqual(turns.length, 2, "exactly maxTurns turns are driven");
    assert.ok(probe.emits.some((m) => m.kind === "status" && /turn limit/.test(String(m.payload["text"]))));
  });

  it("completes with idle when the input source has no more input (source owns the idle clock)", async () => {
    // Idle is no longer an executor timer (task #8): the source resolves undefined,
    // and since ctx.signal is NOT aborted the loop completes as idle (not ended).
    const { queryFn } = fakeTurns([[resultSuccess()]]);
    const probe = makeCtx([undefined]); // first park → no more input → idle
    const result = await new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: new FakeScheduler() }).run(probe.ctx);
    assert.strictEqual(result.turns, 0);
    assert.strictEqual(result.endReason, "idle");
  });

  it("aborts a runaway turn when the per-turn wall-clock fires, then keeps the conversation alive", async () => {
    const kills: (number | undefined)[] = [];
    const { queryFn, turns } = fakeTurns([
      (signal) => hangUntilAbort(signal), // turn 1 hangs until aborted
    ]);
    const sched = new FakeScheduler();
    // One message, then no more input so the loop idle-ends after the timed-out turn.
    const probe = makeCtx(["long question", undefined], { turnTimeoutMs: 5000 });
    const run = new ChatExecutor(nullLogger(), homeDir, {
      queryFn,
      scheduler: sched,
      spawn: () => ({ pid: 4242 }),
      kill: (p) => (kills.push(p), true),
    }).run(probe.ctx);
    await waitForTimer(sched, 5000); // driveChatTurn armed the per-turn wall-clock
    sched.fire(5000); // fire the per-turn wall-clock
    const result = await run;

    assert.strictEqual(turns[0]!.options.abortController!.signal.aborted, true, "the turn was aborted");
    assert.strictEqual(result.turns, 1);
    assert.ok(probe.emits.some((m) => m.kind === "status" && /too long/.test(String(m.payload["text"]))));
  });

  it("ends the whole chat cleanly when the external signal aborts (End chat)", async () => {
    const controller = new AbortController();
    const { queryFn } = fakeTurns([[resultSuccess()]]);
    const sched = new FakeScheduler();
    const probe = makeCtx([null], { signal: controller.signal }); // parks
    const run = new ChatExecutor(nullLogger(), homeDir, { queryFn, scheduler: sched }).run(probe.ctx);
    await flush();
    controller.abort(); // End chat while parked
    const result = await run;
    assert.strictEqual(result.turns, 0);
    assert.strictEqual(result.endReason, "ended");
  });
});
