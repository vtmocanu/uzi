import { it } from "node:test";
import assert from "node:assert/strict";
import { setImmediate as barrier } from "node:timers/promises";
import { CodexAdmissionReporter, type AdmissionSignal } from "../src/codex/admission-signal.js";
import type { CodexNotification } from "../src/codex/transport.js";
import { WorkerClient } from "../src/client.js";
import { makeClaim, nullLogger } from "./helpers.js";

const feature = "codex_account_admission_signal_v1";
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
function note(info: unknown, threadId = "thread", turnId = "turn", final = false): CodexNotification {
  const error = { codexErrorInfo: info, message: "rate limit overloaded 429 raw secret" };
  return final
    ? { kind: "turn_completed", method: "turn/completed", threadId, turnId, status: "failed",
      params: { threadId, turn: { id: turnId, status: "failed", error } } }
    : { kind: "codex_error", method: "error", threadId, turnId, willRetry: true,
      params: { threadId, turnId, willRetry: true, error } };
}
function emit(reporter: CodexAdmissionReporter, info: unknown = "rateLimitExceeded") {
  const attempt = reporter.begin("thread");
  attempt.bind("turn");
  attempt.observe(note(info));
  return attempt;
}
class Clock {
  value = 0;
  timers = new Map<symbol, { at: number; fn: () => void }>();
  now = () => this.value;
  timer = (fn: () => void, ms: number) => {
    const id = Symbol();
    this.timers.set(id, { at: this.value + ms, fn });
    return () => { this.timers.delete(id); };
  };
  advance(ms: number) {
    this.value += ms;
    for (const [id, timer] of this.timers) {
      if (timer.at <= this.value) { this.timers.delete(id); timer.fn(); }
    }
  }
}

it("allocates identity before dispatch, validates binding, and independently deduplicates retry/final classes", async () => {
  const sent: AdmissionSignal[] = [];
  const reporter = new CodexAdmissionReporter(7, () => true, async b => { sent.push(b); return 204; });
  const attempt = reporter.begin("thread");
  assert.match(attempt.id, uuid); // already allocated before provider turn/start
  attempt.observe(note("rateLimitExceeded")); // not yet bound
  attempt.bind("turn");
  attempt.observe(note("rateLimitExceeded", "foreign"));
  attempt.observe(note("serverOverloaded", "thread", "stale"));
  await barrier();
  assert.equal(sent.length, 0, "foreign or stale frames never queue evidence");
  attempt.observe(note("rateLimitExceeded"));
  attempt.observe(note("rateLimitExceeded"));
  attempt.observe(note("rateLimitExceeded", "thread", "turn", true));
  attempt.observe(note("serverOverloaded", "thread", "turn", true));
  assert.equal(sent.length, 0); // handler only queues
  await barrier();
  assert.deepEqual(sent.map(b => b.class), ["rate_limit", "overload"]);
  assert.ok(sent.every(b => b.attempt_id === attempt.id && b.claim_generation === 7));
  assert.equal(new Set([attempt.id, reporter.begin("thread").id]).size, 2);
  assert.deepEqual(Object.keys(sent[0]!).sort(),
    ["attempt_id", "claim_generation", "class", "provider_thread_id", "provider_turn_id"]);
  assert.ok(Buffer.byteLength(JSON.stringify(sent[0])) < 2048);
});

it("uses only closed model error evidence, including all accepted structured HTTP 429 variants", async () => {
  const sent: AdmissionSignal[] = [];
  const reporter = new CodexAdmissionReporter(1, () => true, async b => { sent.push(b); return 204; });
  for (const info of [null, "unknown", "usageLimitExceeded", "sessionBudgetExceeded", "unauthorized",
    "internalServerError", "flexUnavailable", "rate_limit", "rateLimitExceeded text",
    { rateLimitExceeded: {} }, { arbitrary: { httpStatusCode: 429 } },
    { httpConnectionFailed: { httpStatusCode: 503 } },
    { httpConnectionFailed: { httpStatusCode: "429" } }]) emit(reporter, info);
  const unstructured = reporter.begin("thread");
  unstructured.bind("turn");
  unstructured.observe({ kind: "activity", method: "error", params: { message: "429 rate limit" } });
  for (const tag of ["httpConnectionFailed", "responseTooManyFailedAttempts",
    "responseStreamConnectionFailed", "responseStreamDisconnected"]) {
    emit(reporter, { [tag]: { httpStatusCode: 429 } });
  }
  await barrier();
  assert.deepEqual(sent.map(b => b.class), Array(4).fill("rate_limit"));
});

it("rejects empty, oversized, malformed UTF8, control and format coordinates", async () => {
  const sent: AdmissionSignal[] = [];
  const reporter = new CodexAdmissionReporter(1, () => true, async b => { sent.push(b); return 204; });
  for (const id of ["", "x".repeat(129), "é".repeat(65), "\n", "\u007f", "\u200d", "\ud800"]) {
    const a = reporter.begin(id); a.bind("turn"); a.observe(note("rateLimitExceeded", id));
    const b = reporter.begin("thread"); b.bind(id); b.observe(note("rateLimitExceeded", "thread", id));
  }
  const good = reporter.begin("é".repeat(64)); good.bind("turn");
  good.observe(note("serverOverloaded", "é".repeat(64)));
  await barrier();
  assert.equal(sent.length, 1);
});

it("retries transient HTTP or lost ACK at most twice with identical identity; never retries refusal or other responses", async () => {
  for (const status of [0, 408, 429, 500, 503, 204, 200, 302, 400, 401, 403, 404, 409, 422]) {
    const sent: AdmissionSignal[] = [];
    const reporter = new CodexAdmissionReporter(1, () => true, async b => {
      sent.push(b);
      if (status === 0) throw new Error("lost ACK");
      return status;
    });
    emit(reporter);
    await barrier();
    const retries = [0, 408, 429, 500, 503].includes(status);
    assert.equal(sent.length, retries ? 2 : 1, String(status));
    if (retries) assert.deepEqual(sent[0], sent[1]);
  }
});

it("caps each attempt at 3s, and the absolute budget at 10s including queue time", async () => {
  for (const jump of [3000, 10_000]) {
    const clock = new Clock();
    const signals: AbortSignal[] = [];
    const reporter = new CodexAdmissionReporter(1, () => true, async (_b, signal) => {
      signals.push(signal); return new Promise<number>(() => {});
    }, clock);
    emit(reporter);
    emit(reporter, "serverOverloaded");
    await barrier();
    assert.equal(signals.length, 1);
    assert.deepEqual([...clock.timers.values()].map(t => t.at), [3000]);
    clock.advance(jump);
    await barrier();
    assert.ok(signals[0]!.aborted);
    assert.equal(signals.length, jump === 3000 ? 2 : 1);
    if (jump === 3000) {
      clock.advance(3000);
      await barrier();
      assert.ok(signals[1]!.aborted);
      clock.advance(4000); // queued sibling expires at the original observation deadline
      await barrier();
    }
    assert.equal(clock.timers.size, 0);
  }
});

it("nonclosing seal preserves active HTTP completion, discards pending and retries, and bounds lost ACKs", async () => {
  for (const result of [204, 503, "timeout"] as const) {
    const clock = new Clock();
    const sent: AdmissionSignal[] = [];
    let inflight!: AbortSignal;
    let release!: (status: number) => void;
    const reporter = new CodexAdmissionReporter(1, () => true, async (b, signal) => {
      sent.push(b); inflight = signal;
      return sent.length === 1 ? new Promise<number>(resolve => { release = resolve; }) : 204;
    }, clock);
    const old = emit(reporter);
    emit(reporter, "serverOverloaded");
    await barrier();
    reporter.seal();
    assert.equal(inflight.aborted, false);
    const fresh = emit(reporter);
    await barrier();
    assert.equal(sent.length, 1, "new evidence waits for the existing bounded send");
    if (result === "timeout") {
      clock.advance(2999);
      await barrier();
      assert.equal(inflight.aborted, false);
      clock.advance(1);
      await barrier();
      assert.equal(clock.timers.size, 0);
    } else {
      release(result);
      await barrier();
    }
    assert.deepEqual(sent.map(b => b.attempt_id), [old.id, fresh.id],
      "neither a queued old observation nor the old send's retry survives");
    assert.equal(clock.timers.size, 0);
    reporter.seal(true);
    emit(reporter);
    await barrier();
    assert.equal(sent.length, 2);
  }
});

it("a seal between drain scheduling and transport discards the still-unsent observation", async () => {
  const sent: AdmissionSignal[] = [];
  const reporter = new CodexAdmissionReporter(7, () => true, async body => { sent.push(body); return 204; });
  emit(reporter);
  queueMicrotask(() => reporter.seal());
  await barrier();
  assert.equal(sent.length, 0);
  const fresh = emit(reporter);
  await barrier();
  assert.deepEqual(sent.map(body => body.attempt_id), [fresh.id]);
});

it("missing capability and refused authority disable; capability loss drops pending work", async () => {
  let enabled = false;
  let sends = 0;
  const missing = new CodexAdmissionReporter(1, () => enabled, async () => { sends++; return 204; });
  emit(missing); await barrier(); assert.equal(sends, 0);
  enabled = true;
  emit(missing);
  enabled = false; // loss before queued send
  await barrier(); assert.equal(sends, 0);
  for (const refusal of [401, 403, 404]) {
    const refused = new CodexAdmissionReporter(1, () => true, async () => { sends++; return refusal; });
    emit(refused); emit(refused, "serverOverloaded");
    await barrier();
    const before: number = sends;
    emit(refused); await barrier();
    assert.equal(sends, before);
  }
});

it("queue overflow drops excess locally, keeps sibling work independent, and never changes lifecycle", async () => {
  const clock = new Clock();
  let sends = 0;
  let release!: (status: number) => void;
  const reporter = new CodexAdmissionReporter(1, () => true, async () => {
    sends++;
    return sends === 1 ? new Promise<number>(resolve => { release = resolve; }) : 204;
  }, clock);
  for (let i = 0; i < 80; i++) emit(reporter);
  await barrier();
  assert.equal(sends, 1);
  // Exactly 32 entries including the active one. Finishing it drains its siblings.
  release(204);
  await barrier();
  assert.equal(sends, 32);
  assert.equal(clock.timers.size, 0);
  reporter.seal(true);
});

const subscription = {
  auth_mode: "subscription" as const, access_token: "fixture-access", capability: "fixture-capability",
  generation: 3, chatgpt_account_id: "fixture-account", chatgpt_plan_type: null,
};
async function clientFixture(options: { kind?: "issue" | "job" | "chat"; mode?: "subscription" | "api_key" | "claude";
  features?: string[]; status?: number;
  onRequest?: (path: string, body: Record<string, unknown>, signal?: AbortSignal | null) => Response | Promise<Response> | undefined;
  sleep?: () => Promise<void> } = {}) {
  const claim = makeClaim({ kind: options.kind ?? "issue", claim_generation: 7,
    secrets: options.mode === "claude" ? { forge_pat: "fixture-pat" } : { forge_pat: "fixture-pat", codex: options.mode === "api_key"
      ? { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" } : subscription } });
  const requests: { path: string; body: Record<string, unknown>; signal?: AbortSignal | null }[] = [];
  const client = new WorkerClient("https://api.invalid", "join-fixture", "test", nullLogger(), {
    terminalRetrySchedule: [], sleep: options.sleep ?? (async () => {}),
    fetch: async (url, init) => {
      const path = String(url).replace("https://api.invalid", "");
      const body = JSON.parse(String(init?.body ?? "{}"));
      requests.push({ path, body, signal: init?.signal });
      const override = options.onRequest?.(path, body, init?.signal);
      if (override) return override;
      if (path.startsWith("/api/worker/runs/claim")) return Response.json(claim);
      if (path.endsWith("/codex/admission-signal")) return new Response(null, { status: options.status ?? 204 });
      return Response.json({ run: { id: claim.run_id, status: "running" }, applied: true, holds_released: 0 });
    },
  });
  client.protocolFeatures = options.features ?? [feature, "claim_generation_fence"];
  await client.claimRun();
  return { client, claim, requests };
}

it("only owned subscription claims grant authority; absent capability, API keys, Claude and jobs are excluded", async () => {
  for (const options of [{ mode: "api_key" as const }, { mode: "claude" as const },
    { kind: "job" as const }, { kind: "chat" as const }, { features: [] }]) {
    const { client, claim } = await clientFixture(options);
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
  }
  const { client, claim, requests } = await clientFixture();
  assert.equal(client.codexAdmissionReporter("unowned"), undefined);
  emit(client.codexAdmissionReporter(claim.run_id, 7)!);
  await barrier();
  const wire = requests.at(-1)!;
  assert.equal(wire.path, "/api/worker/runs/" + claim.run_id + "/codex/admission-signal");
  assert.equal(wire.body.claim_generation, 7); // never credential refresh generation 3
});

it("every state send and alternate lifecycle endpoint seals before transport, while nonclosing states admit later dispatches", async () => {
  for (const status of ["running", "awaiting_approval", "awaiting_input", "completed", "failed", "paused", "limit_wait"] as const) {
    const { client, claim, requests } = await clientFixture();
    const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
    emit(reporter);
    await client.reportState(claim.run_id, { status, claim_generation: 7 });
    emit(reporter, "serverOverloaded");
    await barrier();
    const sent = requests.filter(r => r.path.endsWith("/admission-signal"));
    assert.equal(sent.length, ["running", "awaiting_approval"].includes(status) ? 1 : 0, status);
  }
  for (const endpoint of ["checked", "semantic", "wall", "completion", "verdict", "code-verdict", "release", "pause"]) {
    const { client, claim, requests } = await clientFixture({ features: [feature, "claim_generation_fence", "gate_revision_v1"] });
    const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
    emit(reporter);
    const state = { status: "awaiting_approval" as const, claim_generation: 7 };
    if (endpoint === "checked") await client.reportPlanCrossCheckGateState(claim.run_id, state);
    if (endpoint === "semantic") await client.reportSemanticPlanGateState(claim.run_id, {
      ...state, status: "running", candidate_digest: "a".repeat(64),
    });
    if (endpoint === "wall") await client.reportWallPark(claim.run_id, { head: "a".repeat(40), published: false, claimGeneration: 7 });
    if (endpoint === "completion") await client.requestCompletionHold(claim.run_id, { head: "a".repeat(40), claimGeneration: 7 });
    if (endpoint === "verdict") await client.reportCrossCheckVerdict(claim.run_id, 7, { verdict: "approve", reason_class: "approve", summary: "fixture", items: [] });
    if (endpoint === "code-verdict") await client.reportCodeCrossCheckVerdict(claim.run_id, 7, { outcome: "failed", findings: [] });
    if (endpoint === "release") await client.releaseRecoveryCustody(claim.run_id, 7);
    if (endpoint === "pause") await client.requestWorkerPause(claim.run_id, 7);
    emit(reporter, "serverOverloaded");
    await barrier();
    const nonclosing = ["checked", "semantic", "pause"].includes(endpoint);
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, nonclosing ? 1 : 0, endpoint);
  }
});

it("pause intent preserves later evidence, while a paused or terminal ACK permanently closes the claim", async () => {
  for (const status of ["running", "paused", "completed"]) {
    const { client, claim, requests } = await clientFixture({
      onRequest: path => path.endsWith("/pause") || path.endsWith("/state")
        ? Response.json({ run: { status }, applied: true }) : undefined,
    });
    const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
    emit(reporter);
    const ack = await client.requestWorkerPause(claim.run_id, 7);
    assert.equal(ack.status, status);
    emit(reporter, "serverOverloaded");
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, status === "running" ? 1 : 0);
    await client.claimRun();
    emit(reporter);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, status === "running" ? 2 : 0);
  }
});

it("state and alternate sends fence before generation omission and strict-decode fallback, preserving caller ACKs", async () => {
  for (const endpoint of ["state", "completion", "wall"] as const) {
    for (const fallback of [false, true]) {
      let reporter: CodexAdmissionReporter | undefined;
      let stateSends = 0;
      let active: AbortSignal | null | undefined;
      let release!: (response: Response) => void;
      const { client, claim, requests } = await clientFixture({
        features: [feature], // no claim_generation_fence: the first body omits it
        onRequest: (path, body, signal) => {
          if (path.endsWith("/register")) return Response.json({ protocol_features: [feature, "claim_generation_fence"] });
          if (path.endsWith("/admission-signal")) {
            if (active) return new Response(null, { status: 204 });
            active = signal;
            return new Promise<Response>(resolve => { release = resolve; });
          }
          if (path.endsWith("/state") || path.endsWith("/completion/hold") || path.endsWith("/wall-park")) {
            stateSends++;
            if (fallback && stateSends === 1) {
              assert.equal(body.claim_generation, 8);
              emit(reporter!, "rateLimitExceeded");
              return new Response('{"error":"invalid request body"}', { status: 400 });
            }
            assert.equal(body.claim_generation, undefined);
            // Nonclosing reports leave an existing HTTP request bounded by its own timer.
            if (active) assert.equal(active.aborted, endpoint !== "state");
            return Response.json({ run: { status: "running" }, applied: true });
          }
          return undefined;
        },
      });
      if (fallback) {
        await client.register("fixture", undefined, undefined, undefined, ["credential_switch_v1"]);
        claim.claim_generation = 8;
        await client.claimRun();
      }
      const generation = claim.claim_generation!;
      reporter = client.codexAdmissionReporter(claim.run_id, generation)!;
      emit(reporter);
      if (endpoint === "state") {
        const ack = await client.reportState(claim.run_id, { status: "running", claim_generation: generation });
        assert.equal(ack.status, "running");
        assert.equal(ack.applied, true);
      } else if (endpoint === "completion") {
        assert.deepEqual(await client.requestCompletionHold(claim.run_id, { head: "a".repeat(40), claimGeneration: generation }), { status: "running" });
      } else {
        const ack = await client.reportWallPark(claim.run_id, { head: "a".repeat(40), published: false, claimGeneration: generation });
        assert.equal(ack.applied, true);
      }
      if (active) {
        release(new Response(null, { status: 204 }));
        await barrier();
      }
      emit(reporter, "serverOverloaded");
      await barrier();
      assert.equal(stateSends, fallback ? 2 : 1);
      const signals = requests.filter(r => r.path.endsWith("/admission-signal"));
      assert.equal(signals.filter(r => r.body.class === "overload").length, endpoint === "state" ? 1 : 0);
    }
  }
});

it("a nonclosing state POST completes before the live admission HTTP 204 without aborting it", async () => {
  let release!: (response: Response) => void;
  let pending: AbortSignal | null | undefined;
  const { client, claim, requests } = await clientFixture({
    onRequest: (path, _body, signal) => {
      if (path.endsWith("/admission-signal")) { pending = signal; return new Promise<Response>(resolve => { release = resolve; }); }
      if (path.endsWith("/register")) return Response.json({ protocol_features: [feature, "claim_generation_fence"] });
      return undefined;
    },
  });
  const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
  emit(reporter);
  await barrier();
  await client.reportState(claim.run_id, { status: "running", claim_generation: 7 });
  assert.ok(pending && !pending.aborted, "state ACK arrives while admission is still in flight");
  release(new Response(null, { status: 204 }));
  await barrier();
  assert.equal(pending.aborted, false, "the live HTTP 204 completes without cancellation");
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
});

it("exact-generation access isolates stale producers, and neither closed nor older claims can reopen authority", async () => {
  const { client, claim, requests } = await clientFixture();
  const old = client.codexAdmissionReporter(claim.run_id, 7)!;
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(claim.run_id, 7), old, "an exact live duplicate preserves its reporter");
  assert.equal(client.codexAdmissionReporter(claim.run_id), undefined);
  claim.claim_generation = 8;
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
  const current = client.codexAdmissionReporter(claim.run_id, 8)!;
  assert.notEqual(current, old);
  emit(old);
  emit(current);
  await barrier();
  assert.deepEqual(requests.filter(r => r.path.endsWith("/admission-signal")).map(r => r.body.claim_generation), [8]);
  await client.reportState(claim.run_id, { status: "completed", claim_generation: 8 });
  await client.claimRun(); // duplicate current response
  assert.equal(client.codexAdmissionReporter(claim.run_id, 8), undefined);
  emit(current, "serverOverloaded");
  claim.claim_generation = 7;
  await client.claimRun(); // out-of-order response
  assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
  await barrier();
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
});

it("same-generation ineligible claims cannot retain a subscription reporter", async () => {
  for (const variant of ["api_key", "claude", "job", "chat"] as const) {
    const { client, claim, requests } = await clientFixture();
    const old = client.codexAdmissionReporter(claim.run_id, 7)!;
    if (variant === "api_key") claim.secrets = { forge_pat: "fixture-pat",
      codex: { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" } };
    else if (variant === "claude") claim.secrets = { forge_pat: "fixture-pat" };
    else claim.kind = variant;
    await client.claimRun();
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined, variant);
    emit(old);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 0, variant);
  }
});

it("sequential closed-run churn beyond 1024 keeps the final run reporting", async () => {
  const { client, claim, requests } = await clientFixture();
  for (let i = 0; i < 1026; i++) {
    const reporter = client.codexAdmissionReporter(claim.run_id, 7);
    assert.ok(reporter, "sequential run " + i + " must retain admission reporting");
    await client.reportState(claim.run_id, { status: "completed", claim_generation: 7 });
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
    assertAdmissionBounds(client);
    claim.run_id = admissionRunId(i);
    await client.claimRun();
  }
  assertAdmissionBounds(client, 1, 1024);
  emit(client.codexAdmissionReporter(claim.run_id, 7)!);
  await barrier();
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
});

function admissionRunId(i: number): string {
  return `00000000-0000-4000-8000-${i.toString(16).padStart(12, "0")}`;
}

it("live overflow records every refused generation, then closing frees capacity", async () => {
  const { client, claim, requests } = await clientFixture();
  const first = claim.run_id;
  const firstReporter = client.codexAdmissionReporter(first, 7)!;
  for (let i = 0; i < 1023; i++) {
    claim.run_id = admissionRunId(i);
    await client.claimRun();
  }
  assertAdmissionBounds(client, 1024, 0);
  const refused = admissionRunId(2000);
  claim.run_id = refused;
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(refused, 7), undefined);
  assertAdmissionBounds(client, 1024, 1);
  await client.reportState(first, { status: "completed", claim_generation: 7 });
  emit(firstReporter);
  // Closing a refused generation must preserve its refusal fence.
  await client.reportState(refused, { status: "completed", claim_generation: 7 });
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(refused, 7), undefined);
  claim.run_id = admissionRunId(2001);
  await client.claimRun();
  emit(client.codexAdmissionReporter(claim.run_id, 7)!);
  await barrier();
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
  assertAdmissionBounds(client, 1024, 2);
});

function assertAdmissionBounds(client: WorkerClient, live?: number, closed?: number): void {
  const maps = client as unknown as { codexAdmission: Map<string, unknown>; codexAdmissionClosed: Map<string, number> };
  assert.ok(maps.codexAdmission.size <= 1024);
  assert.ok(maps.codexAdmissionClosed.size <= 1024);
  for (const generation of maps.codexAdmissionClosed.values()) assert.equal(typeof generation, "number");
  if (live !== undefined) assert.equal(maps.codexAdmission.size, live);
  if (closed !== undefined) assert.equal(maps.codexAdmissionClosed.size, closed);
}

it("old-generation reports do not revoke current authority and capability recovery permits fresh observations", async () => {
  const { client, claim, requests } = await clientFixture();
  const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
  await client.reportState(claim.run_id, { status: "completed", claim_generation: 6 });
  emit(reporter); await barrier();
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
  client.dropFeature(feature);
  client.protocolFeatures = [feature];
  emit(reporter, "serverOverloaded"); await barrier();
  assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 2);
});

it("stale and ended pause or state ACKs retire reporting without changing the acknowledgement", async () => {
  for (const endpoint of ["pause", "state"] as const) {
    for (const status of ["running", "awaiting_approval", "paused", "completed", "failed"]) {
      for (const stale of [false, true]) {
        const { client, claim, requests } = await clientFixture({
          onRequest: path => path.endsWith("/" + endpoint)
            ? Response.json({ run: { status }, applied: !stale,
              ...(stale ? { disposition: "stale_claim" } : {}) }, { status: stale ? 409 : 200 })
            : undefined,
        });
        const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
        emit(reporter);
        const ack = endpoint === "pause"
          ? await client.requestWorkerPause(claim.run_id, 7)
          : await client.reportState(claim.run_id, { status: "running", claim_generation: 7 });
        assert.equal(ack.status, status);
        assert.equal(ack.applied, !stale);
        assert.equal(ack.staleClaim === true, stale);
        emit(reporter, "serverOverloaded");
        await barrier();
        const live = !stale && ["running", "awaiting_approval"].includes(status);
        assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length,
          live ? 1 : 0, endpoint + "/" + status + "/stale=" + stale);
        await client.claimRun();
        assert.equal(client.codexAdmissionReporter(claim.run_id, 7), live ? reporter : undefined);
        emit(reporter);
        await barrier();
        assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length,
          live ? 2 : 0, "a repeated claim must not reopen retired telemetry");
      }
    }
  }
});

it("a stale lifecycle ACK for another generation cannot retire the current reporter", async () => {
  for (const endpoint of ["pause", "state"] as const) {
    const { client, claim, requests } = await clientFixture({
      onRequest: path => path.endsWith("/" + endpoint)
        ? Response.json({ run: { status: "running" }, applied: false, disposition: "stale_claim" }, { status: 409 })
        : undefined,
    });
    const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
    if (endpoint === "pause") await client.requestWorkerPause(claim.run_id, 6);
    else await client.reportState(claim.run_id, { status: "running", claim_generation: 6 });
    emit(reporter);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
  }
});

it("queued retry receives only the remaining absolute budget", async () => {
  const clock = new Clock();
  const sends: { at: number; signal: AbortSignal }[] = [];
  const reporter = new CodexAdmissionReporter(7, () => true, async (_body, signal) => {
    sends.push({ at: clock.now(), signal });
    return new Promise<number>(() => {});
  }, clock);
  emit(reporter);
  emit(reporter, "serverOverloaded");
  await barrier();
  for (let i = 0; i < 3; i++) { clock.advance(3000); await barrier(); }
  assert.deepEqual(sends.map(s => s.at), [0, 3000, 6000, 9000]);
  assert.deepEqual([...clock.timers.values()].map(t => t.at), [10000]);
  clock.advance(1000);
  await barrier();
  assert.ok(sends.every(s => s.signal.aborted));
  assert.equal(clock.timers.size, 0);
  assert.equal(sends.length, 4);
  reporter.seal(true);
});

it("registration and every feature-loss path discard pending work and recover only fresh live evidence", async () => {
  for (const transition of ["register", "replace", "clear", "drop"] as const) {
    const { client, claim, requests } = await clientFixture({
      onRequest: path => path.endsWith("/register")
        ? Response.json({ protocol_features: [] }) : undefined,
    });
    const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
    const pending = emit(reporter);
    if (transition === "register") await client.register("fixture");
    else if (transition === "replace") client.protocolFeatures = [];
    else if (transition === "clear") client.clearFeatures();
    else client.dropFeature(feature);
    const absent = emit(reporter);
    const unobserved = reporter.begin("thread");
    unobserved.bind("turn");
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 0, transition);
    client.protocolFeatures = [feature, "claim_generation_fence"];
    pending.observe(note("rateLimitExceeded"));
    absent.observe(note("rateLimitExceeded"));
    unobserved.observe(note("serverOverloaded"));
    const fresh = emit(reporter);
    await barrier();
    assert.deepEqual(requests.filter(r => r.path.endsWith("/admission-signal")).map(r => r.body.attempt_id),
      [fresh.id], transition + ": discarded and capability-absent attempts cannot replay");
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), reporter);
  }
});

it("register with the feature still advertised discards queued evidence and keeps the live generation", async () => {
  const { client, claim, requests } = await clientFixture({
    onRequest: path => path.endsWith("/register")
      ? Response.json({ protocol_features: [feature, "claim_generation_fence"] }) : undefined,
  });
  const reporter = client.codexAdmissionReporter(claim.run_id, 7)!;
  emit(reporter);
  await client.register("fixture");
  const fresh = emit(reporter);
  await barrier();
  assert.deepEqual(requests.filter(r => r.path.endsWith("/admission-signal")).map(r => r.body.attempt_id), [fresh.id]);
});

it("feature recovery and register never revive ended, refused, or stale-generation reporters", async () => {
  for (const closure of ["ended", "refused", "stale", "stale-ack"] as const) {
    const { client, claim, requests } = await clientFixture({
      status: closure === "refused" ? 403 : 204,
      onRequest: path => path.endsWith("/register")
        ? Response.json({ protocol_features: [feature, "claim_generation_fence"] })
        : closure === "stale-ack" && path.endsWith("/state")
          ? Response.json({ run: { status: "running" }, applied: false, disposition: "stale_claim" }, { status: 409 })
          : undefined,
    });
    const old = client.codexAdmissionReporter(claim.run_id, 7)!;
    if (closure === "refused") { emit(old); await barrier(); }
    else if (closure === "stale") { claim.claim_generation = 8; await client.claimRun(); }
    else await client.reportState(claim.run_id, {
      status: closure === "ended" ? "completed" : "running", claim_generation: 7,
    });
    const before = requests.filter(r => r.path.endsWith("/admission-signal")).length;
    client.clearFeatures();
    emit(old);
    await client.register("fixture");
    await client.claimRun();
    emit(old, "serverOverloaded");
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, before, closure);
  }
});

it("closed-mark eviction fences delayed ordinary and cross-check responses without dropping claims", async () => {
  for (const crossCheck of [false, true]) {
    let hold = false;
    let release!: (response: Response) => void;
    let arrived!: () => void;
    const arrival = new Promise<void>(resolve => { arrived = resolve; });
    const { client, claim, requests } = await clientFixture({
      onRequest: path => {
        if (hold && path.startsWith("/api/worker/runs/claim")) {
          hold = false;
          arrived();
          return new Promise<Response>(resolve => { release = resolve; });
        }
        return undefined;
      },
    });
    const original = { ...claim };
    const old = client.codexAdmissionReporter(original.run_id, 7)!;
    hold = true;
    const delayed = crossCheck ? client.claimCrossCheck() : client.claimRun();
    await arrival;
    await client.reportState(original.run_id, { status: "completed", claim_generation: 7 });
    for (let i = 0; i < 1024; i++) {
      claim.run_id = admissionRunId(i);
      await client.claimRun();
      await client.reportState(claim.run_id, { status: "completed", claim_generation: 7 });
    }
    assertAdmissionBounds(client, 0, 1024);
    const stale = { ...original, ...(crossCheck ? { kind: "cross_check" } : {}) };
    release(Response.json(stale, crossCheck ? { headers: { "X-Uzi-Claim-Kind": "cross_check" } } : undefined));
    assert.deepEqual(await delayed, stale, "observation fencing leaves the ordinary claim result intact");
    assert.equal(client.codexAdmissionReporter(original.run_id, 7), undefined);
    emit(old);
    // A future authoritative claim increments generation; telemetry never asks to reclaim.
    claim.run_id = original.run_id;
    claim.claim_generation = 8;
    await client.claimRun();
    emit(client.codexAdmissionReporter(claim.run_id, 8)!);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
  }
});

it("a capacity-refused newer generation fences an older delayed claim after capacity opens", async () => {
  let hold = false;
  let release!: (response: Response) => void;
  let arrived!: () => void;
  const arrival = new Promise<void>(resolve => { arrived = resolve; });
  const { client, claim, requests } = await clientFixture({
    onRequest: path => {
      if (hold && path.startsWith("/api/worker/runs/claim")) {
        hold = false; arrived();
        return new Promise<Response>(resolve => { release = resolve; });
      }
      return undefined;
    },
  });
  const first = claim.run_id;
  for (let i = 0; i < 1023; i++) {
    claim.run_id = admissionRunId(i);
    await client.claimRun();
  }
  claim.run_id = admissionRunId(2000);
  const pendingClaim = { ...claim };
  hold = true;
  const delayed = client.claimRun();
  await arrival;
  claim.claim_generation = 8;
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(claim.run_id, 8), undefined);
  await client.reportState(claim.run_id, { status: "completed", claim_generation: 8 });
  await client.reportState(first, { status: "completed", claim_generation: 7 });
  release(Response.json(pendingClaim));
  assert.deepEqual(await delayed, pendingClaim);
  assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
  await client.claimRun();
  assert.equal(client.codexAdmissionReporter(claim.run_id, 8), undefined);
  claim.claim_generation = 9;
  await client.claimRun();
  emit(client.codexAdmissionReporter(claim.run_id, 9)!);
  await barrier();
  assert.deepEqual(requests.filter(r => r.path.endsWith("/admission-signal")).map(r => r.body.claim_generation), [9]);
  assertAdmissionBounds(client, 1024, 2);
});

it("delayed old state, pause and signal refusals cannot retire a replacement", async () => {
  for (const endpoint of ["state", "pause", "admission-signal"] as const) {
    let release!: (response: Response) => void;
    let arrived!: () => void;
    const arrival = new Promise<void>(resolve => { arrived = resolve; });
    let hold = true;
    const { client, claim, requests } = await clientFixture({
      onRequest: path => {
        if (hold && path.endsWith("/" + endpoint)) {
          hold = false; arrived();
          return new Promise<Response>(resolve => { release = resolve; });
        }
        return undefined;
      },
    });
    const old = client.codexAdmissionReporter(claim.run_id, 7)!;
    let pending: Promise<unknown> | undefined;
    if (endpoint === "state") pending = client.reportState(claim.run_id, { status: "running", claim_generation: 7 });
    else if (endpoint === "pause") pending = client.requestWorkerPause(claim.run_id, 7);
    else emit(old);
    await arrival;
    claim.claim_generation = 8;
    await client.claimRun();
    const replacement = client.codexAdmissionReporter(claim.run_id, 8)!;
    release(endpoint === "admission-signal" ? new Response(null, { status: 401 })
      : Response.json({ run: { status: "completed" }, applied: false, disposition: "stale_claim" }, { status: 409 }));
    if (pending) await pending;
    await barrier();
    assert.equal(client.codexAdmissionReporter(claim.run_id, 8), replacement);
    emit(old);
    emit(replacement);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal") && r.body.claim_generation === 8).length, 1);
    assertAdmissionBounds(client, 1, 1);
  }
});

it("dedicated signal 401, 403 and 404 retire ownership and release live capacity", async () => {
  for (const status of [401, 403, 404]) {
    const { client, claim, requests } = await clientFixture({ status });
    const old = client.codexAdmissionReporter(claim.run_id, 7)!;
    emit(old);
    await barrier();
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
    assertAdmissionBounds(client, 0, 1);
    await client.claimRun();
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined);
    emit(old);
    claim.run_id = admissionRunId(status);
    await client.claimRun();
    assert.ok(client.codexAdmissionReporter(claim.run_id, 7));
    assertAdmissionBounds(client, 1, 1);
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 1);
  }
});

it("registration and feature revocation fence pending claims while fresh claims bind", async () => {
  for (const transition of ["register", "replace", "clear", "drop"] as const) {
    let hold = false;
    let release!: (response: Response) => void;
    let arrived!: () => void;
    const arrival = new Promise<void>(resolve => { arrived = resolve; });
    const { client, claim, requests } = await clientFixture({
      onRequest: path => {
        if (hold && path.startsWith("/api/worker/runs/claim")) {
          hold = false; arrived();
          return new Promise<Response>(resolve => { release = resolve; });
        }
        if (path.endsWith("/register")) return Response.json({ protocol_features: [feature, "claim_generation_fence"] });
        return undefined;
      },
    });
    const retained = client.codexAdmissionReporter(claim.run_id, 7)!;
    claim.run_id = admissionRunId(3000);
    const delayedClaim = { ...claim };
    hold = true;
    const delayed = client.claimRun();
    await arrival;
    if (transition === "register") await client.register("fixture");
    else if (transition === "replace") client.protocolFeatures = [];
    else if (transition === "clear") client.clearFeatures();
    else client.dropFeature(feature);
    client.protocolFeatures = [feature, "claim_generation_fence"];
    release(Response.json(delayedClaim));
    assert.deepEqual(await delayed, delayedClaim);
    assert.equal(client.codexAdmissionReporter(claim.run_id, 7), undefined, transition);
    emit(retained);
    claim.run_id = admissionRunId(3001);
    await client.claimRun();
    emit(client.codexAdmissionReporter(claim.run_id, 7)!);
    await barrier();
    assert.equal(requests.filter(r => r.path.endsWith("/admission-signal")).length, 2, transition);
    assertAdmissionBounds(client, 2, 0);
  }
});
