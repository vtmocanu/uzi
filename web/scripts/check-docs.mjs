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

  // Relative link/image existence (doc->doc and doc->img). External URLs and
  // pure `#anchor` fragments are skipped; fragments/queries are stripped before
  // resolving the file path (anchors within a target are not verified).
  for (const target of extractTargets(stripCode(raw))) {
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
