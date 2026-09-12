// publish-shots.mjs — copy REVIEWED staged screenshots to their manifest targets.
// Run this only after eyeballing the staged PNGs (layout + leak scan). For each
// themed shot it copies stage/<slug>-<theme>.png into every target dir named in
// shots.json: readme -> <repo>/.github/readme, blog -> --blog-dir. Filenames are
// identical in both places, so the one-time <picture>/html.dark markup keeps
// working across every regen.
//
// Usage:
//   node publish-shots.mjs --manifest <shots.json> --stage <dir> [opts]
//     --only <slugs>     comma list (default: all themed shots present in stage)
//     --readme-dir <d>   override README image dir (default <repo>/.github/readme)
//     --blog-dir <d>     override blog image dir (default ~/stuff/gitrepos/wxs/wxs/hai/static/images/uzi)
//     --dry-run          list what would copy, copy nothing

import { readFileSync, copyFileSync, existsSync, mkdirSync } from "node:fs";
import { join, resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import { homedir } from "node:os";

const args = process.argv.slice(2);
const opt = (n, d) => { const i = args.indexOf(`--${n}`); return i === -1 ? d : args[i + 1]; };
const has = (n) => args.includes(`--${n}`);

const MANIFEST = opt("manifest");
const STAGE = opt("stage");
if (!MANIFEST || !STAGE) { console.error("usage: node publish-shots.mjs --manifest <shots.json> --stage <dir> [opts]"); process.exit(2); }
const M = JSON.parse(readFileSync(MANIFEST, "utf8"));
const ONLY = opt("only") ? opt("only").split(",").map((s) => s.trim()) : null;
const DRY = has("dry-run");

const skillDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const repoRoot = resolve(skillDir, "../../..");
const dirs = {
  readme: opt("readme-dir", join(repoRoot, ".github/readme")),
  blog: opt("blog-dir", join(homedir(), "stuff/gitrepos/wxs/wxs/hai/static/images/uzi")),
};

let copied = 0, missing = 0;
for (const shot of M.shots) {
  if (!shot.themed || shot.manual) continue; // non-themed, or hand-placed (e.g. TUI) shots aren't published by this tool
  if (ONLY && !ONLY.includes(shot.slug)) continue;
  const themes = Object.keys(M.themes);
  for (const t of themes) {
    const src = join(STAGE, `${shot.slug}-${t}.png`);
    if (!existsSync(src)) { if (ONLY) { console.log(`missing in stage: ${shot.slug}-${t}.png`); missing++; } continue; }
    for (const target of shot.targets) {
      const dir = dirs[target];
      if (!dir) { console.log(`no dir for target '${target}'`); continue; }
      const dest = join(dir, `${shot.slug}-${t}.png`);
      if (DRY) { console.log(`would copy ${shot.slug}-${t}.png -> ${dest}`); continue; }
      mkdirSync(dir, { recursive: true });
      copyFileSync(src, dest);
      console.log(`copied ${shot.slug}-${t}.png -> ${target}`);
      copied++;
    }
  }
}
console.log(DRY ? "dry run: nothing copied" : `done: ${copied} file(s) copied${missing ? `, ${missing} missing in stage` : ""}`);
if (missing) process.exitCode = 1; // a requested --only variant was missing: fail so a wrapper doesn't report success
