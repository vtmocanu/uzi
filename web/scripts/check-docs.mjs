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
// missing/duplicate `order` among the in-app-indexed audiences (`user` and,
// since issue #75, `operator` — each in its own order namespace), broken
// relative links (doc->doc and doc->img), and any docs/img/* over the per-image
// byte budget.
// Warns (does not fail): a `user` page whose body exceeds the line budget.
import { readFileSync, readdirSync, existsSync, statSync, realpathSync } from "node:fs";
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

// 🔴 WHAT MAKES THE CONTAINERIZED BUILD SAFE IS THE Dockerfile's COPY SET. Not a
// trimmed build context, and not `.dockerignore`. Three protections exist and only
// the first is a cause; the other two are its consequences:
//
//   (a) web/Dockerfile does `WORKDIR /app/web`, `COPY web/ ./` and
//       `COPY docs/ /app/docs/`, so /app contains EXACTLY web/ and docs/. This
//       file resolves repoRoot as `<scriptDir>/../..` = /app.
//   (b) therefore /app/.git cannot exist, so `fullCheckout` is false and the whole
//       outside-docs block below never runs there;
//   (c) therefore /app/prds and /app/adr do not exist either, so the extraLinkFiles
//       loop's existsSync guards find nothing to add.
//
// Nothing COPYs `.git`, `prds/` or `adr/` into /app, so (b) and (c) hold WHATEVER
// `.dockerignore` says — delete its `.git` line and `fullCheckout` is still false.
// `.dockerignore`'s real job here is the BUILD CONTEXT: it keeps the upload small
// and, load-bearing for `COPY web/ ./` specifically, keeps `web/node_modules` and
// `web/dist` out of the image.
//
// (Corrected twice, 2026-08-03, PRD #103 M5, and the second correction is the
// instructive one. This comment first said "the web image build context is trimmed
// to web/ + docs/ (see the root .dockerignore + Dockerfile)" — WRONG NOUN, RIGHT
// CITATIONS. The first fix corrected the noun and dropped the Dockerfile half,
// which was the true half, leaving a single causal claim resting on the one input
// that does not carry the weight. Both were mechanism errors in a comment about a
// mechanism error.)
//
// Links resolving INSIDE docs/ (doc->doc, doc->img) are always present and always
// checked; targets OUTSIDE docs/ are only checked in a full checkout. Repo-root
// link targets (../ARCHITECTURE.md, ../plan.md) are absent from the viewer's view
// by design either way — the viewer rewrites them to GitLab.
const fullCheckout = existsSync(path.join(repoRoot, ".git"));

const files = readdirSync(docsDir)
  .filter((f) => f.endsWith(".md"))
  .sort();

// The audiences that appear in an in-app index and therefore need an `order`:
// `user` (everyone) and `operator` (admins only, issue #75). Each keeps its own
// order namespace, so operator `order: 10` never collides with user `order: 10`.
const ORDERED_AUDIENCES = new Set(["user", "operator"]);
const orderOwnerByAudience = new Map(); // audience -> (order value -> filename)

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

    if (ORDERED_AUDIENCES.has(meta.audience)) {
      const raw_order = meta.order;
      if (raw_order === undefined || raw_order === "") {
        fail(file, `frontmatter: \`order\` is required for audience:${meta.audience} pages`);
      } else if (!Number.isFinite(Number(raw_order))) {
        fail(file, `frontmatter: \`order\` must be a number (got "${raw_order}")`);
      } else {
        const order = Number(raw_order);
        let owner = orderOwnerByAudience.get(meta.audience);
        if (!owner) {
          owner = new Map();
          orderOwnerByAudience.set(meta.audience, owner);
        }
        if (owner.has(order)) {
          fail(file, `duplicate order ${order} among audience:${meta.audience} pages (also used by ${owner.get(order)})`);
        } else {
          owner.set(order, file);
        }
      }
    }

    // The line budget is a `user` howto house-style rule only; operator docs are
    // reference material (configuration.md etc.) and are exempt.
    if (meta.audience === "user") {
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
// Gated on fullCheckout for the same reason as the doc->outside case above — read
// that comment for the mechanism, which is the Dockerfile's COPY set (/app holds
// only web/ and docs/), NOT `.dockerignore` and NOT a trimmed context.
//
// prds/ and adr/ joined specs/ here in PRD #103 M5. They are the two remaining
// heavy PRD-linking populations. adr/ is small but concentrated: an ADR's number
// is its TRACKING-ITEM number, which may be a PRD number or a bare issue number,
// so THREE of the five open with a relative
// `**PRD**: [prds/done/N-slug.md](../prds/done/N-slug.md)` line (0035, 0042,
// 0065) and the other two open with an EXTERNAL issue link and no relative link
// at all (0106, 0195). Two of those three relative links were dead when this
// landed — 0042 and 0065, both `git mv … prds/done/` rot — and they are the class
// this extension exists to catch.
//
// (Corrected 2026-08-03: the commit that ADDED this paragraph, in the same breath
// as correcting a different false comment in this same file, claimed "every one of
// the five" opens with such a line and that "three such links were dead". Both are
// false and the first defeats itself: BECAUSE the number is an issue number, an
// ADR need not have a PRD at all, and `adr/0106` says so in its own second line —
// "there is no PRD" — warning that a reader assuming otherwise "will go looking
// for prds/106-*.md and find nothing". The counterexample was inside the file the
// sentence was about. The third dead link was in prds/66-guardrail-enforcement.md,
// which is not an ADR opener.)
//
// 🔴 FLAT, NOT RECURSIVE, AND readdirSync IS WHAT MAKES IT SO. A non-recursive
// read returns `done` and `mockups` as directory ENTRIES, and neither ends in
// `.md`, so the filter drops them without a stat. That is a load-bearing property
// of this loop rather than a happy accident: recursing would pull in
// prds/done/*.md, which carries 16 further true positives (all the same move rot)
// and would make this commit a 10-completed-PRD edit. Scoped flat by the user's
// ruling of 2026-08-03; prds/done/ is a KNOWN, DELIBERATE residual. Widening it
// is a decision, not a tidy-up.
// `.claude/rules/*.md` carry moved CLAUDE.md content, including relative links that
// were written when that content sat at the repo root. PRD-less fix, 2026-08-04: the
// CLAUDE.md split silently broke one such link and this checker could not see it.
const extraLinkFiles = ["ARCHITECTURE.md", "README.md", "CLAUDE.md"];
// RECURSIVE on purpose: Claude Code discovers rules recursively ("All .md files
// are discovered recursively, so you can organize rules into subdirectories"), so a
// flat read here would skip .claude/rules/frontend/x.md -- a file the loader DOES
// load. That is the same shape as the bug this block exists to catch.
// FOLLOWS SYMLINKS, because the loader does: the docs recommend
// `ln -s ~/shared-claude-rules .claude/rules/shared` for sharing rules across
// projects, and `Dirent.isDirectory()` is FALSE for a symlink -- so the obvious
// walk skips exactly the shape the docs recommend, while symlinked *files* work
// fine and make it look supported. `seen` guards the cycles that following
// introduces (the docs promise the loader handles circular symlinks; we must too).
const seen = new Set();
const walkRules = (abs, rel) => {
  if (!existsSync(abs)) return;
  const real = realpathSync(abs);
  if (seen.has(real)) return;
  seen.add(real);
  for (const e of readdirSync(abs, { withFileTypes: true }).sort((a, b) => a.name.localeCompare(b.name))) {
    const nextAbs = path.join(abs, e.name);
    const nextRel = path.join(rel, e.name);
    const isDir = e.isDirectory() || (e.isSymbolicLink() && existsSync(nextAbs) && statSync(nextAbs).isDirectory());
    if (isDir) walkRules(nextAbs, nextRel);
    else if (e.name.endsWith(".md")) extraLinkFiles.push(nextRel);
  }
};
walkRules(path.join(repoRoot, ".claude", "rules"), ".claude/rules");
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
