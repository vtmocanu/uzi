import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import {
  collectRepoSkills,
  DROP_REPO_INVALID,
  DROP_SHADOWED_BY_CLAUDE,
  enumerateRepoSkills,
  repoAgentsSkillsDir,
  repoSkillsDir,
} from "../src/repo-skills.js";
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

/** Write <clone>/.agents/skills/<dir>/SKILL.md (the cross-agent root, #1205). */
function writeAgentsSkill(dir: string, content: string): void {
  const d = path.join(repoAgentsSkillsDir(clone), dir);
  fs.mkdirSync(d, { recursive: true });
  fs.writeFileSync(path.join(d, "SKILL.md"), content, "utf8");
}

async function enumerate(maxBytes = 65536) {
  return enumerateRepoSkills(repoSkillsDir(clone), maxBytes);
}

async function collect(maxBytes = 65536) {
  return collectRepoSkills(clone, maxBytes);
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

// Issue #1205: `.agents/skills` (the Codex root) is enumerated as a SECOND real
// root under the identical rules, with `.claude/skills` winning a real-vs-real
// name collision. The canonical cross-agent layout keeps real bodies under
// `.agents/skills` and projects `.claude/skills` as a symlink.
describe("collectRepoSkills (dual-root .claude/skills + .agents/skills)", () => {
  it("enumerates a real .agents/skills skill when there is no .claude/skills", async () => {
    writeAgentsSkill("codex-only", "---\nname: codex-only\ndescription: from agents.\n---\n\nbody\n");
    const { skills, dropped } = await collect();
    assert.deepEqual(skills.map((s) => s.name), ["codex-only"]);
    assert.equal(skills[0]!.description, "from agents.");
    assert.deepEqual(dropped, []);
    // The old single-root path (.claude/skills only) misses it — this is the bug.
    const legacy = await enumerate();
    assert.deepEqual(legacy.skills, [], ".claude/skills-only enumeration cannot see .agents/skills");
  });

  it("returns nothing when .agents/skills itself is a symlink (containment guard)", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-agents-"));
    fs.mkdirSync(path.join(outside, "x"));
    fs.writeFileSync(path.join(outside, "x", "SKILL.md"), "---\nname: x\ndescription: y.\n---\n\nb\n");
    try {
      fs.mkdirSync(path.join(clone, ".agents"), { recursive: true });
      fs.symlinkSync(outside, path.join(clone, ".agents", "skills"));
      const { skills } = await collect();
      assert.deepEqual(skills, [], "a symlinked .agents/skills dir is never enumerated");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("skips a symlinked skill dir or SKILL.md under .agents/skills", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-agents2-"));
    fs.writeFileSync(path.join(outside, "SKILL.md"), "---\nname: leaked\ndescription: secret.\n---\n\nbody\n");
    try {
      writeAgentsSkill("legit", "---\nname: legit\ndescription: ok.\n---\n\nbody\n");
      fs.symlinkSync(outside, path.join(repoAgentsSkillsDir(clone), "linked-dir"));
      fs.mkdirSync(path.join(repoAgentsSkillsDir(clone), "linked-file"));
      fs.symlinkSync(
        path.join(outside, "SKILL.md"),
        path.join(repoAgentsSkillsDir(clone), "linked-file", "SKILL.md"),
      );
      const { skills } = await collect();
      assert.deepEqual(skills.map((s) => s.name), ["legit"], "symlinked dir/file must not be read");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("canonical layout: real .agents/skills + `.claude/skills -> ../.agents/skills` symlink yields the skills", async () => {
    // The exact shape PR #1204 moved this repo to. The `.claude/skills` symlink is
    // rejected by the guard; the real `.agents/skills` side supplies the skills.
    writeAgentsSkill("deploy", "---\nname: deploy\ndescription: how we deploy.\n---\n\nbody\n");
    writeAgentsSkill("review", "---\nname: review\ndescription: how we review.\n---\n\nbody\n");
    fs.mkdirSync(path.join(clone, ".claude"), { recursive: true });
    fs.symlinkSync("../.agents/skills", path.join(clone, ".claude", "skills"));

    const { skills, dropped } = await collect();
    assert.deepEqual(skills.map((s) => s.name), ["deploy", "review"]);
    assert.deepEqual(dropped, []);
    // Acceptance criterion: SAME result a real `.claude/skills/<name>` tree gives.
    // The old single-root path over the symlinked `.claude/skills` sees nothing.
    const legacy = await enumerate();
    assert.deepEqual(legacy.skills, [], "the symlinked .claude/skills projection is (correctly) empty on its own");
  });

  it("real-vs-real collision: .claude/skills wins and the .agents/skills copy is dropped", async () => {
    writeSkill("dup", "---\nname: dup\ndescription: claude wins.\n---\n\nclaude body\n");
    writeAgentsSkill("dup", "---\nname: dup\ndescription: agents copy.\n---\n\nagents body\n");
    // A non-colliding .agents skill still comes through, proving the drop is scoped.
    writeAgentsSkill("solo", "---\nname: solo\ndescription: only in agents.\n---\n\nbody\n");

    const { skills, dropped } = await collect();
    assert.deepEqual(skills.map((s) => s.name), ["dup", "solo"]);
    const dup = skills.find((s) => s.name === "dup")!;
    assert.equal(dup.description, "claude wins.", ".claude/skills is the surviving copy");
    assert.ok(dup.body.includes("claude body"));
    assert.deepEqual(dropped, [{ name: "dup", reason: DROP_SHADOWED_BY_CLAUDE }]);
  });

  it("returns nothing when neither root exists", async () => {
    const { skills, dropped } = await collect();
    assert.deepEqual(skills, []);
    assert.deepEqual(dropped, []);
  });

  // Parent-symlink containment: `lstat` refuses a symlink at the FINAL component
  // but FOLLOWS a symlinked parent, so `.agents -> <outside>` (or `.claude -> …`)
  // must be rejected too, or enumeration escapes the clone even though
  // `<clone>/.agents/skills` lstats as a real directory (issue #1205 follow-up).
  it("returns nothing when the .agents PARENT itself is a symlink (parent-follow escape)", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-agents-parent-"));
    fs.mkdirSync(path.join(outside, "skills", "leaked"), { recursive: true });
    fs.writeFileSync(
      path.join(outside, "skills", "leaked", "SKILL.md"),
      "---\nname: leaked\ndescription: escaped the clone.\n---\n\nsecret\n",
    );
    try {
      // `<clone>/.agents` -> outside, whose real `skills/leaked/SKILL.md` would
      // be read if only the final `skills` component were guarded.
      fs.symlinkSync(outside, path.join(clone, ".agents"));
      const { skills } = await collect();
      assert.deepEqual(skills, [], "a symlinked .agents parent must not be followed");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });

  it("returns nothing when the .claude PARENT itself is a symlink (parent-follow escape)", async () => {
    const outside = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-outside-claude-parent-"));
    fs.mkdirSync(path.join(outside, "skills", "leaked"), { recursive: true });
    fs.writeFileSync(
      path.join(outside, "skills", "leaked", "SKILL.md"),
      "---\nname: leaked\ndescription: escaped the clone.\n---\n\nsecret\n",
    );
    try {
      fs.symlinkSync(outside, path.join(clone, ".claude"));
      const { skills } = await collect();
      assert.deepEqual(skills, [], "a symlinked .claude parent must not be followed");
    } finally {
      fs.rmSync(outside, { recursive: true, force: true });
    }
  });
});
