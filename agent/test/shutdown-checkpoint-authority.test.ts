import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { WorkerClient } from "../src/client.js";
import { buildRunLaneReconcile, createRunLaneInstallerReconcile } from "../src/codex/codex-executor.js";
import { selectCodexBinding } from "../src/codex/select.js";
import { createCodexExecutionSafety, CodexBoundaryError } from "../src/codex/safety.js";
import { ExecutionRegistry, newLocalExecutionEpoch, type RegisteredRoot } from "../src/codex/registry.js";
import type { BoundaryRequest } from "../src/harness.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { cleanCheckpointAdmission, CheckpointSoftDeadlineError } from "../src/checkpoint-stop.js";
import { SteeringChannel, PauseNowSignal, CredentialSwitchSignal } from "../src/steering.js";
import { nullLogger, withReceipts } from "./helpers.js";

function channel(claimGeneration = 0) {
  const cancel = new AbortController();
  const client = withReceipts({ getInputs: async () => ({ inputs: [] }) } as unknown as WorkerClient);
  return { ch: new SteeringChannel(client, "run-shutdown", 1, nullLogger(), cancel, { claimGeneration }), cancel };
}

function binding(authMode: "api_key" | "subscription") {
  const selected = selectCodexBinding({ codex: {
    auth_mode: authMode, access_token: "fixture-access", capability: "fixture-capability",
    ...(authMode === "subscription" ? {
      generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
    } : {}),
  } });
  assert.equal(selected.kind, "codex");
  if (selected.kind !== "codex") throw new Error("missing binding");
  return selected.binding;
}

function request(ch: SteeringChannel, deadlineAt = Date.now() + 10_000): BoundaryRequest {
  return { boundary: "shutdown", deadlineMs: 10_000,
    gracefulShutdownCheckpoint: ch.workerShutdown(deadlineAt) };
}

describe("M1 shutdown checkpoint authority", () => {
  it("worker shutdown prevents a sleeping poll loop from starting another input GET", async () => {
    let reads = 0;
    let sleeping!: () => void;
    const asleep = new Promise<void>(resolve => { sleeping = resolve; });
    let wake!: () => void;
    const wakeup = new Promise<void>(resolve => { wake = resolve; });
    const client = withReceipts({ getInputs: async () => { reads++; return { inputs: [] }; } } as unknown as WorkerClient);
    const ch = new SteeringChannel(client, "run-shutdown", 1, nullLogger(), new AbortController(), {
      sleep: async () => { sleeping(); await wakeup; },
    });
    ch.start();
    try {
      await asleep;
      assert.equal(reads, 1);
      ch.workerShutdown(Date.now() + 10_000);
      wake();
      await (ch as unknown as { loop: Promise<void> }).loop;
      assert.equal(reads, 1, "shutdown must not start another GET when an existing sleep completes");
    } finally {
      wake();
      await ch.stop();
    }
  });

  for (const conflict of ["claim fence", "credential switch"] as const) {
    for (const phase of ["before reconcile", "installer wait", "HTTP acquisition"] as const) {
      it(`delivered ${conflict} during ${phase} denies marked shutdown publication`, async () => {
        const { ch, cancel } = channel(1);
        const marked = request(ch);
        const deliver = () => {
          if (conflict === "claim fence") {
            // The production receipt failure caller is endFencedFlight in this tree.
            // Invoke that local delivery seam; never restart stopped input polling.
            (ch as unknown as { endFencedFlight(reason: string): void }).endFencedFlight("stale");
            assert.equal(ch.claimFence(), "stale");
          } else {
            ch.tripCredentialSwitch(1);
            assert.equal(ch.pendingCredentialSwitch(), 1);
          }
          assert.equal(marked.gracefulShutdownCheckpoint!.stopSignal.aborted, true);
        };
        let requests = 0;
        const server = createServer((req, res) => {
          assert.equal(req.method, "POST");
          assert.equal(req.url, "/api/worker/runs/run-shutdown/codex/refresh");
          req.resume();
          requests++;
          // Delivery occurs after WorkerClient sent the request and before a valid
          // response can grant a token or advance the shared committed generation.
          deliver();
          res.writeHead(200, { "Content-Type": "application/json" });
          res.end(JSON.stringify({ auth_mode: "subscription", access_token: "fixture-next",
            generation: 4, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
            outcome: "advanced" }));
        });
        await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
        const address = server.address();
        assert.ok(address && typeof address !== "string");
        const client = new WorkerClient(`http://127.0.0.1:${address.port}`, "fixture-worker",
          "test", nullLogger(), { sleep: async () => {} });
        const committed = { value: 3 };
        const tokens: string[] = [];
        const installer = createRunLaneInstallerReconcile(cancel.signal, {
          runId: "run-shutdown", binding: binding("subscription"), client, committed,
          registerToken: token => tokens.push(token),
        });
        let finish: (() => void) | undefined;
        if (phase === "installer wait") {
          installer.promise = new Promise(resolve => {
            finish = () => resolve({ results: [], truncated: false });
          });
          // reconcile aborts the real installer before waiting for its promise.
          // Queue delivery so settle has installed its cancellation listener.
          installer.abort.signal.addEventListener("abort", () => queueMicrotask(deliver), { once: true });
        }
        const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
          async () => assert.fail("no spawn"), installer.reconcile);
        let sinks = 0;
        try {
          if (phase === "before reconcile") deliver();
          await assert.rejects(safety.withBoundary(marked, async () => { sinks++; }),
            (error: unknown) => error instanceof CodexBoundaryError &&
              error.errors.some(e => e.category === "authorization"));
          assert.equal(requests, phase === "HTTP acquisition" ? 1 : 0);
          assert.equal(sinks, 0);
          assert.deepEqual(tokens, []);
          assert.equal(committed.value, 3);
        } finally {
          finish?.();
          await installer.settle();
          await safety.dispose({ boundary: "terminal", deadlineMs: 100 });
          server.closeAllConnections();
          await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
        }
      });
    }
  }

  it("marked shutdown cannot bypass an unfinished installer even with spent execution signals", async () => {
    const { ch, cancel } = channel();
    const marked = request(ch);
    const installer = createRunLaneInstallerReconcile(cancel.signal, {
      runId: "run-shutdown", binding: binding("api_key"),
      registerToken: () => assert.fail("unfinished installer registered token"),
      client: { releaseCodex: async () => assert.fail("unfinished installer reconciled credentials") } as never,
    });
    let finish!: () => void;
    installer.promise = new Promise(resolve => {
      finish = () => resolve({ results: [], truncated: false });
    });
    let fire!: () => void;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => assert.fail("no spawn"), installer.reconcile, undefined, undefined,
      (_request, _ms, fireDeadline) => { fire = fireDeadline; return () => {}; });
    // reconcile aborts the installer before joining it. Fire the boundary's deadline
    // at that event, without sleeping or leaving a writer running after the test.
    installer.abort.signal.addEventListener("abort", () => fire(), { once: true });
    try {
      await assert.rejects(safety.withBoundary(marked, async () => assert.fail("no sink")),
        (error: unknown) => error instanceof CodexBoundaryError &&
          error.errors.some(e => e.category === "timeout"));
      assert.equal(cancel.signal.aborted, true);
      assert.equal(marked.gracefulShutdownCheckpoint!.stopSignal.aborted, false);
    } finally {
      finish();
      await installer.settle();
      await safety.dispose({ boundary: "terminal", deadlineMs: 100 });
    }
  });

  for (const obstacle of ["writer", "unverified-survivor", "admission"] as const) {
    it(`marked shutdown keeps the production registry gate for ${obstacle}`, async () => {
      const { ch, cancel } = channel();
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
      let release = 0;
      const installer = createRunLaneInstallerReconcile(cancel.signal, {
        runId: "run-shutdown", binding: binding("api_key"), registerToken: () => {},
        client: { releaseCodex: async () => { release++; return { access_token: "fixture-token" }; } } as never,
      });
      const marked = request(ch, Date.now() + (obstacle === "admission" ? 100 : 10_000));
      let reapEntered = false;
      if (obstacle === "admission") {
        assert.equal(registry.reserveCallback({
          threadId: "thread", turnId: "turn", callId: "unsettled", fingerprint: "unsettled",
        }).kind, "admitted");
        // The real quiesce must drain this accepted callback; the original 100ms
        // deadline bounds the wait. Never manufacture a poison result in the test.
      } else {
        const root: RegisteredRoot = {
          kind: "command",
          reap: async () => {
            reapEntered = true;
            return { ok: false, error: { category: obstacle === "writer" ? "timeout" : "transport",
              message: obstacle === "writer" ? "writer survived reap" : "root emptiness unverified" } };
          },
          dispose: async () => {},
        };
        const reservation = registry.reserveLaunch("command");
        assert.equal(reservation.kind, "reserved");
        if (reservation.kind === "reserved") registry.registerRoot(reservation.reservation, root);
      }
      const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"), installer.reconcile);
      let sinkCalls = 0;
      try {
        await assert.rejects(safety.withBoundary(marked, async () => { sinkCalls++; }),
          (error: unknown) => error instanceof CodexBoundaryError);
        assert.equal(sinkCalls, 0, "no publication action admitted");
        assert.equal(release, 1, "successful credential reconcile alone never authorizes publication");
        assert.equal(reapEntered, obstacle !== "admission");
        assert.equal(registry.isPoisoned(), true);
      } finally { await safety.dispose({ boundary: "terminal", deadlineMs: 100 }); }
    });
  }

  it("a valid real HTTP credential response arriving after the original deadline is discarded", async (t) => {
    let now = 100;
    t.mock.method(Date, "now", () => now);
    let requests = 0;
    const server = createServer((req, res) => {
      assert.equal(req.method, "POST");
      assert.equal(req.url, "/api/worker/runs/run-shutdown/codex/release");
      req.resume();
      requests++;
      now = 151;
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(JSON.stringify({ auth_mode: "api_key", access_token: "fixture-late" }));
    });
    await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
    const address = server.address();
    assert.ok(address && typeof address !== "string");
    const client = new WorkerClient(`http://127.0.0.1:${address.port}`, "fixture-worker",
      "test", nullLogger(), { sleep: async () => {} });
    const { ch, cancel } = channel();
    const marked = request(ch, 150);
    const committed = { value: 3 };
    const installer = createRunLaneInstallerReconcile(cancel.signal, {
      runId: "run-shutdown", binding: binding("api_key"), client, committed,
      registerToken: () => assert.fail("late HTTP token registered"),
    });
    try {
      assert.equal((await installer.reconcile(marked, new AbortController().signal)).kind, "blocked");
      assert.equal(requests, 1);
      assert.equal(committed.value, 3);
    } finally {
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    }
  });

  for (const delivery of ["seeded", "live"] as const) {
    it(`${delivery} graceful finish before shutdown preserves checkpoint authority`, async () => {
      const { ch, cancel } = channel();
      if (delivery === "seeded") ch.seedStopRequested();
      else {
        // Use the existing input routing seam without starting another poller.
        (ch as unknown as { route(kind: string, body: undefined, id: number): void })
          .route("stop", undefined, 3101);
      }
      assert.deepEqual(await ch.awaitFollowUp(10_000), { kind: "ended", reason: "stopped" });
      assert.equal(cancel.signal.aborted, false, "finish lets the current turn complete");
      assert.equal(ch.lifecycleSignal().aborted, false);
      const cause = ch.workerShutdown(Date.now() + 10_000);
      assert.equal(cancel.signal.aborted, true);
      assert.equal(ch.lifecycleSignal().aborted, true);
      assert.equal(ch.terminalLifecycleSignal().aborted, true);
      assert.equal(cause.stopSignal.aborted, false);
    });
  }

  it("spends execution signals once and retains the first immutable deadline", () => {
    const { ch, cancel } = channel();
    const cause = ch.workerShutdown(100);
    assert.equal(cancel.signal.aborted, true);
    assert.equal(ch.lifecycleSignal().aborted, true);
    assert.equal(ch.terminalLifecycleSignal().aborted, true);
    assert.equal(cause.stopSignal.aborted, false);
    assert.equal(ch.workerShutdown(999), cause);
    assert.equal(cause.deadlineAt, 100);
    assert.equal(Object.isFrozen(cause), true);
    ch.markCancelled();
    assert.equal(cause.stopSignal.aborted, true);
    assert.equal(ch.workerShutdown(999).stopSignal.aborted, true);
  });

  for (const stop of ["terminal", "cancel", "switch", "disk"] as const) {
    it(`a delivered ${stop} prevents shutdown eligibility before or after execution abort`, () => {
      for (const before of [true, false]) {
        const { ch } = channel();
        const deliver = () => {
          if (stop === "terminal") ch.abortLifecycle();
          if (stop === "cancel") ch.markCancelled();
          if (stop === "switch") ch.tripCredentialSwitch(0);
          if (stop === "disk") ch.requestDiskStop();
        };
        if (before) deliver();
        const cause = ch.workerShutdown(100);
        if (!before) deliver();
        assert.equal(cause.stopSignal.aborted, true);
      }
    });
  }

  it("pause/rearm never creates checkpoint authority and cannot clear a switch revocation", () => {
    const { ch } = channel();
    ch.abortLifecycle(new PauseNowSignal(), false);
    ch.rearmLifecycle();
    assert.equal(ch.lifecycleSignal().aborted, false);
    ch.abortLifecycle(new CredentialSwitchSignal(), false);
    ch.rearmLifecycle();
    assert.equal(ch.workerShutdown(100).stopSignal.aborted, true);
  });

  for (const authMode of ["api_key", "subscription"] as const) {
    it(`${authMode}: only a marked shutdown bypasses the spent execution lifecycle`, async () => {
      const { ch, cancel } = channel();
      const marked = request(ch);
      let releases = 0;
      let refreshes = 0;
      const tokens: string[] = [];
      const committed = { value: 3 };
      const installer = createRunLaneInstallerReconcile(
        AbortSignal.any([cancel.signal, ch.terminalLifecycleSignal()]),
        {
          runId: "run-shutdown", binding: binding(authMode), committed,
          registerToken: (token) => tokens.push(token),
          client: {
            releaseCodex: async () => { releases++; return { access_token: "fixture-next" }; },
            refreshCodex: async () => { refreshes++; return { access_token: "fixture-next", generation: 4 }; },
          } as never,
        },
      );
      const boundary = new AbortController();
      const unmarked = await installer.reconcile({ boundary: "shutdown", deadlineMs: 1000 }, boundary.signal);
      assert.equal(unmarked.kind, "blocked");
      assert.equal(releases + refreshes, 0);
      assert.equal((await installer.reconcile(marked, boundary.signal)).kind, "ready");
      assert.deepEqual(tokens, ["fixture-next"]);
      assert.equal(releases, authMode === "api_key" ? 1 : 0);
      assert.equal(refreshes, authMode === "subscription" ? 1 : 0);
      assert.equal(committed.value, authMode === "subscription" ? 4 : 3);
    });

    it(`${authMode}: a valid response after the original deadline grants no token or generation`, async (t) => {
      let now = 100;
      t.mock.method(Date, "now", () => now);
      const { ch, cancel } = channel();
      const marked = request(ch, 150);
      const committed = { value: 3 };
      const installer = createRunLaneInstallerReconcile(cancel.signal, {
        runId: "run-shutdown", binding: binding(authMode), committed,
        registerToken: () => assert.fail("late token registration"),
        client: {
          releaseCodex: async () => { now = 151; return { access_token: "fixture-late" }; },
          refreshCodex: async () => { now = 151; return { access_token: "fixture-late", generation: 4 }; },
        } as never,
      });
      assert.equal((await installer.reconcile(marked, new AbortController().signal)).kind, "blocked");
      assert.equal(committed.value, 3);
    });
  }

  it("a delivered transport failure revokes the permit but awaits actual action settlement", async () => {
    const { ch } = channel();
    const marked = request(ch);
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(3));
    const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"),
      async () => ({ kind: "ready" }));
    let entered!: () => void;
    const started = new Promise<void>((resolve) => { entered = resolve; });
    let finish!: () => void;
    const pending = new Promise<void>((resolve) => { finish = resolve; });
    let settled = false;
    const boundary = safety.withBoundary(marked, async (permit) => {
      ch.abortLifecycle(new Error("local transport failure"));
      assert.equal(permit.signal.aborted, true);
      entered();
      await pending;
      throw new Error("primary action failure");
    }).finally(() => { settled = true; });
    const rejected = assert.rejects(boundary, (error: unknown) =>
      error instanceof CodexBoundaryError && error.errors[0]?.category === "authorization" &&
      error.actionError instanceof Error && error.actionError.message === "primary action failure");
    await started;
    assert.equal(settled, false);
    finish();
    await rejected;
  });

  it("installer timeout remains distinct from lifecycle refusal", async () => {
    const { ch, cancel } = channel();
    const marked = request(ch, 0);
    const installer = createRunLaneInstallerReconcile(cancel.signal, {
      runId: "run-shutdown", binding: binding("api_key"),
      registerToken: () => assert.fail("no registration"),
      client: { releaseCodex: async () => assert.fail("no release") } as never,
    });
    installer.promise = Promise.resolve({ results: [], truncated: false });
    const timeout = await installer.reconcile(marked, new AbortController().signal);
    assert.equal(timeout.kind === "blocked" && timeout.errors[0]?.category, "timeout");
    ch.markCancelled();
    const refused = await installer.reconcile(marked, new AbortController().signal);
    assert.equal(refused.kind === "blocked" && refused.errors[0]?.category, "authorization");
    await installer.settle();
  });

  for (const obstacle of ["aborted lifecycle", "installer timeout"] as const) {
    it(`unmarked ${obstacle} retains the original installer protocol refusal`, async () => {
      const lifecycle = new AbortController();
      const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
        runId: "run-shutdown", binding: binding("api_key"),
        registerToken: () => assert.fail("no registration"),
        client: { releaseCodex: async () => assert.fail("no release") } as never,
      });
      let finish!: () => void;
      installer.promise = new Promise(resolve => {
        finish = () => resolve({ results: [], truncated: false });
      });
      if (obstacle === "aborted lifecycle") lifecycle.abort();
      // A real outstanding installer at a spent operation deadline, not a spent lifecycle.
      try {
        assert.deepEqual(await installer.reconcile({
          boundary: "shutdown", deadlineMs: obstacle === "installer timeout" ? 0 : 10_000,
        }, new AbortController().signal), {
          kind: "blocked", errors: [{
            category: "protocol", message: "codex dependency install did not settle before reconcile",
          }],
        });
      } finally { finish(); await installer.settle(); }
    });
  }

  it("safety consumes the cause deadline before credential acquisition", async () => {
    const { ch } = channel();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
    const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"),
      async () => assert.fail("no reconcile"));
    await assert.rejects(safety.withBoundary(request(ch, 0), async () => assert.fail("no sink")),
      (error: unknown) => error instanceof CodexBoundaryError && error.errors[0]?.category === "timeout");
  });

  it("sticky revocation is authorization, even when the original deadline also expired", async () => {
    const { ch } = channel();
    const marked = request(ch, 0);
    ch.markCancelled();
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(2)),
      async () => assert.fail("no spawn"), async () => assert.fail("no reconcile"));
    await assert.rejects(safety.withBoundary(marked, async () => assert.fail("no sink")),
      (error: unknown) => error instanceof CodexBoundaryError && error.errors[0]?.category === "authorization");
  });

  it("unmarked safety ignores new budget entry gating after the existing queue guard passed", async t => {
    let now = 100;
    const budget = new MaintenanceBudget(10_000, () => now);
    const waitFor = MaintenanceBudget.waitFor.bind(MaintenanceBudget);
    let queuePassed = false;
    t.mock.method(MaintenanceBudget, "waitFor", async <T>(pending: Promise<T>): Promise<T> => {
      const value = await waitFor(pending);
      queuePassed = true;
      now = 10_101;
      assert.throws(() => budget.check(), /maintenance deadline exhausted/);
      return value;
    });
    let reconciles = 0;
    const safety = createCodexExecutionSafety(new ExecutionRegistry(newLocalExecutionEpoch(1)),
      async () => assert.fail("no spawn"), async (_request, signal) => {
        assert.equal(queuePassed, true);
        assert.equal(signal.aborted, false);
        reconciles++;
        return { kind: "ready" };
      });
    try {
      await MaintenanceBudget.run(budget, async () => {
        assert.equal(await safety.withBoundary({ boundary: "shutdown", deadlineMs: 10_000 },
          async permit => {
            assert.equal(permit.signal.aborted, false);
            return "published";
          }), "published");
      });
      assert.equal(reconciles, 1);
    } finally {
      budget.dispose();
      await safety.dispose({ boundary: "terminal", deadlineMs: 100 });
    }
  });

  for (const marked of [false, true]) {
    for (const phase of ["reconcile", "permit action"] as const) {
      it(`${marked ? "marked" : "unmarked"} safety budget exhaustion during ${phase} scopes the new cancellation gates`, async () => {
        let now = 100;
        const budget = new MaintenanceBudget(10_000, () => now);
        const registry = new ExecutionRegistry(newLocalExecutionEpoch(1));
        const expire = () => {
          now = 10_101;
          assert.throws(() => budget.check(), /maintenance deadline exhausted/);
        };
        let reconciles = 0;
        let sinks = 0;
        const safety = createCodexExecutionSafety(registry, async () => assert.fail("no spawn"),
          async (_request, signal) => {
            reconciles++;
            assert.equal(signal.aborted, false, "existing queue wait passed with live budget");
            if (phase === "reconcile") {
              expire();
              assert.equal(signal.aborted, marked);
            }
            return { kind: "ready" };
          });
        const boundary: BoundaryRequest = marked ? request(channel().ch) :
          { boundary: "shutdown", deadlineMs: 10_000 };
        try {
          await MaintenanceBudget.run(budget, async () => {
            const pending = safety.withBoundary(boundary, async permit => {
              sinks++;
              if (phase === "permit action") expire();
              assert.equal(permit.signal.aborted, marked);
              return "published";
            });
            if (marked) {
              await assert.rejects(pending, (error: unknown) =>
                error instanceof CodexBoundaryError && error.errors.some(e => e.category === "timeout"));
            } else assert.equal(await pending, "published");
          });
          assert.equal(reconciles, 1);
          assert.equal(sinks, marked && phase === "reconcile" ? 0 : 1);
          assert.equal(registry.isPoisoned(), marked);
        } finally {
          budget.dispose();
          await safety.dispose({ boundary: "terminal", deadlineMs: 100 });
        }
      });
    }
  }

  for (const factory of ["installer", "direct"] as const) {
    for (const authMode of ["api_key", "subscription"] as const) {
      for (const phase of (factory === "installer"
        ? ["before acquisition", "installer join", "credential response"] as const
        : ["before acquisition", "credential response"] as const)) {
        it(`unmarked ${factory} ${authMode} ignores new ambient budget gates at ${phase}`, async () => {
          let now = 100;
          const budget = new MaintenanceBudget(10_000, () => now);
          const lifecycle = new AbortController();
          const boundarySignal = new AbortController().signal;
          const committed = { value: 3 };
          const tokens: string[] = [];
          let calls = 0;
          const expire = () => {
            now = 10_101;
            assert.throws(() => budget.check(), /maintenance deadline exhausted/);
          };
          const response = (signal: AbortSignal) => {
            calls++;
            assert.equal(signal.aborted, false);
            if (phase === "credential response") expire();
            assert.equal(signal.aborted, false, "unmarked credential I/O retains its operation signal");
            return { access_token: "fixture-next", generation: 4 };
          };
          const client = {
            releaseCodex: async (_run: string, _body: unknown, _expected: unknown, signal: AbortSignal) => response(signal),
            refreshCodex: async (_run: string, _body: unknown, _expected: unknown, signal: AbortSignal) => response(signal),
          } as never;
          const registerToken = (token: string) => tokens.push(token);
          const installer = createRunLaneInstallerReconcile(lifecycle.signal, {
            runId: "run-shutdown", binding: binding(authMode), client, committed, registerToken,
          });
          let finish: (() => void) | undefined;
          if (phase === "installer join") {
            installer.promise = new Promise(resolve => {
              finish = () => resolve({ results: [], truncated: false });
            });
            // settle has been entered with live authority. Expire at its actual
            // installer-abort event and settle the owned writer without a real wait.
            installer.abort.signal.addEventListener("abort", () => {
              expire();
              finish!();
            }, { once: true });
          }
          const reconcile = factory === "installer" ? installer.reconcile :
            buildRunLaneReconcile("run-shutdown", client, binding(authMode), registerToken,
              committed, undefined, lifecycle.signal);
          try {
            await MaintenanceBudget.run(budget, async () => {
              // Direct API callers have no queue. Exhaust only after entering the live
              // budget scope; these calls never ask an already-expired queue guard to pass.
              if (phase === "before acquisition") expire();
              assert.deepEqual(await reconcile({ boundary: "shutdown", deadlineMs: 10_000 },
                boundarySignal), { kind: "ready" });
            });
            assert.equal(calls, 1);
            assert.deepEqual(tokens, ["fixture-next"]);
            assert.equal(committed.value, authMode === "subscription" ? 4 : 3);
            assert.equal(lifecycle.signal.aborted, false);
            assert.equal(boundarySignal.aborted, false);
          } finally { budget.dispose(); finish?.(); await installer.settle(); }
        });
      }
    }
  }

  it("maintenance cancellation reaches reconcile without restoring execution authority", async () => {
    const { ch, cancel } = channel();
    const marked = request(ch);
    let now = 100;
    const budget = new MaintenanceBudget(50, () => now);
    try {
      await MaintenanceBudget.run(budget, async () => {
        const installer = createRunLaneInstallerReconcile(cancel.signal, {
          runId: "run-shutdown", binding: binding("api_key"),
          registerToken: () => assert.fail("no registration"),
          client: { releaseCodex: async (_run: string, _body: unknown, _expected: unknown, signal: AbortSignal) => {
            assert.equal(signal.aborted, false);
            now = 151;
            assert.throws(() => budget.check());
            assert.equal(signal.aborted, true);
            return { access_token: "fixture-late" };
          } } as never,
        });
        assert.equal((await installer.reconcile(marked, new AbortController().signal)).kind, "blocked");
        assert.equal(cancel.signal.aborted, true);
        assert.equal(marked.gracefulShutdownCheckpoint!.stopSignal.aborted, false);
      });
    } finally { budget.dispose(); }
  });
});

for (const side of ["captured", "reconciler"] as const) {
  for (const other of ["active", "pause"] as const) {
    for (const reason of ["untyped", "hard", "credential"] as const) {
      it(`soft admission rejects ${side} ${reason} lifecycle with ${other} peer`, async () => {
        const captured = new AbortController(), actual = new AbortController(), soft = new AbortController();
        const bad = side === "captured" ? captured : actual;
        const peer = side === "captured" ? actual : captured;
        bad.abort(reason === "untyped" ? undefined : reason === "credential"
          ? new CredentialSwitchSignal() : new Error("hard stop"));
        if (other === "pause") peer.abort(new PauseNowSignal());
        soft.abort();
        const stop = { signal: soft.signal, lifecycleSignal: captured.signal,
          cooperativeLifecycleReason: (value: unknown) => value instanceof PauseNowSignal };
        assert.equal(cleanCheckpointAdmission(stop, actual.signal), false);
        let calls = 0;
        const reconcile = buildRunLaneReconcile("run-shutdown", {
          releaseCodex: async () => { calls++; return { access_token: "fixture-next" }; },
        } as never, binding("api_key"), () => {}, undefined, undefined, actual.signal);
        const result = await reconcile({ boundary: "checkpoint", deadlineMs: 1000, checkpointStop: stop },
          new AbortController().signal);
        assert.equal(result.kind, "blocked");
        assert.equal(calls, 0);
      });
    }
  }
}

it("soft admission accepts two typed pause lifecycles without credential exchange", async () => {
  const captured = new AbortController(), actual = new AbortController(), soft = new AbortController();
  captured.abort(new PauseNowSignal()); actual.abort(new PauseNowSignal()); soft.abort();
  const stop = { signal: soft.signal, lifecycleSignal: captured.signal,
    cooperativeLifecycleReason: (value: unknown) => value instanceof PauseNowSignal };
  assert.equal(cleanCheckpointAdmission(stop, actual.signal), true);
  let calls = 0;
  const reconcile = buildRunLaneReconcile("run-shutdown", {
    releaseCodex: async () => { calls++; return { access_token: "fixture-next" }; },
  } as never, binding("api_key"), () => {}, undefined, undefined, actual.signal);
  await assert.rejects(reconcile({ boundary: "checkpoint", deadlineMs: 1000, checkpointStop: stop },
    new AbortController().signal), CheckpointSoftDeadlineError);
  assert.equal(calls, 0);
});

for (const authMode of ["api_key", "subscription"] as const) {
  for (const pause of ["now", "wall", "withdrawn-now", "late-now", "late-wall"] as const) {
    it(`${authMode}: genuine ${pause} preserves marked shutdown credential authority`, async () => {
      const { ch, cancel } = channel();
      const route = ch as unknown as { route(kind: string, body?: string, id?: number): void };
      const late = pause.startsWith("late-");
      let marked: BoundaryRequest | undefined = late ? request(ch) : undefined;
      route.route("pause", pause.endsWith("wall") ? "wall" : "now", 1);
      if (pause === "withdrawn-now") route.route("pause_cancel", undefined, 2);
      marked ??= request(ch);
      const tokens: string[] = [];
      let calls = 0, sinks = 0;
      const committed = { value: 3 };
      const installer = createRunLaneInstallerReconcile(AbortSignal.any([cancel.signal, ch.terminalLifecycleSignal()]), {
        runId: "run-shutdown", binding: binding(authMode), committed, registerToken: token => tokens.push(token),
        client: {
          releaseCodex: async () => { calls++; return { access_token: "fixture-next" }; },
          refreshCodex: async () => { calls++; return { access_token: "fixture-next", generation: 4 }; },
        } as never,
      });
      const registry = new ExecutionRegistry(newLocalExecutionEpoch(41));
      const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unused spawn"); }, installer.reconcile);
      try {
        await safety.withBoundary(marked, async () => { sinks++; });
        assert.equal(marked.gracefulShutdownCheckpoint!.stopSignal.aborted, false);
        assert.equal(calls, 1);
        assert.equal(sinks, 1);
        assert.deepEqual(tokens, ["fixture-next"]);
        assert.equal(committed.value, authMode === "subscription" ? 4 : 3);
        assert.equal(registry.isPoisoned(), false);
      } finally {
        await Promise.allSettled([installer.settle(), safety.dispose({ boundary: "terminal", deadlineMs: 100 })]);
      }
    });
  }
}

for (const conflict of ["none", "withdrawn", "cancel", "claim", "switch", "disk", "terminal"] as const) {
  it(`typed pause admission retains independent ${conflict} precedence`, async () => {
    const { ch, cancel } = channel(1);
    const lifecycle = ch.lifecycleSignal();
    const route = ch as unknown as { route(kind: string, body?: string, id?: number): void; endFencedFlight(reason: string): void };
    route.route("pause", "now", 1);
    if (conflict === "withdrawn") route.route("pause_cancel", undefined, 2);
    if (conflict === "cancel") ch.markCancelled();
    if (conflict === "claim") route.endFencedFlight("stale");
    if (conflict === "switch") ch.tripCredentialSwitch(1);
    if (conflict === "disk") ch.requestDiskStop();
    if (conflict === "terminal") ch.abortLifecycle(new Error("terminal"));
    const soft = new AbortController(); soft.abort();
    let calls = 0, sinks = 0;
    const installer = createRunLaneInstallerReconcile(AbortSignal.any([cancel.signal, ch.terminalLifecycleSignal()]), {
      runId: "run-shutdown", binding: binding("api_key"), registerToken: () => {},
      client: { releaseCodex: async () => { calls++; return { access_token: "fixture-next" }; } } as never,
    });
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(42));
    const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unused spawn"); }, installer.reconcile);
    const clean = conflict === "none" || conflict === "withdrawn";
    try {
      await assert.rejects(safety.withBoundary({ boundary: "checkpoint", sink: "done_checkpoint", deadlineMs: 1000,
        checkpointStop: { signal: soft.signal, lifecycleSignal: lifecycle, terminalSignal: AbortSignal.any([ch.terminalLifecycleSignal(), ch.checkpointAuthoritySignal()]),
          cooperativeLifecycleReason: reason => reason instanceof PauseNowSignal,
          authorityRevoked: () => ch.isCancelled() || ch.claimFence() !== undefined ||
            ch.pendingCredentialSwitch() !== undefined || ch.getPauseMode() === "disk" },
      }, async () => { sinks++; }), clean ? CheckpointSoftDeadlineError : CodexBoundaryError);
      assert.equal(lifecycle.reason instanceof PauseNowSignal, true);
      assert.equal(registry.isPoisoned(), !clean);
      assert.equal(calls, 0);
      assert.equal(sinks, 0);
    } finally {
      await Promise.allSettled([installer.settle(), safety.dispose({ boundary: "terminal", deadlineMs: 100 })]);
    }
  });
}

for (const conflict of ["cancel", "claim", "switch", "disk", "terminal"] as const) {
  it(`late ${conflict} revokes a mutation permit after typed pause spent the lifecycle`, async () => {
    const { ch } = channel(1);
    const lifecycle = ch.lifecycleSignal();
    const soft = new AbortController();
    const registry = new ExecutionRegistry(newLocalExecutionEpoch(43));
    const safety = createCodexExecutionSafety(registry, async () => { throw new Error("unused spawn"); });
    let afterPause: boolean | undefined, afterTerminal: boolean | undefined;
    try {
      await assert.rejects(safety.withBoundary({ boundary: "checkpoint", deadlineMs: 1000,
        checkpointStop: { signal: soft.signal, lifecycleSignal: lifecycle, terminalSignal: AbortSignal.any([ch.terminalLifecycleSignal(), ch.checkpointAuthoritySignal()]),
          cooperativeLifecycleReason: reason => reason instanceof PauseNowSignal },
      }, async permit => {
        (ch as unknown as { route(kind: string, body: string, id: number): void }).route("pause", "now", 1);
        soft.abort();
        afterPause = permit.signal.aborted;
        if (conflict === "cancel") (ch as unknown as { route(kind: string): void }).route("cancel");
        else if (conflict === "claim") (ch as unknown as { endFencedFlight(reason: string): void }).endFencedFlight("stale");
        else if (conflict === "switch") ch.tripCredentialSwitch(1);
        else if (conflict === "disk") ch.requestDiskStop();
        else ch.abortLifecycle(new Error("terminal"));
        afterTerminal = permit.signal.aborted;
      }), CodexBoundaryError);
      assert.equal(afterPause, false);
      assert.equal(afterTerminal, true);
      assert.equal(registry.isPoisoned(), true);
    } finally { await Promise.allSettled([safety.dispose({ boundary: "terminal", deadlineMs: 100 })]); }
  });
}
