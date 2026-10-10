// PRD #2603 M1: the NowSummaryController, driven with a fake clock, fake timers, a fake model
// pass and a fake steering view. Nothing here touches a model, the network or real time.

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import {
  HELD_CONTEXT_CALLBACKS,
  NOW_MAX_FRAMES,
  NOW_MIN_INTERVAL_MS,
  NOW_PERIODIC_MS,
  NOW_STOP_BOUND_MS,
  NowSummaryController,
  holdContextCallbacks,
  sanitizeNowText,
  trimToolFrame,
  type NowSummaryDeps,
} from "../src/now-summary.js";
import type { EmittedMessage, RunContext } from "../src/executor.js";
import type { AdviceUsageSnapshot } from "../src/harness.js";
import { LimitReachedError } from "../src/limit.js";
import type { ReadOnlyModelPassOpts } from "../src/model-pass.js";
import type { Milestone, MilestoneProgress } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const MIN = 60_000;

class FakeClock {
  t = 1_000_000;
  private seq = 0;
  private readonly timers = new Map<number, { at: number; fn: () => void }>();
  now = (): number => this.t;
  setTimer = (fn: () => void, ms: number): unknown => {
    const id = ++this.seq;
    this.timers.set(id, { at: this.t + ms, fn });
    return id;
  };
  clearTimer = (id: unknown): void => {
    this.timers.delete(id as number);
  };
  get pendingTimers(): number {
    return this.timers.size;
  }
  /** Move time forward, firing every timer that comes due, earliest first, letting each one's
   *  promise chain settle before the next. */
  async advance(ms: number): Promise<void> {
    const target = this.t + ms;
    for (;;) {
      let next: { id: number; at: number; fn: () => void } | undefined;
      for (const [id, tm] of this.timers) if (tm.at <= target && (!next || tm.at < next.at)) next = { id, ...tm };
      if (!next) break;
      this.timers.delete(next.id);
      this.t = Math.max(this.t, next.at);
      next.fn();
      await settle();
    }
    this.t = target;
    await settle();
  }
}

async function settle(): Promise<void> {
  for (let i = 0; i < 20; i++) await new Promise<void>((r) => setImmediate(r));
}

interface PassCall {
  opts: ReadOnlyModelPassOpts;
  resolve(text: string): void;
  reject(err: unknown): void;
}

function frozen(...ids: string[]): Milestone[] {
  return ids.map((id) => ({ id, title: `Title of ${id}` }) as Milestone);
}

function progress(inProgress: string[], completed: string[] = []): MilestoneProgress {
  return { completed, in_progress: inProgress };
}

function toolUse(agent: string, name: string, input: Record<string, unknown>, agentLabel?: string): EmittedMessage {
  return { kind: "tool_use", agent, ...(agentLabel ? { agentLabel } : {}), payload: { id: "t", name, input } };
}

const agentDispatch = (subagent: string, description: string): EmittedMessage =>
  toolUse("lead", "Agent", { subagent_type: subagent, description });

function usage(input: number, output: number, extra: Partial<AdviceUsageSnapshot[string]> = {}): AdviceUsageSnapshot {
  return {
    "claude-haiku-4-5-20251001": {
      inputTokens: input,
      outputTokens: output,
      cacheReadInputTokens: 0,
      cacheCreationInputTokens: 0,
      ...extra,
    },
  };
}

function harness(over: { claude?: boolean; codex?: boolean; issueRun?: boolean; enabled?: boolean; ignoreAbort?: boolean; redact?: (s: string) => string } = {}) {
  const clock = new FakeClock();
  const emitted: EmittedMessage[] = [];
  const calls: PassCall[] = [];
  const warns: string[] = [];
  const st = {
    enabled: over.enabled ?? true,
    life: new AbortController(),
    fence: undefined as string | undefined,
  };
  const steering = {
    lifecycleSignal: () => st.life.signal,
    claimFence: () => st.fence,
    nowSummaryEnabled: () => st.enabled,
  };
  const log = { ...nullLogger(), warn: (m: string) => void warns.push(m) };
  const deps: NowSummaryDeps = {
    steering,
    claim: {
      issueRun: over.issueRun ?? true,
      claude: over.codex ? false : (over.claude ?? true),
      ...(over.codex
        ? { codex: { runId: "run-1", binding: { authMode: "api_key", capability: "cap" } as never, buildHarness: (() => { throw new Error("the controller must go through runPass"); }) as never } }
        : {}),
    },
    emit: (m) => void emitted.push(m),
    ...(over.redact ? { redact: over.redact } : {}),
    runPass: (opts) =>
      new Promise<string>((resolve, reject) => {
        calls.push({ opts, resolve, reject });
        // A real pass rejects the moment its signal aborts; model that.
        if (!over.ignoreAbort) opts.signal?.addEventListener("abort", () => reject(new Error("now model call aborted")), { once: true });
      }),
    pass: { token: "tok", homeRoot: "/tmp", queryFn: (() => {
      throw new Error("the controller must go through runPass");
    }) as never },
    now: clock.now,
    setTimer: clock.setTimer,
    clearTimer: clock.clearTimer,
    log,
  };
  const c = new NowSummaryController(deps);
  const notes = (): Record<string, unknown>[] => emitted.filter((m) => m.kind === "progress_note").map((m) => m.payload);
  return { c, clock, st, calls, emitted, notes, warns };
}

/** Start the controller with milestone m1 active. */
async function running(h: ReturnType<typeof harness>, ...ids: string[]): Promise<void> {
  h.c.start();
  h.c.observeProgress(progress([ids[0] ?? "m1"]), frozen(...(ids.length ? ids : ["m1", "m2"])));
  // A call needs one tool frame of the new milestone (a fresh milestone clears the frame ring).
  h.c.observeFrame(toolUse("lead", "Read", { file_path: "first.md" }));
  await h.clock.advance(0);
}

describe("NowSummaryController: triggers and the rate window", () => {
  it("calls once the first milestone is active, with the cap, thinking, model and label fixed", async () => {
    const h = harness();
    await running(h);
    assert.equal(h.calls.length, 1);
    const o = h.calls[0]!.opts;
    assert.equal(o.model, "haiku");
    assert.equal(o.maxOutputTokens, 256);
    assert.equal(o.thinking, "disabled");
    assert.equal(o.label, "now");
    assert.equal(o.homePrefix, "uzi-now-");
    assert.equal(o.timeoutMs, 30_000);
    assert.equal(o.token, "tok");
    assert.equal(o.onResult, undefined, "onResult is never passed: nothing reaches the rate-limit observer");
    assert.ok(o.signal, "stop() needs the signal");
  });

  it("AC1: a reviewer lane then a tester lane yields a call within 5 minutes whose prompt carries the tester frames", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("Reviewing the change");
    await settle();
    h.c.observeFrame(agentDispatch("reviewer", "review the diff"));
    h.c.observeFrame(toolUse("reviewer", "Read", { file_path: "api/a.go" }, "review the diff"));
    await h.clock.advance(MIN);
    const firstAt = h.clock.t;
    // The tester lane takes over.
    h.c.observeFrame(agentDispatch("tester", "run the api gate"));
    h.c.observeFrame(toolUse("tester", "Bash", { command: "SECRET_CMD --token=abc", description: "Run the api gate" }, "run the api gate"));
    assert.equal(h.calls.length, 1, "inside the 5 minute window: coalesced, no call yet");
    await h.clock.advance(5 * MIN);
    assert.equal(h.calls.length, 2);
    const prompt = h.calls[1]!.opts.prompt;
    assert.match(prompt, /"role":"tester"/);
    assert.match(prompt, /Run the api gate/);
    assert.ok(!prompt.includes("SECRET_CMD"), "a Bash command never reaches the model");
    assert.ok(h.clock.t - firstAt <= 5 * MIN, "within 5 fake minutes of the lane change");
    h.calls[1]!.resolve("Testing the api gate");
    await settle();
    assert.deepEqual(h.notes().map((n) => n.text), ["Reviewing the change", "Testing the api gate"]);
  });

  it("coalesces several triggers inside the window into one call at the window's end", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    h.c.observeFrame(toolUse("coder", "Edit", { file_path: "b" }));
    h.c.observeFrame(toolUse("tester", "Grep", {}));
    await h.clock.advance(NOW_MIN_INTERVAL_MS - 1);
    assert.equal(h.calls.length, 1);
    await h.clock.advance(1);
    assert.equal(h.calls.length, 2);
    await h.clock.advance(NOW_MIN_INTERVAL_MS * 2);
    assert.equal(h.calls.length, 2, "nothing further pending");
  });

  it("a failed call counts against the window, is swallowed, and posts nothing", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.reject(new Error("boom"));
    await settle();
    assert.deepEqual(h.notes(), []);
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(NOW_MIN_INTERVAL_MS - 1);
    assert.equal(h.calls.length, 1, "the failure still used the 5 minute window");
    await h.clock.advance(1);
    assert.equal(h.calls.length, 2);
    assert.ok(h.warns.some((w) => w.includes("call failed")));
  });

  it("an empty answer is skipped and counts against the window", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("   ");
    await settle();
    assert.deepEqual(h.notes(), []);
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(NOW_MIN_INTERVAL_MS - 1);
    assert.equal(h.calls.length, 1);
  });

  it("a periodic trigger needs 10 minutes AND a new tool frame", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    await h.clock.advance(NOW_PERIODIC_MS + MIN);
    assert.equal(h.calls.length, 1, "no new tool frame: no call");
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 2);
  });

  it("a tool frame before the 10 minutes does not trigger", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(NOW_PERIODIC_MS - MIN);
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "b" }));
    await h.clock.advance(MIN - 1);
    assert.equal(h.calls.length, 1);
  });

  it("sends the active milestone title and only the newest 20 tool frames", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m1"]), frozen("m1", "m2"));
    for (let i = 0; i < 30; i++) h.c.observeFrame(toolUse("lead", "Read", { file_path: `f${i}` }));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(0);
    const prompt = h.calls[0]!.opts.prompt;
    assert.match(prompt, /Title of m1/);
    assert.equal((prompt.match(/"tool":"Read"/g) ?? []).length, NOW_MAX_FRAMES);
    assert.ok(prompt.includes('"f29"') || prompt.includes("f29"));
    assert.ok(!prompt.includes('"f9"'), "older frames are dropped");
  });
});

describe("NowSummaryController: Codex harness (PRD #2603 M2)", () => {
  const luna = (input: number, output: number, extra: Partial<AdviceUsageSnapshot[string]> = {}): AdviceUsageSnapshot => ({
    "gpt-6-luna": { inputTokens: input, outputTokens: output, cacheReadInputTokens: 0, cacheCreationInputTokens: 0, costStatus: "metered", ...extra },
  });

  it("calls through the Codex binding on gpt-6-luna at low effort with the 30 s timeout, never the Anthropic token", async () => {
    const h = harness({ codex: true });
    await running(h);
    assert.equal(h.calls.length, 1);
    const o = h.calls[0]!.opts;
    assert.equal(o.model, "gpt-6-luna");
    assert.equal(o.effort, "low");
    assert.equal(o.timeoutMs, 30_000);
    assert.equal(o.label, "now");
    assert.equal(o.token, undefined);
    assert.equal(o.codex?.runId, "run-1");
    assert.equal(o.codex?.refresh, "deny", "an advice-side refresh must never advance the run's credential generation");
  });

  it("AC1 and AC3: a lane change yields a note from the luna answer, whose prompt carries the active-lane frames and no Bash command", async () => {
    const h = harness({ codex: true });
    await running(h);
    h.calls[0]!.resolve("Reviewing the change");
    await settle();
    h.c.observeFrame(agentDispatch("tester", "run the api gate"));
    h.c.observeFrame(toolUse("tester", "Bash", { command: "SECRET_CMD --token=abc", description: "Run the api gate" }, "run the api gate"));
    await h.clock.advance(5 * MIN);
    assert.equal(h.calls.length, 2);
    const prompt = h.calls[1]!.opts.prompt;
    assert.match(prompt, /"role":"tester"/);
    assert.ok(!prompt.includes("SECRET_CMD"));
    h.calls[1]!.resolve("Testing the api gate");
    await settle();
    assert.deepEqual(h.notes().map((n) => n.text), ["Reviewing the change", "Testing the api gate"]);
  });

  it("posts the luna usage with the note exactly once", async () => {
    const h = harness({ codex: true });
    await running(h);
    h.calls[0]!.opts.onUsage?.(luna(120, 18, { costUSD: 0.0000211 }));
    h.calls[0]!.resolve("Running the api gate");
    await settle();
    const withUsage = h.notes().filter((n) => n.model_usage);
    assert.equal(withUsage.length, 1);
    assert.deepEqual((withUsage[0]!.model_usage as AdviceUsageSnapshot)["gpt-6-luna"], luna(120, 18, { costUSD: 0.0000211 })["gpt-6-luna"]);
  });

  it("a refused model or binding failure is swallowed, counts against the window, and leaves the run untouched", async () => {
    const h = harness({ codex: true });
    await running(h);
    h.calls[0]!.reject(new Error("now model call returned an error result"));
    await settle();
    assert.deepEqual(h.notes(), []);
    assert.ok(h.warns.some((w) => w.includes("call failed")));
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(NOW_MIN_INTERVAL_MS - 1);
    assert.equal(h.calls.length, 1, "the failed call still holds the 5 minute window");
    await h.clock.advance(1);
    assert.equal(h.calls.length, 2, "no fallback model: the next call is the same luna call");
    assert.equal(h.calls[1]!.opts.model, "gpt-6-luna");
  });

  it("abort after usage evidence posts the usage only, once; abort without evidence posts nothing", async () => {
    const a = harness({ codex: true });
    await running(a);
    a.calls[0]!.opts.signal!.addEventListener("abort", () => a.calls[0]!.opts.onUsage!(luna(50, 7)), { once: true });
    await a.c.stop();
    assert.equal(a.notes().length, 1);
    assert.equal(a.notes()[0]!.text, "");
    const b = harness({ codex: true });
    await running(b);
    await b.c.stop();
    assert.deepEqual(b.notes(), []);
  });
});

describe("NowSummaryController: gate", () => {
  it("makes no call when the setting is off, and starts once a poll turns it on", async () => {
    const h = harness({ enabled: false });
    await running(h);
    assert.equal(h.calls.length, 0);
    h.st.enabled = true;
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1);
  });

  it("a Codex run with no usable binding (none selected, factory unwired) makes no call", async () => {
    const h = harness({ claude: false });
    await running(h);
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(NOW_PERIODIC_MS * 2);
    assert.equal(h.calls.length, 0);
  });

  it("a non-issue run makes no call", async () => {
    const h = harness({ issueRun: false });
    await running(h);
    assert.equal(h.calls.length, 0);
  });

  it("makes no call without a frozen list or an active milestone", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 0, "no frozen list yet");
    h.c.observeProgress(progress([], ["m1"]), frozen("m1"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 0, "the only milestone is completed: no active one");
  });

  it("the active milestone is the first frozen id that is in progress and not completed", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m3", "m2"], ["m2"]), frozen("m1", "m2", "m3"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(0);
    assert.match(h.calls[0]!.opts.prompt, /Title of m3/);
  });

  it("waits while a hold is pending and calls once it ends", async () => {
    const h = harness();
    h.c.start();
    let release!: () => void;
    const held = h.c.hold(() => new Promise<void>((r) => (release = r)));
    h.c.observeProgress(progress(["m1"]), frozen("m1"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(MIN);
    assert.equal(h.calls.length, 0, "parked at a gate: no call");
    release();
    await held;
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1, "the trigger that arrived during the hold is served after it");
  });

  it("holds nest: the gate reopens only when the last one ends", async () => {
    const h = harness();
    h.c.start();
    let r1!: () => void;
    let r2!: () => void;
    const a = h.c.hold(() => new Promise<void>((r) => (r1 = r)));
    const b = h.c.hold(() => new Promise<void>((r) => (r2 = r)));
    h.c.observeProgress(progress(["m1"]), frozen("m1"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    r1();
    await a;
    await h.clock.advance(MIN);
    assert.equal(h.calls.length, 0);
    r2();
    await b;
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1);
  });

  it("a hold that throws still releases the gate", async () => {
    const h = harness();
    h.c.start();
    await assert.rejects(h.c.hold(() => Promise.reject(new Error("x"))), /x/);
    h.c.observeProgress(progress(["m1"]), frozen("m1"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1);
  });

  it("makes no call once the lifecycle signal aborted or the claim is fenced", async () => {
    const a = harness();
    a.st.life.abort();
    await running(a);
    assert.equal(a.calls.length, 0);
    const b = harness();
    b.st.fence = "released";
    await running(b);
    assert.equal(b.calls.length, 0);
  });

  it("an observer failure never reaches the caller", () => {
    const h = harness();
    h.c.start();
    // A malformed frame (no payload) must not throw into the run's emit path.
    assert.doesNotThrow(() => h.c.observeFrame({ kind: "tool_use" } as unknown as EmittedMessage));
    assert.doesNotThrow(() => h.c.observeProgress(null as unknown as MilestoneProgress, frozen("m1")));
  });
});

describe("NowSummaryController: the posted note", () => {
  it("posts {text, milestone_id, model_usage} from the worker, never an event or usage key", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.opts.onUsage?.(usage(10, 5, { costUSD: 0.001 }));
    h.calls[0]!.resolve("Running the api gate");
    await settle();
    assert.equal(h.emitted.length, 1);
    const m = h.emitted[0]!;
    assert.equal(m.kind, "progress_note");
    assert.equal(m.agent, "worker");
    assert.deepEqual(Object.keys(m.payload).sort(), ["milestone_id", "model_usage", "text"]);
    assert.equal(m.payload.text, "Running the api gate");
    assert.equal(m.payload.milestone_id, "m1");
    assert.equal("event" in m.payload || "usage" in m.payload, false);
  });

  it("strips control bytes and ANSI, folds whitespace and caps at 120 characters", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve(`\u001b[31mRunning\u0007\n  the   gate\u202e ${"x".repeat(300)}`);
    await settle();
    const text = h.notes()[0]!.text as string;
    assert.ok(!/[\p{Cc}\p{Cf}]/u.test(text));
    assert.ok(Array.from(text).length <= 120);
    assert.ok(text.startsWith("[31mRunning the gate"), `got ${text}`);
  });

  it("sanitizeNowText caps by code point, not UTF-16 unit", () => {
    assert.equal(Array.from(sanitizeNowText("\u{1F600}".repeat(200))).length, 120);
  });

  it("discards the text when the setting turns off mid-call, posts the usage only, and clears an earlier note", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("first note");
    await settle();
    assert.equal(h.notes().length, 1);
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(NOW_MIN_INTERVAL_MS);
    assert.equal(h.calls.length, 2);
    h.st.enabled = false;
    h.calls[1]!.opts.onUsage?.(usage(7, 3));
    h.calls[1]!.resolve("second note must not appear");
    await settle();
    const texts = h.notes().map((n) => n.text);
    assert.ok(!texts.includes("second note must not appear"));
    const usageOnly = h.notes().filter((n) => n.text === "" && n.model_usage);
    assert.equal(usageOnly.length, 1, "spend still counts");
    const clears = h.notes().filter((n) => n.text === "" && !n.model_usage);
    assert.equal(clears.length, 1, "one empty clear note so open pages drop the line");
    await h.clock.advance(NOW_PERIODIC_MS * 3);
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 2, "no further call while off");
    assert.equal(h.notes().filter((n) => n.text === "" && !n.model_usage).length, 1, "the clear is emitted once");
  });

  it("emits no clear note when no note was ever posted", async () => {
    const h = harness();
    await running(h);
    h.st.enabled = false;
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    assert.deepEqual(h.notes(), []);
  });

  it("discards the text when the milestone changed mid-call", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.opts.onUsage?.(usage(4, 2));
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    h.calls[0]!.resolve("about m1");
    await settle();
    assert.deepEqual(h.notes().map((n) => [n.text, n.milestone_id]), [["", "m1"]]);
  });

  it("discards the text when a park opened mid-call", async () => {
    const h = harness();
    await running(h);
    let release!: () => void;
    const held = h.c.hold(() => new Promise<void>((r) => (release = r)));
    h.calls[0]!.resolve("too late");
    await settle();
    assert.deepEqual(h.notes(), []);
    release();
    await held;
  });

  it("discards the text of a call in flight when a hold opened and closed before it settled, posting its usage once", async () => {
    const h = harness({ ignoreAbort: true });
    await running(h);
    await h.c.hold(async () => {});
    assert.equal(h.calls[0]!.opts.signal!.aborted, true);
    h.calls[0]!.opts.onUsage?.(usage(7, 3));
    h.calls[0]!.resolve("stale pre-hold text");
    await settle();
    const notes = h.notes();
    assert.deepEqual(notes.filter((n) => n.text !== ""), []);
    const usageNotes = notes.filter((n) => n.model_usage !== undefined);
    assert.equal(usageNotes.length, 1);
    assert.equal(usageNotes[0]!.text, "");
    assert.equal((usageNotes[0]!.model_usage as AdviceUsageSnapshot)["claude-haiku-4-5-20251001"]!.inputTokens, 7);
  });

  it("the setting turning off aborts a call in flight: its text is not published and its usage is posted once", async () => {
    const h = harness({ ignoreAbort: true });
    await running(h);
    h.st.enabled = false;
    h.c.observeSetting();
    assert.equal(h.calls[0]!.opts.signal!.aborted, true);
    h.calls[0]!.opts.onUsage?.(usage(5, 2));
    h.calls[0]!.resolve("stale text");
    await settle();
    const notes = h.notes();
    assert.deepEqual(notes.filter((n) => n.text !== ""), []);
    assert.equal(notes.filter((n) => n.model_usage !== undefined).length, 1);
  });

  it("posts nothing, not even the usage, once the claim was lost", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.opts.onUsage?.(usage(4, 2));
    h.st.fence = "stale";
    h.calls[0]!.resolve("text");
    await settle();
    assert.deepEqual(h.notes(), []);
  });

  it("a rate-limit error is swallowed and leaves the steering view untouched", async () => {
    const h = harness();
    const orig = h.st;
    await running(h);
    h.calls[0]!.reject(new LimitReachedError({ resetsAtMs: Date.now() + 1000, rateLimitType: "five_hour", detail: "x" }));
    await settle();
    assert.deepEqual(h.notes(), []);
    assert.equal(orig.enabled, true);
    assert.equal(orig.fence, undefined);
    assert.equal(orig.life.signal.aborted, false);
    // The controller keeps working afterwards.
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(NOW_MIN_INTERVAL_MS);
    assert.equal(h.calls.length, 2);
  });
});

describe("NowSummaryController: stop()", () => {
  it("abort after a usage frame posts exactly one usage-only note with those tokens and no cost, and no duplicate after a late settle", async () => {
    const h = harness();
    await running(h);
    const call = h.calls[0]!;
    // The pass delivers its evidence in its finally, which runs after the abort rejects it.
    call.opts.signal!.addEventListener("abort", () => call.opts.onUsage!(usage(120, 30)), { once: true });
    await h.c.stop();
    const usageNotes = h.notes();
    assert.equal(usageNotes.length, 1);
    assert.equal(usageNotes[0]!.text, "");
    assert.equal(usageNotes[0]!.milestone_id, "m1");
    const mu = usageNotes[0]!.model_usage as AdviceUsageSnapshot;
    const entry = mu["claude-haiku-4-5-20251001"]!;
    assert.equal(entry.inputTokens, 120);
    assert.equal(entry.outputTokens, 30);
    assert.equal("costUSD" in entry, false, "no provider cost on a pre-terminal snapshot");
    call.resolve("late text");
    call.opts.onUsage!(usage(999, 999));
    await settle();
    assert.equal(h.notes().length, 1, "a late settle posts nothing more");
  });

  it("abort without usage evidence posts nothing", async () => {
    const h = harness();
    await running(h);
    await h.c.stop();
    assert.deepEqual(h.notes(), []);
  });

  it("returns within the bound for a pass that never settles, and drops its late result", async () => {
    const h = harness({ ignoreAbort: true });
    await running(h);
    // The pass ignores the abort entirely: it never settles.
    const call = h.calls[0]!;
    let stopped = false;
    const p = h.c.stop().then(() => (stopped = true));
    await settle();
    assert.equal(stopped, false, "waits for the bounded cleanup");
    await h.clock.advance(NOW_STOP_BOUND_MS);
    await p;
    assert.equal(stopped, true);
    call.opts.onUsage?.(usage(5, 5));
    call.resolve("late");
    await settle();
    assert.deepEqual(h.notes(), [], "an abandoned call posts nothing");
  });

  it("never waits for the 30 second call timeout", async () => {
    const h = harness();
    await running(h);
    const before = h.clock.t;
    await h.c.stop();
    assert.ok(h.clock.t - before < 30_000);
  });

  it("stops scheduling for good", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    await h.c.stop();
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("coder", "Read", { file_path: "a" }));
    await h.clock.advance(NOW_PERIODIC_MS * 2);
    assert.equal(h.calls.length, 1);
    assert.equal(h.clock.pendingTimers, 0);
  });

  it("a credential switch (the lifecycle signal aborting) aborts the in-flight call", async () => {
    const h = harness();
    await running(h);
    assert.equal(h.calls[0]!.opts.signal!.aborted, false);
    h.st.life.abort();
    assert.equal(h.calls[0]!.opts.signal!.aborted, true);
  });
});

describe("trimToolFrame against the shared run-activity fixture", () => {
  interface FixtureFrame {
    kind: string;
    agent: string | null;
    agent_label: string | null;
    payload: Record<string, unknown>;
    seq: number;
  }
  interface FixtureCase {
    name: string;
    frames: FixtureFrame[];
    expected: { agent: string; agent_label: string; tool: string; detail: string } | null;
  }
  const url = new URL("../../fixtures/run-activity/cases.json", import.meta.url);
  const cases = (JSON.parse(readFileSync(url, "utf8")) as { cases: FixtureCase[] }).cases;

  for (const c of cases) {
    it(c.name, () => {
      let best: FixtureFrame | undefined;
      for (const f of c.frames) if (f.kind === "tool_use" && (!best || f.seq > best.seq)) best = f;
      if (c.expected === null) {
        assert.equal(best, undefined);
        return;
      }
      assert.ok(best);
      const got = trimToolFrame({
        kind: "tool_use",
        ...(best.agent ? { agent: best.agent } : {}),
        ...(best.agent_label ? { agentLabel: best.agent_label } : {}),
        payload: best.payload,
      });
      assert.deepEqual(got, {
        role: c.expected.agent,
        label: c.expected.agent_label,
        tool: c.expected.tool,
        detail: c.expected.detail,
      });
    });
  }

  it("ignores every other kind", () => {
    assert.equal(trimToolFrame({ kind: "tool_result", payload: {} }), undefined);
    assert.equal(trimToolFrame({ kind: "text", payload: { text: "x" } }), undefined);
  });
});

describe("holdContextCallbacks", () => {
  it("wraps every held callback in hold() and leaves absent ones absent", async () => {
    const depths: number[] = [];
    let depth = 0;
    const controller = {
      hold: async <T>(fn: () => Promise<T>): Promise<T> => {
        depth++;
        try {
          return await fn();
        } finally {
          depth--;
        }
      },
    };
    const ctx: Record<string, unknown> = {};
    for (const name of HELD_CONTEXT_CALLBACKS) {
      if (name === "cacheCapBoundary") continue; // absent on purpose
      ctx[name] = async () => {
        depths.push(depth);
        return name;
      };
    }
    holdContextCallbacks(ctx as unknown as RunContext, controller);
    assert.equal("cacheCapBoundary" in ctx, false);
    for (const name of HELD_CONTEXT_CALLBACKS) {
      if (name === "cacheCapBoundary") continue;
      assert.equal(await (ctx[name] as () => Promise<string>)(), name);
    }
    assert.equal(depths.length, HELD_CONTEXT_CALLBACKS.length - 1);
    assert.ok(depths.every((d) => d === 1), "each callback ran inside a hold");
    assert.equal(depth, 0);
  });

  it("covers exactly the callbacks the plan names", () => {
    assert.deepEqual([...HELD_CONTEXT_CALLBACKS].sort(), [
      "askCompletionQuestion",
      "askPlanMissing",
      "askUser",
      "attemptCredentialSwitch",
      "awaitFollowUp",
      "cacheCapBoundary",
      "continueExistingPlanGate",
      "enterCompletionHold",
      "gatePlan",
      "parkForPause",
      "parkForWall",
      "takeResumedGateEvent",
    ]);
  });
});

describe("NowSummaryController: review-rework behaviours", () => {
  it("a milestone change drops the previous milestone's frames and waits for a new tool frame", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m1"]), frozen("m1", "m2"));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "OLD_MILESTONE_FILE" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1);
    h.calls[0]!.resolve("one");
    await settle();
    await h.clock.advance(NOW_MIN_INTERVAL_MS);
    // m2 becomes active and the window is long over, but no frame of m2 has arrived yet.
    h.c.observeProgress(progress(["m2"], ["m1"]));
    await h.clock.advance(MIN);
    assert.equal(h.calls.length, 1, "no new tool frame since the change: no spend");
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "NEW_MILESTONE_FILE" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 2);
    const prompt = h.calls[1]!.opts.prompt;
    assert.match(prompt, /Title of m2/);
    assert.match(prompt, /NEW_MILESTONE_FILE/);
    assert.ok(!prompt.includes("OLD_MILESTONE_FILE"), "the previous milestone's frames are gone");
  });

  it("a milestone change discards the unsent frame count: no call until a frame of the new milestone", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    // More m1 frames arrive after the call, inside the window, then m2 becomes active.
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "OLD_M1_FRAME" }));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "OLD_M1_FRAME_2" }));
    h.c.observeProgress(progress(["m2"], ["m1"]));
    await h.clock.advance(NOW_MIN_INTERVAL_MS + MIN);
    assert.equal(h.calls.length, 1, "the window elapsed but no m2 frame has arrived: no call");
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "M2_FRAME" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 2);
    const prompt = h.calls[1]!.opts.prompt;
    assert.match(prompt, /M2_FRAME/);
    assert.ok(!prompt.includes("OLD_M1_FRAME"), "only m2 frames");
  });

  it("the 5 minute floor is literal: 5*60000-1 ms later still one call, one more ms makes two", async () => {
    const h = harness();
    await running(h);
    h.calls[0]!.resolve("one");
    await settle();
    h.c.observeProgress(progress(["m2"], ["m1"]));
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "next.md" }));
    await h.clock.advance(5 * 60_000 - 1);
    assert.equal(h.calls.length, 1);
    await h.clock.advance(1);
    assert.equal(h.calls.length, 2);
  });

  it("stop() after the bound posts the usage the abandoned call already delivered, unless the claim is fenced", async () => {
    for (const fenced of [false, true]) {
      const h = harness({ ignoreAbort: true });
      await running(h);
      h.calls[0]!.opts.onUsage?.(usage(40, 9));
      if (fenced) h.st.fence = "fenced";
      const p = h.c.stop();
      await settle();
      await h.clock.advance(NOW_STOP_BOUND_MS);
      await p;
      const notes = h.notes();
      if (fenced) {
        assert.deepEqual(notes, []);
        continue;
      }
      assert.equal(notes.length, 1);
      assert.equal(notes[0]!.text, "");
      assert.equal(notes[0]!.milestone_id, "m1");
      assert.equal(((notes[0]!.model_usage as AdviceUsageSnapshot)["claude-haiku-4-5-20251001"] as { inputTokens: number }).inputTokens, 40);
      h.calls[0]!.opts.onUsage?.(usage(500, 500));
      h.calls[0]!.resolve("late");
      await settle();
      assert.equal(h.notes().length, 1, "the late result adds nothing");
    }
  });

  it("a fallback frozen list never replaces the list the controller already holds", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m1"]), frozen("m1", "m2"));
    // The turn-boundary report carries the claim's (different) list as a fallback only.
    h.c.observeProgress(progress(["m2"]), [{ id: "zz", title: "claim list" }] as Milestone[], true);
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(0);
    assert.match(h.calls[0]!.opts.prompt, /Title of m2/, "m2 is still resolved against the held list");
  });

  it("a fallback list is used while the controller has none", async () => {
    const h = harness();
    h.c.start();
    h.c.observeProgress(progress(["m1"]), frozen("m1"), true);
    h.c.observeFrame(toolUse("lead", "Read", { file_path: "a" }));
    await h.clock.advance(0);
    assert.equal(h.calls.length, 1);
  });

  describe("redaction runs before the caps", () => {
    const secret = ["fixture", "redact", "marker"].join("-");
    const redact = (s: string): string => s.split(secret).join("[redacted]");
    const fragment = secret.slice(0, 12);

    it("trimToolFrame redacts a secret straddling the 200 rune cut in every field", () => {
      const pad = "x".repeat(192);
      const f = trimToolFrame(
        { kind: "tool_use", agent: pad + secret, agentLabel: pad + secret, payload: { name: "Read", input: { file_path: pad + secret } } },
        redact,
      )!;
      for (const v of [f.role, f.label, f.detail]) assert.ok(!v.includes(fragment.slice(0, 6)), "no prefix of the secret survives the cut");
      assert.ok(trimToolFrame({ kind: "tool_use", payload: { name: "Read", input: { file_path: pad + secret } } })!.detail.includes(fragment.slice(0, 6)), "without a redactor the cut leaves the prefix (the premise)");
    });

    it("trimToolFrame redacts the tool field before the cap", () => {
      const f = trimToolFrame({ kind: "tool_use", payload: { name: "x".repeat(192) + secret, input: {} } }, redact)!;
      assert.ok(!f.tool.includes(fragment.slice(0, 6)), "no prefix of the secret survives the cut");
      assert.ok(trimToolFrame({ kind: "tool_use", payload: { name: "x".repeat(192) + secret, input: {} } })!.tool.includes(fragment.slice(0, 6)), "without a redactor the prefix survives (the premise)");
    });

    it("sanitizeNowText redacts before the 120 rune cap", () => {
      const out = sanitizeNowText("y".repeat(112) + secret, redact);
      assert.ok(!out.includes(fragment.slice(0, 6)));
    });

    it("the controller redacts the title, the frames and the posted text", async () => {
      const h = harness({ redact });
      h.c.start();
      h.c.observeProgress(progress(["m1"]), [{ id: "m1", title: "t".repeat(192) + secret }] as Milestone[]);
      h.c.observeFrame(toolUse("lead", "Read", { file_path: "z".repeat(192) + secret }));
      await h.clock.advance(0);
      assert.ok(!h.calls[0]!.opts.prompt.includes(fragment.slice(0, 6)));
      h.calls[0]!.resolve("w".repeat(110) + secret);
      await settle();
      const text = String(h.notes()[0]!.text);
      assert.ok(text !== "" && !text.includes(fragment.slice(0, 6)));
    });
  });
});
