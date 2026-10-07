import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { randomUUID } from "node:crypto";
import { createJobWorkspace } from "../src/job-workspace.js";
import { rmTeardownTree, rmTreePinned } from "../src/rmtree.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import { api, installHarness, fakeGitlab, gitlabClaim, runnerWith, simulateCommittedWork } from "./runner-harness.js";
import { recordingLogger } from "./helpers.js";
import { seedRacedTree, startSwapRacer } from "./swap-racer.js";

import { residualTestFixture as fixture, assertOutsideFiles as assertVictim } from "./residual-fixtures.js";

installHarness();

describe("residual worker-owned cleanup (#2324)", () => {
  it("creates a fresh job without needing unsupported-platform removal", async () => fixture(async (root) => {
    const ws = await createJobWorkspace(root, randomUUID(), false);
    assert.ok((await fs.stat(ws.work)).isDirectory());
  }));

  it("refuses a symlink leftover without adopting it", async () => fixture(async (root, victim) => {
    const id = randomUUID();
    await fs.writeFile(path.join(victim, "keep"), "outside");
    await fs.symlink(victim, path.join(root, id), "dir");
    await assert.rejects(createJobWorkspace(root, id, false), process.platform === "linux" ? /symlink or non-directory/ : /no descriptor-pinned walk/);
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
    for (const name of [".other", ".uzi-skills-", ".uzi-skills-.x", ".uzi-skills--x", ".uzi-skills-a\u0000", ".uzi-skills-a\n", ".uzi-skills-a\u007f", ".uzi-skills-a\u0085", ".", ".."]) {
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
    for (const name of [".uzi-skills-/child", ".uzi-skills-../child", "../.uzi-skills-child", ".uzi-skills-a\\child"]) {
      await assert.rejects(rmTreePinned(root, name, { allowSkillsPluginName: true }), /not one path component/);
    }
    if (process.platform !== "linux") return t.skip("real pinned success requires Linux");
    for (const name of [".uzi-skills-issue-2324", ".uzi-skills-feat-a+b", ".uzi-skills-機能-更新"]) {
      const target = path.join(root, name);
      await fs.mkdir(target);
      await fs.writeFile(path.join(target, "skill"), "body");
      await assert.rejects(rmTreePinned(root, name), /not one path component/);
      await rmTeardownTree(target);
      await assert.rejects(fs.stat(target), { code: "ENOENT" });
    }
  }));

  it("plugin opt-in preserves owner and symlink refusals", { skip: process.platform !== "linux" }, async () => fixture(async (root, victim) => {
    const name = ".uzi-skills-feat-a+b";
    const target = path.join(root, name);
    await fs.mkdir(target, { mode: 0o700 });
    await fs.writeFile(path.join(target, "keep"), "retained");
    const mode = (await fs.stat(target)).mode;
    await assert.rejects(rmTreePinned(root, name, {
      allowSkillsPluginName: true, getuid: () => process.getuid!() + 1,
    }), /not owned by this worker/);
    assert.equal((await fs.stat(target)).mode, mode);
    assert.equal(await fs.readFile(path.join(target, "keep"), "utf8"), "retained");
    await fs.rm(target, { recursive: true });
    await fs.writeFile(path.join(victim, "keep"), "outside");
    await fs.symlink(victim, target, "dir");
    await assert.rejects(rmTreePinned(root, name, { allowSkillsPluginName: true }), /symlink or non-directory/);
    assert.ok((await fs.lstat(target)).isSymbolicLink());
    assert.equal(await fs.readFile(path.join(victim, "keep"), "utf8"), "outside");
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
    const { logger, lines } = recordingLogger();
    let target: string | undefined;
    const claim = gitlabClaim(2324);
    let racer: Awaited<ReturnType<typeof startSwapRacer>> | undefined;
    let swaps = 0;
    try {
      await runnerWith(() => ({ executor: { run: async (ctx) => {
        target = skillsPluginDir(ctx.worktreePath);
        await seedRacedTree(target, victim);
        racer = await startSwapRacer(target, victim, root);
        return { branch: ctx.branch };
      } } }), gitlab, undefined, logger, { teardownTestDeps: undefined }).execute(claim);
    } finally { if (racer) swaps = await racer.stop(); }
    assert.ok(api.states.some((state) => state.runId === claim.run_id && state.body.status === "completed"));
    assert.ok(target);
    const retained = await fs.lstat(target).then(() => true, () => false);
    assert.ok(!retained || lines.some((line) => JSON.stringify(line).includes("skills plugin cleanup failed")), "cleanup removed the plugin or reported its refusal");
    await assertVictim(victim, swaps);
  }));
});
