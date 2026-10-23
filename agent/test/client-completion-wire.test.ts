import { describe, it, before, after } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import { readFileSync } from "node:fs";
import type { AddressInfo } from "node:net";
import { nullLogger } from "./helpers.js";
import { FakeApi } from "./fake-api.js";
import { WorkerClient } from "../src/client.js";

// PRD #1232 M2: the agent half of the completion-interlock wire contract. The api side records the
// replies as fixtures/completion-wire/*.json (api/internal/handler TestCompletionWireFixtures); this
// file (1) decodes those exact bytes with the REAL WorkerClient, and (2) proves the shared FakeApi,
// configured ONLY through value-level knobs, emits the same JSON (keys and values) after the
// fixtures' normalization, so the fake every runner test leans on cannot drift from the api.

const TOKEN = "worker-join-token-0123456789";
const UUID_RE = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi;
const TS_RE = /\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})/g;
const NIL_UUID = "00000000-0000-4000-8000-000000000000";

function fixtureBytes(name: string): string {
  return readFileSync(new URL(`../../fixtures/completion-wire/${name}`, import.meta.url), "utf8");
}
function fixture(name: string): unknown {
  return JSON.parse(fixtureBytes(name));
}

/** The README's normalization, and nothing else: UUIDs and RFC 3339 timestamps in string values. */
function normalize(v: unknown): unknown {
  if (typeof v === "string") return v.replace(UUID_RE, NIL_UUID).replace(TS_RE, "2026-01-01T00:00:00Z");
  if (Array.isArray(v)) return v.map(normalize);
  if (v !== null && typeof v === "object") {
    return Object.fromEntries(Object.entries(v as Record<string, unknown>).map(([k, x]) => [k, normalize(x)]));
  }
  return v;
}

/** A stub api that answers every completion route with the given raw bytes. */
async function bytesServer(raw: string): Promise<{ url: string; close: () => Promise<void> }> {
  const server = http.createServer((req, res) => {
    req.resume();
    req.on("end", () => {
      if ((req.url ?? "").endsWith("/worker/register")) {
        res.writeHead(200, { "Content-Type": "application/json" });
        res.end(JSON.stringify({ worker_id: "w1" }));
        return;
      }
      res.writeHead(200, { "Content-Type": "application/json" });
      res.end(raw);
    });
  });
  await new Promise<void>((r) => server.listen(0, "127.0.0.1", () => r()));
  server.unref();
  const { port } = server.address() as AddressInfo;
  return { url: `http://127.0.0.1:${port}`, close: () => new Promise<void>((r) => server.close(() => r())) };
}

function client(url: string): WorkerClient {
  return new WorkerClient(url, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {} });
}

describe("completion wire fixtures decode through the real WorkerClient", () => {
  it("attempt-unmet.json: unmet [m5], attempt_count 1", async () => {
    const srv = await bytesServer(fixtureBytes("attempt-unmet.json"));
    try {
      const r = await client(srv.url).recordCompletionAttempt("run-1", {
        milestonesCompleted: ["m1", "m2", "m3", "m4"], head: "H", worktreeFingerprint: null,
      });
      assert.deepStrictEqual(r, { unmet: ["m5"], attemptCount: 1 });
    } finally {
      await srv.close();
    }
  });

  it("permit-denied.json: non-terminal denial, missing_milestones, unmet [m5]", async () => {
    const srv = await bytesServer(fixtureBytes("permit-denied.json"));
    try {
      const r = await client(srv.url).requestCompletionPermit("run-1", {
        contractRevision: 1, branch: "agent/issue-1", head: "H",
      });
      assert.strictEqual(r.granted, false);
      assert.strictEqual(r.denyReason, "missing_milestones");
      assert.deepStrictEqual(r.unmet, ["m5"]);
      assert.strictEqual(r.permit, undefined);
    } finally {
      await srv.close();
    }
  });

  it("permit-granted.json: permit id, revision, branch, head, empty scope", async () => {
    const srv = await bytesServer(fixtureBytes("permit-granted.json"));
    try {
      const r = await client(srv.url).requestCompletionPermit("run-1", {
        contractRevision: 1, branch: "agent/issue-1", head: "H",
      });
      assert.strictEqual(r.granted, true);
      assert.strictEqual(r.denyReason, undefined);
      assert.deepStrictEqual(r.permit, {
        id: NIL_UUID,
        contractRevision: 1,
        branch: "agent/issue-1",
        head: "H",
        issuedAt: "2026-01-01T00:00:00Z",
        findingIds: [],
        completionScope: {},
      });
    } finally {
      await srv.close();
    }
  });
});

describe("FakeApi value-level knobs emit the recorded completion wire (parity)", () => {
  let api: FakeApi;
  let url: string;
  before(async () => {
    api = new FakeApi(TOKEN);
    url = await api.listen();
  });
  after(async () => {
    await api.close();
  });

  async function post(path: string, body: Record<string, unknown>): Promise<unknown> {
    const res = await fetch(`${url}/api/worker/runs/run-1/completion/${path}`, {
      method: "POST",
      headers: { authorization: `Bearer ${TOKEN}`, "content-type": "application/json" },
      body: JSON.stringify(body),
    });
    assert.strictEqual(res.status, 200);
    return JSON.parse(await res.text());
  }

  it("attempt reply equals attempt-unmet.json", async () => {
    api.setCompletionAttemptUnmet(["m5"]);
    const got = await post("attempt", {
      milestones_completed: ["m1", "m2", "m3", "m4"], head: "H", worktree_fingerprint: null,
    });
    assert.deepStrictEqual(normalize(got), fixture("attempt-unmet.json"));
  });

  it("denied permit reply equals permit-denied.json", async () => {
    api.setCompletionPermitResponse(false, { denyReason: "missing_milestones" });
    api.setCompletionPermitDenyUnmet(["m5"]);
    const got = await post("permit", { contract_revision: 1, branch: "agent/issue-1", head: "H" });
    assert.deepStrictEqual(normalize(got), fixture("permit-denied.json"));
  });

  it("granted permit reply equals permit-granted.json", async () => {
    api.setCompletionPermitResponse(true);
    api.setCompletionPermitId(NIL_UUID);
    const got = await post("permit", { contract_revision: 1, branch: "agent/issue-1", head: "H" });
    assert.deepStrictEqual(normalize(got), fixture("permit-granted.json"));
  });
});
