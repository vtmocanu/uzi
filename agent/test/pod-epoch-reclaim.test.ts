import assert from "node:assert/strict";
import { test } from "node:test";
import http from "node:http";
import { mkdtemp, mkdir, open, readFile, readdir, rm, symlink, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { resolve, join } from "node:path";
import { runPodEpochShadow, readPodEpochProjection, PodEpochSettlementError } from "../src/pod-epoch-reclaim.js";
import { PodEpochDaemonError } from "../src/pod-epoch-daemon.js";
import { WorkerClient } from "../src/client.js";
import { buildPodEpochDeletionDeps } from "../src/main.js";
import { nullLogger } from "./helpers.js";
import { daemon, ENABLE_DELETION_FOR_TEST } from "./pod-epoch-reclaim-support.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";
import type { RetainedSourceEvidence } from "../src/git.js";
import type { RecoveryHoldsResponse } from "../src/protocol.js";

const bytes = await readFile(new URL("../../fixtures/pod-epoch/v2/positive.json", import.meta.url));
const evidence = JSON.parse(bytes.toString());
const fixture = (name: string) => readFile(new URL("../../fixtures/pod-epoch/" + name, import.meta.url));
type Options = Parameters<typeof runPodEpochShadow>[0];
function setup() {
  const calls = { handovers: 0, reads: 0, inspect: 0, discovery: 0, binding: 0 };
  const parent = new AbortController();
  const ownership: PodEpochOwnership = {
    mode: "exclusive", consume: async () => { throw new Error("consume already performed"); },
    matchesEntry: () => { calls.binding++; return true; },
    reacquireShared: async () => { calls.handovers++; },
    close: () => { throw new Error("must retain ownership"); },
  };
  const options: Options = {
    bootstrap: { podUid: evidence.successor, runtime: evidence.runtime,
      evidenceFile: "/etc/uzi/pod-epoch/evidence", firstMode: "exclusive" },
    ownership, consumption: { result: "fresh", mayEvaluate: true }, signal: parent.signal,
    readEvidence: async () => { calls.reads++; return bytes; },
    kernelRelease: () => "6.1.0-test",
    client: { listRecoveryHolds: async run_id => ({ run_id, holds: [] }),
      getRunOwnership: async () => ({ status: "completed" }) },
    git: {
      discoverEpochRetainedSources: async () => {
        calls.discovery++;
        return { groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 };
      },
      inspectRetainedSources: async () => { calls.inspect++; throw new Error("unexpected inspection"); },
    },
  };
  return { options, calls, parent };
}
function clock(options: Options, initial = 0) {
  let time = initial;
  const timers = new Set<{ at: number; callback: () => void }>();
  options.now = () => time;
  options.schedule = (callback, ms) => {
    const item = { at: time + ms, callback }; timers.add(item); return () => { timers.delete(item); };
  };
  return {
    advance(ms: number) {
      time += ms;
      for (;;) {
        const due = [...timers].filter(t => t.at <= time).sort((a, b) => a.at - b.at)[0];
        if (!due) break;
        timers.delete(due); due.callback();
      }
    },
    timers,
  };
}
function source(path: string, runId: string, protectedSource = false): RetainedSourceEvidence {
  return {
    barePath: "/bare", runId, branch: "branch", key: "key", kind: "attempt", generation: 1,
    attempt: "attempt", ledgerState: "live", journalState: "none", markerState: "none",
    presence: "present", descriptor: "fresh", allocatedBytes: 4096, bytesState: "measured",
    sources: [{ clonePath: path, attempt: "attempt", generation: 1, presence: "present",
      ledgerState: "live", live: false, allocatedBytes: 4096 }],
    consumers: [], localRecords: [], holds: { state: "read", ids: [] },
    disposition: { kind: protectedSource ? "local_protected_discard" : "local_discard" },
  };
}
function inventory(options: Options, count: number) {
  const groups = Array.from({ length: Math.min(200, count) }, (_, i) => ({
    barePath: "/bare", selector: { runId: "run" + i, generation: 1, attempt: "attempt" }, paths: ["/tree/" + i],
  }));
  options.git.discoverEpochRetainedSources = async () => ({
    groups, inventoryComplete: true, physicalPaths: count, overflow: Math.max(0, count - 200), overflowExact: true, unreadable: 0,
  });
  options.git.inspectRetainedSources = async opts => {
    const group = groups.find(g => g.selector.runId === opts.selector!.runId)!;
    for (const p of group.paths) opts.beforeTree!(p);
    return { sources: group.paths.map(p => source(p, group.selector.runId, true)), partial: false, unreadable: 0, sampledAt: "" };
  };
  return groups;
}
const tick = () => new Promise<void>(resolve => setImmediate(resolve));

test("bootstrap refusals never open evidence; consumed restart skips a second verdict", async () => {
  for (const reason of ["runtime_not_declared", "no_projection", "consumption_uncertain"] as const) {
    const { options, calls } = setup();
    options.bootstrap = { ...options.bootstrap, firstMode: "shared", reason };
    const verdict = (await runPodEpochShadow(options))!;
    assert.equal(verdict.reason, reason);
    assert.deepEqual(verdict.authority, reason === "runtime_not_declared" ?
      { result: "undeclared", reason } : { result: "unproven", reason });
    assert.equal(verdict.evidence_version, null);
    assert.equal(calls.reads, 0); assert.equal(calls.handovers, 1);
  }
  for (const result of ["existing", "full"] as const) {
    const { options, calls } = setup(); options.consumption = { result, mayEvaluate: false };
    const verdict = await runPodEpochShadow(options);
    assert.equal(verdict?.reason, result === "existing" ? undefined : "consumption_full");
    assert.equal(calls.reads, 0);
  }
  for (const mode of ["shared", "unsupported"] as const) {
    const { options, calls } = setup(); options.ownership = { ...options.ownership, mode };
    assert.equal((await runPodEpochShadow(options))!.reason, mode === "shared" ? "lock_busy" : "unsupported_filesystem");
    assert.equal(calls.reads, 0); assert.equal(calls.handovers, 0);
  }
});
test("already aborted fresh startup is partial shutdown before all evaluation; existing UID stays skipped", async () => {
  for (const state of ["fresh", "refused", "existing"] as const) {
    const existing = state === "existing";
    const { options, calls, parent } = setup();
    if (existing) options.consumption = { result: "existing", mayEvaluate: false };
    if (state === "refused") options.bootstrap = { ...options.bootstrap, firstMode: "shared", reason: "no_projection" };
    clock(options);
    const schedule = options.schedule!;
    const scheduled: number[] = [];
    options.schedule = (callback, ms) => { scheduled.push(ms); return schedule(callback, ms); };
    parent.abort();
    const result = await runPodEpochShadow(options);
    if (existing) assert.equal(result, undefined);
    else {
      assert.equal(result!.verdict, "partial");
      assert.equal(result!.reason, "shutdown");
      assert.equal(result!.examined, 0);
    }
    assert.deepEqual(calls, { handovers: 0, reads: 0, inspect: 0, discovery: 0, binding: 0 });
    assert.deepEqual(scheduled, [595000], "no evidence evaluation or settlement timers are started");
  }
});
test("invalid run identifiers remain kept without either fresh API call or inspection", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    const invalid = ["", ".", "..", "../other", "a/b", "a\\b", "a?query", "a#fragment",
      "a%2fb", "a b", "a\n", "é", "a".repeat(161), undefined, null, 42];
    const groups = inventory(options, invalid.length + 2);
    groups[0]!.selector.runId = "A_0-";
    groups[1]!.selector.runId = "a".repeat(160);
    invalid.forEach((id, i) => { groups[i + 2]!.selector.runId = id as string; });
    const holds: string[] = [], lifecycle: string[] = [], inspected: string[] = [];
    options.client.listRecoveryHolds = async run_id => { holds.push(run_id); return { run_id, holds: [] }; };
    options.client.getRunOwnership = async id => { lifecycle.push(id); return { status: "completed" }; };
    const inspect = options.git.inspectRetainedSources;
    options.git.inspectRetainedSources = async opts => {
      inspected.push(opts.selector!.runId); return inspect(opts);
    };
    const result = (await runPodEpochShadow(options))!;
    const valid = ["A_0-", "a".repeat(160)];
    assert.deepEqual(holds, valid); assert.deepEqual(lifecycle, valid); assert.deepEqual(inspected, valid);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "inventory_partial");
    assert.equal(result.examined, groups.length); assert.equal(result.kept, invalid.length);
    assert.equal(result.would_delete, 2); assert.equal(result.inventory_complete, false);
  } finally { await d.close(); }
});
for (const fails of [false, true])
test("native SH wait owns its 120s independently of pass deadline: " + (fails ? "failure" : "success"), async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    const time = clock(options);
    const discover = options.git.discoverEpochRetainedSources;
    options.git.discoverEpochRetainedSources = async opts => {
      const census = await discover(opts);
      time.advance(594000);
      return census;
    };
    let entered!: () => void;
    const ready = new Promise<void>(resolve => { entered = resolve; });
    const nativeFailure = new Error("native SH timeout after EX closed");
    options.ownership.reacquireShared = () => new Promise<void>((resolve, reject) => {
      // Model the native wait after EX has closed, with its own independent bound.
      options.schedule!(() => fails ? reject(nativeFailure) : resolve(), 120000);
      entered();
    });
    let settled = false;
    const running = runPodEpochShadow(options).then(
      result => { settled = true; return { result, error: undefined }; },
      (error: unknown) => { settled = true; return { result: undefined, error }; },
    );
    await ready;
    assert.equal(time.timers.size, 1, "only the native SH timer remains after guard cleanup");
    time.advance(6000); await tick();
    assert.equal(settled, false, "crossing the pass hard deadline cannot admit or refuse SH");
    time.advance(113999); await tick(); assert.equal(settled, false);
    time.advance(1);
    const outcome = await running;
    if (fails) assert.equal(outcome.error, nativeFailure);
    else {
      assert.equal(outcome.error, undefined); assert.equal(outcome.result!.verdict, "eligible");
      assert.equal(outcome.result!.pass_ms, 594000);
    }
    assert.equal(time.timers.size, 0);
  } finally { await d.close(); }
});
test("negative and invalid evidence sanitize arbitrary reasons and binding failures", async () => {
  for (const [raw, expected] of [
    ['{"handoff":"none","reason":"secret\\n' + "x".repeat(6000) + '"}', "no_predecessor_evidence"],
    ["invalid", "evidence_invalid"],
    [bytes.toString().replace(evidence.successor, evidence.predecessor), "evidence_invalid"],
  ]) {
    const { options, calls } = setup(); options.readEvidence = async () => raw!;
    const result = await runPodEpochShadow(options);
    assert.equal(result!.reason, expected); assert.equal(calls.handovers, 1);
    assert.ok(JSON.stringify(result).length < 16000);
  }
});
test("a v2 contradicted attestation defers before any inventory or daemon request", async () => {
  const raw = await readFile(new URL("../../fixtures/pod-epoch/v2/valid/contradicted.json", import.meta.url));
  const { options, calls } = setup(); options.readEvidence = async () => raw;
  const result = await runPodEpochShadow(options);
  assert.equal(result!.verdict, "deferred"); assert.equal(result!.reason, "attestation_contradicted");
  assert.equal(result!.would_delete, 0); assert.equal(result!.examined, 0); assert.equal(calls.handovers, 1);
});
test("missing projection polls reopened evidence until the soft stop, joining before handover", async () => {
  const { options, calls } = setup(); const time = clock(options);
  options.readEvidence = async () => { calls.reads++; throw Object.assign(new Error(), { code: "ENOENT" }); };
  const running = runPodEpochShadow(options);
  await tick(); time.advance(250); await tick();
  assert.equal(calls.reads, 2);
  time.advance(114750);
  const result = await running;
  assert.equal(result!.reason, "evidence_timeout"); assert.equal(calls.handovers, 1);
  assert.equal(result!.evidence_wait_ms, 115000); assert.equal(time.timers.size, 0);
});
test("uncancelable evidence at hard cap throws and never hands over", async () => {
  const { options, calls } = setup(); const time = clock(options);
  let settle!: (value: Uint8Array) => void;
  options.readEvidence = () => new Promise(resolve => { settle = resolve; });
  const running = runPodEpochShadow(options);
  await tick(); time.advance(115000); await tick(); time.advance(5000);
  await assert.rejects(running, PodEpochSettlementError);
  assert.equal(calls.handovers, 0); settle(bytes); await tick();
});
test("parent shutdown joins active evidence without SH reacquisition", async () => {
  const { options, calls, parent } = setup();
  let finish!: () => void;
  options.readEvidence = () => new Promise(resolve => { finish = () => resolve(bytes); });
  const running = runPodEpochShadow(options); await tick();
  parent.abort(); await tick(); assert.equal(calls.handovers, 0);
  finish(); const result = await running;
  assert.equal(result!.reason, "shutdown"); assert.equal(calls.handovers, 0);
});
test("three globally terminal trees qualify while an open hold and nonterminal tree stay kept", async () => {
  const d = await daemon();
  try {
    const { options, calls } = setup(); options.dockerHost = d.host;
    inventory(options, 5);
    let mutations = 0;
    Object.assign(options.client, {
      releaseRecoveryHold: () => { mutations++; throw new Error("forbidden mutation"); },
      settleRecoveryHold: () => { mutations++; throw new Error("forbidden mutation"); },
    });
    options.client.getRunOwnership = async id => ({ status: id === "run4" ? "running" : ["completed", "failed", "cancelled"][Number(id.slice(3)) % 3]! });
    options.client.listRecoveryHolds = async run_id => ({ run_id,
      holds: run_id === "run3" ? [{ hold_id: "hold", generation: 1 } as never] : [] });
    const result = await runPodEpochShadow(options);
    assert.equal(result!.verdict, "eligible"); assert.equal(result!.would_delete, 3);
    assert.equal(result!.kept, 2); assert.equal(calls.handovers, 1);
    assert.ok(d.requests.every(r => r.startsWith("GET ")));
    assert.equal(mutations, 0);
  } finally { await d.close(); }
});
for (const count of [199, 200, 201]) test("physical cap " + count + " and bounded output", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, count);
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.examined, Math.min(count, 200));
    assert.equal(result.would_delete, Math.min(count, 200));
    assert.equal(result.list.length, 32); assert.equal(result.list_overflow, Math.min(count, 200) - 32);
    assert.equal(result.inventory_overflow, Math.max(0, count - 200));
    assert.equal(result.verdict, count > 200 ? "partial" : "eligible");
    assert.ok(Buffer.byteLength(JSON.stringify(result)) < 16384);
  } finally { await d.close(); }
});
test("second tree entry change clears the entire epoch even when Git catches callback errors", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; const groups = inventory(options, 2);
    let changed = false;
    options.ownership.matchesEntry = () => !changed;
    options.git.inspectRetainedSources = async opts => {
      if (opts.selector!.runId === "run1") changed = true;
      try { opts.beforeTree!(groups.find(g => g.selector.runId === opts.selector!.runId)!.paths[0]!); } catch {}
      return { sources: [source("/tree/" + opts.selector!.runId.slice(3), opts.selector!.runId)], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_delete, 0); assert.deepEqual(result.list, []); assert.equal(result.reason, "entry_changed");
  } finally { await d.close(); }
});
test("daemon veto stays connected throughout held measurement and joins before handover", async () => {
  const d = await daemon();
  try {
    const { options, calls } = setup(); options.dockerHost = d.host; inventory(options, 2);
    let entered!: () => void;
    const ready = new Promise<void>(resolve => { entered = resolve; });
    options.git.inspectRetainedSources = async opts => {
      entered();
      await new Promise<void>(resolve => opts.signal!.addEventListener("abort", () => resolve(), { once: true }));
      return { sources: [], partial: true, unreadable: 0, sampledAt: "" };
    };
    const running = runPodEpochShadow(options); await ready;
    d.veto(); const result = (await running)!;
    assert.equal(result.would_delete, 0); assert.equal(calls.handovers, 1);
    assert.notEqual(result.verdict, "eligible");
  } finally { await d.close(); }
});
test("path drift refuses all authority and malformed fresh holds cannot qualify", async () => {
  const d = await daemon();
  try {
    for (const drift of [false, true]) {
      const { options } = setup(); options.dockerHost = d.host; inventory(options, 1);
      options.client.listRecoveryHolds = async () => ({ run_id: "wrong", holds: [] });
      if (drift) options.git.inspectRetainedSources = async () => ({
        sources: [source("/unexpected", "run0")], partial: false, unreadable: 0, sampledAt: "",
      });
      const result = (await runPodEpochShadow(options))!;
      assert.equal(result.would_delete, 0);
      if (drift) assert.equal(result.reason, "path_set_changed");
    }
  } finally { await d.close(); }
});
for (const stage of ["discovery", "holds", "ownership", "queued_inspection", "measurement"] as const)
test("shutdown aborts and joins " + stage + " without handover", async () => {
  const d = await daemon();
  try {
    const { options, parent, calls } = setup();
    options.dockerHost = d.host; inventory(options, 1);
    let entered!: () => void, finish!: () => void;
    const ready = new Promise<void>(resolve => { entered = resolve; });
    let stageSignal: AbortSignal | undefined;
    const hold = async (signal: AbortSignal) => {
      stageSignal = signal; entered();
      await new Promise<void>(resolve => { finish = resolve; });
    };
    if (stage === "discovery") options.git.discoverEpochRetainedSources = async opts => {
      await hold(opts!.signal!);
      return { groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 };
    };
    if (stage === "holds") options.client.listRecoveryHolds = async (run_id, signal) => {
      await hold(signal!); return { run_id, holds: [] };
    };
    if (stage === "ownership") options.client.getRunOwnership = async (_id, opts) => {
      await hold(opts.signal); return { status: "completed" };
    };
    if (stage === "queued_inspection" || stage === "measurement") options.git.inspectRetainedSources = async opts => {
      if (stage === "measurement") opts.beforeTree!("/tree/0");
      await hold(opts.signal!);
      return { sources: [source("/tree/0", "run0")], partial: false, unreadable: 0, sampledAt: "" };
    };
    const running = runPodEpochShadow(options);
    await ready; parent.abort(); await tick();
    assert.equal(stageSignal!.aborted, true); assert.equal(calls.handovers, 0);
    finish(); const result = (await running)!;
    assert.notEqual(result.verdict, "eligible"); assert.equal(result.inventory_complete, false);
    assert.equal(calls.handovers, 0);
  } finally { await d.close(); }
});
test("multipart duplicate parts are counted once", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    const groups = inventory(options, 1);
    groups[0]!.paths = ["/tree/a", "/tree/b", "/tree/a"];
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_delete, 2); assert.equal(result.examined, 2);
    assert.equal(result.kept, 0);
  } finally { await d.close(); }
});
test("unsafe, unmeasured, live, refused and local-record sources stay kept", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 5);
    options.git.inspectRetainedSources = async opts => {
      const index = Number(opts.selector!.runId.slice(3));
      const s = source("/tree/" + index, opts.selector!.runId);
      opts.beforeTree!(s.sources[0]!.clonePath);
      if (index === 0) s.sources[0]!.presence = "unsafe";
      if (index === 1) { s.bytesState = "unknown"; s.sources[0]!.allocatedBytes = null; }
      if (index === 2) s.sources[0]!.live = true;
      if (index === 3) s.disposition = { kind: "refused", consumers: ["unknown"] };
      if (index === 4) s.localRecords = [{ kind: "terminal", generation: 1, name: "record", resolution: "keep" }];
      return { sources: [s], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_delete, 0); assert.equal(result.kept, 5);
  } finally { await d.close(); }
});
test("fresh HTTP failures keep sources and do not prevent bounded sibling inspection", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 2);
    options.client.listRecoveryHolds = async id => {
      if (id === "run0") throw new Error("untrusted API error");
      return { run_id: id, holds: [] };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_delete, 1); assert.equal(result.kept, 1);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "inventory_partial");
  } finally { await d.close(); }
});
for (const phase of ["headers", "body"] as const)
test("shutdown joins daemon request during " + phase, async () => {
  const { options, parent, calls } = setup();
  let entered!: () => void;
  const ready = new Promise<void>(resolve => { entered = resolve; });
  const server = http.createServer((req, res) => {
    if (req.url === "/_ping") { res.end("OK"); return; }
    if (req.url!.startsWith("/events")) { res.writeHead(200); res.flushHeaders(); return; }
    if (phase === "body") { res.writeHead(200); res.write('{"Swarm":'); }
    entered();
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  options.dockerHost = "tcp://127.0.0.1:" + address.port;
  try {
    const running = runPodEpochShadow(options);
    await ready; parent.abort();
    const result = (await running)!;
    assert.notEqual(result.verdict, "eligible"); assert.equal(calls.handovers, 0);
    assert.equal(result.would_delete, 0);
  } finally {
    server.closeAllConnections();
    await new Promise<void>(resolve => server.close(() => resolve()));
  }
});
test("whole-pass hard cap cannot hand over while discovery remains unsettled", async () => {
  const d = await daemon();
  try {
    const { options, calls } = setup(); options.dockerHost = d.host;
    const time = clock(options);
    let entered!: () => void, finish!: () => void;
    const ready = new Promise<void>(resolve => { entered = resolve; });
    options.git.discoverEpochRetainedSources = async () => {
      entered(); await new Promise<void>(resolve => { finish = resolve; });
      return { groups: [], inventoryComplete: true, physicalPaths: 0, overflow: 0, overflowExact: true, unreadable: 0 };
    };
    const running = runPodEpochShadow(options);
    await ready; time.advance(595000); await tick(); time.advance(5000);
    await assert.rejects(running, PodEpochSettlementError);
    assert.equal(calls.handovers, 0);
    finish(); await tick();
  } finally { await d.close(); }
});
test("parent settlement cap is shared and never grants another five seconds per stage", async () => {
  const { options, parent, calls } = setup(); const time = clock(options);
  let finish!: () => void;
  options.readEvidence = () => new Promise(resolve => { finish = () => resolve(bytes); });
  const running = runPodEpochShadow(options);
  await tick(); parent.abort(); time.advance(5000);
  await assert.rejects(running, PodEpochSettlementError);
  assert.equal(calls.handovers, 0); finish(); await tick();
});
test("fresh malformed hold fields are fail closed for every terminal status", async () => {
  const d = await daemon();
  try {
    for (const h of [null, { hold_id: "", generation: 0 }, { hold_id: "h", generation: -1 },
      { hold_id: "h", generation: 1.5 }, { hold_id: "h".repeat(161), generation: 0 }]) {
      const { options } = setup(); options.dockerHost = d.host; inventory(options, 1);
      options.client.listRecoveryHolds = async run_id => ({ run_id, holds: [h as never] });
      const result = (await runPodEpochShadow(options))!;
      assert.equal(result.would_delete, 0); assert.equal(result.kept, 1);
    }
  } finally { await d.close(); }
});
test("shutdown during intentional daemon cleanup joins without handover", async () => {
  const d = await daemon();
  try {
    const { options, parent, calls } = setup(); options.dockerHost = d.host;
    inventory(options, 1); const time = clock(options);
    const schedule = options.schedule!;
    let cleanupObserved = false;
    options.schedule = (callback, ms) => {
      const cancel = schedule(callback, ms);
      return () => {
        cancel();
        // The daemon cancels its absolute soft deadline during intentional close.
        // The pass's identically timed timer is canceled later in the outer finally.
        if (ms === 595000 && !cleanupObserved) { cleanupObserved = true; parent.abort(); }
      };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(cleanupObserved, true); assert.equal(calls.handovers, 0);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "shutdown");
    assert.equal(time.timers.size, 0);
  } finally { await d.close(); }
});
test("default projection reader joins real regular and Kubernetes projection symlink reads", async () => {
  await mkdir(resolve("../.uzi/scratch"), { recursive: true });
  const dir = await mkdtemp(resolve("../.uzi/scratch/projection-"));
  try {
    await mkdir(join(dir, "..version"));
    await writeFile(join(dir, "..version", "evidence"), bytes);
    await symlink("..version", join(dir, "..data"));
    await symlink("..data/evidence", join(dir, "evidence"));
    const signal = new AbortController().signal;
    for (const path of [join(dir, "..version", "evidence"), join(dir, "evidence")])
      assert.deepEqual(await readPodEpochProjection(path, signal), bytes);
  } finally { await rm(dir, { recursive: true, force: true }); }
});
test("default projection reader handles missing, empty, oversize and nonregular files", async () => {
  await mkdir(resolve("../.uzi/scratch"), { recursive: true });
  const dir = await mkdtemp(resolve("../.uzi/scratch/projection-"));
  try {
    const signal = new AbortController().signal;
    await assert.rejects(readPodEpochProjection(join(dir, "missing"), signal), { code: "ENOENT" });
    await writeFile(join(dir, "empty"), "");
    assert.equal((await readPodEpochProjection(join(dir, "empty"), signal)).length, 0);
    const oversized = Buffer.alloc(65536, 120);
    await writeFile(join(dir, "oversize"), oversized);
    const bounded = await readPodEpochProjection(join(dir, "oversize"), signal);
    assert.equal(bounded.length, 8193);
    assert.deepEqual(bounded, oversized.subarray(0, 8193));
    await assert.rejects(readPodEpochProjection(dir, signal), { message: "evidence_invalid" });
    // A real FIFO with no writer must be refused without waiting to open it.
    await promisify(execFile)("mkfifo", [join(dir, "fifo")]);
    await assert.rejects(readPodEpochProjection(join(dir, "fifo"), signal), { message: "evidence_invalid" });
  } finally { await rm(dir, { recursive: true, force: true }); }
});
test("default projection reader joins actual read and close even when shutdown arrives", async t => {
  await mkdir(resolve("../.uzi/scratch"), { recursive: true });
  const dir = await mkdtemp(resolve("../.uzi/scratch/projection-"));
  try {
    const path = join(dir, "evidence");
    await writeFile(path, bytes);
    const probe = await open(path, "r");
    const prototype = Object.getPrototypeOf(probe) as typeof probe;
    await probe.close();
    const parent = new AbortController();
    let releaseRead!: () => void, releaseClose!: () => void, enteredRead!: () => void, enteredClose!: () => void;
    const reading = new Promise<void>(r => { enteredRead = r; });
    const closing = new Promise<void>(r => { enteredClose = r; });
    const readBarrier = new Promise<void>(r => { releaseRead = r; });
    const closeBarrier = new Promise<void>(r => { releaseClose = r; });
    const originalRead = prototype.read;
    let reads = 0, closes = 0;
    t.mock.method(prototype, "read", async function (this: typeof probe, ...args: Parameters<typeof originalRead>) {
      reads++;
      if (reads === 1) {
        // FileHandle.close is an instance method bound to its actual open handle.
        const close = this.close.bind(this);
        t.mock.method(this, "close", async () => {
          enteredClose(); await closeBarrier;
          await close(); closes++;
        });
        enteredRead(); await readBarrier;
      }
      return originalRead.apply(this, args);
    });
    let settled = false;
    const running = readPodEpochProjection(path, parent.signal).finally(() => { settled = true; });
    try {
      await reading; parent.abort(); await tick();
      assert.equal(settled, false); assert.equal(closes, 0);
      releaseRead(); await closing; await tick();
      assert.equal(settled, false); assert.equal(closes, 0);
    } finally { releaseRead(); releaseClose(); }
    assert.deepEqual(await running, bytes);
    assert.equal(closes, 1);
  } finally {
    t.mock.restoreAll();
    await rm(dir, { recursive: true, force: true });
  }
});
test("empty healthy census is eligible and intentional guard close is harmless", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.verdict, "eligible"); assert.equal(result.would_delete, 0);
    assert.equal(result.reason, "shadow_complete");
  } finally { await d.close(); }
});

for (const endpoint of ["holds", "ownership"] as const) {
  for (const mode of ["overflow", "abort", "deadline"] as const) {
    test("real WorkerClient " + endpoint + " joins delayed body cancellation (" + mode + ")", async () => {
      const d = await daemon();
      let release!: () => void;
      const held = new Promise<void>(r => { release = r; });
      let entered!: () => void, cancelling!: () => void;
      const reading = new Promise<void>(r => { entered = r; });
      const cancelled = new Promise<void>(r => { cancelling = r; });
      let cancelSettled = false, settled = false;
      const { options, calls, parent } = setup();
      options.dockerHost = d.host;
      inventory(options, 1);
      const time = clock(options, Date.now());
      const requests: string[] = [];
      options.client = new WorkerClient("http://example.invalid", "inert", "test", nullLogger(), {
        fetch: async (url, init) => {
          assert.equal(init?.method, "GET");
          const path = new URL(String(url)).pathname;
          requests.push(path);
          // Lifecycle is read before holds, so the first request is ownership.
          const isOwnership = path.endsWith("/ownership");
          if (endpoint === "holds" && isOwnership)
            return new Response(JSON.stringify({ status: "completed" }));
          assert.equal(path.endsWith(endpoint === "holds" ? "/recovery-holds" : "/ownership"), true);
          return new Response(new ReadableStream<Uint8Array>({
            pull(controller) {
              entered();
              if (mode !== "abort") controller.enqueue(new Uint8Array(1024 * 1024 + 1));
            },
            async cancel() { cancelling(); await held; cancelSettled = true; },
          }));
        },
      });
      const running = runPodEpochShadow(options).finally(() => { settled = true; });
      try {
        await reading;
        if (mode === "abort") parent.abort();
        await cancelled;
        // Give daemon socket cleanup time to finish if an RPC detached cancellation.
        for (let i = 0; i < 1000 && !settled; i++) await tick();
        assert.equal(settled, false);
        assert.equal(calls.handovers, 0);
        if (mode === "deadline") {
          time.advance(595000); await tick(); time.advance(5000);
          await assert.rejects(running, PodEpochSettlementError);
          assert.equal(calls.handovers, 0);
          assert.equal(cancelSettled, false);
        } else {
          release();
          const result = await running;
          assert.equal(cancelSettled, true);
          assert.equal(result!.would_delete, 0);
          assert.equal(calls.handovers, mode === "abort" ? 0 : 1);
        }
        assert.equal(requests.length, endpoint === "holds" ? 2 : 1);
      } finally {
        release();
        await running.catch(() => undefined);
        await tick();
        await d.close();
      }
    });
  }
}

for (const failing of ["ownership", "holds"] as const) {
  test("a real WorkerClient HTTP 404 on " + failing +
    (failing === "holds" ? " after terminal ownership succeeded" : "") + " keeps every source", async () => {
    const d = await daemon();
    try {
      const { options, calls } = setup(); options.dockerHost = d.host; inventory(options, 2);
      const requests: string[] = [];
      options.client = new WorkerClient("http://example.invalid", "inert", "test", nullLogger(), {
        fetch: async (url, init) => {
          assert.equal(init?.method, "GET");
          const path = new URL(String(url)).pathname;
          requests.push(path);
          if (failing === "holds" && path.endsWith("/ownership")) return new Response(JSON.stringify({ status: "completed" }));
          return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
        },
      });
      const result = (await runPodEpochShadow(options))!;
      assert.equal(result.would_delete, 0); assert.equal(result.would_settle, 0);
      assert.deepEqual(result.list, []); assert.equal(result.list_overflow, 0);
      assert.equal(result.kept, 2); assert.equal(result.verdict, "partial");
      assert.equal(result.reason, "inventory_partial");
      assert.equal(requests.length, failing === "holds" ? 4 : 2, "one bounded attempt per request, no retry");
      assert.equal(requests.filter(p => p.endsWith("/recovery-holds")).length, failing === "holds" ? 2 : 0);
      assert.equal(calls.handovers, 1);
    } finally { await d.close(); }
  });
}

// ---- M1a widened shadow: authority, worker-recomputed attestation, widened candidates ----
const mutate = async (name: string, change: (doc: any) => void) => {
  const doc = JSON.parse((await fixture(name)).toString()); change(doc); return JSON.stringify(doc);
};
async function run(raw: string | Uint8Array, tweak?: (o: Options) => void, host?: string) {
  const { options, calls } = setup();
  options.readEvidence = async () => { calls.reads++; return raw; };
  if (host) options.dockerHost = host;
  tweak?.(options);
  return { result: (await runPodEpochShadow(options))!, calls, options };
}
test("v2 consistent evidence is attested and runs the pass", async () => {
  const d = await daemon();
  try {
    const { result, calls } = await run(bytes, undefined, d.host);
    assert.deepEqual(result.authority, { result: "attested", reason: "observations_consistent" });
    assert.equal(result.evidence_version, 2); assert.equal(result.verdict, "eligible");
    assert.equal(calls.discovery, 1); assert.deepEqual(result.observations, { kernel: "6.1.0-test" });
  } finally { await d.close(); }
});
test("v1 evidence defers as legacy_evidence with no daemon, census or inventory work", async () => {
  const d = await daemon();
  try {
    const { result, calls } = await run(await fixture("valid/positive.json"), undefined, d.host);
    assert.equal(result.verdict, "deferred"); assert.equal(result.reason, "legacy_evidence");
    assert.deepEqual(result.authority, { result: "legacy_evidence", reason: "legacy_evidence" });
    assert.equal(result.evidence_version, 1);
    assert.deepEqual(d.requests, []); assert.equal(calls.discovery, 0); assert.equal(calls.inspect, 0);
    assert.equal(calls.binding, 0); assert.equal(calls.handovers, 1); assert.equal(result.examined, 0);
  } finally { await d.close(); }
});
test("the contradicted attestation reason is never echoed", async () => {
  const { result } = await run(await mutate("v2/valid/contradicted.json", d => { d.attestation.reason = "secret-controller-text"; }));
  assert.equal(result.reason, "attestation_contradicted");
  assert.deepEqual(result.authority, { result: "contradicted", reason: "attestation_contradicted" });
  assert.ok(!JSON.stringify(result).includes("secret-controller-text"));
});
test("the worker recomputes runtime prefixes even when the controller attests consistent", async () => {
  const cases: Array<[string, (d: any) => void, string]> = [
    ["wrong predecessor container prefix", d => { d.containers[0].containerID = "cri-o://worker"; }, "predecessor_runtime_prefix"],
    ["wrong init container prefix", d => { d.initContainers[0].containerID = "docker://seed"; }, "predecessor_runtime_prefix"],
    ["wrong successor prefix", d => { d.successorContainers[1].containerID = "cri-o://worker2"; }, "successor_runtime_prefix"],
  ];
  for (const [label, change, reason] of cases) {
    const d = await daemon();
    try {
      const { result, calls } = await run(await mutate("v2/positive.json", change), undefined, d.host);
      assert.equal(result.verdict, "deferred", label); assert.equal(result.reason, reason, label);
      assert.deepEqual(result.authority, { result: "contradicted", reason }, label);
      assert.deepEqual(d.requests, [], label); assert.equal(calls.discovery, 0, label);
      assert.equal(calls.binding, 0, label);
    } finally { await d.close(); }
  }
  const crio = await fixture("v2/valid/crio.json");
  const wrong = await mutate("v2/valid/crio.json", d => { d.containers[0].containerID = "containerd://worker"; });
  const bootstrap = (o: Options) => { o.bootstrap = { ...o.bootstrap, runtime: "crio-crun" }; o.kernelRelease = () => "6.12.0-243.el10.x86_64"; };
  assert.equal((await run(crio, bootstrap)).result.authority.result, "attested");
  assert.equal((await run(wrong, bootstrap)).result.reason, "predecessor_runtime_prefix");
});
test("crio-crun requires an exactly audited kernel; containerd-runc only records it", async () => {
  const crio = await fixture("v2/valid/crio.json");
  const as = (kernel: string, runtime: "crio-crun" | "containerd-runc") => (o: Options) => {
    o.bootstrap = { ...o.bootstrap, runtime }; o.kernelRelease = () => kernel;
  };
  for (const kernel of ["6.12.0-243.el10.x86_64", "6.12.0-243.el10.aarch64"]) {
    const { result } = await run(crio, as(kernel, "crio-crun"));
    assert.equal(result.authority.result, "attested", kernel); assert.deepEqual(result.observations, { kernel });
  }
  for (const kernel of ["6.12.0-243.el10.x86_64+debug", "6.12.0-244.el10.x86_64", "6.12.0", ""]) {
    const d = await daemon();
    try {
      const { result, calls } = await run(crio, as(kernel, "crio-crun"), d.host);
      assert.equal(result.verdict, "deferred", kernel); assert.equal(result.reason, "kernel_unaudited", kernel);
      assert.deepEqual(result.authority, { result: "contradicted", reason: "kernel_unaudited" }, kernel);
      assert.equal(result.observations?.kernel, kernel);
      assert.deepEqual(d.requests, [], kernel); assert.equal(calls.discovery, 0, kernel);
    } finally { await d.close(); }
  }
  for (const kernel of ["6.12.0-244.el10.x86_64", "5.15.0-generic", "x".repeat(200)]) {
    const { result } = await run(bytes, as(kernel, "containerd-runc"));
    assert.equal(result.authority.result, "attested", kernel);
  }
  const { result } = await run(bytes, o => { o.kernelRelease = () => "6.1 odd/\u0001" + "y".repeat(100); });
  assert.match(result.observations!.kernel, /^[A-Za-z0-9._+-]{1,64}$/);
});
test("authority reasons are the verdict reason for other pre-evidence refusals", async () => {
  const { result } = await run("invalid");
  assert.deepEqual(result.authority, { result: "unproven", reason: "evidence_invalid" });
  assert.equal(result.evidence_version, null);
  const binding = await run(await mutate("v2/positive.json", d => { d.successor = "00000000-0000-4000-8000-000000000009"; }));
  assert.equal(binding.result.reason, "evidence_binding");
  assert.equal(binding.result.evidence_version, 2);
  assert.deepEqual(binding.result.authority, { result: "unproven", reason: "evidence_binding" });
});
test("contradicted evidence for another pod is a binding refusal, not a contradiction", async () => {
  const { result } = await run(await mutate("v2/valid/contradicted.json", d => { d.successor = "00000000-0000-4000-8000-000000000000"; }));
  assert.equal(result.reason, "evidence_binding");
  assert.deepEqual(result.authority, { result: "unproven", reason: "evidence_binding" });
});
test("an entry mismatch before the daemon is unproven, never attested", async () => {
  const d = await daemon();
  try {
    const { result, calls } = await run(bytes, o => { o.ownership.matchesEntry = () => false; }, d.host);
    assert.equal(result.reason, "entry_changed");
    assert.deepEqual(result.authority, { result: "unproven", reason: "entry_changed" });
    assert.deepEqual(d.requests, []); assert.equal(calls.discovery, 0);
  } finally { await d.close(); }
});
test("a daemon veto is unproven; a daemon transport failure keeps attested with a deferred verdict", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 1);
    const inner = options.git.inspectRetainedSources;
    options.git.inspectRetainedSources = async opts => { d.veto(); await tick(); await tick(); return inner(opts); };
    const result = (await runPodEpochShadow(options))!;
    assert.deepEqual(result.authority, { result: "unproven", reason: "daemon_veto" });
    assert.equal(result.would_delete, 0);
  } finally { await d.close(); }
  const down = await run(bytes, undefined, "tcp://127.0.0.1:1");
  assert.equal(down.result.verdict, "deferred");
  assert.deepEqual(down.result.authority, { result: "attested", reason: "observations_consistent" });
});

function candidateSource(path: string, runId: string, tweak: (s: RetainedSourceEvidence) => void): RetainedSourceEvidence {
  const s = source(path, runId); tweak(s); return s;
}
test("present and missing candidates, strict-plain basis, and the examined invariant", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 9);
    const table: Array<(s: RetainedSourceEvidence) => void> = [
      () => {},
      x => { x.presence = "missing"; x.sources[0]!.presence = "missing"; x.sources[0]!.allocatedBytes = null; },
      x => { x.disposition = { kind: "refused", consumers: [] }; x.epochSettlement = { basis: "strict_plain", disposition: { kind: "local_discard" } }; },
      x => { x.disposition = { kind: "refused", consumers: [] }; },
      x => { x.presence = "unsafe"; x.sources[0]!.presence = "unsafe"; },
      x => { x.disposition = { kind: "refused", consumers: [] }; x.presence = "missing"; x.sources[0]!.presence = "missing";
        x.sources[0]!.allocatedBytes = null; x.epochSettlement = { basis: "strict_plain", disposition: { kind: "local_discard" } }; },
      x => { x.disposition = { kind: "refused", consumers: [] }; x.epochSettlement = { basis: "strict_plain", disposition: { kind: "refused", consumers: [] } }; },
      x => { x.sources[0]!.presence = "present"; x.presence = "unsafe"; },
      x => { x.presence = "missing"; x.sources[0]!.presence = "missing"; x.sources[0]!.allocatedBytes = 4096; },
    ];
    options.git.inspectRetainedSources = async opts => {
      const i = Number(opts.selector!.runId.slice(3)); const path = "/tree/" + i;
      opts.beforeTree!(path);
      return { sources: [candidateSource(path, opts.selector!.runId, table[i]!)], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.verdict, "eligible");
    assert.equal(result.would_delete, 2); assert.equal(result.would_delete_bytes, 8192);
    assert.deepEqual(result.by_basis, { discard: 3, strict_plain: 2 });
    assert.equal(result.examined, 9); assert.equal(result.kept, 4);
    assert.equal(result.examined, result.would_delete + result.would_settle + result.kept);
    assert.deepEqual(result.list.map(e => [e.run_id, e.presence, e.basis, e.bytes]), [
      ["run0", "present", "discard", 4096], ["run2", "present", "strict_plain", 4096],
      ["run1", "missing", "discard", 0], ["run5", "missing", "strict_plain", 0], ["run8", "missing", "discard", 0],
    ]);
    // Missing with non-null bytes: listed as 0 and excluded from would_delete_bytes.
    assert.equal(result.would_settle, 3);
    assert.ok(!result.list.some(e => e.run_id === "run3" || e.run_id === "run4" || e.run_id === "run6" || e.run_id === "run7"));
    assert.ok(d.requests.every(r => r.startsWith("GET ")));
  } finally { await d.close(); }
});
test("journal_extra_fields counts only final candidates whose source carried the report-only flag", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 4);
    const flagged = new Set([0, 1, 2]);
    options.git.inspectRetainedSources = async opts => {
      const i = Number(opts.selector!.runId.slice(3)); const path = "/tree/" + i;
      opts.beforeTree!(path);
      const s = candidateSource(path, opts.selector!.runId, x => {
        if (i === 1) { x.presence = "missing"; x.sources[0]!.presence = "missing"; x.sources[0]!.allocatedBytes = null; }
        if (i === 2) x.disposition = { kind: "refused", consumers: [] };
      });
      if (flagged.has(i)) s.epochJournalExtraFields = true;
      return { sources: [s], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    // run0 (present) and run1 (missing) are flagged candidates; run2 is flagged but kept; run3 is an
    // unflagged present candidate.
    assert.equal(result.would_delete, 2); assert.equal(result.would_settle, 1);
    assert.equal(result.journal_extra_fields, 2);
    assert.equal(result.kept, 1);
  } finally { await d.close(); }
  const { result } = await run(bytes);
  assert.equal(result.journal_extra_fields, 0, "defaults to zero");
});
test("a flagged candidate later demoted is not counted in journal_extra_fields", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    options.git.discoverEpochRetainedSources = async () => ({
      groups: ["/bare-a", "/bare-b"].map(barePath => ({ barePath, selector: { runId: "run0", generation: 1, attempt: "attempt" }, paths: ["/tree/shared"] })),
      inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
    });
    options.git.inspectRetainedSources = async opts => {
      opts.beforeTree!("/tree/shared");
      const s = candidateSource("/tree/shared", "run0", opts.barePath === "/bare-b"
        ? x => { x.sources[0]!.allocatedBytes = 999; x.allocatedBytes = 999; } : () => {});
      s.barePath = opts.barePath!; s.epochJournalExtraFields = true;
      return { sources: [s], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_delete, 0); assert.equal(result.kept, 1);
    assert.equal(result.journal_extra_fields, 0);
  } finally { await d.close(); }
});
test("an invalid pass clears present and missing candidates alike", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; const groups = inventory(options, 2);
    let changed = false; options.ownership.matchesEntry = () => !changed;
    options.git.inspectRetainedSources = async opts => {
      if (opts.selector!.runId === "run1") changed = true;
      try { opts.beforeTree!(groups.find(g => g.selector.runId === opts.selector!.runId)!.paths[0]!); } catch {}
      return { sources: [candidateSource("/tree/" + opts.selector!.runId.slice(3), opts.selector!.runId,
        x => { x.presence = "missing"; x.sources[0]!.presence = "missing"; })], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.would_settle, 0); assert.equal(result.would_delete, 0); assert.deepEqual(result.list, []);
    assert.deepEqual(result.by_basis, { discard: 0, strict_plain: 0 });
  } finally { await d.close(); }
});
test("a run's lifecycle is read before its holds", async () => {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host; inventory(options, 1);
    const order: string[] = [];
    options.client.getRunOwnership = async () => { order.push("lifecycle"); return { status: "completed" }; };
    options.client.listRecoveryHolds = async run_id => { order.push("holds"); return { run_id, holds: [] }; };
    await runPodEpochShadow(options);
    assert.deepEqual(order, ["lifecycle", "holds"]);
  } finally { await d.close(); }
});
test("a path also named by a group refused for its run ID is never a candidate", async () => {
  for (const order of [["bad id!", "run0"], ["run0", "bad id!"]]) {
  const d = await daemon();
  try {
    const { options } = setup(); options.dockerHost = d.host;
    options.git.discoverEpochRetainedSources = async () => ({
      groups: order.map(runId => ({ barePath: "/bare", selector: { runId, generation: 1, attempt: "attempt" }, paths: ["/tree/shared"] })),
      inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
    });
    options.git.inspectRetainedSources = async opts => {
      opts.beforeTree!("/tree/shared");
      return { sources: [candidateSource("/tree/shared", opts.selector!.runId, () => {})], partial: false, unreadable: 0, sampledAt: "" };
    };
    const result = (await runPodEpochShadow(options))!;
    assert.equal(result.verdict, "partial"); assert.equal(result.would_delete, 0); assert.equal(result.would_settle, 0);
    assert.equal(result.kept, 1, order.join()); assert.deepEqual(result.list, [], order.join());
  } finally { await d.close(); }
  }
});
test("one run observing one path with different bytes or basis demotes it", async () => {
  for (const tweak of [
    (x: RetainedSourceEvidence) => { x.sources[0]!.allocatedBytes = 999; x.allocatedBytes = 999; },
    (x: RetainedSourceEvidence) => {
      x.disposition = { kind: "refused", consumers: [] };
      x.epochSettlement = { basis: "strict_plain", disposition: { kind: "local_discard" } };
    },
  ]) {
    const d = await daemon();
    try {
      const { options } = setup(); options.dockerHost = d.host;
      options.git.discoverEpochRetainedSources = async () => ({
        groups: ["/bare-a", "/bare-b"].map(barePath => ({ barePath, selector: { runId: "run0", generation: 1, attempt: "attempt" }, paths: ["/tree/shared"] })),
        inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
      });
      options.git.inspectRetainedSources = async opts => {
        opts.beforeTree!("/tree/shared");
        const s = candidateSource("/tree/shared", "run0", opts.barePath === "/bare-b" ? tweak : () => {});
        s.barePath = opts.barePath!;
        return { sources: [s], partial: false, unreadable: 0, sampledAt: "" };
      };
      const result = (await runPodEpochShadow(options))!;
      assert.equal(result.examined, 1); assert.equal(result.would_delete, 0);
      assert.equal(result.kept, 1); assert.deepEqual(result.list, []);
    } finally { await d.close(); }
  }
});
test("one path observed twice is never double counted", async () => {
  const cases: Array<[string, Array<(s: RetainedSourceEvidence) => void>]> = [
    ["present then missing", [() => {}, x => { x.presence = "missing"; x.sources[0]!.presence = "missing"; x.sources[0]!.allocatedBytes = null; }]],
    ["candidate then refused", [() => {}, x => { x.disposition = { kind: "refused", consumers: [] }; }]],
    ["refused then candidate", [x => { x.disposition = { kind: "refused", consumers: [] }; }, () => {}]],
    ["two runs claim one path", [() => {}, () => {}]],
  ];
  for (const [label, table] of cases) {
    const d = await daemon();
    try {
      const { options } = setup(); options.dockerHost = d.host;
      options.git.discoverEpochRetainedSources = async () => ({
        groups: table.map((_, i) => ({ barePath: "/bare", selector: { runId: "run" + i, generation: 1, attempt: "attempt" }, paths: ["/tree/shared"] })),
        inventoryComplete: true, physicalPaths: 1, overflow: 0, overflowExact: true, unreadable: 0,
      });
      options.git.inspectRetainedSources = async opts => {
        opts.beforeTree!("/tree/shared");
        return { sources: [candidateSource("/tree/shared", opts.selector!.runId, table[Number(opts.selector!.runId.slice(3))]!)],
          partial: false, unreadable: 0, sampledAt: "" };
      };
      const result = (await runPodEpochShadow(options))!;
      assert.equal(result.examined, 1, label); assert.equal(result.would_delete, 0, label);
      assert.equal(result.would_settle, 0, label); assert.equal(result.kept, 1, label); assert.deepEqual(result.list, [], label);
    } finally { await d.close(); }
  }
});

test("#3775 shadow grouping selector and legacy fallback causes preserve candidates", async () => {
  const d = await daemon();
  try {
    for (const mode of ["group", "paths", "selector", "census", "census-empty", "inspection", "inspection-empty", "unknown", "malformed-holds"] as const) {
      const { options } = setup();
      options.dockerHost = d.host;
      const groups = inventory(options, 1);
      if (mode === "group") {
        for (let i = 1; i <= 200; i++) groups.push({ ...groups[0]!, paths: ["/tree/0"] });
      }
      if (mode === "paths") groups.push({ ...groups[0]!, paths: Array.from({ length: 200 }, (_, i) => "/extra/" + i) });
      if (mode === "selector") groups[0]!.selector.runId = "../invalid";
      if (mode === "census" || mode === "census-empty") {
        const discover = options.git.discoverEpochRetainedSources;
        options.git.discoverEpochRetainedSources = async opts => ({ ...await discover(opts), inventoryComplete: false, unreadable: 1,
          ...(mode === "census-empty" ? { partialCauses: {} } : {}) });
      }
      if (mode === "inspection" || mode === "inspection-empty") {
        const inspect = options.git.inspectRetainedSources;
        options.git.inspectRetainedSources = async opts => ({ ...await inspect(opts), partial: true,
          ...(mode === "inspection-empty" ? { partialCauses: {} } : {}) });
      }
      if (mode === "unknown") options.client.getRunOwnership = async () => ({ status: "running" });
      if (mode === "malformed-holds") options.client.listRecoveryHolds = async () => ({ run_id: "wrong", holds: [] });
      const result = (await runPodEpochShadow(options))!;
      const causes = mode === "group" ? { group_cap: 1 } : mode === "paths" ? { group_physical_path_cap: 1 } :
        mode === "selector" ? { selector_refused: 1 } : mode.startsWith("census") ? { census_unclassified: 1 } :
        mode.startsWith("inspection") ? { inspection_unclassified: 1 } : {};
      assert.deepEqual(result.partial_causes, causes);
      assert.equal(result.authority.result, "attested");
      assert.equal(result.verdict, ["unknown", "malformed-holds"].includes(mode) ? "eligible" : "partial");
      assert.equal(result.reason, ["unknown", "malformed-holds"].includes(mode) ? "shadow_complete" : "inventory_partial");
      assert.equal(result.would_delete, ["group", "paths", "census", "census-empty"].includes(mode) ? 1 : 0);
      assert.equal(result.examined, 1);
      assert.equal(result.kept, result.would_delete ? 0 : 1);
    }
  } finally { await d.close(); }
});

// ---- M1b deletion phase, exercised through runPodEpochShadow with fake collaborators ----
type Discard = Awaited<ReturnType<NonNullable<Options["deletion"]>["git"]["discardStoppedSource"]>>;
const DESCRIPTOR = "0".repeat(64);
function deletionWorld(count: number, tune?: (w: ReturnType<typeof makeWorld>) => void) { return makeWorld(count, tune); }
function makeWorld(count: number, tune?: (w: any) => void) {
  const { options, calls, parent } = setup();
  const events: string[] = [];
  const spies = { proofs: [] as any[], discards: [] as any[], ended: [] as Array<{ paths: string[]; options: unknown }>,
    reserved: [] as string[], journalReads: [] as string[], begins: 0, finishes: 0 };
  const world = {
    options, calls, parent, events, spies, groups: [] as Array<{ barePath: string; selector: { runId: string; generation: number; attempt: string }; paths: string[] }>,
    pending: 0, stopped: false, eligible: true, activeTx: undefined as object | undefined,
    reserveRefused: false, finishThrows: false, proofThrows: false,
    /** Mirrors GitCache's process-wide unsettled-delete map being non-empty. */
    unsettled: false,
    holds: (run_id: string): RecoveryHoldsResponse => ({ run_id, holds: [], run_open_holds: { run_id, count: 0 } }),
    ownership: (): { status: string } => ({ status: "completed" }),
    journal: (_branch: string, path: string, runId: string): Array<string | null> =>
      [JSON.stringify({ attemptId: "attempt", clonePath: path, runId })],
    mutateSource: (_s: RetainedSourceEvidence, _i: number) => {},
    discard: (_opts: any, path: string): Discard => ({ kind: "done", sources: [{ clonePath: path, outcome: "removed" }],
      metadata: "retired", tookOver: true }),
  };
  const groups = Array.from({ length: count }, (_, i) => ({
    barePath: "/bare", selector: { runId: "run" + i, generation: 1, attempt: "attempt" }, paths: ["/tree/" + i],
  }));
  world.groups = groups;
  options.git.discoverEpochRetainedSources = async () => ({
    groups, inventoryComplete: true, physicalPaths: count, overflow: 0, overflowExact: true, unreadable: 0 });
  options.git.inspectRetainedSources = async opts => {
    const i = Number(opts.selector!.runId.slice(3));
    const group = groups[i]!;
    for (const p of group.paths) opts.beforeTree!(p);
    const s = source(group.paths[0]!, group.selector.runId);
    s.branch = "b" + i; s.descriptor = DESCRIPTOR;
    s.sources = group.paths.map(path => ({ ...s.sources[0]!, clonePath: path }));
    world.mutateSource(s, i);
    return { sources: [s], partial: false, unreadable: 0, sampledAt: "" };
  };
  options.client.listRecoveryHolds = async run_id => world.holds(run_id);
  options.client.getRunOwnership = async () => world.ownership();
  const scope = {
    assertExclusive() {}, get pendingMutations() { return world.pending; },
    beginMutation() { return { settle() {} }; }, inheritStdio: (s: any) => s,
    stop() { world.stopped = true; }, stopped: () => world.stopped, assertSettled() {},
    finish() { events.push("finish"); spies.finishes++; if (world.finishThrows) throw new Error("finish"); },
  };
  options.ownership = { ...options.ownership,
    beginReclaim: () => { events.push("begin"); spies.begins++; return scope as never; },
    reacquireShared: async () => { events.push("handover"); calls.handovers++; } };
  const tx = {};
  options.deletion = {
    enableForTest: ENABLE_DELETION_FOR_TEST,
    authority: {
      withTransaction: async (fn: (t: unknown) => Promise<unknown>) => {
        world.activeTx = tx;
        try { return await fn(tx); } finally { world.activeTx = undefined; }
      },
      active: () => world.activeTx,
      endSourceUse: async (paths: Iterable<string>, opts: unknown) => {
        spies.ended.push({ paths: [...paths], options: opts }); return { ended: [...paths], kept: [], endedSlots: [] };
      },
    } as never,
    reserve: (async (runId: string, action: (eligible: () => boolean) => Promise<unknown>) => {
      spies.reserved.push(runId);
      return world.reserveRefused ? undefined : action(() => world.eligible);
    }) as never,
    quiesce: async () => true,
    git: {
      assertNoUnsettledDeletes: () => { if (world.unsettled) throw new Error("unsettled runner delete outstanding"); },
      readEpochJournalVector: async (_bare: string, branch: string) => {
        spies.journalReads.push(branch);
        const i = Number(branch.slice(1));
        return world.journal(branch, "/tree/" + i, "run" + i);
      },
      createEpochDiscardProof: (input: any) => {
        if (world.proofThrows) throw new Error("invalid epoch discard proof");
        spies.proofs.push(input); return { input } as never;
      },
      discardStoppedSource: async (opts: any) => {
        const input = opts.epochProof.input;
        const liveDuring = (() => { try { input.checkLive(); return true; } catch { return false; } })();
        const holdsSeen = opts.holds;
        spies.discards.push({ opts, liveDuring, holdsSeen });
        return world.discard(opts, input.paths[0]);
      },
    },
  };
  tune?.(world);
  return world;
}
async function withDeletion(count: number, tune: ((w: ReturnType<typeof makeWorld>) => void) | undefined,
    check: (w: ReturnType<typeof makeWorld>, result: NonNullable<Awaited<ReturnType<typeof runPodEpochShadow>>>) => void | Promise<void>) {
  const d = await daemon();
  try {
    const w = deletionWorld(count, tune);
    w.options.dockerHost = d.host;
    await check(w, (await runPodEpochShadow(w.options))!);
  } finally { await d.close(); }
}
const noMutation = (w: ReturnType<typeof makeWorld>) => {
  assert.equal(w.spies.proofs.length, 0); assert.equal(w.spies.discards.length, 0);
  assert.equal(w.spies.ended.length, 0); assert.equal(w.spies.reserved.length, 0);
};

const withoutSentinel = (activation?: boolean) => (w: ReturnType<typeof makeWorld>) => {
  const deps = w.options.deletion as { enableForTest?: symbol; activation?: boolean };
  delete deps.enableForTest;
  if (activation !== undefined) deps.activation = activation;
};
test("off: no sentinel and activation absent or false leaves no scope, proof or mutation for an eligible attested pass", async () => {
  for (const activation of [undefined, false]) {
    await withDeletion(2, withoutSentinel(activation), (w, result) => {
      assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "shadow_complete");
      assert.equal(result.authority.result, "attested"); assert.equal(result.would_delete, 2);
      noMutation(w); assert.equal(w.spies.begins, 0); assert.equal(w.spies.journalReads.length, 0);
      assert.equal(result.deleted + result.settled + result.kept_after_refusal + result.kept_residue, 0);
      assert.equal(result.reclaim_bytes_state, "unknown");
      assert.deepEqual(w.events, ["handover"]);
    });
  }
});
test("on: no sentinel and activation true deletes and settles exactly like the sentinel path", async () => {
  const run = async (tune: (w: ReturnType<typeof makeWorld>) => void) => {
    let seen: { result: unknown; events: string[]; reserved: string[]; ended: string[][]; proofs: unknown[] } | undefined;
    await withDeletion(3, w => {
      w.mutateSource = (s, i) => { if (i === 1) { s.presence = "missing"; s.sources[0]!.presence = "missing"; s.sources[0]!.allocatedBytes = null; } };
      w.discard = (_o, path) => ({ kind: "done", sources: [{ clonePath: path, outcome: path === "/tree/1" ? "missing" : "removed" }],
        metadata: "retired", tookOver: true });
      tune(w);
    }, (w, result) => {
      seen = { result, events: w.events, reserved: w.spies.reserved, ended: w.spies.ended.map(e => e.paths),
        proofs: w.spies.proofs.map(p => p.paths) };
    });
    return seen!;
  };
  const sentinel = await run(() => {});
  const active = await run(withoutSentinel(true));
  const strip = (r: any) => ({ ...r.result, elapsed_ms: undefined });
  assert.equal((active.result as any).deleted, 2); assert.equal((active.result as any).settled, 1);
  assert.equal((active.result as any).reason, "reclaim_complete");
  assert.deepEqual(active.events, ["begin", "finish", "handover"]);
  assert.deepEqual(active.reserved, ["run0", "run1", "run2"]);
  assert.deepEqual(active.ended, [["/tree/0"], ["/tree/1"], ["/tree/2"]]);
  assert.deepEqual(active.events, sentinel.events);
  assert.deepEqual(active.proofs, sentinel.proofs);
  assert.equal((active.result as any).deleted_bytes, (sentinel.result as any).deleted_bytes);
  assert.equal(strip(active).verdict, strip(sentinel).verdict);
});
test("on, still non-effectful: activation never deletes after an early refusal, a partial verdict, a missing native scope or absent deps", async () => {
  // The unattested-authority and aborted-pass cases below end the pass with a refusal before the deletion gate
  // is evaluated, so they pin those refusals, not the gate's own `authority.result === "attested"` and
  // `!controller.signal.aborted` conjuncts. The gate is reached only after decide("attested") and a synchronous
  // check() that throws on an abort, so no test-world pass arrives there unattested or aborted; those two
  // conjuncts stay unpinned.
  const cases: Array<[string, (w: ReturnType<typeof makeWorld>) => void | Promise<void>]> = [
    ["partial inventory", w => {
      const discover = w.options.git.discoverEpochRetainedSources;
      w.options.git.discoverEpochRetainedSources = async o => ({ ...await discover(o), inventoryComplete: false, unreadable: 1 });
    }],
    ["unattested authority", async w => { const raw = await fixture("v2/valid/contradicted.json"); w.options.readEvidence = async () => raw; }],
    ["aborted pass", w => { w.parent.abort(); }],
    ["no beginReclaim", w => { delete (w.options.ownership as { beginReclaim?: unknown }).beginReclaim; }],
    ["absent deps", w => { delete w.options.deletion; }],
  ];
  for (const [label, tune] of cases) {
    const d = await daemon();
    try {
      const w = deletionWorld(2); withoutSentinel(true)(w); await tune(w); w.options.dockerHost = d.host;
      const result = await runPodEpochShadow(w.options);
      noMutation(w); assert.equal(w.spies.begins, 0, label); assert.equal(w.spies.journalReads.length, 0, label);
      assert.equal((result?.deleted ?? 0) + (result?.settled ?? 0), 0, label);
      assert.notEqual(result?.reason, "reclaim_complete", label);
    } finally { await d.close(); }
  }
});
test("#3851 with activation on, measured candidates proceed, a locally short part stays kept and a pass-wide partial never deletes", async () => {
  // A tree-local shortfall seen by the shadow keeps only that source.
  await withDeletion(3, w => {
    withoutSentinel(true)(w);
    const base = w.mutateSource;
    w.mutateSource = (s, i) => {
      base(s, i);
      if (i === 1) { s.bytesState = "unknown"; s.allocatedBytes = null; s.sources[0]!.allocatedBytes = null; s.sources[0]!.measurementShortfall = "eacces"; }
    };
  }, (w, result) => {
    assert.equal(result.would_delete, 2); assert.equal(result.deleted, 2);
    assert.deepEqual(w.spies.proofs.map(p => p.paths), [["/tree/0"], ["/tree/2"]]);
    assert.ok(w.spies.reserved.every(run => run !== "run1"), "the short source is never reserved");
    assert.ok(w.spies.ended.every(e => !e.paths.includes("/tree/1")));
  });
  // A pass-wide partial inspection never enters the deletion phase.
  await withDeletion(2, w => {
    withoutSentinel(true)(w);
    const inspect = w.options.git.inspectRetainedSources;
    w.options.git.inspectRetainedSources = async o => ({ ...await inspect(o), partial: true });
  }, (w, result) => {
    noMutation(w); assert.equal(w.spies.begins, 0); assert.equal(result.verdict, "partial");
    assert.equal(result.deleted + result.settled, 0);
  });
  // A re-inspection that turns short refuses that source only (activation path).
  await withDeletion(2, w => {
    withoutSentinel(true)(w);
    const inspections = [0, 0];
    w.mutateSource = (s, i) => {
      if (++inspections[i]! === 2 && i === 0) {
        s.bytesState = "unknown"; s.allocatedBytes = null;
        s.sources[0]!.allocatedBytes = null; s.sources[0]!.measurementShortfall = "eacces";
      }
    };
  }, (w, result) => {
    assert.equal(result.kept_after_refusal, 1); assert.equal(result.deleted, 1);
    assert.deepEqual(w.spies.proofs.map(p => p.paths), [["/tree/1"]]);
  });
});
test("nothing under agent/src references the deletion override, which only pod-epoch-reclaim.ts defines", async () => {
  const root = fileURLToPath(new URL("../src/", import.meta.url));
  const files = (await readdir(root, { recursive: true })).filter(f => /\.(ts|mts|cts|js|mjs|cjs)$/.test(f));
  assert.ok(files.length > 50);
  const referencing: string[] = [];
  for (const f of files) {
    const text = await readFile(join(root, f), "utf8");
    if (/enableForTest|ENABLE_DELETION_FOR_TEST|enable-deletion/.test(text)) referencing.push(f);
  }
  assert.deepEqual(referencing, ["pod-epoch-reclaim.ts"]);
  const main = await readFile(join(root, "main.ts"), "utf8");
  assert.ok(!main.includes("enableForTest"));
  const definition = await readFile(join(root, "pod-epoch-reclaim.ts"), "utf8");
  assert.equal(definition.match(/enableForTest\s*===\s*TEST_ENABLE_DELETION\b/g)?.length, 1,
    "exactly one comparison of enableForTest against the sentinel");
  assert.equal(definition.match(/TEST_ENABLE_DELETION\b/g)?.length, 2, "the sentinel is defined once and compared once");
  const built = buildPodEpochDeletionDeps({ authority: {} as never, runner: { withSourceReservation: async () => undefined },
    git: {} as never, dockerHost: undefined, activation: false });
  assert.equal("enableForTest" in built, false, "the deps object main.ts builds has no enableForTest key");
  assert.equal(built.activation, false);
  assert.equal(definition.match(/deps\.activation === true \|\| deps\.enableForTest/g)?.length, 1, "one gate expression");
  assert.ok(!definition.includes("EPOCH_RECLAIM_DELETION_ENABLED"), "the compile-time constant is gone");
});
test("an eligible attested pass with the override deletes present sources, settles missing ones, then finishes before handover", async () => {
  await withDeletion(3, w => {
    w.mutateSource = (s, i) => { if (i === 1) { s.presence = "missing"; s.sources[0]!.presence = "missing"; s.sources[0]!.allocatedBytes = null; } };
    w.discard = (_o, path) => ({ kind: "done", sources: [{ clonePath: path, outcome: path === "/tree/1" ? "missing" : "removed" }],
      metadata: "retired", tookOver: true });
  }, (w, result) => {
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
    assert.equal(result.deleted, 2); assert.equal(result.deleted_bytes, 8192); assert.equal(result.settled, 1);
    assert.equal(result.reclaim_bytes_state, "measured");
    // The shadow fields keep their shadow values.
    assert.equal(result.would_delete, 2); assert.equal(result.would_settle, 1); assert.equal(result.would_delete_bytes, 8192);
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
    assert.deepEqual(w.spies.reserved, ["run0", "run1", "run2"]);
    assert.deepEqual(w.spies.ended.map(e => e.paths), [["/tree/0"], ["/tree/1"], ["/tree/2"]]);
    assert.deepEqual(w.spies.ended[0]!.options, { skipFlightOwned: true });
    for (const proof of w.spies.proofs) {
      assert.deepEqual(proof.decision, { eligible: true, attested: true });
      assert.equal(proof.confirm, DESCRIPTOR); assert.equal(proof.paths.length, 1);
      assert.deepEqual(JSON.parse(proof.expectedRawJournal), { attemptId: "attempt", clonePath: proof.paths[0], runId: proof.selector.runId });
    }
    assert.ok(w.spies.discards.every(x => x.liveDuring), "checkLive holds inside the reservation and transaction");
    assert.deepEqual(w.spies.discards[0]!.holdsSeen, { state: "read", holds: [] });
  });
});
test("checkLive fails when the reservation is revoked or the transaction ended", async () => {
  await withDeletion(1, w => {
    const base = w.discard;
    w.discard = (o, path) => { w.eligible = false; return base(o, path); };
  }, async w => {
    const input = w.spies.proofs[0];
    assert.throws(() => input.checkLive(), /reservation or transaction lost/);
    w.eligible = true;
    assert.throws(() => input.checkLive(), /reservation or transaction lost/, "no transaction is active after the run");
  });
});
test("the reclaim phase runs only for eligible, attested, intact passes with a native scope and deps", async () => {
  const cases: Array<[string, (w: ReturnType<typeof makeWorld>) => void | Promise<void>]> = [
    ["shadow-only (no deletion deps)", w => { delete w.options.deletion; }],
    ["beginReclaim absent", w => { delete (w.options.ownership as { beginReclaim?: unknown }).beginReclaim; }],
    ["undeclared runtime", w => { w.options.bootstrap = { ...w.options.bootstrap, firstMode: "shared", reason: "runtime_not_declared" }; }],
    ["legacy evidence", async w => { const raw = await fixture("valid/positive.json"); w.options.readEvidence = async () => raw; }],
    ["contradicted attestation", async w => { const raw = await fixture("v2/valid/contradicted.json"); w.options.readEvidence = async () => raw; }],
    ["partial inventory", w => {
      const discover = w.options.git.discoverEpochRetainedSources;
      w.options.git.discoverEpochRetainedSources = async o => ({ ...await discover(o), inventoryComplete: false, unreadable: 1 });
    }],
    ["partial inspection", w => {
      const inspect = w.options.git.inspectRetainedSources;
      w.options.git.inspectRetainedSources = async o => ({ ...await inspect(o), partial: true });
    }],
    ["unsupported filesystem", w => { w.options.ownership = { ...w.options.ownership, mode: "unsupported" }; }],
  ];
  for (const [label, tune] of cases) {
    const d = await daemon();
    try {
      const w = deletionWorld(2); await tune(w); w.options.dockerHost = d.host;
      const result = (await runPodEpochShadow(w.options))!;
      noMutation(w); assert.equal(w.spies.begins, 0, label); assert.equal(w.spies.journalReads.length, 0, label);
      assert.equal(result.deleted + result.settled, 0, label); assert.notEqual(result.reason, "reclaim_complete", label);
    } finally { await d.close(); }
  }
});
test("a daemon veto or an invalidated pass never starts the reclaim phase", async () => {
  const d = await daemon();
  try {
    const w = deletionWorld(1); w.options.dockerHost = d.host;
    const inner = w.options.git.inspectRetainedSources;
    w.options.git.inspectRetainedSources = async o => { d.veto(); await tick(); await tick(); return inner(o); };
    const result = (await runPodEpochShadow(w.options))!;
    noMutation(w); assert.equal(w.spies.begins, 0); assert.equal(result.authority.reason, "daemon_veto");
  } finally { await d.close(); }
});

test("deletion needs a terminal run, an empty holds list and a matching run_open_holds of exactly zero", async () => {
  const cases: Array<[string, (w: ReturnType<typeof makeWorld>) => void]> = [
    ["non-terminal on the fresh read", w => { let n = 0; w.ownership = () => ({ status: ++n === 1 ? "completed" : "running" }); }],
    ["unknown holds (malformed response)", w => { let n = 0; w.holds = id => ++n === 1 ? ({ run_id: id, holds: [], run_open_holds: { run_id: id, count: 0 } }) : ({ run_id: "wrong", holds: [] } as never); }],
    ["a hold of this worker", w => { w.holds = id => ({ run_id: id, holds: [{ hold_id: "h", generation: 1 } as never], run_open_holds: { run_id: id, count: 0 } }); }],
    ["a run-wide hold elsewhere (count > 0)", w => { w.holds = id => ({ run_id: id, holds: [], run_open_holds: { run_id: id, count: 1 } }); }],
    ["absent run_open_holds", w => { w.holds = id => ({ run_id: id, holds: [] }); }],
    ["mismatched run_open_holds run", w => { w.holds = id => ({ run_id: id, holds: [], run_open_holds: { run_id: "other", count: 0 } }); }],
    ["non-numeric count", w => { w.holds = id => ({ run_id: id, holds: [], run_open_holds: { run_id: id, count: "0" as never } }); }],
    ["negative count", w => { w.holds = id => ({ run_id: id, holds: [], run_open_holds: { run_id: id, count: -1 } }); }],
    ["failed fresh read", w => { let n = 0; w.holds = id => { if (++n > 1) throw new Error("read failed"); return ({ run_id: id, holds: [], run_open_holds: { run_id: id, count: 0 } }); }; }],
  ];
  for (const [label, tune] of cases) {
    // The first read is the shadow's and the later ones are the deletion-side fresh reads.
    await withDeletion(1, tune, (w, result) => {
      noMutation(w); assert.equal(result.deleted, 0, label);
      if (label === "a hold of this worker") {
        // Not even a shadow candidate: the shadow rule is unchanged and keeps it.
        assert.equal(result.would_delete, 0, label); assert.equal(result.kept_after_refusal, 0, label);
        return;
      }
      assert.equal(result.would_delete, 1, label + ": the shadow verdict is unchanged");
      assert.equal(result.kept_after_refusal, 1, label);
      assert.equal(result.verdict, "partial", label); assert.equal(result.reason, "reclaim_partial", label);
      assert.equal(w.events.at(-1), "handover", label);
    });
  }
});
test("an old api (no run_open_holds anywhere) keeps every source while the shadow verdict is unchanged", async () => {
  await withDeletion(3, w => { w.holds = id => ({ run_id: id, holds: [] }); }, (w, result) => {
    noMutation(w);
    assert.equal(result.would_delete, 3); assert.equal(result.authority.result, "attested");
    assert.equal(result.kept_after_refusal, 3); assert.equal(result.deleted, 0);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "reclaim_partial");
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
});
test("a definitive exact ownership 404 keeps only that run's sources and does not make the pass partial", async () => {
  const d = await daemon();
  try {
    const w = deletionWorld(2); w.options.dockerHost = d.host;
    const requests: string[] = [];
    w.options.client = new WorkerClient("http://example.invalid", "inert", "test", nullLogger(), {
      fetch: async (url, init) => {
        assert.equal(init?.method, "GET");
        const path = new URL(String(url)).pathname; requests.push(path);
        if (path.endsWith("/runs/run0/ownership"))
          return new Response(JSON.stringify({ error: "run not found for this worker" }), { status: 404 });
        if (path.endsWith("/ownership")) return new Response(JSON.stringify({ status: "completed" }));
        const run_id = path.split("/").at(-2)!;
        return new Response(JSON.stringify({ run_id, holds: [], run_open_holds: { run_id, count: 0 } }));
      },
    });
    const result = (await runPodEpochShadow(w.options))!;
    assert.deepEqual(result.partial_causes, {});
    assert.equal(result.would_delete, 1); assert.equal(result.kept, 1);
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
    assert.equal(result.deleted, 1); assert.deepEqual(w.spies.reserved, ["run1"]);
    assert.equal(requests.filter(p => p.includes("/runs/run0/")).every(p => p.endsWith("/ownership")), true,
      "no holds read for the 404 run");
    assert.equal(requests.filter(p => p.includes("/runs/run0/")).length, 1);
  } finally { await d.close(); }
});
test("a 404 with any other body, or on holds, is still a partial pass", async () => {
  for (const [stage, body] of [["ownership", { error: "not found" }], ["ownership", { error: "run not found for this worker", extra: 1 }], ["holds", { error: "run not found for this worker" }]] as const) {
    const d = await daemon();
    try {
      const w = deletionWorld(1); w.options.dockerHost = d.host;
      w.options.client = new WorkerClient("http://example.invalid", "inert", "test", nullLogger(), {
        fetch: async url => {
          const path = new URL(String(url)).pathname;
          if (path.endsWith("/ownership") && stage === "holds") return new Response(JSON.stringify({ status: "completed" }));
          return new Response(JSON.stringify(body), { status: 404 });
        },
      });
      const result = (await runPodEpochShadow(w.options))!;
      assert.equal(result.verdict, "partial", stage + JSON.stringify(body)); noMutation(w);
      assert.equal(Object.keys(result.partial_causes).length, 1);
    } finally { await d.close(); }
  }
});
test("a refused reservation or a missing transaction keeps the source and admits nothing", async () => {
  await withDeletion(1, w => { w.reserveRefused = true; }, (w, result) => {
    assert.deepEqual(w.spies.reserved, ["run0"]); assert.equal(w.spies.proofs.length, 0); assert.equal(w.spies.discards.length, 0);
    assert.equal(result.kept_after_refusal, 1); assert.equal(result.verdict, "partial");
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
  await withDeletion(1, w => {
    (w.options.deletion!.authority as { withTransaction: unknown }).withTransaction = async () => { throw new Error("authority lock timeout"); };
  }, (w, result) => {
    noMutation(w); assert.equal(result.kept_after_refusal, 1); assert.equal(result.reason, "reclaim_partial");
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
  await withDeletion(1, w => { w.proofThrows = true; }, (w, result) => {
    assert.equal(w.spies.discards.length, 0); assert.equal(result.kept_after_refusal, 1); assert.equal(result.verdict, "partial");
  });
});
test("a pre-effect refusal keeps its source, calls no end for it, and the pass continues to the next source", async () => {
  await withDeletion(2, w => {
    w.discard = (_o, path) => path === "/tree/0" ? { kind: "refused", reason: "not_quiescent", consumers: [] } :
      { kind: "done", sources: [{ clonePath: path, outcome: "removed" }], metadata: "retired", tookOver: true };
  }, (w, result) => {
    assert.equal(result.kept_after_refusal, 1); assert.equal(result.deleted, 1);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "reclaim_partial");
    assert.deepEqual(w.spies.ended.map(e => e.paths), [["/tree/1"]]);
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
  // A refusal after a takeover still ends the takeover.
  await withDeletion(1, w => { w.discard = () => ({ kind: "refused", reason: "x", consumers: [], tookOver: true }); }, (w, result) => {
    assert.deepEqual(w.spies.ended.map(e => e.paths), [["/tree/0"]]); assert.equal(result.kept_after_refusal, 1);
  });
});
test("a settled residue failure keeps the source as kept_residue; a settled incomplete never throws", async () => {
  await withDeletion(2, w => {
    w.discard = (_o, path) => path === "/tree/0" ?
      { kind: "incomplete", sources: [{ clonePath: path, outcome: "retained" }], metadata: "kept", tookOver: true } :
      { kind: "done", sources: [{ clonePath: path, outcome: "removed" }], metadata: "retired", tookOver: true };
  }, (w, result) => {
    assert.equal(result.kept_residue, 1); assert.equal(result.deleted, 1);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "reclaim_partial");
    assert.deepEqual(w.events, ["begin", "finish", "handover"]);
  });
});
test("structural keeps never reach the authority path and do not downgrade the verdict", async () => {
  const reads = { ownership: 0, holds: 0 };
  await withDeletion(4, w => {
    w.groups[3]!.paths = ["/tree/3", "/tree/3b"];
    w.mutateSource = (s, i) => {
      if (i === 0) s.disposition = { kind: "local_protected_discard" };
      if (i === 1) s.kind = "canonical";
    };
    w.journal = (_branch, path, runId) => _branch === "b2" ? [] : [JSON.stringify({ attemptId: "attempt", clonePath: path, runId })];
    // Count the api reads; the shadow phase makes exactly one of each per group (4 groups).
    const ownership = w.options.client.getRunOwnership, holds = w.options.client.listRecoveryHolds;
    w.options.client.getRunOwnership = async (...args: Parameters<typeof ownership>) => { reads.ownership++; return ownership(...args); };
    w.options.client.listRecoveryHolds = async (...args: Parameters<typeof holds>) => { reads.holds++; return holds(...args); };
  }, (w, result) => {
    assert.equal(reads.ownership, 4, "no deletion-phase ownership read for a structural keep");
    assert.equal(reads.holds, 4, "no deletion-phase holds read for a structural keep");
    assert.equal(result.would_delete, 5, "the shadow counts every candidate path");
    assert.equal(result.kept_protected, 1);
    assert.equal(result.kept_unrecoverable_shape, 4, "canonical, a no-key attempt and both parts of a two-part source");
    assert.equal(w.spies.proofs.length, 0); assert.equal(w.spies.discards.length, 0); assert.equal(w.spies.reserved.length, 0);
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
    assert.equal(result.deleted + result.settled, 0);
  });
});
test("journal shapes other than the exact strict string or [\"\"] are refused before any proof", async () => {
  const raw = (path: string, runId: string, extra: object = {}) => JSON.stringify({ attemptId: "attempt", clonePath: path, runId, ...extra });
  type Tune = (s: RetainedSourceEvidence) => void;
  const ledger = (state: string, missing = false): Tune => s => {
    s.ledgerState = state as never; s.sources[0]!.ledgerState = state as never;
    if (missing) { s.presence = "missing"; s.sources[0]!.presence = "missing"; s.sources[0]!.allocatedBytes = null; }
  };
  const strict = (_b: string, p: string, r: string) => [raw(p, r)];
  const empty = () => [""];
  const shapes: Array<[string, (b: string, p: string, r: string) => Array<string | null>, boolean, Tune?]> = [
    ["no key", () => [], false], ["valueless", () => [null], false], ["duplicate", (_b, p, r) => [raw(p, r), raw(p, r)], false],
    ["extra field", (_b, p, r) => [raw(p, r, { restoreTip: "x" })], false], ["wrong run", (_b, p) => [raw(p, "other")], false],
    ["wrong path", (_b, _p, r) => [raw("/elsewhere", r)], false], ["non-canonical json", (_b, p, r) => [raw(p, r) + " "], false],
    ["not json", () => ["{"], false], ["array", () => ["[]"], false],
    // (a) live + strict, present or missing
    ["strict string, ledger live", strict, true], ["strict string, live, missing", strict, true, ledger("live", true)],
    // (c) retired + strict + missing footprint; (b) retired + [""] + missing footprint
    ["strict, retired, missing", strict, true, ledger("retired", true)],
    ["explicit empty, retired, missing", empty, true, ledger("retired", true)],
    // every other ledger/footprint combination is an unrecoverable shape
    ["strict, retired, present", strict, false, ledger("retired")],
    ["explicit empty, retired, present", empty, false, ledger("retired")],
    ["explicit empty, ledger live", empty, false], ["explicit empty, live, missing", empty, false, ledger("live", true)],
    ["explicit empty, reclaimed", empty, false, ledger("reclaimed")], ["explicit empty, reclaimed, missing", empty, false, ledger("reclaimed", true)],
    ["explicit empty, abandoned", empty, false, ledger("abandoned")],
    ["strict, reclaimed", strict, false, ledger("reclaimed")], ["strict, abandoned", strict, false, ledger("abandoned")],
    ["strict, ledger none", strict, false, ledger("none")],
  ];
  for (const [label, journal, proceeds, tune] of shapes) {
    await withDeletion(1, w => { w.journal = journal; if (tune) w.mutateSource = s => tune(s); }, (w, result) => {
      if (!proceeds) assert.equal(w.spies.reserved.length, 0, label + ": no reservation for a structural keep");
      assert.equal(w.spies.proofs.length, proceeds ? 1 : 0, label);
      assert.equal(result.kept_unrecoverable_shape, proceeds ? 0 : 1, label);
      assert.equal(result.deleted, proceeds ? 1 : 0, label);
      assert.equal(result.verdict, "eligible", label); assert.equal(result.reason, "reclaim_complete", label);
    });
  }
});
test("a no-key attempt on a first roll (no run_open_holds) is a structural keep before any authority read and never downgrades", async () => {
  const reads = { ownership: 0, holds: 0 };
  await withDeletion(1, w => {
    w.journal = () => [];
    // The api reads the predecessor's capabilities on the first roll, so there is no run_open_holds count.
    w.holds = run_id => ({ run_id, holds: [] });
    const ownership = w.options.client.getRunOwnership, holds = w.options.client.listRecoveryHolds;
    w.options.client.getRunOwnership = async (...args: Parameters<typeof ownership>) => { reads.ownership++; return ownership(...args); };
    w.options.client.listRecoveryHolds = async (...args: Parameters<typeof holds>) => { reads.holds++; return holds(...args); };
  }, (w, result) => {
    assert.equal(result.kept_unrecoverable_shape, 1); assert.equal(result.kept_after_refusal, 0);
    assert.equal(reads.ownership, 1, "only the shadow read"); assert.equal(reads.holds, 1, "only the shadow read");
    assert.equal(w.spies.proofs.length, 0); assert.equal(w.spies.reserved.length, 0); assert.equal(w.spies.discards.length, 0);
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
  });
  // A strict attempt on the same first roll is still an authority refusal (not a shape keep) and downgrades.
  await withDeletion(1, w => { w.holds = run_id => ({ run_id, holds: [] }); }, (w, result) => {
    assert.equal(result.kept_unrecoverable_shape, 0); assert.equal(result.kept_after_refusal, 1);
    assert.equal(result.verdict, "partial"); assert.equal(w.spies.proofs.length, 0);
  });
});
test("fatal outcomes throw PodEpochSettlementError, keep EX and never reach the shared handover", async () => {
  const sources = (path: string) => [{ clonePath: path, outcome: "unknown" as const }];
  const cases: Array<[string, (w: ReturnType<typeof makeWorld>) => void]> = [
    ["unknown result", w => { w.discard = (_o, path) => ({ kind: "unknown", sources: sources(path), metadata: "kept", tookOver: true }); }],
    ["pending child after done", w => { w.discard = (_o, path) => { w.pending = 1; return { kind: "done", sources: [{ clonePath: path, outcome: "removed" }], metadata: "retired", tookOver: true }; }; }],
    ["stop latch after refusal", w => { w.discard = () => { w.stopped = true; return { kind: "refused", reason: "x", consumers: [] }; }; }],
    ["stop latch after incomplete", w => { w.discard = (_o, path) => { w.stopped = true; return { kind: "incomplete", sources: [{ clonePath: path, outcome: "retained" }], metadata: "kept", tookOver: true }; }; }],
    ["done with no outcome", w => { w.discard = () => ({ kind: "done", sources: [], metadata: "retired" }); }],
    ["discard throws after effects", w => { w.discard = () => { throw new Error("boom"); }; }],
    ["scope.finish throws", w => { w.finishThrows = true; }],
  ];
  for (const [label, tune] of cases) {
    const d = await daemon();
    // A fatal refusal deliberately leaves the daemon guard open (the process exits in production); release its
    // timers here so the test process can.
    const timers: Array<() => void> = [];
    try {
      const w = deletionWorld(2); tune(w); w.options.dockerHost = d.host;
      w.options.schedule = (callback, ms) => { const t = setTimeout(callback, ms); const c = () => clearTimeout(t); timers.push(c); return c; };
      await assert.rejects(runPodEpochShadow(w.options), PodEpochSettlementError, label);
      assert.equal(w.calls.handovers, 0, label); assert.ok(!w.events.includes("handover"), label);
      if (label !== "scope.finish throws") assert.equal(w.spies.proofs.length, 1, label + ": no second source after a fatal one");
    } finally { for (const cancel of timers) cancel(); await d.close(); }
  }
});
test("the coordinator itself is fatal for a pending child or stop latch after a discard, even when finish would succeed", async () => {
  for (const latch of ["pending", "stopped"] as const) {
    const w = deletionWorld(2, world => {
      world.discard = (_o, path) => {
        if (latch === "pending") world.pending = 1; else world.stopped = true;
        return { kind: "done", sources: [{ clonePath: path, outcome: "removed" }], metadata: "retired", tookOver: true };
      };
    });
    const d = await daemon(); w.options.dockerHost = d.host;
    const timers: Array<() => void> = [];
    w.options.schedule = (callback, ms) => { const t = setTimeout(callback, ms); const c = () => clearTimeout(t); timers.push(c); return c; };
    try {
      await assert.rejects(runPodEpochShadow(w.options), PodEpochSettlementError, latch);
      assert.equal(w.spies.finishes, 0, latch + ": finish is never reached");
      assert.equal(w.calls.handovers, 0, latch); assert.ok(!w.events.includes("handover"), latch);
      assert.deepEqual(w.spies.reserved, ["run0"], latch + ": the second source never starts");
      assert.equal(w.spies.discards.length, 1, latch);
    } finally { for (const cancel of timers) cancel(); await d.close(); }
  }
});
// A stop latched during a REFUSED re-inspect (the descriptor changed, so the source is kept) must still be fatal
// at once: the refusal path may not skip the settlement check.
async function refusedReinspectWithLatch(count: number) {
  const w = deletionWorld(count, world => {
    const seen = new Map<number, number>();
    world.mutateSource = (s, i) => {
      const n = (seen.get(i) ?? 0) + 1; seen.set(i, n);
      if (i === 0 && n === 2) { world.stopped = true; s.descriptor = "1".repeat(64); }
    };
  });
  const d = await daemon(); w.options.dockerHost = d.host;
  const timers: Array<() => void> = [];
  w.options.schedule = (callback, ms) => { const t = setTimeout(callback, ms); const c = () => clearTimeout(t); timers.push(c); return c; };
  try {
    await assert.rejects(runPodEpochShadow(w.options), PodEpochSettlementError);
    assert.equal(w.spies.finishes, 0, "finish is never reached");
    assert.equal(w.calls.handovers, 0); assert.ok(!w.events.includes("handover"));
    return w;
  } finally { for (const cancel of timers) cancel(); await d.close(); }
}
// A Git delete left unsettled before the pass (GitCache's process-wide unsettled map; defence in depth for any
// delete the process records before or during the pass) is fatal for the pass even when every candidate is only
// refused or settled by metadata, which never consult that map themselves.
test("an unsettled Git delete is fatal: EX kept, no finish, no handover", async () => {
  const missing = (s: RetainedSourceEvidence) => {
    s.ledgerState = "retired" as never; s.sources[0]!.ledgerState = "retired" as never;
    s.presence = "missing"; s.sources[0]!.presence = "missing"; s.sources[0]!.allocatedBytes = null;
  };
  // `before` marks the cases where the delete is outstanding when the deletion phase starts: the pre-scope check
  // alone must end the pass, so no scope opens and nothing is proved, discarded or quiesced.
  const cases: Array<[string, (w: ReturnType<typeof makeWorld>) => void, number?]> = [
    ["outstanding before a pre-effect refusal", w => { w.unsettled = true; w.ownership = (() => { let n = 0; return () => ({ status: ++n === 1 ? "completed" : "running" }); })(); }],
    ["outstanding before a metadata-only repair", w => {
      w.unsettled = true; w.journal = () => [""]; w.mutateSource = missing;
      w.discard = (_o, path) => ({ kind: "done", sources: [{ clonePath: path, outcome: "missing" }], metadata: "retired" });
    }],
    ["outstanding before a pass with zero admitted candidates", w => { w.unsettled = true; }, 0],
    ["raised during a refusal, found at finish", w => {
      w.discard = () => { w.unsettled = true; return { kind: "refused", reason: "x", consumers: [] }; };
    }],
    ["raised during a metadata-only repair, found at finish", w => {
      w.journal = () => [""]; w.mutateSource = missing;
      w.discard = (_o, path) => { w.unsettled = true; return { kind: "done", sources: [{ clonePath: path, outcome: "missing" }], metadata: "retired" }; };
    }],
  ];
  for (const [label, tune, count = 1] of cases) {
    const d = await daemon();
    const timers: Array<() => void> = [];
    try {
      const w = deletionWorld(count, tune); w.options.dockerHost = d.host;
      let quiesces = 0;
      w.options.deletion!.quiesce = async () => { quiesces++; return true; };
      w.options.schedule = (callback, ms) => { const t = setTimeout(callback, ms); const c = () => clearTimeout(t); timers.push(c); return c; };
      await assert.rejects(runPodEpochShadow(w.options), PodEpochSettlementError, label);
      assert.equal(w.spies.finishes, 0, label + ": finish is never reached");
      assert.equal(w.calls.handovers, 0, label); assert.ok(!w.events.includes("handover"), label);
      if (label.startsWith("outstanding before")) {
        noMutation(w);
        assert.equal(w.spies.begins, 0, label + ": no scope opened"); assert.equal(quiesces, 0, label + ": no quiesce");
      }
    } finally { for (const cancel of timers) cancel(); await d.close(); }
  }
});
test("a stop latched during a refused re-inspect is fatal, with no finish and no handover", async () => {
  const w = await refusedReinspectWithLatch(1);
  noMutation(w);
});
test("after a refusal latches a stop, the next source is never reserved, proved or discarded", async () => {
  const w = await refusedReinspectWithLatch(2);
  noMutation(w);
  assert.deepEqual(w.spies.reserved, []);
});
test("shutdown mid-pass starts no new source and leaves the handover to the process exit", async () => {
  const reads: string[] = [];
  await withDeletion(3, w => {
    const base = w.discard;
    w.discard = (o, path) => { w.parent.abort(); return base(o, path); };
    const own = w.options.client.getRunOwnership;
    w.options.client.getRunOwnership = (run, o) => { if (w.spies.begins > 0) reads.push(run); return own(run, o); };
  }, (w, result) => {
    // The soft stop is checked at the loop top: the second source is neither kept nor read.
    assert.equal(result.kept_after_refusal, 0); assert.ok(reads.length > 0 && reads.every(run => run === "run0"), "no authority read for the second source: " + reads.join());
    assert.equal(w.spies.proofs.length, 1); assert.equal(result.deleted, 1);
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "shutdown");
    assert.deepEqual(w.events, ["begin", "finish"]);
  });
});
test("a path or entry change between the shadow and the recheck keeps the source", async () => {
  await withDeletion(1, w => {
    let n = 0;
    w.mutateSource = s => { if (++n > 1) s.descriptor = "1".repeat(64); };
  }, (w, result) => {
    noMutation(w); assert.equal(result.kept_after_refusal, 1); assert.equal(result.verdict, "partial");
  });
  await withDeletion(1, w => {
    let n = 0;
    w.mutateSource = s => { if (++n > 1) s.sources[0]!.live = true; };
  }, (w, result) => { noMutation(w); assert.equal(result.kept_after_refusal, 1); });
});

test("#3851 a deletion re-inspection that is locally short but not partial refuses that source only", async () => {
  await withDeletion(2, w => {
    const inspections = [0, 0];
    w.mutateSource = (s, i) => {
      // The second inspection of source 0 is the deletion re-inspection: a tree-local shortfall leaves
      // the inspection non-partial, yet the present source is no longer byte-measured.
      if (++inspections[i]! === 2 && i === 0) {
        s.bytesState = "unknown"; s.allocatedBytes = null;
        s.sources[0]!.allocatedBytes = null; s.sources[0]!.measurementShortfall = "eacces";
      }
    };
  }, (w, result) => {
    assert.equal(result.would_delete, 2, "the shadow pass saw both sources as measured candidates");
    assert.equal(result.kept_after_refusal, 1);
    assert.equal(result.deleted, 1);
    assert.deepEqual(w.spies.proofs.map(p => p.paths), [["/tree/1"]]);
    assert.deepEqual(w.spies.discards.map(x => x.opts.epochProof.input.paths), [["/tree/1"]]);
    assert.ok(w.spies.reserved.every(run => run !== "run0"), "no reservation was taken for the refused source");
    assert.ok(w.spies.ended.every(e => !e.paths.includes("/tree/0")));
  });
});

test("an eligible attested pass with zero admitted candidates reports reclaim_complete without opening a scope", async () => {
  await withDeletion(0, undefined, (w, result) => {
    noMutation(w);
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
    assert.equal(result.reclaim_bytes_state, "measured"); assert.equal(w.spies.begins, 0);
    assert.deepEqual(w.events, ["handover"]);
  });
  // A source that is not a candidate (live) leaves admits empty too.
  await withDeletion(1, w => { w.mutateSource = s => { s.sources[0]!.live = true; }; }, (w, result) => {
    noMutation(w);
    assert.equal(result.would_delete, 0); assert.equal(result.kept, 1);
    assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
    assert.equal(w.spies.begins, 0); assert.deepEqual(w.events, ["handover"]);
  });
});
test("a takeover whose source use was not ended still counts as deleted, is counted and warns with fixed tokens", async () => {
  for (const ended of [{ ended: [], kept: ["/tree/0"], endedSlots: [] }, { ended: [], kept: [], endedSlots: [] }]) {
    const warnings: Array<[string, unknown]> = [];
    await withDeletion(2, w => {
      w.options.log = { warn: (message, fields) => { warnings.push([message, fields]); } };
      const auth = w.options.deletion!.authority as { endSourceUse: unknown };
      const base = auth.endSourceUse as (p: Iterable<string>, o: unknown) => Promise<unknown>;
      auth.endSourceUse = async (paths: Iterable<string>, o: unknown) => {
        const list = [...paths];
        return list[0] === "/tree/0" ? ended : base(list, o);
      };
    }, (_w, result) => {
      assert.equal(result.deleted, 2, "the delete is not dropped"); assert.equal(result.source_use_not_ended, 1);
      assert.equal(result.verdict, "eligible"); assert.equal(result.reason, "reclaim_complete");
      assert.deepEqual(warnings, [["pod epoch reclaim: source use not ended after a takeover", { cause: "source_use_not_ended" }]]);
    });
  }
});
test("a mid-deletion daemon veto stops the loop with the daemon reason and keeps nothing for the unreached sources", async () => {
  const d = await daemon();
  try {
    const w = deletionWorld(3); w.options.dockerHost = d.host;
    const inner = w.options.git.inspectRetainedSources;
    let second = 0;
    w.options.git.inspectRetainedSources = async o => {
      // The deletion-phase re-inspect of run1 (its shadow inspection was the first).
      if (o.selector!.runId === "run1" && ++second === 2) { d.veto(); await tick(); await tick(); }
      return inner(o);
    };
    const result = (await runPodEpochShadow(w.options))!;
    assert.equal(result.deleted, 1, "run0 was deleted before the veto");
    assert.equal(result.verdict, "partial"); assert.equal(result.reason, "daemon_event");
    assert.equal(result.kept_after_refusal, 0, "a veto is not a per-source keep");
    assert.deepEqual(w.spies.reserved, ["run0"], "no later source reaches the reservation");
    assert.equal(w.spies.proofs.length, 1);
  } finally { await d.close(); }
});
test("a daemon error from a guard that has not aborted still ends the pass with its reason, not a per-source keep", async () => {
  const w = deletionWorld(3);
  let armed = false;
  const guard = { signal: new AbortController().signal, close: async () => {},
    assertHealthy() { if (armed) throw new PodEpochDaemonError("daemon_event"); } };
  w.options.openDaemonGuard = async () => guard;
  const inner = w.options.git.inspectRetainedSources;
  let second = 0;
  w.options.git.inspectRetainedSources = async o => {
    // The deletion-phase re-inspect of run1 (its shadow inspection was the first).
    if (o.selector!.runId === "run1" && ++second === 2) armed = true;
    return inner(o);
  };
  const result = (await runPodEpochShadow(w.options))!;
  assert.equal(guard.signal.aborted, false, "the guard never aborted, so no veto listener fired");
  assert.equal(result.reason, "daemon_event");
  assert.equal(result.deleted, 1, "run0 was deleted before the error");
  assert.equal(result.kept_after_refusal, 0, "a daemon error is not a per-source keep");
  assert.deepEqual(w.spies.reserved, ["run0"], "no later source reaches the reservation");
  assert.equal(w.spies.discards.length, 1);
});
test("a path observed in two groups with a different bare path or selector is demoted and never deleted", async () => {
  const variants: Array<[string, { barePath: string; attempt: string; runId?: string; generation?: number }]> = [
    ["bare path", { barePath: "/bare2", attempt: "attempt" }], ["selector", { barePath: "/bare", attempt: "other" }],
    // A source's run id must equal its group's selector run id to be eligible, so the run id is never the ONLY
    // demoter: the `priorAdmit.selector.runId` clause is defence in depth behind that eligibility rule and the
    // `previous.run_id` comparison, and no input isolates it.
    ["run id only", { barePath: "/bare", attempt: "attempt", runId: "run1" }],
    ["generation only", { barePath: "/bare", attempt: "attempt", generation: 2 }],
  ];
  for (const [label, second] of variants) {
    await withDeletion(1, w => {
      w.groups.push({ barePath: second.barePath,
        selector: { runId: second.runId ?? "run0", generation: second.generation ?? 1, attempt: second.attempt }, paths: ["/tree/0"] });
      const inner = w.options.git.inspectRetainedSources;
      w.options.git.inspectRetainedSources = async o => {
        const r = await inner(o);
        for (const src of r.sources) src.barePath = o.barePath!;
        return r;
      };
    }, (w, result) => {
      noMutation(w);
      assert.equal(result.would_delete, 0, label); assert.equal(result.deleted, 0, label);
      assert.equal(result.kept, 1, label); assert.equal(w.spies.journalReads.length, 0, label);
    });
  }
});
