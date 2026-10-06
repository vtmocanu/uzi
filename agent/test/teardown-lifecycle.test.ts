import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import os from "node:os";
import { randomUUID } from "node:crypto";
import { rmTeardownTree, rmTreePinned, restoreTreeWritability, type TeardownTestDeps } from "../src/rmtree.js";
import { removeProvisionDir } from "../src/provision-run.js";
import { removeJobWorkspace, createJobWorkspace } from "../src/job-workspace.js";
import { runReadOnlyModelPass, isLiveModelPassHome } from "../src/model-pass.js";
import type { SdkQueryFn } from "../src/sdk-executor.js";
import { recordingLogger } from "./helpers.js";
import { api, fakeGitlab, gitlabClaim, installHarness, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { RACED_FILES, seedRacedTree, startSwapRacer } from "./swap-racer.js";

installHarness();
const linux = process.platform === "linux";
const surfaces = ["terminal HOME", "provision", "model pass", "job workspace"] as const;
type Surface = typeof surfaces[number];
type Seed = (target: string) => Promise<void>;

async function exists(target: string): Promise<boolean> {
  return fs.lstat(target).then(() => true, () => false);
}

/** Exercise each public lifecycle seam, preserving the successful result across refusal. */
async function exercise(surface: Surface, parent: string, seed: Seed, testDeps?: TeardownTestDeps) {
  const { logger, lines } = recordingLogger();
  let target = path.join(parent, randomUUID());
  if (surface === "terminal HOME") {
    const { gitlab } = fakeGitlab();
    simulateCommittedWork();
    const claim = gitlabClaim(1831);
    await runnerWith(() => ({
      homeDir: target,
      executor: { run: async (ctx) => { await fs.mkdir(target, { recursive: true }); await seed(target); return { branch: ctx.branch }; } },
    }), gitlab, undefined, logger, { teardownTestDeps: testDeps }).execute(claim);
    assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"), JSON.stringify({ states: api.states, lines }));
    assert.ok(!api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed"));
  } else if (surface === "model pass") {
    const queryFn: SdkQueryFn = (({ options }: { options: { env: { HOME: string } } }) =>
      (async function* () {
        target = options.env.HOME;
        assert.equal(isLiveModelPassHome(target), true);
        await seed(target);
        yield { type: "assistant", message: { role: "assistant", content: [{ type: "text", text: "advice" }] } };
        yield { type: "result", subtype: "success", is_error: false };
      })()) as unknown as SdkQueryFn;
    assert.equal(await runReadOnlyModelPass({
      token: "fixture", systemPrompt: "sys", prompt: "hello", homeRoot: parent,
      homePrefix: "uzi-review-", label: "review", timeoutMs: 60_000,
      queryFn, denyReason: "no tools", log: logger, teardownTestDeps: testDeps,
    }), "advice");
    assert.equal(isLiveModelPassHome(target), false, "unregistered after cleanup settles");
  } else if (surface === "job workspace") {
    const ws = await createJobWorkspace(parent, path.basename(target));
    await seed(target);
    await removeJobWorkspace(ws, logger, testDeps);
  } else {
    await fs.mkdir(target);
    await seed(target);
    await removeProvisionDir(target, logger, testDeps);
  }
  return { target, lines };
}

async function fixture(body: (parent: string, victim: string) => Promise<void>) {
  const parent = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-teardown-"));
  const victim = path.join(parent, "outside");
  await fs.mkdir(victim);
  await fs.writeFile(path.join(victim, "precious"), "keep\n");
  try { await body(parent, victim); }
  finally {
    // All racers/helpers have settled before this fixture cleanup.
    await restoreTreeWritability(parent);
    await fs.rm(parent, { recursive: true, force: true });
  }
}

describe("rmTeardownTree contract", () => {
  it("validates absolute paths and passes exactly dirname, basename and now + 120000", async () => {
    const calls: unknown[] = [];
    const deps: TeardownTestDeps = {
      now: () => 1234,
      removeTreePinned: async (...args) => { calls.push(args); return "absent"; },
    };
    await assert.rejects(rmTeardownTree("relative/run", deps), /non-absolute/);
    assert.deepEqual(calls, []);
    await rmTeardownTree("/data/agent-home/run", deps);
    assert.deepEqual(calls, [["/data/agent-home", "run", { deadline: 121234 }]]);
  });

  it("refuses the filesystem root before opening or removing anything", async () => {
    await assert.rejects(rmTeardownTree("/"), /not one path component/);
  });

  it("propagates a pinned refusal after exactly one removal attempt", async () => {
    const refusal = new Error("pinned removal refused");
    let attempts = 0;
    await assert.rejects(rmTeardownTree("/data/agent-home/run", {
      removeTreePinned: async () => {
        attempts += 1;
        throw refusal;
      },
    }), (error) => error === refusal);
    assert.equal(attempts, 1, "a refusal must not trigger another removal path");
  });
});

for (const surface of surfaces) {
  describe(surface + " pinned teardown", () => {
    for (const readOnly of [false, true]) {
      it("default " + (readOnly ? "read-only" : "ordinary") + " cleanup; non-Linux retains with warning", async () => fixture(async (parent, victim) => {
        const out = await exercise(surface, parent, async (target) => {
          const sub = path.join(target, "cache");
          await fs.mkdir(sub, { recursive: true });
          await fs.writeFile(path.join(sub, "file"), "cache");
          if (readOnly) await fs.chmod(sub, 0o555);
        });
        assert.equal(await exists(out.target), !linux);
        if (!linux) assert.ok(out.lines.some((l) => JSON.stringify(l).includes("cleanup failed")));
        assert.equal(await fs.readFile(path.join(victim, "precious"), "utf8"), "keep\n");
      }));
    }

    for (const kind of ["symlink", "file"] as const) {
      it("default refuses a " + kind + " root, warns and preserves the result", async () => fixture(async (parent, victim) => {
        const out = await exercise(surface, parent, async (target) => {
          await fs.rm(target, { recursive: true });
          if (kind === "symlink") await fs.symlink(victim, target, "dir");
          else await fs.writeFile(target, "keep root");
        });
        assert.equal(await exists(out.target), true);
        assert.ok(out.lines.some((l) => JSON.stringify(l).includes("cleanup failed")));
        assert.equal(await fs.readFile(path.join(victim, "precious"), "utf8"), "keep\n");
      }));
    }

    for (const refusal of ["owner", "budget", "expired deadline"] as const) {
      it("real pinned " + refusal + " refusal warns and retains", async () => fixture(async (parent) => {
        let ran = false;
        const out = await exercise(surface, parent, async (target) => {
          await fs.writeFile(path.join(target, "keep"), "retained");
        }, {
          removeTreePinned: async (p, n, opts) => {
            ran = true;
            if (surface === "model pass") assert.equal(isLiveModelPassHome(path.join(p, n)), true);
            return rmTreePinned(p, n, {
              ...opts,
              ...(refusal === "owner" ? { getuid: () => (process.getuid?.() ?? 0) + 1 } :
                refusal === "budget" ? { maxEntries: 0 } : { deadline: Date.now() - 1 }),
            });
          },
        });
        assert.equal(ran, true);
        assert.equal(await fs.readFile(path.join(out.target, "keep"), "utf8"), "retained");
        const warning = out.lines.find((l) => JSON.stringify(l).includes("cleanup failed"));
        assert.ok(warning);
        if (linux) assert.match(JSON.stringify(warning), refusal === "owner" ? /not owned/ : refusal === "budget" ? /budget/ : /deadline passed/);
      }));
    }

    it("portable injected refusal leaves the outcome and registry order intact", async () => fixture(async (parent) => {
      let ran = false;
      const out = await exercise(surface, parent, async (target) => {
        await fs.writeFile(path.join(target, "keep"), "retained");
      }, {
        now: () => 1234,
        removeTreePinned: async (p, n, opts) => {
          ran = true;
          assert.equal(p, parent);
          assert.equal(path.basename(n), n);
          assert.deepEqual(opts, { deadline: 121234 });
          if (surface === "model pass") assert.equal(isLiveModelPassHome(path.join(p, n)), true);
          throw new Error("injected refusal");
        },
      });
      assert.equal(ran, true);
      assert.equal(await fs.readFile(path.join(out.target, "keep"), "utf8"), "retained");
      assert.ok(out.lines.some((l) => JSON.stringify(l).includes("injected refusal")));
    }));

    it("concurrent swaps preserve every outside name and content", { skip: linux ? false : "Linux pinned-walk racer" }, async () => fixture(async (parent, victim) => {
      await fs.rm(path.join(victim, "precious"));
      let racer: Awaited<ReturnType<typeof startSwapRacer>> | undefined;
      let swaps = 0;
      let out: Awaited<ReturnType<typeof exercise>>;
      try {
        out = await exercise(surface, parent, async (target) => {
          await seedRacedTree(target, victim);
          racer = await startSwapRacer(target, victim, parent);
        });
      } finally {
        if (racer) swaps = await racer.stop();
      }
      assert.ok(swaps > 0, "positive intermediate swaps");
      const names = (await fs.readdir(victim)).sort();
      assert.deepEqual(names, Array.from({ length: RACED_FILES }, (_, i) => "f" + i).sort());
      for (const name of names) assert.equal(await fs.readFile(path.join(victim, name), "utf8"), "keep\n");
      assert.ok(!(await exists(out!.target)) || out!.lines.some((l) => JSON.stringify(l).includes("cleanup failed")));
    }));
  });
}
