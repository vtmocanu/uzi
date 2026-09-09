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
// only; a symlink is followed ONLY when its resolved real path is a regular file
// inside the clone tree and not under `.git/` (escaping/broken/looping/non-file/`.git/`
// symlinks are dropped); size-capped, line-leading @-imports stripped, CRLF normalized.
// Safety is structure + framing, NOT prose filtering (see the file's own trust-model
// comment); these tests pin the structural transforms only.
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

  it("a regular file lstat sees but readFile cannot open ⇒ dropped: read_error (never throws)", async (t) => {
    // lstat passes (isFile, under cap) but the read fails — e.g. EACCES on a mode-000
    // file. The reader must catch it structurally so BOTH callers stay non-fatal.
    const p = repoInstructionsPath(clone);
    write("# unreadable\n");
    fs.chmodSync(p, 0o000);
    // Running as root defeats mode bits (reads succeed regardless), so verify the
    // premise holds before asserting; otherwise skip with a clear reason.
    let reallyUnreadable = false;
    try {
      fs.readFileSync(p, "utf8");
    } catch {
      reallyUnreadable = true;
    }
    if (!reallyUnreadable) {
      fs.chmodSync(p, 0o644); // restore so afterEach cleanup can remove it
      t.skip("chmod 0o000 does not block reads here (likely running as root)");
      return;
    }
    try {
      const result = await readRepoInstructions(clone);
      assert.deepStrictEqual(result, { dropped: "read_error" });
    } finally {
      fs.chmodSync(p, 0o644); // restore so afterEach cleanup can remove it
    }
  });

  it("marker amplification over the cap ⇒ dropped: too_large (bounds the INJECTED size)", async () => {
    // A file UNDER the raw cap made entirely of 3-byte `@a` import lines. Each line is
    // replaced by the ~31-byte marker, so the sanitized text amplifies well over the
    // cap. The post-sanitization re-check must drop it, so the injected text can never
    // exceed the cap regardless of amplification.
    const line = "@a\n"; // 3 bytes raw; stripped to a ~31-byte marker
    const count = Math.floor((REPO_INSTRUCTIONS_MAX_BYTES - 1) / line.length);
    const body = line.repeat(count);
    assert.ok(Buffer.byteLength(body, "utf8") <= REPO_INSTRUCTIONS_MAX_BYTES, "raw file is under the cap");
    write(body);
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "too_large" });
  });

  it("exactly at the cap is read (not dropped)", async () => {
    const body = "y".repeat(REPO_INSTRUCTIONS_MAX_BYTES);
    write(body);
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.strictEqual(result.text, body);
  });

  it("a symlink whose target escapes the clone tree is never read ⇒ dropped: symlinked", async () => {
    // A real target OUTSIDE the clone, and CLAUDE.md is a symlink to it. The resolved
    // real path is not contained by the clone, so it is refused — a hostile repo
    // cannot redirect the read out of its tree.
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

  it("an in-tree symlink IS followed and read (CLAUDE.md -> AGENTS.md)", async () => {
    // The common convention (this repo's own layout): CLAUDE.md is a relative symlink
    // to a real, in-tree AGENTS.md. It is followed and its content read.
    const body = "# Conventions\nRun the gate before every push.\n";
    fs.writeFileSync(path.join(clone, "AGENTS.md"), body);
    fs.symlinkSync("AGENTS.md", repoInstructionsPath(clone));
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.strictEqual(result.text, body);
  });

  it("an in-tree symlink to a file in a SUBDIRECTORY is followed", async () => {
    const body = "# Nested\nInstructions live under docs/.\n";
    fs.mkdirSync(path.join(clone, "docs"));
    fs.writeFileSync(path.join(clone, "docs", "instructions.md"), body);
    fs.symlinkSync("docs/instructions.md", repoInstructionsPath(clone));
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.strictEqual(result.text, body);
  });

  it("an in-tree symlink CHAIN is followed (CLAUDE.md -> a.md -> b.md)", async () => {
    // realpath walks the whole chain; every hop stays inside the clone.
    const body = "# End of the chain\nThis is b.md.\n";
    fs.writeFileSync(path.join(clone, "b.md"), body);
    fs.symlinkSync("b.md", path.join(clone, "a.md"));
    fs.symlinkSync("a.md", repoInstructionsPath(clone));
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.strictEqual(result.text, body);
  });

  it("a relative symlink that escapes via .. ⇒ dropped: symlinked", async () => {
    // The target is a real file, but it resolves OUTSIDE the clone (a sibling under
    // tmpdir, since the clone is a direct child of tmpdir), so the containment check
    // refuses it — not the broken-link path.
    const outsideName = `uzi-repoinstr-escape-${process.pid}-${Date.now()}.md`;
    const outside = path.join(clone, "..", outsideName);
    fs.writeFileSync(outside, "# outside the clone\n");
    try {
      fs.symlinkSync(path.join("..", outsideName), repoInstructionsPath(clone));
      assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
    } finally {
      fs.rmSync(outside, { force: true });
    }
  });

  it("a broken/dangling symlink ⇒ dropped: symlinked", async () => {
    // Target does not exist, so realpath throws — never read.
    fs.symlinkSync("does-not-exist.md", repoInstructionsPath(clone));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
  });

  it("a symlink to an in-tree DIRECTORY ⇒ dropped: symlinked", async () => {
    // Contained, but the resolved target is not a regular file.
    fs.mkdirSync(path.join(clone, "subdir"));
    fs.symlinkSync("subdir", repoInstructionsPath(clone));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
  });

  it("a symlink into a name-prefix SIBLING of the clone ⇒ dropped: symlinked", async () => {
    // The escape a `realTarget.startsWith(realClone)` check would WRONGLY admit: a
    // sibling dir whose path shares the clone's name prefix (e.g. `<clone>-evil`). The
    // `path.relative` containment used here yields `../<clone>-evil/...`, so it is
    // correctly refused. Pins the exact defense the code comments call out.
    const evil = `${clone}-evil`;
    fs.mkdirSync(evil);
    const target = path.join(evil, "secret.md");
    fs.writeFileSync(target, "# outside the clone via a name-prefix sibling\n");
    try {
      fs.symlinkSync(target, repoInstructionsPath(clone)); // absolute target
      assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
    } finally {
      fs.rmSync(evil, { recursive: true, force: true });
    }
  });

  it("a symlink to a file under the clone's .git/ dir ⇒ dropped: symlinked", async () => {
    // In-tree but never legitimate instruction content: a `.git/` target is refused
    // even though it is contained, so a symlink cannot inject repo metadata.
    fs.mkdirSync(path.join(clone, ".git"));
    fs.writeFileSync(path.join(clone, ".git", "config"), "[core]\n\trepositoryformatversion = 0\n");
    fs.symlinkSync(path.join(".git", "config"), repoInstructionsPath(clone));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "symlinked" });
  });

  it("sanitization applies to the RESOLVED content of an in-tree symlink", async () => {
    // The @-import strip runs on the followed target's bytes, not the link.
    fs.writeFileSync(
      path.join(clone, "AGENTS.md"),
      "# Conventions\n@./secrets.md\nRun the gate before every push.\n",
    );
    fs.symlinkSync("AGENTS.md", repoInstructionsPath(clone));
    const result = await readRepoInstructions(clone);
    assert.ok("text" in result);
    assert.ok(!result.text.includes("@./secrets.md"));
    assert.strictEqual(result.text.match(/<!-- uzi: @-import stripped -->/g)?.length, 1);
    assert.ok(result.text.includes("Run the gate before every push."));
  });

  it("an in-tree symlink to an oversized (> 64 KiB) real file ⇒ dropped: too_large", async () => {
    // The resolved target's size is what bounds the cap, checked before reading.
    fs.writeFileSync(path.join(clone, "AGENTS.md"), "x".repeat(REPO_INSTRUCTIONS_MAX_BYTES + 1));
    fs.symlinkSync("AGENTS.md", repoInstructionsPath(clone));
    assert.deepStrictEqual(await readRepoInstructions(clone), { dropped: "too_large" });
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
