import { describe, it, expect } from "vitest";

// PRD #98 review B1. Every source file under web/src must be free of NUL (U+0000) bytes.
//
// This is a REVIEWABILITY gate, not a correctness one, and it earns its place because the
// failure it prevents is invisible by construction:
//
//   * git classifies a file containing a NUL as BINARY. The Judge page (821 lines, 32 KB)
//     landed as `Bin 0 -> 32202 bytes | 1 file changed, 0 insertions(+), 0 deletions(-)`,
//     so its first review had no diff to read, and every later edit to it would have shown
//     "Binary files differ" in `git log -p` and in the GitLab MR.
//   * plain `grep`/`rg` skip binary files and report NOTHING rather than erroring. The
//     auditor lost time to exactly this: a `dangerouslySetInnerHTML` it had just inserted
//     into that file was invisible until `grep -a`. Same false-green shape as the `rg -r`
//     incident, on a different tool — the search succeeds and lies.
//
// Neither symptom shows up in a typecheck, a unit test, or a build, which is why a comment
// saying "do not use NUL" would not have caught it. Only reading the bytes does. Two
// literal NULs (the separators in two key builders) were enough to do all of the above, on
// the one file that renders the judge's untrusted free text.
//
// It uses import.meta.glob rather than node:fs, following workerSizes.test.ts and for the
// same load-bearing reason recorded there: web/Dockerfile copies web/ and docs/ and
// nothing else, and this project has no @types/node, so a node:fs import fails the
// typecheck gate even though vitest runs it happily. (Measured 2026-07-21: the first cut
// of this file used node:fs, passed `npm test`, and failed `npm run typecheck` with
// TS2307 — a green test suite hiding a red gate.)
const SOURCES = import.meta.glob("../**/*.{ts,tsx,js,jsx,css,json,html}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

describe("source bytes (PRD #98 review B1)", () => {
  it("finds source files to check at all", () => {
    // Without this the suite could pass by scanning nothing — a green that proves the
    // glob pattern compiled, not that the tree is clean.
    expect(Object.keys(SOURCES).length).toBeGreaterThan(100);
  });

  it("contains no NUL byte in any source file", () => {
    const offenders = Object.entries(SOURCES)
      .filter(([, text]) => text.includes("\u0000"))
      .map(([path]) => path);
    expect(offenders).toEqual([]);
  });
});
