import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { ChildProcess } from "node:child_process";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { helperRejectionReason } from "../src/run-procs.js";
import { runAgentHelper } from "../src/rmtree.js";
import { commandRootCommand, runnerCommand } from "../src/runner-uid.js";

/**
 * Issue #4089. Under an ambient MaintenanceBudget, runAgentHelper's timeout and cancel killed the
 * helper with `child.kill`. Under the runner-uid split the worker cannot signal the helper (EPERM),
 * and Node reports that refused kill by emitting 'error' synchronously from `kill`, whose handler
 * killed again without end. Here `child.kill` is mocked to refuse exactly that way; the helper is
 * a real hanging node process (single-uid: the wrappers pass through), so a real same-uid kill
 * is observable.
 */

type CommandWrapper = (command: string, args: readonly string[]) => { command: string; args: string[] };
type Mode = "timeout" | "abort";

const SCRIPT = "require('fs').writeFileSync(process.argv[1], String(process.pid)); setInterval(() => {}, 1000);";
const ABORT_REASON = "test abort reason (issue 4089)";
const plain: CommandWrapper = (command, args) => ({ command, args: [...args] });

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));
const alive = (pid: number): boolean => { try { process.kill(pid, 0); return true; } catch { return false; } };

async function until(cond: () => boolean, ms: number, what: string): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) assert.fail(`timed out waiting for ${what}`);
    await sleep(20);
  }
}

/** Node's own contract for a refused kill: 'error' emitted synchronously from `kill`, returns false. */
function refuseKill(t: TestContext): { calls: () => number } {
  const mock = t.mock.method(ChildProcess.prototype, "kill", function (this: ChildProcess) {
    this.emit("error", Object.assign(new Error("kill EPERM"), { code: "EPERM", errno: -1, syscall: "kill" }));
    return false;
  });
  return { calls: () => mock.mock.callCount() };
}

interface Launched {
  state: { settled: boolean; error: unknown; value: unknown };
  done: Promise<void>;
  pid: () => Promise<number>;
  /** Mode "abort": abort the ambient budget once the helper is up. Mode "timeout": nothing to do. */
  trigger: () => Promise<void>;
  /** Free the helper (test-side kill) so a joined run can close. */
  freeHelper: () => Promise<void>;
}

function launch(
  t: TestContext,
  wrap: CommandWrapper,
  mode: Mode,
  opts: Parameters<typeof runAgentHelper>[5],
): Launched {
  const dir = mkdtempSync(path.join(os.tmpdir(), "uzi-budget-refused-kill-"));
  const pidFile = path.join(dir, "pid");
  const ac = new AbortController();
  let helperPid: number | undefined;
  t.after(() => {
    // Only a positive pid: an empty or partly written pid file reads as 0, and kill(0) would hit
    // the test runner's own process group.
    let p = helperPid;
    if (p === undefined) { try { p = Number(readFileSync(pidFile, "utf8")); } catch { /* never started */ } }
    if (p !== undefined && Number.isInteger(p) && p > 0) { try { process.kill(p, "SIGKILL"); } catch { /* gone */ } }
    rmSync(dir, { recursive: true, force: true });
  });
  const state: Launched["state"] = { settled: false, error: undefined, value: undefined };
  // Long enough that a loaded host still writes the pid file before the timeout fires.
  const helperTimeout = mode === "timeout" ? 1500 : 10_000;
  const done = MaintenanceBudget.runProducer(
    () => runAgentHelper(wrap, SCRIPT, [pidFile], helperTimeout, "", opts),
    { durationMs: 600_000, signal: ac.signal },
  ).then(
    (value) => { state.settled = true; state.value = value; },
    (error: unknown) => { state.settled = true; state.error = error; },
  );
  const pid = async (): Promise<number> => {
    await until(() => { try { return readFileSync(pidFile, "utf8") !== ""; } catch { return false; } }, 8000, "the helper's pid file");
    const read = Number(readFileSync(pidFile, "utf8"));
    assert.ok(Number.isInteger(read) && read > 0, `a positive helper pid, got ${read}`);
    helperPid = read;
    return helperPid;
  };
  return {
    state,
    done,
    pid,
    trigger: async () => { if (mode === "abort") { await pid(); ac.abort(new Error(ABORT_REASON)); } },
    freeHelper: async () => { process.kill(await pid(), "SIGKILL"); },
  };
}

/** Settle within `ms` or fail: no unbounded wait. */
async function settledWithin(l: Launched, ms: number): Promise<void> {
  await until(() => l.state.settled, ms, "the helper run to settle");
  await l.done;
}

function assertOriginalReason(mode: Mode, error: unknown): void {
  assert.ok(error instanceof Error, String(error));
  assert.doesNotMatch(error.message, /EPERM/);
  if (mode === "timeout") assert.equal(helperRejectionReason(error), "helper_timeout", error.message);
  else assert.equal(error.message, ABORT_REASON);
}

const wrappers: Array<[string, CommandWrapper]> = [
  ["runnerCommand", runnerCommand],
  ["commandRootCommand", commandRootCommand],
];
const modes: Mode[] = ["timeout", "abort"];

describe("runAgentHelper under a budget, refused kill (issue #4089)", () => {
  for (const [name, wrap] of wrappers) {
    for (const mode of modes) {
      it(`${mode} with ${name}: the same-uid kill ends the helper, the original reason is reported, child.kill is never used`, async (t) => {
        const kill = refuseKill(t);
        const l = launch(t, wrap, mode, { uidSplit: true } as Parameters<typeof runAgentHelper>[5]);
        const pid = await l.pid();
        await l.trigger();
        await settledWithin(l, 8000);
        assertOriginalReason(mode, l.state.error);
        assert.equal(kill.calls(), 0, "child.kill is not called when a same-uid killer applies");
        await until(() => !alive(pid), 5000, "the helper to be gone");
      });
    }
  }

  for (const mode of modes) {
    it(`${mode}, budget only: an injected killer that fails leaves the helper running and the run joined`, async (t) => {
      const kill = refuseKill(t);
      const killed: number[] = [];
      const l = launch(t, plain, mode, { killAsHelperUid: (pid: number) => { killed.push(pid); return false; } });
      const pid = await l.pid();
      await l.trigger();
      await until(() => killed.length > 0, 5000, "the injected killer");
      await sleep(700);
      assert.deepEqual(killed, [pid]);
      assert.equal(kill.calls(), 0);
      assert.equal(l.state.settled, false, "still joined while the helper is alive");
      assert.ok(alive(pid));
      await l.freeHelper();
      await settledWithin(l, 8000);
      assertOriginalReason(mode, l.state.error);
    });

    it(`${mode}, no same-uid killer: one refused child.kill keeps the original reason and the run joined`, async (t) => {
      const kill = refuseKill(t);
      const l = launch(t, plain, mode, {});
      const pid = await l.pid();
      await l.trigger();
      await until(() => kill.calls() > 0, 5000, "child.kill");
      await sleep(700);
      assert.equal(kill.calls(), 1, "a refused kill is not retried from the error handler");
      assert.equal(l.state.settled, false, "still joined while the helper is alive");
      assert.ok(alive(pid));
      await l.freeHelper();
      await settledWithin(l, 8000);
      assertOriginalReason(mode, l.state.error);
    });

    it(`${mode} with joinOnTimeout and an injected killer: killed once through it, never through child.kill`, async (t) => {
      const kill = refuseKill(t);
      const killed: number[] = [];
      const l = launch(t, plain, mode, { joinOnTimeout: true, killAsHelperUid: (pid: number) => { killed.push(pid); return false; } });
      const pid = await l.pid();
      await l.trigger();
      await until(() => killed.length > 0, 5000, "the injected killer");
      await sleep(700);
      assert.deepEqual(killed, [pid]);
      assert.equal(kill.calls(), 0);
      assert.equal(l.state.settled, false, "pending until the helper closes");
      await l.freeHelper();
      await settledWithin(l, 8000);
      assertOriginalReason(mode, l.state.error);
    });
  }
});
