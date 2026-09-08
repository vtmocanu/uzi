// Console-scope integrity guard (PRD #1167 m3fix). Two regressions bit the first
// m3 commit and neither tsc, vitest, nor the contrast oracle caught them:
//   A. A comment in index.css was closed early by a `*/` sitting inside the text
//      "--queue-*/…", so PostCSS failed to parse the WHOLE stylesheet and the app
//      rendered blank. The contrast oracle's regex comment-strip silently
//      tolerated it (it stops at the first `*/`), and CI's build never reaches the
//      CSS step because check-docs fails first — so this guard checks comment
//      balance on the RAW source, where a stray `*/` is unambiguous.
//   B. The `.console` scope remapped only the base palette, not the
//      --queue-*/--neutral-*/--syn-*/--tool-rail aliases. CSS custom properties
//      inherit their already-RESOLVED value, so a `--queue-fg: var(--muted)`
//      declared on the [data-theme] ancestor resolved to the theme's LIGHT muted
//      and inherited that light value into a nested `.console` unchanged — code
//      panes on a light theme rendered light syntax/pills. The fix re-declares the
//      aliases ON `.console`; this guard resolves them against the console rule's
//      OWN base tokens and asserts they equal ember's, so a console pane provably
//      looks like ember token-for-token.
//
// Runs under the vitest `node` project (src/lib/**), so node:fs is available.
import { describe, it, expect } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const testDir = path.dirname(fileURLToPath(import.meta.url));
const css = readFileSync(path.resolve(testDir, "..", "index.css"), "utf8");

// ── Bug A guard: comments must balance ──────────────────────────────────────
it("index.css comments are balanced (no `*/` closes a comment early)", () => {
  let inComment = false;
  for (let i = 0; i < css.length - 1; i++) {
    const pair = css[i] + css[i + 1];
    if (!inComment && pair === "/*") {
      inComment = true;
      i++;
      continue;
    }
    if (inComment && pair === "*/") {
      inComment = false;
      i++;
      continue;
    }
    // A `*/` while NOT inside a comment means an earlier comment closed early
    // (e.g. a `*/` glyph buried in comment text such as "--queue-*/--neutral-*").
    if (!inComment && pair === "*/") {
      throw new Error(
        `index.css: stray "*/" at offset ${i} — a comment closed early (a "*/" inside comment text breaks PostCSS parsing)`,
      );
    }
  }
  expect(inComment, "index.css has an unterminated /* comment").toBe(false);
});

// ── shared minimal parser (mirrors contrast.test.ts) ────────────────────────
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

type RGB = [number, number, number];

// Resolve a token to a triple within an arbitrary token map, following var()
// aliases against THAT SAME map (so a console alias resolves against the console
// rule's own dark base tokens, exactly as the browser does per element).
function resolveIn(tokens: Record<string, string>, token: string, seen: string[] = []): RGB {
  const raw = tokens[token];
  if (raw === undefined) {
    throw new Error(`token ${token} is undefined (chain: ${[...seen, token].join(" -> ")})`);
  }
  const v = raw.match(/^var\(\s*(--[\w-]+)\s*\)$/);
  if (v) {
    if (seen.includes(token)) throw new Error(`var() cycle at ${token}`);
    return resolveIn(tokens, v[1], [...seen, token]);
  }
  const t = raw.match(/^(\d+)\s+(\d+)\s+(\d+)$/);
  if (!t) throw new Error(`token ${token} = "${raw}" is neither a triple nor a var() alias`);
  return [Number(t[1]), Number(t[2]), Number(t[3])];
}

// ── Bug B guard: the console scope resolves to ember token-for-token ─────────
describe("console scope == ember", () => {
  const rules = topLevelRules(stripComments(css));
  const emberRule = rules.find((r) => /\[data-theme="ember"\]/.test(r.selector));
  const consoleRule = rules.find((r) => /\.console\b/.test(r.selector));

  it("the ember block and the .console scope rule both exist", () => {
    expect(emberRule, "ember [data-theme] block not found").toBeTruthy();
    expect(consoleRule, ".console scope rule not found").toBeTruthy();
  });

  const ember = parseTokens(emberRule?.body ?? "");
  const scope = parseTokens(consoleRule?.body ?? "");

  // Base palette + status: the machine surface must be ember's dark set exactly.
  const BASE = [
    "--bg", "--surface", "--raised", "--edge", "--edge-strong",
    "--fg", "--muted", "--faint", "--brand", "--brand-hover", "--on-brand",
    "--ok", "--warn", "--danger", "--info", "--plan",
  ];
  // The aliases that bug B left resolving to the light theme's values.
  const ALIASES = [
    "--ring",
    "--queue-fg", "--queue-border", "--queue-surface",
    "--neutral-fg", "--neutral-border", "--neutral-surface",
    "--syn-cmd", "--syn-flag", "--syn-str", "--syn-op", "--syn-comment", "--syn-arg", "--tool-rail",
  ];

  it.each([...BASE, ...ALIASES])("%s resolves to the ember value inside .console", (token) => {
    // resolveIn(scope, …) walks aliases against the console rule's OWN tokens —
    // if bug B returns (an alias not re-declared on .console) this throws
    // "undefined"; if it resolves to a light value it mismatches ember.
    expect(resolveIn(scope, token), `console ${token}`).toEqual(resolveIn(ember, token));
  });

  // PRD #1167 M4: Hall's frame and Shadow's live cards RIDE this same rule, so they
  // inherit the ember base+alias remap proven token-for-token above — they are dark
  // exactly like a console pane BECAUSE they share the rule whose tokens equal ember's.
  // This asserts both selectors are on the rule; the token equality above then covers them.
  it("the dark-remap rule also scopes Hall's .frame and Shadow's [data-live]", () => {
    expect(consoleRule?.selector, "Hall frame not on the ember remap rule").toContain(
      '[data-theme="hall"] .frame',
    );
    expect(consoleRule?.selector, "Shadow live cards not on the ember remap rule").toContain(
      '[data-theme="shadow"] [data-live]',
    );
  });
});

// ── PRD #1167 M4: Shadow's attention surfaces get a LIGHT treatment ──────────
// awaiting_approval (and in-review) surfaces carry `data-attention`; under Shadow
// they must NOT go dark like [data-live] — they get a rust left rail + a rust
// tint over white so the human's decision surface stays light (PRD #1167).
describe("Shadow attention rule", () => {
  const rules = topLevelRules(stripComments(css));
  const attentionRule = rules.find((r) => /\[data-theme="shadow"\]\s+\[data-attention\]/.test(r.selector));

  it("a [data-theme=\"shadow\"] [data-attention] rule exists", () => {
    expect(attentionRule, "Shadow [data-attention] rule not found").toBeTruthy();
  });

  it("sets a box-shadow inset rail in the brand and a background tint", () => {
    const body = attentionRule?.body ?? "";
    // The rail is an inset box-shadow in the theme brand (rust), no layout reflow.
    expect(body, "attention rail box-shadow missing").toMatch(/box-shadow:\s*inset[^;]*var\(--brand\)/);
    // The tint overrides the card's bg utility with an opaque LIGHT rust-over-white
    // color. Pin the exact value so a regression to a DARK attention surface (which
    // would make the human's decision surface read like a [data-live] card) is caught.
    expect(body, "attention background tint missing").toMatch(/background-color:\s*rgb\(/);
    expect(body, "attention tint is not the light rust rgb(251 244 240)").toContain("251 244 240");
  });
});

// ── PRD #1167 M4: Shadow's live surfaces get a solid dark --surface background ──
// The mock (lines 89-90) applies TWO rules to every [data-live]: the shared dark
// var-remap (proven above) AND a second rule giving it `background: var(--surface)`
// so a live board card, run row and run header read as solid dark cards, not just
// recoloured text on the light page. This guards against regressing to the
// unreadable-header state (ember-light text on the light ground) if that second rule
// is dropped. NOTE two selectors mention [data-live] now — the shared var-remap rule
// and this bg rule — so match the one whose body sets the dark background.
describe("Shadow live-surface background", () => {
  const rules = topLevelRules(stripComments(css));
  const liveBgRule = rules.find(
    (r) =>
      r.selector.trim() === '[data-theme="shadow"] [data-live]' &&
      /background-color/.test(r.body),
  );

  it("a [data-theme=\"shadow\"] [data-live] rule sets the dark --surface background", () => {
    expect(liveBgRule, "Shadow [data-live] background rule not found").toBeTruthy();
    expect(liveBgRule?.body, "live background not var(--surface)").toMatch(
      /background-color:\s*rgb\(var\(--surface\)\)/,
    );
  });
});
