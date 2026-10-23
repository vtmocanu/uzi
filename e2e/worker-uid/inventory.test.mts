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
test("capture inventory requires the cancellation, trusted bare and sandbox proofs", () => {
  const captureFile = "agent/test/git-planning-diff.test.ts";
  const source = readFileSync(path.join(repo, captureFile), "utf8");
  const required = [
    "cancels during worktree comparisons and leaves no temp tree",
    "cancels while many tiny worktree entries are walked and leaves no temp tree",
    "reads loose trusted bare objects as the real runner uid without write authority",
    "reads packed trusted bare objects as the real runner uid without write authority",
    "captures under the enforced required command sandbox",
  ];
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/capture-inventory."));
  const api = new API({ cwd: scratch });
  try {
    const fixtures = [{ name: "actual", source, error: false }];
    for (let i = 0; i < required.length; i++) {
      const title = JSON.stringify(required[i]);
      assert.ok(source.includes(title));
      fixtures.push({ name: "renamed-" + i, source: source.replace(title, JSON.stringify(required[i] + " changed")), error: true });
      fixtures.push({ name: "nonliteral-" + i, source: source.replace(title, title + ' + ""'), error: true });
    }
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    for (const fixture of fixtures) {
      const collect = () => inventoryForSource(program.getSourceFile(path.join(scratch, fixture.name + ".mts")), "git-planning-diff");
      if (fixture.error) assert.throws(collect, /missing required literal capture leaf/, fixture.name);
      else {
        const leaves = collect();
        assert.equal(leaves.length, 12);
        for (const title of required) assert.ok(leaves.some((leaf: { names: string[] }) => leaf.names[1] === title), title);
      }
    }
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

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

const claudeFile = "e2e/worker-uid/claude-command-real.test.mts";
const claudeSuite = "real Claude guarded command lane";
const claudeTitles = [
  "stops a detached command and its descendants through a guard-style cancel and confirms the drain",
  "expires a short explicit deadline on a command with a detached descendant and drains the whole tree",
  "cancelling the selected run's command leaves a sibling run's command alive until it completes",
  "lets another attempt's capture scan skip the live command's recorded root while the owner's own reap still kills it",
  "rejects a candidate observation whose root start time differs from the enrolled one and cancels nothing",
  "never selects a larger unattributed process tree",
  "returns the exact worker_memory_pressure result as the selected command's tool error",
  "refuses a registration whose source is not one of the two model-command literals",
  "never enrolls unminted, cloned or foreign-context metadata, yet the command still runs",
  "gives a command that declares its own worker marks no protection and no special enrollment",
  "poisons the lane and kills the exact supervisor when a stopped supervisor cannot confirm disposal",
  "keeps native Bash when the guard is off",
];
test("claude command real inventory requires the twelve literal leaves under one UID gate", () => {
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/claude-real-inventory."));
  const api = new API({ cwd: scratch });
  const declarations = claudeTitles.map(name => `it(${JSON.stringify(name)}, async () => {});`);
  const wrap = (body: string, gate = true) =>
    `describe(${JSON.stringify(claudeSuite)}, ${gate ? "{ skip: INIT_SKIP }, " : ""}() => { ${body} });`;
  try {
    const fixtures: { name: string; source: string; error?: RegExp }[] = [
      { name: "literal", source: wrap(declarations.join("\n")) },
      { name: "actual", source: readFileSync(path.join(repo, claudeFile), "utf8") },
      { name: "duplicate", source: wrap([...declarations, declarations[0]!].join("\n")), error: /duplicate source leaf/ },
      { name: "nested", source: wrap(`describe("nested", () => { ${declarations.join("\n")} });`), error: /missing required literal Claude command proof/ },
      { name: "ungated", source: wrap(declarations.join("\n"), false), error: /expected one UID gate, found 0/ },
      { name: "double-gated", source: wrap(declarations.join("\n")) + wrap(""), error: /expected one UID gate, found 2/ },
      { name: "wrong-suite", source: declarations.join("\n").replace(/^/, `describe("other suite", { skip: INIT_SKIP }, () => {`) + "});", error: /missing required literal Claude command proof/ },
    ];
    for (let index = 0; index < declarations.length; index++) {
      for (const mutation of ["missing", "renamed", "nonliteral"] as const) {
        const changed = [...declarations];
        if (mutation === "missing") changed.splice(index, 1);
        else changed[index] = `it(${JSON.stringify(claudeTitles[index]! + (mutation === "renamed" ? " changed" : ""))}${mutation === "nonliteral" ? ' + ""' : ""}, async () => {});`;
        fixtures.push({ name: `${mutation}-${index}`, source: wrap(changed.join("\n")),
          error: mutation === "nonliteral" ? /nonliteral targeted/ : /missing required literal Claude command proof/ });
      }
    }
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    const wanted = claudeTitles.map(name => ({ file: claudeFile, names: [claudeSuite, name] }));
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".mts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, fixture.name);
      const collect = () => inventoryForSource(program.getSourceFile(filename), "claude-command-real");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.deepEqual(collect(), wanted, fixture.name);
    }
    const actual = JSON.parse(execFileSync(process.execPath,
      [path.join(repo, "e2e/worker-uid/inventory.mjs")], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual(actual.filter((item: { file: string }) => item.file === claudeFile), wanted);
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

const singleFile = "e2e/worker-uid/single-uid-preflight.test.mts";
const singleTitle = "single-uid launcher preflight matches the container posture";
test("single-uid inventory requires the one literal top-level preflight leaf", () => {
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/single-inventory."));
  const api = new API({ cwd: scratch });
  const declaration = `test(${JSON.stringify(singleTitle)}, { timeout: 60000 }, async () => {});`;
  try {
    const fixtures: { name: string; source: string; error?: RegExp }[] = [
      { name: "literal", source: declaration },
      { name: "actual", source: readFileSync(path.join(repo, singleFile), "utf8") },
      { name: "missing", source: "", error: /missing required literal single-uid proof/ },
      { name: "changed", source: declaration.replace("launcher", "changed"), error: /missing required literal single-uid proof/ },
      { name: "nonliteral", source: declaration.replace(JSON.stringify(singleTitle), JSON.stringify(singleTitle) + ' + ""'), error: /nonliteral targeted/ },
      { name: "duplicate", source: declaration + declaration, error: /duplicate source leaf/ },
      { name: "nested", source: `describe("nested", () => { ${declaration} });`, error: /missing required literal single-uid proof/ },
    ];
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    const wanted = [{ file: singleFile, names: [singleTitle] }];
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".mts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, fixture.name);
      const collect = () => inventoryForSource(program.getSourceFile(filename), "single-uid-preflight");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.deepEqual(collect(), wanted, fixture.name);
    }
    const run = (lane: string) => JSON.parse(execFileSync(process.execPath,
      [path.join(repo, "e2e/worker-uid/inventory.mjs"), lane], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual(run("single"), wanted);
    // The single-uid leaf stays out of the other two lanes.
    assert.equal(run("command").some((item: { file: string }) => item.file === singleFile), false);
    assert.equal(run("worker").some((item: { file: string }) => item.file === singleFile), false);
    assert.throws(() => execFileSync(process.execPath, [path.join(repo, "e2e/worker-uid/inventory.mjs"), "bogus"],
      { cwd: repo, stdio: "pipe" }), /unknown lane/);
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});

const sharedFile = "e2e/worker-uid/runner-shared-real.test.mts";
const sharedTitles = [
  "runner-init refuses a missing, wrong or symlinked layout marker before any write",
  "runner-init as root builds worker:runner 3775 with a worker:worker 0755 .tmp and leaves old and sibling data untouched",
  "runner-init refuses a hostile runner-shared or .tmp symlink or file without following or re-owning anything",
  "runner-init as the worker uid requires ownership by the current uid and fails closed on anything else",
  "the runner, runner-cmd and dind uids cannot replace .tmp or runner-shared entries, and their groups exclude 10001",
  "a fresh .tmp is created by the entrypoint and by ensureRunnerTmpdir in both uid modes, and a planted symlink or file is refused",
  "per-uid scratch under a live writer: a mismatched owner is refused untouched, an expected owner is adopted with writer exclusion not established",
  "old-layout and new-layout entrypoints overlap on one data directory without re-owning, wiping or following into each other's tree",
  "foreign-owned run-directory content is retained, counted and logged by the production retention delete in split and single-uid modes",
];
test("runner-shared inventory requires the nine literal top-level real-uid leaves", () => {
  mkdirSync(path.join(repo, ".uzi/scratch"), { recursive: true });
  const scratch = mkdtempSync(path.join(repo, ".uzi/scratch/shared-inventory."));
  const api = new API({ cwd: scratch });
  const declarations = sharedTitles.map(name => `test(${JSON.stringify(name)}, { timeout: 100000 }, async () => {});`);
  try {
    const fixtures: { name: string; source: string; error?: RegExp }[] = [
      { name: "literal", source: declarations.join("\n") },
      { name: "actual", source: readFileSync(path.join(repo, sharedFile), "utf8") },
      { name: "duplicate", source: [...declarations, declarations[0]!].join("\n"), error: /duplicate source leaf/ },
      { name: "nested", source: `describe("nested", () => { ${declarations.join("\n")} });`, error: /missing required literal runner-shared proof/ },
    ];
    for (let index = 0; index < declarations.length; index++) {
      for (const mutation of ["missing", "renamed", "nonliteral"] as const) {
        const changed = [...declarations];
        if (mutation === "missing") changed.splice(index, 1);
        else changed[index] = `test(${JSON.stringify(sharedTitles[index]! + (mutation === "renamed" ? " changed" : ""))}${mutation === "nonliteral" ? ' + ""' : ""}, { timeout: 100000 }, async () => {});`;
        fixtures.push({ name: `${mutation}-${index}`, source: changed.join("\n"),
          error: mutation === "nonliteral" ? /nonliteral targeted/ : /missing required literal runner-shared proof/ });
      }
    }
    writeFileSync(path.join(scratch, "tsconfig.json"), JSON.stringify({ files: fixtures.map(f => f.name + ".mts") }));
    for (const fixture of fixtures) writeFileSync(path.join(scratch, fixture.name + ".mts"), fixture.source);
    const program = api.updateSnapshot({ openProjects: [path.join(scratch, "tsconfig.json")] }).getProjects()[0].program;
    const wanted = sharedTitles.map(name => ({ file: sharedFile, names: [name] }));
    for (const fixture of fixtures) {
      const filename = path.join(scratch, fixture.name + ".mts");
      assert.equal(program.getSyntacticDiagnostics(filename).length, 0, fixture.name);
      const collect = () => inventoryForSource(program.getSourceFile(filename), "runner-shared-real");
      if (fixture.error) assert.throws(collect, fixture.error, fixture.name);
      else assert.deepEqual(collect(), wanted, fixture.name);
    }
    const run = (lane: string) => JSON.parse(execFileSync(process.execPath,
      [path.join(repo, "e2e/worker-uid/inventory.mjs"), lane], { cwd: repo, encoding: "utf8" }));
    assert.deepEqual(run("shared"), wanted);
    // The root-started leaves stay out of the other lanes.
    for (const lane of ["worker", "command", "single"]) {
      assert.equal(run(lane).some((item: { file: string }) => item.file === sharedFile), false, lane);
    }
  } finally {
    api.close();
    rmSync(scratch, { recursive: true, force: true });
  }
});
