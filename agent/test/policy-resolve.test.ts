import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs/promises";
import path from "node:path";
import { createHmac } from "node:crypto";
import { startSweepWorker, sweepClient, pollUntil } from "./worker-sweep-rig.js";
import { WorkerClient } from "../src/client.js";
import { Outbox, type RawWriteSeam } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { journalAndResolveTerminal, makeTerminalOutboxDeps, resolvePendingTerminal, type TerminalOutboxDeps } from "../src/terminal-resolve.js";
import { validatePolicyHoldRequest, decodePolicyHoldReply, type PolicyHoldRequest, type PolicyHoldReply } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

it("shared Go/TypeScript policy wire cases", async () => {
  const cases = JSON.parse(await fs.readFile(new URL("../../fixtures/policy-hold/requests.json", import.meta.url), "utf8")) as
    { name: string; valid: boolean; body: unknown }[];
  assert.ok(cases.length > 0);
  for (const fixture of cases) {
    if (fixture.valid) assert.deepEqual(validatePolicyHoldRequest(fixture.body), fixture.body, fixture.name);
    else assert.throws(() => validatePolicyHoldRequest(fixture.body), fixture.name);
  }
});

function assertUnprovenPolicySnapshot(registry: ActiveRunRegistry): void {
  const snapshot = registry.build();
  assert.deepEqual(snapshot.active, [], "unknown ownership cannot supply a source-generation lease");
  assert.equal(snapshot.pending_overflow, true, "conservative protection remains before ownership proof");
  assert.equal(registry.claimsPausedByPendingOverflow(), true);
  assert.deepEqual(snapshot.pending_all, [{ run_id: run, claim_generation: 7 }]);
}

const run = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
const original: PolicyHoldRequest = {
  outcome_id: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", source_generation: 7, claim_generation: 7,
  detail: "refused", continuation_mode: "same_conversation", tag: "cyberPolicy", phase: "implementation",
  correlation_id: "pr-" + "a".repeat(32) + "-1", session_id: "session", head: "a".repeat(40),
  published: true, checkpoint_contains_latest: true,
};
const receipt = (disposition: PolicyHoldReply["disposition"] = "applied"): PolicyHoldReply => ({
  disposition, outcome_id: original.outcome_id, source_generation: 7,
  observed_status: "paused", observed_generation: 7, hold_reason: "codex_policy_hold",
});
async function fixture(t: { after(fn: () => Promise<void>): void }, over: { rawWrite?: RawWriteSeam; runMaxBytes?: number; maxBytes?: number; terminalMaxBytes?: number } = {}) {
  const dir = await fs.mkdtemp(path.resolve("../.uzi/scratch/policy-test-"));
  t.after(() => fs.rm(dir, { recursive: true, force: true }));
  const root = path.join(dir, "outbox");
  const make = () => new Outbox({ root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 4e6, retentionMs: 86400000, ...over });
  const outbox = make();
  await outbox.init();
  return { root, outbox, make, file: path.join(root, run, "terminal-7.json") };
}
function deps(outbox: Outbox, report: NonNullable<TerminalOutboxDeps["client"]["reportPolicyHold"]>): TerminalOutboxDeps {
  return { outbox, terminalMaxBytes: 1024, gapFillMax: 1, log: nullLogger(), client: {
    hasFeature: () => false,
    getMessageGaps: async () => { throw new Error("policy must not gap-fill"); },
    postMessages: async () => { throw new Error("policy must not tombstone"); },
    reportPolicyHold: report,
  } };
}
const args = { runId: run, claimGeneration: 7, send: async () => { throw new Error("policy must never use /state"); } };

it("closed request validation matches the server fields and rejects invalid/oversized evidence", () => {
  assert.deepEqual(validatePolicyHoldRequest(original), original);
  for (const bad of [
    { ...original, claim_generation: 6 }, { ...original, source_generation: 0 },
    { ...original, claim_generation: Number.MAX_SAFE_INTEGER + 1 },
    { ...original, outcome_id: "00000000-0000-0000-0000-000000000000" },
    { ...original, outcome_id: original.outcome_id.toUpperCase() },
    { ...original, outcome_id: original.outcome_id + "\n" },
    { ...original, correlation_id: original.correlation_id + "\n" },
    { ...original, head: original.head + "\n" },
    { ...original, head: "a".repeat(64) + "\n" },
    { ...original, tag: "unknown" }, { ...original, phase: "review" },
    { ...original, correlation_id: "private provider text" }, { ...original, correlation_id: "pr-" + "a".repeat(32) + "-9007199254740992" },
    { ...original, session_id: "x".repeat(18000) }, { ...original, session_id: "\uD800" },
    { ...original, head: "" }, { ...original, published: "true" },
    { ...original, checkpoint_contains_latest: 1 }, { ...original, extra: "unknown" },
    { ...original, detail: "refused", phase: undefined },
    Object.create(original),
  ]) assert.throws(() => validatePolicyHoldRequest(bad));
  assert.deepEqual(validatePolicyHoldRequest({ outcome_id: original.outcome_id, source_generation: 7,
    claim_generation: 8, detail: "resume_rejected", continuation_mode: "fresh_from_saved_work", published: false }),
    { outcome_id: original.outcome_id, source_generation: 7, claim_generation: 8, detail: "resume_rejected",
      continuation_mode: "fresh_from_saved_work", published: false });
});

it("production WorkerClient sends exact authenticated wire with strict positive receipts and no fallback", async t => {
  const calls: { url: string; body: string; auth: string | null }[] = [];
  let status = 200;
  let response: unknown = receipt();
  t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(url), body: String(init?.body), auth: new Headers(init?.headers).get("Authorization") });
    return new Response(JSON.stringify(response), { status });
  });
  const client = new WorkerClient("https://example.com", "test-token", "test", nullLogger());
  assert.deepEqual(await client.reportPolicyHold(run, original), receipt());
  assert.deepEqual(JSON.parse(calls[0]!.body), original);
  assert.equal(calls[0]!.auth, "Bearer test-token");
  for (const invalid of [
    { ...receipt(), outcome_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc" },
    { ...receipt(), source_generation: 8 }, { ...receipt(), observed_generation: 0 },
    { ...receipt(), observed_status: "unknown" }, { ...receipt(), disposition: "unknown" },
    { ...receipt("terminal"), observed_status: "running" },
    { ...receipt("resolved_newer_policy"), applying_generation: 7 },
    { ...receipt(), unexpected: true },
  ]) {
    response = invalid;
    await assert.rejects(client.reportPolicyHold(run, original));
  }
  for (const code of [404, 405, 409]) {
    status = code;
    response = code === 409 ? { ...receipt("pending"), reason: "stale_claim" } : { reason: "run_not_owned" };
    if (code === 409) assert.equal((await client.reportPolicyHold(run, original)).disposition, "pending");
    else await assert.rejects(client.reportPolicyHold(run, original));
  }
  status = 200;
  response = receipt("pending");
  await assert.rejects(client.reportPolicyHold(run, original));
  response = { ...receipt(), reason: "x".repeat(18000) };
  await assert.rejects(client.reportPolicyHold(run, original));
  const before = calls.length;
  await assert.rejects(client.reportPolicyHold(run, { ...original, session_id: "x".repeat(18000) }));
  assert.equal(calls.length, before);
  assert.ok(calls.every(c => c.url.endsWith("/policy-hold") && JSON.parse(c.body).claim_generation === 7));
});

it("authenticated restart and cross-family first writer preserve immutable outcome and source on ACK", async t => {
  const { outbox, make, file } = await fixture(t);
  assert.deepEqual(await outbox.journalPolicyHold(run, original), { journaled: true, adopted: false });
  const bytes = await fs.readFile(file, "utf8");
  assert.equal(JSON.parse(bytes).outcome_kind, "policy_hold");
  assert.deepEqual(await outbox.journalTerminal(run, 7, "running", 0, { status: "failed" }), { journaled: true, adopted: true });
  await outbox.journalPolicyHold(run, { ...original, outcome_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc" });
  assert.equal(await fs.readFile(file, "utf8"), bytes);
  const restarted = make();
  await restarted.init();
  assert.equal(await restarted.hasPendingPolicyOutcome(), true);
  let sends = 0;
  await resolvePendingTerminal(deps(restarted, async (_id, body) => {
    sends++; assert.deepEqual(body, original); return receipt();
  }), args);
  assert.equal(sends, 1);
  assert.equal(restarted.listPendingTerminals()[0]!.server_settled, true);
  assert.equal(await restarted.hasPendingPolicyOutcome(run), false);
  assert.equal(await restarted.hasPendingPolicyOutcome(), false);
  assert.equal(await fs.readFile(file, "utf8"), bytes, "ACK must retain source/custody evidence");
  assert.equal(await restarted.retireTerminal(run, 7), false);
  await restarted.staleRetireTerminal(run, 7);
  assert.equal(await fs.readFile(file, "utf8"), bytes);
  const again = make(); await again.init();
  assert.equal(await again.hasPendingPolicyOutcome(run), true, "settlement is process-local, restart must replay");
});

it("ordinary terminal first writer wins over a later policy attempt", async t => {
  const { outbox } = await fixture(t);
  await outbox.journalTerminal(run, 7, "running", 0, { status: "failed" });
  assert.deepEqual(await outbox.journalPolicyHold(run, original), { journaled: true, adopted: true });
  assert.deepEqual((await outbox.readTerminalJournal(run, 7))?.body, { status: "failed" });
});

it("lost ACK and current-claim conversion preserve original journal and omit refusal metadata", async t => {
  const { outbox, file } = await fixture(t);
  await outbox.journalPolicyHold(run, original);
  const bytes = await fs.readFile(file, "utf8");
  let count = 0;
  const d = deps(outbox, async (_id, body) => {
    count++;
    if (count === 1) { assert.deepEqual(body, original); throw new Error("lost ACK"); }
    assert.deepEqual(body, { outcome_id: original.outcome_id, source_generation: 7, claim_generation: 9,
      detail: "resume_rejected", continuation_mode: "fresh_from_saved_work", published: false });
    return { ...receipt("already_applied"), observed_status: "queued", observed_generation: 10 };
  });
  await resolvePendingTerminal(d, args); // boot has no independent authority
  assert.equal(outbox.listPendingTerminals()[0]!.server_settled, undefined);
  await resolvePendingTerminal(d, { ...args, currentPolicyAuthority: { claimGeneration: 9, continuationMode: "fresh_from_saved_work" } });
  assert.equal(outbox.listPendingTerminals()[0]!.server_settled, true);
  assert.equal(await fs.readFile(file, "utf8"), bytes);
});

for (const kind of ["pending", "404", "stale409", "malformed", "newer", "terminal", "already_applied"] as const) {
  it("policy reconciliation " + kind + " only positive evidence settles delivery", async t => {
    const { outbox, file } = await fixture(t);
    await outbox.journalPolicyHold(run, original);
    const bytes = await fs.readFile(file, "utf8");
    const positive = ["newer", "terminal", "already_applied"].includes(kind);
    const d = deps(outbox, async () => {
      if (kind === "404" || kind === "stale409") throw new Error(kind);
      if (kind === "malformed") return { ...receipt(), source_generation: 8 };
      if (kind === "newer") return { ...receipt("resolved_newer_policy"), applying_generation: 8, observed_generation: 9, observed_status: "running" };
      if (kind === "terminal") return { ...receipt("terminal"), observed_status: "cancelled" };
      return receipt(kind === "already_applied" ? kind : "pending");
    });
    await resolvePendingTerminal(d, args);
    assert.equal(outbox.listPendingTerminals()[0]!.server_settled === true, positive);
    assert.equal(outbox.listPendingTerminals()[0]!.blocked_reason, undefined);
    assert.equal(await fs.readFile(file, "utf8"), bytes);
  });
}

it("shared resolver singleflight serializes boot/drain and sweep skip does not compete", async t => {
  const { outbox } = await fixture(t);
  await outbox.journalPolicyHold(run, original);
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  let entered!: () => void;
  const started = new Promise<void>(r => { entered = r; });
  let active = 0; let max = 0; let calls = 0;
  const d = deps(outbox, async () => {
    calls++; active++; max = Math.max(max, active); entered();
    await held; active--; return receipt("pending");
  });
  const boot = resolvePendingTerminal(d, args);
  await started;
  const drain = resolvePendingTerminal(d, args);
  await resolvePendingTerminal(d, { ...args, ifBusy: "skip" });
  assert.equal(calls, 1);
  release(); await Promise.all([boot, drain]);
  assert.equal(calls, 2); assert.equal(max, 1);
});

it("ENOSPC keeps snapshot-visible memory first writer and retries journaling before HTTP", async t => {
  let full = true;
  const { outbox, file } = await fixture(t, { rawWrite: async (write, ctx) => {
    if (full && ctx.kind === "terminal") throw Object.assign(new Error("disk full"), { code: "ENOSPC" });
    return write();
  } });
  assert.equal((await outbox.journalPolicyHold(run, original)).journaled, false);
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 4);
  assertUnprovenPolicySnapshot(registry);
  registry.notePolicyOwnership(run, 7);
  assert.equal(registry.build().active[0]?.terminal_pending, true, "independent current proof supplies the lease");
  let sent = 0;
  const d = deps(outbox, async (_id, body) => { sent++; assert.deepEqual(body, original); return receipt(); });
  await resolvePendingTerminal(d, args);
  assert.equal(sent, 0);
  await outbox.journalPolicyHold(run, { ...original, outcome_id: "cccccccc-cccc-4ccc-8ccc-cccccccccccc" });
  full = false;
  await resolvePendingTerminal(d, args);
  assert.equal(sent, 1);
  assert.deepEqual(JSON.parse(await fs.readFile(file, "utf8")).body, original);
});

for (const bad of ["corrupt", "oversized", "MAC", "unknown-kind", "bad-body", "run-cap", "total-cap"] as const) {
  it("restart retains " + bad + " physical candidates as a conservative execution blocker", async t => {
    const f = await fixture(t);
    await f.outbox.journalPolicyHold(run, original);
    if (bad === "corrupt") await fs.writeFile(f.file, "{");
    if (bad === "oversized") await fs.truncate(f.file, 2e6);
    if (bad === "MAC") {
      const data = JSON.parse(await fs.readFile(f.file, "utf8")); data.body.claim_generation = 8;
      await fs.writeFile(f.file, JSON.stringify(data));
    }
    // Re-sign with the fixture key so these reach closed shape validation after authentication.
    if (bad === "unknown-kind" || bad === "bad-body") {
      const data = JSON.parse(await fs.readFile(f.file, "utf8"));
      if (bad === "unknown-kind") data.outcome_kind = "other";
      else data.body.tag = "other";
      delete data.mac;
      const canonical = (v: unknown): string => v !== null && typeof v === "object" ?
        "{" + Object.entries(v).sort(([a], [b]) => a < b ? -1 : a > b ? 1 : 0).map(([k, value]) => JSON.stringify(k) + ":" + canonical(value)).join(",") + "}" : JSON.stringify(v);
      data.mac = createHmac("sha256", await fs.readFile(path.join(f.root, ".key"))).update("uzi.outbox.terminal.v1").update(canonical(data)).digest("hex");
      await fs.writeFile(f.file, JSON.stringify(data));
    }
    const o = bad === "run-cap" || bad === "total-cap" ? new Outbox({ root: f.root, log: nullLogger(),
      runMaxBytes: bad === "run-cap" ? 10 : 1e6, maxBytes: bad === "total-cap" ? 10 : 4e6, retentionMs: 86400000 }) : f.make();
    await o.init();
    if (bad === "unknown-kind" || bad === "bad-body") assert.equal((await o.observeTerminalAuthentication(run, 7)).kind, "malformed");
    assert.equal(await o.hasPendingPolicyOutcome(run), true);
    assert.equal(o.listPendingTerminals()[0]!.outcome_kind, "unknown");
    const registry = new ActiveRunRegistry(() => o.listPendingTerminals(), () => 4);
    assertUnprovenPolicySnapshot(registry);
    registry.notePolicyOwnership(run, 7);
    assertUnprovenPolicySnapshot(registry); // malformed source remains unknown even with a current owner
    let sends = 0;
    await resolvePendingTerminal(deps(o, async () => { sends++; return receipt(); }), args);
    assert.equal(sends, 0);
  });
}

it("real Worker awaits boot policy replay before claims, and its existing drain/sweep re-drive it", async t => {
  const { outbox, file } = await fixture(t);
  await outbox.journalPolicyHold(run, original);
  let release!: () => void;
  const held = new Promise<void>(r => { release = r; });
  let calls = 0;
  let claims = 0;
  let messageCalls = 0;
  let settled = false;
  const client = sweepClient({
    reportState: async () => { throw new Error("policy entered ordinary /state"); },
    postMessages: async () => { messageCalls++; },
  });
  client.claimRun = async () => { claims++; return null; };
  client.reportPolicyHold = async (_id, body) => {
    calls++;
    assert.deepEqual(body, original, "boot/drain never fabricate current authority");
    if (calls === 1) await held;
    return receipt(settled ? "already_applied" : "pending");
  };
  const worker = startSweepWorker({ outbox, client });
  try {
    await pollUntil(() => calls === 1, 2000, "boot policy send");
    assert.equal(claims, 0, "boot barrier is awaited before ordinary claims");
    release();
    await pollUntil(() => calls >= 2, 2000, "existing heartbeat sweep retries policy");
    // Drive the existing real message drain too; policy resolution shares its owner.
    await outbox.appendSegment(run, 7, [{ seq: 1, kind: "status", agent: "worker", payload: { text: "status" } }]);
    settled = true;
    await pollUntil(() => messageCalls > 0 && outbox.listPendingTerminals()[0]?.server_settled === true, 2000,
      "drain and sweep settle delivery");
    assert.ok(await fs.stat(file), "source remains after delivery");
  } finally { release(); await worker.stop(); }
});

it("retirement guard refusal preserves settled delivery and never retries the same lifetime", async t => {
  const { outbox, file } = await fixture(t);
  await outbox.journalPolicyHold(run, original);
  const before = await fs.readFile(file, "utf8");
  const retire = t.mock.method(outbox, "retireTerminal", async () => false);
  let calls = 0;
  const d = deps(outbox, async () => { calls++; return receipt(); });
  await resolvePendingTerminal(d, args);
  await resolvePendingTerminal(d, args);
  assert.equal(retire.mock.callCount(), 1);
  assert.equal(calls, 1);
  assert.equal(outbox.listPendingTerminals()[0]?.server_settled, true);
  assert.equal(await fs.readFile(file, "utf8"), before);
});

it("receipt decoder rejects terminal-looking uncertainty and obsolete newer episode evidence", () => {
  assert.throws(() => decodePolicyHoldReply({ ...receipt("resolved_newer_policy"), applying_generation: 7 }, original));
  assert.throws(() => decodePolicyHoldReply({ ...receipt("resolved_newer_policy"), applying_generation: 8, observed_generation: 7 }, original));
  assert.equal(decodePolicyHoldReply({ ...receipt("pending"), observed_status: "cancelled" }, original).disposition, "pending");
});

it("already_applied receipts accept the full current status domain; terminal remains restricted", () => {
  for (const status of ["queued", "claimed", "running", "awaiting_approval", "awaiting_input", "limit_wait",
    "completed", "failed", "cancelled", "awaiting_followup", "pool_wait", "paused", "recovery_wait"]) {
    assert.equal(decodePolicyHoldReply({ ...receipt("already_applied"), observed_status: status }, original).observed_status, status);
    if (!["completed", "failed", "cancelled"].includes(status)) {
      assert.throws(() => decodePolicyHoldReply({ ...receipt("terminal"), observed_status: status }, original));
    }
  }
  assert.throws(() => decodePolicyHoldReply({ ...receipt("already_applied"), observed_status: "other" }, original));
  assert.equal(validatePolicyHoldRequest({ ...original, session_id: "会話" }).session_id, "会話");
});

it("ordinary root/run entries never invent a policy blocker; late corrupt aliases do", async t => {
  const { outbox, root } = await fixture(t);
  await fs.mkdir(path.join(root, run));
  for (let i = 0; i < 270; i++) {
    await fs.writeFile(path.join(root, "ordinary-" + i), "");
    await fs.writeFile(path.join(root, run, "ordinary-" + i), "");
  }
  const unrelated = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";
  await fs.mkdir(path.join(root, unrelated));
  await fs.writeFile(path.join(root, unrelated, "terminal-1.json"), "{");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), false);
  assert.equal(await outbox.hasPendingPolicyOutcome(), true);
  await fs.mkdir(path.join(root, run.toUpperCase()));
  await fs.writeFile(path.join(root, run.toUpperCase(), "terminal-9.json"), "{");
  assert.equal(await outbox.hasPendingPolicyOutcome(run), true);
});

it("disabled policy deps retain snapshot membership without an unjournaled send", async t => {
  const { root } = await fixture(t);
  await fs.writeFile(path.join(root, ".key"), "invalid");
  const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1e6, maxBytes: 4e6, retentionMs: 86400000 });
  await outbox.init();
  assert.equal(outbox.isDisabled(), true);
  let sends = 0;
  const d = deps(outbox, async () => { sends++; return receipt(); });
  const opts = { gapFillMax: 1, terminalMaxBytes: 1024, log: nullLogger() };
  assert.equal(makeTerminalOutboxDeps(outbox, d.client, opts), undefined);
  const retained = makeTerminalOutboxDeps(outbox, d.client, { ...opts, retainDisabledPolicy: true })!;
  assert.equal((await outbox.journalPolicyHold(run, original)).journaled, false);
  assert.equal(outbox.listPendingTerminals()[0]?.outcome_kind, "policy_hold");
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 4);
  assertUnprovenPolicySnapshot(registry);
  await resolvePendingTerminal(retained, args);
  assert.equal(sends, 0);
});

it("policy admission beyond 256 generations uses byte budgets, not lifetime counts", async t => {
  const { outbox } = await fixture(t, { rawWrite: async (_write, ctx) => {
    if (ctx.kind === "terminal") throw Object.assign(new Error("disk full"), { code: "ENOSPC" });
    return _write();
  } });
  for (let generation = 1; generation <= 270; generation++) {
    await outbox.journalPolicyHold(run, { ...original, source_generation: generation, claim_generation: generation });
  }
  assert.equal(outbox.listPendingTerminals().length, 270);
  assert.ok(outbox.listPendingTerminals().every(p => p.outcome_kind === "policy_hold"));
});

it("physical decoding cannot bypass aggregate run/total budgets on journals added after init", async t => {
  const f = await fixture(t);
  for (let generation = 1; generation <= 3; generation++) {
    await f.outbox.journalTerminal(run, generation, "running", 0, { status: "completed" });
  }
  const one = (await fs.stat(path.join(f.root, run, "terminal-1.json"))).size;
  for (const scope of ["run", "total"]) {
    const root = path.join(f.root, "scan-" + scope);
    const o = new Outbox({ root, log: nullLogger(), runMaxBytes: scope === "run" ? one * 2 : 1e6,
      maxBytes: scope === "total" ? one * 2 : 4e6, retentionMs: 86400000 });
    await fs.mkdir(root);
    await fs.copyFile(path.join(f.root, ".key"), path.join(root, ".key"));
    await o.init();
    await fs.mkdir(path.join(root, run));
    for (let generation = 1; generation <= 3; generation++) {
      await fs.copyFile(path.join(f.root, run, "terminal-" + generation + ".json"), path.join(root, run, "terminal-" + generation + ".json"));
    }
    assert.equal(await o.hasPendingPolicyOutcome(run), true, scope + " aggregate budget must retain uncertainty");
  }
});

for (const kind of ["policy_hold", "unknown"] as const) {
  it("disabled store cannot send an ordinary terminal over a retained " + kind + " winner", async t => {
    const f = await fixture(t);
    await fs.writeFile(path.join(f.root, ".key"), "invalid");
    const outbox = new Outbox({ root: f.root, log: nullLogger(),
      runMaxBytes: kind === "unknown" ? 1 : 1e6, maxBytes: kind === "unknown" ? 1 : 4e6, retentionMs: 86400000 });
    await outbox.init();
    assert.equal(outbox.isDisabled(), true);
    assert.equal((await outbox.journalPolicyHold(run, original)).journaled, false);
    const before = outbox.listPendingTerminals();
    assert.equal(before[0]?.outcome_kind, kind);
    let sends = 0;
    const ordinaryDeps = makeTerminalOutboxDeps(outbox,
      deps(outbox, async () => { throw new Error("no durable policy send"); }).client,
      { terminalMaxBytes: 1024, gapFillMax: 1, log: nullLogger() });
    assert.ok(ordinaryDeps, "legacy callers must still see retained policy/unknown arbitration");
    await journalAndResolveTerminal(ordinaryDeps, {
      ...args, phase: "implementation", messagesThroughSeq: 0, body: { status: "failed" },
      send: async () => { sends++; return { applied: true, status: "failed" }; },
    });
    assert.equal(sends, 0, "an unavailable journal cannot authorize a competing terminal");
    assert.deepEqual(outbox.listPendingTerminals(), before);
    const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 4);
    assertUnprovenPolicySnapshot(registry);
  });
}
