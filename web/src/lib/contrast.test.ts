// Contrast oracle (PRD #1167 M3, extended M4). This test READS web/src/index.css
// from disk, auto-discovers every theme name in a `[data-theme="X"]` selector
// (the ember base block's `:root:not([data-theme]), [data-theme="ember"]` counts,
// as does the M4 shared light block `[data-theme="dawn"], [data-theme="hall"],
// [data-theme="shadow"]` — its ONE comma selector registers all three names,
// which is why themeNamesIn returns every match rather than the first), resolves
// each theme's `var()` aliases within the block, and asserts WCAG 2.2 AA (4.5:1)
// for every text token on every surface it can land on. It is the guardrail that
// keeps every theme AA-clean: ember and mission were designed to pass and MUST
// pass here — and hall/shadow inherit Dawn's exact token set, so they pass
// identically. If a theme fails, the parser or the luminance math is wrong, not
// the CSS.
//
// Runs under the vitest `node` project (src/lib/**), so node:fs is available; the
// minimal signatures live in web/src/node-fs.d.ts (web/ ships no @types/node).
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const AA = 4.5;

// From web/src/lib/, index.css sits at web/src/index.css → one level up.
const testDir = path.dirname(fileURLToPath(import.meta.url));
const cssPath = path.resolve(testDir, "..", "index.css");
const css = readFileSync(cssPath, "utf8");

// ── Parser ────────────────────────────────────────────────────────────────
// Strip comments FIRST: several comments in index.css contain brace and
// [data-theme="…"] literals (e.g. the console-scope rationale) that would
// otherwise corrupt the brace walk and the theme discovery.
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "");
}

interface Rule {
  selector: string;
  body: string;
}

// Walk top-level rules only, tracking brace depth so nested @layer/@media/
// @keyframes blocks are captured whole (as one top-level rule) rather than
// mistaken for theme blocks. A theme block is flat, so its body is the depth-1
// declaration list.
function topLevelRules(src: string): Rule[] {
  const rules: Rule[] = [];
  let depth = 0;
  let preludeStart = 0;
  let bodyStart = 0;
  for (let i = 0; i < src.length; i++) {
    const ch = src[i];
    if (ch === "{") {
      if (depth === 0) {
        bodyStart = i + 1;
      }
      depth++;
    } else if (ch === "}") {
      depth--;
      if (depth === 0) {
        rules.push({
          selector: src.slice(preludeStart, bodyStart - 1).trim(),
          body: src.slice(bodyStart, i),
        });
        preludeStart = i + 1;
      }
    }
  }
  return rules;
}

// A theme block is any top-level rule with one or more comma-separated selector
// parts that are EXACTLY `[data-theme="name"]` (anchored, so descendant selectors
// like the console scope's `[data-theme="dawn"] .console` and Shadow's
// `[data-theme="shadow"] [data-live]` never match). Returns EVERY matched name —
// so the shared `[data-theme="dawn"], [data-theme="hall"], [data-theme="shadow"]`
// light-theme block (PRD #1167 M4) registers all three off its one comma selector,
// and the per-theme AA loop then covers hall/shadow with no new test data (they
// share Dawn's exact token set, so they pass identically).
function themeNamesIn(selector: string): string[] {
  const names: string[] = [];
  for (const part of selector.split(",")) {
    const m = part.trim().match(/^\[data-theme="([a-z]+)"\]$/);
    if (m) names.push(m[1]);
  }
  return names;
}

// Parse a block body into { token: rawValue }. Splitting on ";" is safe: no
// declaration value in index.css contains a semicolon (grid-image's commas and
// linear-gradient parens have none).
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

const themes: Record<string, Record<string, string>> = {};
for (const rule of topLevelRules(stripComments(css))) {
  for (const name of themeNamesIn(rule.selector)) {
    themes[name] = parseTokens(rule.body);
  }
}

const themeNames = Object.keys(themes).sort();

type RGB = [number, number, number];

// Resolve a token to an [r,g,b] triple, following var(--other) chains WITHIN the
// same theme. Fails loudly (throws) on an unresolvable/typo'd alias or a value
// that is neither a triple nor a single var() reference.
function resolveTriple(theme: string, token: string, seen: string[] = []): RGB {
  const tokens = themes[theme];
  const raw = tokens[token];
  if (raw === undefined) {
    throw new Error(`theme "${theme}": token ${token} is undefined (chain: ${[...seen, token].join(" -> ")})`);
  }
  const varMatch = raw.match(/^var\(\s*(--[\w-]+)\s*\)$/);
  if (varMatch) {
    if (seen.includes(token)) {
      throw new Error(`theme "${theme}": var() cycle at ${token} (chain: ${[...seen, token].join(" -> ")})`);
    }
    return resolveTriple(theme, varMatch[1], [...seen, token]);
  }
  const triple = raw.match(/^(\d+)\s+(\d+)\s+(\d+)$/);
  if (!triple) {
    throw new Error(`theme "${theme}": token ${token} = "${raw}" is neither a channel triple nor a var() alias`);
  }
  return [Number(triple[1]), Number(triple[2]), Number(triple[3])];
}

// ── WCAG 2.2 relative luminance + contrast ratio ────────────────────────────
function channelLinear(c: number): number {
  const cs = c / 255;
  return cs <= 0.03928 ? cs / 12.92 : Math.pow((cs + 0.055) / 1.055, 2.4);
}

function luminance([r, g, b]: RGB): number {
  return 0.2126 * channelLinear(r) + 0.7152 * channelLinear(g) + 0.0722 * channelLinear(b);
}

function contrast(a: RGB, b: RGB): number {
  const la = luminance(a);
  const lb = luminance(b);
  const lighter = Math.max(la, lb);
  const darker = Math.min(la, lb);
  return (lighter + 0.05) / (darker + 0.05);
}

// The pill pattern is `bg-tone/10 text-tone`: the effective background is the
// tone blended at alpha 0.10 over the (opaque) surface.
function tintOver(tone: RGB, surface: RGB, alpha = 0.1): RGB {
  return [
    tone[0] * alpha + surface[0] * (1 - alpha),
    tone[1] * alpha + surface[1] * (1 - alpha),
    tone[2] * alpha + surface[2] * (1 - alpha),
  ];
}

// ── The checks ──────────────────────────────────────────────────────────────
// Text tokens that must clear AA on the three opaque surfaces.
const TEXT_TOKENS = ["--fg", "--muted", "--faint", "--brand", "--ok", "--warn", "--danger", "--info", "--plan"];
const SURFACES = ["--bg", "--surface", "--raised"];
// Status tones additionally checked against their own 10% pill tint over surface.
const STATUS_TONES = ["--ok", "--warn", "--danger", "--info", "--plan"];

// Every token whose value is a plain var() alias — resolving them all guards
// against a typo (--ring, --queue-*, --neutral-*, --syn-*, --tool-rail).
function aliasTokens(theme: string): string[] {
  return Object.entries(themes[theme])
    .filter(([, raw]) => /^var\(\s*--[\w-]+\s*\)$/.test(raw))
    .map(([name]) => name);
}

it("discovers ember, mission, dawn, hall and shadow", () => {
  expect(themeNames).toEqual(
    expect.arrayContaining(["dawn", "ember", "hall", "mission", "shadow"]),
  );
});

describe.each(themeNames)("theme %s", (theme) => {
  it("resolves every var() alias to a valid triple", () => {
    for (const token of aliasTokens(theme)) {
      // resolveTriple throws with a naming message on any unresolvable alias.
      expect(() => resolveTriple(theme, token), `theme "${theme}" alias ${token}`).not.toThrow();
    }
  });

  describe.each(TEXT_TOKENS)("%s as text", (token) => {
    it.each(SURFACES)(`clears AA on %s`, (surface) => {
      const ratio = contrast(resolveTriple(theme, token), resolveTriple(theme, surface));
      expect(
        ratio,
        `${theme}: ${token} on ${surface} measured ${ratio.toFixed(2)}:1 (need ${AA}:1)`,
      ).toBeGreaterThanOrEqual(AA);
    });
  });

  it.each(STATUS_TONES)(`%s clears AA on its own 10%% tint`, (tone) => {
    const toneRGB = resolveTriple(theme, tone);
    const bg = tintOver(toneRGB, resolveTriple(theme, "--surface"));
    const ratio = contrast(toneRGB, bg);
    expect(
      ratio,
      `${theme}: ${tone} on its own 10% tint over --surface measured ${ratio.toFixed(2)}:1 (need ${AA}:1)`,
    ).toBeGreaterThanOrEqual(AA);
  });
});

// Compact per-theme summary (the weakest measured pairing per theme), printed so
// a run of this file shows PASS + the tightest margin for each theme at a glance.
it("prints a per-theme AA summary", () => {
  const lines: string[] = [];
  for (const theme of themeNames) {
    let min = Infinity;
    let where = "";
    const record = (ratio: number, label: string) => {
      if (ratio < min) {
        min = ratio;
        where = label;
      }
    };
    for (const token of TEXT_TOKENS) {
      for (const surface of SURFACES) {
        record(contrast(resolveTriple(theme, token), resolveTriple(theme, surface)), `${token} on ${surface}`);
      }
    }
    for (const tone of STATUS_TONES) {
      const toneRGB = resolveTriple(theme, tone);
      record(contrast(toneRGB, tintOver(toneRGB, resolveTriple(theme, "--surface"))), `${tone} on 10% tint`);
    }
    const verdict = min >= AA ? "PASS" : "FAIL";
    lines.push(`${verdict} theme=${theme} min=${min.toFixed(2)}:1 (${where})`);
    expect(min, `${theme} weakest pairing ${where} = ${min.toFixed(2)}:1`).toBeGreaterThanOrEqual(AA);
  }
  console.log("\ncontrast oracle (WCAG 2.2 AA, need 4.50:1)\n" + lines.join("\n"));
});
