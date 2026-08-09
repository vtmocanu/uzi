import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  REPO_INSTRUCTIONS_MAX_BYTES,
  readRepoInstructions,
  repoInstructionsPath,
} from "../src/repo-instructions.js";

// PRD #246 M2 — the structural sanitizer for the clone's ROOT CLAUDE.md. Root file
// only, symlink never followed, size-capped, line-leading @-imports stripped, CRLF
// normalized. Safety is structure + framing, NOT prose filtering (see the file's own
// trust-model comment); these tests pin the structural transforms only.
describe("readRepoInstructions (PRD #246 M2)", () => {
  let clone: string;
  beforeEach(() => {
    clone = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-repoinstr-"));
  });
  afterEach(() => {
    fs.rmSync(clone, { recursive: true, force: true });
  });

  const write = (body: string) => fs.writeFileSync(repoInstructionsPath(clone), body);

  it("absent file ⇒ dropped: absent", async () => {
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "absent" });
  });

  it("root-only: repoInstructionsPath is <clone>/CLAUDE.md", () => {
    assert.strictEqual(repoInstructionsPath(clone), path.join(clone, "CLAUDE.md"));
  });

  it("oversized (> 64 KiB) ⇒ dropped: too_large", async () => {
    write("x".repeat(REPO_INSTRUCTIONS_MAX_BYTES + 1));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "too_large" });
  });

  it("exactly at the cap is read (not dropped)", async () => {
    const body = "y".repeat(REPO_INSTRUCTIONS_MAX_BYTES);
    write(body);
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.strictEqual(result.text, body);
  });

  it("a symlinked CLAUDE.md is NEVER read ⇒ dropped: symlinked", async () => {
    // A real target OUTSIDE the clone, and CLAUDE.md is a symlink to it. lstat must
    // see the symlink and refuse, exactly like repo-skills.ts refuses a symlinked
    // SKILL.md — so a hostile repo cannot redirect the read out of its tree.
    const outside = path.join(os.tmpdir(), `uzi-repoinstr-target-${process.pid}-${Date.now()}.md`);
    fs.writeFileSync(outside, "# secret outside the clone\n");
    try {
      fs.symlinkSync(outside, repoInstructionsPath(clone));
      assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
    } finally {
      fs.rmSync(outside, { force: true });
    }
  });

  it("a directory named CLAUDE.md ⇒ dropped: symlinked (non-regular file)", async () => {
    fs.mkdirSync(repoInstructionsPath(clone));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
  });

  it("line-leading @-import lines are stripped to a visible marker; prose survives", async () => {
    write("# Conventions\n@./secrets.md\nRun the gate before every push.\n@docs/internal.md\n   @~/home/rc\nDone.\n");
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    const text = result.text;
    // Every @-import path is gone.
    assert.ok(!text.includes("@./secrets.md"));
    assert.ok(!text.includes("@docs/internal.md"));
    assert.ok(!text.includes("@~/home/rc"));
    // Replaced with an auditable marker, one per stripped line.
    assert.strictEqual(text.match(/<!-- uzi: @-import stripped -->/g)?.length, 3);
    // Normal prose is untouched.
    assert.ok(text.includes("# Conventions"));
    assert.ok(text.includes("Run the gate before every push."));
    assert.ok(text.includes("Done."));
  });

  it("CRLF is normalized to LF", async () => {
    write("# Title\r\nline one\r\nline two\r\n");
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.ok(!result.text.includes("\r"));
    assert.strictEqual(result.text, "# Title\nline one\nline two\n");
  });

  it("an inline @ref mid-line survives (documented: inert, we read the file ourselves)", async () => {
    write("Ask @teammate before you deploy. See config@v2.\n");
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.ok(result.text.includes("Ask @teammate before you deploy."));
    assert.ok(result.text.includes("config@v2"));
    assert.ok(!result.text.includes("@-import stripped"));
  });

  it("an empty/whitespace-only file is returned as text, not a drop", async () => {
    write("   \n\t\n");
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
  });

  it("the size cap is 64 KiB", () => {
    assert.strictEqual(REPO_INSTRUCTIONS_MAX_BYTES, 64 * 1024);
  });
});
