import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { makeProductionLaunchAdviceRoot, CODEX_PRODUCTION_PROVIDER } from "../src/codex/codex-executor.js";
import { CODEX_BIN } from "../src/codex/launcher.js";
import { WORKER_UID, RUNNER_UID, runnerCommand, uidSplitActive } from "../src/runner-uid.js";
import type { LaunchAdviceRootSeam } from "../src/codex/codex-advice-harness.js";
import type { Logger } from "../src/log.js";
import { createJobWorkspace } from "../src/job-workspace.js";
import { rmTeardownTree, rmTreePinned, restoreTreeWritability } from "../src/rmtree.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { api, installHarness, fakeGitlab, gitlabClaim, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { recordingLogger } from "./helpers.js";
import { RACED_FILES, seedRacedTree, startSwapRacer } from "./swap-racer.js";

installHarness();

async function fixture(body: (root: string, victim: string) => Promise<void>) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "cdr-residual-"));
  const victim = path.join(root, "outside");
  await fs.mkdir(victim);
  try { await body(root, victim); }
  finally {
    await restoreTreeWritability(root);
    await fs.rm(root, { recursive: true, force: true });
  }
}

async function assertVictim(victim: string, swaps: number) {
  assert.ok(swaps > 0, "positive intermediate swaps prove the race happened");
  const names = (await fs.readdir(victim)).sort();
  console.log(JSON.stringify({ swaps, outsideRemaining: names.length, outsideLost: RACED_FILES - names.length }));
  assert.deepEqual(names, Array.from({ length: RACED_FILES }, (_, i) => `f${i}`).sort());
  for (const name of names) assert.equal(await fs.readFile(path.join(victim, name), "utf8"), "keep\n");
}

describe("residual worker-owned cleanup (#2324)", () => {
  it("creates a fresh job without needing unsupported-platform removal", async () => fixture(async (root) => {
    const ws = await createJobWorkspace(root, randomUUID(), false);
    assert.ok((await fs.stat(ws.work)).isDirectory());
  }));

  it("refuses a symlink leftover without adopting it", async () => fixture(async (root, victim) => {
    const id = randomUUID();
    await fs.writeFile(path.join(victim, "keep"), "outside");
    await fs.symlink(victim, path.join(root, id), "dir");
    await assert.rejects(createJobWorkspace(root, id, false));
    assert.ok((await fs.lstat(path.join(root, id))).isSymbolicLink());
    assert.equal(await fs.readFile(path.join(victim, "keep"), "utf8"), "outside");
  }));

  it("replaces an ordinary leftover job root successfully", { skip: process.platform !== "linux" }, async () => fixture(async (root) => {
    const id = randomUUID();
    const target = path.join(root, id);
    await fs.mkdir(target);
    await fs.writeFile(path.join(target, "old"), "discard");
    const ws = await createJobWorkspace(root, id, false);
    assert.ok((await fs.stat(ws.work)).isDirectory());
    await assert.rejects(fs.stat(path.join(target, "old")), { code: "ENOENT" });
  }));

  it("default pinned remover keeps its ordinary-name contract", async (t) => fixture(async (root) => {
    for (const name of [".uzi-skills-issue-2324", ".hidden", "-option", "", ".", "..", "a/b", "a b", "é"]) {
      await assert.rejects(rmTreePinned(root, name), /not one path component/);
    }
    if (process.platform !== "linux") return t.skip("real pinned success requires Linux");
    for (const name of ["a", "0", "_", "run-1", "a.b_c-0", "a..", "A-Z"]) {
      await fs.mkdir(path.join(root, name));
      assert.equal(await rmTreePinned(root, name), "removed");
    }
  }));

  it("teardown permits only the exact generated hidden plugin prefix", async (t) => fixture(async (root) => {
    for (const name of [".other", ".uzi-skills-", ".", ".."]) {
      let invoked = false;
      await assert.rejects(rmTeardownTree(`${root}/${name}`, {
        removeTreePinned: async (parent, leaf, opts) => {
          invoked = true;
          assert.equal(opts?.allowSkillsPluginName, undefined);
          return rmTreePinned(parent, leaf, opts);
        },
      }));
      assert.equal(invoked, true);
    }
    for (const name of [".uzi-skills-/child", ".uzi-skills-../child", "../.uzi-skills-child"]) {
      await assert.rejects(rmTreePinned(root, name, { allowSkillsPluginName: true }), /not one path component/);
    }
    if (process.platform !== "linux") return t.skip("real pinned success requires Linux");
    const target = path.join(root, ".uzi-skills-issue-2324");
    await fs.mkdir(target);
    await fs.writeFile(path.join(target, "skill"), "body");
    await rmTeardownTree(target);
    await assert.rejects(fs.stat(target), { code: "ENOENT" });
  }));

  it("leftover job replacement never deletes an outside file during swaps", { skip: process.platform !== "linux" }, async () => fixture(async (root, victim) => {
    const id = randomUUID();
    const target = path.join(root, id);
    await seedRacedTree(target, victim);
    const racer = await startSwapRacer(target, victim, root);
    let swaps = 0;
    try {
      await createJobWorkspace(root, id, false).catch(() => undefined);
    } finally { swaps = await racer.stop(); }
    await assertVictim(victim, swaps);
  }));

  it("terminal plugin cleanup never deletes an outside file during swaps", { skip: process.platform !== "linux" }, async () => fixture(async (root, victim) => {
    simulateCommittedWork();
    const { gitlab } = fakeGitlab();
    const { logger } = recordingLogger();
    const claim = gitlabClaim(2324);
    let racer: Awaited<ReturnType<typeof startSwapRacer>> | undefined;
    let swaps = 0;
    try {
      await runnerWith(() => ({ executor: { run: async (ctx) => {
        const target = skillsPluginDir(ctx.worktreePath);
        await seedRacedTree(target, victim);
        racer = await startSwapRacer(target, victim, root);
        return { branch: ctx.branch };
      } } }), gitlab, undefined, logger, { teardownTestDeps: undefined }).execute(claim);
    } finally { if (racer) swaps = await racer.stop(); }
    assert.ok(api.states.some((state) => state.runId === claim.run_id && state.body.status === "completed"));
    await assertVictim(victim, swaps);
  }));
});

it("real advice cwd disposal never deletes outside files during swaps", {
  skip: process.platform !== "linux" || process.getuid?.() !== WORKER_UID || !uidSplitActive() || !existsSync(CODEX_BIN)
    ? "requires the packaged worker uid-split runtime" : false,
}, async () => fixture(async (root, victim) => {
  await fs.chown(root, -1, RUNNER_UID);
  await fs.chmod(root, 0o3775);
  const { logger } = recordingLogger();
  // The additional logger is structurally compatible with the unchanged baseline's
  // two-argument factory, so the baseline fails for outside loss, not compilation.
  const factory: (home: string, mode: "api_key", log: Logger) => LaunchAdviceRootSeam = makeProductionLaunchAdviceRoot;
  const handle = await factory(root, "api_key", logger)({
    kind: "advice", label: "review", provider: CODEX_PRODUCTION_PROVIDER, model: "gpt-6.1-sol",
  });
  const owned = path.join(root, "codex-advice-data", path.basename(handle.cwd));
  let racer: Awaited<ReturnType<typeof startSwapRacer>> | undefined;
  let swaps = 0;
  try {
    await seedRacedTree(handle.cwd, victim);
    racer = await startSwapRacer(handle.cwd, victim, root);
    await handle.dispose();
  } finally {
    if (racer) swaps = await racer.stop();
    // Unit 2 owns provider-data deletion. This is fixture cleanup only, after
    // provider disposal and racer settlement, using its existing owning uid.
    const command = runnerCommand(process.execPath, ["-e", "require('node:fs').rmSync(process.argv[1], {recursive:true,force:true})", owned]);
    const cleaned = spawnSync(command.command, command.args, { env: { PATH: "/usr/bin:/bin" }, timeout: 10_000 });
    assert.equal(cleaned.status, 0, "owned provider fixture cleanup");
  }
  await assertVictim(victim, swaps);
}));
