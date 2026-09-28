// issue #1783 — preloaded into every test file (package.json `test`: a second `--import`). It
// points the run-quiescence reaper at an EMPTY fake proc root, so no test's outcome depends on the
// host's live process table (a CI runner's setgid ssh-agent reads back unreadable and would make
// every proof `unverified`). A test that reaps real children opts into a scoped real view
// (`descendantsOf`), and a test planting fake processes into its own fake root; either restores
// this default with restoreHermeticView().

import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { setQuiescenceViewForTests, type QuiescenceView } from "../../src/run-quiescence.js";

const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hermetic-proc-"));
process.on("exit", () => fs.rmSync(root, { recursive: true, force: true }));

/** The default view: an empty fake proc root, so every reap is quiescent with nothing to kill. */
export const HERMETIC_VIEW: QuiescenceView = { procRoot: root };

/** Reinstall {@link HERMETIC_VIEW}. */
export function restoreHermeticView(): void {
  setQuiescenceViewForTests(HERMETIC_VIEW);
}

restoreHermeticView();
