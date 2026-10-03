import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { type Executor, type ExecutorResult, type RunContext, StubExecutor } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { LimitReachedError } from "../src/limit.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { dropRunCaches } from "../src/run-caches.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  fakeGitlab,
  git,
  gitlabClaim,
  input,
  installHarness,
  planThenDoneQuery,
  runner,
  runnerWith,
  simulateCommittedWork,
} from "./runner-harness.js";

installHarness();

// PRD #1809 M1 (D2): a park that ends the run's process drops the rebuildable caches
// (`.cache/go-build`, `go/pkg/mod`, `.npm/_cacache`) from the preserved HOME and keeps
// everything else a resume needs. On the base code a parked HOME kept every cache, so the
// first test is the regression: it fails there on the three "cache dropped" assertions.
// This host runs single-uid; the exact uid-split ownership (runner-owned `0555` module
// cache under a worker-owned HOME) is the opt-in `e2e/home-uid-split/` fixture's subtree case.

const SID = "aaaaaaaa-bbbb-cccc-dddd-000000001809";
const PROJECT = "-data-runner-clone";
const TRANSCRIPT_REL = path.join(".claude", "projects", PROJECT, `${SID}.jsonl`);
const TRANSCRIPT = '{"type":"user","message":"the session before the park"}\n';

const CACHES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
/** Resume state, `go/bin`, and files this code has never heard of: all must survive a drop. */
const KEPT = [
  TRANSCRIPT_REL,
  path.join(".claude", "projects", PROJECT, SID, "subagents", "agent-1.jsonl"),
  path.join(".claude", "todos", "t.json"),
  ".claude.json",
  path.join("go", "bin", "gopls"),
  path.join(".cache", "unknown-tool", "state"),
  "unknown.txt",
];

/** A HOME shaped like a Go + npm run's: the three caches (the module cache with the Go
 *  toolchain's read-only `0555` package dirs) beside the files a resume needs. */
function seedHome(home: string): void {
  const mod = path.join(home, "go", "pkg", "mod", "gopkg.in", "inf.v0@v0.9.1");
  fs.mkdirSync(mod, { recursive: true });
  fs.writeFileSync(path.join(mod, "dec.go"), "package inf\n");
  fs.mkdirSync(path.join(home, "go", "pkg", "mod", "cache", "download"), { recursive: true });
  fs.mkdirSync(path.join(home, ".cache", "go-build", "0a"), { recursive: true });
  fs.writeFileSync(path.join(home, ".cache", "go-build", "0a", "0a1b-d"), "x".repeat(4096));
  fs.mkdirSync(path.join(home, ".npm", "_cacache", "index-v5"), { recursive: true });
  fs.writeFileSync(path.join(home, ".npm", "_cacache", "index-v5", "entry"), "{}");
  for (const rel of KEPT) {
    fs.mkdirSync(path.dirname(path.join(home, rel)), { recursive: true });
    fs.writeFileSync(path.join(home, rel), rel === TRANSCRIPT_REL ? TRANSCRIPT : "keep\n");
  }
  // Last, so no later mkdir under them undoes the mode.
  fs.chmodSync(mod, 0o555);
  fs.chmodSync(path.dirname(mod), 0o555);
}

function assertCachesDropped(home: string): void {
  for (const rel of CACHES) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), false, `${rel} must be dropped on a process-ending park`);
  }
}

function assertCachesKept(home: string): void {
  for (const rel of CACHES) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), true, `${rel} must be kept`);
  }
}

function assertResumeStateKept(home: string): void {
  for (const rel of KEPT) {
    assert.strictEqual(fs.existsSync(path.join(home, rel)), true, `${rel} must survive`);
  }
  assert.strictEqual(fs.readFileSync(path.join(home, TRANSCRIPT_REL), "utf8"), TRANSCRIPT);
}

/** The test's own cleanup must not leak a `0555` tree. */
function forceRm(dir: string): void {
  const widen = (p: string): void => {
    let st;
    try {
      st = fs.lstatSync(p);
    } catch {
      return;
    }
    if (!st.isDirectory()) return;
    fs.chmodSync(p, 0o700);
    for (const e of fs.readdirSync(p)) widen(path.join(p, e));
  };
  widen(dir);
  fs.rmSync(dir, { recursive: true, force: true });
}

describe("RunRunner — cache drop on a process-ending park (PRD #1809 M1)", () => {
  it("a usage-limit park drops the caches, keeps the resume state, and the resume continues the parked session", async (t) => {
    // The drop pins descriptors through /proc/self/fd and refuses without it (rmHomeSubtree).
    if (!fs.existsSync("/proc/self/fd")) return t.skip("no /proc/self/fd on this host: the cache drop refuses here by design");
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-park-"));
    try {
      const iid = 1809;
      const runId = "18090000-0000-4000-8000-000000001809";
      let home = "";
      const parkFactory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (): Promise<ExecutorResult> => {
              seedHome(home);
              throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
            },
          },
        };
      };
      await runnerWith(parkFactory, gitlab).execute(gitlabClaim(iid, { run_id: runId, wait_on_limit: true }));
      assert.ok(
        api.states.some((s) => s.runId === runId && s.body.status === "limit_wait"),
        "precondition: the run parked at limit_wait",
      );
      const modeBefore = fs.lstatSync(home).mode;
      assertCachesDropped(home);
      assertResumeStateKept(home);
      assert.strictEqual(fs.lstatSync(home).mode, modeBefore, "the HOME root's mode is untouched");

      // The resume continues the ACTUAL session. The runner's transcript preflight hands the
      // executor the parked session id only while `<sid>.jsonl` is still under the HOME
      // (otherwise it drops it and starts fresh), and the REAL SdkExecutor then asks the SDK
      // to resume exactly that session with HOME pointing at the preserved dir, which is
      // where the CLI reads the transcript back from. The fake queryFn records both.
      const queries: SdkOptions[] = [];
      const inner = planThenDoneQuery();
      const queryFn: SdkQueryFn = (params) => {
        queries.push(params.options);
        return inner(params);
      };
      let seenSession: string | null | undefined;
      const resumeFactory: ExecutorFactory = (id) => {
        const sdk = new SdkExecutor(nullLogger(), path.join(homeRoot, id), { queryFn });
        return {
          homeDir: path.join(homeRoot, id),
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seenSession = ctx.sessionId;
              return sdk.run(ctx);
            },
          },
        };
      };
      simulateCommittedWork();
      api.setInputs(runId, [input("approve_plan")]);
      await runnerWith(resumeFactory, gitlab).execute(
        gitlabClaim(iid, { run_id: runId, wait_on_limit: true, session_id: SID, last_seq: 1000 }),
      );
      assert.strictEqual(seenSession, SID, "the runner hands the resume the parked session id (transcript found)");
      assert.ok(queries.length > 0, "precondition: the SDK was queried on the resume");
      assert.strictEqual(queries[0]!.resume, SID, "the first SDK turn resumes the parked session");
      assert.strictEqual(queries[0]!.env?.HOME, home, "the SDK resolves that session under the preserved HOME");
      assert.ok(
        api.states.some((s) => s.runId === runId && s.body.status === "completed"),
        "the resumed run completed",
      );
    } finally {
      forceRm(homeRoot);
    }
  });

  it("a process-ending park reaps the run's attributed processes BEFORE its cache drop (N-a)", async (t) => {
    if (!fs.existsSync("/proc/self/fd")) return t.skip("no /proc/self/fd on this host: the cache drop refuses here by design");
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-reap-"));
    try {
      let home = "";
      const events: string[] = [];
      let cachesAtReap: boolean[] = [];
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (): Promise<ExecutorResult> => {
              seedHome(home);
              throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
            },
            killAgentTree: () => events.push("kill"),
            reapAttributedProcesses: async () => {
              events.push("reap");
              cachesAtReap = CACHES.map((rel) => fs.existsSync(path.join(home, rel)));
              return { killed: [], left: [], complete: true };
            },
          },
        };
      };
      const runId = "18090000-0000-4000-8000-000000001813";
      await runnerWith(factory, gitlab).execute(gitlabClaim(1813, { run_id: runId, wait_on_limit: true }));
      assert.ok(api.states.some((s) => s.runId === runId && s.body.status === "limit_wait"), "precondition: parked");
      assert.ok(events.includes("reap"), `the attributed reap ran (${JSON.stringify(events)})`);
      assert.deepStrictEqual(cachesAtReap, [true, true, true], "it ran while the caches were still there: before the drop");
      assertCachesDropped(home);
      assertResumeStateKept(home);
    } finally {
      forceRm(homeRoot);
    }
  });

  for (const [label, reapResult] of [
    ["left a live process", async () => ({ killed: [], left: [4242], complete: true })],
    ["was incomplete", async () => ({ killed: [], left: [], complete: false })],
    ["rejected", async () => Promise.reject(new Error("reap exploded"))],
  ] as const) {
    it(`a process-ending park keeps the caches when the attributed reap ${label} (#1828)`, async (t) => {
      if (!fs.existsSync("/proc/self/fd")) return t.skip("no fd dir on this host: the cache drop refuses here by design");
      const { gitlab } = fakeGitlab();
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1828-keep-"));
      try {
        let home = "";
        const factory: ExecutorFactory = (id) => {
          home = path.join(homeRoot, id);
          return {
            homeDir: home,
            executor: {
              run: async (): Promise<ExecutorResult> => {
                seedHome(home);
                throw new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
              },
              killAgentTree: () => undefined,
              reapAttributedProcesses: reapResult,
            },
          };
        };
        const runId = "18090000-0000-4000-8000-000000001828";
        await runnerWith(factory, gitlab).execute(gitlabClaim(1828, { run_id: runId, wait_on_limit: true, claim_generation: 1 }));
        assert.ok(api.states.some((s) => s.runId === runId && s.body.status === "limit_wait"), "precondition: parked");
        assertCachesKept(home);
        assertResumeStateKept(home);
      } finally {
        forceRm(homeRoot);
      }
    });
  }

  it("the finalize security reap awaits the attributed reap BEFORE the PAT-bearing push (N-a)", async () => {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    const events: string[] = [];
    const exec: Executor = {
      run: async (ctx) => ({ branch: ctx.branch }),
      killAgentTree: () => events.push("kill"),
      reapAttributedProcesses: async () => {
        // Resolves on a later tick: a push that did not await it would be recorded first.
        await new Promise((r) => setTimeout(r, 20));
        events.push("reap");
        return { killed: [], left: [], complete: true };
      },
    };
    const origPush = git.pushBranch.bind(git);
    (git as unknown as { pushBranch: unknown }).pushBranch = async (...args: unknown[]) => {
      events.push("push");
      return (origPush as (...a: unknown[]) => Promise<void>)(...args);
    };
    try {
      await runner(exec, gitlab).execute(gitlabClaim(1814));
    } finally {
      (git as unknown as { pushBranch: unknown }).pushBranch = origPush;
    }
    const first = (e: string) => events.indexOf(e);
    assert.ok(first("reap") >= 0 && first("push") >= 0, JSON.stringify(events));
    assert.ok(first("kill") < first("reap"), `the group kill, then the attributed reap (${JSON.stringify(events)})`);
    assert.ok(first("reap") < first("push"), `the attributed reap finished before the push (${JSON.stringify(events)})`);
  });

  it("a worker-shutdown interrupt (preserveSession only) keeps the caches", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-shut-"));
    try {
      let home = "";
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              started();
              await new Promise<void>((_, reject) =>
                ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }),
              );
              return { branch: ctx.branch };
            },
          },
        };
      };
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(1810, { wait_on_limit: true });
      const done = runner.execute(claim);
      await startedP;
      runner.shutdown();
      await done;
      assert.strictEqual(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"),
        false,
        "precondition: a shutdown requeues, it does not fail",
      );
      assertCachesKept(home);
      assertResumeStateKept(home);
    } finally {
      forceRm(homeRoot);
    }
  });

  it("a plan-gate park with its executor still live keeps the caches", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-gate-"));
    try {
      // Calibrate the settle below on a real drop over the same HOME shape on this host, so
      // a drop started at the gate (even fire-and-forget) would have finished, and logged,
      // well within it; a fixed short sleep let such a drop pass unseen.
      const probe = path.join(homeRoot, "calibrate");
      seedHome(probe);
      const t0 = Date.now();
      await dropRunCaches(probe, nullLogger());
      const settleMs = Math.max(500, 3 * (Date.now() - t0));
      const { logger, lines } = recordingLogger();
      const dropLines = () =>
        lines.filter((l) => /run cache/.test((l as { msg: string }).msg)).map((l) => (l as { msg: string }).msg);
      let home = "";
      let returned = false;
      const stub = new StubExecutor(nullLogger(), { planGate: true });
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              try {
                return await stub.run(ctx);
              } finally {
                returned = true;
              }
            },
          },
        };
      };
      const claim = gitlabClaim(1812);
      const execution = runnerWith(factory, gitlab, undefined, logger, { planApprovalTimeoutMs: 60_000 }).execute(claim);
      const deadline = Date.now() + 10_000;
      while (!api.states.some((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval")) {
        assert.ok(Date.now() < deadline, "precondition: the run parked at the plan gate");
        await new Promise((r) => setTimeout(r, 5));
      }
      // Let any park-side work settle while the gate is still open.
      await new Promise((r) => setTimeout(r, settleMs));
      assert.strictEqual(returned, false, "precondition: the executor is still live at the gate");
      assertCachesKept(home);
      assertResumeStateKept(home);
      assert.deepStrictEqual(dropLines(), [], "no cache drop ran at the gate");
      api.setInputs(claim.run_id, [input("approve_plan")]);
      await execution;
      assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"));
      // A completed run removes its whole HOME instead; no drop runs on the way either.
      await new Promise((r) => setTimeout(r, settleMs));
      assert.deepStrictEqual(dropLines(), [], "no cache drop ran at any point of a gate-then-complete run");
    } finally {
      forceRm(homeRoot);
    }
  });

  it("dropRunCaches: a spent deadline skips every subtree with a warning and keeps the caches", async () => {
    const home = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-deadline-"));
    try {
      seedHome(home);
      const { logger, lines } = recordingLogger();
      await dropRunCaches(home, logger, { deadlineMs: 0 });
      assertCachesKept(home);
      assertResumeStateKept(home);
      const warn = lines.find((l) => (l as { msg: string }).msg.includes("ran out of time")) as
        | { level: string; skipped: string[] }
        | undefined;
      assert.ok(warn, `a warning names the skipped subtrees: ${JSON.stringify(lines)}`);
      assert.strictEqual(warn.level, "warn");
      assert.deepStrictEqual(warn.skipped, CACHES);
      const info = lines.find((l) => (l as { msg: string }).msg === "run caches dropped on park") as
        | { dropped: string[]; skipped: string[] }
        | undefined;
      assert.deepStrictEqual(info?.dropped, []);
      assert.deepStrictEqual(info?.skipped, CACHES);
    } finally {
      forceRm(home);
    }
  });

  it("a terminal run still removes its whole HOME, caches included", async () => {
    const { gitlab } = fakeGitlab();
    const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-1809-term-"));
    try {
      let home = "";
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedHome(home);
              return { branch: ctx.branch };
            },
          },
        };
      };
      await runnerWith(factory, gitlab).execute(gitlabClaim(1811));
      assert.ok(home !== "", "precondition: the executor ran");
      assert.strictEqual(fs.existsSync(home), false, "a terminal run's HOME is removed as before");
    } finally {
      forceRm(homeRoot);
    }
  });
});
