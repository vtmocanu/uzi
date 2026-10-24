import { codexRig, FakeCodexExecutor } from "./runner-codex-sinks-fixture.js";
import type { SteeringChannel } from "../src/steering.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Executor, RunContext } from "../src/executor.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { MessageBatcher } from "../src/batcher.js";
import {
  api,
  client,
  deferred,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";

installHarness({ httpTimeoutMs: 100 });

// PRD #1226 M4 (D3/D6): the Runner's enterCompletionHold seam, driven end-to-end through the real
// runner (clone seed → executor → phasePublish → finally). The injected executor calls
// ctx.enterCompletionHold and reports the boolean it returned; the git CAPTURE calls
// (worktreeStatus / fetchAgentBranch / verifyRunnerTrackingCovers / trackingTip / checkpointPack)
// are stubbed on the shared GitCache AFTER the runner has already seeded the clone, so the capture
// verdict is deterministic without a live checkpoint endpoint. The FakeApi completion/hold route
// answers a configurable {run:{status}} so the paused/refused ACK contract is provable.

const CAPTURED_HEAD = "cafef00dcafef00dcafef00dcafef00dcafef00d";

interface HoldProbe {
  executor: Executor;
  held: { value: boolean | undefined };
  /** PRD #1225 (CodeRabbit !1254): number of verifyRunnerTrackingCovers calls = capture attempts the
   *  bounded retry loop actually ran. Lets a test pin the retry count against a "shrink attempts to 1"
   *  or "drop the try/catch" mutant. */
  captureAttempts: { count: number };
}

// makeHoldExecutor returns an executor whose run() stubs the capture git to the given verdict, calls
// ctx.enterCompletionHold, and — modelling the sdk-executor call sites — latches completionHeld on a
// true return (the hold was entered) or THROWS the legacy terminal error on a false return (kept
// live, falls back to the legacy throw). It records the boolean for the test.
//
// `verdict` is either a single constant verdict (used by every attempt) or a SCRIPT of per-attempt
// verdicts (`"throw"` makes verifyRunnerTrackingCovers throw that attempt, a boolean makes it
// return that value). The script drives enterCompletionHold's bounded capture-retry loop across a
// throw, a false, and a true so the loop — not a single constant verdict — is exercised.
function makeHoldExecutor(verdict: boolean | Array<"throw" | boolean>, afterHold?: () => void): HoldProbe {
  const held: { value: boolean | undefined } = { value: undefined };
  const captureAttempts = { count: 0 };
  const executor: Executor = {
    run: async (ctx: RunContext) => {
      // Stub the capture git deterministically (the runner already seeded the real clone).
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus; // clean tree
      mockCommittedTracking(git, CAPTURED_HEAD);
      git.verifyRunnerTrackingCovers = (async () => {
        // Count every attempt that reaches the positive-verify step (one per capture attempt).
        const i = captureAttempts.count++;
        const step = Array.isArray(verdict)
          ? verdict[Math.min(i, verdict.length - 1)]!
          : verdict;
        if (step === "throw") throw new Error("capture attempt failed (scripted throw)");
        return step;
      }) as typeof git.verifyRunnerTrackingCovers;
      git.trackingTip = (async () => CAPTURED_HEAD) as typeof git.trackingTip;
      git.checkpointPack = (async () => null) as typeof git.checkpointPack; // publish is a no-op

      const entered = await ctx.enterCompletionHold!("test completion hold reason");
      held.value = entered;
      afterHold?.();
      if (entered) return { branch: ctx.branch, completionHeld: { reason: "test completion hold reason" } };
      // A false return means keep-live-then-legacy-throw at the real call sites.
      throw new Error("legacy terminal (kept live, could not hold)");
    },
  };
  return { executor, held, captureAttempts };
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

/** A pre-created run HOME with a sentinel; if the run preserves HOME the sentinel survives. */
function seedHome(): { homeDir: string; sentinel: string; root: string } {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-hold-home-"));
  const homeDir = path.join(root, "h");
  fs.mkdirSync(homeDir, { recursive: true });
  const sentinel = path.join(homeDir, "SESSION_TRANSCRIPT");
  fs.writeFileSync(sentinel, "resumable session state\n");
  return { homeDir, sentinel, root };
}

describe("RunRunner — recoverable completion hold (PRD #1226 M4 D6)", () => {
  it("ambiguous ACK + exact running snapshot retains clone and HOME without a terminal report, even after executor error", async (t) => {
    const close = MessageBatcher.prototype.close;
    let completedCloses = 0;
    t.mock.method(MessageBatcher.prototype, "close", async function (
      this: MessageBatcher, ...args: Parameters<MessageBatcher["close"]>
    ) {
      await close.apply(this, args);
      completedCloses++;
    });
    const { gitlab, calls } = fakeGitlab();
    const probe = makeHoldExecutor(true);
    const home = seedHome();
    const claim = { ...gitlabClaim(1254), claim_generation: 7 };
    const executor: Executor = {
      run: async (ctx) => {
        await probe.executor.run(ctx);
        throw new Error("late executor error after unresolved hold");
      },
    };
    try {
      api.setCompletionHoldResponse("", 200);
      api.parkRunOwnership = {
        status: "running", claim_generation: 7, hold_reason: null, claim_released: false,
      };
      const readsBefore = api.ownershipRequests.length;
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1, completionHoldReconcileTimeoutMs: 5_000,
      }).execute(claim);

      assert.equal(completedCloses, 1, "execute awaits real batcher close after the late error");
      assert.equal(probe.held.value, true, "running cannot fence a delayed hold POST");
      assert.deepEqual(api.ownershipRequests.slice(readsBefore), [claim.run_id], "one reconciliation read");
      assert.ok(!statuses(claim.run_id).some((status) => ["completed", "failed", "cancelled"].includes(status)));
      assert.equal(calls.length, 0);
      assert.equal(fs.existsSync(worktreeDirFor(1254)), true);
      assert.equal(fs.existsSync(home.sentinel), true);
    } finally {
      api.parkRunOwnership = undefined;
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("ambiguous ACK + exact unheld terminal proof permits normal cleanup", async () => {
    const { gitlab } = fakeGitlab();
    let readsAfterHold: string[] = [];
    const readsBefore = api.ownershipRequests.length;
    const { executor, held } = makeHoldExecutor(true, () => {
      readsAfterHold = api.ownershipRequests.slice(readsBefore);
    });
    const home = seedHome();
    const claim = { ...gitlabClaim(1255), claim_generation: 7 };
    try {
      api.setCompletionHoldResponse("", 200);
      api.parkRunOwnership = {
        status: "failed", claim_generation: 7, hold_reason: null, claim_released: false,
      };
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1, completionHoldReconcileTimeoutMs: 5_000,
      }).execute(claim);

      assert.equal(held.value, false);
      assert.deepEqual(readsAfterHold, [claim.run_id], "one read before normal terminal cleanup");
      assert.equal(fs.existsSync(worktreeDirFor(1255)), false);
      assert.equal(fs.existsSync(home.sentinel), false);
    } finally {
      api.parkRunOwnership = undefined;
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("verified capture + paused ACK → held: requests the hold with the captured head, preserves the clone and HOME", async () => {
    const { gitlab, calls } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(true);
    const home = seedHome();
    const claim = gitlabClaim(1250);
    try {
      api.setCompletionHoldResponse("paused", 200);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, true, "enterCompletionHold entered the verified hold");
      // The hold was requested with the captured tracking tip H.
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold was requested exactly once");
      assert.strictEqual(api.completionHoldRequests[0]!.runId, claim.run_id);
      assert.strictEqual(api.completionHoldRequests[0]!.body.head, CAPTURED_HEAD, "the captured head H rode the hold request");
      // A hold is non-terminal: no completed/failed report, and NO push/MR.
      assert.ok(!statuses(claim.run_id).includes("completed"), "a held run is not completed");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a held run is not failed");
      assert.strictEqual(calls.length, 0, "a held run opens no MR");
      // The destructive cleanup did NOT run: the clone and the HOME are preserved for resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1250)), true, "the runner clone is preserved");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the run HOME (SDK session) is preserved");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("capture retries across throw→false→true: holds on the third attempt, preserving the clone and HOME (bounded retry loop)", async () => {
    // The scripted verdict drives enterCompletionHold's bounded capture-retry loop: attempt 0 throws
    // (the try/catch retains and retries), attempt 1 returns false (unverified ⇒ retry), attempt 2
    // returns true (verified ⇒ capture, then request the hold). A "shrink attempts to 1" or "drop the
    // try/catch" mutant would abort on the throw/false attempts before ever reaching the true verdict,
    // so held.value would be false and no hold would be requested.
    const { gitlab, calls } = fakeGitlab();
    const { executor, held, captureAttempts } = makeHoldExecutor(["throw", false, true]);
    const home = seedHome();
    const claim = gitlabClaim(1253);
    try {
      api.setCompletionHoldResponse("paused", 200);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(captureAttempts.count, 3, "the bounded retry loop ran all three capture attempts (throw, false, true)");
      assert.strictEqual(held.value, true, "the third, verified attempt entered the hold");
      // The hold was requested exactly once, only after the retries produced a verified capture.
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold was requested exactly once, after the retries");
      assert.strictEqual(api.completionHoldRequests[0]!.runId, claim.run_id);
      assert.strictEqual(api.completionHoldRequests[0]!.body.head, CAPTURED_HEAD, "the captured head H rode the hold request");
      // A hold is non-terminal: no completed/failed report, and NO push/MR.
      assert.ok(!statuses(claim.run_id).includes("completed"), "a held run is not completed");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a held run is not failed");
      assert.strictEqual(calls.length, 0, "a held run opens no MR");
      // The destructive cleanup did NOT run: the clone and the HOME are preserved for resume.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1253)), true, "the runner clone is preserved");
      assert.strictEqual(fs.existsSync(home.sentinel), true, "the run HOME (SDK session) is preserved");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("refused hold ACK (non-paused) → false: does NOT park, and clears the preserve flags so the run's normal terminal cleanup removes the clone and HOME", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(true);
    const home = seedHome();
    const claim = gitlabClaim(1251);
    try {
      // The server refuses the hold: it answers running (a 409-style refusal reads its status off the
      // body too). enterCompletionHold must return false. The retain-everything flags are held ONLY
      // while the hold is being attempted; on a refusal it is NOT parked, so it clears them and the
      // failing run (the executor's legacy throw) follows normal terminal cleanup — otherwise a run
      // that attempts a hold, is kept live, then FAILS would leak its clone + HOME.
      api.setCompletionHoldResponse("running", 409);
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, false, "a non-paused ACK ⇒ enterCompletionHold returns false");
      assert.strictEqual(api.completionHoldRequests.length, 1, "the hold WAS requested (capture verified)");
      // Not parked ⇒ the flags were cleared ⇒ the terminal (failed) run cleaned up.
      assert.strictEqual(fs.existsSync(worktreeDirFor(1251)), false, "the clone is cleaned up on a refused ACK (not parked)");
      assert.strictEqual(fs.existsSync(home.sentinel), false, "the HOME is cleaned up on a refused ACK (not parked)");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });

  it("unverified capture → false: never requests the hold, and clears the preserve flags so the run's normal terminal cleanup removes the clone and HOME", async () => {
    const { gitlab } = fakeGitlab();
    const { executor, held } = makeHoldExecutor(false); // verifyRunnerTrackingCovers → false
    const home = seedHome();
    const claim = gitlabClaim(1252);
    try {
      api.setCompletionHoldResponse("paused", 200); // would grant, but must never be reached
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined, {
        recoveryRetryMs: 1,
      }).execute(claim);

      assert.strictEqual(held.value, false, "an unverified capture ⇒ enterCompletionHold returns false");
      assert.strictEqual(
        api.completionHoldRequests.length,
        0,
        "the hold is NEVER requested before a positively-verified capture",
      );
      // Giving up on an unverified capture is a NOT-parked outcome: the flags are cleared and the
      // failing run cleans up (the AC's no-clean-while-uncertain protection covers the in-flight
      // retries, not the terminal outcome once the bounded attempts are exhausted).
      assert.strictEqual(fs.existsSync(worktreeDirFor(1252)), false, "the clone is cleaned up on an unverified capture (not parked)");
      assert.strictEqual(fs.existsSync(home.sentinel), false, "the HOME is cleaned up on an unverified capture (not parked)");
    } finally {
      fs.rmSync(home.root, { recursive: true, force: true });
    }
  });
});

const exactHeld = {
  status: "paused", claim_generation: 7, hold_reason: "completion_blocked", claim_released: false,
};
const exactTerminal = {
  status: "failed", claim_generation: 7, hold_reason: null, claim_released: false,
};

/** Drive the public execute seam and observe git publication independently of forge traffic. */
async function reconcileCase(
  t: TestContext,
  configure: (runId: string) => void,
  retained = true,
  budget: number | null = 5_000,
): Promise<void> {
  const forge = fakeGitlab();
  let reconciliationReads: string[] = [];
  const probe = makeHoldExecutor(true, () => { reconciliationReads = [...api.ownershipRequests]; });
  const home = seedHome();
  const claim = { ...gitlabClaim(2816), claim_generation: 7 };
  const push = t.mock.method(git, "pushBranch", async () => {
    throw new Error("unexpected git push");
  });
  configure(claim.run_id);
  try {
    await runnerWith(() => ({ executor: probe.executor, homeDir: home.homeDir }), forge.gitlab,
      undefined, undefined, {
        recoveryRetryMs: 1,
        ...(budget === null ? {} : { completionHoldReconcileTimeoutMs: budget }),
      }).execute(claim);
    assert.equal(probe.held.value, retained);
    assert.deepEqual(reconciliationReads, [claim.run_id], "exactly one reconciliation read");
    if (retained) assert.deepEqual(api.ownershipRequests, [claim.run_id]);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(api.completionHoldRequests[0]!.body.head, CAPTURED_HEAD);
    assert.equal(push.mock.callCount(), 0, "no git push");
    assert.deepEqual(forge.all, [], "no forge reads or writes");
    if (retained)
      assert.ok(!statuses(claim.run_id).some((s) => ["completed", "failed", "cancelled"].includes(s)));
    assert.equal(fs.existsSync(worktreeDirFor(2816)), retained, "clone custody");
    assert.equal(fs.existsSync(home.sentinel), retained, "HOME custody");
  } finally {
    fs.rmSync(home.root, { recursive: true, force: true });
  }
}

describe("completion hold ambiguous wire acknowledgements", () => {
  const acknowledgements = [
    "transport lost", "HTTP timeout", "failed body read", "empty body", "malformed JSON",
    "missing status", "empty status", "unknown status", "unmodelled HTTP response",
  ];
  for (const ack of acknowledgements) {
    it(`commits exact completion hold before ${ack}`, async (t) => {
      let bodyReadFailed = false;
      const fetchReal = globalThis.fetch;
      if (ack === "failed body read") {
        t.mock.method(globalThis, "fetch", async (...args: Parameters<typeof fetch>) => {
          const response = await fetchReal(...args);
          if (String(args[0]).endsWith("/completion/hold")) {
            // Receipt and commit happen on the real server; only the body reader fails.
            t.mock.method(response, "text", async () => {
              bodyReadFailed = true;
              throw new Error("injected body read failure");
            });
          }
          return response;
        });
      }
      await reconcileCase(t, (runId) => {
        api.completionHoldWire.set(runId, (res) => {
          api.committedHoldOwnership.set(runId, { ...exactHeld });
          if (ack === "transport lost") { res.destroy(); return; }
          if (ack === "HTTP timeout") return; // client abort bounds this open request
          if (ack === "unmodelled HTTP response") { res.writeHead(204); res.end(); return; }
          const raw = ack === "malformed JSON" ? "{" :
            ack === "missing status" ? '{"run":{}}' :
            ack === "empty status" ? '{"run":{"status":""}}' :
            ack === "unknown status" ? '{"run":{"status":"unrecognised"}}' :
            ack === "failed body read" ? '{"run":{"status":"paused"}}' : "";
          res.writeHead(200, { "Content-Type": "application/json" });
          res.end(raw);
        });
      });
      assert.equal(bodyReadFailed, ack === "failed body read");
      assert.deepEqual([...api.committedHoldOwnership.values()], [exactHeld]);
    });
  }
});

describe("completion hold ownership proof fields", () => {
  const cases: Array<{ name: string; body: unknown; retained?: boolean; httpStatus?: number }> = [
    ...["completed", "failed", "cancelled"].map((status) => ({
      name: `exact unheld ${status} permits cleanup`, body: { ...exactTerminal, status }, retained: false,
    })),
    { name: "exact completion hold retains", body: exactHeld },
    ...["running", "paused", "pool_wait", "awaiting_approval", "awaiting_input", "awaiting_followup", "limit_wait", "recovery_wait", "unknown"].map((status) => ({
      name: `status ${status} cannot prove refusal`, body: { ...exactTerminal, status },
    })),
    ...["user_pause", "budget_exhausted", "provider_unavailable", "recovery"].map((hold_reason) => ({
      name: `other pause reason ${hold_reason}`, body: { ...exactHeld, hold_reason },
    })),
    ...[0, -1, 0.5, Number.MAX_SAFE_INTEGER + 1, "7", undefined, 6, 8].map((claim_generation) => ({
      name: `terminal generation ${String(claim_generation)}`, body: { ...exactTerminal, claim_generation },
    })),
    ...[true, null, undefined, "false"].map((claim_released) => ({
      name: `terminal release ${String(claim_released)}`, body: { ...exactTerminal, claim_released },
    })),
    ...["", undefined, "completion_blocked"].map((hold_reason) => ({
      name: `terminal hold reason ${String(hold_reason)}`, body: { ...exactTerminal, hold_reason },
    })),
    { name: "null ownership", body: null },
    { name: "nonobject ownership", body: "failed" },
    ...[404, 503].map((httpStatus) => ({ name: `ownership HTTP ${httpStatus}`, body: {}, httpStatus })),
  ];
  for (const c of cases) {
    it(c.name, async (t) => {
      await reconcileCase(t, (runId) => {
        api.completionHoldWire.set(runId, (res) => { res.writeHead(200); res.end(""); });
        api.ownershipWire.set(runId, (res) => {
          res.writeHead(c.httpStatus ?? 200, { "Content-Type": "application/json" });
          res.end(JSON.stringify(c.body));
        });
      }, c.retained ?? true);
    });
  }

  it("running read cannot fence a POST applied only after that read", async (t) => {
    const applied = deferred();
    let applyPendingPost: (() => void) | undefined;
    await reconcileCase(t, (runId) => {
      api.committedHoldOwnership.set(runId, { ...exactTerminal, status: "running" });
      api.completionHoldWire.set(runId, () => {
        // The client times out while the server keeps the original request pending.
        applyPendingPost = () => {
          api.committedHoldOwnership.set(runId, { ...exactHeld });
          applied.resolve();
        };
      });
      api.ownershipWire.set(runId, (res) => {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify(api.committedHoldOwnership.get(runId)), () => {
          assert.ok(applyPendingPost, "the original POST is still pending");
          applyPendingPost();
        });
      });
    });
    await applied.promise;
    assert.deepEqual([...api.committedHoldOwnership.values()], [exactHeld]);
  });

  it("uses the 5000ms production reconciliation budget without waiting for expiry", async (t) => {
    const read = client.getRunOwnership.bind(client);
    let observedBudget = 0;
    t.mock.method(client, "getRunOwnership", async (runId: string, options?: AbortSignal | { signal?: AbortSignal; deadline: number }) => {
      assert.ok(options && !(options instanceof AbortSignal));
      observedBudget = options.deadline - Date.now();
      return read(runId, options);
    });
    await reconcileCase(t, (runId) => {
      api.setCompletionHoldResponse("", 200);
      api.committedHoldOwnership.set(runId, exactHeld);
    }, true, null);
    assert.ok(observedBudget > 4900 && observedBudget <= 5000, `budget=${observedBudget}`);
  });

  for (const budget of [0, -1, NaN, Infinity, -Infinity]) {
    it(`rejects invalid reconciliation budget ${budget}`, () => {
      const { gitlab } = fakeGitlab();
      assert.throws(() => runnerWith(() => ({ executor: makeHoldExecutor(true).executor }), gitlab,
        undefined, undefined, { completionHoldReconcileTimeoutMs: budget }),
      /completionHoldReconcileTimeoutMs must be positive and finite/);
    });
  }
});

describe("completion hold streamed ownership bounds", () => {
  for (const overflow of [false, true]) {
    it(overflow ? "cancels open body overflow before deadline abort" : "bounds a stalled ownership body", async (t) => {
      const events: string[] = [];
      const fetchReal = globalThis.fetch;
      let signal: AbortSignal | undefined;
      let readStarted = 0;
      let readDuration = 0;
      let readError: unknown;
      let abortedAtRejection: boolean | undefined;
      let liveAtStall: boolean | undefined;
      t.mock.method(globalThis, "fetch", async (...args: Parameters<typeof fetch>) => {
        const ownership = /\/runs\/([^/]+)\/ownership$/.exec(String(args[0]));
        if (!ownership) return fetchReal(...args);
        // Answer without a real round trip: awaiting the fake API first let a loaded host spend
        // the whole budget before the stalled body existed, so there was nothing to cancel.
        api.ownershipRequests.push(ownership[1]!);
        signal = args[1]?.signal ?? undefined;
        liveAtStall = signal !== undefined && !signal.aborted;
        signal?.addEventListener("abort", () => { events.push("abort"); }, { once: true });
        readStarted = performance.now();
        return new Response(new ReadableStream<Uint8Array>({
          start(controller) {
            controller.enqueue(new TextEncoder().encode('{"status":'));
            if (overflow) controller.enqueue(new Uint8Array(16 * 1024 + 1));
            // Deliberately never close: only cancellation can release this body.
          },
          cancel() { events.push("cancel"); },
        }), { status: 200 });
      });
      const read = client.getRunOwnership.bind(client);
      t.mock.method(client, "getRunOwnership", async (runId: string, options?: AbortSignal | { signal?: AbortSignal; deadline: number }) => {
        try { return await read(runId, options); }
        catch (error) {
          readDuration = performance.now() - readStarted;
          events.push("reject");
          readError = error;
          abortedAtRejection = signal?.aborted;
          throw error;
        }
      });
      await reconcileCase(t, (runId) => {
        api.setCompletionHoldResponse("", 200);
        api.committedHoldOwnership.set(runId, exactHeld);
      }, true, overflow ? 5_000 : 1_000);
      assert.equal(liveAtStall, true, "the stalled body exists while the deadline is live");
      assert.ok(events.includes("cancel"));
      assert.ok(events.indexOf("cancel") < events.indexOf("reject"), "cancel is requested before rejection");
      if (overflow) {
        assert.match(String(readError), /exceeds 16384 bytes/);
        assert.equal(abortedAtRejection, false, "overflow rejection precedes deadline");
        assert.ok(!events.slice(0, events.indexOf("reject")).includes("abort"));
      }
      else assert.ok(events.indexOf("abort") < events.indexOf("reject"));
      // Scheduling margin over the 1 s stall budget; overflow rejects long before its deadline.
      assert.ok(readDuration < (overflow ? 1_000 : 5_000), `bounded read took ${readDuration}ms`);
      t.diagnostic(`ownership ${overflow ? "overflow" : "stall"} duration_ms=${readDuration.toFixed(2)} events=${events.join(",")}`);
    });
  }
});

for (const route of ["POST", "ownership probe"] as const) {
  it(`first cancelled completion hold ${route} stops immediately with one budget`, async () => {
    const claim = { ...gitlabClaim(2829), claim_generation: 7 };
    let factories = 0;
    let stopped = false;
    let context: RunContext | undefined;
    const probe = makeHoldExecutor(true, () => {
      stopped = context!.signal!.aborted;
      assert.equal(factories, 1);
    });
    const run = probe.executor.run.bind(probe.executor);
    probe.executor.run = async (ctx) => { context = ctx; return run(ctx); };
    if (route === "POST") api.setCompletionHoldResponse("cancelled", 200);
    else {
      api.completionHoldWire.set(claim.run_id, res => { res.writeHead(200); res.end(""); });
      api.committedHoldOwnership.set(claim.run_id, { ...exactTerminal, status: "cancelled" });
    }
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor: probe.executor }), forge.gitlab, undefined, undefined, {
      cancellationBudgetFactory: () => { factories++; return new MaintenanceBudget(); },
    }).execute(claim);
    assert.equal(stopped, true, "model is stopped before the hold caller resumes");
    assert.equal(probe.held.value, false);
    assert.equal(factories, 1, "later terminal handling must reuse the first budget");
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });
}

describe("3282 completion hold shutdown scope", () => {
  for (const codex of [true, false]) for (const verified of [true, false]) {
    for (const allowDegradedCapture of [false, true]) {
      it(`3282 ${codex ? "Codex" : "Claude"} shutdown during ${verified ? "verified" : "unverified"} capture with ${allowDegradedCapture ? "degraded" : "normal"} options preserves authority`, async t => {
        const issue = 328210 + Number(codex) * 10 + Number(verified) * 2 + Number(allowDegradedCapture);
        const claim = { ...gitlabClaim(issue), claim_generation: 7 };
        const home = seedHome();
        const rig = codexRig();
        const { gitlab, calls } = fakeGitlab();
        let runner: ReturnType<typeof runnerWith>;
        let attempts = 0, held: boolean | undefined;
        let holdError: unknown;
        const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected publication"); });
        api.setCompletionHoldResponse("paused", 200);
        const behavior: Executor["run"] = async ctx => {
          git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
          mockCommittedTracking(git, CAPTURED_HEAD);
          git.verifyRunnerTrackingCovers = (async () => {
            attempts++;
            // Flip shutdown inside the in-flight capture, after the loop's admission check.
            runner.shutdown();
            return verified;
          }) as typeof git.verifyRunnerTrackingCovers;
          git.trackingTip = (async () => CAPTURED_HEAD) as typeof git.trackingTip;
          git.checkpointPack = (async () => null) as typeof git.checkpointPack;
          try {
            held = await ctx.enterCompletionHold!("shutdown scope", { allowDegradedCapture });
          } catch (error) {
            holdError = error;
            throw error;
          }
          return {
            branch: ctx.branch,
            ...(held ? { completionHeld: { reason: "shutdown scope" } } : { walled: { reason: "shutdown scope" } }),
          };
        };
        const executor: Executor = codex ? new FakeCodexExecutor(rig.safety, behavior) : { run: behavior };
        runner = runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined,
          { recoveryRetryMs: 1 });
        const expectedHold = verified || allowDegradedCapture;
        try {
          await runner.execute(claim);
          if (codex) {
            // B refuses ordinary source/report work after authority revocation. The
            // typed graceful-shutdown checkpoint has its separate #3109 contract.
            assert.equal(held, undefined);
            assert.ok(holdError, "revoked capture unwinds before reporting a hold");
            assert.equal(attempts, 1);
            assert.equal(api.completionHoldRequests.length, 0);
            assert.ok(fs.existsSync(worktreeDirFor(issue)), "unverified source is retained");
            assert.ok(fs.existsSync(home.sentinel), "session is retained");
            assert.equal(rig.boundaries.includes("finalize"), false);
          } else {
            assert.equal(held, expectedHold, "shutdown preserves the ordinary hold boolean rather than throwing for requeue");
            assert.equal(holdError, undefined, "the sink does not throw DataVolumeWaitShutdown");
            assert.equal(attempts, 1, "shutdown prevents another capture attempt");
            assert.equal(api.completionHoldRequests.length, Number(expectedHold));
            if (expectedHold) {
              assert.equal(api.completionHoldRequests[0]!.runId, claim.run_id);
              assert.equal(api.completionHoldRequests[0]!.body.head, verified ? CAPTURED_HEAD : "");
            }
            assert.equal(fs.existsSync(worktreeDirFor(issue)), expectedHold, "only a hold retains the clone");
            assert.equal(fs.existsSync(home.sentinel), expectedHold, "ordinary false clears provisional HOME retention");
            assert.equal(rig.boundaries.includes("finalize"), false, "Claude retains the legacy facade branch");
          }
          assert.equal(api.completionPermitRequests.length, 0);
          assert.equal(push.mock.callCount(), 0);
          assert.equal(calls.length, 0);
          assert.equal(statuses(claim.run_id).some(s => ["completed", "failed", "cancelled"].includes(s)), false);
        } finally { fs.rmSync(home.root, { recursive: true, force: true }); }
      });
    }
  }
});

describe("3282 degraded wall completion hold", () => {
  for (const status of ["paused", "running", "queued", "claimed", "completed", "failed", "cancelled", ""] as const) it(`3282 empty-head capture exhaustion with ${status || "ambiguous"} ACK`, async t => {
    const home = seedHome();
    const claim = { ...gitlabClaim(328203), claim_generation: 7 };
    const { gitlab, calls } = fakeGitlab();
    let attempts = 0, held: boolean | undefined;
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected publication"); });
    api.setCompletionHoldResponse(status, ["running", "queued", "claimed"].includes(status) ? 409 : 200);
    api.parkRunOwnership = { status: "running", claim_generation: 7, hold_reason: null, claim_released: false };
    const exec: Executor = { run: async ctx => {
      fs.writeFileSync(path.join(ctx.worktreePath, "wall-only.txt"), "not captured anywhere\n");
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
      mockCommittedTracking(git, CAPTURED_HEAD);
      git.verifyRunnerTrackingCovers = (async () => { attempts++; return false; }) as typeof git.verifyRunnerTrackingCovers;
      held = await ctx.enterCompletionHold!("wall", { allowDegradedCapture: true });
      return { branch: ctx.branch, ...(held ? { completionHeld: { reason: "wall" } } : { walled: { reason: "wall" } }) };
    } };
    try {
      await runnerWith(() => ({ executor: exec, homeDir: home.homeDir }), gitlab, undefined, undefined,
        { recoveryRetryMs: 1 }).execute(claim);
      assert.equal(attempts, 3);
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(api.completionHoldRequests[0]!.body.head, "");
      const keep = !["completed", "failed", "cancelled"].includes(status);
      assert.equal(held, status === "paused" || status === "");
      assert.equal(fs.existsSync(worktreeDirFor(328203)), keep, "live refusals retain source; terminal refusals keep existing cleanup");
      assert.equal(fs.existsSync(home.sentinel), keep, "session retention follows the same disposition");
      if (keep) assert.equal(fs.readFileSync(path.join(worktreeDirFor(328203), "wall-only.txt"), "utf8"), "not captured anywhere\n");
      assert.equal(push.mock.callCount(), 0); assert.equal(calls.length, 0);
      assert.equal(statuses(claim.run_id).some(s => ["completed", "failed", "cancelled"].includes(s)), false);
    } finally { fs.rmSync(home.root, { recursive: true, force: true }); }
  });

  it("3282 declined Codex hold bypasses real FINALIZE boundary and credential reconcile", async t => {
    const claim = { ...gitlabClaim(328204), claim_generation: 7 };
    const home = seedHome();
    const rig = codexRig();
    const { gitlab, calls } = fakeGitlab();
    let afterHoldRefresh = 0, afterHoldRelease = 0;
    api.setCompletionHoldResponse("running", 409);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected publication"); });
    const executor = new FakeCodexExecutor(rig.safety, async ctx => {
      git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
      mockCommittedTracking(git, CAPTURED_HEAD);
      git.verifyRunnerTrackingCovers = (async () => false) as typeof git.verifyRunnerTrackingCovers;
      const held = await ctx.enterCompletionHold!("wall", { allowDegradedCapture: true });
      assert.equal(held, false);
      afterHoldRefresh = rig.refreshCalls(); afterHoldRelease = rig.releaseCalls();
      return { branch: ctx.branch, walled: { reason: "wall" } };
    });
    try {
      await runnerWith(() => ({ executor, homeDir: home.homeDir }), gitlab, undefined, undefined,
        { recoveryRetryMs: 1 }).execute(claim);
      assert.equal(rig.boundaries.includes("finalize"), false);
      assert.equal(rig.refreshCalls(), afterHoldRefresh);
      assert.equal(rig.releaseCalls(), afterHoldRelease);
      assert.equal(push.mock.callCount(), 0); assert.equal(calls.length, 0);
      assert.equal(api.completionPermitRequests.length, 0);
      assert.equal(statuses(claim.run_id).some(s => ["completed", "failed", "cancelled"].includes(s)), false);
      assert.equal(fs.existsSync(worktreeDirFor(328204)), true);
      assert.equal(fs.existsSync(home.sentinel), true);
    } finally { fs.rmSync(home.root, { recursive: true, force: true }); }
  });

  for (const boundary of ["admission", "capture", "retry", "before POST"] as const) it(`3282 cancel at ${boundary} never posts a completion hold`, async () => {
    const claim = { ...gitlabClaim(328205), claim_generation: 7 };
    const { gitlab } = fakeGitlab();
    let attempts = 0;
    const ready = deferred(), release = deferred();
    let runner: ReturnType<typeof runnerWith>;
    const cancel = () => {
      const active = (runner as unknown as { activeRuns: Map<string, { steering: SteeringChannel }> }).activeRuns.get(claim.run_id)!;
      active.steering.markCancelled();
    };
    const exec: Executor = {
      closeAndSettlePlanSummary: async () => {
        if (boundary === "admission") { ready.resolve(); await release.promise; }
      },
      run: async ctx => {
        git.worktreeStatus = (async () => []) as typeof git.worktreeStatus;
        mockCommittedTracking(git, CAPTURED_HEAD);
        git.verifyRunnerTrackingCovers = (async () => {
          attempts++;
          if (boundary !== "admission" && (boundary !== "retry" || attempts === 2)) {
            ready.resolve(); await release.promise;
          }
          return boundary === "before POST";
        }) as typeof git.verifyRunnerTrackingCovers;
        await ctx.enterCompletionHold!("wall", { allowDegradedCapture: true });
        throw new Error("hold should propagate cancellation");
      },
    };
    runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, undefined, { recoveryRetryMs: 1 });
    const running = runner.execute(claim);
    try {
      let watchdog: NodeJS.Timeout | undefined;
      try {
        await Promise.race([ready.promise, running.then(() => { throw new Error("run ended before barrier"); }),
          new Promise<never>((_, reject) => { watchdog = setTimeout(() => reject(new Error("3282 cancellation barrier timed out")), 2000); })]);
      } finally { clearTimeout(watchdog); }
      cancel(); release.resolve();
      await running;
      assert.equal(api.completionHoldRequests.length, 0, "cancel known before POST must not request a hold");
      const terminal = api.states.filter(s => s.runId === claim.run_id).at(-1)!.body;
      assert.equal(terminal.status, "failed");
      assert.equal(terminal.failure_reason, "run cancelled", "API maps this wire report to cancellation");
      assert.equal(attempts, boundary === "admission" ? 0 : boundary === "retry" ? 2 : 1);
    } finally { release.resolve(); await running; }
  });
});
