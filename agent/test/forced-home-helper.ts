import childProcess from "node:child_process";
import { syncBuiltinESMExports } from "node:module";
import type { TestContext } from "node:test";

/** Force the HOME helper's completeness bit without consulting a busy host proc tree. Other
 *  worker helpers stay untouched. A real-operations control proves the incomplete result blocks. */
export function forceIncompleteHomeHelper(t: TestContext): { enabled: boolean; calls: number } {
  const state = { enabled: false, calls: 0 };
  const spawn = childProcess.spawn;
  const spy = t.mock.method(childProcess, "spawn", (command: string, args: readonly string[], options: childProcess.SpawnOptions) => {
    if (command === process.execPath && args[0] === "-e" && args[1]?.includes('const want = Buffer.from("HOME=" + home);')) {
      state.calls++;
      const summary = { scanned: 1, truncated: false, unresolved: state.enabled ? 1 : 0, procMount: "ok" };
      const output = "H\t" + JSON.stringify(summary) + "\n";
      return spawn(command, ["-e", `process.stdout.write(${JSON.stringify(output)})`], options);
    }
    return spawn(command, args, options);
  });
  syncBuiltinESMExports();
  t.after(() => {
    spy.mock.restore();
    syncBuiltinESMExports();
  });
  return state;
}
