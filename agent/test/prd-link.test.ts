// PRD #362 M3b — PRD-link resolver + path-traversal guard.
//
// Security-sensitive: the clone is attacker-influenceable. These tests pin the
// accept/reject SETS of the ported validator and the end-to-end resolver against
// a temp dir standing in for a clone, and assert that no file outside the clone
// is ever read (traversal + symlink escape return the nulls fallback).
//
// Control bytes in the "rejects control bytes" case are written as ESCAPE
// SEQUENCES (\u0000 NUL, \t TAB, \u007f DEL), not raw bytes, so this
// security-critical file stays UTF-8 text: diffable in review and greppable.

import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import { symlinkSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { performance } from "node:perf_hooks";

import { validatePrdPath, findValidPrdCore, resolvePrdInput } from "../src/prd-link.js";

// A warn sink that records calls so tests can assert a fallback logged a reason.
function capturingLogger(): { warn: (m: string, f?: Record<string, unknown>) => void; calls: string[] } {
  const calls: string[] = [];
  return { warn: (m) => calls.push(m), calls };
}

const PRD_BODY = "# PRD 362\n\nThis is the resolved PRD content.\n";

describe("validatePrdPath (port of Go prdpath.Validate)", () => {
  it("accepts well-formed clone-relative PRD paths", () => {
    assert.equal(validatePrdPath("prds/362-x.md"), true);
    assert.equal(validatePrdPath("prds/done/362-x.md"), true);
    assert.equal(validatePrdPath("prds/a_b-c.2.md"), true);
  });

  it("rejects empty and oversized paths", () => {
    assert.equal(validatePrdPath(""), false);
    assert.equal(validatePrdPath("prds/" + "a".repeat(600) + ".md"), false);
  });

  it("rejects an oversize path by BYTE length, not char length (multibyte)", () => {
    // `é` is 2 UTF-8 bytes but JS length 1. Build a path whose .length <= 512
    // but whose Buffer.byteLength > 512. The byte-length check runs BEFORE the
    // segment-charset check in validatePrdPath, so the length rule is exactly
    // what rejects this — pinning that the measurement is bytes, not chars (a
    // regression to `p.length` would wrongly accept it).
    const p = "prds/" + "é".repeat(300) + ".md";
    assert.ok(p.length <= 512); // char length is within the cap …
    assert.ok(Buffer.byteLength(p, "utf8") > 512); // … but byte length exceeds it
    assert.equal(validatePrdPath(p), false);
  });

  it("rejects control bytes, DEL, and backslash", () => {
    assert.equal(validatePrdPath("prds/a\u0000b.md"), false); // NUL
    assert.equal(validatePrdPath("prds/a\tb.md"), false); // TAB (<0x20)
    assert.equal(validatePrdPath("prds/a\u007fb.md"), false); // DEL
    assert.equal(validatePrdPath("prds\\..\\x.md"), false); // backslash smuggling
  });

  it("rejects non-rooted and non-.md paths", () => {
    assert.equal(validatePrdPath("/prds/x.md"), false); // absolute
    assert.equal(validatePrdPath("docs/x.md"), false); // wrong root
    assert.equal(validatePrdPath("prds/x.txt"), false); // wrong ext
    assert.equal(validatePrdPath("prds/x.md.bak"), false); // wrong ext
  });

  it("rejects traversal by each of the three independent rules", () => {
    // `..` segment / dotfile-prefix / normalize each reject these.
    assert.equal(validatePrdPath("prds/../x.md"), false);
    assert.equal(validatePrdPath("prds/../../etc/passwd.md"), false);
    assert.equal(validatePrdPath("prds/./x.md"), false); // `.` segment + normalize
    assert.equal(validatePrdPath("prds//x.md"), false); // empty segment + normalize
  });

  it("rejects dotfiles (e.g. .git)", () => {
    assert.equal(validatePrdPath("prds/.git/config.md"), false);
    assert.equal(validatePrdPath("prds/.md"), false); // held only by the dotfile rule
  });

  it("rejects illegal segment characters", () => {
    assert.equal(validatePrdPath("prds/a b.md"), false); // space
    assert.equal(validatePrdPath("prds/a:b.md"), false); // colon
  });
});

describe("findValidPrdCore (detect span → extract core → validate)", () => {
  it("finds a bare link core", () => {
    assert.equal(findValidPrdCore("see prds/362-x.md for details"), "prds/362-x.md");
  });

  it("reduces GitHub and GitLab blob URLs (and #/? suffixes) to the same core", () => {
    const core = "prds/362-x.md";
    assert.equal(
      findValidPrdCore("https://github.com/vtmocanu/uzi/blob/main/prds/362-x.md"),
      core,
    );
    assert.equal(
      findValidPrdCore("https://gitlab.com/x/y/-/blob/main/prds/362-x.md"),
      core,
    );
    assert.equal(
      findValidPrdCore("https://github.com/vtmocanu/uzi/blob/main/prds/362-x.md#L4"),
      core,
    );
    assert.equal(
      findValidPrdCore("https://gitlab.com/x/y/-/blob/main/prds/362-x.md?ref=main"),
      core,
    );
  });

  it("returns null when a detected span fails validation (traversal)", () => {
    assert.equal(findValidPrdCore("prds/../secret.md"), null);
    assert.equal(findValidPrdCore("prds/.git/config.md"), null);
  });

  it("returns null when there is no link at all", () => {
    assert.equal(findValidPrdCore("no prd here, just prose about /etc/passwd"), null);
    assert.equal(findValidPrdCore(""), null);
  });

  it("finds a valid core even when an invalid one appears first", () => {
    assert.equal(
      findValidPrdCore("bad prds/../x.md then good prds/362-x.md"),
      "prds/362-x.md",
    );
  });

  // Code review PR #387, finding 7: a body that mentions another PRD before its own.
  it("prefers the core whose number matches preferIid over document order", () => {
    const body = "Unlike prds/100-old.md, this implements prds/362-new.md";
    // Without the hint, the first valid core wins (unchanged behavior).
    assert.equal(findValidPrdCore(body), "prds/100-old.md");
    // With the issue iid, the matching PRD wins even though it appears second.
    assert.equal(findValidPrdCore(body, 362), "prds/362-new.md");
  });

  it("matches preferIid against a done/ archived core, and falls back on no match", () => {
    const body = "see prds/done/362-x.md and prds/401-y.md";
    assert.equal(findValidPrdCore(body, 362), "prds/done/362-x.md");
    // No core matches iid 999 → first valid core (document order) is returned.
    assert.equal(findValidPrdCore(body, 999), "prds/done/362-x.md");
  });

  it("ignores preferIid when the only link is a different PRD (single-link case)", () => {
    // A lone link resolves the same with or without a mismatching hint.
    assert.equal(findValidPrdCore("see prds/362-x.md", 5), "prds/362-x.md");
  });

  it("returns null FAST on hostile blob-like input (ReDoS guard)", () => {
    // The old blob-prefix regex straddled two greedy classes across the `blob/`
    // literal and backtracked O(n²) on this shape (~64s at 500k chars). The
    // de-nested core-only pattern scans it linearly; the input carries no
    // `prds/…*.md` core, so the result is null and returns fast.
    const hostile = "https://" + "a/blob/x/".repeat(20000);
    const start = performance.now();
    const core = findValidPrdCore(hostile);
    const elapsed = performance.now() - start;
    assert.equal(core, null);
    // Generous bound to avoid CI flake; the real proof is the de-nested regex.
    assert.ok(elapsed < 500, `detector took ${elapsed}ms (expected < 500ms)`);
  });
});

describe("resolvePrdInput", () => {
  let clone: string;
  let outsideSecret: string;

  before(async () => {
    clone = await fs.mkdtemp(path.join(os.tmpdir(), "prd-clone-"));
    await fs.mkdir(path.join(clone, "prds"), { recursive: true });
    await fs.writeFile(path.join(clone, "prds", "362-x.md"), PRD_BODY, "utf8");
    // A secret OUTSIDE the clone — no traversal/symlink case may ever read it.
    outsideSecret = path.join(os.tmpdir(), `prd-outside-secret-${process.pid}.md`);
    await fs.writeFile(outsideSecret, "TOP SECRET must never be read", "utf8");
  });

  after(async () => {
    await fs.rm(clone, { recursive: true, force: true });
    await fs.rm(outsideSecret, { force: true });
  });

  it("resolves and reads a valid bare link", async () => {
    const r = await resolvePrdInput("implements prds/362-x.md", clone);
    assert.equal(r.prdPath, "prds/362-x.md");
    assert.equal(r.prdText, PRD_BODY);
  });

  // Code review PR #387, finding 7: with two PRD links, the issue iid picks the right
  // file to READ, not just the right core to name.
  it("reads the iid-matching PRD when the body mentions another PRD first", async () => {
    await fs.writeFile(path.join(clone, "prds", "100-old.md"), "OLD PRD BODY", "utf8");
    const desc = "Supersedes prds/100-old.md; implements prds/362-x.md";
    // Issue #362 → reads 362-x.md despite 100-old.md appearing first.
    const matched = await resolvePrdInput(desc, clone, 362);
    assert.equal(matched.prdPath, "prds/362-x.md");
    assert.equal(matched.prdText, PRD_BODY);
    // No iid hint → first valid core (100-old.md), the pre-#7 behavior.
    const firstWins = await resolvePrdInput(desc, clone);
    assert.equal(firstWins.prdPath, "prds/100-old.md");
    assert.equal(firstWins.prdText, "OLD PRD BODY");
    await fs.rm(path.join(clone, "prds", "100-old.md"), { force: true });
  });

  it("resolves the same core from GitHub and GitLab blob URLs (+ suffix)", async () => {
    for (const desc of [
      "https://github.com/vtmocanu/uzi/blob/main/prds/362-x.md",
      "https://gitlab.com/x/y/-/blob/main/prds/362-x.md#L4",
      "prose then https://github.com/vtmocanu/uzi/blob/main/prds/362-x.md?ref=main end",
    ]) {
      const r = await resolvePrdInput(desc, clone);
      assert.equal(r.prdPath, "prds/362-x.md", desc);
      assert.equal(r.prdText, PRD_BODY, desc);
    }
  });

  it("rejects traversal attempts and reads no file outside the clone", async () => {
    const attempts = [
      "prds/../../../etc/passwd", // no .md — never matches/validates
      "prds/../secret.md", // ends in .md, reaches the traversal rules
      "prds/../../etc/passwd.md",
      "look at /etc/passwd", // absolute, non-prds
      "prds\\..\\x.md", // backslash
      "prds/.git/config.md", // dotfile
    ];
    for (const desc of attempts) {
      const log = capturingLogger();
      const r = await resolvePrdInput(desc, clone, undefined, log);
      assert.deepEqual(r, { prdPath: null, prdText: null }, desc);
      assert.notEqual(r.prdText, "TOP SECRET must never be read", desc);
    }
  });

  it("returns nulls FAST on hostile blob-like input (no core, ReDoS guard)", async () => {
    // End-to-end proof that the untrusted path this module hardens cannot be
    // driven into pathological backtracking: no core → nulls, and fast.
    const hostile = "https://" + "a/blob/x/".repeat(20000);
    const start = performance.now();
    const r = await resolvePrdInput(hostile, clone);
    const elapsed = performance.now() - start;
    assert.deepEqual(r, { prdPath: null, prdText: null });
    assert.ok(elapsed < 500, `resolve took ${elapsed}ms (expected < 500ms)`);
  });

  it("catches a symlink escape via realpath containment", async (t) => {
    // prds/evil.md is a valid-looking core, but symlinks OUT of the clone.
    const link = path.join(clone, "prds", "evil.md");
    try {
      symlinkSync(outsideSecret, link);
    } catch (err) {
      t.skip(`symlink not creatable in this env: ${(err as Error).message}`);
      return;
    }
    try {
      const log = capturingLogger();
      const r = await resolvePrdInput("see prds/evil.md", clone, undefined, log);
      assert.deepEqual(r, { prdPath: null, prdText: null });
      assert.ok(log.calls.some((m) => m.includes("symlink") || m.includes("escape")));
    } finally {
      await fs.rm(link, { force: true });
    }
  });

  it("falls back to nulls for a valid-looking but missing file", async () => {
    const log = capturingLogger();
    const r = await resolvePrdInput("see prds/nope.md", clone, undefined, log);
    assert.deepEqual(r, { prdPath: null, prdText: null });
    assert.ok(log.calls.length > 0);
  });

  it("returns nulls when there is no link at all", async () => {
    const r = await resolvePrdInput("just an issue with no prd reference", clone);
    assert.deepEqual(r, { prdPath: null, prdText: null });
  });

  it("caps an oversized file read", async () => {
    const big = "a".repeat(300 * 1024); // > 256 KiB
    await fs.writeFile(path.join(clone, "prds", "big.md"), big, "utf8");
    const r = await resolvePrdInput("see prds/big.md", clone);
    assert.equal(r.prdPath, "prds/big.md");
    assert.equal(r.prdText?.length, 256 * 1024);
  });

  it("never throws — returns nulls even for a garbage cloneDir", async () => {
    // path.resolve on a bizarre cloneDir must not escape as a throw.
    const r = await resolvePrdInput("prds/362-x.md", "\0not-a-dir");
    assert.deepEqual(r, { prdPath: null, prdText: null });
  });
});
