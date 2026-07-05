import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { prepareSkillPlugin, resolveSkillCaps } from "../src/skills-run.js";
import { skillsPluginDir } from "../src/skills-plugin.js";
import type { ClaimSkill } from "../src/protocol.js";

let worktree: string;
beforeEach(() => {
  worktree = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-run-"));
});
afterEach(() => {
  fs.rmSync(worktree, { recursive: true, force: true });
  fs.rmSync(skillsPluginDir(worktree), { recursive: true, force: true });
});

function writeRepoSkill(dir: string, name: string, desc: string, body: string): void {
  const d = path.join(worktree, ".claude", "skills", dir);
  fs.mkdirSync(d, { recursive: true });
  fs.writeFileSync(path.join(d, "SKILL.md"), `---\nname: ${name}\ndescription: ${desc}\n---\n\n${body}\n`);
}

function loadedNames(): string[] {
  const skillsRoot = path.join(skillsPluginDir(worktree), "skills");
  if (!fs.existsSync(skillsRoot)) return [];
  return fs.readdirSync(skillsRoot).sort();
}

const caps = resolveSkillCaps(null); // defaults 65536 / 32

describe("prepareSkillPlugin (shared SDK + stub path)", () => {
  it("materializes delivered skills and reports no repo survivors when opt-in is off", async () => {
    const delivered: ClaimSkill[] = [{ name: "ci-cd-norms", description: "cicd.", body: "# CICD\n" }];
    writeRepoSkill("ignored", "repo-skill", "repo.", "body"); // present but flag off
    const out = await prepareSkillPlugin({ skills: delivered, repoSkillsEnabled: false, worktreePath: worktree }, caps);
    assert.deepEqual(out.runSkills.map((s) => s.name), ["ci-cd-norms"]);
    assert.deepEqual(out.repoSurvivorNames, []);
    assert.deepEqual(loadedNames(), ["ci-cd-norms"], "only the delivered skill is on disk");
    assert.equal(out.pluginPath, skillsPluginDir(worktree));
  });

  it("appends repo skills at lowest precedence when opted in", async () => {
    const delivered: ClaimSkill[] = [{ name: "team-kb", description: "kb.", body: "# KB\n" }];
    writeRepoSkill("deploy-notes", "deploy-notes", "deploy.", "# Deploy");
    const out = await prepareSkillPlugin({ skills: delivered, repoSkillsEnabled: true, worktreePath: worktree }, caps);
    assert.deepEqual(out.runSkills.map((s) => s.name), ["team-kb", "deploy-notes"], "delivered first, repo last");
    assert.deepEqual(out.repoSurvivorNames, ["deploy-notes"]);
    assert.deepEqual(loadedNames(), ["deploy-notes", "team-kb"]);
  });

  it("drops a repo skill colliding with a delivered skill (repo is lowest precedence)", async () => {
    const delivered: ClaimSkill[] = [{ name: "team-kb", description: "delivered.", body: "# Delivered\n" }];
    writeRepoSkill("shadow", "team-kb", "repo shadow.", "# Repo");
    const out = await prepareSkillPlugin({ skills: delivered, repoSkillsEnabled: true, worktreePath: worktree }, caps);
    assert.deepEqual(out.runSkills.map((s) => s.name), ["team-kb"]);
    assert.equal(out.runSkills[0]!.body, "# Delivered\n", "the delivered body wins");
    assert.deepEqual(out.repoSurvivorNames, []);
    assert.ok(out.drops.some((d) => d.name === "team-kb" && d.reason === "repo_collision"));
  });

  it("evicts repo skills first when over the per-run cap", async () => {
    const delivered: ClaimSkill[] = [{ name: "keep", description: "d.", body: "b\n" }];
    writeRepoSkill("evictme", "evictme", "r.", "body");
    const out = await prepareSkillPlugin({ skills: delivered, repoSkillsEnabled: true, worktreePath: worktree }, { maxBytes: 65536, maxPerRun: 1 });
    assert.deepEqual(out.runSkills.map((s) => s.name), ["keep"]);
    assert.deepEqual(out.repoSurvivorNames, [], "the evicted repo skill is not a survivor");
    assert.deepEqual(loadedNames(), ["keep"]);
    assert.ok(out.drops.some((d) => d.name === "evictme" && d.reason === "over_limit"));
  });
});
