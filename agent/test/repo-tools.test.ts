import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import fssync from "node:fs";
import os from "node:os";
import path from "node:path";
import { extractRepoDevboxPackages, mergeToolPackages } from "../src/repo-tools.js";

let dir: string;
beforeEach(async () => {
  dir = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-repo-tools-"));
});
afterEach(async () => {
  await fs.rm(dir, { recursive: true, force: true });
});

async function writeDevbox(content: string): Promise<void> {
  await fs.writeFile(path.join(dir, "devbox.json"), content, "utf8");
}

describe("extractRepoDevboxPackages (packages-only, hooks never run)", () => {
  it("HOSTILE MANIFEST: extracts only packages; init_hook + scripts are ignored, never executed", async () => {
    // A sentinel a hook WOULD create if anything executed the manifest.
    const pwned = path.join(dir, "PWNED");
    await writeDevbox(
      JSON.stringify({
        packages: ["kubectl@1.31", "hello", "github:NixOS/nixpkgs#evil"],
        shell: {
          init_hook: `touch ${pwned}`,
          scripts: { build: `touch ${pwned}` },
        },
        env: { SECRET: "$(cat /run/secrets/worker_token)" },
      }),
    );

    const pkgs = await extractRepoDevboxPackages(dir);

    // Only the shape-valid packages; the flake ref is dropped.
    assert.deepStrictEqual(pkgs, ["kubectl@1.31", "hello"]);
    // The hook/script never ran — pure JSON extraction, no shell.
    assert.strictEqual(fssync.existsSync(pwned), false, "an init_hook/script must NEVER execute");
  });

  it("supports the object form (name + optional version)", async () => {
    await writeDevbox(JSON.stringify({ packages: { python3: { version: "3.11" }, jq: "latest", bad: { x: 1 } } }));
    const pkgs = await extractRepoDevboxPackages(dir);
    assert.deepStrictEqual(pkgs.sort(), ["bad", "jq@latest", "python3@3.11"].sort());
  });

  it("returns [] for a missing or malformed devbox.json", async () => {
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), []); // no file
    await writeDevbox("{ not json");
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), []);
    await writeDevbox(JSON.stringify({ shell: { init_hook: "x" } })); // no packages
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), []);
  });

  it("dedupes and caps the count", async () => {
    const many = Array.from({ length: 100 }, (_, i) => `pkg${i}`);
    await writeDevbox(JSON.stringify({ packages: [...many, "pkg0", "pkg0"] }));
    const pkgs = await extractRepoDevboxPackages(dir);
    assert.strictEqual(pkgs.length, 64);
    assert.strictEqual(new Set(pkgs).size, 64);
  });

  it("returns [] for an oversized devbox.json without loading it (size guard, audit L1)", async () => {
    // Valid JSON, but larger than the 1 MiB stat ceiling: rejected before readFile.
    const filler = "x".repeat(1024 * 1024 + 1);
    await writeDevbox(JSON.stringify({ packages: ["hello"], _pad: filler }));
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), []);
  });

  it("returns [] for a symlinked device, never an unbounded read (isFile guard, audit)", async () => {
    // stat() follows the symlink; a char device (/dev/zero) reports size 0 and
    // would pass a size-only check, then hang readFile on its endless stream. The
    // isFile() guard rejects it first. (If /dev/zero is absent, stat throws → [].)
    await fs.symlink("/dev/zero", path.join(dir, "devbox.json"));
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), []);
  });
});

describe("mergeToolPackages (tier-1 wins conflicts)", () => {
  it("unions, with tier-1 winning a version conflict on the same base name", () => {
    const merged = mergeToolPackages(["kubectl@1.31", "jq"], ["kubectl@9.9", "terraform@1.7", "jq@1.6"]);
    // kubectl + jq are tier-1's; terraform is the only surviving tier-2 add.
    assert.deepStrictEqual(merged, ["kubectl@1.31", "jq", "terraform@1.7"]);
  });

  it("returns tier-1 unchanged when tier-2 is empty, and tier-2 when tier-1 is empty", () => {
    assert.deepStrictEqual(mergeToolPackages(["a"], []), ["a"]);
    assert.deepStrictEqual(mergeToolPackages([], ["b@1"]), ["b@1"]);
  });

  it("dedupes two tier-2 versions of the same base — first wins (audit dedupe nit)", () => {
    // Both entries share base "node"; only the first survives, so provisioning
    // never gets two conflicting versions of one package.
    assert.deepStrictEqual(mergeToolPackages([], ["node@20", "node@22", "jq"]), ["node@20", "jq"]);
  });
});
