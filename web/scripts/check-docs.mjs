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

// WHAT MAKES THE CONTAINERIZED BUILD SAFE IS `.git` BEING ABSENT, NOT A TRIMMED
// CONTEXT. This comment said (here and again above extraLinkFiles below) that
// "the web image build context is trimmed to web/ + docs/". Measured 2026-08-03
// (PRD #103 M5) against the root .dockerignore, whose entire non-comment content
// is `.git .gitignore .gitmodules .env .env.* **/.env **/.env.* inspiration/
// api/ agent/ e2e/ web/node_modules web/dist`: the context is the repo root MINUS
// that named list, so it still CONTAINS prds/, adr/, specs/, controller/,
// deploy/, Formula/, fixtures/, .claude/, CLAUDE.md and ARCHITECTURE.md.
//
// The conclusion below is unchanged and the mechanism is not. `.git` is excluded,
// so `fullCheckout` is false in the image build and the whole outside-docs block
// is skipped there regardless of what else the context carries. A reader
// reasoning from the trimmed-context version would conclude that the existsSync
// guards are what keep the image build green and that adding a directory to
// extraLinkFiles could break it. Neither is true: the guards only make each entry
// optional, and nothing under `if (fullCheckout)` runs in the image at all.
//
// Links resolving INSIDE docs/ (doc->doc, doc->img) are always in context and
// always checked; targets OUTSIDE docs/ are only checked in a full checkout.
// Repo-root link targets (../ARCHITECTURE.md, ../plan.md) are absent from the
// viewer's view by design either way — the viewer rewrites them to GitLab.
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
// Gated on fullCheckout for the same reason as the doc->outside case above —
// read that comment for the mechanism, which is `.git` being absent from the
// image build context rather than the context being trimmed to web/ + docs/.
//
// prds/ and adr/ joined specs/ here in PRD #103 M5. They are the two remaining
// heavy PRD-linking populations, and adr/ is the worse of the two: an ADR's
// number is its originating issue number, so every one of the five opens with a
// `[prds/N-slug.md](../prds/N-slug.md)` line that goes stale the moment that PRD
// is moved to prds/done/. Three such links were dead when this landed.
//
// 🔴 FLAT, NOT RECURSIVE, AND readdirSync IS WHAT MAKES IT SO. A non-recursive
// read returns `done` and `mockups` as directory ENTRIES, and neither ends in
// `.md`, so the filter drops them without a stat. That is a load-bearing property
// of this loop rather than a happy accident: recursing would pull in
// prds/done/*.md, which carries 16 further true positives (all the same move rot)
// and would make this commit a 10-completed-PRD edit. Scoped flat by the user's
// ruling of 2026-08-03; prds/done/ is a KNOWN, DELIBERATE residual. Widening it
// is a decision, not a tidy-up.
const extraLinkFiles = ["ARCHITECTURE.md", "README.md", "CLAUDE.md"];
for (const dir of ["specs", "prds", "adr"]) {
  const abs = path.join(repoRoot, dir);
  if (!existsSync(abs)) continue; // optional directories
  for (const f of readdirSync(abs).sort()) {
    if (f.endsWith(".md")) extraLinkFiles.push(path.join(dir, f));
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
