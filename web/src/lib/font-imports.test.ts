// Bundled-font wiring guard (PRD #1167 M6 follow-up). This stands in for the
// PRD's "vite build check that the woff2 land in dist same-origin": a real
// dist-grep would need a slow `vite build` and dist is gitignored, and the
// @fontsource CSS references its woff2 with a RELATIVE url(), so Vite always
// bundles them same-origin — the risk actually worth guarding is a dropped (or
// stray) @fontsource import, which this test catches by reading main.tsx from
// disk and pinning the EXACT set of bundled family/subset/weight entries.
//
// Keep EXPECTED in sync with the imports in src/main.tsx:
//   IBM Plex Sans — latin + latin-ext at 400/500/600/700 (700 covers the widely
//     used `font-bold`, so bold is a real face, not a synthetic 600).
//   IBM Plex Mono — latin + latin-ext at 400/500/600 (600 covers `font-semibold`
//     on the mono usage/plan/CLI surfaces, a real face not a synthetic 500). No
//     monospace surface uses `font-bold` (700), so mono 700 is deliberately NOT
//     bundled.
//
// Runs under the vitest `node` project (src/lib/**), so node:fs is available.
// Mirrors the file-reading style of typeface-layer.test.ts.
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const testDir = path.dirname(fileURLToPath(import.meta.url));
const mainSrc = readFileSync(path.resolve(testDir, "..", "main.tsx"), "utf8");

// Every @fontsource specifier main.tsx side-effect-imports, e.g.
// "@fontsource/ibm-plex-sans/latin-700.css".
function fontsourceImports(src: string): string[] {
  const found: string[] = [];
  const re = /import\s+["'](@fontsource\/[^"']+)["'];?/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(src)) !== null) found.push(m[1]);
  return found;
}

const EXPECTED = [
  "@fontsource/ibm-plex-sans/latin-400.css",
  "@fontsource/ibm-plex-sans/latin-500.css",
  "@fontsource/ibm-plex-sans/latin-600.css",
  "@fontsource/ibm-plex-sans/latin-700.css",
  "@fontsource/ibm-plex-sans/latin-ext-400.css",
  "@fontsource/ibm-plex-sans/latin-ext-500.css",
  "@fontsource/ibm-plex-sans/latin-ext-600.css",
  "@fontsource/ibm-plex-sans/latin-ext-700.css",
  "@fontsource/ibm-plex-mono/latin-400.css",
  "@fontsource/ibm-plex-mono/latin-500.css",
  "@fontsource/ibm-plex-mono/latin-600.css",
  "@fontsource/ibm-plex-mono/latin-ext-400.css",
  "@fontsource/ibm-plex-mono/latin-ext-500.css",
  "@fontsource/ibm-plex-mono/latin-ext-600.css",
];

describe("bundled font imports (PRD #1167 M6)", () => {
  const actual = fontsourceImports(mainSrc);

  it("imports every expected @fontsource weight/subset", () => {
    for (const spec of EXPECTED) {
      expect(actual, `main.tsx dropped a bundled font import: ${spec}`).toContain(spec);
    }
  });

  it("imports NOTHING beyond the expected set (no stray weight/subset)", () => {
    for (const spec of actual) {
      expect(EXPECTED, `main.tsx imports an unexpected @fontsource entry: ${spec}`).toContain(spec);
    }
    // Cross-check the totals so a duplicated import cannot mask a dropped one.
    expect(new Set(actual).size).toBe(EXPECTED.length);
  });

  it("bundles Sans 700 (real bold, not synthesised from 600)", () => {
    expect(actual).toContain("@fontsource/ibm-plex-sans/latin-700.css");
    expect(actual).toContain("@fontsource/ibm-plex-sans/latin-ext-700.css");
  });

  it("does NOT bundle Mono 700 (no monospace surface uses font-bold)", () => {
    expect(actual).not.toContain("@fontsource/ibm-plex-mono/latin-700.css");
    expect(actual).not.toContain("@fontsource/ibm-plex-mono/latin-ext-700.css");
  });
});
