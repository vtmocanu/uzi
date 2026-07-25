#!/usr/bin/env node
// Build-time guard for the in-app docs (PRD #7 M4). Runs standalone
// (`npm run check-docs`) and as the first step of `npm run build`, so a
// frontmatter/link/order/image-size regression fails the image build — the only
// gate there is until CI exists.
//
// Path note: this file lives at web/scripts/, so docs/ is `../../docs` from here
// (a different depth than the viewer's `../../../docs` in web/src/lib/docs.ts).
// Both only work because the web/ <-> docs/ sibling layout is preserved in the
// build context (docker-compose.yml web.build.context + web/Dockerfile).
//
// Checks (fail the build): missing/invalid frontmatter (README.md exempt),
// duplicate `order` among `user` pages, broken relative links (doc->doc and
// doc->img), and any docs/img/* over the per-image byte budget.
// Warns (does not fail): a `user` page whose body exceeds the line budget.
import { readFileSync, readdirSync, existsSync, statSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..", "..");
const docsDir = path.join(repoRoot, "docs");
const imgDir = path.join(docsDir, "img");

const AUDIENCES = new Set(["user", "operator", "design", "contributor"]);
const LINE_BUDGET = 60; // body lines per user page (house style, PRD #7)
const MAX_IMAGE_BYTES = 300 * 1024; // ships to every visitor; keep it lean

const errors = [];
const warnings = [];
const fail = (file, msg) => errors.push(`${file}: ${msg}`);
const warn = (file, msg) => warnings.push(`${file}: ${msg}`);

// Leading-fence frontmatter parser — a mirror of web/src/lib/docs.ts so the
// gate accepts exactly what the viewer parses. Returns meta:null when there is
// no leading `---` fence at byte 0.
function parseFrontmatter(raw) {
  if (!raw.startsWith("---\n")) return { meta: null, body: raw };
  const close = raw.indexOf("\n---", 4);
  if (close === -1) return { meta: null, body: raw };
  const yaml = raw.slice(4, close);
  const afterFence = raw.indexOf("\n", close + 4);
  const body = (afterFence === -1 ? "" : raw.slice(afterFence + 1)).replace(/^\n+/, "");
  const meta = {};
  for (const line of yaml.split("\n")) {
    const m = /^([A-Za-z]+):\s*(.*)$/.exec(line.trim());
    if (m) meta[m[1]] = m[2].trim();
  }
  return { meta, body };
}

// Drop fenced and inline code so shell/code snippets that happen to contain
// `](` are not mistaken for markdown links.
function stripCode(md) {
  return md.replace(/```[\s\S]*?```/g, "").replace(/`[^`\n]*`/g, "");
}

// Inline links and images: [text](target) / ![alt](target). Captures the URL
// up to the first whitespace or `)`, discarding any optional "title".
function extractTargets(md) {
  const targets = [];
  const re = /!?\[[^\]]*\]\(\s*([^)\s]+)[^)]*\)/g;
  let m;
  while ((m = re.exec(md)) !== null) targets.push(m[1]);
  return targets;
}

const isExternal = (t) => /^[a-z][a-z0-9+.-]*:/i.test(t) || t.startsWith("//");

// The web image build context is trimmed to web/ + docs/ (see the root
// .dockerignore + Dockerfile), so repo-root link targets (../ARCHITECTURE.md,
// ../plan.md) are absent there by design — the viewer rewrites them to GitLab
// anyway. Links resolving INSIDE docs/ (doc->doc, doc->img) are always in
// context and always checked; targets OUTSIDE docs/ are only checked in a full
// checkout, so the containerized build stays green.
const fullCheckout = existsSync(path.join(repoRoot, ".git"));

const files = readdirSync(docsDir)
  .filter((f) => f.endsWith(".md"))
  .sort();

const orderOwner = new Map(); // order value -> filename (duplicate detection)

for (const file of files) {
  if (file === "README.md") continue; // exempt: it is the docs index/meta page
  const raw = readFileSync(path.join(docsDir, file), "utf8");
  const { meta, body } = parseFrontmatter(raw);

  if (meta === null) {
    fail(file, "missing or malformed frontmatter (needs a leading `---` fence at byte 0)");
  } else {
    if (!meta.title) fail(file, "frontmatter: missing `title`");
    if (!meta.audience) {
      fail(file, "frontmatter: missing `audience`");
    } else if (!AUDIENCES.has(meta.audience)) {
      fail(file, `frontmatter: invalid audience "${meta.audience}" (expected ${[...AUDIENCES].join("|")})`);
    }

    if (meta.audience === "user") {
      const raw_order = meta.order;
      if (raw_order === undefined || raw_order === "") {
        fail(file, "frontmatter: `order` is required for audience:user pages");
      } else if (!Number.isFinite(Number(raw_order))) {
        fail(file, `frontmatter: \`order\` must be a number (got "${raw_order}")`);
      } else {
        const order = Number(raw_order);
        if (orderOwner.has(order)) {
          fail(file, `duplicate order ${order} (also used by ${orderOwner.get(order)})`);
        } else {
          orderOwner.set(order, file);
        }
      }

      const bodyLines = body.replace(/\n+$/, "").split("\n").length;
      if (bodyLines > LINE_BUDGET) {
        warn(file, `${bodyLines} body lines exceed the ${LINE_BUDGET}-line house-style budget`);
      }
    }
  }

  const noCode = stripCode(raw);

  // Reference-style links are invisible to the inline-link existence check below
  // and can render broken, so the gate forbids them: fail on link/image
  // reference definitions (`[label]: target`) and full/collapsed reference
  // usages (`[text][ref]`, `[text][]`). Code is already stripped so snippets do
  // not trip it. The "inline links only" convention is documented in M5.
  if (/^ {0,3}\[[^\]]+\]:\s+\S/m.test(noCode)) {
    fail(file, "reference-style link/image definition ([label]: target) found — use inline links only");
  }
  if (/\]\[[^\]]*\]/.test(noCode)) {
    fail(file, "reference-style link usage ([text][ref]) found — use inline links only");
  }

  // Relative link/image existence (doc->doc and doc->img). External URLs and
  // pure `#anchor` fragments are skipped; fragments/queries are stripped before
  // resolving the file path (anchors within a target are not verified).
  for (const target of extractTargets(noCode)) {
    if (isExternal(target) || target.startsWith("#")) continue;
    const filePart = target.split("#")[0].split("?")[0];
    if (filePart === "") continue;
    const abs = path.resolve(docsDir, filePart);
    const insideDocs = abs === docsDir || abs.startsWith(docsDir + path.sep);
    if (!insideDocs && !fullCheckout) continue;
    if (!existsSync(abs)) {
      fail(file, `broken relative link: ${target} -> ${path.relative(repoRoot, abs)} (not found)`);
    }
  }
}

// Relative-link existence OUTSIDE docs/ (issue #132). ARCHITECTURE.md and
// specs/*.md link PRDs heavily, and nothing validated them — so every
// `git mv prds/X.md prds/done/X.md` silently broke its own inbound references.
// Measured 2026-07-25: 36 distinct dead PRD paths had accumulated, 11 in
// ARCHITECTURE.md alone. These files carry no frontmatter and no `order`, so
// only the link check applies to them.
//
// Gated on fullCheckout for the same reason as the doc->outside case above: the
// web image build context is trimmed to web/ + docs/, so these files are absent
// there by design and the containerized build must stay green.
const extraLinkFiles = ["ARCHITECTURE.md", "README.md", "CLAUDE.md"];
if (existsSync(path.join(repoRoot, "specs"))) {
  for (const f of readdirSync(path.join(repoRoot, "specs")).sort()) {
    if (f.endsWith(".md")) extraLinkFiles.push(path.join("specs", f));
  }
}

if (fullCheckout) {
  for (const rel of extraLinkFiles) {
    const abs = path.join(repoRoot, rel);
    if (!existsSync(abs)) continue; // optional files
    const noCode = stripCode(readFileSync(abs, "utf8"));
    const baseDir = path.dirname(abs);
    for (const target of extractTargets(noCode)) {
      if (isExternal(target) || target.startsWith("#")) continue;
      const filePart = target.split("#")[0].split("?")[0];
      if (filePart === "") continue;
      if (!existsSync(path.resolve(baseDir, filePart))) {
        fail(rel, `broken relative link: ${target} (not found)`);
      }
    }
    // A link whose display text is itself a path must not name a file that does
    // not exist. Repairing a target and leaving the text behind resolves
    // correctly and reads wrong, sending a reader to a directory with nothing in
    // it — 8 such links existed before #132.
    //
    // The discriminator is "the TEXT does not resolve", NOT "text differs from
    // target": a correct link in specs/ reads
    // [prds/done/X.md](../prds/done/X.md), where the two legitimately differ by
    // the `../` a relative path needs. A first cut of this check compared them
    // literally and produced 18 false positives on correct links.
    for (const m of noCode.matchAll(/\[([^\]]*\.md)\]\(([^)\s]+)\)/g)) {
      const text = m[1];
      // Only text that names a PATH (contains a separator) makes a claim that
      // can go stale. A bare filename like [auth-design.md](docs/auth-design.md)
      // is a display name, not a claim — requiring the separator removed 4 false
      // positives on correct links.
      if (isExternal(m[2]) || text.includes(" ") || !text.includes("/")) continue;
      const textResolvesFromRoot = existsSync(path.resolve(repoRoot, text));
      const textResolvesFromHere = existsSync(path.resolve(baseDir, text));
      if (!textResolvesFromRoot && !textResolvesFromHere) {
        warn(rel, `link text "${text}" looks like a path but no such file exists (target "${m[2]}" may have been repaired without updating the text)`);
      }
    }
  }
}

// Per-image byte budget for everything shipped in docs/img/.
if (existsSync(imgDir)) {
  for (const f of readdirSync(imgDir).sort()) {
    const p = path.join(imgDir, f);
    const st = statSync(p);
    if (!st.isFile()) continue;
    if (st.size > MAX_IMAGE_BYTES) {
      fail(`img/${f}`, `${Math.round(st.size / 1024)} KB exceeds the ${MAX_IMAGE_BYTES / 1024} KB per-image budget`);
    }
  }
}

for (const w of warnings) console.warn(`WARN  ${w}`);
if (errors.length > 0) {
  for (const e of errors) console.error(`ERROR ${e}`);
  console.error(`\ncheck-docs: FAILED with ${errors.length} error(s), ${warnings.length} warning(s)`);
  process.exit(1);
}
console.log(`check-docs: OK - ${files.length} docs validated, ${warnings.length} warning(s)`);
