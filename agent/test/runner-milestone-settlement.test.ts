import assert from "node:assert/strict";
import { it } from "node:test";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { PassThrough, Readable } from "node:stream";
import type { Executor } from "../src/executor.js";
import type { FetchAgentBranchOptions } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { SupervisedChildExitTimeoutError } from "../src/codex/launcher.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { codexRig, commitInTree, GIT_ENV, installRecoveryRootCleanup, virtualBoundaryClock } from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

type FlightView = { checkpointFloor?: string; runId: string };
type RunnerSeam = {
  owedOptions(flight: FlightView, bare: string, branch: string): Promise<FetchAgentBranchOptions>;
};
type GitSeam = {
  runGit(cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string): Promise<string>;
};
const cases = ["soft adoption prep", "soft bridge CAS", "hard bridge CAS", "hard import CAS",
  "unsafe post-import read", "done soft import CAS"] as const;

for (const seam of cases) {
  it("3304 runner settlement through floor: " + seam, async t => {
    const clock = virtualBoundaryClock();
    const publishedBranch = "feature/3304-settlement";
    const runGit = (...args: string[]) => execFileSync("git", args, { env: GIT_ENV, encoding: "utf8" }).trim();
    runGit("-C", fx.originPath, "checkout", "-b", publishedBranch);
    commitInTree(fx.originPath, "published.txt", "published floor P\n");
    const published = runGit("-C", fx.originPath, "rev-parse", "HEAD");
    runGit("-C", fx.originPath, "checkout", "main");
    const bare = git.barePathFor(fx.originPath);
    let active = false, imported = false, triggered = false, completed = false;
    let bridge = "", head = "", branch = "", runId = "";
    let failure: unknown;
    let flight: FlightView | undefined;
    let rig: ReturnType<typeof codexRig> | undefined;
    const readArgv: string[][] = [], events: string[] = [];
    let checkpointPublications = 0, totalPublications = 0;
    const publish = client.publishCheckpoint.bind(client);
    t.mock.method(client, "publishCheckpoint", async (...args: Parameters<typeof client.publishCheckpoint>) => {
      totalPublications++;
      if (active) checkpointPublications++;
      return publish(...args);
    });
    const runnerSeam = RunRunner.prototype as unknown as RunnerSeam;
    const owed = runnerSeam.owedOptions;
    t.mock.method(runnerSeam, "owedOptions", async function (this: RunnerSeam, f: FlightView, b: string, name: string) {
      if (active) {
        flight = f;
        if (bridge && !triggered && seam === "soft adoption prep") {
          triggered = true;
          clock.advance(10_000);
        }
      }
      return owed.call(this, f, b, name);
    });
    const build = git.bridgeToFloors.bind(git);
    t.mock.method(git, "bridgeToFloors", async (...args: Parameters<typeof git.bridgeToFloors>) => {
      const result = await build(...args);
      if (active && result.kind === "built") bridge = result.sha;
      return result;
    });
    const fetch = git.fetchAgentBranch.bind(git);
    t.mock.method(git, "fetchAgentBranch", async (...args: Parameters<typeof git.fetchAgentBranch>) => {
      const result = await fetch(...args);
      if (active && result.kind === "updated") imported = true;
      return result;
    });
    const gitSeam = git as unknown as GitSeam;
    const realGit = gitSeam.runGit.bind(git);
    t.mock.method(gitSeam, "runGit", async (...args: Parameters<GitSeam["runGit"]>) => {
      const result = await realGit(...args);
      const argv = args[1];
      const refIndex = argv.indexOf("refs/uzi-runner/" + branch);
      const candidate = refIndex < 0 ? undefined : argv[refIndex + 1];
      if (active && !triggered && argv.includes("update-ref") && refIndex >= 0) {
        const adoption = bridge && candidate === bridge;
        const importCas = !bridge && candidate === head;
        if ((adoption && (seam === "soft bridge CAS" || seam === "hard bridge CAS"))
          || (importCas && (seam === "hard import CAS" || seam === "done soft import CAS"))) {
          triggered = true;
          clock.advance(seam.startsWith("hard") ? 30_000 : seam.startsWith("done") ? 60_000 : 10_000);
        }
      }
      return result;
    });
    const executor: Executor = {
      get safety() { return rig?.safety; },
      run: async ctx => {
        rig = codexRig({
          lifecycleSignal: AbortSignal.any([ctx.signal!, ctx.terminalLifecycleSignal!]),
          now: clock.now, armDeadline: clock.armDeadline,
          boundaryProcess: request => {
            if (!(active && imported && !triggered && seam === "unsafe post-import read")) return undefined;
            triggered = true;
            readArgv.push([...request.argv]);
            return {
              stdin: new PassThrough(), stdout: Readable.from([]), stderr: Readable.from([]),
              root: {
                kind: "boundary_action",
                dispose: async () => { events.push("dispose"); },
                reap: async () => {
                  events.push("reap");
                  return { ok: false, error: { category: "timeout", message: "owned read still live" } };
                },
              },
              waitChild: async () => { clock.advance(10_000); throw new SupervisedChildExitTimeoutError(); },
            };
          },
        });
        branch = ctx.branch; runId = ctx.runId;
        const base = runGit("-C", fx.originPath, "rev-parse", "main");
        runGit("-C", ctx.worktreePath, "reset", "--hard", base);
        commitInTree(ctx.worktreePath, "executor.txt", "divergent H\n");
        head = (await git.branchTip(ctx.worktreePath, branch))!;
        active = true;
        try {
          await ctx.checkpoint!({ reap: true,
            sink: seam.startsWith("done") ? "done_checkpoint" : "milestone_checkpoint",
            progress: { completed: ["3304-settlement"], in_progress: [] } });
        } catch (error) { failure = error; }
        active = false;
        assert.equal(triggered, true, "requested seam actually fired");
        assert.ok(flight, "observed real runner flight");
        assert.equal(checkpointPublications, 0);
        const fatal = seam.startsWith("hard") || seam.startsWith("unsafe");
        assert.equal(rig.registry.isPoisoned(), fatal);
        if (fatal) {
          assert.ok(failure instanceof CodexBoundaryError, String(failure));
          assert.notEqual(flight.checkpointFloor, bridge || head, "unconfirmed candidate cannot advance the floor");
          const receiptPath = path.join(bare, "uzi-owed",
            "receipt-" + createHash("sha256").update(branch).digest("hex") + ".json");
          const receipt = JSON.parse(fs.readFileSync(receiptPath, "utf8"));
          if (seam.startsWith("unsafe")) {
            assert.equal(failure.step, "checkpoint_import_readback", "the deadline/reap failure names the active read phase");
            assert.equal(receipt.phase, "committed", "the import settled before the unsafe read");
            assert.equal((await git.committedTrackingOwnership(bare, branch, runId, head)).kind, "owned");
          } else {
            assert.equal(receipt.phase, "pending", "hard stop leaves no committed adoption proof");
            assert.equal((await git.committedTrackingOwnership(bare, branch, runId, bridge || head)).kind, "not_owned");
          }
          for (const sha of bridge ? [head, bridge] : [head]) {
            assert.equal(runGit("-C", bare, "rev-parse", "refs/uzi-owed/" + runId + "/" + sha), sha);
          }
          assert.ok(!api.states.some(s => s.body.milestones_completed?.includes("3304-settlement")));
        } else {
          assert.equal(failure, undefined);
          const expected = seam.startsWith("done") ? head : bridge;
          assert.ok(expected);
          assert.equal(await git.trackingTip(bare, branch), expected);
          assert.equal((await git.committedTrackingOwnership(bare, branch, runId, expected)).kind, "owned");
          if (!seam.startsWith("done")) {
            assert.equal(flight.checkpointFloor, bridge, "adoption settles all the way through floor advancement");
            assert.equal(await git.ancestry(bare, published, bridge), "ancestor");
            assert.equal(await git.ancestry(bare, head, bridge), "ancestor");
          }
          assert.ok(api.states.some(s => s.body.milestones_completed?.includes("3304-settlement")));
        }
        completed = true;
        // Fatal checkpoints must still unwind through the runner's normal failure path.
        if (failure) throw failure;
        return { branch };
      },
    };
    await runnerWith(() => ({ executor }), fakeGitlab().gitlab, undefined, undefined, {
      checkpointIntervalMs: 0,
      checkpointTestHooks: { absoluteNow: clock.now, armSoftDeadline: clock.arm },
    }).execute(gitlabClaim(330410, { kind: "task", branch: publishedBranch, open_mr: false }));
    assert.equal(completed, true, "runner must not swallow an executor assertion");
    assert.equal(triggered, true);
    const texts = api.messages(runId).map(m => String(m.payload.text));
    if (seam.startsWith("hard") || seam.startsWith("unsafe")) {
      assert.equal(totalPublications, 0, "fatal checkpoint cannot publish during unwind either");
      assert.ok(!texts.some(text => text.includes("checkpoint publish skipped: soft deadline")));
      assert.ok(!api.states.some(s => s.body.milestones_completed?.includes("3304-settlement")));
    } else {
      assert.ok(texts.some(text => text.includes("checkpoint publish skipped: soft deadline")));
    }
    if (seam.startsWith("done")) {
      assert.ok(api.states.some(s => s.body.status === "completed"), "done still finalizes after its soft stop");
      const finalTip = runGit("-C", fx.originPath, "rev-parse", "refs/heads/" + branch);
      const finalBareTip = await git.trackingTip(bare, branch);
      assert.equal(finalTip, finalBareTip, "finalize pushes the settled tracking candidate");
      assert.equal(await git.ancestry(bare, head, finalTip), "ancestor");
      assert.equal(await git.ancestry(bare, published, finalTip), "ancestor");
    }
    if (seam.startsWith("unsafe")) {
      assert.equal(readArgv.length, 1);
      assert.ok(readArgv[0]!.some(arg => ["config", "rev-parse", "for-each-ref", "show-ref"].includes(arg)),
        "injected process is an actual post-import Git read: " + JSON.stringify(readArgv));
      assert.ok(events.includes("dispose"));
      assert.ok(events.includes("reap"));
    }
  });
}
