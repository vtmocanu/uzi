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

  it("run-quiescence.ts has none of the env-read shapes this scans for (process.env, process-module imports, process aliases, other .env reads)", () => {
    const src = code(OWNER);
    // `process.env`, `process["env"]`, and a destructured `{ env } = process`.
    assert.deepEqual(matches(src, /\bprocess\s*(?:\?\.|\.)\s*env\b|\bprocess\s*\[\s*["'`]env["'`]\s*\]/g), []);
    assert.deepEqual(matches(src, /\benv\b[^}=]*\}\s*=\s*process\b/g), []);
    // No import of the process module at all (so no `env` binding by named import, alias, default
    // or namespace import): static `import … from`, `export … from`, a dynamic `import()` or a
    // `require()` of "process" / "node:process". The specifier is a string, so this reads `src`.
    assert.deepEqual(matches(src, /\b(?:import|export)\b[^;]*?["'`](?:node:)?process["'`]/g), []);
    assert.deepEqual(matches(src, /\brequire\s*\(\s*["'`](?:node:)?process["'`]\s*\)/g), []);
    const bare = withoutStrings(src);
    // No aliasing of the process object (`const p = process;`, `= globalThis.process`,
    // `= globalThis?.process`): a bare `process` on the right of `=`, not followed by a member access.
    assert.deepEqual(
      matches(bare, /=\s*(?:globalThis\s*(?:\?\.|\.)\s*)?process\b(?!\s*(?:\?\.|\.|\[))/g),
      [],
    );
    // No `env` member read on anything, whatever the receiver (an alias, `globalThis.process`, a
    // call result), by dot, optional chain or a computed "env" key. The one `.env` in code today is
    // the helper spawner handing its own `opts.env` (built by the caller from `workerSpawnEnv(...)`)
    // to `spawn`; a spawn option `env:` object key is not a member access and is not matched.
    assert.deepEqual(matches(bare, /[\w$)\]]*\s*(?:\?\.|\.)\s*env\b/g), ["opts.env"]);
    assert.match(bare, /const defaultHelperSpawn: HelperSpawn = \(command, args, opts\) =>\s*spawn\([^;]*\benv: opts\.env\b/);
    assert.deepEqual(matches(src, /\[\s*["'`]env["'`]\s*\]/g), []);
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

// issue #2213 — the residue quarantine latch has no production release (a container restart is the
// only one), so production code must never reset it. Only the test-only reset's own definition names
// it in agent/src; the hermetic preload resets it after every test.
describe("production code never resets the residue quarantine latch", () => {
  const OWNER_Q = path.join(SRC, "residue-quarantine.ts");
  const files = sourceFiles(SRC);

  it("scans the owning module", () => {
    assert.ok(files.includes(OWNER_Q));
  });

  it("no other src file names the reset", () => {
    const offenders = files
      .filter((f) => f !== OWNER_Q)
      .filter((f) => /\b(resetResidueQuarantineForTests)\b/.test(fs.readFileSync(f, "utf8")))
      .map((f) => path.relative(SRC, f));
    assert.deepEqual(offenders, []);
  });

  it("in residue-quarantine.ts, the reset is only defined, never called", () => {
    const src = code(OWNER_Q);
    assert.deepEqual(matches(src, /(export\s+function\s+)?\bresetResidueQuarantineForTests\s*\(/g), [
      "export function resetResidueQuarantineForTests(",
    ]);
  });

  it("in residue-quarantine.ts, the latch state is written only by the first-wins latch and the reset", () => {
    const src = code(OWNER_Q);
    const writes = matches(src, /\blatched\s*=(?!=)[^;]*;/g);
    assert.deepEqual(writes, [
      "latched = { cause, runId: input.runId, site, latchedAt: now.toISOString() };",
      "latched = undefined;",
    ]);
  });
});

// issue #2213 — every place agent/src starts a Claude provider turn through an injected `queryFn`
// carries the synchronous quarantine check as the statement right before the call, so a new call
// site cannot be added without it. (The Codex funnels and the git funnel are proven by behaviour in
// the residue-quarantine-*.test.ts files; the spawn belts are proven by their own unit tests.)
describe("every queryFn call site asserts the residue quarantine immediately before the call", () => {
  const files = sourceFiles(SRC);
  const CALL = /\bqueryFn\(\s*\{/;
  const sites = files.flatMap((f) => {
    const lines = code(f).split("\n");
    return lines.flatMap((line, i) => (CALL.test(line) ? [{ file: path.relative(SRC, f), line, before: lines.slice(Math.max(0, i - 3), i) }] : []));
  });

  it("finds the six known call sites", () => {
    assert.deepEqual(
      sites.map((s) => s.file).sort(),
      ["chat-executor.ts", "claude-advice-harness.ts", "claude-cross-check.ts", "claude-harness.ts", "isolated-executor.ts", "job-runner.ts"],
    );
  });

  it("each is preceded (within the three lines before) by assertResidueQuarantineOpen(\"provider_turn\")", () => {
    const missing = sites.filter((s) => !s.before.some((l) => l.includes('assertResidueQuarantineOpen("provider_turn")'))).map((s) => s.file);
    assert.deepEqual(missing, []);
  });
});
