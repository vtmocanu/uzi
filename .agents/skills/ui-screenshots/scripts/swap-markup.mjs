// swap-markup.mjs — one-time conversion of the README and blog image tags to
// theme-aware form, driven by shots.json. After this runs once, every regen just
// re-publishes the same -light/-dark filenames and no markup changes.
//
//   README (GitHub): <img src=".github/readme/<slug>.png" …> -> <picture> with a
//     prefers-color-scheme:dark <source> and the -light png as the <img> fallback.
//   Blog (Hextra):  <img … src="/images/uzi/<slug>.png" …> -> a -light + -dark pair
//     with classes uzi-shot-light / uzi-shot-dark, swapped by CSS on html.dark (the
//     blog's own toggle, which follows the browser by default). CSS is added to
//     custom.css if missing. prefers-color-scheme is deliberately NOT used on the
//     blog so it stays in sync with the on-page theme switch.
//
// Idempotent: a slug already converted (its -light.png referenced) is skipped.
//
// Usage:
//   node swap-markup.mjs --manifest <shots.json> --readme <README.md> --blog <post.md> --css <custom.css> [--only slugs] [--dry-run]

import { readFileSync, writeFileSync } from "node:fs";

const args = process.argv.slice(2);
const opt = (n, d) => { const i = args.indexOf(`--${n}`); return i === -1 ? d : args[i + 1]; };
const has = (n) => args.includes(`--${n}`);

const M = JSON.parse(readFileSync(opt("manifest"), "utf8"));
const ONLY = opt("only") ? opt("only").split(",").map((s) => s.trim()) : null;
const DRY = has("dry-run");
const themedSlugs = (target) => M.shots.filter((s) => s.themed && !s.manual && s.targets.includes(target) && (!ONLY || ONLY.includes(s.slug)));
const reEsc = (s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
const attr = (tag, name) => { const m = tag.match(new RegExp(name + '="([^"]*)"')); return m ? m[1] : null; };

let changed = [];

// ── README: <img …> -> <picture> ─────────────────────────────────────────────
const readmePath = opt("readme");
if (readmePath) {
  let src = readFileSync(readmePath, "utf8");
  for (const shot of themedSlugs("readme")) {
    const slug = shot.slug;
    if (src.includes(`.github/readme/${slug}-light.png`)) continue; // already converted
    const rx = new RegExp(`<img\\s+src="\\.github/readme/${reEsc(slug)}\\.png"[\\s\\S]*?>`);
    const m = src.match(rx);
    if (!m) continue;
    const tag = m[0];
    const width = attr(tag, "width");
    const alt = attr(tag, "alt") || "";
    const picture =
      `<picture>\n` +
      `      <source media="(prefers-color-scheme: dark)" srcset=".github/readme/${slug}-dark.png">\n` +
      `      <img src=".github/readme/${slug}-light.png"${width ? ` width="${width}"` : ""}\n` +
      `           alt="${alt}">\n` +
      `    </picture>`;
    src = src.replace(tag, picture);
    changed.push(`README ${slug}`);
  }
  if (!DRY) writeFileSync(readmePath, src);
}

// ── Blog: single <img> -> -light + -dark pair with swap classes ───────────────
const blogPath = opt("blog");
if (blogPath) {
  let src = readFileSync(blogPath, "utf8");
  for (const shot of themedSlugs("blog")) {
    const slug = shot.slug;
    if (src.includes(`/images/uzi/${slug}-light.png`)) continue; // already converted
    const rx = new RegExp(`<img[^>]*?src="/images/uzi/${reEsc(slug)}\\.png"[^>]*?/?>`);
    const m = src.match(rx);
    if (!m) continue;
    const tag = m[0];
    const withClass = (variant) => {
      let t = tag.replace(`/images/uzi/${slug}.png`, `/images/uzi/${slug}-${variant}.png`);
      const cls = `uzi-shot uzi-shot-${variant}`;
      if (/class="/.test(t)) t = t.replace(/class="([^"]*)"/, `class="$1 ${cls}"`);
      else t = t.replace(/^<img/, `<img class="${cls}"`);
      return t;
    };
    src = src.replace(tag, withClass("light") + withClass("dark"));
    changed.push(`blog ${slug}`);
  }
  if (!DRY) writeFileSync(blogPath, src);
}

// ── CSS: add the html.dark swap rule once ─────────────────────────────────────
const cssPath = opt("css");
if (cssPath) {
  let css = readFileSync(cssPath, "utf8");
  if (!css.includes(".uzi-shot-dark")) {
    const block =
      `\n/* uzi UI screenshots: show the light or dark capture per the blog theme (html.dark), */\n` +
      `/* which follows the browser by default and honors the on-page toggle. */\n` +
      `/* img+class specificity beats theme rules like .hai-carousel-slide img (display:block). */\n` +
      `img.uzi-shot { display: block; margin-inline: auto; }\n` +
      `img.uzi-shot-dark { display: none; }\n` +
      `:is(.dark) img.uzi-shot-light { display: none; }\n` +
      `:is(.dark) img.uzi-shot-dark { display: block; }\n`;
    css += block;
    if (!DRY) writeFileSync(cssPath, css);
    changed.push("custom.css uzi-shot rules");
  }
}

console.log(DRY ? "DRY RUN — no files written" : "written");
console.log(changed.length ? "changed:\n  " + changed.join("\n  ") : "no changes (already converted?)");
