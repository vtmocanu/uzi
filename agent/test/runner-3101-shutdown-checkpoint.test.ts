import { it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import type { Readable } from "node:stream";
import type { Executor, RunContext } from "../src/executor.js";
import {
  client, deferred, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith,
} from "./runner-harness.js";
import {
  codexRig, commitInTree, GIT_ENV, trackedFile, type CodexRig,
} from "./runner-codex-sinks-fixture.js";

installHarness();

// Shutdown cancels execution while the bounded checkpoint retains separate authority.
// The fake provider leaves its real registry/safety alive through terminal disposal,
// matching CodexExecutor's deferRegistryTeardown: true runner ownership.
for (const installer of ["completed", "absent-promise"] as const) {
  it(`#3101 shutdown checkpoint preserves exact committed and dirty content; installer=${installer}`, async () => {
    const scratch = path.resolve(import.meta.dirname, "../../.uzi/scratch");
    fs.mkdirSync(scratch, { recursive: true });
    const dir = fs.mkdtempSync(path.join(scratch, "3101-checkpoint-"));
    const receiver = path.join(dir, "receiver.git");
    const committed = "committed work before shutdown\n";
    const dirty = "uncommitted work before shutdown\n";
    const iid = installer === "completed" ? 310101 : 310102;
    const started = deferred();
    let rig: CodexRig | undefined;
    let context: RunContext | undefined;
    const publications: Array<{ tip: string; bytes: Buffer }> = [];
    const originalPublish = client.publishCheckpoint;
    client.publishCheckpoint = async (_runId, tip, pack: Readable) => {
      const chunks: Buffer[] = [];
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      publications.push({ tip, bytes: Buffer.concat(chunks) });
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/test/3101" } };
    };
    try {
      // Only the default branch is available here. No alternates, runner clone,
      // or worker tracking-ref objects can satisfy the assertions after import.
      execFileSync("git", ["clone", "--bare", "--no-local", fx.originPath, receiver],
        { env: GIT_ENV, stdio: "pipe" });
      const executor: Executor = {
        run: async (ctx) => {
          context = ctx;
          assert.ok(ctx.signal, "Runner supplies its genuine run signal");
          assert.ok(ctx.terminalLifecycleSignal, "Runner supplies its terminal lifecycle signal");
          rig = codexRig({
            lifecycleSignal: AbortSignal.any([ctx.signal, ctx.terminalLifecycleSignal]),
          });
          executor.safety = rig.safety;
          if (installer === "completed") {
            rig.installer.promise = Promise.resolve({ results: [], truncated: false })
              .then((result) => { rig!.installer.result = result; return result; });
            await rig.installer.promise;
            assert.ok(rig.installer.result, "actual shared installer state has completed");
          } else {
            assert.equal(rig.installer.promise, undefined, "absent promise, not a no-lockfile install");
          }
          commitInTree(ctx.worktreePath, "COMMITTED-3101.txt", committed);
          fs.writeFileSync(path.join(ctx.worktreePath, "DIRTY-3101.txt"), dirty);
          started.resolve();
          try {
            await new Promise<void>((resolve) => {
              if (ctx.signal!.aborted) resolve();
              else ctx.signal!.addEventListener("abort", () => resolve(), { once: true });
            });
            throw new Error("runner shutdown interrupted provider");
          } finally {
            // Production's terminal install join does not drop the settled promise.
            await rig.installer.settle();
          }
        },
      };
      const { gitlab } = fakeGitlab();
      const runner = runnerWith(() => ({ executor }), gitlab, undefined, undefined, {
        codexBoundaryDeadlineMs: 60_000,
        shutdownPublishTimeoutMs: 60_000,
      });
      const execution = runner.execute(gitlabClaim(iid));
      await Promise.race([
        started.promise,
        execution.then(() => { throw new Error("run ended before shutdown regression reached readiness"); }),
      ]);
      runner.shutdown();
      await execution;
      assert.equal(context!.signal!.aborted, true);
      assert.equal(context!.terminalLifecycleSignal!.aborted, true);
      assert.ok(rig!.boundaries.includes("shutdown"), "real safety reconciles the shutdown sink");

      // Consume ONLY the emitted pack plus the independent default-branch seed.
      // Both exact file contents must survive capture,
      // fetch-back, pack production, and import.
      const content = publications.map(({ tip, bytes }) => {
        execFileSync("git", ["-C", receiver, "index-pack", "--stdin"],
          { env: GIT_ENV, input: bytes, stdio: ["pipe", "pipe", "pipe"] });
        const show = (file: string): string => execFileSync(
          "git", ["-C", receiver, "show", `${tip}:${file}`],
          { env: GIT_ENV, encoding: "utf8", stdio: "pipe" },
        );
        return { committed: show("COMMITTED-3101.txt"), dirty: show("DIRTY-3101.txt") };
      });
      assert.deepEqual(content, [{ committed, dirty }],
        "#3101 emitted independent checkpoint pack must contain exact committed and dirty work");
      assert.equal(trackedFile(iid, "COMMITTED-3101.txt"), committed, "real fetch-back imports committed work");
      assert.equal(trackedFile(iid, "DIRTY-3101.txt"), dirty, "real capture and fetch-back import dirty work");
      assert.equal(execFileSync("git", ["-C", git.barePathFor(fx.originPath), "rev-parse",
        `refs/uzi-runner/agent/issue-${iid}`],
      { env: GIT_ENV, encoding: "utf8" }).trim(), publications[0]!.tip);
      assert.deepEqual(rig!.disposeBoundaries, ["terminal"]);
    } finally {
      client.publishCheckpoint = originalPublish;
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
}
