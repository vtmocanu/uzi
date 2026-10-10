import assert from "node:assert/strict";
import { it } from "node:test";
import { WorkerClient, RequestError, isStrictDecodeError, isTransient, type PlanCrossCheckCandidate } from "../src/client.js";
import { nullLogger, makeClaim } from "./helpers.js";

it("cleanup metadata rejects ordinary ownership and nonexact identities", async (t) => {
  const lead = "11111111-1111-4111-8111-111111111111";
  const valid = { protocol: "code_snapshot_cleanup_v1", lead_run_id: lead,
    head_commit: "a".repeat(40), outcome: "completed", lead_status: "running",
    owned_by_worker: true, checker_run_id: "22222222-2222-4222-8222-222222222222",
    checker_claim_generation: 1, checker_status: "completed" };
  t.mock.method(globalThis, "fetch", async (url: Parameters<typeof fetch>[0]) => {
    assert.match(String(url), /ownership\?purpose=code_snapshot/);
    return Response.json(valid);
  });
  assert.deepEqual(await client().getCodeSnapshotCleanup(lead), valid);
  for (const body of [{ status: "completed", claim_generation: 1 },
    { ...valid, extra: true }, { ...valid, head_commit: valid.head_commit + "\\n" },
    { ...valid, checker_claim_generation: 0 }]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(body));
    await assert.rejects(client().getCodeSnapshotCleanup(lead), /invalid code snapshot cleanup/);
  }
});

it("legacy plan claims and statuses decode without code-stage fields", async (t) => {
  const legacy = makeClaim({ kind: "cross_check", cross_check: { stage: "plan", lead_run_id: "lead", round: 1,
    candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
    plan_md: "legacy plan", milestones: [], required_capabilities: [], required_tools: [],
    size_class: "s", base_commit: "a".repeat(40), planning_diff: "" } });
  t.mock.method(globalThis, "fetch", async () => Response.json(legacy, { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
  const decoded = await client().claimCrossCheck();
  assert.equal(decoded?.cross_check?.stage, "plan");
  assert.equal(decoded?.code_cross_check_required, undefined);
  assert.equal(decoded?.cross_check && "head_commit" in decoded.cross_check, false);
  t.mock.method(globalThis, "fetch", async () => Response.json(wire()));
  const status = await client().planCrossCheckStatus("lead", 3);
  assert.equal(status.result, "candidate");
  assert.equal("head_commit" in status, false);
});

it("production dedicated claim client discriminates stages and retains plan requirements", async (t) => {
  const plan = makeClaim({ kind: "cross_check", cross_check: { stage: "plan", lead_run_id: "lead", round: 1,
    candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
    plan_md: "plan", milestones: [], required_capabilities: [], required_tools: [],
    size_class: "s", base_commit: "a".repeat(40), planning_diff: "" } });
  for (const cross_check of [
    { ...plan.cross_check, stage: "unknown" },
    { ...plan.cross_check, planning_diff: undefined },
    { ...plan.cross_check, milestones: undefined },
    { ...plan.cross_check, stage: "code", head_commit: "bad", code_context: {} },
  ]) {
    t.mock.method(globalThis, "fetch", async () => Response.json({ ...plan, cross_check },
      { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
    await assert.rejects(client().claimCrossCheck(), /invalid cross-check claim/);
  }
  t.mock.method(globalThis, "fetch", async () => Response.json({ ...plan, cross_check: {
    ...plan.cross_check, stage: "code", head_commit: "d".repeat(40), code_context: {},
    planning_diff: undefined, guidance_snapshot: undefined,
  } }, { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
  assert.equal((await client().claimCrossCheck())?.cross_check?.stage, "code");
});

it("production code claim rejects lead identifiers with trailing newline or controls", async (t) => {
  const cross_check = { stage: "code", lead_run_id: "valid_lead", round: 1,
    candidate_digest: "b".repeat(64), deadline_at: "2030-01-01T00:00:00Z",
    plan_md: "plan", base_commit: "a".repeat(40), head_commit: "d".repeat(40), code_context: {} };
  for (const suffix of ["", "\n", "\r", "\u0000", "\u001b", "\u202e"]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(makeClaim({ kind: "cross_check",
      cross_check: { ...cross_check, lead_run_id: cross_check.lead_run_id + suffix } as NonNullable<ReturnType<typeof makeClaim>["cross_check"]> }),
      { headers: { "X-Uzi-Claim-Kind": "cross_check" } }));
    if (suffix) await assert.rejects(client().claimCrossCheck(), /invalid cross-check claim/);
    else assert.equal((await client().claimCrossCheck())?.cross_check?.lead_run_id, "valid_lead");
  }
});

it("production code verdict client rejects duplicate IDs, non-ASCII IDs and actual UTF-8 bounds before HTTP", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json({}));
  const finding = { id: "F1", severity: "major" as const, path: "source.ts", line: 1, title: "defect", detail: "" };
  const valid = { ...finding, detail: "é".repeat(900) };
  const c = client();
  await c.reportCodeCrossCheckVerdict("child", 1, { outcome: "completed", findings: [valid] });
  assert.equal(fetch.mock.callCount(), 1);
  for (const findings of [
    [finding, finding],
    [{ ...finding, id: "é" }],
    [{ ...finding, id: "valid_ID\n" }],
    [{ ...finding, id: "valid_ID\r" }],
    [{ ...finding, id: "control\u0000" }],
    [{ ...finding, id: "ansi\u001b[31m" }],
    [{ ...finding, id: "bidi\u202e" }],
    [{ ...finding, detail: "é".repeat(1024) }],
    Array.from({ length: 21 }, (_, i) => ({ ...finding, id: String(i) })),
    Array.from({ length: 20 }, (_, i) => ({ ...valid, id: String(i) })),
  ]) await assert.rejects(c.reportCodeCrossCheckVerdict("child", 1, { outcome: "completed", findings }), /invalid code/);
  assert.equal(fetch.mock.callCount(), 1, "invalid output never reaches HTTP");
});

const candidate: PlanCrossCheckCandidate = { plan_md: "plan", planning_diff: "", milestones: [],
  required_capabilities: [], required_tools: [], size_class: "s", base_commit: "a".repeat(40) };
const wire = () => ({ result: "candidate", round: 1, checker_run_id: null,
  candidate_digest: "b".repeat(64), candidate_generation: 3, candidate,
  verdict: "approve", reason_class: "approve", findings: null,
  deadline_at: "2030-01-01T00:00:00Z", lead_last_seq: 4 });
const client = () => new WorkerClient("http://example.com", "fixture-join", "test", nullLogger(),
  { sleep: async () => {} });

it("submit and status validate stored variants before exposing approve", async (t) => {
  const malformed = [null, {}, { ...wire(), round: 2 }, { ...wire(), candidate_digest: "g".repeat(64) },
    { ...wire(), candidate_generation: 0 }, { ...wire(), checker_run_id: "not-uuid" },
    { ...wire(), deadline_at: "tomorrow" }, { ...wire(), deadline_at: "2030-02-30T00:00:00Z" },
    { ...wire(), lead_last_seq: 0x80000000 },
    { ...wire(), reason_class: "revise" }, { ...wire(), verdict: "unknown" },
    { ...wire(), candidate: { ...candidate, milestones: null } },
    { ...wire(), candidate: { ...candidate, base_commit: "g".repeat(40) } },
    { ...wire(), candidate: { ...candidate, required_tools: [1] } },
    { ...wire(), candidate: { ...candidate, required_capabilities: ["x".repeat(257)] } },
    { ...wire(), candidate: { ...candidate, plan_md: "é".repeat(128 * 1024 + 1) } },
    { ...wire(), candidate: { ...candidate, planning_diff: "x".repeat(512 * 1024 + 1) } },
    { ...wire(), candidate: { ...candidate, milestones: Array(65).fill({ id: "m", title: "title" }) } },
    { ...wire(), findings: { summary: "x".repeat(4097), items: [] } },
    { ...wire(), findings: { summary: "", items: [{ file: "", severity: "critical", summary: "", rationale: "" }] } },
    { result: "no_row", reason_class: "unknown", lead_last_seq: 0 },
    { result: "no_row", reason_class: "no_candidate", lead_last_seq: -1 }];
  for (const call of [
    (c: WorkerClient) => c.submitPlanCrossCheck("run", 3, candidate),
    (c: WorkerClient) => c.planCrossCheckStatus("run", 3),
  ]) {
    for (const value of malformed) {
      t.mock.method(globalThis, "fetch", async () => Response.json(value));
      await assert.rejects(call(client()), /invalid.*cross-check/);
    }
    t.mock.method(globalThis, "fetch", async () => Response.json({ ...wire(),
      candidate: { ...candidate, required_capabilities: null, required_tools: null },
      findings: { summary: "", items: null } }));
    const decoded = await call(client());
    assert.equal(decoded.result, "candidate");
    if (decoded.result !== "candidate") throw new Error("missing candidate");
    assert.deepEqual(decoded.candidate.required_capabilities, []);
    assert.deepEqual(decoded.candidate.required_tools, []);
    assert.deepEqual(decoded.findings?.items, []);
  }
});

it("strict checked state rejects skew once with its exact generation and presentation identity", async (t) => {
  const c = client();
  c.protocolFeatures = ["gate_revision_v1", "claim_generation_fence"];
  const body = { status: "awaiting_approval" as const, claim_generation: 3,
    presentation_id: "8b0c1a2e-3f4d-4e5f-8a6b-7c8d9e0f1a2b", adopt_gate_revision: 2,
    plan_cross_check_gate_reason: "interrupted" as const };
  const sent: unknown[] = [];
  const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
    sent.push(JSON.parse(init!.body as string));
    return Response.json({ error: "invalid request body" }, { status: 400 });
  });
  await assert.rejects(c.reportPlanCrossCheckGateState("run", body), (err: unknown) => {
    assert.ok(err instanceof RequestError);
    assert.equal(isStrictDecodeError(err), true, "the response exercises the exact downgrade discriminator");
    return true;
  });
  assert.equal(fetch.mock.callCount(), 1);
  assert.deepEqual(sent, [body]);
});

const ackMaxBytes = 6 * 1024 * 1024;
const checkedState = { status: "awaiting_approval" as const, claim_generation: 3 };

function streamedAck(byteLength: number, contentLength?: string) {
  const prefix = Buffer.from(JSON.stringify({ run: { status: "awaiting_approval" }, gate_revision: 2 }));
  let emitted = 0;
  let cancellations = 0;
  const stream = new ReadableStream<Uint8Array>({
    pull(controller) {
      if (emitted === byteLength) {
        controller.close();
        return;
      }
      const chunk = Buffer.alloc(Math.min(64 * 1024, byteLength - emitted), 32);
      if (emitted === 0) prefix.copy(chunk);
      emitted += chunk.byteLength;
      controller.enqueue(chunk);
    },
    cancel() { cancellations++; },
  }, { highWaterMark: 0 });
  return {
    response: new Response(stream, { headers: contentLength === undefined ? {} : { "Content-Length": contentLength } }),
    cancellations: () => cancellations,
    emitted: () => emitted,
  };
}

it("strict checked state rejects actual streamed ACK overflow and cancels without trusting Content-Length", async (t) => {
  for (const contentLength of [undefined, "64"]) {
    const streamed = streamedAck(8_388_659, contentLength);
    let posts = 0;
    const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
      assert.equal(init?.method, "POST");
      return ++posts === 1 ? streamed.response : Response.json({ run: { status: "awaiting_approval" }, gate_revision: 2 });
    });
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    await assert.rejects(c.reportPlanCrossCheckGateState("run", checkedState), (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.equal(err.name, "ResponseBodyOverflowError");
      assert.match(err.message, /response body exceeds 6291456 bytes/);
      assert.equal(isTransient(err), false);
      return true;
    });
    assert.equal(streamed.cancellations(), 1);
    assert.equal(streamed.emitted(), ackMaxBytes + 64 * 1024, "stop at the first overflowing chunk");
    assert.equal(fetch.mock.callCount(), 1);
  }
});

it("strict checked state accepts large ACKs through EOF at and below the byte cap", async (t) => {
  for (const byteLength of [ackMaxBytes - 1, ackMaxBytes]) {
    const streamed = streamedAck(byteLength);
    t.mock.method(globalThis, "fetch", async () => streamed.response);
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    const ack = await c.reportPlanCrossCheckGateState("run", checkedState);
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "awaiting_approval");
    assert.equal(ack.gateRevision, 2);
    assert.equal(streamed.emitted(), byteLength);
  }
});

it("strict checked state propagates ACK stream failures while ordinary state retains compatibility", async (t) => {
  const response = () => new Response(new ReadableStream<Uint8Array>({
    pull() { throw new Error("fixture ACK stream failure"); },
  }));
  t.mock.method(globalThis, "fetch", async () => response());
  const c = client();
  c.protocolFeatures = ["gate_revision_v1"];
  await assert.rejects(c.reportPlanCrossCheckGateState("run", checkedState), /fixture ACK stream failure/);
  assert.deepEqual(await c.reportState("run", { status: "running" }), { applied: true, status: undefined });
});

it("strict checked state retries identical requests after transient transport or ACK stream failures", async (t) => {
  for (const failure of ["transport", "stream"]) {
    const sent: unknown[] = [];
    const fetch = t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
      assert.equal(init?.method, "POST");
      sent.push(JSON.parse(init!.body as string));
      if (sent.length === 1) {
        if (failure === "transport") throw new TypeError("fixture fetch failure");
        return new Response(new ReadableStream<Uint8Array>({
          pull() { throw new Error("fixture ACK stream failure"); },
        }));
      }
      return Response.json({ run: { status: "awaiting_approval" }, gate_revision: 2 });
    });
    const c = client();
    c.protocolFeatures = ["gate_revision_v1"];
    const ack = await c.reportPlanCrossCheckGateState("run", checkedState);
    assert.equal(ack.applied, true);
    assert.equal(ack.status, "awaiting_approval");
    assert.equal(ack.gateRevision, 2);
    assert.equal(fetch.mock.callCount(), 2);
    assert.deepEqual(sent, [checkedState, checkedState]);
  }
});

it("ordinary state still accepts an ACK larger than the strict byte cap", async (t) => {
  const streamed = streamedAck(ackMaxBytes + 1);
  t.mock.method(globalThis, "fetch", async () => streamed.response);
  const ack = await client().reportState("run", { status: "running" });
  assert.equal(ack.applied, true);
  assert.equal(ack.status, "awaiting_approval");
  assert.equal(streamed.emitted(), ackMaxBytes + 1);
});

it("strict checked state rejects missing negotiation or generation without sending", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => { throw new Error("must not send"); });
  const c = client();
  await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation: 3 }));
  c.protocolFeatures = ["gate_revision_v1"];
  for (const claim_generation of [undefined, 0, -1, 1.5, NaN, Infinity])
    await assert.rejects(c.reportPlanCrossCheckGateState("run", { status: "running", claim_generation }));
  assert.equal(fetch.mock.callCount(), 0);
});

const codeFinding = { id: "F_1-", severity: "major" as const, path: "src/code.ts", line: 1,
  title: "defect", detail: "details" };
const disposition = { finding_id: codeFinding.id, disposition: "addressed" as const, reason: "fixed" };
const codeWire = () => ({ stage: "code", round: 1, candidate_generation: 3,
  base_commit: "a".repeat(40), head_commit: "b".repeat(40), candidate_digest: "c".repeat(64),
  checker_run_id: "22222222-2222-4222-8222-222222222222", checker_harness: "codex",
  checker_model: "fixture-model", checker_effort: "high", outcome: "completed",
  reason_class: null, findings: [codeFinding], dispositions: null,
  interrupted_at: null, finalized_at: null, deadline_at: "2030-01-01T00:00:00Z" });
const finalizedCodeWire = () => ({ ...codeWire(), dispositions: [disposition],
  finalized_at: "2030-01-01T00:00:00Z" });

it("code disposition POST sends literal keys, allows empty finalizer, and preserves abort", async (t) => {
  const controller = new AbortController();
  t.mock.method(globalThis, "fetch", async (url: unknown, init?: RequestInit) => {
    assert.equal(String(url), "http://example.com/api/worker/runs/lead/cross-checks/code/dispositions");
    assert.equal(init?.method, "POST");
    const body = JSON.parse(init!.body as string);
    assert.deepEqual(body, { claim_generation: 3, dispositions: [disposition] });
    assert.ok(init?.signal);
    controller.abort();
    assert.equal(init.signal.aborted, true);
    return Response.json(finalizedCodeWire());
  });
  const result = await client().reportCodeCrossCheckDispositions("lead", 3, [disposition], controller.signal);
  assert.ok("dispositions" in result);
  assert.deepEqual(result.dispositions, [disposition]);
  t.mock.method(globalThis, "fetch", async (_url: unknown, init?: RequestInit) => {
    assert.deepEqual(JSON.parse(init!.body as string), { claim_generation: 3, dispositions: [] });
    return Response.json({ ...finalizedCodeWire(),
      dispositions: [{ finding_id: codeFinding.id, disposition: "not_reported", reason: "" }] });
  });
  const empty = await client().reportCodeCrossCheckDispositions("lead", 3, []);
  assert.ok("dispositions" in empty);
  assert.equal(empty.dispositions?.[0]?.disposition, "not_reported");
});

it("code dispositions validate generation, ASCII IDs and UTF-8 reason bytes before HTTP", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => Response.json(finalizedCodeWire()));
  const c = client();
  const exact = "é".repeat(512);
  await c.reportCodeCrossCheckDispositions("lead", 3, [{ ...disposition, reason: exact }]);
  assert.equal(Buffer.byteLength(exact), 1024);
  for (const generation of [0, -1, 1.5, NaN, Infinity, Number.MAX_SAFE_INTEGER + 1])
    await assert.rejects(c.reportCodeCrossCheckDispositions("lead", generation, []), /invalid code/);
  const bad = [
    [disposition, disposition], [{ ...disposition, reason: exact + "a" }],
    [{ ...disposition, reason: " " }], [{ ...disposition, reason: "\uD800" }],
    [{ ...disposition, disposition: "not_reported", reason: "" }],
    [{ ...disposition, id: codeFinding.id }], null, {},
    Array.from({ length: 21 }, (_, i) => ({ ...disposition, finding_id: String(i) })),
    ...["", "é", "a".repeat(65), "F1\n", "F1\r", "F1\u0000", "F1\u001b", "F1\u202e", " F1"]
      .map(finding_id => [{ ...disposition, finding_id }]),
  ];
  for (const batch of bad)
    await assert.rejects(c.reportCodeCrossCheckDispositions("lead", 3,
      batch as Parameters<WorkerClient["reportCodeCrossCheckDispositions"]>[2]), /invalid code/);
  assert.equal(fetch.mock.callCount(), 1, "invalid batches never reach HTTP");
});

it("code status keeps M1 rollout compatibility and interrupted worker projection", async (t) => {
  for (const body of [
    { ...codeWire(), dispositions: undefined, finalized_at: undefined },
    { ...codeWire(), dispositions: undefined, finalized_at: "2030-01-01T00:00:00Z" },
    codeWire(),
    { ...finalizedCodeWire(), findings: [], dispositions: null, reason_class: "interrupted",
      interrupted_at: "2030-01-01T00:00:00Z" },
  ]) {
    t.mock.method(globalThis, "fetch", async () => Response.json(body));
    const status = await client().codeCrossCheckStatus("lead", 3);
    assert.ok("findings" in status);
    assert.equal(status.dispositions, null);
    if (body.interrupted_at) assert.deepEqual(status.findings, []);
    else assert.equal(status.finalized_at, null);
  }
});

it("code response refuses invalid, incomplete or unknown persisted dispositions through real clients", async (t) => {
  const exact = "é".repeat(512);
  const invalid = [
    { ...finalizedCodeWire(), dispositions: null },
    { ...finalizedCodeWire(), dispositions: [] },
    { ...finalizedCodeWire(), dispositions: [disposition, disposition] },
    { ...finalizedCodeWire(), dispositions: {} },
    { ...finalizedCodeWire(), finalized_at: null },
    { ...finalizedCodeWire(), finalized_at: "invalid" },
    { ...finalizedCodeWire(), outcome: "failed", findings: [] },
    { ...finalizedCodeWire(), interrupted_at: "2030-01-01T00:00:00Z", findings: [] },
    ...["unknown", "", "é", "a".repeat(65), "F1\n", "F1\r", "F1\u0000", "F1\u001b", "F1\u202e"]
      .map(finding_id => ({ ...finalizedCodeWire(), dispositions: [{ ...disposition, finding_id }] })),
    ...["", " ", exact + "a", "\uD800"].map(reason =>
      ({ ...finalizedCodeWire(), dispositions: [{ ...disposition, reason }] })),
    { ...finalizedCodeWire(), dispositions: [{ ...disposition, disposition: "unknown" }] },
    { ...finalizedCodeWire(), dispositions: [{ ...disposition, disposition: "not_reported" }] },
    { ...finalizedCodeWire(), dispositions: [{ ...disposition, id: codeFinding.id }] },
  ];
  for (const call of [
    (c: WorkerClient) => c.codeCrossCheckStatus("lead", 3),
    (c: WorkerClient) => c.submitCodeCrossCheck("lead", 3, { base_commit: "a".repeat(40), head_commit: "b".repeat(40) }),
    (c: WorkerClient) => c.reportCodeCrossCheckDispositions("lead", 3, [disposition]),
  ]) {
    for (const body of invalid) {
      t.mock.method(globalThis, "fetch", async () => Response.json(body));
      await assert.rejects(call(client()), /invalid code cross-check/);
    }
    for (const d of [{ ...disposition, reason: exact }, { ...disposition, disposition: "declined", reason: exact },
      { ...disposition, disposition: "not_reported", reason: "" }]) {
      t.mock.method(globalThis, "fetch", async () => Response.json({ ...finalizedCodeWire(), dispositions: [d] }));
      const status = await call(client());
      assert.ok("dispositions" in status);
      assert.deepEqual(status.dispositions, [d]);
    }
  }
});

it("code disposition response uses the bounded reader despite false Content-Length", async (t) => {
  const stream = streamedAck(ackMaxBytes + 1, "1");
  const fetch = t.mock.method(globalThis, "fetch", async () => stream.response);
  await assert.rejects(client().reportCodeCrossCheckDispositions("lead", 3, []), /response body exceeds/);
  assert.equal(fetch.mock.callCount(), 1);
  assert.equal(stream.cancellations(), 1);
});
