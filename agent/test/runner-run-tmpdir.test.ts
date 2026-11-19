import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { type ExecutorResult, type RunContext, StubExecutor } from "../src/executor.js";
import type { ExecutorFactory } from "../src/runner.js";
import type { RunTmpdirPrepared } from "../src/run-tmpdir.js";
import type { Logger } from "../src/log.js";
import { nullLogger, recordingLogger, makeClaim } from "./helpers.js";
import { fakeGitlab, fx, gitlabClaim, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";

installHarness();

// Issue #4420: the runner gate that decides whether a flight gets a run-owned TMPDIR leaf.
// Claude flights on Linux get ctx.runTmpdir; Codex and command-sandboxing executors never do.

const UNAVAILABLE = "run TMPDIR unavailable; launching with the shared TMPDIR";
const runIdOf = (iid: number): string => `44200000-0000-4000-8000-${String(iid).padStart(12, "0")}`;
const LEAF = "/data/runner/.tmp/run-44200000-0000-4000-8000-000000000001";
const CODEX_CREDENTIAL = {
  auth_mode: "subscription",
  access_token: "fixture-codex-access-token-4420",
  capability: "fixture-codex-capability-4420",
  generation: 3,
  chatgpt_account_id: "verified-account",
  chatgpt_plan_type: null,
} as const;

function withRoot(fn: (homeRoot: string) => Promise<void>): Promise<void> {
  const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-4420-"));
  return fn(homeRoot).finally(() => fs.rmSync(homeRoot, { recursive: true, force: true }));
}

async function launch(
  iid: number,
  homeRoot: string,
  opts: { sandboxes?: boolean; codexSecret?: boolean; prepared: RunTmpdirPrepared },
): Promise<{ ctxDir: string | undefined; seenRun: boolean; prepareCalls: string[]; lines: unknown[] }> {
  const { gitlab } = fakeGitlab();
  const prepareCalls: string[] = [];
  let ctxDir: string | undefined = "unset";
  let seenRun = false;
  const factory: ExecutorFactory = (id) => {
    const stub = new StubExecutor(nullLogger());
    return {
      homeDir: path.join(homeRoot, id),
      executor: {
        ...(opts.sandboxes ? { sandboxesCommands: true } : {}),
        run: (ctx: RunContext): Promise<ExecutorResult> => {
          seenRun = true;
          ctxDir = ctx.runTmpdir;
          return stub.run(ctx);
        },
      },
    };
  };
  simulateCommittedWork();
  const base = gitlabClaim(iid, { run_id: runIdOf(iid), claim_generation: 2 });
  const claim = opts.codexSecret ? { ...base, secrets: { ...base.secrets, codex: CODEX_CREDENTIAL } } : base;
  const { logger, lines } = recordingLogger();
  await runnerWith(factory, gitlab, undefined, logger, {
    runTmpdirTestDeps: { prepare: async (runId: string) => { prepareCalls.push(runId); return opts.prepared; } },
  }).execute(claim);
  return { ctxDir, seenRun, prepareCalls, lines };
}

const unavailableReasons = (lines: unknown[]): unknown[] =>
  (lines as Array<{ msg?: string; reason?: unknown }>).filter((l) => l.msg === UNAVAILABLE).map((l) => l.reason);

describe("RunRunner: run-owned TMPDIR gate (issue #4420)", { skip: process.platform !== "linux" }, () => {
  it("(a) a Claude flight gets ctx.runTmpdir set to the prepared dir, with no warning", () =>
    withRoot(async (homeRoot) => {
      const r = await launch(4421, homeRoot, { prepared: { dir: LEAF } });
      assert.ok(r.seenRun, "precondition: the executor ran");
      assert.deepEqual(r.prepareCalls, [runIdOf(4421)]);
      assert.equal(r.ctxDir, LEAF);
      assert.deepEqual(unavailableReasons(r.lines), []);
    }));

  it("(a2) a skipped preparation launches without runTmpdir and logs the reason token", () =>
    withRoot(async (homeRoot) => {
      const r = await launch(4422, homeRoot, { prepared: { skipped: true, reason: "layout_refused" } });
      assert.ok(r.seenRun);
      assert.equal(r.ctxDir, undefined);
      assert.deepEqual(unavailableReasons(r.lines), ["layout_refused"]);
    }));

  it("(b) a claim carrying secrets.codex never prepares a run TMPDIR", () =>
    withRoot(async (homeRoot) => {
      const r = await launch(4423, homeRoot, { codexSecret: true, prepared: { dir: LEAF } });
      assert.ok(r.seenRun);
      assert.deepEqual(r.prepareCalls, []);
      assert.equal(r.ctxDir, undefined);
    }));

  it("(c) an executor that sandboxes its commands never prepares a run TMPDIR", () =>
    withRoot(async (homeRoot) => {
      const r = await launch(4424, homeRoot, { sandboxes: true, prepared: { dir: LEAF } });
      assert.ok(r.seenRun);
      assert.deepEqual(r.prepareCalls, []);
      assert.equal(r.ctxDir, undefined);
    }));

  it("(d) the self-improve finalize install receives flight.runTmpdir as TMPDIR", () =>
    withRoot(async (homeRoot) => {
      const bin = path.join(homeRoot, "fake-bin");
      fs.mkdirSync(bin, { recursive: true });
      const seen = path.join(homeRoot, "npm-tmpdir");
      fs.writeFileSync(path.join(bin, "npm"), ["#!/bin/sh", `printf '%s' "\${TMPDIR-unset}" > '${seen}'`, ""].join("\n"), { mode: 0o755 });
      const factory: ExecutorFactory = (id) => {
        const stub = new StubExecutor(nullLogger());
        return {
          homeDir: path.join(homeRoot, id),
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
        run_id: runIdOf(4425),
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
        runTmpdirTestDeps: { prepare: async () => ({ dir: LEAF }) },
      }).execute(claim);
      assert.equal(fs.readFileSync(seen, "utf8"), LEAF);
    }));
});
