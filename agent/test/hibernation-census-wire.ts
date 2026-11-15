import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import type { InventoryEntry } from "../src/hibernation-inventory.js";

// What the api's inventory decoder accepts (PRD #4208 U2 wire contract): kind, classification and content_state match
// ^[a-z0-9_:.-]{1,64}$, no string carries a control or bidi character, needs_receipt carries a run uuid and a 40 or
// 64 hex tip, and the entry cap is 10,000.
const TOKEN = /^[a-z0-9_:.-]{1,64}$/;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const TIP = /^(?:[0-9a-f]{40}|[0-9a-f]{64})$/;
const UNSAFE = /\p{Cc}|\p{Cf}|\p{Zl}|\p{Zp}/u;
const KINDS = new Set(["runtime_lock", "outbox", "agent_home", "jobs", "lost_found", "bare_repo", "runner_ref", "worktree",
  "canonical_clone", "retained_attempt", "journal", "unsettled_op", "runner_scratch", "unknown_path"]);
const CLASSES = new Set(["disposable", "needs_receipt", "protected", "unknown", "unrecoverable"]);
const STATES = new Set(["content_proven", "unproven_worktree_bytes", "ref", "none"]);

export function assertWireEntries(entries: readonly InventoryEntry[]): void {
  assert.ok(entries.length <= 10_000);
  for (const e of entries) {
    const text = JSON.stringify(e);
    assert.match(e.kind, TOKEN, text);
    assert.ok(KINDS.has(e.kind), `kind ${text}`);
    assert.match(e.classification, TOKEN, text);
    assert.ok(CLASSES.has(e.classification), `classification ${text}`);
    if (e.content_state !== undefined) {
      assert.match(e.content_state, TOKEN, text);
      assert.ok(STATES.has(e.content_state), `content_state ${text}`);
    }
    for (const value of Object.values(e)) assert.equal(UNSAFE.test(String(value)), false, `control or bidi character in ${text}`);
    assert.ok(e.path.length > 0 && e.path.length <= 1024, text);
    if (e.detail !== undefined) assert.match(e.detail, TOKEN, text);
    if (e.classification === "needs_receipt") {
      assert.match(e.run_id ?? "", UUID, text);
      assert.match(e.tip ?? "", TIP, text);
    }
    if (e.run_id !== undefined) assert.match(e.run_id, UUID, text);
    if (e.tip !== undefined) assert.match(e.tip, TIP, text);
  }
}

export interface FakeDocker {
  host: string;
  requests: string[];
  close(): Promise<void>;
}

/** A GET-only stand-in for the DinD daemon on a loopback port; `routes` maps a request path to a JSON body or a status. */
export async function startFakeDocker(routes: Record<string, unknown>): Promise<FakeDocker> {
  const requests: string[] = [];
  const server = http.createServer((req, res) => {
    requests.push(`${req.method} ${req.url}`);
    const body = routes[req.url ?? ""];
    if (req.method !== "GET" || body === undefined) { res.statusCode = 404; res.end("{}"); return; }
    if (typeof body === "number") { res.statusCode = body; res.end("{}"); return; }
    res.setHeader("content-type", "application/json");
    res.end(JSON.stringify(body));
  });
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    host: `tcp://127.0.0.1:${port}`,
    requests,
    close: () => new Promise<void>(resolve => { server.close(() => resolve()); server.closeAllConnections(); }),
  };
}

/** The four routes a healthy, empty daemon answers. */
export const EMPTY_DOCKER: Record<string, unknown> = {
  "/containers/json?all=1": [],
  "/volumes": { Volumes: null, Warnings: null },
  "/images/json": [],
  "/system/df": { BuildCache: [] },
};
