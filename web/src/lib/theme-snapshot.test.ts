// Ember/Mission colour-snapshot guard (PRD #1167 m8, SC6). This is the mechanical
// no-regression guard for the two DARK themes: PRD #1167 added the light themes
// (dawn/hall/shadow), the appearance UI, the typeface layer and the docs — it must
// NOT have changed how ember or mission render. So this test READS web/src/index.css
// from disk, extracts the ember block (the combined
// `:root:not([data-theme]), [data-theme="ember"]` selector) and the mission block
// (`[data-theme="mission"]`), and pins every colour/design token each declares to a
// FROZEN expected value == the pre-#1167 value (read from the current, unchanged
// blocks and hard-coded below).
//
// The ONE permitted diff is that `--font-sans`/`--font-mono` moved OUT of the theme
// blocks into the shared typeface layer in m6 (data-font, orthogonal to theme), so
// the frozen set deliberately excludes them and a separate check pins that they are
// absent from both blocks (re-adding one to a theme fails the test). The no-drift
// check compares the full declared token set (minus the two font tokens) against the
// frozen key set, so a future ADDED or REMOVED token in either block also fails —
// this catches drift in BOTH directions, not just a changed value.
//
// If this test FAILS, either the frozen map below has a typo or ember/mission
// genuinely drifted — investigate; do not loosen the test to make it pass.
//
// Runs under the vitest `node` project (src/lib/**), so node:fs is available; the
// minimal signatures live in web/src/node-fs.d.ts (web/ ships no @types/node).
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

// From web/src/lib/, index.css sits at web/src/index.css → one level up.
const testDir = path.dirname(fileURLToPath(import.meta.url));
const css = readFileSync(path.resolve(testDir, "..", "index.css"), "utf8");

// ── shared minimal parser (mirrors contrast.test.ts / console-scope.test.ts) ──
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, "");
}

interface Rule {
  selector: string;
  body: string;
}

// Walk top-level rules only, tracking brace depth so nested @layer/@media blocks are
// captured whole rather than mistaken for theme blocks. A theme block is flat, so its
// body is the depth-1 declaration list.
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

// Parse a block body into { token: rawValue }. Splitting on ";" is safe: no
// declaration value in index.css contains a semicolon (grid-image's commas and
// linear-gradient parens have none). The rawValue is compared byte-for-byte, so the
// mission --grid-image keeps its exact multi-line whitespace.
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

// The two typeface tokens legitimately moved to the shared typeface layer in m6 — the
// only permitted change to the ember/mission blocks. Excluded from the frozen set and
// separately asserted absent below.
const FONT_TOKENS = ["--font-sans", "--font-mono"];

// ── Frozen expected token maps (== the pre-#1167 values) ─────────────────────
// Read verbatim from the current (unchanged) ember/mission blocks in index.css and
// hard-coded here as the literals. Values are the RAW declared strings, not resolved
// (so a var() alias stays "var(--x)"). Covers every token each block declares EXCEPT
// the two font tokens above.
const EXPECTED: Record<string, Record<string, string>> = {
  ember: {
    // Palette
    "--bg": "8 10 15",
    "--surface": "15 18 26",
    "--raised": "23 27 38",
    "--edge": "39 45 60",
    "--edge-strong": "58 66 86",
    "--fg": "228 232 240",
    "--muted": "148 158 176",
    "--faint": "122 133 154",
    "--brand": "251 146 60",
    "--brand-hover": "253 186 116",
    "--on-brand": "26 16 8",
    // Focus ring
    "--ring": "var(--brand)",
    // Status
    "--ok": "52 211 153",
    "--warn": "251 191 36",
    "--danger": "251 113 133",
    "--info": "56 189 248",
    "--plan": "129 140 248",
    // Queue + neutral pills
    "--queue-fg": "var(--muted)",
    "--queue-border": "var(--edge)",
    "--queue-surface": "var(--raised)",
    "--neutral-fg": "var(--muted)",
    "--neutral-border": "var(--edge)",
    "--neutral-surface": "var(--raised)",
    // Shell syntax highlighting
    "--syn-cmd": "var(--brand)",
    "--syn-flag": "var(--info)",
    "--syn-str": "var(--ok)",
    "--syn-op": "var(--brand-hover)",
    "--syn-comment": "var(--faint)",
    "--syn-arg": "var(--fg)",
    "--tool-rail": "var(--edge)",
    // Radius
    "--radius": "0.625rem",
    // Backdrop grid: ember has none
    "--grid-image": "none",
    "--grid-size": "auto",
  },
  mission: {
    // Palette
    "--bg": "5 8 15",
    "--surface": "11 17 32",
    "--raised": "16 26 48",
    "--edge": "28 42 69",
    "--edge-strong": "71 85 105",
    "--fg": "241 245 249",
    "--muted": "148 163 184",
    "--faint": "116 132 155",
    "--brand": "34 211 238",
    "--brand-hover": "103 232 249",
    "--on-brand": "2 6 23",
    // Focus ring
    "--ring": "var(--brand)",
    // Status
    "--ok": "52 211 153",
    "--warn": "251 191 36",
    "--danger": "244 63 94",
    "--info": "34 211 238",
    "--plan": "129 140 248",
    // Violet queue + lighter slate neutral pills
    "--queue-fg": "196 181 253",
    "--queue-border": "91 33 182",
    "--queue-surface": "46 16 101",
    "--neutral-fg": "203 213 225",
    "--neutral-border": "51 65 85",
    "--neutral-surface": "30 41 59",
    // Shell syntax highlighting (flag/op re-pointed off the collapsed cyan)
    "--syn-cmd": "var(--brand)",
    "--syn-flag": "var(--warn)",
    "--syn-str": "var(--ok)",
    "--syn-op": "var(--muted)",
    "--syn-comment": "var(--faint)",
    "--syn-arg": "var(--fg)",
    "--tool-rail": "var(--edge)",
    // Sharper radius
    "--radius": "0.25rem",
    // Faint blueprint grid (raw multi-line value, whitespace preserved)
    "--grid-line": "rgb(var(--brand) / 0.025)",
    "--grid-image":
      "linear-gradient(var(--grid-line) 1px, transparent 1px),\n    linear-gradient(90deg, var(--grid-line) 1px, transparent 1px)",
    "--grid-size": "36px 36px",
  },
};

// ── Block discovery ──────────────────────────────────────────────────────────
const rules = topLevelRules(stripComments(css));

// Find the top-level rule whose comma-separated selector has a part EXACTLY equal to
// `[data-theme="name"]` (anchored, so descendant scopes like the console remap's
// `[data-theme="shadow"] [data-live]` never match). The ember rule's prelude also
// carries the leading @import/@config at-rules and `:root:not([data-theme])`, but its
// `[data-theme="ember"]` comma-part still matches exactly.
function findThemeBlock(name: string): Rule | undefined {
  return rules.find((r) => r.selector.split(",").some((part) => part.trim() === `[data-theme="${name}"]`));
}

describe.each(["ember", "mission"])("theme %s colour snapshot (== pre-#1167)", (theme) => {
  const rule = findThemeBlock(theme);
  const expected = EXPECTED[theme];

  it(`the ${theme} [data-theme] block exists in index.css`, () => {
    expect(rule, `${theme} [data-theme] block not found in index.css`).toBeTruthy();
  });

  const tokens = parseTokens(rule?.body ?? "");

  // Req 2 + 5: every frozen token equals its pre-#1167 value, compared as the RAW
  // declared string, with a message naming theme + token + expected vs actual.
  it.each(Object.keys(expected))("%s equals its frozen pre-#1167 value", (token) => {
    expect(
      tokens[token],
      `theme "${theme}" token ${token}: expected ${JSON.stringify(expected[token])} but index.css declares ${JSON.stringify(
        tokens[token],
      )} — the dark theme drifted (only --font-* may move, in m6)`,
    ).toBe(expected[token]);
  });

  // Req 3: no drift in the token SET — the declared --* tokens (minus the two font
  // tokens) must exactly equal the frozen key set, so an ADDED or REMOVED token in
  // either block fails the test (catches both directions).
  it("declares exactly the frozen token set (no added or removed token)", () => {
    const declared = Object.keys(tokens).filter((t) => !FONT_TOKENS.includes(t));
    const expectedKeys = Object.keys(expected);
    const added = declared.filter((t) => !expectedKeys.includes(t)).sort();
    const missing = expectedKeys.filter((t) => !declared.includes(t)).sort();
    expect(
      { added, missing },
      `theme "${theme}" token set drifted from the frozen pre-#1167 set — added: [${added.join(
        ", ",
      )}], removed: [${missing.join(", ")}]`,
    ).toEqual({ added: [], missing: [] });
  });

  // Req 4: the two font tokens must NOT be declared in the theme block — they moved to
  // the typeface layer in m6. Re-adding one to a theme block fails here (the drift
  // check above filters them out, so this pins the one permitted change explicitly).
  it.each(FONT_TOKENS)("does not declare %s (moved to the typeface layer in m6)", (token) => {
    expect(
      tokens[token],
      `theme "${theme}" must NOT declare ${token} in its [data-theme] block — it moved to the shared typeface layer in m6`,
    ).toBeUndefined();
  });
});
