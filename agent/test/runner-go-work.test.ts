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
import { api, fakeGitlab, fx, gitlabClaim, input, installHarness, planThenDoneQuery, runnerWith, simulateCommittedWork } from "./runner-harness.js";

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
});
