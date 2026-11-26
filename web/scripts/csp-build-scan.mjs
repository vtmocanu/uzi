// Literal CSP scan of a build output (PRD #4686, "Security"; plan decision 9).
//
//   node scripts/csp-build-scan.mjs <dir>...
//
// Exit 0 = clean, 1 = hits (listed with file, pattern and a short excerpt), 2 = a
// directory is missing or holds no scannable file.
//
// This is a LITERAL text scan (no AST), not a proof of absence: dynamically constructed
// code is covered at runtime by `script-src 'self'` without 'unsafe-eval'. It looks in
// every .js/.mjs for `eval(`, `new Function` and the minified `Function(` at an
// identifier boundary, and in every .html for an inline <script> without `src`.

import { existsSync, readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const JS_PATTERNS = [
  { name: "eval(", re: /eval\(/g },
  { name: "new Function", re: /new\s+Function/g },
  // Minifiers rewrite `new Function(` to `Function(`. The boundary excludes
  // `Function.prototype...`, `isFunction(`, `x.Function(` and `$Function(`.
  { name: "Function(", re: /(^|[^A-Za-z0-9_$.])Function\(/g },
];
// The tag body allows `>` inside a quoted attribute value.
const INLINE_SCRIPT = /<script\b((?:"[^"]*"|'[^']*'|[^>"'])*)>/gi;
// One attribute: its name, then an optional quoted or unquoted value.
const ATTRIBUTE = /([^\s"'<>/=]+)(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s"'=<>`]+))?/g;

// True when the tag body carries an attribute NAMED src. Text inside another attribute's
// value (`title="src=x"`) and longer names (`data-src`) do not count.
function hasSrcAttribute(tagBody) {
  for (const m of tagBody.matchAll(ATTRIBUTE)) if (m[1].toLowerCase() === "src") return true;
  return false;
}

function excerpt(text, index) {
  const start = Math.max(0, index - 30);
  return text
    .slice(start, index + 50)
    .replace(/\s+/g, " ")
    .trim();
}

function scanText(file, text) {
  const hits = [];
  if (/\.(m?js)$/.test(file)) {
    for (const { name, re } of JS_PATTERNS) {
      re.lastIndex = 0;
      for (let m = re.exec(text); m; m = re.exec(text)) {
        hits.push({ file, pattern: name, excerpt: excerpt(text, m.index) });
      }
    }
  } else if (/\.html?$/.test(file)) {
    INLINE_SCRIPT.lastIndex = 0;
    for (let m = INLINE_SCRIPT.exec(text); m; m = INLINE_SCRIPT.exec(text)) {
      if (!hasSrcAttribute(m[1])) hits.push({ file, pattern: "inline <script>", excerpt: excerpt(text, m.index) });
    }
  }
  return hits;
}

function walk(dir, out) {
  for (const name of readdirSync(dir).sort()) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else if (/\.(m?js|html?)$/.test(name)) out.push(p);
  }
  return out;
}

/** Scan directories; returns { hits, files } or throws on a missing/empty dir. */
function scanDirs(dirs) {
  const hits = [];
  let files = 0;
  for (const dir of dirs) {
    if (!existsSync(dir) || !statSync(dir).isDirectory()) throw new Error(`not a directory: ${dir}`);
    const found = walk(dir, []);
    if (found.length === 0) throw new Error(`no .js/.mjs/.html files under ${dir}`);
    for (const f of found) {
      files++;
      hits.push(...scanText(f, readFileSync(f, "utf8")));
    }
  }
  return { hits, files };
}

function main(argv) {
  if (argv.length === 0) {
    console.error("usage: node scripts/csp-build-scan.mjs <dir>...");
    return 2;
  }
  let result;
  try {
    result = scanDirs(argv);
  } catch (e) {
    console.error(`csp-build-scan: ${e.message}`);
    return 2;
  }
  for (const h of result.hits) console.log(`${h.file}: ${h.pattern}: ${h.excerpt}`);
  console.log(`csp-build-scan: ${result.files} files scanned, ${result.hits.length} hits`);
  return result.hits.length > 0 ? 1 : 0;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) process.exitCode = main(process.argv.slice(2));
