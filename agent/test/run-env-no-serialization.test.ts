// PRD #4416 M2 (W2b): run-env values live in memory for the run and are never persisted. Every module
// that writes a durable record (the outbox, the publication intent, the restore journal, recovery and
// hibernation state, the active-run registry, retained homes, terminal preservation) must not even
// name the run-env claim fields or modules, so no record it serialises can carry a value.
//
// Method, for every module: SOURCE SCAN. No serializer is driven with a sentinel claim; the scan
// asserts the modules do not name the run-env fields at all.
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";

const SRC = new URL("../src/", import.meta.url);
const PERSISTING = /^(outbox|publication-intent|restore-journal|recovery(-[a-z]+)?|hibernation-[a-z-]+|active-run-registry|retained-homes|terminal-[a-z-]+)\.ts$/;
const NAMES_RUN_ENV = /run_env|runEnv|RunEnv|run-env|RUN_ENV/;

const modules = fs.readdirSync(SRC).filter((f) => PERSISTING.test(f)).sort();

describe("durable-record modules never reference run-env values (source scan)", () => {
  it("covers every persisting module family", () => {
    for (const family of ["outbox", "publication-intent", "restore-journal", "recovery", "hibernation-", "active-run-registry", "retained-homes", "terminal-"]) {
      assert.ok(modules.some((m) => m.startsWith(family)), `no module found for ${family}`);
    }
  });
  for (const m of modules) {
    it(`${m} has no run_env reference`, () => {
      const text = fs.readFileSync(new URL(m, SRC), "utf8");
      assert.ok(!NAMES_RUN_ENV.test(text), `${m} names run_env/runEnv: a value could reach a durable record`);
    });
  }
});

describe("the claim's run-env fields stay in the modules that need them", () => {
  it("only the claim decoder, environment builders, prompt, forms and executors reference them", () => {
    const allowed = new Set([
      "agents.ts", "client.ts", "codex/codex-executor.ts", "codex/render.ts", "env-probe.ts", "executor.ts", "harness.ts",
      "prompt.ts", "protocol.ts", "run-env-claim.ts", "run-env-drop-event.ts", "run-env-forms.ts", "run-env-merge.ts",
      "run-env-names.ts", "run-env-prompt.ts", "run-kind.ts", "runner.ts", "sdk-env.ts", "sdk-executor.ts", "worker.ts",
    ]);
    const walk = (dir: URL, prefix: string): string[] =>
      fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) =>
        e.isDirectory() ? walk(new URL(`${e.name}/`, dir), `${prefix}${e.name}/`) : e.name.endsWith(".ts") ? [`${prefix}${e.name}`] : []);
    const naming = walk(SRC, "").filter((f) => NAMES_RUN_ENV.test(fs.readFileSync(new URL(f, SRC), "utf8")));
    const unexpected = naming.filter((f) => !allowed.has(f));
    assert.deepEqual(unexpected, [], "a new module names run_env: confirm it persists nothing, then list it here");
  });
});
