import assert from "node:assert/strict";
import { test } from "node:test";
import http from "node:http";
import { mkdtemp, mkdir, open, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { resolve, join } from "node:path";
import { runPodEpochShadow, readPodEpochProjection, PodEpochSettlementError } from "../src/pod-epoch-reclaim.js";
import { WorkerClient } from "../src/client.js";
import { nullLogger } from "./helpers.js";
import type { PodEpochOwnership } from "../src/pod-epoch-lock.js";
import type { RetainedSourceEvidence } from "../src/git.js";

const bytes = await readFile(new URL("../../fixtures/pod-epoch/valid/positive.json", import.meta.url));
const evidence = JSON.parse(bytes.toString());
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
async function daemon() {
  let events: http.ServerResponse | undefined;
  const requests: string[] = [];
  const server = http.createServer((req, res) => {
    requests.push(req.method + " " + req.url);
    if (req.url?.startsWith("/events")) { events = res; res.writeHead(200); res.flushHeaders(); return; }
    const body = req.url === "/_ping" ? "OK" : JSON.stringify(
      req.url === "/info" ? { Swarm: { LocalNodeState: "inactive" }, LiveRestoreEnabled: false } :
      req.url === "/volumes" ? { Volumes: [] } : []);
    res.end(body);
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  assert.ok(address && typeof address !== "string");
  return {
    host: "tcp://127.0.0.1:" + address.port,
    requests,
    veto: () => events!.write('{"Type":"container"}\n'),
    close: async () => {
      events?.destroy();
      server.closeAllConnections();
      await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
    },
  };
}
function source(path: string, runId: string, protectedSource = false): RetainedSourceEvidence {
  return {
    barePath: "/bare", runId, branch: "branch", key: "key", kind: "attempt", generation: 1,
    attempt: "attempt", ledgerState: "abandoned", journalState: "none", markerState: "none",
    presence: "present", descriptor: "fresh", allocatedBytes: 4096, bytesState: "measured",
    sources: [{ clonePath: path, attempt: "attempt", generation: 1, presence: "present",
      ledgerState: "abandoned", live: false, allocatedBytes: 4096 }],
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
    assert.equal((await runPodEpochShadow(options))!.reason, reason);
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
          const isHolds = path.endsWith("/recovery-holds");
          if (endpoint === "ownership" && isHolds)
            return new Response(JSON.stringify({ run_id: "run0", holds: [] }));
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
        assert.equal(requests.length, endpoint === "holds" ? 1 : 2);
      } finally {
        release();
        await running.catch(() => undefined);
        await tick();
        await d.close();
      }
    });
  }
}
