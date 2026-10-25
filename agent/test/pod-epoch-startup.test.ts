import assert from "node:assert/strict";
import fs from "node:fs";
import { test } from "node:test";

const source = fs.readFileSync(new URL("../src/main.ts", import.meta.url), "utf8");

// Bind to executable symbols within main, ignoring explanatory comments and imports.
function contract(text: string): void {
  const start = text.indexOf("async function main(): Promise<void> {");
  assert.notEqual(start, -1, "main boundary exists");
  const main = text.slice(start).replace(/\/\*[\s\S]*?\*\//g, "").replace(/\/\/[^\n]*/g, "");
  assert.match(main, /^async function main\(\): Promise<void> \{\s*recordMaintenanceBootstrap\(snapshotMaintenanceBootstrap\(\)\);/,
    "maintenance bootstrap is the first main statement");
  const at = (symbol: string): number => {
    const position = main.indexOf(symbol);
    assert.notEqual(position, -1, "required executable symbol: " + symbol);
    return position;
  };
  const acquire = at("await createPodEpoch().acquire(");
  assert.ok(at('process.on(sig,') < acquire, "signal cancellation precedes epoch acquisition");
  assert.ok(at('controller.abort()') < acquire, "early signal handler aborts lock wait");
  const consume = at("await epochOwnership.consume(");
  assert.ok(acquire < consume, "consume requires acquired epoch ownership");
  const restore = at("restoreEpochRuntimeTmpdir(epochOwnership)");
  assert.ok(consume < restore, "runtime TMPDIR restoration follows ownership and consumption");
  for (const symbol of ["loadConfig()", "ensureRunnerTmpdir(", "resolveDockerWiring(",
    "await outbox.init()", "git.inspectRetireScratchAtStartup(", "reclaimStrandedRunHomes(",
    "jobRunner.reapStaleWorkspaces()", "reapCodexOrphansAtStartup(", "startupReapVerdict(",
    "await worker.run("]) {
    assert.ok(restore < at(symbol), "runtime TMPDIR restored before " + symbol);
    assert.ok(consume < at(symbol), "epoch consumed before " + symbol);
  }
  const acquisition = main.slice(acquire, main.indexOf(": undefined", acquire));
  assert.match(acquisition, /signal: controller.signal/, "bounded acquisition receives early cancellation");
  assert.doesNotMatch(acquisition, /firstMode\s*:\s*["']exclusive["']/, "M1 never enables candidate EX");
  assert.ok(at("startupReapVerdict(") < at("await worker.run("), "startup settlement precedes admission");
}

test("pod epoch main startup orders ownership, consumption and runtime TMPDIR before writers and admission", () => {
  contract(source);
});

test("startup contract rejects omitted runtime TMPDIR restoration at its named assertion", () => {
  const call = "if (epochOwnership) restoreEpochRuntimeTmpdir(epochOwnership);";
  assert.ok(source.includes(call), "restore mutation applies to current candidate");
  assert.throws(() => contract(source.replace(call, "")),
    /required executable symbol: restoreEpochRuntimeTmpdir/, "omit restore must redden the restoration contract");
});
