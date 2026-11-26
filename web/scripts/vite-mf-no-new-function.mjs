// Vite plugin: removes the three literal `new Function` sites from the PINNED
// @module-federation/sdk build so the host bundle carries no dynamic-code literal
// (PRD #4686, "Security": the CSP stays `script-src 'self'` with no 'unsafe-eval').
//
// runtime-core's own sites are removed by the `define` in vite.config.ts
// (FEDERATION_ALLOW_NEW_FUNCTION / __system_context__ -> "undefined"). The sdk's two
// files below carry UNGUARDED sites that no define can reach, so this transform replaces
// the exact snippets with an expression that throws only when it is CALLED, never when
// the module is evaluated. Those call paths (a remote entry using `getPublicPath`
// strings, and the node-only `importNodeModule`) are not used by uzi's same-origin
// `type: "module"` remotes.
//
// Pinned on purpose: the build FAILS if the installed sdk is not PINNED_SDK_VERSION or
// if any snippet is not found exactly once, so an sdk bump cannot silently reintroduce a
// literal or change what is stubbed. Bump the pin and the snippets together.

import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

export const PINNED_SDK_VERSION = "2.9.3";

// The expression every site is replaced with. It must not contain the literals the
// scanner (scripts/csp-build-scan.mjs) looks for.
const THROW_WHEN_CALLED = '(() => { throw new Error("uzi: dynamic code evaluation is disabled"); })()';

// Resolved-id suffix -> the exact snippets (each must occur exactly once).
const SDK_SNIPPETS = {
  "node_modules/@module-federation/sdk/dist/utils.js": [
    "new Function(module.getPublicPath)()",
    'new Function("return " + module.getPublicPath)()()',
  ],
  "node_modules/@module-federation/sdk/dist/node.js": ['new Function("name", `return import(name)`)(name)'],
};

function countOccurrences(code, snippet) {
  let n = 0;
  for (let i = code.indexOf(snippet); i !== -1; i = code.indexOf(snippet, i + snippet.length)) n++;
  return n;
}

function sdkRootOf(id) {
  // id ends with <root>/dist/<file>; the package.json sits at <root>.
  return id.slice(0, id.lastIndexOf("/dist/"));
}

/**
 * The pure rewrite behind the plugin. `readVersion(root)` returns the installed
 * sdk version for the package root that owns `id`.
 */
function rewriteSdk(code, id, readVersion) {
  const clean = id.split("?")[0].replace(/\\/g, "/");
  const suffix = Object.keys(SDK_SNIPPETS).find((s) => clean.endsWith(s));
  if (!suffix) return null;
  const version = readVersion(sdkRootOf(clean));
  assertPinned(version);
  let out = code;
  for (const snippet of SDK_SNIPPETS[suffix]) {
    const n = countOccurrences(out, snippet);
    if (n !== 1) {
      throw new Error(
        `vite-mf-no-new-function: expected exactly one occurrence of ${JSON.stringify(snippet)} in ${suffix}, found ${n}`,
      );
    }
    out = out.split(snippet).join(THROW_WHEN_CALLED);
  }
  return out;
}

function readInstalledVersion(root) {
  return JSON.parse(readFileSync(`${root}/package.json`, "utf8")).version;
}

function assertPinned(version) {
  if (version !== PINNED_SDK_VERSION) {
    throw new Error(
      `vite-mf-no-new-function: installed @module-federation/sdk is ${version}, expected ${PINNED_SDK_VERSION}; re-pin the snippets in web/scripts/vite-mf-no-new-function.mjs`,
    );
  }
}

// The sdk's package.json is not in its "exports" map, so resolve the entry point from
// the web directory and walk up to the package root that names the sdk.
function resolveInstalledSdkVersion() {
  const webDir = fileURLToPath(new URL("..", import.meta.url));
  let dir = dirname(createRequire(join(webDir, "package.json")).resolve("@module-federation/sdk"));
  for (;;) {
    try {
      const pkg = JSON.parse(readFileSync(join(dir, "package.json"), "utf8"));
      if (pkg.name === "@module-federation/sdk") return pkg.version;
    } catch {
      // no package.json here; keep walking up
    }
    const parent = dirname(dir);
    if (parent === dir) throw new Error("vite-mf-no-new-function: could not locate the @module-federation/sdk package root");
    dir = parent;
  }
}

/**
 * `options.resolveSdkVersion` is a test seam; production resolves the installed sdk.
 * Beyond the per-file checks, the plugin fails a build that does not transform both
 * target files while any @module-federation/ module was part of it, so an sdk layout
 * change cannot pass silently.
 */
export function mfNoNewFunction(options = {}) {
  const resolveSdkVersion = options.resolveSdkVersion ?? resolveInstalledSdkVersion;
  const transformed = new Set();
  let sawFederation = false;
  let isBuild = false;
  return {
    name: "uzi-mf-no-new-function",
    enforce: "pre",
    configResolved(config) {
      isBuild = config.command === "build";
    },
    buildStart() {
      transformed.clear();
      sawFederation = false;
      assertPinned(resolveSdkVersion());
    },
    transform(code, id) {
      const clean = id.split("?")[0].replace(/\\/g, "/");
      if (clean.includes("@module-federation/")) sawFederation = true;
      const out = rewriteSdk(code, id, readInstalledVersion);
      if (out === null) return null;
      transformed.add(Object.keys(SDK_SNIPPETS).find((s) => clean.endsWith(s)));
      return { code: out, map: null };
    },
    buildEnd(error) {
      if (error || !isBuild || !sawFederation) return;
      const missing = Object.keys(SDK_SNIPPETS).filter((s) => !transformed.has(s));
      if (missing.length > 0) {
        throw new Error(
          `vite-mf-no-new-function: the federation runtime is in the build but ${missing.join(", ")} was never transformed; the sdk layout changed, re-pin web/scripts/vite-mf-no-new-function.mjs`,
        );
      }
    },
  };
}
