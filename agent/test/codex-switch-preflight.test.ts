import { test } from "node:test";
import assert from "node:assert/strict";
import { coordinateCodexSwitchPreflight, type CodexSwitchOperation, type CodexSwitchAuthority, type CodexSwitchPreflightOptions } from "../src/codex-switch-preflight.js";
import type { CodexSwitchPreflightResult } from "../src/client.js";

const initial: CodexSwitchOperation = { claimGeneration: 1, registerNonce: "nonce", opId: "op1", targetSecretId: "target", revision: 1 };
function fixture() {
  let time = 0;
  let probes = 0;
  let clears = 0;
  const waits: number[] = [];
  const controller = new AbortController();
  const authority: CodexSwitchAuthority = { owned: true, ...initial, routeAvailable: true, ledgerAvailable: true, pending: { ...initial } };
  const options: CodexSwitchPreflightOptions = {
    initial, signal: controller.signal, now: () => time,
    delay: async (ms, b) => { assert.equal(b.deadline, 10000); waits.push(ms); time += ms; },
    reconcile: async () => authority,
    probe: async () => ({ kind: "ready", expectedEpoch: 11, ...initial }),
    confirmFailedClear: async () => { clears++; return true; },
  };
  const run = () => {
    const probe = options.probe;
    options.probe = async (op, bounds) => { probes++; assert.equal(bounds.deadline, 10000); return probe(op, bounds); };
    return coordinateCodexSwitchPreflight(options);
  };
  return { options, authority, controller, waits, run, advance: (ms: number) => { time += ms; },
    counts: () => ({ probes, clears, time }) };
}
const refusal = (reason: string): CodexSwitchPreflightResult => ({ kind: "not_applied", reason, ...initial });

for (const reason of ["policy_refusal", "vault_locked", "baseline_missing", "baseline_mismatch", "target_moved", "target_mismatch",
  "credential_disabled", "credential_rejected", "credential_unavailable", "credential_changed", "credential_gone",
  "credential_not_decryptable"]) {
  test("exact typed refusal clears: " + reason, async () => {
    const f = fixture();
    f.options.probe = async () => refusal(reason);
    assert.deepEqual(await f.run(), { kind: "continued_A", operation: initial, reason });
    assert.deepEqual(f.counts(), { probes: 1, clears: 1, time: 0 });
  });
}
test("ready returns current target without clearing", async () => {
  const f = fixture();
  assert.deepEqual(await f.run(), { kind: "ready", expectedEpoch: 11, operation: initial });
  assert.equal(f.counts().clears, 0);
});
for (const field of ["claimGeneration", "registerNonce", "opId"] as const) {
  test("refusal must match " + field, async () => {
    const f = fixture();
    f.options.probe = async () => ({ ...refusal("policy_refusal"), [field]: field === "claimGeneration" ? 2 : "other" });
    assert.equal((await f.run()).kind, "retained_stop");
    assert.equal(f.counts().clears, 0);
  });
}
for (const field of ["routeAvailable", "ledgerAvailable"] as const) {
  test(field + " absent stops before probe", async () => {
    const f = fixture(); f.authority[field] = false;
    assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "unavailable" });
    assert.equal(f.counts().probes, 0);
  });
}
test("six busy observations clear after exact backoffs", async () => {
  const f = fixture(); f.options.probe = async () => refusal("credential_busy");
  assert.equal((await f.run()).kind, "continued_A");
  assert.deepEqual(f.waits, [250, 500, 1000, 2000, 4000]);
  assert.deepEqual(f.counts(), { probes: 6, clears: 1, time: 7750 });
});
test("contention can clear to ready", async () => {
  const f = fixture(); let n = 0;
  f.options.probe = async () => ++n < 3 ? refusal("credential_busy") : { kind: "ready", expectedEpoch: 11, ...initial };
  assert.equal((await f.run()).kind, "ready");
  assert.deepEqual(f.waits, [250, 500]); assert.equal(f.counts().clears, 0);
});
test("three ambiguous probes reconcile and stop; no text escapes", async () => {
  const f = fixture(); let reads = 0;
  f.options.reconcile = async () => { reads++; return f.authority; };
  f.options.probe = async () => { throw new Error("sensitive arbitrary text"); };
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "uncertain" });
  assert.deepEqual(f.waits, [250, 500]);
  assert.deepEqual(f.counts(), { probes: 3, clears: 0, time: 750 });
  assert.equal(reads, 6);
});
test("failed reconciliation prevents ambiguous retry", async () => {
  const f = fixture();
  f.options.probe = async () => { f.authority.owned = false; return { kind: "uncertain", detail: "anything" }; };
  assert.deepEqual(await f.run(), { kind: "fenced" }); assert.equal(f.counts().probes, 1);
});
test("one deadline includes requests and capped waits", async () => {
  const f = fixture();
  f.options.probe = async () => { f.advance(2000); return refusal("credential_busy"); };
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "deadline" });
  assert.deepEqual(f.waits, [250, 500, 1000, 250]);
  assert.deepEqual(f.counts(), { probes: 4, clears: 0, time: 10000 });
});
test("sixth busy arriving at deadline cannot clear", async () => {
  const f = fixture(); let n = 0;
  f.options.probe = async () => { if (++n === 6) f.advance(2250); return refusal("credential_busy"); };
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "deadline" });
  assert.equal(f.counts().clears, 0);
});
for (const phase of ["before", "delay", "probe"]) {
  test("cancellation interrupts " + phase, async () => {
    const f = fixture();
    if (phase === "before") f.controller.abort();
    if (phase === "delay") {
      f.options.probe = async () => refusal("credential_busy");
      f.options.delay = async () => { f.controller.abort(); return new Promise(() => {}); };
    }
    if (phase === "probe") f.options.probe = async () => { f.controller.abort(); return new Promise(() => {}); };
    assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "cancelled" });
    assert.equal(f.counts().clears, 0);
  });
}
test("stale nonce is fenced before probing", async () => {
  const f = fixture(); f.authority.registerNonce = "new";
  assert.deepEqual(await f.run(), { kind: "fenced" }); assert.equal(f.counts().probes, 0);
});
test("supersession follows authoritative newer op through preflight", async () => {
  const f = fixture(); let n = 0;
  const newer = { ...initial, opId: "op2", targetSecretId: "target2", revision: 2 };
  f.options.probe = async op => {
    if (++n === 1) { f.authority.pending = newer; return { kind: "superseded", ...newer }; }
    assert.deepEqual(op, newer); return { kind: "ready", expectedEpoch: 11, ...newer };
  };
  assert.deepEqual(await f.run(), { kind: "ready", expectedEpoch: 11, operation: newer }); assert.equal(f.counts().clears, 0);
});
test("superseded without valid newer pending stops", async () => {
  const f = fixture(); f.options.probe = async () => ({ kind: "superseded", ...initial });
  assert.equal((await f.run()).kind, "retained_stop");
});
test("infinite supersession with frozen clock has finite cap", async () => {
  const f = fixture(); let n = 1;
  f.options.probe = async () => {
    const next = { ...initial, opId: "op" + ++n, revision: n };
    f.authority.pending = next; return { kind: "superseded", ...next };
  };
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "safety_limit" });
  assert.equal(f.counts().probes, 32);
});
test("new op cannot inherit five busy observations", async () => {
  const f = fixture(); let n = 0;
  const newer = { ...initial, opId: "op2", revision: 2 };
  f.options.probe = async op => {
    n++;
    if (n === 6) { f.authority.pending = newer; return { kind: "superseded", ...newer }; }
    if (n === 8) return { kind: "ready", expectedEpoch: 11, ...op };
    return { kind: "not_applied", reason: "credential_busy", ...op };
  };
  assert.equal((await f.run()).kind, "ready");
  assert.equal(f.counts().clears, 0);
  assert.deepEqual(f.waits, [250, 500, 1000, 2000, 4000, 250]);
});
test("uncertain final busy attempt never clears", async () => {
  const f = fixture(); let n = 0;
  f.options.probe = async () => ++n < 6 ? refusal("credential_busy") : { kind: "uncertain", detail: "timeout" };
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "uncertain" });
  assert.equal(f.counts().clears, 0);
});
test("typed fenced always stops without clear", async () => {
  const f = fixture(); f.options.probe = async () => ({ kind: "fenced", ...initial });
  assert.deepEqual(await f.run(), { kind: "fenced" });
  assert.equal(f.counts().clears, 0);
});
test("clear must be confirmed", async () => {
  const f = fixture(); f.options.probe = async () => refusal("vault_locked");
  f.options.confirmFailedClear = async () => false;
  assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "clear_unconfirmed" });
});
for (const reason of ["generation_mismatch", "no_pending_switch"]) {
  test("stale or missing op never clears or continues A: " + reason, async () => {
    const f = fixture();
    f.options.probe = async () => refusal(reason);
    const result = await coordinateCodexSwitchPreflight(f.options);
    assert.equal(result.kind, reason === "generation_mismatch" ? "fenced" : "retained_stop");
    assert.equal(f.counts().clears, 0);
  });
}

for (const result of [{ kind: "refused_committed", reason: "credential_changed", ...initial }, refusal("arbitrary secret text")] as CodexSwitchPreflightResult[]) {
  test("committed or unknown refusal retains A gate", async () => {
    const f = fixture(); f.options.probe = async () => result;
    assert.equal((await f.run()).kind, "retained_stop"); assert.equal(f.counts().clears, 0);
  });
}

for (const expectedEpoch of [undefined, 0, -1, 1.5, Number.MAX_SAFE_INTEGER + 1]) {
  test(`READY with invalid epoch ${expectedEpoch} retains A`, async () => {
    const f = fixture();
    f.options.probe = async () => ({ kind: "ready", expectedEpoch, ...initial } as CodexSwitchPreflightResult);
    assert.deepEqual(await f.run(), { kind: "retained_stop", reason: "uncertain" });
    assert.equal(f.counts().clears, 0);
  });
}
