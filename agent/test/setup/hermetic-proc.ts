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
// its own view. It also installs a process-free default environment probe (issue #1866, below).

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { setQuiescenceViewForTests, type QuiescenceView } from "../../src/run-quiescence.js";
import { setDefaultEnvProbeSpawnerForTests, type EnvProbeSpawner } from "../../src/env-probe.js";

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
