// Parity gate for the in-app changelog (PRD #415 M4). `Release.body` is the
// PARITY SURFACE: for every release the parser emits, its `body` must equal
// `bash scripts/changelog-section.sh body <version>` byte-for-byte, so the two
// extractions (the TS parser in changelog.ts and the shell oracle the GitHub
// Release notes use) can never drift. We import the already-parsed `releases`
// (the parse of the bundled repo-root CHANGELOG.md) to test the EXACT bundled
// output, and shell out to the oracle per version.
//
// The oracle prints a trailing newline; `Release.body` has none — so we strip
// only the single trailing newline before comparing (never internal/leading
// content). Skips cleanly (does not fail) when bash or the script is unavailable,
// e.g. in an environment without a full checkout.
import { describe, it, expect } from "vitest";
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import { releases } from "./changelog";

// From web/src/lib/, the repo root is `../../..` — the same depth docs.ts uses
// for its `../../../docs` glob.
const testDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(testDir, "..", "..", "..");
const scriptRel = "scripts/changelog-section.sh";
const scriptAbs = path.join(repoRoot, scriptRel);

// bash runnable AND the oracle present? Otherwise skip cleanly rather than fail.
const bashOk = spawnSync("bash", ["-c", "true"], { encoding: "utf8" }).status === 0;
const available = bashOk && existsSync(scriptAbs);

describe("changelog parity: Release.body === changelog-section.sh body <version>", () => {
  if (!available) {
    it.skip(`skipped: ${!bashOk ? "bash not runnable" : `${scriptRel} not found`}`, () => {});
    return;
  }

  // Guard against a vacuous pass: the parser must have found releases to compare.
  it("parses at least one release from the bundled CHANGELOG", () => {
    expect(releases.length).toBeGreaterThan(0);
  });

  for (const release of releases) {
    it(`body parity for ${release.version}`, () => {
      const res = spawnSync("bash", [scriptRel, "body", release.version], {
        cwd: repoRoot,
        encoding: "utf8",
      });
      expect(res.error).toBeUndefined();
      expect(res.status, `changelog-section.sh exited ${res.status}: ${res.stderr}`).toBe(0);
      expect(res.stdout.replace(/\n$/, "")).toBe(release.body);
    });
  }
});
