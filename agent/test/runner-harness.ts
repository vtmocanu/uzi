import { afterEach, beforeEach } from "node:test";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { SDKMessage } from "@anthropic-ai/claude-agent-sdk";
import { FakeApi } from "./fake-api.js";
import { makeFixture, type Fixture } from "./fixture-repo.js";
import { makeClaim, nullLogger } from "./helpers.js";
import { WorkerClient } from "../src/client.js";
import { GitCache } from "../src/git.js";
import {
  type Executor,
  type RunContext,
  type ExecutorResult,
} from "../src/executor.js";
import { type SdkQueryFn } from "../src/sdk-executor.js";
import {
  GitLabClient,
  ForgejoClient,
  GitHubClient,
  type FetchFn,
} from "../src/forge.js";
import {
  RunRunner,
  type ExecutorFactory,
  type RunnerOptions,
} from "../src/runner.js";
import type { PlanVerdict } from "../src/steering.js";
import type { Logger } from "../src/log.js";
import type { UserInput } from "../src/protocol.js";

export const TOKEN = "tkn-runner-123456";

export let api: FakeApi;
export let baseUrl: string;
export let fx: Fixture;
export let git: GitCache;
export let client: WorkerClient;
export let homeDir: string;

/**
 * Register the per-test harness. Call ONCE at the top level of each
 * runner-*.test.ts. The exported bindings above are ES live bindings, so a
 * test body reads whatever this hook most recently assigned -- which is why
 * splitting runner.test.ts needed no edits inside any test body.
 */
export function installHarness(): void {
  beforeEach(async () => {
    api = new FakeApi(TOKEN);
    baseUrl = await api.listen();
    fx = makeFixture();
    git = new GitCache(fx.dataDir, nullLogger());
    homeDir = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-runnerhome-"));
    client = new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
      sleep: async () => {},
      terminalRetrySchedule: [1, 1],
    });
  });

  afterEach(async () => {
    await api.close();
    fx.cleanup();
    fs.rmSync(homeDir, { recursive: true, force: true });
  });
}

// PRD #51 M3 (b): the run's working tree is the RUNNER CLONE under runner/, not a
// linked worktree under worktrees/.
export function worktreeDirFor(iid: number): string {
  const repoDir = path
    .basename(git.barePathFor(fx.originPath))
    .replace(/\.git$/, "");
  return path.join(fx.dataDir, "runner", repoDir, `issue-${iid}`);
}

export function isAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

export async function waitDead(pid: number, timeoutMs = 2000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (isAlive(pid) && Date.now() < deadline)
    await new Promise((r) => setTimeout(r, 10));
}

export interface MrCall {
  url: string;
  method: string;
  headers: Record<string, string>;
  body?: string;
}

/** A GitLab client whose transport is captured; opens MR !42 with no network. */
export function fakeGitlab(): { gitlab: GitLabClient; calls: MrCall[] } {
  const calls: MrCall[] = [];
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({
      url,
      method: init.method,
      headers: init.headers,
      body: init.body,
    });
    return {
      status: 201,
      text: async () =>
        JSON.stringify({
          iid: 42,
          web_url: "https://gitlab.example.test/org/repo/-/merge_requests/42",
        }),
    };
  };
  return { gitlab: new GitLabClient({ fetchFn }), calls };
}

/** A Forgejo client whose transport is captured; opens PR #42 with no network. */
export function fakeForgejo(): { forgejo: ForgejoClient; calls: MrCall[] } {
  const calls: MrCall[] = [];
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({
      url,
      method: init.method,
      headers: init.headers,
      body: init.body,
    });
    return {
      status: 201,
      text: async () =>
        JSON.stringify({
          number: 42,
          html_url: "https://forgejo.example.test/org/repo/pulls/42",
        }),
    };
  };
  return { forgejo: new ForgejoClient({ fetchFn }), calls };
}

/** A GitHub client whose transport is captured; opens PR #42 with no network. */
export function fakeGitHub(): { github: GitHubClient; calls: MrCall[] } {
  const calls: MrCall[] = [];
  const fetchFn: FetchFn = async (url, init) => {
    calls.push({
      url,
      method: init.method,
      headers: init.headers,
      body: init.body,
    });
    return {
      status: 201,
      text: async () =>
        JSON.stringify({
          number: 42,
          html_url: "https://github.com/org/repo/pull/42",
        }),
    };
  };
  return { github: new GitHubClient({ fetchFn }), calls };
}

export function runner(
  executor: Executor,
  gitlab: GitLabClient,
  joinToken?: string,
  extra?: Partial<RunnerOptions>,
): RunRunner {
  // Wrap the single executor as a factory (PRD #42): each execute() gets it back.
  // The per-run-executor tests below inject a real per-run factory instead.
  return runnerWith(() => ({ executor }), gitlab, joinToken, nullLogger(), extra);
}

export function runnerWith(
  makeExecutor: ExecutorFactory,
  gitlab: GitLabClient,
  joinToken?: string,
  log: Logger = nullLogger(),
  extra?: Partial<RunnerOptions>,
): RunRunner {
  return new RunRunner(client, git, makeExecutor, log, 20, joinToken, {
    pollMs: 5,
    planApprovalTimeoutMs: 0, // disabled — the gate resolves from injected inputs
    // PRD #88 D-W. NOT a speed knob, and deliberately NOT 0 (which disables the
    // deadline the way planApprovalTimeoutMs above is disabled). A clarification park
    // blocks by design, so every way of breaking it — an autopilot run that stops
    // short-circuiting, an id that stops being re-used — makes the park never resolve.
    // On the 24h production default that surfaces as a wall-clock TEST TIMEOUT, and
    // `node --test` prints `ℹ fail 0` while timing out (CLAUDE.md, measured twice on
    // PRD #121). So the mutation controls for the ask_user tests below would have
    // "reddened" in exactly the shape this repo warns is unreadable. A bounded
    // deadline turns each of those hangs into a NAMED failure: the run fails with
    // REASON_QUESTION_TIMEOUT and the assertion that fires says which property broke.
    // Generous relative to the 5ms poll, so it is never reached while an answer is
    // actually being delivered.
    questionTimeoutMs: 600,
    gitlab,
    // Spread LAST so a test can override checkpointIntervalMs / now (PRD #267) and any
    // other RunnerOptions field. Optional: existing call sites pass nothing.
    ...extra,
  });
}

export const gitlabClaim = (iid: number, overrides = {}) =>
  makeClaim({
    issue_iid: iid,
    issue_title: `Fix thing ${iid}`,
    repo: {
      id: "r1",
      url: "https://gitlab.example.test/org/repo",
      clone_url: fx.originPath,
    },
    last_seq: 0,
    secrets: {
      forge_pat: "fixture-forge-pat-000000",
      anthropic_oauth_token: "dummy-oauth-do-not-scan",
    },
    ...overrides,
  });


// --- end-to-end: the real SdkExecutor driven through the runner --------------

export function assistant(content: unknown[], sessionId = "sess-e2e"): SDKMessage {
  return {
    type: "assistant",
    session_id: sessionId,
    message: { content },
  } as unknown as SDKMessage;
}
export function resultOk(sessionId = "sess-e2e"): SDKMessage {
  return {
    type: "result",
    subtype: "success",
    is_error: false,
    num_turns: 1,
    session_id: sessionId,
  } as unknown as SDKMessage;
}
/** A queryFn that yields the plan turn, then the done turn. */
export function planThenDoneQuery(): SdkQueryFn {
  const scripts: SDKMessage[][] = [
    [
      assistant([
        {
          type: "tool_use",
          id: "p",
          name: "mcp__uzi__submit_plan",
          input: { plan_md: "# PLAN\n- do it" },
        },
      ]),
      resultOk(),
    ],
    [
      assistant([
        { type: "text", text: "done implementing" },
        { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
      ]),
      resultOk(),
    ],
  ];
  let i = 0;
  return (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      for await (const _ of params.prompt) {
        /* drain */
      }
      for (const m of script) yield m;
    })();
  };
}

/** Like planThenDoneQuery, but the submit_plan call carries a `milestones` list
 *  (PRD #122 M1). Passing `undefined` omits the field entirely, proving the
 *  back-compat "no milestones" shape. */
export function planWithMilestonesThenDoneQuery(
  milestones: Array<{ id: string; title: string }> | undefined,
): SdkQueryFn {
  const planInput: Record<string, unknown> = { plan_md: "# PLAN\n- do it" };
  if (milestones !== undefined) planInput["milestones"] = milestones;
  const scripts: SDKMessage[][] = [
    [
      assistant([
        { type: "tool_use", id: "p", name: "mcp__uzi__submit_plan", input: planInput },
      ]),
      resultOk(),
    ],
    [
      assistant([
        { type: "text", text: "done implementing" },
        { type: "tool_use", id: "d", name: "mcp__uzi__signal_done", input: {} },
      ]),
      resultOk(),
    ],
  ];
  let i = 0;
  return (params) => {
    const script = scripts[Math.min(i, scripts.length - 1)]!;
    i++;
    return (async function* () {
      for await (const _ of params.prompt) {
        /* drain */
      }
      for (const m of script) yield m;
    })();
  };
}

export function input(kind: UserInput["kind"], body?: string): UserInput {
  return { id: 1, kind, body: body ?? null };
}



export type PlanVerdictKind = PlanVerdict["kind"];

// --- PRD #42 M1: per-run executor + HOME isolation + secret eviction ----------

export interface Deferred {
  promise: Promise<void>;
  resolve: () => void;
}
export function deferred(): Deferred {
  let resolve!: () => void;
  const promise = new Promise<void>((r) => (resolve = () => r()));
  return { promise, resolve };
}

/** Resolves once `n` participants have arrived (idempotent past n). */
export function barrier(n: number): { arrive: () => void; ready: Promise<void> } {
  let count = 0;
  const d = deferred();
  return {
    arrive: () => {
      if (++count >= n) d.resolve();
    },
    ready: d.promise,
  };
}

/**
 * A fake executor modelling the B1 reap: run() "spawns" its injected pids into a
 * PRIVATE set (mirroring SdkExecutor.spawnedPids), signals it is mid-run, then
 * blocks on a gate so the test can hold two runs live at once; killAgentTree reaps
 * ITS set into a shared log and clears it. One instance per run (from the factory)
 * is the whole point — a single shared instance would let one run's reap clear
 * another's set (the pre-#42 hazard).
 */
export class FakeReapExecutor implements Executor {
  private readonly live = new Set<number>();
  constructor(
    private readonly runId: string,
    private readonly injected: readonly number[],
    private readonly killLog: Array<{ runId: string; pid: number }>,
    private readonly onSpawned: () => void,
    private readonly gate: Promise<void>,
  ) {}
  async run(ctx: RunContext): Promise<ExecutorResult> {
    for (const p of this.injected) this.live.add(p);
    this.onSpawned();
    await this.gate;
    return { branch: ctx.branch };
  }
  killAgentTree(): void {
    for (const pid of this.live) this.killLog.push({ runId: this.runId, pid });
    this.live.clear();
  }
  livePids(): number[] {
    return [...this.live].sort((a, b) => a - b);
  }
}

/** A logger that records every secret registered/evicted, for the eviction tests. */
export function secretRecordingLogger(): {
  logger: Logger;
  added: string[];
  removed: string[];
} {
  const added: string[] = [];
  const removed: string[] = [];
  const self: Logger = {
    debug() {},
    info() {},
    warn() {},
    error() {},
    addSecret: (s) => {
      added.push(s);
    },
    removeSecret: (s) => {
      removed.push(s);
    },
    child: () => self,
  };
  return { logger: self, added, removed };
}
