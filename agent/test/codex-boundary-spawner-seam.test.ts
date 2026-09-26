import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { PassThrough } from "node:stream";
import { boundaryProcessSpawnerForTest } from "../src/codex/codex-executor.js";
import type { CodexEffectLaunchSpec, CodexRootHandle } from "../src/codex/launcher.js";

// issue #1769 m2: `boundaryProcessSpawnerForTest` is the test seam that exposes the production
// boundary-process spawner (the one CodexExecutor wires into createCodexExecutionSafety) so an e2e
// fixture can build a REAL boundary scope. This unit test pins what the seam builds, with a fake
// launcher: a `command` request becomes the command sandbox rooted at the request's cwd, a
// `worker_pat` request the plain supervised spec, and both register as boundary_action roots.

function fakeLaunch(specs: CodexEffectLaunchSpec[]): (spec: CodexEffectLaunchSpec) => Promise<CodexRootHandle> {
  return async (spec) => {
    specs.push(spec);
    return {
      started: {
        event: "started", supervisorPid: 300, childPid: 301, subreaper: true,
        nondumpable: true, uid: 10003, liveCapsZero: true, capBoundingSet: "0xc0", noNewPrivs: true,
      },
      supervisorPid: 300,
      transport: { stdin: new PassThrough(), stdout: new PassThrough(), stderr: new PassThrough() },
      snapshot: async () => ({ event: "snapshot", id: 1, processes: [] }),
      waitChild: async () => ({ event: "child_exit", code: 0 }),
      dispose: async () => ({ clean: true, event: { event: "dispose", id: 1, state: "drained", authority: "ECHILD+__WALL" } }),
      failed: undefined,
      whenFailed: new Promise<Error>(() => undefined),
    };
  };
}

describe("boundaryProcessSpawnerForTest (issue #1769 m2 test seam)", () => {
  it("builds the production command-sandbox spec for a command request and a plain spec for worker_pat", async () => {
    const specs: CodexEffectLaunchSpec[] = [];
    const spawner = boundaryProcessSpawnerForTest("required", fakeLaunch(specs));
    const clone = "/data/runner/repo/issue-1";
    const env = { PATH: "/usr/bin" };

    const consumer = await spawner({ argv: ["/usr/bin/git", "-C", clone, "index-pack", "--stdin"], cwd: clone, env, identity: "command" }, 1000);
    const producer = await spawner({ argv: ["/usr/bin/git", "-C", "/data/repos/r.git", "pack-objects", "--revs"], cwd: "/data/repos/r.git", env, identity: "worker_pat" }, 1000);

    assert.strictEqual(specs.length, 2);
    const [commandSpec, patSpec] = specs;
    assert.strictEqual(commandSpec!.identity, "command");
    assert.strictEqual(commandSpec!.cwd, clone, "the command sandbox is rooted at the request's cwd");
    assert.notStrictEqual(commandSpec!.command, "/usr/bin/git", "the git child runs under the command sandbox wrapper");
    assert.ok(commandSpec!.args.includes("/usr/bin/git") && commandSpec!.args.includes("index-pack"));
    assert.ok(commandSpec!.args.includes(clone), "the sandbox root names the clone");
    assert.strictEqual(patSpec!.identity, "worker_pat");
    assert.strictEqual(patSpec!.command, "/usr/bin/git");
    assert.deepStrictEqual(patSpec!.args, ["-C", "/data/repos/r.git", "pack-objects", "--revs"]);

    for (const launched of [consumer, producer]) {
      assert.strictEqual(launched.root.kind, "boundary_action");
      assert.ok(launched.stdin && launched.stdout && launched.stderr);
      assert.deepStrictEqual(await launched.waitChild(1000), { event: "child_exit", code: 0 });
    }
  });
});
