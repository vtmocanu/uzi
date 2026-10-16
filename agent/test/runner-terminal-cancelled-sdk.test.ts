import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import { SdkExecutor, type SdkQueryFn } from "../src/sdk-executor.js";
import { CredentialSwitchSignal, PauseNowSignal } from "../src/steering.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api, assistant, client, deferred, fakeGitlab, fx, git, gitlabClaim,
  homeDir, installHarness, resultOk, runnerWith, worktreeDirFor,
} from "./runner-harness.js";

installHarness();

async function bounded<T>(promise: Promise<T>, label: string): Promise<T> {
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<never>((_, reject) => {
        timer = setTimeout(() => reject(new Error(label)), 3_000);
      }),
    ]);
  } finally {
    clearTimeout(timer);
  }
}

describe("M2 real runner + SDK terminal control after lifecycle recovery", () => {
  for (const recovery of ["credential switch", "pause"] as const) {
    it(`retains dirty source when terminal control aborts a live SDK turn after declined ${recovery}`,
      { timeout: 10_000 }, async (t) => {
        const iid = recovery === "pause" ? 29082 : 29081;
        const claim = gitlabClaim(iid, { claim_generation: 7, auto_approve: true });
        const interrupted = deferred();
        const resumed = deferred();
        const terminalAborted = deferred();
        const cleanup = deferred();
        const controllers: AbortController[] = [];
        let ctxRef: RunContext | undefined;
        let turns = 0;
        let source = "";
        let resumedSignal: AbortSignal | undefined;
        let captures = 0;
        const tracked = Buffer.from("dirty tracked source before SDK\n");
        const untracked = Buffer.from("untracked source before SDK\n");

        const queryFn: SdkQueryFn = (params) => {
          const turn = ++turns;
          return (async function* () {
            for await (const _ of params.prompt) { /* drain the real SDK prompt */ }
            if (turn === 1) {
              yield assistant([{
                type: "tool_use", id: "plan", name: "mcp__uzi__submit_plan",
                input: { plan_md: "# Plan\n- preserve work", milestones: [] },
              }]);
              yield resultOk();
              return;
            }
            const controller = params.options.abortController!;
            controllers.push(controller);
            if (turn === 2) interrupted.resolve();
            else {
              resumedSignal = controller.signal;
              resumed.resolve();
            }
            const abort = deferred();
            const onAbort = () => {
              if (turn === 3) terminalAborted.resolve();
              abort.resolve();
            };
            controller.signal.addEventListener("abort", onAbort, { once: true });
            try {
              if (controller.signal.aborted) onAbort();
              // Cleanup releases every scripted flight even when a readiness assertion fails.
              await Promise.race([abort.promise, cleanup.promise]);
            } finally {
              controller.signal.removeEventListener("abort", onAbort);
            }
          })();
        };
        const sdk = new SdkExecutor(nullLogger(), homeDir, { queryFn });
        const executor: Executor = {
          run: async (ctx) => {
            ctxRef = ctx;
            source = ctx.worktreePath;
            fs.writeFileSync(path.join(source, "README.md"), tracked);
            fs.writeFileSync(path.join(source, "untracked.txt"), untracked);
            t.mock.method(git, "worktreeStatus", async () => ["M README.md", "?? untracked.txt"]);
            t.mock.method(git, "commitWipMarker", async () => { captures++; return false; });
            // Neither recovery can establish a published checkpoint.
            t.mock.method(git, "checkpointPack", async () => null);
            return sdk.run(ctx);
          },
          closeAndSettlePlanSummary: (d) => sdk.closeAndSettlePlanSummary(d),
          killAgentTree: () => sdk.killAgentTree(),
          reapAttributedProcesses: () => sdk.reapAttributedProcesses(),
        };
        const { gitlab, calls } = fakeGitlab();
        const push = t.mock.method(git, "pushBranch", async () => { assert.fail("unexpected git push"); });
        const publish = t.mock.method(client, "publishCheckpoint", async () => {
          assert.fail("unexpected checkpoint publication");
        });
        const { logger, lines } = recordingLogger();
        const run = runnerWith(() => ({ executor, homeDir }), gitlab, undefined, logger, {
          recoveryRetryMs: 1, planApprovalTimeoutMs: 0, questionTimeoutMs: 0,
          checkpointIntervalMs: 0,
        });
        const execution = run.execute(claim);
        // Observe rejection immediately, including on an assertion's cleanup path.
        const settled = execution.then(() => undefined, (error: unknown) => error);
        try {
          await bounded(interrupted.promise, "initial implementation turn did not start");
          assert.equal(ctxRef!.signal!.aborted, false);
          if (recovery === "credential switch") api.requestCredentialSwitch(claim.run_id, 7);
          else api.setInputs(claim.run_id, [{ id: 7001, kind: "pause", body: "now" }]);

          await bounded(resumed.promise, "recovery did not resume a provider turn").catch((error) => {
            throw new Error(`${error.message} (shared aborted=${ctxRef?.signal?.aborted}, captures=${captures}): ${JSON.stringify(lines)}`);
          });
          assert.equal(turns, 3);
          assert.equal(ctxRef!.signal!.aborted, true, "the real shared signal was spent by recovery");
          assert.ok(ctxRef!.signal!.reason instanceof (
            recovery === "credential switch" ? CredentialSwitchSignal : PauseNowSignal
          ));
          assert.equal(resumedSignal!.aborted, false, "the resumed provider turn is live before terminal control");
          assert.ok(captures > 0, "the runner attempted the actual recovery checkpoint");
          const statuses = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
          assert.ok(statuses.includes(recovery === "credential switch" ? "credential_switch_failed" : "pause_failed"));

          api.rawInputGets(claim.run_id, {
            inputs: [], terminal_cancelled: { run_id: claim.run_id, generation: 7 },
          });
          await bounded(terminalAborted.promise, "terminal GET control did not abort the live SDK provider");
          assert.equal(resumedSignal!.aborted, true);
          assert.equal(await bounded(settled, "execute did not settle after terminal control"), undefined);
          assert.equal(turns, 3, "no new provider turn after terminal control");
          assert.equal(run.isExecuting(claim.run_id), false);
          assert.equal(source, worktreeDirFor(iid));
          assert.deepEqual(fs.readFileSync(path.join(source, "README.md")), tracked);
          assert.deepEqual(fs.readFileSync(path.join(source, "untracked.txt")), untracked);
          const config = fs.readFileSync(path.join(git.barePathFor(fx.originPath), "config"), "utf8");
          assert.match(config, /uzi-stopped/);
          assert.ok(config.includes(claim.run_id), "the retained journal names this run");
          assert.ok(!api.states.some((s) => s.runId === claim.run_id
            && ["failed", "completed", "cancelled"].includes(s.body.status)), "no competing terminal report");
          assert.equal(calls.length, 0, "no forge publication");
          assert.equal(push.mock.callCount(), 0);
          assert.equal(publish.mock.callCount(), 0);
        } finally {
          run.shutdown();
          for (const controller of controllers) controller.abort();
          cleanup.resolve();
          await bounded(settled, "cleanup could not settle execute");
        }
      });
  }
});
