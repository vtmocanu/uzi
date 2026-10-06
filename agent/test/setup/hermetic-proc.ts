// issue #1783 — preloaded into every test file (package.json `test`: a second `--import`). It
// points the run-quiescence reaper at an EMPTY fake proc root, so no test's outcome depends on the
// host's live process table (a CI runner's setgid ssh-agent reads back unreadable and would make
// every proof `unverified`). A test that reaps real children opts into a scoped real view
// (`descendantsOf`), and a test planting fake processes into its own fake root; either restores
// this default with restoreHermeticView().
//
// test/helpers.ts, test/runner-harness.ts and test/fake-proc.ts import this module too, so a bare
// single-file run without the preload is hermetic as well. ES modules evaluate once per process
// (the preload's `./test/setup/hermetic-proc.ts` and a test's `./setup/hermetic-proc.js` resolve to
// the same module), so the default is installed once, at load, before any test body can install
// its own view. It also installs process-free SDK HOME-attribution operations (#2230) and a
// process-free default environment probe (issue #1866). Real HOME-helper subjects call the direct
// run-procs functions or explicitly supply SdkExecutorOptions.runProcesses; those stay real.

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { setQuiescenceViewForTests, type QuiescenceView } from "../../src/run-quiescence.js";
import { setDefaultEnvProbeSpawnerForTests, type EnvProbeSpawner } from "../../src/env-probe.js";
import { setDefaultRunProcessOpsForTests, type RunProcessOps } from "../../src/run-procs.js";
import { afterEach } from "node:test";
import { resetResidueQuarantineForTests } from "../../src/residue-quarantine.js";

const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hermetic-proc-"));
process.on("exit", () => fs.rmSync(root, { recursive: true, force: true }));

/** The default view: an empty fake proc root, so every reap is quiescent with nothing to kill. */
export const HERMETIC_VIEW: QuiescenceView = { procRoot: root };

/** Reinstall {@link HERMETIC_VIEW}. */
export function restoreHermeticView(): void {
  setQuiescenceViewForTests(HERMETIC_VIEW);
}

restoreHermeticView();

/** issue #1866 M2: the default run-start environment probe for executor rigs that inject none: it
 *  starts no process and reports every fact `ok`, so a test's facts (and so its prompts) never
 *  depend on the host. A test that drives the probe injects its own spawner
 *  (SdkExecutorOptions.envProbeSpawner) or calls spawnRunnerProbe directly. */
const HERMETIC_ENV_PROBE: EnvProbeSpawner = async () => ({
  code: 0,
  stdout: `${JSON.stringify({ uzi_envprobe: 1, proc: "ok", home: "ok", tmp: "ok" })}\n`,
  cleanedUp: true,
});

/** Reinstall the process-free default environment probe (after a test cleared or replaced it). */
export function restoreHermeticEnvProbe(): void {
  setDefaultEnvProbeSpawnerForTests(() => HERMETIC_ENV_PROBE);
}

restoreHermeticEnvProbe();

/** Scripted SDK queries start no agent processes. Explicit per-executor operations still win. */
const HERMETIC_RUN_PROCESS_OPS: RunProcessOps = {
  scan: async () => ({ pids: [], complete: true }),
  reap: async () => ({ killed: [], left: [], complete: true }),
};

/** Restore the shared test default without changing the direct HOME scan/reap functions. */
export function restoreHermeticRunProcessOps(): void {
  setDefaultRunProcessOpsForTests(HERMETIC_RUN_PROCESS_OPS);
}

restoreHermeticRunProcessOps();

/**
 * issue #2213: the residue quarantine latch is process-wide state with no production release, so a
 * test that latches it (directly, or through a runner flow over a planted unreadable process) must
 * not leak the latch into the next test of the same file. Call this once at the top level of a test
 * file: it resets the latch after every test. Deliberately a function, not a preload side effect:
 * registering a node:test hook at import time would make every child script that imports a test
 * helper start the test runner and print its summary to stdout.
 */
export function resetResidueQuarantineAfterEach(): void {
  afterEach(() => {
    resetResidueQuarantineForTests();
  });
}
