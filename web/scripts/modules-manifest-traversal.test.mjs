// node --test web/scripts/modules-manifest-traversal.test.mjs
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { federationOnlyFiles, ManifestShapeError, zeroModuleRequestViolations, zeroModuleViolations } from "./modules-manifest-traversal.mjs";

// A synthetic manifest: index statically imports a shared vendor chunk; federation.ts is a
// dynamic entry that imports the same vendor chunk plus a federation-only runtime chunk.
const manifest = () => ({
  "index.html": { file: "assets/index-a.js", isEntry: true, imports: ["_vendor.js"], css: ["assets/index-a.css"] },
  "_vendor.js": { file: "assets/vendor-b.js" },
  "src/modules/federation.ts": {
    file: "assets/federation-c.js",
    isDynamicEntry: true,
    imports: ["_vendor.js", "_mf-runtime.js"],
  },
  "_mf-runtime.js": { file: "assets/mf-runtime-d.js", css: ["assets/mf-d.css"] },
});

const html = (extra = "") =>
  `<html><head><script type="module" src="/assets/index-a.js"></script>` +
  `<link rel="modulepreload" href="/assets/vendor-b.js">${extra}</head></html>`;

describe("federationOnlyFiles", () => {
  it("is F minus S: the runtime chunk and its css, not the shared vendor chunk", () => {
    const { only, S, F } = federationOnlyFiles(manifest());
    assert.deepEqual([...only].sort(), ["assets/federation-c.js", "assets/mf-d.css", "assets/mf-runtime-d.js"]);
    assert.ok(S.has("assets/vendor-b.js") && F.has("assets/vendor-b.js"));
  });

  it("rejects a manifest whose federation entry is missing", () => {
    const m = manifest();
    delete m["src/modules/federation.ts"];
    assert.throws(() => federationOnlyFiles(m), ManifestShapeError);
  });

  it("rejects a federation entry that is not a dynamic entry", () => {
    const m = manifest();
    m["src/modules/federation.ts"].isDynamicEntry = false;
    assert.throws(() => federationOnlyFiles(m), /not a dynamic entry/);
  });

  it("does not follow dynamicImports (they load on demand)", () => {
    const m = manifest();
    m["index.html"].dynamicImports = ["src/modules/federation.ts"];
    assert.ok(federationOnlyFiles(m).only.has("assets/federation-c.js"));
  });
});

describe("zeroModuleViolations", () => {
  it("passes when only index and shared vendor files are requested", () => {
    const v = zeroModuleViolations({
      manifest: manifest(),
      requestedPaths: ["/", "/assets/index-a.js", "/assets/vendor-b.js", "/assets/index-a.css", "/modules.json"],
      indexHtml: html(),
    });
    assert.deepEqual(v, []);
  });

  it("fails an eager request for a federation-only chunk", () => {
    const v = zeroModuleViolations({
      manifest: manifest(),
      requestedPaths: ["/assets/index-a.js", "/assets/mf-runtime-d.js?x=1"],
      indexHtml: html(),
    });
    assert.equal(v.length, 1);
    assert.match(v[0], /requested: assets\/mf-runtime-d\.js/);
  });

  it("fails a modulepreload of a federation-only chunk", () => {
    const v = zeroModuleViolations({
      manifest: manifest(),
      requestedPaths: [],
      indexHtml: html('<link rel="modulepreload" crossorigin href="/assets/federation-c.js">'),
    });
    assert.equal(v.length, 1);
    assert.match(v[0], /modulepreload/);
  });

  it("fails a script tag for a federation-only chunk", () => {
    const v = zeroModuleViolations({
      manifest: manifest(),
      requestedPaths: [],
      indexHtml: html('<script type="module" src="/assets/mf-runtime-d.js"></script>'),
    });
    assert.equal(v.length, 1);
    assert.match(v[0], /script tag/);
  });
});

describe("zeroModuleRequestViolations", () => {
  // The base shape: the entry chunk and its css only, plus one route chunk loaded on demand.
  const base = () => ({
    "index.html": {
      file: "assets/index-a.js",
      isEntry: true,
      css: ["assets/index-a.css"],
      dynamicImports: ["src/modules/federation.ts", "src/docs.ts"],
    },
    "src/docs.ts": { file: "assets/docs-e.js", isDynamicEntry: true },
    "src/modules/federation.ts": { file: "assets/federation-c.js", isDynamicEntry: true },
  });
  const plainHtml = '<html><head><script type="module" src="/assets/index-a.js"></script></head></html>';

  it("passes the base shape: entry, css and a route chunk", () => {
    const v = zeroModuleRequestViolations({
      manifest: base(),
      requestedPaths: ["/", "/assets/index-a.js", "/assets/index-a.css", "/assets/docs-e.js", "/modules.json", "/assets/font-1.woff2"],
      indexHtml: plainHtml,
    });
    assert.deepEqual(v, []);
  });

  it("fails an extra static chunk the entry imports (a bundler runtime chunk), requested", () => {
    const m = base();
    m["index.html"].imports = ["_rolldown-runtime.js"];
    m["_rolldown-runtime.js"] = { file: "assets/rolldown-runtime-f.js" };
    m["src/modules/federation.ts"].imports = ["_rolldown-runtime.js"];
    const v = zeroModuleRequestViolations({ manifest: m, requestedPaths: ["/assets/index-a.js", "/assets/rolldown-runtime-f.js"], indexHtml: plainHtml });
    assert.deepEqual(v, ["extra static chunk requested: assets/rolldown-runtime-f.js"]);
  });

  it("fails any modulepreload in index.html", () => {
    const v = zeroModuleRequestViolations({
      manifest: base(),
      requestedPaths: [],
      indexHtml: '<link rel="modulepreload" crossorigin href="/assets/rolldown-runtime-f.js">',
    });
    assert.deepEqual(v, ["index.html preloads a chunk the base build did not: assets/rolldown-runtime-f.js"]);
  });

  it("fails an unexpected js asset, and the federation chunk", () => {
    const v = zeroModuleRequestViolations({
      manifest: base(),
      requestedPaths: ["/assets/surprise-g.js", "/assets/federation-c.js"],
      indexHtml: plainHtml,
    });
    assert.deepEqual(v, ["unexpected asset requested: assets/surprise-g.js", "unexpected asset requested: assets/federation-c.js"]);
  });
});
