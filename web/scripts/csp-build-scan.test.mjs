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

// Output lines are `<file>: <pattern>: <excerpt>`; the pattern name is asserted on its own,
// so a fixture that another pattern would still catch (`Function(` inside `new Function(`)
// goes red when its own pattern is removed.
function patternsOf(r, dir) {
  return r.stdout
    .split("\n")
    .filter((l) => l.startsWith(dir))
    .map((l) => {
      const rest = l.slice(l.indexOf(": ", dir.length) + 2);
      return rest.slice(0, rest.indexOf(": "));
    });
}

describe("csp-build-scan hits", () => {
  const cases = [
    ["eval(", { "assets/a.js": "var x=eval('1');" }, ["eval("]],
    ["new Function (no paren)", { "assets/a.mjs": "var f=new Function;" }, ["new Function"]],
    ["new Function with a call also trips the minified form", { "assets/a.mjs": "var f=new Function('a','return a');" }, ["new Function", "Function("]],
    ["Function(", { "assets/a.js": "var f=Function('a','return a');" }, ["Function("]],
    ["Function( at line start", { "assets/a.js": "Function('return 1')()" }, ["Function("]],
    ["inline <script>", { "index.html": "<html><script>alert(1)</script></html>" }, ["inline <script>"]],
    ["inline <script> with data-src", { "index.html": '<script data-src="x">alert(1)</script>' }, ["inline <script>"]],
    ["inline <script> with src text inside another value", { "index.html": '<script title="src=x">alert(1)</script>' }, ["inline <script>"]],
    ["inline <script> whose attribute value holds a >", { "index.html": '<script title="a>b src=x">alert(1)</script>' }, ["inline <script>"]],
  ];
  for (const [label, files, patterns] of cases) {
    it(`flags ${label}`, () => {
      const dir = dirWith(files);
      const r = scan(dir);
      assert.equal(r.status, 1, r.stdout + r.stderr);
      assert.deepEqual(patternsOf(r, dir), patterns, r.stdout);
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

  for (const [label, html] of [
    ["a quoted src", '<script src="/a.js"></script>'],
    ["an unquoted src after other attributes", "<script type=module src=/a.js></script>"],
    ["an uppercase SRC", '<SCRIPT SRC="/a.js"></SCRIPT>'],
  ]) {
    it(`does not flag ${label}`, () => {
      const r = scan(dirWith({ "index.html": html }));
      assert.equal(r.status, 0, r.stdout + r.stderr);
    });
  }
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
