import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import {
  PrDescriptionConflict,
  PrDescriptionInvalid,
  PrDescriptionNotFound,
  PrDescriptionRateLimited,
  RequestError,
  WorkerClient,
  isTransient,
  type PrDescriptionConflictReason,
} from "../src/client.js";
import {
  PR_DESC_ACK_OUTCOMES,
  type PrDescriptionAckRequest,
  type PrDescriptionBindRequest,
  type PrDescriptionLookupRequest,
  type PrDescriptionStageRequest,
  type PrDescriptionState,
  type PrDescriptionVersionDTO,
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

function newClient(): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {} });
}

const zero = () => ({ added: 0, deleted: 0 });

function version(over: Partial<PrDescriptionVersionDTO> = {}): PrDescriptionVersionDTO {
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
    state: "pending",
    created_at: "2026-09-27T10:00:00Z",
    published_at: null,
    ...over,
  };
}

function prState(over: Partial<PrDescriptionState> = {}): PrDescriptionState {
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
    assert.deepEqual(await newClient().stagePrDescription(RUN_ID, stageReq()), { version: version() });

    const bound = { version: version({ mr_iid: 42, rendered_region_sha256: REGION }), pr: prState({ published_version: published, last_outcome: "published" }) };
    respond = () => ({ status: 200, body: JSON.stringify(bound) });
    assert.deepEqual(await newClient().bindPrDescription(RUN_ID, bindReq()), bound);

    for (const lookup of [
      { match: "published", matched_version_id: VERSION_ID, pr: prState({ published_version: published }) },
      { match: "pending", matched_version_id: VERSION_ID, pr: prState() },
      { match: "none", matched_version_id: null, pr: null },
    ]) {
      respond = () => ({ status: 200, body: JSON.stringify(lookup) });
      assert.deepEqual(await newClient().lookupPrDescription(RUN_ID, lookupReq()), lookup);
    }

    for (const outcome of PR_DESC_ACK_OUTCOMES) {
      const ack = { pr: prState({ lock_version: 8, last_outcome: outcome }), recovered_version_id: outcome === "published" ? VERSION_ID : null };
      respond = () => ({ status: 200, body: JSON.stringify(ack) });
      assert.deepEqual(await newClient().ackPrDescription(RUN_ID, { ...ackReq(), outcome }), ack);
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
        { version: { ...version(), fields: { ...version().fields, scope_notes: [{ kind: "removed", text: "x" }] } } },
      ],
      bind: [{ version: version() }, { version: version(), pr: { ...prState(), lock_version: "7" } }],
      lookup: [{ match: "maybe", matched_version_id: null, pr: null }, { match: "none", matched_version_id: 1, pr: null }],
      ack: [{ pr: prState({ last_outcome: "abandoned" as never }), recovered_version_id: null }, { recovered_version_id: null }],
    };
    for (const { op, call } of CALLS) {
      for (const body of bad[op]!) {
        respond = () => ({ status: 200, body: typeof body === "string" ? body : JSON.stringify(body) });
        await assert.rejects(call(newClient()), (err: unknown) => {
          assert.ok(!(err instanceof RequestError), `${op}: ${String(err)}`);
          assert.match(String(err), new RegExp(`malformed pr-description ${op} response`));
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

  it("absent on a claim without a PR record", async () => {
    respond = () => ({ status: 200, body: claimBody({}) });
    const claim = await newClient().claimRun();
    assert.ok(claim);
    assert.equal(claim.pr_description, undefined);
  });

  it("carries the PR state, including the published version, when present", async () => {
    const state = prState({
      last_outcome: "published",
      published_version: version({ state: "published", mr_iid: 42, rendered_region_sha256: REGION, published_at: "2026-09-27T10:05:00Z" }),
    });
    respond = () => ({ status: 200, body: claimBody({ pr_description: state }) });
    const claim = await newClient().claimRun();
    assert.deepEqual(claim?.pr_description, state);
    assert.equal(claim?.pr_description?.published_version?.rendered_region_sha256, REGION);
  });
});
