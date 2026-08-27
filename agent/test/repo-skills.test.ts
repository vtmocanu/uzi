import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { DROP_REPO_INVALID, enumerateRepoSkills, repoSkillsDir } from "../src/repo-skills.js";
import { DROP_TOO_LARGE } from "../src/skills-plugin.js";

let clone: string;
beforeEach(() => {
  clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-repo-"));
});
afterEach(() => {
  fs.rmSync(clone, { recursive: true, force: true });
});

/** Write <clone>/.claude/skills/<dir>/SKILL.md. */
function writeSkill(dir: string, content: string): void {
  const d = path.join(repoSkillsDir(clone), dir);
  fs.mkdirSync(d, { recursive: true });
  fs.writeFileSync(path.join(d, "SKILL.md"), content, "utf8");
}

async function enumerate(maxBytes = 65536) {
  return enumerateRepoSkills(repoSkillsDir(clone), maxBytes);
}

describe("enumerateRepoSkills", () => {
  it("returns nothing when the repo has no .claude/skills", async () => {
    const { skills, dropped } = await enumerate();
    assert.deepEqual(skills, []);
    assert.deepEqual(dropped, []);
  });

  it("parses name+description and STRIPS every other frontmatter key", async () => {
    // allowed-tools / model / hooks are capability-granting keys — the security
    // point of M6 is that they never survive into the loaded skill.
    writeSkill(
      "deploy-notes",
      [
        "---",
        "name: deploy-notes",
        "description: how we deploy.",
        "allowed-tools: Bash, Write, Edit",
        "model: opus",
        "hooks: evil",
        "---",
        "",
        "# Deploy",
        "steps",
        "",
      ].join("\n"),
    );
    const { skills, dropped } = await enumerate();
    assert.equal(dropped.length, 0);
    assert.equal(skills.length, 1);
    // ClaimSkill carries ONLY name/description/body — allowed-tools cannot ride
    // along structurally, and the body preserves content below the frontmatter.
    assert.deepEqual(
      { name: skills[0]!.name, description: skills[0]!.description },
      { name: "deploy-notes", description: "how we deploy." },
    );
    assert.ok(skills[0]!.body.includes("# Deploy"));
    assert.ok(!JSON.stringify(skills[0]).includes("allowed-tools"));
  });

  it("drops a skill whose name fails the regex (uppercase, underscores, traversal, dots)", async () => {
    writeSkill("bad", "---\nname: Bad_Name\ndescription: x.\n---\n\nbody\n");
    writeSkill("evil", "---\nname: ../escape\ndescription: x.\n---\n\nbody\n");
    writeSkill("dot", "---\nname: ..\ndescription: x.\n---\n\nbody\n");
    const { skills, dropped } = await enumerate();
    assert.equal(skills.length, 0);
    assert.equal(dropped.length, 3);
    assert.ok(dropped.every((d) => d.reason === DROP_REPO_INVALID));
  });

  it("drops a name with a colon (would break the uzi:<name> qualifier) or a space", async () => {
    // A colon in the name would split the SDK's plugin:skill enable-list token;
    // the regex forbids it (and every non-[a-z0-9-] char), so both are dropped.
    writeSkill("colon", "---\nname: foo:bar\ndescription: x.\n---\n\nbody\n");
    writeSkill("space", "---\nname: foo bar\ndescription: x.\n---\n\nbody\n");
    const { skills, dropped } = await enumerate();
    assert.equal(skills.length, 0);
    assert.deepEqual(
      dropped.map((d) => d.reason).sort(),
      [DROP_REPO_INVALID, DROP_REPO_INVALID],
    );
  });

  it("drops a skill with an empty description or empty body", async () => {
    writeSkill("nodesc", "---\nname: nodesc\ndescription: \n---\n\nbody\n");
    writeSkill("nobody", "---\nname: nobody\ndescription: x.\n---\n\n");
    const { skills, dropped } = await enumerate();
    assert.equal(skills.length, 0);
    assert.equal(dropped.length, 2);
    assert.ok(dropped.every((d) => d.reason === DROP_REPO_INVALID));
  });

  it("drops a skill whose SKILL.md exceeds maxBytes without loading it wholesale", async () => {
    writeSkill("big", "---\nname: big\ndescription: x.\n---\n\n" + "x".repeat(500));
    const { skills, dropped } = await enumerate(50);
    assert.equal(skills.length, 0);
    assert.deepEqual(dropped, [{ name: "big", reason: DROP_TOO_LARGE }]);
  });

  it("never follows a symlinked skills dir, skill dir, or SKILL.md", async () => {
    // A real target outside the clone the symlinks try to reach.
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-"));
    fs.writeFileSync(path.join(outside, "SKILL.md"), "---\nname: leaked\ndescription: secret.\n---\n\nbody\n");
    try {
      // A legit skill, plus a skill DIR that is a symlink to `outside`, plus a
      // SKILL.md that is a symlink to outside's file.
      writeSkill("legit", "---\nname: legit\ndescription: ok.\n---\n\nbody\n");
      fs.symlinkSync(outside, path.join(repoSkillsDir(clone), "linked-dir"));
      fs.mkdirSync(path.join(repoSkillsDir(clone), "linked-file"));
      fs.symlinkSync(path.join(outside, "SKILL.md"), path.join(repoSkillsDir(clone), "linked-file", "SKILL.md"));

      const { skills } = await enumerate();
      assert.deepEqual(skills.map((s) => s.name), ["legit"], "symlinked dir/file must not be read");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("returns nothing when .claude/skills itself is a symlink", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside2-"));
    fs.mkdirSync(path.join(outside, "x"));
    fs.writeFileSync(path.join(outside, "x", "SKILL.md"), "---\nname: x\ndescription: y.\n---\n\nb\n");
    try {
      fs.mkdirSync(path.join(clone, ".claude"), { recursive: true });
      fs.symlinkSync(outside, path.join(clone, ".claude", "skills"));
      const { skills } = await enumerate();
      assert.deepEqual(skills, [], "a symlinked skills dir is never enumerated");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("sorts results by name", async () => {
    writeSkill("zeta", "---\nname: zeta\ndescription: z.\n---\n\nb\n");
    writeSkill("alpha", "---\nname: alpha\ndescription: a.\n---\n\nb\n");
    const { skills } = await enumerate();
    assert.deepEqual(skills.map((s) => s.name), ["alpha", "zeta"]);
  });
});
