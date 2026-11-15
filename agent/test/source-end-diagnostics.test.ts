import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";
import { fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { nullLogger } from "./helpers.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { acquireWorkerLifetime } from "../src/authority-lock.js";
import { makeTmp, testLockOptions } from "./authority-test-support.js";
import path from "node:path";

installHarness();

type Subject = {
  authority?: object;
  sourceEndDiagnosticState(flight: object): Record<string, unknown>;
  emitSourceEndDiagnostics(flight: object, log: Logger): void;
  endFlightSourceUse(flight: object, log: Logger): Promise<{ endedSlots: unknown[] }>;
};
function fixture(options: { held?: boolean; barrier?: boolean; throwing?: boolean;
  foreign?: boolean; noLease?: boolean; kept?: boolean; tails?: boolean; reasons?: string[] } = {}) {
  const subject = Object.create(RunRunner.prototype) as Subject;
  let ends = 0, declines = 0;
  subject.authority = {
    retireSink() {},
    awaitSinkQuiescent: async () => options.barrier !== false,
    ownedSourceEpoch: () => "private-epoch",
    markSourceEndDeclined() { declines++; },
    heldByOtherSink: () => options.foreign === true,
    leaseFor: () => options.noLease ? undefined : {},
    endSourceUse: async () => { ends++; return { kept: options.kept ? ["/private/source-secret"] : [], endedSlots: [], reasons: options.reasons }; },
  };
  // Deliberately leave the remediation latch without pending evidence: the real seam
  // must retain it without trying publication or endSourceUse.
  Object.assign(subject, { inFlightAuthority: async (_flight: unknown, action: () => Promise<void>) => action(),
    cleanupAuthorityWait: () => ({ timeoutMs: 1 }) });
  const flight = { worktreePath: "/private/source-secret", sourceUsePaths: new Set(["/private/source-secret"]),
    sourceTailsJoined: options.tails !== false, ...(options.held ? { remediationRestrictions: { roots: ["private-root"] } } : {}) };
  const emitted: Array<{ message: string; fields?: Record<string, unknown> }> = [];
  const log = { warn(message: string, fields?: Record<string, unknown>) {
    emitted.push({ message, fields });
    if (options.throwing) throw new Error("private-logger-error");
  } } as Logger;
  return { subject, flight, log, emitted, counts: () => ({ ends, declines }) };
}

describe("bounded source-end diagnostics", () => {
  it("copies only fixed reasons without guessing a retry from kept", async () => {
    for (const reasons of [undefined, ["no_lease"], ["pending_operation", "queued_retry", "queued_retry", "/private/raw-error"]]) {
      const f = fixture({ kept: true, reasons });
      await f.subject.endFlightSourceUse(f.flight, f.log);
      const summary = f.emitted.find(entry => entry.message === "source use teardown summary")!;
      assert.deepEqual(summary.fields?.reasons, reasons?.includes("queued_retry")
        ? ["queued_retry", "pending_operation"] : reasons ?? []);
      assert.equal(JSON.stringify(f.emitted).includes("/private"), false);
    }
  });

  for (const mismatch of [false, true]) {
    it(`publication refusal stays private with throwing logger (mismatch=${mismatch})`, async t => {
      const root = makeTmp(t);
      const lifetime = await acquireWorkerLifetime(root, testLockOptions());
      const authority = new AuthorityRuntime({ lifetime, workerToken: "diagnostic-test-token" });
      const clone = path.join(root, "private-source");
      const lease = await authority.bindSource(clone);
      const f = fixture({ throwing: true });
      Object.assign(f.subject.authority!, { leaseFor: () => mismatch ? undefined : lease });
      Object.assign(f.flight, {
        worktreePath: clone, sourceUsePaths: new Set([clone]), sourceLease: lease,
        executor: {}, sourceJoinFailed: !mismatch,
        completionSourceBound: { clonePath: clone },
      });
      await f.subject.endFlightSourceUse(f.flight, f.log);
      const summary = f.emitted.find(entry => entry.message === "source use teardown summary")!;
      assert.equal(summary.fields?.publication, "unpublished");
      assert.equal(summary.fields?.publication_epoch_mismatch, mismatch);
      assert.equal(f.counts().ends, 1);
      f.subject.emitSourceEndDiagnostics(f.flight, f.log);
      assert.equal(f.emitted.filter(entry => entry.message === "source use teardown summary").length, 1);
      for (const secret of [root, clone, "private-logger-error"])
        assert.equal(JSON.stringify(f.emitted).includes(secret), false);
    });
  }

  for (const options of [
    { foreign: true, kept: true }, { noLease: true, kept: true },
    { barrier: false, kept: true }, { tails: false },
  ]) {
    it(`records actual source-end refusal branches privately: ${JSON.stringify(options)}`, async () => {
      const f = fixture(options);
      await f.subject.endFlightSourceUse(f.flight, f.log);
      const summaries = f.emitted.filter(entry => entry.message === "source use teardown summary");
      assert.equal(summaries.length, 1);
      const fields = summaries[0]!.fields!;
      assert.equal(fields.outcome, options.tails === false ? "tails_unjoined" : "unconfirmed");
      assert.equal(fields.foreign, options.foreign === true);
      assert.equal(fields.no_lease, options.noLease === true);
      assert.equal(fields.barrier, options.barrier === false ? "unconfirmed" : "joined");
      assert.equal(fields.kept, options.kept ? 1 : 0);
      assert.equal(f.counts().ends, options.tails === false ? 0 : 1);
      for (const secret of ["/private", "source-secret", "private-epoch"])
        assert.equal(JSON.stringify(f.emitted).includes(secret), false);
    });
  }

  it("reports the otherwise silent held remediation without ending source use", async () => {
    const f = fixture({ held: true });
    const result = await f.subject.endFlightSourceUse(f.flight, f.log);
    assert.deepEqual(result.endedSlots, []);
    assert.deepEqual(f.counts(), { ends: 0, declines: 1 });
    assert.equal(f.emitted.length, 1);
    assert.ok(f.emitted[0]);
    assert.equal(f.emitted[0].fields?.outcome, "remediation_held");
    assert.equal(f.emitted[0].fields?.remediation, "held");
  });

  it("keeps the whole emitted set finite and free of source identities and exceptions", async () => {
    const f = fixture({ held: true, barrier: false });
    await f.subject.endFlightSourceUse(f.flight, f.log);
    const serialized = JSON.stringify(f.emitted);
    for (const secret of ["/private", "source-secret", "private-root", "private-epoch", "private-logger-error"])
      assert.equal(serialized.includes(secret), false, secret);
    assert.ok(f.emitted.length <= 3);
    const summary = f.emitted.find(entry => entry.message === "source use teardown summary")!;
    assert.equal(summary.fields?.barrier, "unconfirmed");
    assert.deepEqual(summary.fields?.reasons, []);
    for (const [key, value] of Object.entries(summary.fields!)) {
      if (key === "reasons") continue;
      assert.ok(typeof value === "boolean" || typeof value === "number" || typeof value === "string");
      if (typeof value === "number") assert.ok(Number.isInteger(value) && value >= 0 && value <= 2048);
      if (typeof value === "string") assert.ok(new Set([
        "source_end", "skipped", "held", "unconfirmed", "not_attempted", "remediation_held",
      ]).has(value), value);
    }
  });

  it("a throwing logger never changes held disposition and is attempted once", async () => {
    const f = fixture({ held: true, throwing: true });
    await f.subject.endFlightSourceUse(f.flight, f.log);
    f.subject.emitSourceEndDiagnostics(f.flight, f.log);
    assert.deepEqual(f.counts(), { ends: 0, declines: 1 });
    assert.equal(f.emitted.length, 1);
  });

  it("the real outer teardown finalizer retains an early failure and attempts one bounded summary", async t => {
    const original = new Error("private-early-teardown-error");
    const emitted: Record<string, unknown>[] = [];
    const log = nullLogger();
    log.child = () => log;
    log.warn = (message, fields) => {
      if (message !== "source use teardown summary") return;
      emitted.push(fields!);
      throw new Error("private-logger-error");
    };
    const runner = runnerWith(() => ({ executor: { run: async () => ({ branch: "unused" }) } }),
      fakeGitlab().gitlab, undefined, log);
    type EarlyFlight = { steering: { stop(): Promise<void> }; batcher: { close(): Promise<void> } };
    const seam = runner as unknown as { reconcileWorkerMemory(flight: EarlyFlight): Promise<void> };
    let interrupted: EarlyFlight | undefined;
    // Test cleanup joins the poller that the injected early exit deliberately bypasses.
    // It does not emit diagnostics; only the production finalizer can satisfy the assertion.
    t.after(async () => {
      if (interrupted) {
        await interrupted.steering.stop();
        await interrupted.batcher.close();
      }
    });
    // Throw inside the actual terminal finally, before source end can emit its summary.
    t.mock.method(seam, "reconcileWorkerMemory", async (flight: EarlyFlight) => { interrupted = flight; throw original; });
    await assert.rejects(runner.execute(gitlabClaim(436500)), error => error === original);
    assert.equal(emitted.length, 1);
    assert.deepEqual(emitted[0], {
      stage: "teardown", terminal: "skipped", executor: false, process: false,
      commands: false, steering: false, auxiliaries: false, memory: false, park_reap: false,
      remediation: "none", barrier: "not_attempted", publication: "not_attempted",
      outcome: "early_exit", reasons: [], sources: 0, kept: 0, foreign: false, no_lease: false,
      publication_epoch_mismatch: false,
    });
  });

  it("an auxiliary stop failure is reported without escaping normal failure handling with a throwing logger", async t => {
    const original = new Error("private-auxiliary-error");
    const summaries: Record<string, unknown>[] = [];
    const log = nullLogger();
    log.child = () => log;
    log.warn = (message, fields) => {
      if (message === "source use teardown summary") {
        summaries.push(fields!);
        throw new Error("private-logger-error");
      }
    };
    const runner = runnerWith(() => ({ executor: { run: async () => ({ branch: "unused" }) } }),
      fakeGitlab().gitlab, undefined, log);
    const seam = runner as unknown as {
      startMidTurnTicker(...args: unknown[]): { stop(): Promise<void> };
    };
    let stops = 0;
    t.mock.method(seam, "startMidTurnTicker", () => ({
      stop: async () => { stops++; throw original; },
    }));
    // execute handles the phase failure through its normal failed-run path. The
    // teardown summary must still record the failed join even if logging throws.
    await runner.execute(gitlabClaim(436501));
    assert.ok(stops > 0, "the actual auxiliary settlement must run");
    assert.equal(summaries.length, 1);
    assert.equal(summaries[0]!.auxiliaries, false);
    assert.equal(summaries[0]!.outcome, "no_authority", "this auxiliary diagnostic fixture has no authority; source-end protection is covered by authority fixtures");
    for (const secret of ["private-auxiliary-error", "private-logger-error"])
      assert.equal(JSON.stringify(summaries).includes(secret), false);
  });

  it("source-end finalization preserves an original authority error when the logger throws", async () => {
    const f = fixture({ throwing: true });
    const original = new Error("private-authority-error");
    Object.assign(f.subject.authority!, { awaitSinkQuiescent: async () => { throw original; } });
    await assert.rejects(f.subject.endFlightSourceUse(f.flight, f.log), error => error === original);
    assert.equal(f.emitted.length, 1);
    assert.equal(JSON.stringify(f.emitted).includes("private-authority-error"), false);
    assert.deepEqual(f.counts(), { ends: 0, declines: 0 });
  });

  it("emits only once when both the source-end and outer teardown finalizers run", async () => {
    const f = fixture();
    await f.subject.endFlightSourceUse(f.flight, f.log);
    f.subject.emitSourceEndDiagnostics(f.flight, f.log);
    assert.equal(f.emitted.length, 1);
    assert.ok(f.emitted[0]);
    assert.equal(f.emitted[0].fields?.outcome, "ended");
  });
});
