import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";
import { it } from "node:test";
import { fileURLToPath } from "node:url";
import { API } from "typescript/unstable/sync";

const repo = fileURLToPath(new URL("../../", import.meta.url));
const harness = path.join(repo, "e2e/worker-uid");
const suite = "Unit2 runner source capture";
const file = "agent/test/git-planning-diff.test.ts";
const leaves = [
  "cancels while the object-store snapshot streams and leaves no temp tree",
  "cancels while many tiny object files are copied and leaves no temp tree",
  "never reads a symlink target's content",
  "reads each symlink only through its still-open pinned parent descriptor",
  "refuses a leaf swapped to a regular file before readlink without resolving the pathname",
  "refuses leaf and parent symlink escapes, nonregular ignores and an unavailable base",
  "captures an unchanged tracked symlink tree as an empty patch",
  "captures edits elsewhere next to unchanged tracked symlinks",
  "treats a symlinked .gitignore as absent while the path stays a candidate",
];
type Identity = { file: string; names: string[] };
const expected: Identity[] = leaves.map(leaf => ({ file, names: [suite, leaf] }));
const declarations = leaves.map(leaf => `it(${JSON.stringify(leaf)}, () => {});`);
const wrap = (body: string, title = JSON.stringify(suite)) => `describe(${title}, () => { ${body} });`;

it("worker UID inventory requires exactly nine literal capture leaves and preserves UID gates", async () => {
  const { inventoryForSource } = await import(new URL("../../e2e/worker-uid/inventory.mjs", import.meta.url).href);
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/worker-uid-inventory."));
  const api = new API({ cwd: scratch });
  try {
    const fixtures: { name: string; source: string; error?: RegExp; uid?: boolean }[] = [
      { name: "exact", source: wrap(declarations.join("\n") + `
        it("captures under the enforced required command sandbox", () => {});
        for (const scenario of ["one", "two"]) it("generated " + scenario, () => {});
        it(` + "`generated ${scenario}`" + `, () => {});
      `) + wrap(declarations.join("\n"), JSON.stringify("Unit2 bounded runner stdout transport")) },
      { name: "real", source: readFileSync(path.join(repo, file), "utf8") },
      { name: "missing-suite", source: declarations.join("\n"), error: /expected exactly one literal/ },
      { name: "changed-suite", source: wrap(declarations.join("\n"), JSON.stringify(suite + " changed")), error: /expected exactly one literal/ },
      { name: "nonliteral-suite", source: wrap(declarations.join("\n"), JSON.stringify(suite) + ' + ""'), error: /expected exactly one literal/ },
      { name: "duplicate-suite", source: wrap(declarations.join("\n")) + wrap(""), error: /expected exactly one literal/ },
      { name: "duplicate-leaf", source: wrap([...declarations, declarations[0]].join("\n")), error: /duplicate source leaf identity/ },
      { name: "nested-only", source: wrap(wrap(declarations.join("\n"), '"nested"')), error: /missing required literal capture leaf/ },
      { name: "nonliteral-nested", source: wrap(wrap(declarations.join("\n"), '"nested" + ""')), error: /missing required literal capture leaf/ },
      { name: "uid", uid: true, source: 'describe("UID suite", { skip: INIT_SKIP }, () => { it("UID leaf", () => {}); });' },
      { name: "uid-nonliteral", uid: true, source: 'describe("UID suite", { skip: INIT_SKIP }, () => { it("UID" + " leaf", () => {}); });', error: /nonliteral targeted/ },
      { name: "uid-duplicate", uid: true, source: 'describe("UID suite", { skip: INIT_SKIP }, () => { it("UID leaf", () => {}); it("UID leaf", () => {}); });', error: /duplicate source leaf identity/ },
    ];
    for (let index = 0; index < leaves.length; index++) {
      for (const mutation of ["missing", "changed", "nonliteral"]) {
        const changed = [...declarations];
        if (mutation === "missing") changed.splice(index, 1);
        else changed[index] = `it(${JSON.stringify(leaves[index] + (mutation === "changed" ? " changed" : ""))}${mutation === "nonliteral" ? ' + ""' : ""}, () => {});`;
        fixtures.push({ name: `${mutation}-${index}`, source: wrap(changed.join("\n")), error: /missing required literal capture leaf/ });
      }
    }
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(fixture => fixture.name + ".ts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".ts"), fixture.source);
    const snapshot = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] });
    const program = snapshot.getProjects()[0]!.program;
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".ts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, fixture.name);
      const source = program.getSourceFile(filename);
      assert.ok(source, fixture.name);
      const collect = () => inventoryForSource(source, fixture.uid ? "codex-executor" : "git-planning-diff");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else if (fixture.uid) assert.deepEqual(collect(), [{ file: "agent/test/codex-executor.test.ts", names: ["UID suite", "UID leaf"] }]);
      else assert.deepEqual(collect().sort((a: Identity, b: Identity) => a.names[1]!.localeCompare(b.names[1]!)),
        [...expected].sort((a, b) => a.names[1]!.localeCompare(b.names[1]!)), fixture.name);
    }
    // Exercise CLI wiring once; all title mutations above share the small parser project.
    const inventory: Identity[] = JSON.parse(execFileSync(process.execPath, [path.join(harness, "inventory.mjs")], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual([...new Set(inventory.map(test => test.file))], [
      "agent/test/codex-launcher.test.ts", "agent/test/codex-executor.test.ts",
      "agent/test/codex-shared-dir.test.ts", "agent/test/entrypoint-migration.test.ts", file,
    ]);
    assert.deepEqual(inventory.filter(test => test.file === file).map(test => test.names[1]).sort(), [...leaves].sort());
    const inventoryFile = path.join(scratch, "expected.json");
    writeFileSync(inventoryFile, JSON.stringify(inventory));
    const pattern = new RegExp(execFileSync(process.execPath, [path.join(harness, "pattern.mjs"), inventoryFile], { cwd: repo, encoding: "utf8" }));
    for (const leaf of leaves) {
      assert.ok(pattern.test(`${suite} ${leaf}`), leaf);
      assert.equal(pattern.test(`prefix ${suite} ${leaf}`), false);
      assert.equal(pattern.test(`${suite} ${leaf} suffix`), false);
      assert.equal(pattern.test(`Unit2 bounded runner stdout transport ${leaf}`), false);
    }
    for (const leaf of ["captures under the enforced required command sandbox", "generated one",
      leaves[8]!.replace(".gitignore", "Xgitignore")]) {
      assert.equal(pattern.test(`${suite} ${leaf}`), false, leaf);
    }
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});
