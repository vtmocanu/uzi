// node --test web/scripts/csp-build-scan.test.mjs
import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { after, describe, it } from "node:test";
import { fileURLToPath } from "node:url";

const SCAN = join(dirname(fileURLToPath(import.meta.url)), "csp-build-scan.mjs");
const tmp = mkdtempSync(join(tmpdir(), "csp-scan-"));
after(() => rmSync(tmp, { recursive: true, force: true }));

let n = 0;
function dirWith(files) {
  const dir = join(tmp, `d${n++}`);
  mkdirSync(join(dir, "assets"), { recursive: true });
  for (const [f, body] of Object.entries(files)) writeFileSync(join(dir, f), body);
  return dir;
}
const scan = (...dirs) => spawnSync(process.execPath, [SCAN, ...dirs], { encoding: "utf8" });

describe("csp-build-scan hits", () => {
  const cases = [
    ["eval(", { "assets/a.js": "var x=eval('1');" }],
    ["new Function", { "assets/a.mjs": "var f=new Function('a','return a');" }],
    ["Function(", { "assets/a.js": "var f=Function('a','return a');" }],
    ["Function( at line start", { "assets/a.js": "Function('return 1')()" }],
    ["inline <script>", { "index.html": "<html><script>alert(1)</script></html>" }],
  ];
  for (const [pattern, files] of cases) {
    it(`flags ${pattern}`, () => {
      const r = scan(dirWith(files));
      assert.equal(r.status, 1, r.stdout + r.stderr);
      assert.ok(r.stdout.includes(pattern.split(" at ")[0]), r.stdout);
    });
  }
});

describe("csp-build-scan non-hits", () => {
  it("passes controls", () => {
    const dir = dirWith({
      "assets/a.js":
        'const s=Function.prototype.toString.call(f);const t="evaluate";const u=isFunction(x);const v=a.Function(1);',
      "index.html": '<html><script type="module" src="/assets/a.js"></script></html>',
    });
    const r = scan(dir);
    assert.equal(r.status, 0, r.stdout + r.stderr);
  });
});

describe("csp-build-scan usage", () => {
  it("exits 2 for a missing dir", () => {
    assert.equal(scan(join(tmp, "nope")).status, 2);
  });
  it("exits 2 for an empty dir", () => {
    const dir = join(tmp, "empty");
    mkdirSync(dir);
    assert.equal(scan(dir).status, 2);
  });
  it("exits 2 with no args", () => {
    assert.equal(scan().status, 2);
  });
});
