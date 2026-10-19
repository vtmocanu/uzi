import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { WorkerClient } from "../src/client.js";
import { decodeInterruptedCalls, interruptedEligible, interruptedRegistration, interruptedSnapshot, prepareInterruptedCalls } from "../src/interrupted-calls.js";
import { makeTextRedactor } from "../src/redact.js";
import { nullLogger } from "./helpers.js";

const worker = "11111111-1111-4111-8111-111111111111";
const run = "22222222-2222-4222-8222-222222222222";
const identity = interruptedRegistration(worker, "registration-nonce", 1)!;
function dto(): Record<string, unknown> {
  return { format: 1, run_id: run, worker_id: worker, register_nonce: identity.nonce,
    claim_generation: 3, source_generation: 2, inspected_count: 1, history_incomplete: true,
    unavailable: false, truncated: false, entries: [{ seq: 10, source_generation: 2,
      agent: "lead", agent_instance: null, agent_label: null, tool_name: "Bash",
      command: "git status", display_omitted: false }] };
}
const decoded = () => decodeInterruptedCalls(dto(), run, 3, identity)!;
function deferred<T>() {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>(r => { resolve = r; });
  return { promise, resolve };
}

describe("M2 strict interrupted evidence", () => {
  it("requires every key, exact bindings, safe integers and flags", () => {
    assert.ok(decoded());
    for (const key of Object.keys(dto())) {
      const raw = dto(); delete raw[key];
      assert.equal(decodeInterruptedCalls(raw, run, 3, identity), undefined, key);
    }
    for (const patch of [
      { extra: true }, { format: 2 }, { run_id: worker }, { worker_id: run },
      { register_nonce: "other" }, { claim_generation: 4 }, { claim_generation: 2 ** 53 },
      { source_generation: 3 }, { source_generation: 0 }, { source_generation: null },
      { inspected_count: -1 }, { inspected_count: 257 }, { inspected_count: 0 },
      { inspected_count: 1.5 }, { history_incomplete: false }, { unavailable: 0 },
      { truncated: "false" }, { entries: {} }, { entries: Array(4).fill((dto().entries as unknown[])[0]) },
    ]) assert.equal(decodeInterruptedCalls({ ...dto(), ...patch }, run, 3, identity), undefined, JSON.stringify(patch));
  });
  it("rejects malicious entry shapes, invalid Unicode, flags, sequence and byte counts", () => {
    const entry = (dto().entries as Record<string, unknown>[])[0]!;
    for (const key of Object.keys(entry)) {
      const changed = { ...entry }; delete changed[key];
      assert.equal(decodeInterruptedCalls({ ...dto(), entries: [changed] }, run, 3, identity), undefined, key);
    }
    for (const patch of [
      { call_id: "raw-id" }, { seq: 0 }, { seq: 2147483648 }, { source_generation: 1 },
      { agent: "a".repeat(129) }, { agent: "\ud800" }, { agent_instance: 0 },
      { agent_label: "😀".repeat(129) }, { tool_name: null }, { command: "é".repeat(1025) },
      { command: "\udfff" }, { command: "" }, { display_omitted: null },
    ]) assert.equal(decodeInterruptedCalls({ ...dto(), entries: [{ ...entry, ...patch }] }, run, 3, identity), undefined);
    assert.ok(decodeInterruptedCalls({ ...dto(), unavailable: true, entries: [{ ...entry, command: "" }] }, run, 3, identity));
    assert.ok(decodeInterruptedCalls({ ...dto(), source_generation: null, inspected_count: 0, entries: [] }, run, 3, identity));
  });
  it("accepts a text-only inspected tail without a source and rejects a source without inspected rows", () => {
    for (const inspected_count of [0, 1, 255, 256]) {
      const raw = { ...dto(), source_generation: null, inspected_count, entries: [] };
      assert.equal(decodeInterruptedCalls(raw, run, 3, identity)?.inspected_count, inspected_count);
    }
    for (const inspected_count of [-1, 257, 0.5]) {
      assert.equal(decodeInterruptedCalls({ ...dto(), source_generation: null, inspected_count, entries: [] }, run, 3, identity), undefined);
    }
    assert.equal(decodeInterruptedCalls({ ...dto(), inspected_count: 0, entries: [] }, run, 3, identity), undefined);
    assert.ok(decodeInterruptedCalls({ ...dto(), inspected_count: 1, entries: [] }, run, 3, identity));
  });
  it("normalizes full source then redacts, sanitizes, re-redacts and caps including markers", async () => {
    const raw = dto();
    const entries = raw.entries as Record<string, unknown>[];
    entries[0]!.command = "ｓｅｃｒｅｔＡＢＣ" + "\u2028" + "secret\u0000ABC";
    const calls = decodeInterruptedCalls(raw, run, 3, identity)!;
    let secret = "secretABC";
    const get = await prepareInterruptedCalls({ kind: "issue", generation: 3, check: () => true,
      fetch: async () => calls, redact: s => makeTextRedactor([secret])(s) });
    assert.equal(get()!.entries[0]!.command, "***REDACTED******REDACTED***");
    secret = "git status";
    const again = await prepareInterruptedCalls({ kind: "issue", generation: 3, check: () => true,
      fetch: async () => decoded(), redact: s => makeTextRedactor([secret])(s) });
    assert.equal(again()!.entries[0]!.command, "***REDACTED***");
    secret = "different-secret";
    assert.equal(again()!.entries[0]!.command, "git status", "redaction is current at delivery");
    entries[0]!.command = "x".repeat(2040) + "12345678";
    const capped = await prepareInterruptedCalls({ kind: "task", generation: 3, check: () => true,
      fetch: async () => decodeInterruptedCalls(raw, run, 3, identity), redact: makeTextRedactor(["12345678"]) });
    const command = capped()!.entries[0]!.command;
    assert.ok(Buffer.byteLength(command) <= 2048);
    assert.ok(command.endsWith("[omitted]"));
    assert.ok(!command.includes("12345678"));
    assert.ok(Object.isFrozen(capped()!.entries[0]));
  });
});

describe("M2 setup ownership", () => {
  it("fetches exactly once for all six kinds independently of harness/session and excludes G1/other kinds", async () => {
    for (const kind of ["issue", "ci_fix", "self_improve", "prompt", "task", "mr_rework", "job", "chat", "judge", "cross_check", "review"]) {
      for (const generation of [1, 2]) {
        let reads = 0;
        const get = await prepareInterruptedCalls({ kind, generation, check: () => true,
          fetch: async () => { ++reads; return decoded(); }, redact: s => s });
        get(); get();
        assert.equal(reads, interruptedEligible(kind, generation) ? 1 : 0);
      }
    }
  });
  it("discards a late response and invalidates the getter when its authority check fails", async () => {
    let live = true;
    const pending = deferred<ReturnType<typeof decoded>>();
    const setup = prepareInterruptedCalls({ kind: "issue", generation: 3, check: () => live,
      fetch: () => pending.promise, redact: s => s });
    live = false; pending.resolve(decoded());
    assert.equal((await setup)(), undefined);
    live = true;
    const get = await prepareInterruptedCalls({ kind: "issue", generation: 3, check: () => live,
      fetch: async () => decoded(), redact: s => s });
    assert.ok(get()); await Promise.resolve(); live = false;
    assert.equal(get(), undefined);
  });
  it("optional IO failures never swallow lifecycle failures", async () => {
    let ended = false;
    const failure = new Error("lifecycle stopped");
    await assert.rejects(prepareInterruptedCalls({ kind: "issue", generation: 3,
      check: () => { if (ended) throw failure; return true; },
      fetch: async () => { ended = true; throw new Error("optional IO"); }, redact: s => s }), e => e === failure);
  });
  it("snapshot provenance includes zero/unavailable counts, unknown origin and mixed WIP", () => {
    for (const seededFrom of ["tracking", "checkpoint", "origin", "default"] as const) {
      for (const priorCommits of [0, 4, NaN]) for (const wipRecovered of [false, true]) {
        assert.equal(interruptedSnapshot(1, { seededFrom, priorCommits, wipRecovered }), undefined);
        const snapshot = interruptedSnapshot(2, { seededFrom, priorCommits, wipRecovered });
        if (seededFrom === "default" && !wipRecovered) { assert.equal(snapshot, undefined); continue; }
        assert.deepEqual(snapshot, {
          base: seededFrom === "origin" ? "generic" : seededFrom === "default" ? "default" : "interrupted",
          interruptedEdits: wipRecovered,
          commits: seededFrom === "origin" || Number.isNaN(priorCommits) ? null : priorCommits,
        });
        assert.ok(Object.isFrozen(snapshot));
      }
    }
  });
});

describe("M2 transport and registration", () => {
  function transportClient(fetcher: typeof fetch) {
    return new WorkerClient("http://example.invalid", "join-fixture", "test", nullLogger(), { fetch: fetcher });
  }
  it("sends the captured nonce only on the dedicated route; registration start fences even failed same-ID attempts", async () => {
    let fail = false;
    const pending = deferred<Response>();
    let request: RequestInit | undefined;
    const client = transportClient(async (url, init) => {
      if (String(url).endsWith("/register")) {
        if (fail) throw new Error("register failed");
        return Response.json({ worker_id: worker, register_nonce: identity.nonce });
      }
      assert.equal(String(url), `http://example.invalid/api/worker/runs/${run}/interrupted-calls?claim_generation=3`);
      request = init; return pending.promise;
    });
    await client.register("worker");
    const captured = client.interruptedCallsRegistration!;
    const read = client.getInterruptedCalls(run, 3, captured, new AbortController().signal);
    fail = true;
    await assert.rejects(client.register("worker"));
    assert.equal(client.interruptedCallsRegistration, undefined);
    pending.resolve(Response.json(dto()));
    assert.equal(await read, undefined);
    assert.equal(new Headers(request!.headers).get("X-Worker-Register-Nonce"), identity.nonce);
    fail = false;
    await client.register("worker");
    assert.notEqual(client.interruptedCallsRegistration, captured);
    client.protocolFeatures = [];
    assert.ok(client.interruptedCallsRegistration, "memory capability absent does not revoke registration evidence");
    client.revokeMemoryAuthority();
    assert.equal(client.interruptedCallsRegistration, undefined);
  });
  it("memory feature loss revokes memory authority while retaining interrupted registration", async () => {
    const feature = "worker_memory_pressure_v1";
    for (const loss of ["drop", "replace", "heartbeat", "clear"] as const) {
      let features = [feature];
      const client = transportClient(async url => String(url).endsWith("/register")
        ? Response.json({ worker_id: worker, register_nonce: identity.nonce, protocol_features: features })
        : Response.json({ protocol_features: features }));
      await client.register("worker");
      const captured = client.interruptedCallsRegistration;
      assert.ok(captured);
      assert.ok(client.memoryIncarnation);
      let invalidations = 0;
      const unsubscribe = client.subscribeMemoryInvalidation(() => { ++invalidations; });
      try {
        if (loss === "drop") client.dropFeature(feature);
        if (loss === "replace") client.protocolFeatures = [];
        if (loss === "heartbeat") { features = []; await client.heartbeat(); }
        if (loss === "clear") client.clearFeatures();
        assert.equal(client.memoryIncarnation, undefined);
        assert.equal(invalidations, 1);
        assert.equal(client.interruptedCallsRegistration, loss === "clear" ? undefined : captured);
        client.protocolFeatures = [feature];
        assert.equal(client.memoryIncarnation, undefined, "feature growth cannot restore memory authority");
      } finally { unsubscribe(); }
    }
  });
  it("bounds combined headers and body to one deadline and cancels the stream", async () => {
    let cancelled = false;
    // Production default is 2000 ms; a scaled injected bound keeps this test off real 2 s waits.
    const client = new WorkerClient("http://example.invalid", "join-fixture", "test", nullLogger(), {
      interruptedCallsTimeoutMs: 300,
      fetch: async (url) => {
        if (String(url).endsWith("/register")) return Response.json({ worker_id: worker, register_nonce: identity.nonce });
        await new Promise(r => setTimeout(r, 150));
        return new Response(new ReadableStream({ start(c) { c.enqueue(new TextEncoder().encode("{")); }, cancel() { cancelled = true; } }));
      },
    });
    await client.register("worker");
    const start = performance.now();
    await assert.rejects(client.getInterruptedCalls(run, 3, client.interruptedCallsRegistration!, new AbortController().signal));
    assert.ok(performance.now() - start < 420, "body did not get a renewed deadline");
    assert.ok(cancelled);
  });
  it("rejects actual byte overflow, malformed JSON and invalid UTF8; errors and 404 are optional", async () => {
    for (const response of [
      new Response("x".repeat(16385), { headers: { "Content-Length": "1" } }),
      new Response("{"), new Response(new Uint8Array([0xff])),
      new Response("private failure content", { status: 404 }), new Response("", { status: 500 }),
      Response.json({ ...dto(), extra: true }),
    ]) {
      const client = transportClient(async url => String(url).endsWith("/register")
        ? Response.json({ worker_id: worker, register_nonce: identity.nonce }) : response);
      await client.register("worker");
      const get = await prepareInterruptedCalls({ kind: "issue", generation: 3, check: () => true,
        fetch: () => client.getInterruptedCalls(run, 3, client.interruptedCallsRegistration!, new AbortController().signal), redact: s => s });
      assert.equal(get(), undefined);
    }
  });
});
