// PRD #1247 M5b — the INERT held-state credential-switch WIRE SEAM (TypeScript half).
//
// These tests pin the two pieces the shared FakeApi cannot express:
//   1. readRunAck reading the TOP-LEVEL credential_switch / stale-claim disposition (unit level).
//   2. getInputs surfacing a credential_switch beside the inputs, and postMessages / MessageBatcher
//      stamping (or, by default, OMITTING) claim_generation on the wire body.
// Everything here is additive/optional: with nothing set, the wire body stays byte-identical, which
// is exactly what the two claim_generation tests assert. A LATER behavior unit will act on the
// parsed fields and set the generation; today nothing does.

import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient, readRunAck } from "../src/client.js";
import { nullLogger } from "./helpers.js";

const TOKEN = "worker-join-token-0123456789";

interface CapturedRequest {
  method: string;
  path: string;
  body: Record<string, unknown>;
}

/**
 * A tiny worker-protocol server that records every request's parsed JSON body and answers each
 * request from a supplied responder. Enough to assert the EXACT wire body a /messages POST carries
 * (which the shared FakeApi discards) and to return an inputs body carrying the M5
 * credential_switch signal (which the shared FakeApi cannot express). Not a general fake — each
 * test supplies the one response shape it needs.
 */
class Recorder {
  readonly requests: CapturedRequest[] = [];
  private readonly server: http.Server;

  constructor(private readonly respond: (req: CapturedRequest) => { status: number; body: unknown }) {
    this.server = http.createServer((req, res) => {
      let data = "";
      req.on("data", (chunk) => (data += chunk));
      req.on("end", () => {
        const url = new URL(req.url ?? "/", "http://fake");
        const captured: CapturedRequest = {
          method: req.method ?? "GET",
          path: url.pathname,
          body: data ? (JSON.parse(data) as Record<string, unknown>) : {},
        };
        this.requests.push(captured);
        const { status, body } = this.respond(captured);
        res.writeHead(status, { "Content-Type": "application/json" });
        res.end(JSON.stringify(body));
      });
    });
  }

  async listen(): Promise<string> {
    await new Promise<void>((resolve) => this.server.listen(0, "127.0.0.1", resolve));
    // unref so a lingering handle never keeps the test file from draining (see fake-api.ts).
    this.server.unref();
    const { port } = this.server.address() as AddressInfo;
    return `http://127.0.0.1:${port}`;
  }

  async close(): Promise<void> {
    await new Promise<void>((resolve, reject) =>
      this.server.close((err) => (err ? reject(err) : resolve())),
    );
  }
}

const openServers: Recorder[] = [];

async function start(
  respond: (req: CapturedRequest) => { status: number; body: unknown },
): Promise<{ url: string; requests: CapturedRequest[] }> {
  const rec = new Recorder(respond);
  openServers.push(rec);
  const url = await rec.listen();
  return { url, requests: rec.requests };
}

function newClient(baseUrl: string): WorkerClient {
  return new WorkerClient(baseUrl, TOKEN, "0.1.0-test", nullLogger(), {
    sleep: async () => {},
    terminalRetrySchedule: [1, 1, 1],
  });
}

afterEach(async () => {
  while (openServers.length > 0) await openServers.pop()!.close();
});

// The signal + disposition ride the ack body TOP-LEVEL, beside `run`, on the 200 ack, the ordinary
// 409, and (for stale_claim) the released/superseded 409. readRunAck reads the single-use body once.
describe("readRunAck — PRD #1247 M5 held-state seam", () => {
  it("surfaces a top-level credential_switch alongside run.status on a 200 ack", async () => {
    const fields = await readRunAck(
      new Response(JSON.stringify({ run: { status: "running" }, credential_switch: { generation: 7 } }), {
        status: 200,
      }),
    );
    assert.deepStrictEqual(fields.credentialSwitch, { generation: 7 });
    assert.strictEqual(fields.staleClaim, undefined);
    assert.strictEqual(fields.status, "running");
  });

  it("detects the top-level stale_claim disposition on a 409 ack", async () => {
    const fields = await readRunAck(
      new Response(JSON.stringify({ run: { status: "cancelled" }, disposition: "stale_claim" }), { status: 409 }),
    );
    assert.strictEqual(fields.staleClaim, true);
    assert.strictEqual(fields.credentialSwitch, undefined);
    assert.strictEqual(fields.status, "cancelled");
  });

  it("leaves both fields absent when the body carries neither (back-compat)", async () => {
    const fields = await readRunAck(new Response(JSON.stringify({ run: { status: "running" } }), { status: 200 }));
    assert.strictEqual(fields.credentialSwitch, undefined);
    assert.strictEqual(fields.staleClaim, undefined);
  });

  it("ignores a malformed credential_switch (non-numeric generation)", async () => {
    const fields = await readRunAck(
      new Response(JSON.stringify({ run: { status: "running" }, credential_switch: { generation: "nope" } }), {
        status: 200,
      }),
    );
    assert.strictEqual(fields.credentialSwitch, undefined);
  });
});

describe("getInputs — PRD #1247 M5 credential_switch signal", () => {
  it("surfaces credential_switch beside the inputs when the poll carries it", async () => {
    const { url } = await start(() => ({
      status: 200,
      body: { inputs: [{ id: 3, kind: "follow_up", body: "go" }], credential_switch: { generation: 11 } },
    }));
    const res = await newClient(url).getInputs("run-1");
    assert.deepStrictEqual(res.inputs, [{ id: 3, kind: "follow_up", body: "go" }]);
    assert.deepStrictEqual(res.credentialSwitch, { generation: 11 });
  });

  it("returns the array with credentialSwitch undefined when the poll omits it", async () => {
    const { url } = await start(() => ({ status: 200, body: { inputs: [] } }));
    const res = await newClient(url).getInputs("run-1");
    assert.deepStrictEqual(res.inputs, []);
    assert.strictEqual(res.credentialSwitch, undefined);
  });
});

