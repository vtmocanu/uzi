import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import fssync from "node:fs";
import os from "node:os";
import path from "node:path";
import { extractRepoDevboxPackages, mergeToolPackages, filterDeniedPackages } from "../src/repo-tools.js";

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

  it("parses a JSONC manifest with a // header comment block (this repo's real shape)", async () => {
    await writeDevbox(`{
  // devbox.json — tool profile for this repo.
  // Edit "packages" to add tools; https://www.jetify.com/devbox
  "packages": ["ruby@3.3"]
}`);
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), ["ruby@3.3"]);
  });

  it("parses a manifest with both /* block */ and // line comments", async () => {
    await writeDevbox(`{
  /* block comment describing the profile */
  "packages": [
    "jq", // a line comment after a value
    "ruby@3.3"
  ]
}`);
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), ["jq", "ruby@3.3"]);
  });

  it("FALSE POSITIVE GUARD: // and /* */ inside a STRING VALUE are not treated as comments", async () => {
    // The env URL carries `//` and `/* */`; treating them as comments would corrupt
    // the string and break the JSON. A real line comment sits elsewhere.
    await writeDevbox(`{
  // real comment here
  "packages": ["hello"],
  "env": { "URL": "git+https://example.com/a/*b*/c" }
}`);
    // Extraction succeeds and "hello" survives — proof the string was not mis-cut
    // (a mis-cut would either drop the package or fail the parse entirely → []).
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), ["hello"]);
  });

  it("tolerates trailing commas in arrays and objects", async () => {
    await writeDevbox(`{ "packages": ["jq", "hello", ], }`);
    assert.deepStrictEqual(await extractRepoDevboxPackages(dir), ["jq", "hello"]);
  });

  it("returns [] for malformed JSONC (unterminated block comment)", async () => {
    await writeDevbox(`{ "packages": ["x"] /* oops`);
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

describe("filterDeniedPackages (tier-2 credential-CLI denylist, PRD #123 M1b)", () => {
  it("drops a denied package by BASE NAME even when version-pinned", () => {
    const { kept, dropped } = filterDeniedPackages(["glab@1.2", "jq", "ripgrep"], ["glab", "vault"]);
    assert.deepStrictEqual(dropped, ["glab@1.2"]);
    assert.deepStrictEqual(kept, ["jq", "ripgrep"]);
  });

  it("keeps every package when none is denied", () => {
    const { kept, dropped } = filterDeniedPackages(["jq", "ripgrep@14"], ["glab", "vault"]);
    assert.deepStrictEqual(kept, ["jq", "ripgrep@14"]);
    assert.deepStrictEqual(dropped, []);
  });

  it("preserves input order in both outputs", () => {
    const { kept, dropped } = filterDeniedPackages(
      ["a", "vault@1", "b", "glab", "c"],
      ["glab", "vault"],
    );
    assert.deepStrictEqual(kept, ["a", "b", "c"]);
    assert.deepStrictEqual(dropped, ["vault@1", "glab"]);
  });

  it("matches case-insensitively on the base name (keeps original casing)", () => {
    const { kept, dropped } = filterDeniedPackages(["Glab@1.2", "GH", "jq"], ["glab", "gh"]);
    assert.deepStrictEqual(dropped, ["Glab@1.2", "GH"]);
    assert.deepStrictEqual(kept, ["jq"]);
  });

  it("empty denied list keeps everything (older server ⇒ no filtering)", () => {
    const { kept, dropped } = filterDeniedPackages(["glab@1.2", "jq"], []);
    assert.deepStrictEqual(kept, ["glab@1.2", "jq"]);
    assert.deepStrictEqual(dropped, []);
  });
});
