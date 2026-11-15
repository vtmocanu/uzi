import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { RunRunner } from "../src/runner.js";
import type { Logger } from "../src/log.js";

type Subject = {
  authority?: object;
  sourceEndDiagnosticState(flight: object): Record<string, unknown>;
  emitSourceEndDiagnostics(flight: object, log: Logger): void;
  endFlightSourceUse(flight: object, log: Logger): Promise<{ endedSlots: unknown[] }>;
};
function fixture(options: { held?: boolean; barrier?: boolean; throwing?: boolean } = {}) {
  const subject = Object.create(RunRunner.prototype) as Subject;
  let ends = 0, declines = 0;
  subject.authority = {
    retireSink() {},
    awaitSinkQuiescent: async () => options.barrier !== false,
    ownedSourceEpoch: () => "private-epoch",
    markSourceEndDeclined() { declines++; },
    heldByOtherSink: () => false,
    leaseFor: () => ({}),
    endSourceUse: async () => { ends++; return { kept: [], endedSlots: [] }; },
  };
  // Deliberately leave the remediation latch without pending evidence: the real seam
  // must retain it without trying publication or endSourceUse.
  Object.assign(subject, { inFlightAuthority: async (_flight: unknown, action: () => Promise<void>) => action(),
    cleanupAuthorityWait: () => ({ timeoutMs: 1 }) });
  const flight = { worktreePath: "/private/source-secret", sourceUsePaths: new Set(["/private/source-secret"]),
    sourceTailsJoined: true, ...(options.held ? { remediationRestrictions: { roots: ["private-root"] } } : {}) };
  const emitted: Array<{ message: string; fields?: Record<string, unknown> }> = [];
  const log = { warn(message: string, fields?: Record<string, unknown>) {
    emitted.push({ message, fields });
    if (options.throwing) throw new Error("private-logger-error");
  } } as Logger;
  return { subject, flight, log, emitted, counts: () => ({ ends, declines }) };
}

describe("bounded source-end diagnostics", () => {
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
    for (const value of Object.values(summary.fields!)) {
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

  it("records an early teardown exit without replacing the original error", () => {
    const f = fixture({ throwing: true });
    const original = new Error("original");
    assert.throws(() => {
      try { throw original; }
      finally { f.subject.emitSourceEndDiagnostics(f.flight, f.log); }
    }, error => error === original);
    assert.ok(f.emitted[0]);
    assert.equal(f.emitted[0].fields?.stage, "teardown");
    assert.equal(f.emitted[0].fields?.outcome, "early_exit");
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
