import { it } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { PassThrough } from "node:stream";
import { installHarness, git, deferred } from "./runner-harness.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
installHarness();

for (const cancel of ["signal", "timeout"] as const) {
  it(`actual report child ${cancel} keeps reservation and bare lock until close`, async t => {
    const f = await reportRetirementFixture(t);
    const context = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, 2))!.context;
    const abort = new AbortController();
    const ready = deferred(), signalled = deferred();
    let closed = false, settled = false, successor = false;
    let ownChild: ReturnType<typeof spawn> | undefined;
    const pending = git.withBoundaryProcessSpawner(async () => {
      // Literal inert fixture, never executes the screened/requested command payload.
      const child = spawn(process.execPath, ["-e",
        'process.on("SIGTERM",()=>{process.stdout.write("signal\\n");setTimeout(()=>process.exit(143),100)});process.stdout.write("ready\\n");setInterval(()=>{},1000)'],
        { stdio: ["ignore", "pipe", "pipe"] });
      ownChild = child;
      child.stdout!.on("data", chunk => {
        if (String(chunk).includes("ready")) ready.resolve();
        if (String(chunk).includes("signal")) signalled.resolve();
      });
      const completed = new Promise<{ code: number }>(resolve => child.once("close", code => {
        closed = true; resolve({ code: code ?? -1 });
      }));
      return { stdin: null, stdout: child.stdout, stderr: child.stderr, completed,
        cancel: async () => { child.kill("SIGTERM"); await completed; } };
    }, new AbortController().signal, () => f.r.retireRecoveryReport(f.claim.run_id, 2, {
      ...context, signal: abort.signal,
    })).finally(() => { settled = true; });
    t.after(async () => { ownChild?.kill("SIGTERM"); await pending; });
    await ready.promise;
    assert.equal(f.r.isExecuting(f.claim.run_id), true);
    if (cancel === "signal") abort.abort(new Error("own-child cancel"));
    await signalled.promise;
    const next = git.withBareLock(f.bare, async () => { successor = true; });
    assert.equal(closed, false);
    assert.equal(settled, false);
    assert.equal(successor, false);
    assert.equal(f.r.isExecuting(f.claim.run_id), true);
    await pending; await next;
    assert.equal(closed, true);
    assert.equal(successor, true);
    assert.equal(f.r.isExecuting(f.claim.run_id), false);
    await f.assertPending();
  });
}


it("report signal cancels existing boundary child and retains bare until whole-root settlement", async t => {
  const f = await reportRetirementFixture(t);
  const abort = new AbortController();
  const entered = deferred(), closed = deferred(), cancelled = deferred();
  let spawns = 0, timeout = 0, settled = false, successor = false;
  const stdout = new PassThrough(), stderr = new PassThrough();
  const pending = git.withReportProofBudget({ signal: abort.signal, deadline: Date.now() + 1000 },
    () => git.withBoundaryProcessSpawner(async request => {
      spawns++; timeout = request.timeoutMs!;
      entered.resolve();
      return { stdin: null, stdout, stderr,
        completed: closed.promise.then(() => ({ code: 0 })),
        cancel: async () => { cancelled.resolve(); await closed.promise; },
      };
    }, new AbortController().signal,
    () => git.withBareLock(f.bare, () => git.ancestry(f.bare, f.head, f.head))))
    .finally(() => { settled = true; });
  const rejected = assert.rejects(pending);
  t.after(async () => { closed.resolve(); stdout.end(); stderr.end(); await rejected; });
  await entered.promise;
  assert.ok(timeout > 0 && timeout <= 1000, "report deadline reaches boundary spawn");
  abort.abort();
  await cancelled.promise;
  const next = git.withBareLock(f.bare, async () => { successor = true; });
  await new Promise<void>(r => setImmediate(r));
  assert.equal(settled, false);
  assert.equal(successor, false);
  closed.resolve(); stdout.end(); stderr.end();
  await rejected; await next;
  assert.equal(successor, true);
  assert.equal(spawns, 1);
  await f.assertPending();
});

it("expired report inside an existing boundary spawns no child", async t => {
  const f = await reportRetirementFixture(t);
  let spawns = 0;
  assert.equal(await git.withReportProofBudget({
    signal: new AbortController().signal, deadline: Date.now() - 1,
  }, () => git.withBoundaryProcessSpawner(async () => {
    spawns++; throw new Error("unexpected child");
  }, new AbortController().signal, () => git.ancestry(f.bare, f.head, f.head))), "unknown");
  assert.equal(spawns, 0);
  await f.assertPending();
});
