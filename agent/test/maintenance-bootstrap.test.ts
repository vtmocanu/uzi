import { test } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { maintenanceBootstrapRoots, readMaintenancePosture, recordMaintenanceBootstrap } from "../src/run-quiescence.js";

const agent = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

test("#3031 E3: the bootstrap set is recorded once, detached, and can never be re-enrolled", () => {
  assert.equal(maintenanceBootstrapRoots(), undefined, "unrecorded grants nothing");
  const entry = { pid: 30, parentPid: 10, startTime: 200, executable: "/image/esbuild" };
  const roots = [entry];
  recordMaintenanceBootstrap(roots);
  const expected = [{ pid: 30, parentPid: 10, startTime: 200, executable: "/image/esbuild" }];
  // Mutating the caller's array or entry after recording changes nothing.
  roots.push({ pid: 31, parentPid: 10, startTime: 201, executable: "/image/other" });
  entry.pid = 99; entry.executable = "/tmp/evil";
  assert.deepEqual(maintenanceBootstrapRoots(), expected);
  const saved = maintenanceBootstrapRoots()!;
  assert.ok(Object.isFrozen(saved) && saved.every(root => Object.isFrozen(root)));
  assert.throws(() => (saved as unknown as unknown[]).push({}));
  assert.throws(() => { (saved[0] as { pid: number }).pid = 1; });
  assert.throws(() => recordMaintenanceBootstrap([]), /already recorded/);
  assert.throws(() => recordMaintenanceBootstrap(undefined), /already recorded/);
  assert.deepEqual(maintenanceBootstrapRoots(), expected);
});

test("#3031 E1: main records the snapshot as its first statement and nowhere else", () => {
  const main = fs.readFileSync(path.join(agent, "src/main.ts"), "utf8");
  const body = main.slice(main.indexOf("async function main(): Promise<void> {") + "async function main(): Promise<void> {".length);
  const first = body.split("\n").map(line => line.trim()).find(line => line !== "" && !line.startsWith("//"));
  assert.equal(first, "recordMaintenanceBootstrap(snapshotMaintenanceBootstrap());");
  const callers = fs.readdirSync(path.join(agent, "src"), { recursive: true, encoding: "utf8" })
    .filter(file => file.endsWith(".ts"))
    .filter(file => /\b(recordMaintenanceBootstrap|snapshotMaintenanceBootstrap)\(/.test(
      fs.readFileSync(path.join(agent, "src", file), "utf8").replace(/export function \w+\(/g, "")));
  assert.deepEqual(callers, ["main.ts"]);
});

// knip's own TypeScript parser (oxc), resolved through the pinned knip devDependency so this
// test adds no dependency. It sees every runtime dynamic import, literal or computed.
type Node = { type?: string; callee?: { type?: string; name?: string } };
const oxc = createRequire(path.join(agent, "node_modules/knip/package.json"))("oxc-parser") as {
  parseSync(file: string, source: string, options: { lang: "ts" }): { program: object; errors: unknown[] };
};

/** Every runtime import() or require() call in the files. Type-only import() positions are
 *  TSImportType nodes, never ImportExpression, so they do not count. */
function runtimeDynamicImports(files: string[]): string[] {
  const found: string[] = [];
  for (const file of files) {
    const { program, errors } = oxc.parseSync(file, fs.readFileSync(file, "utf8"), { lang: "ts" });
    if (errors.length) { found.push(`${file}: unparsable`); continue; }
    const walk = (node: unknown): void => {
      if (!node || typeof node !== "object") return;
      if (Array.isArray(node)) { node.forEach(walk); return; }
      const n = node as Node;
      if (n.type === "ImportExpression" ||
          (n.type === "CallExpression" && n.callee?.type === "Identifier" && n.callee.name === "require"))
        found.push(`${file}: ${n.type}`);
      for (const value of Object.values(node)) walk(value);
    };
    walk(program);
  }
  return found;
}

test("#3031 E5: agent/src has no runtime dynamic import that could spawn an unenrolled esbuild", () => {
  const entryPoints = (fs.readdirSync(path.join(agent, "src"), { recursive: true, encoding: "utf8" }) as string[])
    .filter(file => file.endsWith(".ts") && !file.endsWith(".d.ts")).map(file => path.join(agent, "src", file));
  assert.ok(entryPoints.length > 50, "every source file is an entry point");
  assert.deepEqual(runtimeDynamicImports(entryPoints), []);
});

test("#3031 E5 detector catches literal and computed imports and ignores import types", () => {
  const dir = fs.mkdtempSync(path.resolve(agent, "../.uzi/scratch/e5-"));
  try {
    const probe = (name: string, source: string) => {
      const file = path.join(dir, name);
      fs.writeFileSync(file, source);
      return runtimeDynamicImports([file]).length;
    };
    fs.writeFileSync(path.join(dir, "target.ts"), "export const x = 1;\n");
    assert.equal(probe("literal.ts", 'export const f = () => import("./target.ts");\n'), 1);
    assert.equal(probe("computed.ts", "export const f = (p: string) => import(p);\n"), 1);
    assert.equal(probe("template.ts", "export const f = (p: string) => import(`./${p}`);\n"), 1);
    assert.equal(probe("require.ts", "declare const require: (p: string) => unknown;\nexport const f = (p: string) => require(p);\n"), 1);
    assert.equal(probe("types.ts", 'export type T = typeof import("./target.ts");\nexport const g: import("./target.ts").X | 0 = 0;\n'), 0);
  } finally { fs.rmSync(dir, { recursive: true, force: true }); }
});

test("#3031 E6: posture diagnostics parse bounded values and never guess", () => {
  const root = fs.mkdtempSync(path.resolve(agent, "../.uzi/scratch/posture-"));
  try {
    fs.mkdirSync(path.join(root, "sys/kernel/yama"), { recursive: true });
    fs.mkdirSync(path.join(root, "self"));
    fs.writeFileSync(path.join(root, "sys/kernel/yama/ptrace_scope"), "1\n");
    fs.writeFileSync(path.join(root, "self/status"), "Name:\tnode\nSeccomp:\t2\nCapEff:\t0000000000000000\n");
    assert.deepEqual({ ...readMaintenancePosture(root), uidSplit: false },
      { yamaPtraceScope: 1, seccomp: 2, capEff: "0000000000000000", uidSplit: false });
    fs.writeFileSync(path.join(root, "sys/kernel/yama/ptrace_scope"), "7\n");
    fs.writeFileSync(path.join(root, "self/status"), "Seccomp:\tfiltered\nCapEff:\tzz\n");
    assert.deepEqual({ ...readMaintenancePosture(root), uidSplit: false },
      { yamaPtraceScope: null, seccomp: null, capEff: null, uidSplit: false });
    fs.rmSync(path.join(root, "sys"), { recursive: true });
    assert.equal(readMaintenancePosture(root).yamaPtraceScope, null);
  } finally { fs.rmSync(root, { recursive: true, force: true }); }
});
