import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { describe, it } from "node:test";
import { fileURLToPath } from "node:url";

// issue #4066 - main.ts cannot be imported without booting a worker, and the TypeScript compiler API is not available
// in this toolchain, so the wiring of the shutdown barrier is pinned structurally: every graceful exit of mainBody
// runs the barrier before its stop log, the deadline observer is armed by the first signal, and the one deliberate
// hard exit stays barrier-free. Behaviour is proven by test/authority-crash-proofs.test.ts; this file only fails
// when someone removes or reorders the calls.

const here = path.dirname(fileURLToPath(import.meta.url));
const source = fs.readFileSync(path.join(here, "..", "src", "main.ts"), "utf8");

const start = source.indexOf("async function mainBody()");
const end = source.indexOf("// PRD #1429 M6: only auto-run `main()`");
assert.ok(start > 0 && end > start, "mainBody is located");
const mainBody = source.slice(start, end);
const afterAuthority = mainBody.slice(mainBody.indexOf("new AuthorityRuntime("));

/** The text of the first brace-balanced block opening at or after `from`. */
function blockAt(text: string, from: number): string {
  const open = text.indexOf("{", from);
  let depth = 0;
  for (let i = open; i < text.length; i += 1) {
    if (text[i] === "{") depth += 1;
    else if (text[i] === "}" && --depth === 0) return text.slice(open, i + 1);
  }
  throw new Error("unbalanced block");
}

describe("issue #4066: main.ts runs the shutdown barrier", () => {
  it("has exactly three awaited barrier calls, one per graceful exit, each before its stop log", () => {
    const calls = [...afterAuthority.matchAll(/await finishShutdown\(\)/g)];
    assert.equal(calls.length, 3, "startup-reap shutdown verdict, signal before the worker started, and after worker.run");
    for (const call of calls) {
      const rest = afterAuthority.slice(call.index);
      const log = rest.indexOf("log.info(");
      assert.ok(log > 0, "a stop log follows");
      const logText = rest.slice(log, log + 400);
      assert.match(logText, /exit_code: gate\.exitCode/, "the stop log reports the gate's exit code");
      assert.match(logText, /shutdown_result: gate\.result/, "and its result");
    }
    assert.ok(!/exit_code: 0/.test(afterAuthority), "no stop log hardcodes a clean exit code any more");
  });

  it("the barrier call passes the authority runtime, the git cache, the signal-fixed deadline and the logger", () => {
    const wrapper = afterAuthority.slice(afterAuthority.indexOf("const finishShutdown"));
    const body = blockAt(wrapper, 0);
    assert.equal([...body.matchAll(/finishWorkerShutdown\(/g)].length, 1);
    assert.match(body, /finishWorkerShutdown\(\{ authority, git, deadlineAt, log \}\)/);
    assert.match(body, /armShutdownObserver\(\)/, "no signal: the deadline starts (and is observed) at the barrier");
  });

  it("has no bare return after the authority runtime exists that skips the barrier", () => {
    for (const bare of afterAuthority.matchAll(/\breturn;/g)) {
      const before = afterAuthority.slice(Math.max(0, bare.index - 700), bare.index);
      assert.match(before, /await finishShutdown\(\)/, "every return; is preceded by the barrier");
    }
  });

  it("arms the deadline observer on the first signal and a repeated signal resets nothing", () => {
    const handler = blockAt(mainBody, mainBody.indexOf("process.on(sig"));
    assert.match(handler, /!shutdownBarrierStarted\) armShutdownObserver\(\)/);
    assert.ok(handler.indexOf("armShutdownObserver(") < handler.indexOf("shutdownRuntime?.()"), "armed before the runtime is told to shut down");
    const arm = blockAt(mainBody, mainBody.indexOf("const armShutdownObserver"));
    assert.match(arm, /shutdownDeadlineAt \?\?= Date\.now\(\) \+ SHUTDOWN_RESULT_DEADLINE_MS/);
    assert.match(arm, /armShutdownResultDeadline\(/);
    assert.match(source, /const SHUTDOWN_RESULT_DEADLINE_MS = 35_000;/);
  });

  it("arms the observer inside finishShutdown so every barrier entry (worker_returned included) is observed", () => {
    const finish = blockAt(mainBody, mainBody.indexOf("const finishShutdown"));
    assert.match(finish, /armShutdownObserver\(\)/);
    assert.ok(finish.indexOf("armShutdownObserver(") < finish.indexOf("finishWorkerShutdown("), "armed before the barrier runs");
  });

  it("tells the authority runtime the budget when the runtime shuts down", () => {
    const runtime = blockAt(mainBody, mainBody.indexOf("shutdownRuntime = () =>"));
    assert.match(runtime, /authority\.beginShutdown\(shutdownDeadlineAt\)/);
  });

  it("keeps the startup-reap incomplete exit barrier-free: it is an explicit process.exit(1)", () => {
    const exit = blockAt(mainBody, mainBody.indexOf('if (verdict === "exit")'));
    assert.match(exit, /process\.exit\(1\)/);
    assert.ok(!exit.includes("finishShutdown") && !exit.includes("finishWorkerShutdown"), "no barrier on the hard exit");
  });
});
