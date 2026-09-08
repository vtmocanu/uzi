// Typeface-layer integrity guard (PRD #1167 M6). The typeface (--font-sans /
// --font-mono) is orthogonal to the theme: data-font selects the family, data-theme
// selects the palette. M6 moved --font-* OUT of the three theme blocks (ember,
// mission, and the shared dawn/hall/shadow light block) and into a dedicated layer
// (`:root` for System, `:root[data-font="plex"]` for the bundled IBM Plex). This
// guard pins that separation so a future theme edit cannot silently re-couple them:
//   (a) a `:root` rule and a `:root[data-font="plex"]` rule both exist and each
//       defines BOTH --font-sans and --font-mono;
//   (b) NO `[data-theme="…"]` block declares --font-sans/--font-mono anymore (they
//       moved to the layer, so theme and typeface can never drift back together);
//   (c) the typeface layer appears AFTER the last `[data-theme]` block in source
//       order, so no theme block (or theme-scoped rule) can shadow it.
//
// Runs under the vitest `node` project (src/lib/**), so node:fs is available. The
// minimal parser mirrors console-scope.test.ts / contrast.test.ts.
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const testDir = path.dirname(fileURLToPath(import.meta.url));
const css = readFileSync(path.resolve(testDir, "..", "index.css"), "utf8");

function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "");
}

interface Rule {
  selector: string;
  body: string;
}

function topLevelRules(src: string): Rule[] {
  const rules: Rule[] = [];
  let depth = 0;
  let preludeStart = 0;
  let bodyStart = 0;
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    if (ch === "{") {
      if (depth === 0) bodyStart = i + 1;
      depth++;
    } else if (ch === "}") {
      depth--;
      if (depth === 0) {
        rules.push({ selector: src.slice(preludeStart, bodyStart - 1).trim(), body: src.slice(bodyStart, i) });
        preludeStart = i + 1;
      }
    }
  }
  return rules;
}

function parseTokens(body: string): Record<string, string> {
  const tokens: Record<string, string> = {};
  for (const decl of body.split(";")) {
    const idx = decl.indexOf(":");
    if (idx === -1) continue;
    const name = decl.slice(0, idx).trim();
    if (!name.startsWith("--")) continue;
    tokens[name] = decl.slice(idx + 1).trim();
  }
  return tokens;
}

const rules = topLevelRules(stripComments(css));

describe("typeface layer (PRD #1167 M6)", () => {
  const baseRule = rules.find((r) => r.selector.trim() === ":root");
  const plexRule = rules.find((r) => r.selector.trim() === ':root[data-font="plex"]');

  it("declares a base :root rule and a :root[data-font=\"plex\"] rule", () => {
    expect(baseRule, "base :root typeface rule not found").toBeTruthy();
    expect(plexRule, ':root[data-font="plex"] typeface rule not found').toBeTruthy();
  });

  it("both typeface rules define --font-sans AND --font-mono", () => {
    for (const [label, rule] of [
      ["base :root", baseRule],
      [':root[data-font="plex"]', plexRule],
    ] as const) {
      const tokens = parseTokens(rule?.body ?? "");
      expect(tokens["--font-sans"], `${label} is missing --font-sans`).toBeTruthy();
      expect(tokens["--font-mono"], `${label} is missing --font-mono`).toBeTruthy();
    }
    // The Plex rule must actually name the IBM Plex families (not a copy of System).
    expect(parseTokens(plexRule?.body ?? "")["--font-sans"]).toContain("IBM Plex Sans");
    expect(parseTokens(plexRule?.body ?? "")["--font-mono"]).toContain("IBM Plex Mono");
  });

  it("no [data-theme] block declares --font-sans/--font-mono anymore", () => {
    for (const rule of rules) {
      if (!/\[data-theme=/.test(rule.selector)) continue;
      const tokens = parseTokens(rule.body);
      expect(
        tokens["--font-sans"],
        `--font-sans leaked back into a theme block: ${rule.selector.slice(0, 60)}`,
      ).toBeUndefined();
      expect(
        tokens["--font-mono"],
        `--font-mono leaked back into a theme block: ${rule.selector.slice(0, 60)}`,
      ).toBeUndefined();
    }
  });

  it("the typeface layer comes AFTER the last [data-theme] block in source order", () => {
    let lastThemeIdx = -1;
    rules.forEach((r, i) => {
      if (/\[data-theme=/.test(r.selector)) lastThemeIdx = i;
    });
    const baseIdx = rules.findIndex((r) => r.selector.trim() === ":root");
    const plexIdx = rules.findIndex((r) => r.selector.trim() === ':root[data-font="plex"]');
    expect(lastThemeIdx, "no [data-theme] block found").toBeGreaterThanOrEqual(0);
    expect(baseIdx, "base :root typeface rule precedes a theme block").toBeGreaterThan(lastThemeIdx);
    expect(plexIdx, ':root[data-font="plex"] rule precedes a theme block').toBeGreaterThan(lastThemeIdx);
  });
});
