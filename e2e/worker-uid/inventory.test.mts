import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import path from "node:path";
import { test } from "node:test";
import { fileURLToPath, pathToFileURL } from "node:url";

const repo = fileURLToPath(new URL("../../", import.meta.url));
const require = createRequire(path.join(repo, "agent/package.json"));
const { API } = await import(pathToFileURL(require.resolve("typescript/unstable/sync")).href);
const { inventoryForSource } = await import(new URL("./inventory.mjs", import.meta.url).href);
const title = "real adopted detached tree: eligibility, identity refusal, concurrent cleanup and exact pressure failure";
const realTitle = "real supervisor observation fixtures execute under the command UID";
const file = "e2e/worker-uid/memory-command-real.test.mts";

test("command inventory requires explicit literal test declarations from the scoped project", () => {
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/command-inventory."));
  const api = new API({ cwd: scratch });
  try {
    const declaration = [title, realTitle].map(name =>
      `test(${JSON.stringify(name)}, { timeout: 110000 }, async () => {});`).join("\n");
    const fixtures = [
      { name: "literal", source: declaration },
      { name: "actual", source: readFileSync(path.join(repo, file), "utf8") },
      { name: "missing", source: "", error: /missing required literal/ },
      { name: "missing-real", source: declaration.split("\n")[0]!, error: /missing required literal/ },
      { name: "missing-memory", source: declaration.split("\n")[1]!, error: /missing required literal/ },
      { name: "changed", source: declaration.replace("real adopted", "changed adopted"), error: /missing required literal/ },
      { name: "nonliteral", source: declaration.replace(JSON.stringify(title), JSON.stringify(title) + ' + ""'), error: /nonliteral targeted/ },
      { name: "duplicate", source: declaration + declaration, error: /duplicate source leaf/ },
      { name: "nested", source: `describe("nested", () => { ${declaration} });`, error: /missing required literal/ },
    ];
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".mts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0);
      const collect = () => inventoryForSource(program.getSourceFile(filename), "memory-command-real");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.deepEqual(collect(), [title, realTitle].map(name => ({ file, names: [name] })));
    }
    const expected = JSON.parse(execFileSync(process.execPath,
      [path.join(repo, "e2e/worker-uid/inventory.mjs"), "command"], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual(expected, [title, realTitle].map(name => ({ file, names: [name] })));
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

test("fixed reaper inventory pins all literal leaves under exactly one UID gate", () => {
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/reaper-inventory."));
  const api = new API({ cwd: scratch });
  try {
    const source = readFileSync(path.join(repo, "agent/test/codex-capture-reaper-real.test.ts"), "utf8");
    const fixtures = [
      { name: "actual", source },
      { name: "missing", source: source.replace("fixed capture reaper: descendant settlement precedes verdict", "changed"), error: /required literal fixed reaper/ },
      { name: "missing-own", source: source.replace("fixed capture reaper: own mode kills owned residue before capture proceeds", "changed"), error: /required literal fixed reaper/ },
      { name: "missing-report", source: source.replace("fixed capture reaper: genuine report preserves the same owned writer", "changed"), error: /required literal fixed reaper/ },
      { name: "ungated", source: source.replace("skip: GROUP_B_SKIP", "skip: false"), error: /required literal fixed reaper/ },
      { name: "duplicate-gate", source: source + '\ndescribe("extra", { skip: GROUP_B_SKIP }, () => {});', error: /required literal fixed reaper/ },
    ];
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    for (const fixture of fixtures) {
      const collect = () => inventoryForSource(program.getSourceFile(path.join(scratch, fixture.name + ".mts")), "codex-capture-reaper-real");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.equal(collect().length, 4);
    }
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

const retainedTitles = [
  "recovers retained command-owned loose objects",
  "recovers retained command-owned packed objects",
  "normalizes before verifying an already captured restore tip",
  "preserves worker-owned hardlinks while recovering command-owned loose objects",
  "preserves worker-owned hardlinks while recovering command-owned packed objects",
  "preserves runner-owned hardlinks while recovering command-owned loose objects",
  "preserves runner-owned hardlinks while recovering command-owned packed objects",
  "refuses command-owned multiply linked mutation candidates and keeps custody",
  "keeps source and custody when retained object recovery fails"
];
test("retained recovery inventory requires every literal real-UID permission proof", () => {
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/retained-inventory."));
  const api = new API({ cwd: scratch });
  const retainedFile = "agent/test/git-retained-permissions.test.ts";
  const suite = "retained recovery object permissions across real UIDs";
  const declarations = retainedTitles.map(name => `it(${JSON.stringify(name)}, async () => {});`);
  const wrap = (body: string, gate = true) =>
    `describe(${JSON.stringify(suite)}, ${gate ? "{ skip: INIT_SKIP }, " : ""}() => { ${body} });`;
  try {
    const fixtures: { name: string; source: string; error?: RegExp }[] = [
      { name: "literal", source: wrap(declarations.join("\n")) },
      { name: "actual", source: readFileSync(path.join(repo, retainedFile), "utf8") },
      { name: "duplicate", source: wrap([...declarations, declarations[0]!].join("\n")), error: /duplicate source leaf/ },
      { name: "nested", source: wrap('describe("nested", () => {' + declarations.join("\n") + '});'), error: /missing required literal retained recovery/ },
      { name: "ungated", source: wrap(declarations.join("\n"), false), error: /expected one UID gate/ },
    ];
    for (let index = 0; index < declarations.length; index++) {
      for (const mutation of ["missing", "renamed", "nonliteral"] as const) {
        const changed = [...declarations];
        if (mutation === "missing") changed.splice(index, 1);
        else changed[index] = `it(${JSON.stringify(retainedTitles[index]! + (mutation === "renamed" ? " changed" : ""))}${mutation === "nonliteral" ? ' + ""' : ""}, async () => {});`;
        fixtures.push({ name: `${mutation}-${index}`, source: wrap(changed.join("\n")),
          error: mutation === "nonliteral" ? /nonliteral targeted/ : /missing required literal retained recovery/ });
      }
    }
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    const wanted = retainedTitles.map(name => ({ file: retainedFile, names: [suite, name] }));
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".mts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, fixture.name);
      const collect = () => inventoryForSource(program.getSourceFile(filename), "git-retained-permissions");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.deepEqual(collect(), wanted, fixture.name);
    }
    const actual = JSON.parse(execFileSync(process.execPath,
      [path.join(repo, "e2e/worker-uid/inventory.mjs")], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual(actual.filter((item: { file: string }) => item.file === retainedFile), wanted);
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});
