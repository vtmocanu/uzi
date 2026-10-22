// Helper for runner-shared-real.test.mts: runs the production ensureRunnerTmpdir under whatever
// uid this process was started as and prints the outcome as one JSON line. The environment it
// checks is passed explicitly so the process's own TMPDIR (the transpiler cache) stays elsewhere.
import { ensureRunnerTmpdir } from "../../agent/src/runner-tmpdir.js";

const spec = JSON.parse(process.argv[2]!) as { dataDir: string; tmpdir: string };
try {
  const result = ensureRunnerTmpdir({ dataDir: spec.dataDir, env: { TMPDIR: spec.tmpdir } });
  console.log(JSON.stringify({ ok: true, ...result, uid: process.getuid?.() }));
} catch (error) {
  console.log(JSON.stringify({ ok: false, name: (error as Error).name, message: (error as Error).message, uid: process.getuid?.() }));
}
