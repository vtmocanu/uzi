// The "no federation code for a zero-module deployment" check (PRD #4686, plan decision 10),
// as pure functions over a Vite build manifest (`build.manifest: true`).
//
//   S = every file reachable from index.html by STATIC imports (what a page load fetches)
//   F = every file reachable from src/modules/federation.ts by static imports
//   F \ S = the federation-only files: they exist for module hosts only
//
// A document with zero configured modules must never request, preload or script-tag an
// F \ S file. Shared vendor chunks (react and friends) sit in F and S both and are fine.
// Only `imports` are followed: `dynamicImports` are loaded on demand, which is the point.

const INDEX_KEY = "index.html";
const FEDERATION_KEY = "src/modules/federation.ts";

export class ManifestShapeError extends Error {}

function closure(manifest, startKey) {
  const files = new Set();
  const seen = new Set();
  const stack = [startKey];
  while (stack.length > 0) {
    const key = stack.pop();
    if (seen.has(key)) continue;
    seen.add(key);
    const chunk = manifest[key];
    if (!chunk) throw new ManifestShapeError(`manifest has no entry ${JSON.stringify(key)}`);
    files.add(chunk.file);
    for (const css of chunk.css ?? []) files.add(css);
    for (const dep of chunk.imports ?? []) stack.push(dep);
  }
  return files;
}

/** Returns { S, F, only } (sets of manifest file paths). Throws ManifestShapeError when the
 *  federation module is missing or is not a dynamic entry (the build no longer isolates it). */
export function federationOnlyFiles(manifest) {
  if (!manifest[INDEX_KEY]) throw new ManifestShapeError(`manifest has no ${INDEX_KEY} entry`);
  const fed = manifest[FEDERATION_KEY];
  if (!fed) throw new ManifestShapeError(`manifest has no ${FEDERATION_KEY} entry`);
  if (fed.isDynamicEntry !== true) {
    throw new ManifestShapeError(`${FEDERATION_KEY} is not a dynamic entry; it is bundled into the first load`);
  }
  const S = closure(manifest, INDEX_KEY);
  const F = closure(manifest, FEDERATION_KEY);
  const only = new Set([...F].filter((f) => !S.has(f)));
  return { S, F, only };
}

const SCRIPT_SRC = /<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["'][^>]*>/gi;
const PRELOAD_HREF = /<link\b[^>]*\brel\s*=\s*["']modulepreload["'][^>]*>/gi;
const HREF = /\bhref\s*=\s*["']([^"']+)["']/i;

function fileOfUrl(url) {
  let path = url;
  try {
    path = new URL(url, "http://host.invalid").pathname;
  } catch {
    // keep as is
  }
  return path.replace(/^\/+/, "");
}

/**
 * Violations of (c) and (d) for a zero-module document: `requestedPaths` are the URL paths
 * the document requested, `indexHtml` the served index.html. Returns a list of messages;
 * empty means the zero-module property holds.
 */
export function zeroModuleViolations({ manifest, requestedPaths, indexHtml }) {
  const { only } = federationOnlyFiles(manifest);
  const out = [];
  for (const p of requestedPaths) {
    const f = fileOfUrl(p);
    if (only.has(f)) out.push(`federation-only file requested: ${f}`);
  }
  for (let m = SCRIPT_SRC.exec(indexHtml); m; m = SCRIPT_SRC.exec(indexHtml)) {
    const f = fileOfUrl(m[1]);
    if (only.has(f)) out.push(`federation-only file in an index.html script tag: ${f}`);
  }
  for (let m = PRELOAD_HREF.exec(indexHtml); m; m = PRELOAD_HREF.exec(indexHtml)) {
    const href = HREF.exec(m[0]);
    if (href && only.has(fileOfUrl(href[1]))) out.push(`federation-only file in an index.html modulepreload: ${fileOfUrl(href[1])}`);
  }
  return out;
}

/**
 * The stricter zero-module request budget (PRD #4686 example 4: the only added request is
 * the small no-cache /modules.json fetch). With the base build the document loads exactly
 * its entry chunk and stylesheet; route chunks load on demand. So for a zero-module load:
 *
 *   - the entry chunk is the file of the `index.html` manifest entry (plus its css);
 *   - every OTHER file the entry reaches by static imports (a vendor split, a bundler
 *     runtime chunk) is an extra request the base build did not make, whether or not the
 *     federation chunk also uses it, and is refused, requested or preloaded;
 *   - a requested .js/.css file must be the entry, or a route chunk (a dynamic import of
 *     the entry other than federation.ts) with its own static closure.
 *
 * A deliberate future vendor split must update this expectation together with the PRD.
 */
export function zeroModuleRequestViolations({ manifest, requestedPaths, indexHtml }) {
  const index = manifest[INDEX_KEY];
  if (!index) throw new ManifestShapeError(`manifest has no ${INDEX_KEY} entry`);
  const entry = new Set([index.file, ...(index.css ?? [])]);
  const extraStatic = new Set([...closure(manifest, INDEX_KEY)].filter((f) => !entry.has(f)));
  const allowed = new Set(entry);
  for (const key of index.dynamicImports ?? []) {
    if (key === FEDERATION_KEY) continue;
    for (const f of closure(manifest, key)) allowed.add(f);
  }
  const out = [];
  for (const p of requestedPaths) {
    const f = fileOfUrl(p);
    if (extraStatic.has(f)) out.push(`extra static chunk requested: ${f}`);
    else if (/\.(m?js|css)$/.test(f) && f.startsWith("assets/") && !allowed.has(f)) {
      out.push(`unexpected asset requested: ${f}`);
    }
  }
  for (let m = PRELOAD_HREF.exec(indexHtml); m; m = PRELOAD_HREF.exec(indexHtml)) {
    const href = HREF.exec(m[0]);
    if (href) out.push(`index.html preloads a chunk the base build did not: ${fileOfUrl(href[1])}`);
  }
  return out;
}
