import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import { WorkerClient } from "../src/client.js";
import {
  TERMINAL_CONTRIBUTION_STATES,
  TERMINAL_REASONS,
  type TerminalPreservationRequest,
  type TerminalPreservationResponse,
} from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// Issue #4007 slice B: the worker side of the terminal-preservation wire contract, pinned to the api's
// shared fixtures (fixtures/terminal-preservation) so a drift on either side reddens a test.
const dir = new URL("../../fixtures/terminal-preservation/", import.meta.url);
const read = <T>(name: string): T => JSON.parse(readFileSync(new URL(name, dir), "utf8")) as T;
const request = read<TerminalPreservationRequest>("request.json");
const reasons = read<Record<string, { status_class: string }>>("reasons.json");
const completionReasons = read<string[]>("completion-reasons.json");
const ackFixture = JSON.parse(readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
const RUN = request.manifest.run_id;
const response = (state: string) => read<TerminalPreservationResponse>(`response.${state}.json`);

describe("vocabulary pinned to the shared fixtures", () => {
  it("TERMINAL_REASONS equals reasons.json keys", () => {
    assert.deepEqual([...TERMINAL_REASONS].sort(), Object.keys(reasons).sort());
  });
  it("contribution states have one response fixture each", () => {
    for (const state of TERMINAL_CONTRIBUTION_STATES) assert.equal(response(state).state, state);
  });
  it("abandoned classes and completion reasons", () => {
    assert.deepEqual(read<string[]>("abandoned.json"), ["ignored", "cold_dirty"]);
    assert.deepEqual(completionReasons, ["terminal_preservation_pending", "terminal_preservation_refused"]);
    for (const a of request.manifest.abandoned) assert.ok(["ignored", "cold_dirty"].includes(a.class));
  });
});

// Decode sites for the two new completed-publication reasons.
const run = "11111111-1111-4111-8111-111111111111";
const worker = "22222222-2222-4222-8222-222222222222";
const hold = "33333333-3333-4333-8333-333333333333";
const repo = "44444444-4444-4444-8444-444444444444";
const other = "55555555-5555-4555-8555-555555555555";
const ACK_FEATURES = ["recovery_completed_publication_v1", "claim_generation_fence", "recovery_inventory_v1"];
const row = () => ({ hold_id: hold, generation: 1, inventory_guarded: true, has_available_capture: false });
const exact = () => ({ run_id: run, hold_id: hold, generation: 1, repo_id: repo,
  original_worker_id: worker, original_worker_identity: "original", live_worker_id: worker,
  live_run_id: run, inventory_guarded: true, state: "open", run_status: "failed",
  current_claim_generation: 1, claim_released: false });

async function holdClient(listing: unknown, exactBody: unknown) {
  const transport: typeof fetch = async (input) => {
    const url = new URL(String(input));
    if (url.pathname.endsWith("/register")) return Response.json({ worker_id: worker, protocol_features: [] });
    return Response.json(url.search ? exactBody : listing);
  };
  const client = new WorkerClient("http://maintenance.test", "worker-token", "test", nullLogger(), { fetch: transport });
  await client.register("worker", undefined, 1, undefined, ["recovery_held_publication_v2"]);
  return client;
}

describe("terminal preservation completion reasons decode at every site", () => {
  for (const reason of completionReasons) {
    it(`state ACK keeps ${reason} and mints no receipt`, async () => {
      const m = mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
        if (new URL(String(input)).pathname.endsWith("/register")) return Response.json({ worker_id: ackFixture.ack.completed_publication_receipt.worker_id, protocol_features: ACK_FEATURES });
        return Response.json({ run: ackFixture.ack.run, completed_publication_reason: reason });
      });
      try {
        const client = new WorkerClient("http://publication.test", "t", "test", nullLogger(), { sleep: async () => {} });
        await client.register("worker");
        const ack = await client.reportState(ackFixture.ack.completed_publication_receipt.run_id, ackFixture.request);
        assert.equal(ack.completedPublicationReason, reason);
        assert.equal(ack.completedPublicationReceipt, undefined);
      } finally { m.mock.restore(); }
    });

    it(`maintenance hold row with ${reason} is valid`, async () => {
      const client = await holdClient(
        { run_id: run, holds: [row(), { ...row(), hold_id: other, generation: 2, completed_publication_reason: reason }] },
        { run_id: run, exact_hold: exact() });
      assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
    });

    it(`exact hold with ${reason} is not invalid_exact_hold`, async () => {
      const client = await holdClient({ run_id: run, holds: [row()] },
        { run_id: run, exact_hold: { ...exact(), completed_publication_reason: reason } });
      assert.equal((await client.observeTerminalMaintenanceHold(run, 1)).ok, true);
    });
  }

  it("an unknown junk reason still drops at the ACK and invalidates the hold sites", async () => {
    const m = mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
      if (new URL(String(input)).pathname.endsWith("/register")) return Response.json({ worker_id: worker, protocol_features: ACK_FEATURES });
      return Response.json({ run: ackFixture.ack.run, completed_publication_reason: "terminal_preservation_junk" });
    });
    try {
      const client = new WorkerClient("http://publication.test", "t", "test", nullLogger(), { sleep: async () => {} });
      await client.register("worker");
      assert.equal((await client.reportState(run, ackFixture.request)).completedPublicationReason, undefined);
    } finally { m.mock.restore(); }
    const rowClient = await holdClient(
      { run_id: run, holds: [row(), { ...row(), hold_id: other, generation: 2, completed_publication_reason: "terminal_preservation_junk" }] },
      { run_id: run, exact_hold: exact() });
    assert.equal((await rowClient.observeTerminalMaintenanceHold(run, 1)).ok, false);
    const exactClient = await holdClient({ run_id: run, holds: [row()] },
      { run_id: run, exact_hold: { ...exact(), completed_publication_reason: "terminal_preservation_junk" } });
    assert.deepEqual(await exactClient.observeTerminalMaintenanceHold(run, 1), { ok: false, reason: "invalid_exact_hold" });
  });
});

describe("publishTerminalPreservation", () => {
  let client: WorkerClient;
  let calls: Array<{ url: URL; init: RequestInit }>;
  let answer: (init: RequestInit) => Response | Promise<Response>;

  beforeEach(() => {
    calls = [];
    answer = () => Response.json(response("published"));
    mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      calls.push({ url: new URL(String(input)), init: init ?? {} });
      return answer(init ?? {});
    });
    client = new WorkerClient("http://tp.test", "worker-token", "test", nullLogger(), { sleep: async () => {} });
  });
  afterEach(() => mock.restoreAll());

  const publish = (signal?: AbortSignal) => client.publishTerminalPreservation(RUN, request, signal);

  it("posts the fixture request as JSON with auth to the run's route", async () => {
    await publish();
    assert.equal(calls.length, 1);
    const { url, init } = calls[0]!;
    assert.equal(url.pathname, `/api/worker/runs/${RUN}/terminal-preservation`);
    assert.equal(init.method, "POST");
    const headers = init.headers as Record<string, string>;
    assert.equal(headers["Content-Type"], "application/json");
    assert.equal(headers.Authorization, "Bearer worker-token");
    assert.deepEqual(JSON.parse(String(init.body)), request);
  });

  it("published at the requested tip is final", async () => {
    assert.deepEqual(await publish(), { kind: "final", outcome: "published", response: response("published") });
  });

  it("published at a different tip is unknown/malformed", async () => {
    answer = () => Response.json({ ...response("published"), tip: "9".repeat(40) });
    assert.deepEqual(await publish(), { kind: "unknown", cause: "malformed_response", httpStatus: 200 });
  });

  for (const name of ["recorded", "recorded-pr"]) {
    it(`${name} (server-shaped abandonment) is final`, async () => {
      const body = read<TerminalPreservationResponse>(`response.${name}.json`);
      answer = () => Response.json(body);
      assert.deepEqual(await publish(), { kind: "final", outcome: "recorded", response: body });
    });
  }
  it("recorded fixtures carry the stored abandonment dispositions", () => {
    assert.equal(response("recorded").disposition, "closed_issue_abandoned");
    assert.equal(response("recorded-pr").disposition, "closed_pr_abandoned");
    assert.equal(response("published").disposition, "preserved");
  });

  it("refused state with a known reason is refused", async () => {
    answer = () => Response.json(response("refused"));
    assert.deepEqual(await publish(), { kind: "refused", reason: "send_suppressed", httpStatus: 200 });
  });

  it("refused state without a reason is malformed", async () => {
    answer = () => Response.json({ ...response("refused"), reason: undefined });
    const r = await publish();
    assert.deepEqual(r, { kind: "unknown", cause: "malformed_response", httpStatus: 200 });
  });

  for (const state of ["invoked", "publish_unknown", "prepared"]) {
    it(`${state} is unknown, never final`, async () => {
      answer = () => Response.json(response(state));
      const r = await publish();
      assert.equal(r.kind, "unknown");
      if (r.kind === "unknown") assert.equal(r.cause, "not_final");
    });
  }

  for (const name of ["unknown-state", "unknown-reason", "bad-sha", "extra-field",
    "request-disposition", "recorded-preserved", "preserved-closed"]) {
    it(`invalid response ${name} is unknown/malformed`, async () => {
      answer = () => Response.json(read(`invalid/response.${name}.json`));
      assert.deepEqual(await publish(), { kind: "unknown", cause: "malformed_response", httpStatus: 200 });
    });
  }

  it("a wrong ref, missing field, non-object and oversize body are malformed", async () => {
    const base = response("published") as unknown as Record<string, unknown>;
    const { preservation_id: _omit, ...missing } = base;
    for (const body of [
      JSON.stringify({ ...base, ref: "refs/uzi-terminal/" + other }),
      JSON.stringify({ ...base, ref: "refs/heads/main" }),
      JSON.stringify(missing),
      JSON.stringify({ ...base, disposition: "discard" }),
      JSON.stringify({ ...base, disposition: "preserve" }),
      JSON.stringify({ ...base, disposition: "closed_pr_abandoned" }),
      JSON.stringify([]),
      "not json",
      JSON.stringify({ ...base, preservation_state: "x".repeat(20 * 1024) }),
    ]) {
      answer = () => new Response(body, { status: 200 });
      assert.deepEqual(await publish(), { kind: "unknown", cause: "malformed_response", httpStatus: 200 }, body.slice(0, 40));
    }
  });

  const RETRYABLE = ["publication_in_flight", "authority_changed", "forge_unavailable"];
  for (const [reason, { status_class }] of Object.entries(reasons)) {
    if (RETRYABLE.includes(reason)) {
      it(`HTTP ${status_class} ${reason} is unknown/retryable, never refused`, async () => {
        answer = () => Response.json({ error: "x", reason }, { status: Number(status_class), headers: { "Retry-After": "30" } });
        assert.deepEqual(await publish(), { kind: "unknown", cause: "retryable", reason, httpStatus: Number(status_class) });
      });
      continue;
    }
    it(`HTTP ${status_class} ${reason} is refused with that reason`, async () => {
      answer = () => Response.json({ error: "terminal preservation refused", reason }, { status: Number(status_class) });
      assert.deepEqual(await publish(), { kind: "refused", reason, httpStatus: Number(status_class) });
    });
  }

  it("wire-only reasons: busy is retryable, the rest refused", async () => {
    answer = () => Response.json({ error: "x", reason: "busy" }, { status: 429 });
    assert.deepEqual(await publish(), { kind: "unknown", cause: "retryable", reason: "busy", httpStatus: 429 });
    for (const [status, reason] of [[409, "stale_claim"], [409, "missing_claim_generation"], [400, "invalid_request"]] as const) {
      answer = () => Response.json({ error: "x", reason }, { status });
      assert.deepEqual(await publish(), { kind: "refused", reason, httpStatus: status });
    }
  });

  it("a status that does not match the reason's mapping is unknown/malformed", async () => {
    for (const [status, reason] of [[409, "pack_too_large"], [422, "foreign_tip"], [409, "forge_unavailable"],
      [503, "authority_changed"], [202, "unsupported"], [409, "busy"], [409, "invalid_request"]] as const) {
      answer = () => Response.json({ error: "x", reason }, { status });
      assert.deepEqual(await publish(), { kind: "unknown", cause: "malformed_response", httpStatus: status }, `${status} ${reason}`);
    }
  });

  it("a 200 reason is allowed only on refused, and only a stored refusal reason", async () => {
    const bad: Array<[string, unknown]> = [
      ["reason on published", { ...response("published"), reason: "send_suppressed" }],
      ["reason on recorded", { ...response("recorded"), reason: "send_suppressed" }],
      ["reason on invoked", { ...response("invoked"), reason: "forge_unavailable" }],
      ["refused with a wire-only reason", { ...response("refused"), reason: "forge_unavailable" }],
      ["refused with a non-stored vocabulary reason", { ...response("refused"), reason: "authority_changed" }],
    ];
    for (const [label, body] of bad) {
      answer = () => Response.json(body);
      assert.deepEqual(await publish(), { kind: "unknown", cause: "malformed_response", httpStatus: 200 }, label);
    }
    for (const reason of ["create_refused", "update_refused", "foreign_tip", "send_suppressed"]) {
      answer = () => Response.json({ ...response("refused"), reason });
      assert.deepEqual(await publish(), { kind: "refused", reason, httpStatus: 200 });
    }
  });

  it("an HTTP error without a known reason is unknown", async () => {
    for (const [status, body] of [[500, { error: "internal error" }], [404, { error: "run not found" }],
      [409, { error: "x", reason: "made_up" }], [401, { error: "no" }]] as const) {
      answer = () => Response.json(body, { status });
      assert.deepEqual(await publish(), { kind: "unknown", cause: "http_status", httpStatus: status });
    }
    answer = () => new Response("<html>bad gateway</html>", { status: 502 });
    assert.deepEqual(await publish(), { kind: "unknown", cause: "http_status", httpStatus: 502 });
  });

  it("a 200 sibling status other than 200 never yields final", async () => {
    answer = () => Response.json(response("published"), { status: 201 });
    assert.equal((await publish()).kind, "unknown");
  });

  it("a network error is unknown/transport", async () => {
    answer = () => { throw new TypeError("fetch failed"); };
    assert.deepEqual(await publish(), { kind: "unknown", cause: "transport" });
  });

  it("an abort is unknown/aborted", async () => {
    const abort = new AbortController();
    answer = (init) => new Promise<Response>((_, reject) => {
      init.signal?.addEventListener("abort", () => reject(init.signal!.reason), { once: true });
      abort.abort();
    });
    assert.deepEqual(await publish(abort.signal), { kind: "unknown", cause: "aborted" });
  });
});

describe("publishTerminalPreservation with an injected transport", () => {
  afterEach(() => mock.restoreAll());

  it("posts through the constructor fetch and never the global one", async () => {
    const globalCalls: string[] = [];
    mock.method(globalThis, "fetch", async (input: string | URL | Request) => {
      globalCalls.push(String(input));
      throw new Error("global fetch must not be used");
    });
    const injected: Array<{ url: URL; method?: string }> = [];
    const transport: typeof fetch = async (input, init) => {
      injected.push({ url: new URL(String(input)), method: init?.method });
      return Response.json(response("published"));
    };
    const client = new WorkerClient("http://tp.test", "worker-token", "test", nullLogger(), { fetch: transport, sleep: async () => {} });
    const result = await client.publishTerminalPreservation(RUN, request);
    assert.equal(result.kind, "final");
    assert.equal(injected.length, 1);
    assert.equal(injected[0]!.url.pathname, `/api/worker/runs/${RUN}/terminal-preservation`);
    assert.equal(injected[0]!.method, "POST");
    assert.deepEqual(globalCalls, []);
  });

  it("a non-string tip is malformed even when its string form is a valid sha", async () => {
    const base = response("recorded") as unknown as Record<string, unknown>;
    for (const tip of [[String(base.tip)], 12345, { toString: () => String(base.tip) }]) {
      const transport: typeof fetch = async () => Response.json({ ...base, tip });
      const client = new WorkerClient("http://tp.test", "worker-token", "test", nullLogger(), { fetch: transport, sleep: async () => {} });
      assert.deepEqual(await client.publishTerminalPreservation(RUN, request),
        { kind: "unknown", cause: "malformed_response", httpStatus: 200 }, JSON.stringify(tip));
    }
  });
});
