#!/usr/bin/env node
// Build-time guard against SILENTLY-DEAD Tailwind classes (issue #170). Runs
// standalone (`npm run check-styles`) and as a step of `npm run build`, so a
// mistyped color stem fails the web image build the same way check-docs does.
//
// THE MECHANISM IT CATCHES. A Tailwind utility whose stem is not in the config
// fails COMPLETELY silently: no error, no build warning, no console noise — the
// class simply generates no CSS and the element inherits. `text-warning` shipped
// where the semantic token is `warn` (tailwind.config.js) and rendered grey
// instead of amber; `bg-bg` (the page-background token is `ink: token("bg")`,
// so the class is `bg-ink`, never `bg-bg`) rendered transparent; `border-line`
// (the token is `edge`) rendered borderless. None of these are typos a compiler
// or the bundler can see — the string is a valid string, just not a valid class.
//
// HOW IT DECIDES MEMBERSHIP: the project's OWN engine, not a hand-kept allowlist.
// We run the real postcss + tailwindcss over `@tailwind utilities;` with the
// collected tokens as raw content and ask whether each token's escaped selector
// appears in the generated CSS. So the check tracks tailwind.config.js exactly:
// add a color token and its classes become "known" with no edit here.
//
// WHY AST, NOT REGEX (load-bearing). Candidate classes are extracted from the
// TypeScript AST — only from JSX `className` attribute values and `cx(...)` call
// arguments (cx is the repo's sole class helper, src/components/ui.tsx). A naive
// regex over source text false-positives on prose: Board.tsx carries a comment
// that literally contains `bg-bg` while EXPLAINING this very bug, and a regex
// would flag the comment. Extracting only from real className/cx subtrees means
// comments and prose can never be mistaken for classes.
//
// SCOPE (deliberately narrow for a false-positive-free first landing). Only
// tokens whose bare form starts with a COLOR-CONSUMING prefix (text-/bg-/border-/
// …, see COLOR_PREFIXES) are flagged — that is where silent color failures live
// and matches issue #170's stated scope. Because membership already uses the real
// engine, widening to ALL utilities is just widening that one constant later.
import { readFileSync, readdirSync, writeFileSync, mkdtempSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
import os from "node:os";
import path from "node:path";
// PARSER: @babel/parser, not the `typescript` package. TypeScript 7 (the native
// port) removed the synchronous programmatic parser from its public JS API:
// `import ts from "typescript"` now resolves to `lib/version.cjs` (just `{ version }`),
// so `ts.createSourceFile` / `ts.ScriptTarget` / `ts.forEachChild` are all undefined,
// and the only text->AST path left is the Go-backed, async, explicitly-unstable
// `typescript/unstable/sync` Project model. Re-implementing a JSX-aware parser on
// TS7's raw scanner would be far riskier for this false-positive-sensitive guard, so
// this script parses with Babel instead. Babel is already resolved in this tree via
// @vitejs/plugin-react (which runs Babel over these same .tsx files), so it adds no
// download weight and is proven to accept this codebase's syntax. The AST walk below
// is a faithful port of the previous TypeScript-AST version.
import { parse } from "@babel/parser";
import { isStringLiteral, isTemplateLiteral, isJSXAttribute, isJSXIdentifier, isCallExpression, isIdentifier, VISITOR_KEYS } from "@babel/types";
import postcss from "postcss";
import tailwindPostcss from "@tailwindcss/postcss";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const webRoot = path.resolve(scriptDir, "..");
const srcDir = path.join(webRoot, "src");

// Prefixes for the utilities that consume a color token. A token flagged here is
// only an ERROR when the real engine did not generate it (see below). Membership
// runs through the engine, so broadening the check to every utility family is a
// one-line edit to this list — deferred to keep this first landing free of false
// positives and matched to issue #170's text-/bg-/border- scope.
const COLOR_PREFIXES = [
  "text-", "bg-", "border-", "ring-", "fill-", "stroke-", "decoration-",
  "divide-", "from-", "via-", "to-", "accent-", "caret-", "placeholder-",
  "outline-", "shadow-",
];

// ── 1. Collect source files (mirrors tailwind.config.js `content`) ─────────────
const tsFiles = [];
const walkSrc = (dir) => {
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const abs = path.join(dir, e.name);
    if (e.isDirectory()) walkSrc(abs);
    else if (/\.(ts|tsx)$/.test(e.name) && !/\.test\.(ts|tsx)$/.test(e.name)) tsFiles.push(abs);
  }
};
walkSrc(srcDir);
tsFiles.sort();

// ── 2. Extract candidate class tokens ──────────────────────────────────────────
// usages: { file, line, token }. Recorded per occurrence, deduped by
// file:line:token afterwards (a `cx(...)` nested inside a `className` is reached
// by both collectors below, which is fine — the dedup collapses it).
const usages = [];
const push = (rawTokens, file, line) => {
  for (const tok of rawTokens) {
    if (!tok || tok.includes("${")) continue; // drop dynamic fragments defensively
    usages.push({ file, line, token: tok });
  }
};

// Split a static quasi/literal into whitespace tokens, dropping the fragment that
// abuts a `${…}` interpolation. `hasBefore`/`hasAfter` say whether an
// interpolation sits immediately before/after this piece of text; if the text
// does not START with whitespace its first token is a continuation of the prior
// `${…}` (e.g. the `red` in `text-${x}red`) and if it does not END with
// whitespace its last token is a prefix for the next `${…}` (e.g. `text-` before
// `${tone}`). Those are never complete class names, so they are dropped.
const quasiTokens = (text, hasBefore, hasAfter) => {
  const startsWs = /^\s/.test(text);
  const endsWs = /\s$/.test(text);
  const toks = text.split(/\s+/).filter(Boolean);
  if (hasBefore && !startsWs && toks.length) toks.shift();
  if (hasAfter && !endsWs && toks.length) toks.pop();
  return toks;
};

// Generic child walk — the Babel equivalent of `ts.forEachChild`. VISITOR_KEYS
// lists exactly the child-bearing properties for a node type, so this skips
// `loc`, comments and other non-AST fields (prose can never be read as classes).
const eachChild = (node, cb) => {
  const keys = VISITOR_KEYS[node.type];
  if (!keys) return;
  for (const key of keys) {
    const val = node[key];
    if (Array.isArray(val)) {
      for (const c of val) if (c && typeof c.type === "string") cb(c);
    } else if (val && typeof val.type === "string") {
      cb(val);
    }
  }
};

for (const abs of tsFiles) {
  const rel = path.relative(webRoot, abs);
  const src = readFileSync(abs, "utf8");
  // JSX lives only in `.tsx` here (TS requires it); enabling `jsx` for a plain
  // `.ts` would misread a `<T>x` angle-bracket type assertion as JSX, so gate it.
  const plugins = abs.endsWith(".tsx") ? ["typescript", "jsx"] : ["typescript"];
  const ast = parse(src, { sourceType: "module", plugins });
  const lineOf = (node) => node.loc.start.line; // Babel loc is already 1-based

  // Collect every string/template literal inside a known className/cx subtree,
  // recursing through ternaries, logical `&&`, arrays and nested cx() to reach
  // them. Template quasis get the abutting-fragment drop; interpolated
  // expressions are recursed so `${cond ? "text-danger" : ""}` is still seen.
  // Babel folds NoSubstitutionTemplateLiteral and TemplateExpression into one
  // TemplateLiteral node (quasis.length === expressions.length + 1): quasi `i`
  // has an interpolation before it iff `i > 0`, and after it iff `i < n` — which
  // reproduces the old head/span hasBefore/hasAfter flags exactly, including the
  // no-interpolation case (a single quasi, both flags false).
  const collect = (node) => {
    if (isStringLiteral(node)) {
      push(quasiTokens(node.value, false, false), rel, lineOf(node));
      return;
    }
    if (isTemplateLiteral(node)) {
      const n = node.expressions.length;
      node.quasis.forEach((q, i) => {
        const text = q.value.cooked ?? q.value.raw;
        push(quasiTokens(text, i > 0, i < n), rel, lineOf(q));
      });
      node.expressions.forEach(collect);
      return;
    }
    eachChild(node, collect);
  };

  const walk = (node) => {
    if (isJSXAttribute(node) && isJSXIdentifier(node.name) && node.name.name === "className" && node.value) {
      // node.value is a StringLiteral (className="…") or a JSXExpressionContainer
      // (className={…}); collect() recurses into the container's expression.
      collect(node.value);
    } else if (isCallExpression(node) && isIdentifier(node.callee) && node.callee.name === "cx") {
      node.arguments.forEach(collect);
    }
    eachChild(node, walk);
  };
  walk(ast.program);
}

// index.html carries only the static app shell (no JSX/cx), so its classes come
// from `class="…"` attributes. Comments are blanked first (offsets preserved for
// line numbers) so prose can never be read as classes — same rule as the AST path.
const htmlPath = path.join(webRoot, "index.html");
const html = readFileSync(htmlPath, "utf8").replace(/<!--[\s\S]*?-->/g, (m) => m.replace(/[^\n]/g, " "));
const attrRe = /\bclass(?:Name)?\s*=\s*("([^"]*)"|'([^']*)')/g;
let hm;
while ((hm = attrRe.exec(html)) !== null) {
  const value = hm[2] !== undefined ? hm[2] : hm[3];
  const line = html.slice(0, hm.index).split("\n").length;
  push(value.split(/\s+/).filter(Boolean), "index.html", line);
}

// Dedup by file:line:token (collapses the className/cx double-walk).
const seen = new Set();
const dedup = [];
for (const u of usages) {
  const key = `${u.file}\t${u.line}\t${u.token}`;
  if (seen.has(key)) continue;
  seen.add(key);
  dedup.push(u);
}

// ── 3. Membership via the real engine ──────────────────────────────────────────
// Tailwind v4's PostCSS plugin (@tailwindcss/postcss) reads its content from
// source files rather than a `content: [{ raw }]` config, so we feed the
// collected tokens through a throwaway HTML file: `source(none)` disables the
// automatic filesystem scan so ONLY these tokens are considered, `@config` loads
// the project's theme exactly as src/index.css does, and `@source` points the
// scanner at the token file. A token that generates no rule is an unknown class.
const uniqueTokens = [...new Set(dedup.map((u) => u.token))];
const tmpDir = mkdtempSync(path.join(os.tmpdir(), "uzi-check-styles-"));
let css;
try {
  const tokenFile = path.join(tmpDir, "tokens.html");
  writeFileSync(tokenFile, `<div class="${uniqueTokens.join(" ")}"></div>\n`);
  const input = [
    '@import "tailwindcss" source(none);',
    '@config "./tailwind.config.js";',
    `@source "${tokenFile}";`,
  ].join("\n");
  css = (await postcss([tailwindPostcss()]).process(input, {
    from: path.join(webRoot, "check-styles.virtual.css"),
  })).css;
} finally {
  rmSync(tmpDir, { recursive: true, force: true });
}

// Escape-tolerant selector test: matches `.<token>` allowing tailwind's optional
// backslash escapes for `/` (opacity), `[]` (arbitrary values) and `:` (variants),
// with a trailing boundary so `text-x` does not match `.text-xs`.
const esc = (t) => "\\." + [...t].map((c) => (/[a-zA-Z0-9-]/.test(c) ? c : "\\\\?" + c.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))).join("") + "(?![\\w-])";
const knownCache = new Map();
const known = (t) => {
  if (!knownCache.has(t)) knownCache.set(t, new RegExp(esc(t)).test(css));
  return knownCache.get(t);
};

// Strip leading variant segments (`hover:`, `md:`, `group-hover:`, …) and a
// leading `-` (negative utilities) to reach the bare stem we prefix-match.
const bareForm = (token) => token.replace(/^(-)?([a-z][a-z0-9-]*:)+/, "").replace(/^-/, "");

// ── 4. Scope + report ──────────────────────────────────────────────────────────
const errors = [];
for (const u of dedup) {
  const bare = bareForm(u.token);
  if (COLOR_PREFIXES.some((p) => bare.startsWith(p)) && !known(u.token)) {
    errors.push(`${u.file}:${u.line}: unknown Tailwind class "${u.token}" — its stem is not in tailwind.config.js (it renders silently as no style)`);
  }
}

if (errors.length > 0) {
  for (const e of errors) console.error(`ERROR ${e}`);
  console.error(`\ncheck-styles: FAILED with ${errors.length} error(s)`);
  process.exit(1);
}
console.log(`check-styles: OK — ${dedup.length} class usages validated`);
