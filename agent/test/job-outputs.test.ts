import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

import { OUTPUT_FILES_MAX, outputDisplayName, resolveOutputFile, validateOutputFilePaths } from "../src/job-outputs.js";

// PRD #1909 M4: the lexical and filesystem checks on the files a job may hand back.

const roots: string[] = [];
afterEach(async () => {
  while (roots.length) await fsp.rm(roots.pop()!, { recursive: true, force: true });
});
async function workspace(): Promise<{ work: string; outside: string }> {
  const d = await fsp.mkdtemp(path.join(os.tmpdir(), "uzi-joboutputs-"));
  roots.push(d);
  const work = path.join(d, "work");
  await fsp.mkdir(path.join(work, "outputs"), { recursive: true });
  await fsp.mkdir(path.join(work, "sources"));
  await fsp.mkdir(path.join(work, "inputs"));
  const outside = path.join(d, "outside.txt");
  await fsp.writeFile(outside, "secret");
  return { work, outside };
}

describe("validateOutputFilePaths", () => {
  it("accepts an absent list, and paths under outputs/ or sources/, dropping duplicates and keeping order", () => {
    assert.deepStrictEqual(validateOutputFilePaths(undefined), { ok: true, value: [] });
    assert.deepStrictEqual(validateOutputFilePaths(null), { ok: true, value: [] });
    assert.deepStrictEqual(validateOutputFilePaths(["outputs/b.txt", "sources/abc", "outputs/b.txt", "outputs/d/e.csv"]), {
      ok: true,
      value: ["outputs/b.txt", "sources/abc", "outputs/d/e.csv"],
    });
  });

  const bad: Array<[string, unknown]> = [
    ["a non-array", "outputs/a"],
    ["a non-string entry", [1]],
    ["an empty string", [""]],
    ["an absolute path", ["/etc/passwd"]],
    ["a home-relative path", ["~/x"]],
    ["a parent segment", ["outputs/../inputs/01-doc.md"]],
    ["a leading parent segment", ["../x"]],
    ["a current-directory segment", ["outputs/./a"]],
    ["an empty segment", ["outputs//a"]],
    ["a trailing slash", ["outputs/a/"]],
    ["a backslash", ["outputs\\a"]],
    ["a control character", ["outputs/a\nb"]],
    ["a NUL", ["outputs/a\0b"]],
    ["the bare directory", ["outputs"]],
    ["another workspace directory", ["inputs/01-doc.md"]],
    ["a look-alike directory", ["outputs2/a"]],
    ["a path over 1024 bytes", ["outputs/" + "a".repeat(1025)]],
    ["more than the cap", Array.from({ length: OUTPUT_FILES_MAX + 1 }, (_, i) => `outputs/${i}`)],
  ];
  for (const [name, input] of bad) {
    it(`refuses ${name}`, () => {
      assert.strictEqual(validateOutputFilePaths(input).ok, false);
    });
  }

  it("accepts exactly the cap", () => {
    const r = validateOutputFilePaths(Array.from({ length: OUTPUT_FILES_MAX }, (_, i) => `outputs/${i}`));
    assert.ok(r.ok && r.value.length === OUTPUT_FILES_MAX);
  });
});

describe("resolveOutputFile", () => {
  it("resolves a regular file to its real path with a basename display name", async () => {
    const { work } = await workspace();
    await fsp.mkdir(path.join(work, "outputs/sub"));
    await fsp.writeFile(path.join(work, "outputs/sub/r.md"), "x");
    const r = resolveOutputFile(work, "outputs/sub/r.md");
    assert.ok(r.ok);
    assert.strictEqual(r.value.real, await fsp.realpath(path.join(work, "outputs/sub/r.md")));
    assert.strictEqual(r.value.displayName, "r.md");
  });

  it("accepts a symlink that stays inside outputs/ or sources/, and refuses one that leaves them", async () => {
    const { work, outside } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/a.txt"), "a");
    await fsp.writeFile(path.join(work, "sources/s"), "s");
    await fsp.writeFile(path.join(work, "inputs/i.txt"), "i");
    await fsp.symlink(path.join(work, "outputs/a.txt"), path.join(work, "outputs/in-outputs"));
    await fsp.symlink(path.join(work, "sources/s"), path.join(work, "outputs/in-sources"));
    await fsp.symlink(outside, path.join(work, "outputs/out"));
    await fsp.symlink(path.join(work, "inputs/i.txt"), path.join(work, "outputs/to-inputs"));
    await fsp.symlink("../../outside.txt", path.join(work, "outputs/rel-out"));
    await fsp.symlink("/etc/hostname", path.join(work, "outputs/system"));
    assert.ok(resolveOutputFile(work, "outputs/in-outputs").ok);
    assert.ok(resolveOutputFile(work, "outputs/in-sources").ok);
    for (const rel of ["outputs/out", "outputs/to-inputs", "outputs/rel-out", "outputs/system"]) {
      assert.strictEqual(resolveOutputFile(work, rel).ok, false, rel);
    }
  });

  it("refuses a symlinked outputs/ directory that points elsewhere", async () => {
    const { work } = await workspace();
    const elsewhere = path.join(path.dirname(work), "elsewhere");
    await fsp.mkdir(elsewhere);
    await fsp.writeFile(path.join(elsewhere, "f.txt"), "f");
    await fsp.rm(path.join(work, "outputs"), { recursive: true });
    await fsp.symlink(elsewhere, path.join(work, "outputs"));
    assert.strictEqual(resolveOutputFile(work, "outputs/f.txt").ok, false);
  });

  it("refuses a directory, a missing file and a non-regular file", async () => {
    const { work } = await workspace();
    await fsp.mkdir(path.join(work, "outputs/d"));
    assert.strictEqual(resolveOutputFile(work, "outputs/d").ok, false);
    assert.strictEqual(resolveOutputFile(work, "outputs/none.txt").ok, false);
    try {
      execFileSync("mkfifo", [path.join(work, "outputs/pipe")]);
    } catch {
      return; // no mkfifo on this host: the directory case above already covers "not a regular file"
    }
    assert.strictEqual(resolveOutputFile(work, "outputs/pipe").ok, false, "a FIFO must never be opened for reading");
  });

  it("denies a secret path through the path guard", async () => {
    const { work } = await workspace();
    await fsp.writeFile(path.join(work, "outputs/token"), "t");
    const real = await fsp.realpath(path.join(work, "outputs/token"));
    assert.strictEqual(resolveOutputFile(work, "outputs/token", [real]).ok, false);
  });
});

describe("outputDisplayName", () => {
  it("is the basename, without control or invisible characters", () => {
    assert.strictEqual(outputDisplayName("outputs/a/b/report.pdf"), "report.pdf");
    assert.strictEqual(outputDisplayName("outputs/x‮y.txt"), "xy.txt");
    assert.strictEqual(outputDisplayName("outputs/ ​ "), "output");
    assert.strictEqual(outputDisplayName("outputs/résumé 報告.md"), "résumé 報告.md");
  });
  it("bounds a long name to 100 code points and keeps a short extension", () => {
    const n = outputDisplayName("outputs/" + "a".repeat(300) + ".csv");
    assert.strictEqual(Array.from(n).length, 100);
    assert.ok(n.endsWith(".csv"));
    assert.strictEqual(Array.from(outputDisplayName("outputs/" + "b".repeat(300))).length, 100);
  });
});
