import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// issue #1783 M1 review (N4) — the quiescence test view narrows (or fakes) the process table a reap
// sees, so production code must never install one. A source scan of agent/src pins that: only the
// setter's own definition names `setQuiescenceViewForTests`, the module state it writes is assigned
// nowhere else, and the one place a `view` is added to a request is the helper's stdin, from that
// test-set state.

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const OWNER = path.join(SRC, "run-quiescence.ts");

function sourceFiles(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return sourceFiles(p);
    return /\.(c|m)?(t|j)sx?$/.test(e.name) && !e.name.endsWith(".d.ts") ? [p] : [];
  });
}

/** The source with block and line comments removed (crude, but enough for these identifiers: a
 *  `//` directly after ":" or "\" is kept, so URLs and escapes in strings survive). */
function code(file: string): string {
  return fs
    .readFileSync(file, "utf8")
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/(^|[^:\\])\/\/.*$/gm, "$1");
}

/** `text` with every single- and double-quoted string literal emptied (template literals are kept,
 *  since their `${}` holes are code). */
const withoutStrings = (text: string): string =>
  text.replace(/"(?:[^"\\\n]|\\.)*"|'(?:[^'\\\n]|\\.)*'/g, (m) => m[0]! + m[0]!);

const matches = (text: string, re: RegExp): string[] => [...text.matchAll(re)].map((m) => m[0]);

describe("production code never installs a quiescence test view", () => {
  const files = sourceFiles(SRC);

  it("scans a non-trivial source tree that includes the owning module", () => {
    assert.ok(files.length > 20, `only ${files.length} source files found under ${SRC}`);
    assert.ok(files.includes(OWNER));
  });

  it("no other src file names the setter, the view type or its module state", () => {
    const offenders = files
      .filter((f) => f !== OWNER)
      .filter((f) => /\b(setQuiescenceViewForTests|QuiescenceView|testView)\b/.test(fs.readFileSync(f, "utf8")))
      .map((f) => path.relative(SRC, f));
    assert.deepEqual(offenders, []);
  });

  it("in run-quiescence.ts, the setter is only defined, never called", () => {
    const src = code(OWNER);
    assert.deepEqual(matches(src, /(export\s+function\s+)?\bsetQuiescenceViewForTests\s*\(/g), [
      "export function setQuiescenceViewForTests(",
    ]);
  });

  it("in run-quiescence.ts, the test-view state is written only inside the setter", () => {
    const src = code(OWNER);
    // The declaration carries no initializer: it starts undefined, and only the setter sets it.
    assert.deepEqual(matches(src, /\b(?:let|var|const)\s+testView\b[^;]*;/g), ["let testView: QuiescenceView | undefined;"]);
    // Plain `=` and the logical assignments `??=`, `||=`, `&&=` (never `==` / `===`), including one
    // after a type annotation (`testView: T = ...`), so an initialized declaration counts as a write.
    const writes = matches(src, /\btestView\b(?:\s*:[^=;]*)?\s*(?:\?\?|\|\||&&)?=(?!=)[^;]*;/g);
    assert.deepEqual(writes, ["testView = view === undefined ? undefined : structuredClone(view);"]);
    const setter = /export function setQuiescenceViewForTests\([^)]*\): void \{[\s\S]*?\n\}/.exec(src)?.[0] ?? "";
    assert.ok(setter.includes(writes[0]!), "the one write sits in the setter's body");
  });

  it("run-quiescence.ts never reads the environment, so no env var can supply or shape a view", () => {
    const src = code(OWNER);
    // `process.env`, `process["env"]`, and a destructured `{ env } = process`.
    assert.deepEqual(matches(src, /\bprocess\s*(?:\?\.|\.)\s*env\b|\bprocess\s*\[\s*["'`]env["'`]\s*\]/g), []);
    assert.deepEqual(matches(src, /\benv\b[^}=]*\}\s*=\s*process\b/g), []);
    // And, stated for the view path itself: the setter and the view validation mention no `process`.
    const setter = /export function setQuiescenceViewForTests\([^)]*\): void \{[\s\S]*?\n\}/.exec(src)?.[0] ?? "";
    const validation = /function isQuiescenceView\([^)]*\): v is QuiescenceView \{[\s\S]*?\n\}/.exec(src)?.[0] ?? "";
    assert.ok(setter.length > 0 && validation.length > 0, "found the setter and the view validation");
    assert.doesNotMatch(setter, /\bprocess\b/);
    assert.doesNotMatch(validation, /\bprocess\b/);
  });

  it("in run-quiescence.ts, `view` is added to a request only from the test-set state", () => {
    const src = code(OWNER);
    // Every `view:` / `view?:` in code: the ScanRequest field, two parameter annotations, and the
    // one object property, the helper's stdin request built from the test-set view.
    const sites = matches(src, /\S*[ \t]*\bview\??[ \t]*:[ \t]*[A-Za-z]+/g).map((s) => s.trim());
    assert.deepEqual(sites.sort(), [
      "...req, view: testView",
      "setQuiescenceViewForTests(view: QuiescenceView",
      "view?: QuiescenceView",
      "viewReapDeps(view: QuiescenceView",
    ]);
    assert.match(src, /JSON\.stringify\(testView === undefined \? req : \{ \.\.\.req, view: testView \}\)/);
    // No other way of attaching one: no `.view =` assignment, no computed "view" key, and no
    // shorthand `view` property (`{ ...req, view }`): a bare `view` followed by "," or "}" that is not
    // a member access or a spread (`o.view`, `...view`).
    assert.deepEqual(matches(src, /\.view\s*=(?!=)/g), []);
    assert.deepEqual(matches(src, /\[\s*["'`]view["'`]\s*\]/g), []);
    assert.deepEqual(matches(withoutStrings(src), /(?<![.\w$])view\s*[,}]/g), []);
  });
});
