// node --test web/scripts/vite-mf-no-new-function.test.mjs
// Runs the plugin against fixture COPIES of the pinned sdk files laid out like
// node_modules, so a test never touches the real install.
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { after, describe, it } from "node:test";
import { mfNoNewFunction, PINNED_SDK_VERSION } from "./vite-mf-no-new-function.mjs";

const UTILS = [
  "function getResourceUrl(module, sourceUrl) {",
  '\tif (!module.getPublicPath.startsWith("function")) publicPath = new Function(module.getPublicPath)();',
  '\telse publicPath = new Function("return " + module.getPublicPath)()();',
  "}",
  "export { getResourceUrl };",
  "",
].join("\n");
const NODE = [
  "function importNodeModule(name) {",
  "\tconst promise = new Function(\"name\", `return import(name)`)(name).then((res) => res);",
  "\treturn promise;",
  "}",
  "export { importNodeModule };",
  "",
].join("\n");

const tmp = mkdtempSync(join(tmpdir(), "mf-nnf-"));
after(() => rmSync(tmp, { recursive: true, force: true }));

function fixture(name, version, files) {
  const root = join(tmp, name, "node_modules/@module-federation/sdk");
  mkdirSync(join(root, "dist"), { recursive: true });
  writeFileSync(join(root, "package.json"), JSON.stringify({ name: "@module-federation/sdk", version }));
  for (const [f, body] of Object.entries(files)) writeFileSync(join(root, "dist", f), body);
  return root;
}

const plugin = mfNoNewFunction();
const run = (code, id) => plugin.transform(code, id);

describe("vite-mf-no-new-function", () => {
  it("replaces both utils.js sites", () => {
    const root = fixture("ok1", PINNED_SDK_VERSION, { "utils.js": UTILS });
    const out = run(UTILS, `${root}/dist/utils.js`).code;
    assert.ok(!out.includes("new Function"));
    assert.equal(out.split("dynamic code evaluation is disabled").length - 1, 2);
  });

  it("replaces the node.js site", () => {
    const root = fixture("ok2", PINNED_SDK_VERSION, { "node.js": NODE });
    const out = run(NODE, `${root}/dist/node.js?v=abc`).code;
    assert.ok(!out.includes("new Function"));
    assert.ok(out.includes("dynamic code evaluation is disabled"));
  });

  it("ignores every other module", () => {
    assert.equal(run("new Function('x')", "/x/node_modules/other/dist/utils.js"), null);
  });

  it("throws on a missing snippet", () => {
    const root = fixture("miss", PINNED_SDK_VERSION, { "utils.js": UTILS });
    const changed = UTILS.replace("new Function(module.getPublicPath)()", "something()");
    assert.throws(() => run(changed, `${root}/dist/utils.js`), /expected exactly one occurrence/);
  });

  it("throws on a duplicated snippet", () => {
    const root = fixture("dup", PINNED_SDK_VERSION, { "node.js": NODE });
    assert.throws(() => run(NODE + NODE, `${root}/dist/node.js`), /found 2/);
  });

  it("throws on an sdk version change", () => {
    const root = fixture("ver", "2.9.4", { "utils.js": UTILS });
    assert.throws(() => run(UTILS, `${root}/dist/utils.js`), /expected 2\.9\.3/);
  });

  it("the stub throws when called, not when evaluated", async () => {
    const root = fixture("stub", PINNED_SDK_VERSION, { "node.js": NODE });
    const out = run(NODE, `${root}/dist/node.js`).code;
    const file = join(tmp, "stub-out.mjs");
    writeFileSync(file, out);
    // Evaluating the module must not throw.
    const mod = await import(file);
    assert.throws(() => mod.importNodeModule("x"), /uzi: dynamic code evaluation is disabled/);
  });
});
