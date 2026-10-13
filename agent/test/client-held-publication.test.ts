import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { Readable } from "node:stream";
import { afterEach, beforeEach, describe, it, mock } from "node:test";
import {
  HELD_PUBLICATION_FEATURE,
  HELD_PUBLICATION_TIMEOUT_MS,
  HeldFeatureUnavailableError,
  WorkerClient,
} from "../src/client.js";
import {
  HELD_FINAL_FALLBACK_REASONS,
  HELD_FINAL_RETAINED_REASONS,
  type RecoveryReleaseRequest,
  type RecoveryReleaseResponse,
} from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

// Issue #2545: the agent side of the held-publication wire contract, pinned to the api's fixtures
// (fixtures/api-contract/recovery_held_final.*) so a drift on either side reddens a test.
const fixtureDir = new URL("../../fixtures/api-contract/", import.meta.url);
const read = <T>(name: string): T => JSON.parse(readFileSync(new URL(name, fixtureDir), "utf8")) as T;
const request = read<RecoveryReleaseRequest>("recovery_held_final.request.json");
const ack = read<RecoveryReleaseResponse>("recovery_held_final.ack.json");
const retained = read<RecoveryReleaseResponse>("recovery_held_final.retained.json");
const reasons = read<Record<string, { definitive: boolean }>>("recovery_held_final.reasons.json");

const RUN = ack.run_id;
const TIP = request.final_disposition!.source_sha!;
const COVERAGE = request.final_disposition!.coverage_digest;
const PUB = request.final_disposition!.publication_id!;

let client: WorkerClient;
let features: string[];
let calls: Array<{ path: string; init: RequestInit }>;
let answer: (path: string, init: RequestInit) => Response | Promise<Response>;

beforeEach(() => {
  features = ["recovery_inventory_v1", HELD_PUBLICATION_FEATURE];
  calls = [];
  answer = () => Response.json({}, { status: 200 });
  mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const path = new URL(String(input)).pathname;
    if (path.endsWith("/register")) return Response.json({ worker_id: "w1", protocol_features: features });
    calls.push({ path, init: init ?? {} });
    return answer(path, init ?? {});
  });
  client = new WorkerClient("http://held.test", "worker-token", "test", nullLogger(), { sleep: async () => {} });
});
afterEach(() => mock.restoreAll());

describe("held publication contract types pinned to the api fixtures", () => {
  it("the FINAL request fixture is a held_publication disposition", () => {
    assert.equal(request.final_disposition?.kind, "held_publication");
    assert.equal(request.generation, ack.generation);
    assert.match(PUB, /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);
    assert.match(TIP, /^[0-9a-f]{40}$/);
    assert.match(COVERAGE, /^[0-9a-f]{64}$/);
    assert.equal(request.final_disposition?.capture_id, undefined);
  });

  it("the ack and retained fixtures have the RecoveryReleaseResponse shape the worker validates", () => {
    assert.deepEqual(Object.keys(ack).sort(), ["generation", "holds_released", "released", "run_id"]);
    assert.equal(ack.released, true);
    assert.equal(ack.holds_released, 1);
    assert.equal(retained.released, false);
    assert.equal(retained.holds_released, 0);
    assert.equal(retained.retained, true);
    assert.ok(retained.reason && retained.reason in reasons, "the retained reason is a known token");
  });

  it("the worker's reason list is exactly the fixture's, and its fallback set covers every definitive reason", () => {
    assert.deepEqual([...HELD_FINAL_RETAINED_REASONS].sort(), Object.keys(reasons).sort());
    for (const [reason, { definitive }] of Object.entries(reasons)) {
      if (definitive) assert.ok(HELD_FINAL_FALLBACK_REASONS.has(reason), `${reason} is definitive and falls back to the archive`);
    }
    // The one deliberate widening: the api may revive "unsupported", the archive is always available.
    assert.deepEqual(
      [...HELD_FINAL_FALLBACK_REASONS].filter(r => !reasons[r]!.definitive),
      ["held_publication_unsupported"],
    );
    assert.equal(HELD_FINAL_FALLBACK_REASONS.has("held_publication_unavailable"), false,
      "unavailable is the one transient reason: the held FINAL is kept");
  });
});

describe("releaseRecoveryCustody carries the held disposition", () => {
  it("posts the fixture's final_disposition verbatim for the exact generation", async () => {
    await client.register("w");
    answer = () => Response.json(ack);
    const res = await client.releaseRecoveryCustody(RUN, request.generation, undefined, request.final_disposition);
    assert.deepEqual(res, ack);
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.path, `/api/worker/runs/${RUN}/archives/release`);
    assert.deepEqual(JSON.parse(String(calls[0]!.init.body)), request);
  });

  it("returns a retained answer unchanged for the caller to classify", async () => {
    await client.register("w");
    answer = () => Response.json(retained);
    const res = await client.releaseRecoveryCustody(RUN, request.generation, undefined, request.final_disposition);
    assert.deepEqual(res, retained);
  });

  it("without the negotiated feature nothing is sent and the typed error is non-HTTP", async () => {
    features = ["recovery_inventory_v1"];
    await client.register("w");
    await assert.rejects(
      client.releaseRecoveryCustody(RUN, request.generation, undefined, request.final_disposition),
      HeldFeatureUnavailableError,
    );
    assert.deepEqual(calls, [], "no release request left the worker");
  });

  it("an archive FINAL is unaffected by the held feature being absent", async () => {
    features = ["recovery_inventory_v1"];
    await client.register("w");
    answer = () => Response.json(ack);
    await client.releaseRecoveryCustody(RUN, 3, undefined,
      { kind: "archive", capture_id: "c1", source_sha: TIP, coverage_digest: COVERAGE });
    assert.equal(calls.length, 1);
  });
});

describe("publishHeld", () => {
  it("uploads an octet-stream pack with the three held headers and a half-duplex body", async () => {
    await client.register("w");
    const pack = Buffer.from("PACK-bytes");
    answer = () => Response.json({ publication_id: PUB, ref: `refs/uzi-held/${RUN}/3`, tip: TIP, state: "created" });
    const res = await client.publishHeld(RUN, 3, TIP, COVERAGE, pack);
    assert.deepEqual(res, { ok: true, body: { publication_id: PUB, ref: `refs/uzi-held/${RUN}/3`, tip: TIP, state: "created" } });
    const { path, init } = calls[0]!;
    assert.equal(path, `/api/worker/runs/${RUN}/held-publication`);
    assert.equal(init.method, "POST");
    const headers = init.headers as Record<string, string>;
    assert.equal(headers["Content-Type"], "application/octet-stream");
    assert.equal(headers["X-Uzi-Held-Tip"], TIP);
    assert.equal(headers["X-Uzi-Held-Generation"], "3");
    assert.equal(headers["X-Uzi-Held-Coverage"], COVERAGE);
    assert.equal(headers["Authorization"], "Bearer worker-token");
    assert.equal(init.body, pack);
    assert.equal((init as { duplex?: string }).duplex, "half");
  });

  it("accepts a streamed pack", async () => {
    await client.register("w");
    answer = () => Response.json({ publication_id: PUB, ref: "r", tip: TIP, state: "created" });
    const stream = Readable.from([Buffer.from("a"), Buffer.from("b")]);
    await client.publishHeld(RUN, 3, TIP, COVERAGE, stream);
    assert.equal(calls[0]!.init.body, stream);
  });

  it("uses a 330 s timeout on this route only", async () => {
    await client.register("w");
    assert.equal(HELD_PUBLICATION_TIMEOUT_MS, 330_000);
    const timeouts: number[] = [];
    const real = AbortSignal.timeout.bind(AbortSignal);
    mock.method(AbortSignal, "timeout", (ms: number) => { timeouts.push(ms); return real(ms); });
    answer = () => Response.json({ publication_id: PUB, ref: "r", tip: TIP, state: "created" });
    await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x"));
    assert.deepEqual(timeouts, [330_000]);
    timeouts.length = 0;
    answer = () => Response.json({ published: true });
    await client.publishCheckpoint(RUN, TIP, Readable.from([Buffer.from("x")]));
    await client.releaseRecoveryCustody(RUN, 3, undefined, request.final_disposition);
    assert.ok(timeouts.length >= 2);
    assert.ok(timeouts.every(ms => ms !== 330_000), `no other route uses the held timeout: ${timeouts.join(",")}`);
  });

  it("returns the api's bounded refusal reason for each non-2xx", async () => {
    await client.register("w");
    const cases: Array<[number, Record<string, string>, string]> = [
      [409, { error: "held publication refused", reason: "create_refused" }, "create_refused"],
      [422, { error: "x", reason: "held_publication_unsupported" }, "held_publication_unsupported"],
      [413, { error: "x", reason: "pack_too_large" }, "pack_too_large"],
      [503, { error: "x", reason: "forge_unavailable" }, "forge_unavailable"],
      [429, { error: "x", reason: "busy" }, "busy"],
      [408, { error: "held pack upload timed out" }, "http_408"],
      [500, { error: "internal error" }, "http_500"],
    ];
    for (const [status, body, reason] of cases) {
      answer = () => Response.json(body, { status });
      assert.deepEqual(await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x")),
        { ok: false, httpStatus: status, reason }, `status ${status}`);
    }
  });

  it("never reports a non-created state as created, and rejects a malformed 2xx", async () => {
    await client.register("w");
    answer = () => Response.json({ publication_id: PUB, ref: "r", tip: TIP, state: "create_unknown", reason: "forge_unavailable" });
    const unknown = await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x"));
    assert.ok(unknown.ok && unknown.body.state === "create_unknown", "the caller sees the state, and only `created` continues");
    answer = () => Response.json({ state: "created" });
    assert.deepEqual(await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x")),
      { ok: false, httpStatus: 200, reason: "malformed_response" });
    answer = () => new Response("not json", { status: 200 });
    assert.deepEqual(await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x")),
      { ok: false, httpStatus: 200, reason: "malformed_response" });
  });

  it("sends nothing without the negotiated feature", async () => {
    features = ["recovery_inventory_v1"];
    await client.register("w");
    assert.deepEqual(await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x")),
      { ok: false, reason: "unsupported_feature" });
    assert.deepEqual(calls, []);
  });

  it("a transport failure throws so the caller treats it as a refusal", async () => {
    await client.register("w");
    answer = () => { throw new TypeError("fetch failed"); };
    await assert.rejects(client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x")), /fetch failed/);
  });

  it("never carries a forge credential: only the worker bearer is sent", async () => {
    await client.register("w");
    answer = () => Response.json({ publication_id: PUB, ref: "r", tip: TIP, state: "created" });
    await client.publishHeld(RUN, 3, TIP, COVERAGE, Buffer.from("x"));
    const headers = calls[0]!.init.headers as Record<string, string>;
    assert.deepEqual(Object.keys(headers).sort(), [
      "Authorization", "Content-Type", "X-Client-Version", "X-Uzi-Held-Coverage", "X-Uzi-Held-Generation", "X-Uzi-Held-Tip",
    ]);
  });
});
