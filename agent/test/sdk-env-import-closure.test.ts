// The worker image runs test/js-deps-pnpm-real.test.ts with NO node_modules beside it, so
// the src import closure of that test (and of sdk-env.ts) may load only `node:` builtins
// and relative modules. This walks that closure statically and names the chain to any
// bare specifier (for example a package such as zod).

import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const pnpmTest = path.join(here, "js-deps-pnpm-real.test.ts");
const sdkEnv = path.resolve(here, "..", "src", "sdk-env.ts");

/** The source with every block and line comment replaced by a space. String literals ('...', "...", `...`)
 *  are copied through whole, so a comment opener inside one starts no comment. A quoted string ends at its
 *  closing quote or at a newline; a template literal ends at its closing backtick. */
function stripComments(src: string): string {
  let out = "";
  let i = 0;
  while (i < src.length) {
    const c = src[i]!;
    const two = src.slice(i, i + 2);
    if (two === "/*") {
      const end = src.indexOf("*/", i + 2);
      i = end < 0 ? src.length : end + 2;
      out += " ";
    } else if (two === "//") {
      const end = src.indexOf("\n", i);
      i = end < 0 ? src.length : end;
      out += " ";
    } else if (c === "'" || c === '"' || c === "`") {
      let j = i + 1;
      while (j < src.length && src[j] !== c && !(c !== "`" && src[j] === "\n")) j += src[j] === "\\" ? 2 : 1;
      out += src.slice(i, j + 1);
      i = j + 1;
    } else {
      out += c;
      i++;
    }
  }
  return out;
}

/** Value specifiers loaded by a source text. Skips only `import type` / `export type`
 *  statements; `import { type X } from "x"` stays a load under verbatimModuleSyntax. */
function valueSpecifiers(src: string): string[] {
  const out: string[] = [];
  const stripped = stripComments(src);
  const stmt = /(?:^|[;\n}])\s*(import|export)\s+(type\s+)?([^;'"`]*?\bfrom\s*)?(["'])([^"'\n]+)\4/g;
  for (const m of stripped.matchAll(stmt)) {
    const kind = m[1];
    const isType = m[2] !== undefined;
    const hasFrom = m[3] !== undefined;
    if (isType) continue;
    // `import "x"` has no from clause; `export ...` needs one to be a load.
    if (kind === "export" && !hasFrom) continue;
    out.push(m[5] as string);
  }
  for (const m of stripped.matchAll(/\bimport\s*\(\s*(["'])([^"'\n]+)\1\s*\)/g)) out.push(m[2] as string);
  return out;
}

function resolveRelative(from: string, spec: string): string | undefined {
  const base = path.resolve(path.dirname(from), spec);
  const candidates = [
    base.replace(/\.js$/, ".ts"),
    base,
    `${base}.ts`,
    path.join(base, "index.ts"),
    path.join(base.replace(/\.js$/, ""), "index.ts"),
  ];
  return candidates.find((c) => c.endsWith(".ts") && fs.existsSync(c) && fs.statSync(c).isFile());
}

interface Walk {
  visited: Set<string>;
  violations: string[];
}

function walk(roots: string[]): Walk {
  const visited = new Set<string>();
  const violations: string[] = [];
  const parent = new Map<string, string>();
  const chain = (file: string): string => {
    const parts: string[] = [];
    for (let f: string | undefined = file; f !== undefined; f = parent.get(f)) parts.unshift(path.basename(f));
    return parts.join(" -> ");
  };
  const queue = [...roots];
  while (queue.length > 0) {
    const file = queue.shift() as string;
    if (visited.has(file)) continue;
    visited.add(file);
    for (const spec of valueSpecifiers(fs.readFileSync(file, "utf8"))) {
      if (spec.startsWith("node:")) continue;
      if (!spec.startsWith("./") && !spec.startsWith("../")) {
        violations.push(`${chain(file)} imports bare specifier "${spec}"`);
        continue;
      }
      const target = resolveRelative(file, spec);
      if (target === undefined) {
        violations.push(`${chain(file)} imports unresolvable "${spec}"`);
        continue;
      }
      if (!parent.has(target)) parent.set(target, file);
      queue.push(target);
    }
  }
  return { visited, violations };
}

function pnpmTestSrcRoots(): string[] {
  const text = fs.readFileSync(pnpmTest, "utf8");
  const roots: string[] = [];
  for (const spec of valueSpecifiers(text)) {
    if (!spec.startsWith("../src/")) continue;
    const t = resolveRelative(pnpmTest, spec);
    assert.ok(t !== undefined, `cannot resolve ${spec} from js-deps-pnpm-real.test.ts`);
    roots.push(t);
  }
  return roots;
}

describe("worker-image import closure", () => {
  it("sdk-env.ts and the pnpm proof test's src imports load only node: builtins and relative modules", () => {
    const roots = [sdkEnv, ...pnpmTestSrcRoots()];
    const { visited, violations } = walk(roots);
    assert.ok(visited.has(sdkEnv), "walk must visit sdk-env.ts");
    assert.ok(visited.size > 1, "walk must visit more than the root");
    assert.deepEqual(violations.slice(0, 5), [], "non-relative, non-node: imports in the closure");
  });

  it("the detector reports a bare specifier (self-check)", () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "closure-selfcheck-"));
    try {
      fs.writeFileSync(path.join(dir, "a.ts"), 'import { b } from "./b.js";\nexport const a = b;\n');
      fs.writeFileSync(path.join(dir, "b.ts"), 'import { type T } from "somepkg";\nexport const b = 1;\n');
      fs.writeFileSync(path.join(dir, "c.ts"), 'import type { T } from "typeonly";\nimport fs from "node:fs";\n');
      fs.writeFileSync(path.join(dir, "d.ts"), 'const open = "/*";\nimport x from "hidden-pkg";\nconst close = "*/";\nexport { open, close, x };\n');
      const hidden = walk([path.join(dir, "d.ts")]);
      assert.equal(hidden.violations.length, 1, "a comment opener inside a string literal hides no import");
      assert.match(hidden.violations[0] as string, /imports bare specifier "hidden-pkg"/);
      const bad = walk([path.join(dir, "a.ts")]);
      assert.equal(bad.violations.length, 1);
      assert.match(bad.violations[0] as string, /a\.ts -> b\.ts imports bare specifier "somepkg"/);
      const ok = walk([path.join(dir, "c.ts")]);
      assert.deepEqual(ok.violations, []);
    } finally {
      fs.rmSync(dir, { recursive: true, force: true });
    }
  });
});
