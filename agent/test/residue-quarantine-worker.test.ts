import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import type { AddressInfo } from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { WorkerClient } from "../src/client.js";
import type { Config } from "../src/config.js";
import { latchResidueQuarantine, residueQuarantine } from "../src/residue-quarantine.js";
import type { TerminalRejectionCoordinator } from "../src/terminal-rejections.js";
import { Worker } from "../src/worker.js";
import type { RunRunner } from "../src/runner.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import type { ChatClaimResponse, ClaimResponse, WorkerStats } from "../src/protocol.js";
import { StatsCollector } from "../src/stats.js";
import { nullLogger } from "./helpers.js";
import { resetResidueQuarantineAfterEach } from "./setup/hermetic-proc.js";

resetResidueQuarantineAfterEach();

// issue #2213 — the worker-wide residue quarantine latch as the worker loop and the heartbeat see it.
// Layer: the Worker claim loops (admission) and the WorkerClient heartbeat wire (visibility).

const TOKEN = "worker-join-token-0123456789";
const FIXTURE_URL = new URL("../../fixtures/worker-heartbeat-residue-quarantine/latched.json", import.meta.url);

interface LatchedFixture {
  residue_quarantine: { cause: string; latched_at: string; run_id: string | null; site: string };
}
/** A throw-on-unreadable read: a missing fixture is fatal, never a skip. */
function readFixture(): LatchedFixture {
  return JSON.parse(fs.readFileSync(fileURLToPath(FIXTURE_URL), "utf8")) as LatchedFixture;
}

// ─── heartbeat wire ────────────────────────────────────────────────────────────────────────

interface Recorded {
  kind: "register" | "heartbeat" | "other";
  body: Record<string, unknown> | undefined;
}
interface Prog {
  url: string;
  close: () => Promise<void>;
  requests: Recorded[];
  cfg: { features: string[]; heartbeat: (i: number) => { status: number; body?: string } };
}

async function startServer(): Promise<Prog> {
  const requests: Recorded[] = [];
  const cfg: Prog["cfg"] = { features: [], heartbeat: () => ({ status: 204 }) };
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("utf8");
      let body: Record<string, unknown> | undefined;
      try {
        body = raw ? (JSON.parse(raw) as Record<string, unknown>) : undefined;
      } catch {
        body = undefined;
      }
      const url = req.url ?? "";
      let reply: { status: number; body?: string };
      if (url.endsWith("/register")) {
        requests.push({ kind: "register", body });
        reply = { status: 200, body: JSON.stringify({ worker_id: "w1", protocol_features: cfg.features }) };
      } else if (url.endsWith("/heartbeat")) {
        reply = cfg.heartbeat(requests.filter((r) => r.kind === "heartbeat").length);
        requests.push({ kind: "heartbeat", body });
      } else {
        requests.push({ kind: "other", body });
        reply = { status: 404 };
      }
      res.writeHead(reply.status, reply.body ? { "Content-Type": "application/json" } : {});
      res.end(reply.body ?? "");
    });
  });
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  const port = (server.address() as AddressInfo).port;
  return {
    url: `http://127.0.0.1:${port}`,
    close: () => new Promise<void>((resolve) => server.close(() => resolve())),
    requests,
    cfg,
  };
}

let srv: Prog;
beforeEach(async () => {
  srv = await startServer();
});
afterEach(async () => {
  await srv.close();
});

const newClient = (): WorkerClient =>
  new WorkerClient(srv.url, TOKEN, "0.1.0-test", nullLogger(), { sleep: async () => {}, terminalRetrySchedule: [1, 1, 1] });
const heartbeats = (): Recorded[] => srv.requests.filter((r) => r.kind === "heartbeat");
const INVALID_BODY = { status: 400, body: JSON.stringify({ error: "invalid request body" }) };

/** Latch like the detection site does, with the exact sample cause of the shared fixture. */
function latchLikeFixture(): void {
  const f = readFixture().residue_quarantine;
  latchResidueQuarantine({ cause: f.cause, runId: f.run_id ?? undefined, site: f.site }, nullLogger(), new Date(f.latched_at));
}

describe("heartbeat residue_quarantine (issue #2213)", () => {
  it("the body's member has exactly the shared fixture's key set and types, and its values for the same latch", async () => {
    srv.cfg.features = ["worker_residue_quarantine"];
    latchLikeFixture();
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    const sent = heartbeats()[0]!.body?.residue_quarantine as Record<string, unknown>;
    const fixture = readFixture().residue_quarantine;
    assert.deepEqual(Object.keys(sent).sort(), Object.keys(fixture).sort());
    for (const k of Object.keys(fixture) as Array<keyof typeof fixture>) {
      assert.equal(typeof sent[k], typeof fixture[k], `${k} has the fixture's JSON type`);
    }
    assert.deepEqual(sent, fixture, "the fixture is exactly what the agent sends for this latch");
    assert.match(String(sent.latched_at), /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  });

  it("run_id is null when no run detected the process", async () => {
    srv.cfg.features = ["worker_residue_quarantine"];
    latchResidueQuarantine({ cause: "c", site: "review_pre_fetch" }, nullLogger());
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    const sent = heartbeats()[0]!.body!.residue_quarantine as { run_id: unknown };
    assert.equal(sent.run_id, null);
  });

  it("a hostile cause (ESC, a bidi override, a newline, an oversized tail) reaches the wire sanitized and capped", async () => {
    srv.cfg.features = ["worker_residue_quarantine"];
    latchResidueQuarantine({ cause: `ev\u001b[31mil\u202ex\nboom${"y".repeat(500)}`, runId: "r1", site: "pre_clone" }, nullLogger());
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    const sent = heartbeats()[0]!.body!.residue_quarantine as { cause: string };
    // eslint-disable-next-line no-control-regex
    assert.doesNotMatch(sent.cause, /[\u0000-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/);
    assert.ok(sent.cause.startsWith("ev?[31mil?x?boom"));
    assert.ok(sent.cause.length <= 163);
  });

  it("is not sent while the worker is not latched, even when the api advertised the token", async () => {
    srv.cfg.features = ["worker_residue_quarantine"];
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    assert.ok(heartbeats()[0]!.body && !("residue_quarantine" in heartbeats()[0]!.body!));
  });

  it("is not sent to an api that did not advertise worker_residue_quarantine", async () => {
    srv.cfg.features = [];
    latchLikeFixture();
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    assert.ok(heartbeats()[0]!.body && !("residue_quarantine" in heartbeats()[0]!.body!));
  });

  it("is stripped on the strict-decode fallback, which clears the feature set", async () => {
    srv.cfg.features = ["worker_residue_quarantine"];
    srv.cfg.heartbeat = (i) => (i === 0 ? INVALID_BODY : { status: 204 });
    latchLikeFixture();
    const c = newClient();
    await c.register("w");
    await c.heartbeat(undefined, undefined, undefined, undefined, residueQuarantine());
    const hb = heartbeats();
    assert.equal(hb.length, 2, "one stripped retry");
    assert.ok(hb[0]!.body && "residue_quarantine" in hb[0]!.body!, "the first attempt carried it");
    assert.ok(hb[1]!.body && !("residue_quarantine" in hb[1]!.body!), "the retry did not");
    assert.equal(c.hasFeature("worker_residue_quarantine"), false);
  });
});

// ─── admission ─────────────────────────────────────────────────────────────────────────────

interface Rig {
  worker: Worker;
  beats: Array<{ stats: WorkerStats | undefined; quarantine: unknown }>;
  claims: { run: number; chat: number };
  executed: string[];
}

const rigRoots: string[] = [];
afterEach(() => {
  for (const r of rigRoots.splice(0)) fs.rmSync(r, { recursive: true, force: true });
});

function makeRig(opts: { runClaim?: () => Promise<ClaimResponse | null>; terminalRejections?: TerminalRejectionCoordinator } = {}): Rig {
  const root = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-2213-worker-"));
  rigRoots.push(root);
  const beats: Rig["beats"] = [];
  const claims = { run: 0, chat: 0 };
  const executed: string[] = [];
  const client = {
    register: async () => ({ worker_id: "wid-1" }),
    heartbeat: async (stats: WorkerStats | undefined, _o: unknown, _s: unknown, _m: unknown, quarantine: unknown) => {
      beats.push({ stats, quarantine });
      return false;
    },
    hasFeature: () => false,
    claimRun: async (): Promise<ClaimResponse | null> => {
      claims.run++;
      return opts.runClaim ? opts.runClaim() : null;
    },
    claimChat: async (): Promise<ChatClaimResponse | null> => {
      claims.chat++;
      return null;
    },
  } as unknown as WorkerClient;
  const config = {
    workerName: "w1",
    workerTemplate: "base",
    pollIntervalMs: 1,
    heartbeatIntervalMs: 2,
    chatPollMs: 1,
    chatSessions: 1,
    maxConcurrentRuns: 1,
    dataDir: root,
  } as unknown as Config;
  const runner = {
    resumePendingRecoveries: async () => {},
    execute: async (c: ClaimResponse) => {
      executed.push(c.run_id);
    },
  } as unknown as RunRunner;
  const worker = new Worker(
    config,
    client,
    runner,
    { execute: async () => {} } as unknown as ChatRunner,
    { execute: async () => {} } as unknown as JudgeRunner,
    { execute: async () => {} } as unknown as ReviewRunner,
    nullLogger(),
    () => ({ ok: true, missing: [] as string[] }),
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
    undefined,
    (dataDir) => new StatsCollector({ dataDir, processRss: () => 64 * 1024 * 1024 }),
    undefined,
    undefined,
    undefined,
    opts.terminalRejections,
  );
  return { worker, beats, claims, executed };
}

async function until(cond: () => boolean, what: string): Promise<void> {
  const deadline = Date.now() + 10_000;
  while (!cond()) {
    if (Date.now() > deadline) throw new Error(`timed out waiting for ${what}`);
    await new Promise((r) => setTimeout(r, 2));
  }
}

describe("a quarantined worker admits nothing (issue #2213)", () => {
  it("control: an unlatched worker claims on both lanes", async () => {
    const rig = makeRig();
    const ac = new AbortController();
    const done = rig.worker.run(ac.signal);
    try {
      await until(() => rig.claims.run >= 2 && rig.claims.chat >= 2, "claims on both lanes");
    } finally {
      ac.abort();
      await done.catch(() => undefined);
    }
  });

  it("latched: zero claimRun and zero claimChat calls while heartbeats continue, and each carries the latch", async () => {
    latchResidueQuarantine({ cause: "pid 7 unattributed", runId: "r1", site: "pre_clone" }, nullLogger());
    const rig = makeRig();
    const ac = new AbortController();
    const done = rig.worker.run(ac.signal);
    try {
      await until(() => rig.beats.length >= 5, "heartbeats while latched");
      // Let both claim loops spin well past several poll intervals.
      await new Promise((r) => setTimeout(r, 60));
      assert.equal(rig.claims.run, 0, "the run lane claimed nothing");
      assert.equal(rig.claims.chat, 0, "the chat lane claimed nothing");
      assert.ok(rig.beats.every((b) => (b.quarantine as { cause?: string } | undefined)?.cause === "pid 7 unattributed"));
    } finally {
      ac.abort();
      await done.catch(() => undefined);
    }
  });

  it("a latch set while the worker runs stops the next claim on both lanes", async () => {
    const rig = makeRig();
    const ac = new AbortController();
    const done = rig.worker.run(ac.signal);
    try {
      await until(() => rig.claims.run >= 2 && rig.claims.chat >= 2, "claims before the latch");
      latchResidueQuarantine({ cause: "late", site: "finalize" }, nullLogger());
      await new Promise((r) => setTimeout(r, 30)); // an in-flight claim call settles
      const run = rig.claims.run;
      const chat = rig.claims.chat;
      await new Promise((r) => setTimeout(r, 60));
      assert.equal(rig.claims.run, run, "no further run claim");
      assert.equal(rig.claims.chat, chat, "no further chat claim");
    } finally {
      ac.abort();
      await done.catch(() => undefined);
    }
  });

  it("a latch landing while admission is pending stops the claim and releases the admission", async () => {
    let acquired = 0;
    let released = 0;
    const terminalRejections = {
      acquireAdmission: async () => {
        acquired++;
        await new Promise((r) => setTimeout(r, 5));
        latchResidueQuarantine({ cause: "latched during admission", site: "finalize" }, nullLogger());
        return () => {
          released++;
        };
      },
      loop: async () => {},
      protectExecution: () => () => {},
    } as unknown as TerminalRejectionCoordinator;
    const rig = makeRig({ terminalRejections });
    const ac = new AbortController();
    const done = rig.worker.run(ac.signal);
    try {
      await until(() => acquired >= 1 && released >= 1, "admission acquired and released");
      await new Promise((r) => setTimeout(r, 40));
      assert.equal(rig.claims.run, 0, "no claimRun after the latch landed mid-admission");
      assert.equal(released, acquired, "every acquired admission was released");
    } finally {
      ac.abort();
      await done.catch(() => undefined);
    }
  });
});
