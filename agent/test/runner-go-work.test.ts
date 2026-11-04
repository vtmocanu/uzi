import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Options as SdkOptions } from "@anthropic-ai/claude-agent-sdk";
import { type ExecutorResult, type RunContext, StubExecutor } from "../src/executor.js";
import { type ExecutorFactory } from "../src/runner.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import type { GoWorkFsOps } from "../src/go-work.js";
import type { Logger } from "../src/log.js";
import { nullLogger, recordingLogger, makeClaim } from "./helpers.js";
import { LimitReachedError } from "../src/limit.js";
import { DiskParkSignal } from "../src/cache-cap.js";
import { api, client, fakeGitlab, fx, gitlabClaim, input, installHarness, planThenDoneQuery, runnerWith, simulateCommittedWork } from "./runner-harness.js";

installHarness();

// Issue #3870 M2: the runner prepares `<runHome>/.uzi-go-work/g<claimGeneration>` for a Claude flight,
// hands it to the lead CLI as GOTMPDIR, removes older generations' leaves on a later claim after a
// complete HOME reap, and falls back to launching without GOTMPDIR on any ordinary creation failure.

const UNAVAILABLE = "managed Go work dir unavailable; launching without GOTMPDIR";
const HAS_PROC_FD = fs.existsSync("/proc/self/fd");

interface Launch {
  options: SdkOptions[];
  /** The `.uzi-go-work` entries (and whether GOTMPDIR was a directory) when the SDK was first queried:
   *  a completed run tears its HOME down, so the live state is only observable at launch. */
  atLaunch: { entries: string[] | null; gotmpdirIsDir: boolean };
  home: string;
  lines: unknown[];
}

/** Run one claim through the real SdkExecutor with a recording fake queryFn. */
async function launch(
  iid: number,
  generation: number,
  homeRoot: string,
  opts: { prepare?: (home: string) => void; fsOps?: GoWorkFsOps; reap?: boolean } = {},
): Promise<Launch> {
  const { gitlab } = fakeGitlab();
  const options: SdkOptions[] = [];
  const inner = planThenDoneQuery();
  let atLaunch: Launch["atLaunch"] = { entries: null, gotmpdirIsDir: false };
  const queryFn: SdkQueryFn = (params) => {
    if (options.length === 0) {
      const dir = params.options.env?.GOTMPDIR;
      atLaunch = {
        entries: home && fs.existsSync(path.join(home, ".uzi-go-work")) ? fs.readdirSync(path.join(home, ".uzi-go-work")).sort() : null,
        gotmpdirIsDir: dir !== undefined && fs.existsSync(dir) && fs.statSync(dir).isDirectory(),
      };
    }
    options.push(params.options);
    return inner(params);
  };
  let home = "";
  const factory: ExecutorFactory = (id) => {
    home = path.join(homeRoot, id);
    if (opts.prepare) {
      fs.mkdirSync(home, { recursive: true });
      opts.prepare(home);
    }
    const sdk = new SdkExecutor(nullLogger(), home, { queryFn });
    return {
      homeDir: home,
      executor: {
        run: (ctx: RunContext): Promise<ExecutorResult> => sdk.run(ctx),
        ...(opts.reap ? { reapAttributedProcesses: async () => ({ killed: [], left: [], complete: true }) } : {}),
      },
    };
  };
  const { logger, lines } = recordingLogger();
  simulateCommittedWork();
  const runId = `38700000-0000-4000-8000-${String(iid).padStart(12, "0")}`;
  api.setInputs(runId, [input("approve_plan")]);
  await runnerWith(factory, gitlab, undefined, logger, opts.fsOps ? { goWorkTestDeps: { fsOps: opts.fsOps } } : {}).execute(
    gitlabClaim(iid, { run_id: runId, claim_generation: generation }),
  );
  return { options, atLaunch, home, lines };
}

const unavailableReasons = (lines: unknown[]): unknown[] =>
  (lines as Array<{ msg?: string; reason?: unknown }>).filter((l) => l.msg === UNAVAILABLE).map((l) => l.reason);

const completed = (runId: string): boolean => api.states.some((s) => s.runId === runId && s.body.status === "completed");
const lastRunId = (iid: number): string => `38700000-0000-4000-8000-${String(iid).padStart(12, "0")}`;

function withRoot(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-3870-"));
  return fn(homeRoot).finally(() => fs.rmSync(homeRoot, { recursive: true, force: true }));
}

const failing = (code: string) => async (): Promise<never> => {
  throw Object.assign(new Error(code), { code });
};

describe("RunRunner: managed Go work dir (issue #3870 M2)", { skip: process.platform !== "linux" }, () => {
  it("(a) distinct runs and generations get GOTMPDIR = <runHome>/.uzi-go-work/g<gen>, and the dir exists", () =>
    withRoot(async (homeRoot) => {
      const one = await launch(3871, 2, homeRoot);
      const two = await launch(3872, 3, homeRoot);
      assert.ok(one.options.length > 0 && two.options.length > 0, "precondition: both runs reached the SDK");
      assert.equal(one.options[0]!.env?.GOTMPDIR, path.join(one.home, ".uzi-go-work", "g2"));
      assert.equal(two.options[0]!.env?.GOTMPDIR, path.join(two.home, ".uzi-go-work", "g3"));
      assert.notEqual(one.home, two.home);
      assert.ok(one.atLaunch.gotmpdirIsDir && two.atLaunch.gotmpdirIsDir, "the leaf existed when the SDK was launched");
      assert.deepEqual(unavailableReasons([...one.lines, ...two.lines]), []);
      assert.ok(completed(lastRunId(3871)) && completed(lastRunId(3872)));
    }));

  it("(b) generation 0 launches without GOTMPDIR, logs the diagnostic, and the run completes", () =>
    withRoot(async (homeRoot) => {
      const r = await launch(3873, 0, homeRoot);
      assert.ok(r.options.length > 0);
      assert.equal(Object.hasOwn(r.options[0]!.env ?? {}, "GOTMPDIR"), false);
      assert.deepEqual(unavailableReasons(r.lines), ["no_generation"]);
      assert.equal(r.atLaunch.entries, null, "nothing was created");
      assert.ok(completed(lastRunId(3873)), "the run is not failed or parked");
    }));

  it("(c) a pre-planted .uzi-go-work symlink falls back and is left untouched", () =>
    withRoot(async (homeRoot) => {
      const elsewhere = path.join(homeRoot, "elsewhere");
      fs.mkdirSync(elsewhere);
      const r = await launch(3874, 2, homeRoot, { prepare: (home) => fs.symlinkSync(elsewhere, path.join(home, ".uzi-go-work")) });
      assert.equal(Object.hasOwn(r.options[0]!.env ?? {}, "GOTMPDIR"), false);
      assert.equal(unavailableReasons(r.lines).length, 1);
      assert.deepEqual(fs.readdirSync(elsewhere), [], "nothing was created through the symlink");
      assert.equal(r.atLaunch.gotmpdirIsDir, false);
      assert.ok(completed(lastRunId(3874)));
    }));

  for (const [label, fsOps, reason] of [
    ["mkdir", { mkdir: failing("EACCES") }, "parent_create_failed"],
    ["open", { open: failing("EIO") }, "home_unavailable"],
    ["chmod", { chmod: failing("EPERM") }, "parent_create_failed"],
  ] as const) {
    it(`(d) an injected ${label} failure falls back without GOTMPDIR and the run completes`, () =>
      withRoot(async (homeRoot) => {
        const iid = 3880 + ["mkdir", "open", "chmod"].indexOf(label);
        const r = await launch(iid, 2, homeRoot, { fsOps });
        assert.equal(Object.hasOwn(r.options[0]!.env ?? {}, "GOTMPDIR"), false);
        assert.deepEqual(unavailableReasons(r.lines), [reason]);
        assert.ok(completed(lastRunId(iid)));
      }));
  }

  it("(e) claim generation 5 removes older leaf g3, keeps g7, and creates g5 after a complete reap", (t) => {
    if (!HAS_PROC_FD) return t.skip("no /proc/self/fd: the descriptor-pinned removal refuses here by design");
    return withRoot(async (homeRoot) => {
      const r = await launch(3875, 5, homeRoot, {
        reap: true,
        prepare: (home) => {
          const parent = path.join(home, ".uzi-go-work");
          for (const leaf of ["g3", "g7"]) {
            fs.mkdirSync(path.join(parent, leaf, "go-build1"), { recursive: true });
            fs.writeFileSync(path.join(parent, leaf, "go-build1", "x.o"), "x");
            fs.chmodSync(path.join(parent, leaf), 0o700);
          }
          fs.chmodSync(parent, 0o700);
        },
      });
      assert.deepEqual(r.atLaunch.entries, ["g5", "g7"], "g3 removed, g7 kept, g5 created");
      assert.equal(r.options[0]!.env?.GOTMPDIR, path.join(r.home, ".uzi-go-work", "g5"));
      assert.ok(r.atLaunch.gotmpdirIsDir);
      assert.ok(completed(lastRunId(3875)));
    });
  });

  it("(e2) without an attributed reap (no reapAttributedProcesses) older leaves are not removed", (t) => {
    if (!HAS_PROC_FD) return t.skip("no /proc/self/fd");
    return withRoot(async (homeRoot) => {
      const r = await launch(3876, 5, homeRoot, {
        prepare: (home) => {
          const parent = path.join(home, ".uzi-go-work");
          fs.mkdirSync(path.join(parent, "g3"), { recursive: true });
          fs.chmodSync(path.join(parent, "g3"), 0o700);
          fs.chmodSync(parent, 0o700);
        },
      });
      assert.deepEqual(r.atLaunch.entries, ["g3", "g5"], "no reap ran, so g3 is still there");
      assert.equal(r.options[0]!.env?.GOTMPDIR, path.join(r.home, ".uzi-go-work", "g5"));
    });
  });

  it("(g) the self-improve finalize install receives GOTMPDIR when the run has a HOME", () =>
    withRoot(async (homeRoot) => {
      const bin = path.join(homeRoot, "fake-bin");
      fs.mkdirSync(bin, { recursive: true });
      const seen = path.join(homeRoot, "npm-gotmpdir");
      // A fake `npm` that records the GOTMPDIR it was launched with.
      fs.writeFileSync(path.join(bin, "npm"), ["#!/bin/sh", `printf '%s' "\${GOTMPDIR-unset}" > '${seen}'`, ""].join("\n"), { mode: 0o755 });
      let home = "";
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        const stub = new StubExecutor(nullLogger());
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              fs.writeFileSync(path.join(ctx.worktreePath, "package.json"), '{"name":"x","version":"1.0.0"}\n');
              fs.writeFileSync(path.join(ctx.worktreePath, "package-lock.json"), "{}\n");
              const result = await stub.run(ctx);
              return { ...result, toolEnv: { PATH: bin } };
            },
          },
        };
      };
      const claim = makeClaim({
        run_id: "38700000-0000-4000-8000-000000003877",
        claim_generation: 4,
        kind: "self_improve",
        self_improve_dogfood: true,
        issue_iid: 77,
        issue_title: "Self-improvement cycle",
        issue_description: "Pick one top improvement and land it.",
        base_branch: "main",
        repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
        last_seq: 0,
        secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
      });
      const { gitlab } = fakeGitlab();
      const log: Logger = nullLogger();
      await runnerWith(factory, gitlab, undefined, log, {
        checkRunner: async (check) => ({ name: check.name, status: "skipped", detail: "not run" }),
      }).execute(claim);
      assert.equal(fs.readFileSync(seen, "utf8"), path.join(home, ".uzi-go-work", "g4"));
    }));

  // Codex controls: a Codex executor's `safety` is only populated inside run(), so the launch gate must
  // exclude it by the signals known beforehand (`sandboxesCommands`, `claim.secrets.codex`).
  const CODEX_CREDENTIAL = {
    auth_mode: "subscription",
    access_token: "fixture-codex-access-token-3870",
    capability: "fixture-codex-capability-3870",
    generation: 3,
    chatgpt_account_id: "verified-account",
    chatgpt_plan_type: null,
  } as const;

  async function codexControl(iid: number, homeRoot: string, opts: { sandboxes: boolean; codexSecret: boolean }): Promise<{ ctxDir: string | undefined; seenRun: boolean; leafExisted: boolean; home: string }> {
    const { gitlab } = fakeGitlab();
    let home = "";
    let ctxDir: string | undefined = "unset";
    let seenRun = false;
    let leafExisted = true;
    const factory: ExecutorFactory = (id) => {
      home = path.join(homeRoot, id);
      const stub = new StubExecutor(nullLogger());
      return {
        homeDir: home,
        executor: {
          ...(opts.sandboxes ? { sandboxesCommands: true } : {}),
          run: (ctx: RunContext): Promise<ExecutorResult> => {
            seenRun = true;
            ctxDir = ctx.managedGoWorkDir;
            leafExisted = fs.existsSync(path.join(home, ".uzi-go-work"));
            return stub.run(ctx);
          },
        },
      };
    };
    simulateCommittedWork();
    const runId = lastRunId(iid);
    const base = gitlabClaim(iid, { run_id: runId, claim_generation: 2 });
    const claim = opts.codexSecret ? { ...base, secrets: { ...base.secrets, codex: CODEX_CREDENTIAL } } : base;
    await runnerWith(factory, gitlab, undefined, nullLogger()).execute(claim);
    return { ctxDir, seenRun, leafExisted, home };
  }

  it("(h) an executor that sandboxes its commands gets no managed Go work leaf", () =>
    withRoot(async (homeRoot) => {
      const r = await codexControl(3890, homeRoot, { sandboxes: true, codexSecret: false });
      assert.ok(r.seenRun, "precondition: the executor ran");
      assert.equal(r.ctxDir, undefined);
      assert.equal(r.leafExisted, false, "no .uzi-go-work was created in the run HOME");
    }));

  it("(i) a claim carrying secrets.codex gets no managed Go work leaf", () =>
    withRoot(async (homeRoot) => {
      const r = await codexControl(3891, homeRoot, { sandboxes: false, codexSecret: true });
      assert.ok(r.seenRun, "precondition: the executor ran");
      assert.equal(r.ctxDir, undefined);
      assert.equal(r.leafExisted, false, "no .uzi-go-work was created in the run HOME");
    }));
});

// Issue #3870 M3 (D5): park routing. A park that ENDED the run's process removes every managed Go work
// leaf after the complete attributed reap (the process-ended park, and the disk park before its capture);
// a park that keeps a live or re-claimable executor keeps them. The `.uzi-go-work` parent itself stays.

const PARENT = ".uzi-go-work";
const PARK_CACHES = [".cache/go-build", "go/pkg/mod", ".npm/_cacache"];
const PARK_KEPT = [".claude.json", path.join(".claude", "projects", "-data-runner-clone", "sid.jsonl")];

function seedParkHome(home: string): void {
  for (const rel of [...PARK_CACHES.map((c) => path.join(c, "x")), ...PARK_KEPT]) {
    fs.mkdirSync(path.dirname(path.join(home, rel)), { recursive: true });
    fs.writeFileSync(path.join(home, rel), "data\n");
  }
}

/** A leaf with a Go-like build tree inside it. */
function writeLeaf(home: string, name: string): string {
  const leaf = path.join(home, PARENT, name);
  fs.mkdirSync(path.join(leaf, "go-build1", "b001"), { recursive: true });
  fs.writeFileSync(path.join(leaf, "go-build1", "b001", "x.o"), "obj");
  return leaf;
}

const leavesOf = (home: string): string[] => (fs.existsSync(path.join(home, PARENT)) ? fs.readdirSync(path.join(home, PARENT)).sort() : []);
const cachesPresent = (home: string): boolean[] => PARK_CACHES.map((rel) => fs.existsSync(path.join(home, rel)));
const keptPresent = (home: string): boolean[] => PARK_KEPT.map((rel) => fs.existsSync(path.join(home, rel)));
const parkStatus = (runId: string): string | undefined =>
  api.states.find((s) => s.runId === runId && (s.body.status === "limit_wait" || s.body.status === "recovery_wait"))?.body.status;
const cleanupLines = (lines: unknown[], msg: string, reason: string): unknown[] =>
  (lines as Array<{ msg?: string; reason?: unknown }>).filter((l) => l.msg === msg && l.reason === reason);

interface ParkOpts {
  reap?: "complete" | "left" | "incomplete";
  /** Runs inside the executor after the HOME is seeded; `leaf` is this generation's managed leaf. */
  inRun?: (home: string, leaf: string) => void;
  /** Park with a disk park signal instead of a usage limit. */
  disk?: boolean;
}
interface Parked {
  home: string;
  leaf: string;
  runId: string;
  lines: unknown[];
  /** `flight.sourceJoinFailed` as the source use ends, after the park drops. */
  sourceJoinFailed: boolean | undefined;
  /** Whether this generation's leaf still existed when the disk park's capture began. */
  leafAtCapture: boolean | undefined;
}

async function park(iid: number, generation: number, homeRoot: string, opts: ParkOpts = {}): Promise<Parked> {
  const { gitlab } = fakeGitlab();
  let home = "";
  let leaf = "";
  const factory: ExecutorFactory = (id) => {
    home = path.join(homeRoot, id);
    leaf = path.join(home, PARENT, `g${generation}`);
    return {
      homeDir: home,
      executor: {
        run: async (): Promise<ExecutorResult> => {
          seedParkHome(home);
          opts.inRun?.(home, leaf);
          throw opts.disk ? new DiskParkSignal(true) : new LimitReachedError({ resetsAtMs: Date.now() + 5 * 3600_000, rateLimitType: "five_hour" });
        },
        killAgentTree: () => undefined,
        ...(opts.reap
          ? { reapAttributedProcesses: async () => ({ killed: [], left: opts.reap === "left" ? [4242] : [], complete: opts.reap !== "incomplete" }) }
          : {}),
      },
    };
  };
  if (opts.disk) client.protocolFeatures = ["recovery_cause_data_volume_full", "claim_generation_fence"];
  const { logger, lines } = recordingLogger();
  const runner = runnerWith(factory, gitlab, undefined, logger);
  const spy = runner as unknown as Record<string, (...a: unknown[]) => Promise<unknown>>;
  const out: Parked = { home: "", leaf: "", runId: lastRunId(iid), lines, sourceJoinFailed: undefined, leafAtCapture: undefined };
  let ended = false;
  const end = spy.endFlightSourceUse!.bind(runner);
  spy.endFlightSourceUse = async (...a) => {
    ended = true;
    out.sourceJoinFailed = (a[0] as { sourceJoinFailed?: boolean }).sourceJoinFailed;
    return end(...a);
  };
  const capture = spy.captureRecoveryRestorePoint!.bind(runner);
  spy.captureRecoveryRestorePoint = async (...a) => {
    out.leafAtCapture ??= fs.existsSync(leaf);
    return capture(...a);
  };
  await runner.execute(gitlabClaim(iid, { run_id: out.runId, claim_generation: generation, wait_on_limit: true }));
  assert.ok(ended, "precondition: the flight's source use ended after the park drops");
  out.home = home;
  out.leaf = leaf;
  return out;
}

describe("RunRunner: park routing of the managed Go work leaves (issue #3870 M3)", { skip: process.platform !== "linux" || !HAS_PROC_FD }, () => {
  it("(p1) a usage-limit park after a complete reap removes every leaf, drops the caches, keeps the resume state and the parent", () =>
    withRoot(async (homeRoot) => {
      const p = await park(3890, 3, homeRoot, { reap: "complete", inRun: (home) => writeLeaf(home, "g1") });
      assert.equal(parkStatus(p.runId), "limit_wait", "precondition: parked at limit_wait");
      assert.deepEqual(leavesOf(p.home), [], "the older leaf and this generation's leaf are both gone");
      assert.ok(fs.existsSync(path.join(p.home, PARENT)), "the parent stays");
      assert.deepEqual(cachesPresent(p.home), [false, false, false]);
      assert.deepEqual(keptPresent(p.home), [true, true]);
      assert.equal(cleanupLines(p.lines, "managed Go work dir cleanup", "process_ended_park").length, 1);
    }));

  for (const [label, reap] of [
    ["left a live process", "left"],
    ["was incomplete", "incomplete"],
  ] as const) {
    it(`(p2) a park whose attributed reap ${label} keeps the leaves and the caches`, () =>
      withRoot(async (homeRoot) => {
        // Generation 1: a first claim skips the claim-time HOME reap, which would otherwise fail the run.
        const p = await park(label === "was incomplete" ? 3892 : 3891, 1, homeRoot, { reap, inRun: (home) => writeLeaf(home, "g7") });
        assert.equal(parkStatus(p.runId), "limit_wait");
        assert.deepEqual(leavesOf(p.home), ["g1", "g7"]);
        assert.deepEqual(cachesPresent(p.home), [true, true, true]);
        assert.deepEqual(cleanupLines(p.lines, "managed Go work dir cleanup", "process_ended_park"), []);
      }));
  }

  it("(p3) a worker-shutdown interrupt keeps the leaves", () =>
    withRoot(async (homeRoot) => {
      const { gitlab } = fakeGitlab();
      let home = "";
      let started!: () => void;
      const startedP = new Promise<void>((r) => (started = r));
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedParkHome(home);
              writeLeaf(home, "g1");
              started();
              await new Promise<void>((_, reject) => ctx.signal!.addEventListener("abort", () => reject(new Error("aborted mid-run")), { once: true }));
              return { branch: ctx.branch };
            },
            reapAttributedProcesses: async () => ({ killed: [], left: [], complete: true }),
          },
        };
      };
      const runner = runnerWith(factory, gitlab);
      const claim = gitlabClaim(3893, { run_id: lastRunId(3893), claim_generation: 3, wait_on_limit: true });
      const done = runner.execute(claim);
      await startedP;
      runner.shutdown();
      await done;
      assert.deepEqual(leavesOf(home), ["g1", "g3"]);
      assert.deepEqual(cachesPresent(home), [true, true, true]);
    }));

  it("(p3) a plan-gate park with its executor still live keeps the leaves", () =>
    withRoot(async (homeRoot) => {
      const { gitlab } = fakeGitlab();
      let home = "";
      const stub = new StubExecutor(nullLogger(), { planGate: true });
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedParkHome(home);
              writeLeaf(home, "g1");
              return stub.run(ctx);
            },
            reapAttributedProcesses: async () => ({ killed: [], left: [], complete: true }),
          },
        };
      };
      const claim = gitlabClaim(3894, { run_id: lastRunId(3894), claim_generation: 3 });
      const execution = runnerWith(factory, gitlab, undefined, undefined, { planApprovalTimeoutMs: 60_000 }).execute(claim);
      const deadline = Date.now() + 10_000;
      while (!api.states.some((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval")) {
        assert.ok(Date.now() < deadline, "precondition: the run parked at the plan gate");
        await new Promise((r) => setTimeout(r, 5));
      }
      await new Promise((r) => setTimeout(r, 300));
      assert.deepEqual(leavesOf(home), ["g1", "g3"]);
      assert.deepEqual(cachesPresent(home), [true, true, true]);
      api.setInputs(claim.run_id, [input("approve_plan")]);
      await execution;
    }));

  it("(p4) a scratch removal failure leaves sourceJoinFailed as a clean park has it; a cache drop failure still sets it", () =>
    withRoot(async (homeRoot) => {
      const control = await park(3895, 3, homeRoot, { reap: "complete" });
      assert.equal(parkStatus(control.runId), "limit_wait");
      assert.ok(!control.sourceJoinFailed, "precondition: a clean park does not set sourceJoinFailed");

      // The leaf is replaced by a symlink: the pinned removal refuses it, so the removal fails.
      const elsewhere = path.join(homeRoot, "elsewhere");
      fs.mkdirSync(path.join(elsewhere, "keep"), { recursive: true });
      const scratch = await park(3896, 3, homeRoot, {
        reap: "complete",
        inRun: (_home, leaf) => {
          fs.rmSync(leaf, { recursive: true, force: true });
          fs.symlinkSync(elsewhere, leaf);
        },
      });
      assert.equal(parkStatus(scratch.runId), "limit_wait");
      assert.equal(cleanupLines(scratch.lines, "managed Go work dir cleanup incomplete", "process_ended_park").length, 1, "the removal reported failure");
      assert.ok(fs.lstatSync(scratch.leaf).isSymbolicLink(), "the symlink was not followed or removed");
      assert.ok(fs.existsSync(path.join(elsewhere, "keep")), "nothing outside the HOME was touched");
      assert.deepEqual(cachesPresent(scratch.home), [false, false, false], "the caches were still dropped");
      assert.ok(!scratch.sourceJoinFailed, "a scratch failure is not a source join failure");

      // Counterpart: a failed cache drop does set it (the scratch is still removed: independent).
      const cacheOther = path.join(homeRoot, "cache-elsewhere");
      fs.mkdirSync(cacheOther);
      const cache = await park(3897, 3, homeRoot, {
        reap: "complete",
        inRun: (home) => {
          fs.rmSync(path.join(home, "go", "pkg", "mod"), { recursive: true, force: true });
          fs.mkdirSync(path.join(home, "go", "pkg"), { recursive: true });
          fs.symlinkSync(cacheOther, path.join(home, "go", "pkg", "mod"));
        },
      });
      assert.equal(cache.sourceJoinFailed, true, "a cache drop failure still sets sourceJoinFailed");
      assert.deepEqual(leavesOf(cache.home), [], "the scratch removal ran regardless of the cache drop");
    }));

  it("(p5) a disk park removes the leaves BEFORE its capture", () =>
    withRoot(async (homeRoot) => {
      const p = await park(3898, 3, homeRoot, { reap: "complete", disk: true, inRun: (home) => writeLeaf(home, "g1") });
      assert.equal(parkStatus(p.runId), "recovery_wait", "precondition: the disk park was taken");
      assert.equal(p.leafAtCapture, false, "this generation's leaf was gone when the capture began");
      assert.deepEqual(leavesOf(p.home), []);
      assert.ok(fs.existsSync(path.join(p.home, PARENT)));
      assert.deepEqual(cachesPresent(p.home), [false, false, false]);
      assert.equal(cleanupLines(p.lines, "managed Go work dir cleanup", "disk_park_pre_capture").length, 1);
    }));

  it("(p6) a terminal run removes its whole HOME, the Go work parent included", () =>
    withRoot(async (homeRoot) => {
      const { gitlab } = fakeGitlab();
      let home = "";
      let leafSeen = false;
      const factory: ExecutorFactory = (id) => {
        home = path.join(homeRoot, id);
        return {
          homeDir: home,
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              seedParkHome(home);
              leafSeen = fs.existsSync(path.join(home, PARENT, "g3"));
              return { branch: ctx.branch };
            },
          },
        };
      };
      await runnerWith(factory, gitlab).execute(gitlabClaim(3899, { run_id: lastRunId(3899), claim_generation: 3 }));
      assert.ok(leafSeen, "precondition: the managed leaf existed during the run");
      assert.equal(fs.existsSync(home), false);
    }));
});
