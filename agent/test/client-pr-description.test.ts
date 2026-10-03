import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import {
  PrDescriptionConflict,
  PrDescriptionInvalid,
  PrDescriptionMalformedResponse,
  PrDescriptionNotFound,
  PrDescriptionRateLimited,
  RequestError,
  SanitizedPrDescriptionFields,
  WorkerClient,
  isTransient,
  type PrDescriptionConflictReason,
} from "../src/client.js";
import type { Logger } from "../src/log.js";
import {
  PR_DESC_ACK_OUTCOMES,
  type PrDescriptionAckRequest,
  type PrDescriptionBindRequest,
  type PrDescriptionLookupRequest,
  type PrDescriptionStageRequest,
  type PrDescriptionState,
  type PrDescriptionVersionDTO,
  type RawPrDescriptionFields,
} from "../src/protocol.js";

// PRD #1798 M4 (D9), worker transport half: the four pr-description routes pinned against the api
// contract on main (api/internal/handler/handler.go route mounts, handler/worker_pr_description.go
// status map, apitypes/pr_description.go wire shapes). A recording HTTP server asserts the exact
// method, path and body, and replays the handler's error bodies to pin the typed-error mapping.

const TOKEN = "worker-join-token-0123456789";
const RUN_ID = "11111111-2222-3333-4444-555555555555";
const VERSION_ID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee";
const SHA_A = "a".repeat(40);
const SHA_B = "b".repeat(40);
const REGION = "c".repeat(64);

interface Recorded {
  method: string;
  url: string;
  headers: http.IncomingHttpHeaders;
  body: string;
}

interface Reply {
  status: number;
  body: string;
  headers?: Record<string, string>;
}

let server: http.Server;
let baseUrl: string;
let recorded: Recorded[];
let respond: (req: Recorded) => Reply;

beforeEach(async () => {
  recorded = [];
  respond = () => ({ status: 200, body: "{}" });
  server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c) => chunks.push(c as Buffer));
    req.on("end", () => {
      const rec: Recorded = {
        method: req.method ?? "",
        url: req.url ?? "",
        headers: req.headers,
        body: Buffer.concat(chunks).toString("utf8"),
      };
      recorded.push(rec);
      const { status, body, headers } = respond(rec);
      res.writeHead(status, { "Content-Type": "application/json", ...headers });
      res.end(body);
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  baseUrl = `http://127.0.0.1:${(server.address() as AddressInfo).port}`;
});

afterEach(async () => {
  await new Promise<void>((resolve) => server.close(() => resolve()));
});

function newClient(log: Logger = nullLogger()): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", log, { sleep: async () => {} });
}

const zero = () => ({ added: 0, deleted: 0 });

// Wire fixtures: what the api SENDS, before decoding. Their fields are plain objects (raw at the
// type level); only the client's decoders turn them into SanitizedPrDescriptionFields instances.
// assert/strict's deepEqual compares prototypes, so a decoded value is compared through its JSON
// form (which is the plain wire shape: an instance serializes to its fields).
const plain = (v: unknown): unknown => JSON.parse(JSON.stringify(v));
type WireVersion = Omit<PrDescriptionVersionDTO, "fields"> & { fields: RawPrDescriptionFields };
type WireState = Omit<PrDescriptionState, "published_version"> & { published_version: WireVersion | null };

function version(over: Partial<WireVersion> = {}): WireVersion {
  return {
    id: VERSION_ID,
    run_id: RUN_ID,
    claim_generation: 3,
    mr_iid: null,
    fields: {
      summary: "Adds a plain-English description.",
      changes: ["The PR body carries a summary."],
      scope_notes: [{ kind: "deferred", text: "Docs follow later." }],
      review_pointers: ["agent/src/client.ts"],
      verification: [{ command: "npm test", result: "pass", verified_at_sha: "abc1234" }],
    },
    size: {
      unavailable: false,
      files: 2,
      code: { added: 10, deleted: 1 },
      tests: { added: 5, deleted: 0 },
      docs: zero(),
      config: zero(),
      generated: zero(),
      vendored: zero(),
    },
    base_sha: SHA_A,
    head_sha: SHA_B,
    target_branch: "main",
    source: "generated",
    rendered_region_sha256: null,
    region_has_diagram: null,
    state: "pending",
    created_at: "2026-09-27T10:00:00Z",
    published_at: null,
    ...over,
  };
}

function prState(over: Partial<WireState> = {}): WireState {
  return { mr_iid: 42, lock_version: 7, last_outcome: null, published_version: null, ...over };
}

const stageReq = (): PrDescriptionStageRequest => ({
  claim_generation: 3,
  source: "generated",
  fields: version().fields,
  size: version().size,
  base_sha: SHA_A,
  head_sha: SHA_B,
  target_branch: "main",
});
const bindReq = (): PrDescriptionBindRequest => ({
  claim_generation: 3,
  version_id: VERSION_ID,
  mr_iid: 42,
  rendered_region_sha256: REGION,
});
const lookupReq = (): PrDescriptionLookupRequest => ({ claim_generation: 3, mr_iid: 42, region_sha256: REGION });
const ackReq = (): PrDescriptionAckRequest => ({
  claim_generation: 3,
  version_id: VERSION_ID,
  outcome: "published",
  expected_lock_version: 7,
});

type Call = (c: WorkerClient) => Promise<unknown>;
const CALLS: { op: string; call: Call; ok: () => unknown }[] = [
  { op: "stage", call: (c) => c.stagePrDescription(RUN_ID, stageReq()), ok: () => ({ version: version() }) },
  {
    op: "bind",
    call: (c) => c.bindPrDescription(RUN_ID, bindReq()),
    ok: () => ({ version: version({ mr_iid: 42, rendered_region_sha256: REGION }), pr: prState() }),
  },
  {
    op: "lookup",
    call: (c) => c.lookupPrDescription(RUN_ID, lookupReq()),
    ok: () => ({ match: "none", matched_version_id: null, pr: null }),
  },
  {
    op: "ack",
    call: (c) => c.ackPrDescription(RUN_ID, ackReq()),
    ok: () => ({ pr: prState({ lock_version: 8, last_outcome: "published" }), recovered_version_id: null }),
  },
];

describe("pr-description client wire (PRD #1798 M4, D9)", () => {
  it("each call POSTs the exact route with a JSON body carrying claim_generation", async () => {
    const bodies: Record<string, unknown> = {
      stage: stageReq(),
      bind: bindReq(),
      lookup: lookupReq(),
      ack: ackReq(),
    };
    for (const { op, call, ok } of CALLS) {
      recorded = [];
      respond = () => ({ status: 200, body: JSON.stringify(ok()) });
      await call(newClient());
      assert.equal(recorded.length, 1, op);
      const r = recorded[0]!;
      assert.equal(r.method, "POST", op);
      assert.equal(r.url, `/api/worker/runs/${RUN_ID}/pr-description/${op}`, op);
      assert.equal(r.headers.authorization, `Bearer ${TOKEN}`, op);
      assert.equal(r.headers["content-type"], "application/json", op);
      const sent = JSON.parse(r.body) as Record<string, unknown>;
      assert.equal(sent.claim_generation, 3, op);
      assert.deepEqual(sent, bodies[op], op);
    }
  });

  it("stage sends mr_iid only for a refresh, and ack sends observed_region_sha256 only when set", async () => {
    respond = (r) =>
      r.url.endsWith("/stage")
        ? { status: 200, body: JSON.stringify({ version: version({ mr_iid: 42 }) }) }
        : { status: 200, body: JSON.stringify({ pr: prState(), recovered_version_id: VERSION_ID }) };
    const c = newClient();
    await c.stagePrDescription(RUN_ID, { ...stageReq(), mr_iid: 42, source: "deterministic_only", size: null });
    await c.ackPrDescription(RUN_ID, { ...ackReq(), outcome: "skipped_human_edit", observed_region_sha256: REGION });
    const stage = JSON.parse(recorded[0]!.body) as Record<string, unknown>;
    assert.equal(stage.mr_iid, 42);
    assert.equal(stage.size, null);
    assert.equal(stage.source, "deterministic_only");
    const ack = JSON.parse(recorded[1]!.body) as Record<string, unknown>;
    assert.equal(ack.outcome, "skipped_human_edit");
    assert.equal(ack.observed_region_sha256, REGION);
  });

  it("the run id is path-escaped", async () => {
    respond = () => ({ status: 200, body: JSON.stringify({ match: "none", matched_version_id: null, pr: null }) });
    await newClient().lookupPrDescription("a/b c", lookupReq());
    assert.equal(recorded[0]!.url, "/api/worker/runs/a%2Fb%20c/pr-description/lookup");
  });

  it("decodes each response shape", async () => {
    const published = version({ state: "published", mr_iid: 42, rendered_region_sha256: REGION, published_at: "2026-09-27T10:05:00Z" });
    respond = () => ({ status: 200, body: JSON.stringify({ version: version() }) });
    const staged = await newClient().stagePrDescription(RUN_ID, stageReq());
    assert.deepEqual(plain(staged), { version: version() });
    assert.ok(staged.version.fields instanceof SanitizedPrDescriptionFields);
    assert.ok(SanitizedPrDescriptionFields.is(staged.version.fields));

    const bound = { version: version({ mr_iid: 42, rendered_region_sha256: REGION }), pr: prState({ published_version: published, last_outcome: "published" }) };
    respond = () => ({ status: 200, body: JSON.stringify(bound) });
    const gotBound = await newClient().bindPrDescription(RUN_ID, bindReq());
    assert.deepEqual(plain(gotBound), bound);
    assert.ok(SanitizedPrDescriptionFields.is(gotBound.version.fields));
    assert.ok(SanitizedPrDescriptionFields.is(gotBound.pr.published_version?.fields));

    for (const lookup of [
      { match: "published", matched_version_id: VERSION_ID, pr: prState({ published_version: published }) },
      { match: "pending", matched_version_id: VERSION_ID, pr: prState() },
      { match: "none", matched_version_id: null, pr: null },
    ]) {
      respond = () => ({ status: 200, body: JSON.stringify(lookup) });
      const got = await newClient().lookupPrDescription(RUN_ID, lookupReq());
      assert.deepEqual(plain(got), lookup);
      if (got.pr?.published_version) assert.ok(SanitizedPrDescriptionFields.is(got.pr.published_version.fields));
    }

    for (const outcome of PR_DESC_ACK_OUTCOMES) {
      const ack = { pr: prState({ lock_version: 8, last_outcome: outcome }), recovered_version_id: outcome === "published" ? VERSION_ID : null };
      respond = () => ({ status: 200, body: JSON.stringify(ack) });
      assert.deepEqual(plain(await newClient().ackPrDescription(RUN_ID, { ...ackReq(), outcome })), ack);
    }
  });

  it("preserves a sanitized diagram, freezes every nested value, and returns a deep mutable copy", async () => {
    const diagram = {
      kind: "flow" as const,
      title: "Overview",
      nodes: [{ key: "start", label: "Start" }, { key: "work", label: "Do work" }, { key: "end", label: "Finish" }],
      edges: [{ from: "start", to: "work", label: "Next" }, { from: "work", to: "end", label: "" }],
    };
    respond = () => ({ status: 200, body: JSON.stringify({ version: version({ fields: { ...version().fields, diagram }, region_has_diagram: true }) }) });
    const { version: decoded } = await newClient().stagePrDescription(RUN_ID, stageReq());
    const d = decoded.fields.diagram;
    assert.ok(d);
    assert.deepEqual(plain(d), diagram);
    assert.equal(decoded.region_has_diagram, true);
    for (const value of [decoded.fields, d, d.nodes, d.nodes[0], d.edges, d.edges[0]]) assert.ok(Object.isFrozen(value));
    const raw = decoded.fields.toRaw();
    assert.ok(raw.diagram);
    raw.diagram.nodes[0]!.label = "Changed";
    raw.diagram.edges[0]!.label = "Changed";
    raw.diagram.nodes.push({ key: "extra", label: "Extra" });
    assert.deepEqual(plain(d), diagram);
  });

  it("accepts a two-participant sequence diagram", async () => {
    const diagram = { kind: "sequence" as const, title: "", nodes: [{ key: "worker", label: "Worker" }, { key: "api", label: "API" }], edges: [{ from: "worker", to: "api", label: "Stage" }, { from: "api", to: "worker", label: "Bound" }] };
    respond = () => ({ status: 200, body: JSON.stringify({ version: version({ fields: { ...version().fields, diagram } }) }) });
    const { version: decoded } = await newClient().stagePrDescription(RUN_ID, stageReq());
    assert.deepEqual(plain(decoded.fields.diagram), diagram);
  });

  it("accepts a legacy response without diagram and sends the optional bind flag", async () => {
    respond = () => ({ status: 200, body: JSON.stringify({ version: version() }) });
    const staged = await newClient().stagePrDescription(RUN_ID, stageReq());
    assert.equal(staged.version.fields.diagram, undefined);
    assert.equal("diagram" in staged.version.fields.toRaw(), false);
    respond = () => ({ status: 200, body: JSON.stringify({ version: version(), pr: prState() }) });
    await newClient().bindPrDescription(RUN_ID, { ...bindReq(), region_has_diagram: false });
    assert.equal((JSON.parse(recorded.at(-1)!.body) as PrDescriptionBindRequest).region_has_diagram, false);
  });

  it("rejects malformed diagram data in a successful response", async () => {
    const good = { kind: "flow", title: "Overview", nodes: [{ key: "a", label: "A" }, { key: "b", label: "B" }, { key: "c", label: "C" }], edges: [{ from: "a", to: "b", label: "" }, { from: "b", to: "c", label: "" }] };
    const bad = [
      null, { ...good, kind: "unknown" }, { ...good, nodes: good.nodes.slice(0, 2) },
      { ...good, nodes: [...good.nodes, { key: "a", label: "Duplicate" }] },
      { ...good, nodes: [{ key: "Bad-Key", label: "A" }, ...good.nodes.slice(1)] },
      { ...good, nodes: [{ key: "a", label: "@person" }, ...good.nodes.slice(1)] },
      { ...good, nodes: [{ key: "a", label: "x".repeat(61) }, ...good.nodes.slice(1)] },
      { ...good, title: "x".repeat(81) }, { ...good, edges: good.edges.slice(0, 1) },
      { ...good, edges: [{ from: "a", to: "missing", label: "" }, good.edges[1]] },
      { ...good, edges: [{ from: "a", to: "a", label: "" }, good.edges[1]] },
      { ...good, edges: [{ from: "a", to: "b", label: "@person" }, good.edges[1]] },
      { ...good, nodes: [{ key: "a", label: "é".repeat(31) }, ...good.nodes.slice(1)] },
      { ...good, title: 42 }, { ...good, edges: [{ from: "a", to: "b", label: 1 }, good.edges[1]] },
    ];
    for (const diagram of bad) {
      respond = () => ({ status: 200, body: JSON.stringify({ version: version({ fields: { ...version().fields, diagram: diagram as never } }) }) });
      await assert.rejects(newClient().stagePrDescription(RUN_ID, stageReq()), PrDescriptionMalformedResponse);
    }
  });

  it("an unknown last_outcome string is kept (forward skew), not refused", async () => {
    const ack = { pr: prState({ lock_version: 8, last_outcome: "skipped_future_reason" }), recovered_version_id: null };
    respond = () => ({ status: 200, body: JSON.stringify(ack) });
    const got = await newClient().ackPrDescription(RUN_ID, ackReq());
    assert.equal(got.pr.last_outcome, "skipped_future_reason");
    assert.deepEqual(plain(got), ack);
  });

  it("decoded fields are frozen instances built from the checked keys only", async () => {
    const wire = version();
    const extra = { ...wire, fields: { ...wire.fields, injected: "x", scope_notes: [{ kind: "added", text: "t", extra: 1 }] } };
    respond = () => ({ status: 200, body: JSON.stringify({ version: extra }) });
    const { version: v } = await newClient().stagePrDescription(RUN_ID, stageReq());
    assert.ok(Object.isFrozen(v.fields));
    assert.ok(Object.isFrozen(v.fields.changes));
    assert.ok(Object.isFrozen(v.fields.scope_notes));
    assert.ok(Object.isFrozen(v.fields.scope_notes[0]));
    assert.ok(Object.isFrozen(v.fields.verification[0]));
    assert.equal("injected" in v.fields, false);
    assert.deepEqual(plain(v.fields.scope_notes), [{ kind: "added", text: "t" }]);
    // toRaw is a mutable plain copy, not an instance, and serializes identically.
    const raw = v.fields.toRaw();
    assert.equal(SanitizedPrDescriptionFields.is(raw), false);
    assert.equal(Object.getPrototypeOf(raw), Object.prototype);
    raw.changes.push("mutated");
    assert.equal(v.fields.changes.length, 1);
    assert.equal(JSON.stringify(v.fields), JSON.stringify(v.fields.toRaw()));
  });

  it("a decoded size is rebuilt from its known keys and frozen: unknown size and bucket keys are dropped", async () => {
    const wire = version();
    const extra = {
      ...wire,
      size: { ...wire.size, injected: "x", code: { ...wire.size!.code, extra: 1 } },
    };
    respond = () => ({ status: 200, body: JSON.stringify({ version: extra }) });
    const { version: v } = await newClient().stagePrDescription(RUN_ID, stageReq());
    assert.ok(v.size !== null);
    assert.equal("injected" in v.size, false);
    assert.equal("extra" in v.size.code, false);
    assert.deepEqual(plain(v.size), wire.size);
    assert.ok(Object.isFrozen(v.size));
    for (const b of ["code", "tests", "docs", "config", "generated", "vendored"] as const) {
      assert.ok(Object.isFrozen(v.size[b]), b);
    }
  });

  it("the ack outcome tuple is exactly the api's six outcomes (no abandoned)", () => {
    assert.deepEqual(
      [...PR_DESC_ACK_OUTCOMES],
      ["published", "skipped_human_edit", "skipped_no_region", "skipped_malformed", "skipped_snapshot_moved", "write_failed"],
    );
  });

  it("a malformed 200 body is refused, never cast", async () => {
    const bad: Record<string, unknown[]> = {
      stage: [
        "",
        "not json",
        {},
        { version: { ...version(), fields: { ...version().fields, changes: null } } },
        { version: { ...version(), source: "model" } },
        { version: { ...version(), region_has_diagram: "yes" } },
        { version: { ...version(), fields: { ...version().fields, scope_notes: [{ kind: "removed", text: "x" }] } } },
      ],
      bind: [{ version: version() }, { version: version(), pr: { ...prState(), lock_version: "7" } }],
      lookup: [{ match: "maybe", matched_version_id: null, pr: null }, { match: "none", matched_version_id: 1, pr: null }],
      ack: [
        { pr: { ...prState(), last_outcome: 5 }, recovered_version_id: null },
        { pr: prState({ published_version: version({ state: "retracted" as never }) }), recovered_version_id: null },
        { recovered_version_id: null },
      ],
    };
    for (const { op, call } of CALLS) {
      for (const body of bad[op]!) {
        respond = () => ({ status: 200, body: typeof body === "string" ? body : JSON.stringify(body) });
        await assert.rejects(call(newClient()), (err: unknown) => {
          assert.ok(err instanceof PrDescriptionMalformedResponse, `${op}: ${String(err)}`);
          assert.ok(!(err instanceof RequestError), `${op}: ${String(err)}`);
          assert.equal(err.op, op);
          assert.match(String(err), new RegExp(`malformed pr-description ${op} response`));
          // Non-transient: a retry wrapper must not re-send (a re-stage would create a 2nd version).
          assert.equal(isTransient(err), false, op);
          return true;
        });
      }
    }
  });
});

describe("pr-description typed errors (writePrDescriptionError's status map)", () => {
  const REASONS: readonly PrDescriptionConflictReason[] = [
    "stale_claim",
    "lock_conflict",
    "version_conflict",
    "too_many_versions",
    "run_terminal",
    "repo_required",
  ];

  it("409 with each handler reason maps to PrDescriptionConflict exposing that reason", async () => {
    for (const { op, call } of CALLS) {
      for (const reason of REASONS) {
        respond = () => ({ status: 409, body: JSON.stringify({ error: "conflict", reason }) });
        await assert.rejects(call(newClient()), (err: unknown) => {
          assert.ok(err instanceof PrDescriptionConflict, `${op}/${reason}`);
          assert.ok(err instanceof RequestError);
          assert.equal(err.reason, reason);
          assert.equal(err.status, 409);
          assert.equal(err.name, "PrDescriptionConflict");
          assert.equal(isTransient(err), false);
          return true;
        });
      }
    }
  });

  it("409 without a known reason stays a plain RequestError", async () => {
    for (const body of [JSON.stringify({ error: "x" }), JSON.stringify({ error: "x", reason: "vault_locked" }), "plain text", "[]"]) {
      respond = () => ({ status: 409, body });
      await assert.rejects(newClient().bindPrDescription(RUN_ID, bindReq()), (err: unknown) => {
        assert.ok(err instanceof RequestError);
        assert.ok(!(err instanceof PrDescriptionConflict));
        assert.equal(err.status, 409);
        return true;
      });
    }
  });

  it("429 maps to PrDescriptionRateLimited with the Retry-After, still transient", async () => {
    respond = () => ({ status: 429, body: JSON.stringify({ error: "too many requests" }), headers: { "Retry-After": "60" } });
    await assert.rejects(newClient().stagePrDescription(RUN_ID, stageReq()), (err: unknown) => {
      assert.ok(err instanceof PrDescriptionRateLimited);
      assert.equal(err.status, 429);
      assert.equal(err.retryAfterMs, 60_000);
      assert.equal(isTransient(err), true);
      return true;
    });
    // A huge Retry-After is clamped to 60s, never parked on for hours (or Infinity).
    respond = () => ({ status: 429, body: "{}", headers: { "Retry-After": "99999999999999999999" } });
    await assert.rejects(newClient().bindPrDescription(RUN_ID, bindReq()), (err: unknown) => {
      assert.ok(err instanceof PrDescriptionRateLimited);
      assert.equal(err.retryAfterMs, 60_000);
      return true;
    });
    respond = () => ({ status: 429, body: "{}", headers: { "Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT" } });
    await assert.rejects(newClient().stagePrDescription(RUN_ID, stageReq()), (err: unknown) => {
      assert.ok(err instanceof PrDescriptionRateLimited);
      assert.equal(err.retryAfterMs, undefined);
      return true;
    });
  });

  it("the stage is attempted once: a 429 or 5xx is not retried by the client", async () => {
    respond = () => ({ status: 503, body: "{}" });
    await assert.rejects(newClient().stagePrDescription(RUN_ID, stageReq()), (err: unknown) => {
      assert.ok(err instanceof RequestError);
      assert.equal(err.constructor, RequestError);
      assert.equal(err.status, 503);
      return true;
    });
    assert.equal(recorded.length, 1);
  });

  it("400 maps to PrDescriptionInvalid and 404 to PrDescriptionNotFound on every route", async () => {
    for (const { op, call } of CALLS) {
      respond = () => ({ status: 400, body: JSON.stringify({ error: "pr description request is invalid" }) });
      await assert.rejects(call(newClient()), (err: unknown) => {
        assert.ok(err instanceof PrDescriptionInvalid, op);
        assert.equal(err.status, 400);
        assert.match(err.body, /invalid/);
        return true;
      });
      respond = () => ({ status: 404, body: JSON.stringify({ error: "run not found for this worker" }) });
      await assert.rejects(call(newClient()), (err: unknown) => {
        assert.ok(err instanceof PrDescriptionNotFound, op);
        assert.equal(err.status, 404);
        return true;
      });
    }
  });
});

describe("claim pr_description (workersvc.ClaimPayload.PrDescription, omitempty)", () => {
  const claimBody = (extra: Record<string, unknown>) =>
    JSON.stringify({
      run_id: RUN_ID,
      issue_iid: 1,
      issue_title: "t",
      issue_description: "d",
      repo: {},
      secrets: {},
      last_seq: 0,
      agents: [],
      ...extra,
    });

  const recordingLog = () => {
    const warns: { msg: string; fields: unknown }[] = [];
    const log: Logger = { ...nullLogger(), warn: (msg: string, fields?: unknown) => void warns.push({ msg, fields }) };
    return { warns, log };
  };

  it("absent on a claim without a PR record, with no warning", async () => {
    respond = () => ({ status: 200, body: claimBody({}) });
    const { warns, log } = recordingLog();
    const claim = await newClient(log).claimRun();
    assert.ok(claim);
    assert.equal(claim.pr_description, undefined);
    assert.equal("pr_description" in claim, false);
    assert.deepEqual(warns, []);
  });

  it("a null 200 claim body is tolerated (returned as before), not a TypeError", async () => {
    respond = () => ({ status: 200, body: "null" });
    const { warns, log } = recordingLog();
    assert.equal(await newClient(log).claimRun(), null);
    assert.deepEqual(warns, []);
  });

  it("carries the PR state, including the published version, when present", async () => {
    const state = prState({
      last_outcome: "published",
      published_version: version({ state: "published", mr_iid: 42, rendered_region_sha256: REGION, published_at: "2026-09-27T10:05:00Z" }),
    });
    respond = () => ({ status: 200, body: claimBody({ pr_description: state }) });
    const { warns, log } = recordingLog();
    const claim = await newClient(log).claimRun();
    assert.deepEqual(plain(claim?.pr_description), state);
    assert.equal(claim?.pr_description?.published_version?.rendered_region_sha256, REGION);
    // The decoded claim's published fields are real sanitized instances (and typed so).
    const fields: SanitizedPrDescriptionFields | undefined = claim?.pr_description?.published_version?.fields;
    assert.ok(fields instanceof SanitizedPrDescriptionFields);
    assert.ok(SanitizedPrDescriptionFields.is(fields));
    assert.ok(Object.isFrozen(fields));
    assert.equal(fields.summary, "Adds a plain-English description.");
    assert.deepEqual(warns, []);
  });

  it("a malformed pr_description is dropped from the claim, and the warning carries no value", async () => {
    const malformed: unknown[] = [
      { ...prState(), lock_version: "7" },
      prState({ published_version: { ...version(), fields: { ...version().fields, summary: 1 as never } } }),
      prState({ published_version: version({ source: "model" as never }) }),
      { ...prState(), mr_iid: 1.5 },
      { ...prState(), last_outcome: false },
      "a string",
      null,
      1,
      [],
      {},
    ];
    for (const bad of malformed) {
      const { warns, log } = recordingLog();
      respond = () => ({ status: 200, body: claimBody({ pr_description: bad }) });
      const claim = await newClient(log).claimRun();
      assert.ok(claim, JSON.stringify(bad));
      assert.equal("pr_description" in claim, false, JSON.stringify(bad));
      assert.equal(claim.run_id, RUN_ID);
      assert.equal(warns.length, 1);
      assert.deepEqual(warns[0]!.fields, { run_id: RUN_ID });
    }
  });

  it("an unknown last_outcome on the claim's record is kept (forward skew)", async () => {
    respond = () => ({ status: 200, body: claimBody({ pr_description: prState({ last_outcome: "a_newer_outcome" }) }) });
    const { warns, log } = recordingLog();
    const claim = await newClient(log).claimRun();
    assert.equal(claim?.pr_description?.last_outcome, "a_newer_outcome");
    assert.deepEqual(warns, []);
  });
});

describe("raw vs sanitized fields are distinct types (D7, compile-checked)", () => {
  it("only a decoded instance fits a sanitized slot; spreads, raw fields and outside construction do not", async () => {
    respond = () => ({ status: 200, body: JSON.stringify({ version: version() }) });
    const staged = await newClient().stagePrDescription(RUN_ID, stageReq());
    const sanitized: SanitizedPrDescriptionFields = staged.version.fields;
    const raw: RawPrDescriptionFields = { ...version().fields, summary: "RAW model text" };
    let slot: SanitizedPrDescriptionFields = sanitized;

    // @ts-expect-error raw (unsanitized) fields must not type as SanitizedPrDescriptionFields.
    slot = raw;
    // @ts-expect-error a spread copy is a plain object, not an instance (the #private brand is lost).
    slot = { ...sanitized };
    // @ts-expect-error overlaying one raw field on a spread copy is refused.
    slot = { ...sanitized, summary: raw.summary };
    // @ts-expect-error overlaying all raw fields on a spread copy is refused.
    slot = { ...sanitized, ...raw };
    // @ts-expect-error a version's fields must be sanitized; a raw literal does not satisfy the DTO.
    const notDto: PrDescriptionVersionDTO = version();
    void notDto;
    // @ts-expect-error no other module can construct one: the mint token is module-private.
    assert.throws(() => new SanitizedPrDescriptionFields(Symbol("forged"), raw), TypeError);
    // Nor can a subclass, which has to pass the same token to super().
    class Forged extends SanitizedPrDescriptionFields {
      constructor() {
        // @ts-expect-error the mint token is module-private.
        super(Symbol("forged"), raw);
      }
    }
    assert.throws(() => new Forged(), TypeError);

    // KNOWN TYPE HOLES (documented on the class): Object.assign's declared return type is an
    // intersection, which is assignable to each member, so this compiles; so does an `any`.
    // Neither is an instance at runtime, and is() says so (the brand is #private, so a forged
    // prototype does not pass either).
    slot = Object.assign({}, sanitized, raw);
    assert.equal(SanitizedPrDescriptionFields.is(slot), false);
    const untyped: unknown = JSON.parse(JSON.stringify(sanitized));
    slot = untyped as any;
    assert.equal(SanitizedPrDescriptionFields.is(slot), false);
    assert.equal(SanitizedPrDescriptionFields.is(Object.create(SanitizedPrDescriptionFields.prototype)), false);
    for (const spread of [{ ...sanitized }, { ...sanitized, ...raw }]) {
      assert.equal(SanitizedPrDescriptionFields.is(spread), false);
    }
    // structuredClone and Proxy are declared to return their argument's type, so both compile
    // into the slot with no cast; neither carries the #private brand at runtime.
    slot = structuredClone(sanitized);
    assert.equal(SanitizedPrDescriptionFields.is(slot), false);
    assert.equal(Object.getPrototypeOf(slot), Object.prototype);
    slot = new Proxy(sanitized, {});
    assert.equal(SanitizedPrDescriptionFields.is(slot), false);
    // A lying proxy cannot report other text for a frozen member: the Proxy invariant throws.
    const lying = new Proxy(sanitized, { get: (t, k, r) => (k === "summary" ? raw.summary : Reflect.get(t, k, r)) });
    assert.throws(() => lying.summary, TypeError);
    // Mapped types over the class drop the #private brand, so they are structural: Pick of the
    // data members takes raw fields, Readonly<> takes raw fields plus a toRaw function.
    const picked: Pick<SanitizedPrDescriptionFields, "summary" | "changes" | "scope_notes" | "review_pointers" | "verification"> = raw;
    const readonlyView: Readonly<SanitizedPrDescriptionFields> = { ...raw, toRaw: () => raw };
    assert.equal(SanitizedPrDescriptionFields.is(picked), false);
    assert.equal(SanitizedPrDescriptionFields.is(readonlyView), false);
    assert.ok(SanitizedPrDescriptionFields.is(sanitized));

    // A sanitized value is NOT assignable where raw fields are expected (readonly arrays): a
    // refresh re-stages through toRaw(), which the api sanitizes again.
    // @ts-expect-error readonly sanitized arrays do not fit the mutable raw request shape.
    const badRestage: PrDescriptionStageRequest = { ...stageReq(), fields: sanitized };
    void badRestage;
    const restage: PrDescriptionStageRequest = { ...stageReq(), fields: sanitized.toRaw() };
    assert.deepEqual(restage.fields, version().fields);
  });
});
