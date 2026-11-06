// issue #2969 M3 (D3 transport, integrity channel and deferred cleanup): the retained re-claim's predecessor FINAL pass
// (RunRunner.settlePredecessorHolds) driven against the REAL RecoveryCoordinator.operateFrozenInventory with its real
// recoveryRPC, the real WorkerClient (reserve, upload, release) and the real maintenanceFetch / undici fetch. The api is a
// real node:http server reached by routing the client's recovery URLs (global fetch is wrapped only to rewrite the
// origin, or to model an uncooperative transport for the abort-never-settles cases). Real git, real journal, real
// registered exact-hold capability.
//
// Pass entry: the real retained re-claim (RunRunner.execute with a claim at generation 3 that finds a retained
// generation-2 source), exactly as runner-retained-predecessor-final.test.ts does; only the per-record operation is
// real here instead of stubbed. Two exact-hold facts (the list and the exact-hold GET) are served by the fake api too, so a
// failed grant-2 observation is produced by api data, not by a stub.
import { afterEach, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import http from "node:http";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { setTimeout as sleep } from "node:timers/promises";
import type { AddressInfo } from "node:net";
import type { ExecutorFactory } from "../src/runner.js";
import type { Executor } from "../src/executor.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import { type CaptureInput, type RecoveryCoordinator, type RecoveryOutcome, type RecoveryRecord } from "../src/recovery.js";
import { quiesceRunAttempt, mintAttemptId } from "../src/run-quiescence.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { fixtureFetchTracking, fixtureTrackingOptions } from "./runner-tracking-fixture.js";
import { api, baseUrl, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const JOURNAL_KEY = "fixture-journal-key";
const REPO_ID = "00000000-0000-4000-8000-000000002969";
const CODEX_BOUNDARY_MS = 1_000;
const FAR = "2099-01-01T00:00:00Z";
const FEATURES = ["recovery_inventory_v1", "claim_generation_fence", "recovery_held_publication_v2"];
const holdId = (generation: number): string => `00000000-0000-4000-8000-0000000001${String(generation).padStart(2, "0")}`;

function command(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com", "-c", "commit.gpgsign=false", ...args], {
    env: { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.startsWith("GIT_"))),
      GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
    encoding: "utf8", stdio: "pipe",
  }).trim();
}
function journal(bare: string, branch: string): any {
  return JSON.parse(command(bare, "config", `uzi-recovery.${branch}.clone`));
}

// ── the fake api ─────────────────────────────────────────────────────────────────────────────────────────────────────

type Kind = "holds" | "exact" | "reserve" | "status" | "upload" | "release" | "reconcile";
interface Hit { kind: Kind; generation: number | undefined; at: number }
interface Ctx { generation: number | undefined; body: Buffer; req: http.IncomingMessage; res: http.ServerResponse; hits: Hit[] }
/** Returns true when the hook wrote (or deliberately withheld) the response itself. */
type Hook = (ctx: Ctx) => Promise<boolean> | boolean;

interface Server {
  url: string; hits: Hit[];
  /** Per-kind scripted behaviour, consulted before the default for predecessor generations. */
  hooks: Partial<Record<Kind, Hook>>;
  /** Generations whose hold the api has closed (a committed FINAL). */
  released: Set<number>;
  receipts: Map<number, unknown>;
  /** Every release body the api received, in order (a FINAL replay must repeat the saved request exactly). */
  releaseBodies: Array<{ generation: number; body: any }>;
  /** What the exact-hold GET reports as the run's current claim generation, per hold generation. */
  currentGeneration: (generation: number) => number;
  /** The upload answer for a generation: a status and body, or undefined for the default success. */
  uploadRefusal: (generation: number) => { status: number; body: string } | undefined;
  workerId: string;
  close(): Promise<void>;
}

async function startServer(claim: { run_id: string }, claimGeneration: number, workerId: string): Promise<Server> {
  const captures = new Map<string, { generation: number; manifest?: { checksum: string; byte_size: number } }>();
  const hits: Hit[] = [];
  const server: Server = {
    url: "", hits, hooks: {}, released: new Set(), receipts: new Map(), releaseBodies: [], workerId,
    currentGeneration: () => claimGeneration, uploadRefusal: () => undefined, close: async () => {},
  };
  const holdFor = (generation: number) => ({ hold_id: holdId(generation), generation, inventory_guarded: true,
    has_available_capture: [...captures.values()].some(c => c.generation === generation && c.manifest !== undefined) });
  const http_ = http.createServer(async (req, res) => {
    const chunks: Buffer[] = [];
    for await (const chunk of req) chunks.push(chunk as Buffer);
    const body = Buffer.concat(chunks);
    const url = new URL(req.url!, "http://127.0.0.1");
    const rest = url.pathname.replace(`/api/worker/runs/${claim.run_id}/`, "");
    const json = (status: number, value: unknown): void => {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(value));
    };
    const generations = [1, 2, claimGeneration];
    const parse = (): any => { try { return JSON.parse(body.toString("utf8") || "{}"); } catch { return {}; } };
    const run = async (kind: Kind, generation: number | undefined, fallback: () => void): Promise<void> => {
      hits.push({ kind, generation, at: Date.now() });
      const hook = generation !== claimGeneration ? server.hooks[kind] : undefined;
      if (hook && await hook({ generation, body, req, res, hits })) return;
      fallback();
    };
    if (rest === "recovery-holds" && req.method === "GET") {
      const wanted = url.searchParams.get("generation");
      if (wanted === null) {
        return run("holds", undefined, () => json(200, { run_id: claim.run_id,
          holds: generations.filter(g => !server.released.has(g)).map(holdFor) }));
      }
      const generation = Number(wanted);
      return run("exact", generation, () => json(200, { run_id: claim.run_id, exact_hold: {
        run_id: claim.run_id, generation, hold_id: holdId(generation), repo_id: REPO_ID, inventory_guarded: true,
        original_worker_id: workerId, original_worker_identity: "original",
        run_status: "running", current_claim_generation: server.currentGeneration(generation), claim_released: false,
        ...(server.released.has(generation)
          ? { state: "released", release_evidence: "owner_discard" }
          : { state: "open", live_worker_id: workerId, live_run_id: claim.run_id }),
      } }));
    }
    if (rest === "archives/reserve") {
      const request = parse();
      const generation = Number(request.generation);
      return run("reserve", generation, () => {
        const key = String(request.idempotency_key);
        if (!captures.has(key)) captures.set(key, { generation });
        json(200, { capture_id: `cap-${generation}-${[...captures.keys()].indexOf(key)}`, state: "preparing" });
      });
    }
    if (rest === "archives/release") {
      const request = parse();
      const generation = Number(request.generation);
      if (generation !== claimGeneration) server.releaseBodies.push({ generation, body: request });
      return run("release", generation, () => {
        if (generation === claimGeneration) return json(503, { error: "claim generation keeps custody" });
        server.released.add(generation);
        server.receipts.set(generation, request.final_disposition);
        json(200, { run_id: claim.run_id, generation, released: true, holds_released: 1 });
      });
    }
    const captureRoute = /^archives\/([^/]+)(?:\/(upload|reconcile))?$/.exec(rest);
    if (captureRoute) {
      const id = captureRoute[1]!;
      const entry = [...captures.entries()].map(([, v], i) => ({ ...v, id: `cap-${v.generation}-${i}` })).find(c => c.id === id);
      const generation = entry?.generation;
      const live = [...captures.values()][Number(id.split("-")[2])];
      if (captureRoute[2] === "upload") {
        return run("upload", generation, () => {
          if (generation === claimGeneration) return json(503, { error: "claim generation upload deferred" });
          const refusal = generation === undefined ? undefined : server.uploadRefusal(generation);
          if (refusal) { res.writeHead(refusal.status); return void res.end(refusal.body); }
          if (live) live.manifest = JSON.parse(String(req.headers["x-uzi-recovery-manifest"]));
          json(200, { capture_id: id, state: "available", manifest_bound: true });
        });
      }
      if (captureRoute[2] === "reconcile") {
        return run("reconcile", generation, () => json(200, { run_id: claim.run_id, generation, capture_id: id,
          outcome: server.receipts.has(generation!) ? "accepted" : "replaceable",
          ...(server.receipts.has(generation!) ? { final_receipt: server.receipts.get(generation!) } : {}) }));
      }
      return run("status", generation, () => json(200, { capture_id: id, state: live?.manifest ? "available" : "preparing",
        manifest_bound: live?.manifest !== undefined, checksum: live?.manifest?.checksum, byte_size: live?.manifest?.byte_size,
        expires_at: FAR }));
    }
    json(404, { error: "unknown route " + rest });
  });
  await new Promise<void>(resolve => http_.listen(0, "127.0.0.1", resolve));
  server.url = `http://127.0.0.1:${(http_.address() as AddressInfo).port}`;
  server.close = async () => { http_.closeAllConnections(); await new Promise<void>(resolve => http_.close(() => resolve())); };
  return server;
}

// ── the world ────────────────────────────────────────────────────────────────────────────────────────────────────────

const cleanups: Array<() => Promise<void> | void> = [];
const callbackFailures: unknown[] = [];
afterEach(async () => {
  for (const cleanup of cleanups.splice(0).reverse()) await cleanup();
  assert.deepEqual(callbackFailures.splice(0), [], "a callback assertion was swallowed by the runner");
});

interface Opts {
  generations?: number[];
  claimGeneration?: number;
  budget?: { successorPrepAllowanceMs?: number; actionCapMs?: number; settlementMarginMs?: number };
  /** Awaited once the world exists and before the run starts: the earlier history of the frozen generations. */
  prestate?: (world: Pre) => Promise<void>;
  /** Called with the fake api and the world before the run starts. */
  arrange?: (server: Server, world: Pre) => void;
  /** Wraps the client's fetch of the recovery routes: `next` performs the real fetch to the fake api. */
  transport?: (kind: Kind, init: RequestInit | undefined, next: () => Promise<Response>, world: Pre) => Promise<Response>;
  /** Runs inside the successor's model call (the flight is still executing). */
  duringModel?: (world: Pre) => Promise<void>;
  /** Extra state: when the successor model never runs, the test may end the flight itself. */
  stopFlight?: boolean;
}
interface Pre {
  claim: ReturnType<typeof gitlabClaim>; bare: string; clone: { path: string; branch: string };
  events: string[]; recovery: RecoveryCoordinator; server: Server; runner: ReturnType<typeof runnerWith>;
  operateSignals: Array<AbortSignal | undefined>;
}

function kindOf(url: string): Kind | undefined {
  if (/\/recovery-holds\?generation=/.test(url)) return "exact";
  if (/\/recovery-holds$/.test(url)) return "holds";
  if (/\/archives\/reserve$/.test(url)) return "reserve";
  if (/\/archives\/release$/.test(url)) return "release";
  if (/\/archives\/[^/]+\/upload$/.test(url)) return "upload";
  if (/\/archives\/[^/]+\/reconcile$/.test(url)) return "reconcile";
  if (/\/archives\/[^/]+$/.test(url)) return "status";
  return undefined;
}

async function drive(o: Opts = {}) {
  const claimGeneration = o.claimGeneration ?? 3;
  const generations = o.generations ?? [2];
  const claim = gitlabClaim(2512, { claim_generation: claimGeneration, session_id: "predecessor-session" });
  claim.repo.id = REPO_ID;
  claim.inventory_guarded = true;
  const bare = await git.ensureClone(fx.originPath);
  fs.chmodSync(path.dirname(bare), 0o700);
  fs.chmodSync(bare, 0o700);
  const attemptId = mintAttemptId(Math.max(1, claimGeneration - 1));
  const clone = await git.createOrAttachRunnerClone(bare, 2512, noProofReseed, claim.run_id, false, undefined,
    { attemptId, isLive: () => false, beforeSeed: async () => {} });
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id, attemptId);
  // The predecessors' owed work: one committed change that the frozen generations cover, plus uncommitted dirty work.
  fs.writeFileSync(path.join(clone.path, "owed.txt"), "owed predecessor work\n");
  command(clone.path, "add", "owed.txt");
  command(clone.path, "commit", "-m", "owed predecessor work");
  fs.writeFileSync(path.join(clone.path, "retained.txt"), "only local dirty work\n");
  api.setOwnershipStatus(claim.run_id, "running", claimGeneration);

  api.setRegisterProtocolFeatures(FEATURES);
  const registration = await client.register("predecessor-transport", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
  client.getRunOwnership = async () => ({ status: "running", claim_generation: claimGeneration, inventory_guarded: true });

  const server = await startServer(claim, claimGeneration, registration.worker_id!);
  cleanups.push(() => server.close());

  const events: string[] = [];
  const operateSignals: Array<AbortSignal | undefined> = [];
  let pre!: Pre;
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (input: Parameters<typeof realFetch>[0], init?: Parameters<typeof realFetch>[1]) => {
    const url = typeof input === "string" ? input : input instanceof URL ? input.href : (input as Request).url;
    const kind = url.startsWith(baseUrl) && url.includes(`/runs/${claim.run_id}/`) ? kindOf(url) : undefined;
    if (!kind) return realFetch(input, init);
    const target = server.url + url.slice(baseUrl.length);
    const next = () => realFetch(target, init);
    // The claim's own generation is a different flight's custody; only a predecessor's requests are scripted.
    const fromBody = typeof init?.body === "string" ? Number(JSON.parse(init.body).generation) : undefined;
    const fromId = /\/archives\/cap-(\d+)-/.exec(url);
    const generation = fromBody ?? (fromId ? Number(fromId[1]) : /generation=(\d+)/.test(url) ? Number(/generation=(\d+)/.exec(url)![1]) : undefined);
    return o.transport && generation !== undefined && generation !== claimGeneration ? o.transport(kind, init, next, pre) : next();
  }) as typeof fetch;
  cleanups.push(() => { globalThis.fetch = realFetch; });

  const prepare = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => { events.push("prepare"); return prepare(...args); };
  let models = 0;
  const logs = recordingLogger();
  const make: ExecutorFactory = runId => {
    const run: Executor["run"] = async () => {
      models++;
      events.push("model");
      try { await o.duringModel?.(pre); } catch (error) { callbackFailures.push(error); }
      throw new Error("model interrupted before settled completion");
    };
    return { homeDir: path.join(homeDir, runId), executor: { run } };
  };
  const runner = runnerWith(make, fakeGitlab().gitlab, JOURNAL_KEY, logs.logger, {
    recoveryRetryMs: 1, quiesceRun: quiesceRunAttempt, codexBoundaryDeadlineMs: CODEX_BOUNDARY_MS,
    ...(o.budget ? { predecessorFinalBudgetMs: o.budget } : {}),
  } as never);
  const recovery = Reflect.get(runner, "recovery") as RecoveryCoordinator;

  // The owed work of each predecessor generation, frozen the way the generation's own claim froze it.
  const head = command(clone.path, "rev-parse", "HEAD");
  const frozen: RecoveryRecord[] = [];
  for (const generation of generations) {
    await fixtureFetchTracking(git, bare, clone.path, clone.branch, claim.run_id, generation);
    const options = await fixtureTrackingOptions(git, bare, clone.branch, claim.run_id, generation);
    const record = await recovery.freezeInventory({
      context: options.context as PositiveOwedCandidateContext, currentSha: head, defaultBranch: "main", repoId: REPO_ID, locallyQuiescent: true,
      heldPolicy: { version: 1, blocked: false, everKnown: false, flaggedCommits: [], ciGlobs: [".custom-ci/**"], extraFloors: [] },
    });
    assert.ok(record, `fixture: generation ${generation} froze`);
    frozen.push(record!);
  }
  const operated: Array<{ generation: number; outcome?: RecoveryOutcome; error?: unknown }> = [];
  const original = recovery.operateFrozenInventory.bind(recovery);
  recovery.operateFrozenInventory = async (input: CaptureInput, options = {}) => {
    if (!options.predecessor) return original(input, options);
    const generation = input.record.generation!;
    events.push(`operate:${generation}`);
    operateSignals.push(input.signal);
    const entry: (typeof operated)[number] = { generation };
    operated.push(entry);
    try { entry.outcome = await original(input, options); return entry.outcome; }
    catch (error) { entry.error = error; throw error; }
  };
  pre = { claim, bare, clone, events, recovery, server, runner, operateSignals };
  await o.prestate?.(pre);
  o.arrange?.(server, pre);
  await runner.execute(claim);
  return { ...pre, logs, operated, frozen, models: () => models };
}
type Result = Awaited<ReturnType<typeof drive>>;

function logged(r: Result, msg: string): Array<Record<string, any>> {
  return (r.logs.lines as Array<Record<string, any>>).filter(l => l.msg === msg);
}
const pass = (r: Result): Record<string, any> => logged(r, "retained predecessor FINAL pass").at(-1)!;
const postsOf = (r: { server: Server }, kind: Kind, generations = [1, 2]) =>
  r.server.hits.filter(h => h.kind === kind && h.generation !== undefined && generations.includes(h.generation));
const failedReports = (r: Result) => api.states.filter(s => s.runId === r.claim.run_id && s.body.status === "failed");

/** The retained recovery stopped before any successor: custody and the predecessor stay exactly as they were. */
function assertStopped(r: Result, reason?: RegExp): void {
  assert.equal(r.events.includes("prepare"), false, "no successor was prepared");
  assert.equal(r.models(), 0, "no model call");
  assert.ok(failedReports(r).length > 0, "the claim ended with the custody-retaining failed report");
  const retained = journal(r.bare, r.clone.branch);
  assert.equal(retained.clonePath, r.clone.path, "no successor replaces the predecessor");
  assert.equal(retained.recovery.stage, "blocked");
  assert.equal(retained.recovery.blocker, "quiescence_failed");
  assert.equal(fs.readFileSync(path.join(r.clone.path, "retained.txt"), "utf8"), "only local dirty work\n");
  assert.equal(pass(r)?.verdict, "stop");
  if (reason) assert.match(String(pass(r).stop_reason), reason);
}
function assertProceeded(r: Result): void {
  assert.ok(r.events.includes("prepare"), "the successor was prepared");
  assert.equal(r.models(), 1, "the successor reached its model");
  assert.notEqual(journal(r.bare, r.clone.branch).recovery?.blocker, "quiescence_failed");
}

// ── positive twin ────────────────────────────────────────────────────────────────────────────────────────────────────

it("positive twin: the real coordinator uploads, FINALs and acknowledges the predecessor generation before the successor", async () => {
  const r = await drive();
  assertProceeded(r);
  assert.deepEqual(r.events.filter(e => e.startsWith("operate:") || e === "prepare"), ["operate:2", "prepare"]);
  assert.equal(postsOf(r, "upload").length, 1);
  assert.equal(postsOf(r, "release").length, 1);
  assert.deepEqual([pass(r).verdict, pass(r).outcomes], ["proceed", ["2:acked"]]);
  const record = (await r.recovery.inspect(r.claim.run_id)).find(x => x.generation === 2)!;
  assert.equal(record.finalAcknowledged, true);
});

// ── helpers for the cases below ──────────────────────────────────────────────────────────────────────────────────────

const recoveryRoot = (): string => git.recoveryRoot;
const recordFile = (r: { claim: { run_id: string } }, record: RecoveryRecord): string =>
  path.join(recoveryRoot(), r.claim.run_id, record.captureId + ".json");
const journalFiles = (runId: string): Record<string, string> => {
  const dir = path.join(recoveryRoot(), runId);
  return Object.fromEntries(fs.readdirSync(dir).sort().map(f => [f, fs.readFileSync(path.join(dir, f)).toString("base64")]));
};
const savedRecord = async (r: { recovery: RecoveryCoordinator; claim: { run_id: string } }, generation: number): Promise<RecoveryRecord> =>
  (await r.recovery.inspect(r.claim.run_id)).find(x => x.generation === generation)!;
/** The held scope's deadline for the running pass: the iteration deadline in the journal minus R. */
function scopeDeadlineOf(world: Pre, prepMs: number): number {
  return journal(world.bare, world.clone.branch).recovery.deadline - (2 * CODEX_BOUNDARY_MS + prepMs);
}
const LATE_PREP_MS = 300_000 - 15_000 - 2 * CODEX_BOUNDARY_MS;
/** An abort reason as the platform raises it, for a transport that settles by rejecting. */
const abortError = (signal: AbortSignal | null | undefined): unknown => signal?.reason ?? new DOMException("aborted", "AbortError");

// ── D3 transport ─────────────────────────────────────────────────────────────────────────────────────────────────────

it("headers never arrive: the cap aborts the request at capDeadline, the fetch settles via its signal, clean skip", async () => {
  let responseClosed = false;
  const r = await drive({
    budget: { actionCapMs: 2_500 },
    arrange: server => {
      server.hooks.reserve = ctx => { ctx.res.on("close", () => { responseClosed = true; }); return true; };
    },
  });
  assertProceeded(r);
  assert.equal(pass(r).capped, true, "the action cap fired");
  assert.deepEqual([pass(r).verdict, pass(r).operated], ["proceed", 1]);
  assert.equal(responseClosed, true, "the abort closed the connection the api was holding");
  assert.equal(postsOf(r, "reserve").length, 1);
  assert.equal(postsOf(r, "upload").length + postsOf(r, "release").length, 0, "nothing past the stalled request was sent");
  const record = await savedRecord(r, 2);
  assert.notEqual(record.finalAcknowledged, true);
  assert.equal(record.finalRequest, undefined);
  assert.ok(pass(r).added_ms < pass(r).scope_ms, "the pass settled before the scope deadline");
});

it("late headers after the abort already settled: clean skip, and nothing the late answer carries is ever written", async () => {
  const seen: { before?: Record<string, string>; after?: Record<string, string>; lateAnswered?: boolean } = {};
  const r = await drive({
    budget: { actionCapMs: 2_500 },
    arrange: server => {
      server.hooks.reserve = async ctx => {
        await sleep(3_800);
        seen.lateAnswered = ctx.res.destroyed;
        return false; // the default reserve answer, written after the client gave up
      };
    },
    duringModel: async world => {
      seen.before = journalFiles(world.claim.run_id);
      await sleep(2_500);
      seen.after = journalFiles(world.claim.run_id);
    },
  });
  assertProceeded(r);
  assert.equal(pass(r).capped, true);
  assert.equal(seen.lateAnswered, true, "the api's late answer reached a connection the client had already closed");
  assert.equal(postsOf(r, "reserve").length, 1, "the late answer did not trigger a follow-up request");
  assert.equal(postsOf(r, "upload").length, 0);
  assert.deepEqual(seen.after, seen.before, "the journal is byte-identical before and after the late headers");
});

it("an abort that settles only after the scope deadline keeps the flight held while pending, then stops (quiescence_failed)", async () => {
  const seen: { events?: string[]; aborted?: boolean; pastScope?: boolean } = {};
  const r = await drive({
    budget: { successorPrepAllowanceMs: LATE_PREP_MS, settlementMarginMs: 500 },
    transport: async (kind, init, next, world) => {
      if (kind !== "reserve") return next();
      const scopeDeadline = scopeDeadlineOf(world, LATE_PREP_MS);
      // An uncooperative transport: it ignores the signal until the scope deadline is behind it.
      await sleep(Math.max(0, scopeDeadline - Date.now()) + 300);
      seen.events = [...world.events];
      seen.aborted = init?.signal?.aborted;
      seen.pastScope = Date.now() > scopeDeadline;
      throw abortError(init?.signal);
    },
  });
  assert.equal(seen.aborted, true, "the cap had aborted the request long before it settled");
  assert.equal(seen.pastScope, true);
  assert.equal(seen.events?.includes("prepare"), false, "no successor while the aborted request was still pending");
  assert.equal(seen.events?.includes("model"), false);
  assertStopped(r, /settlement_late/);
  assert.equal(pass(r).capped, true);
});

it("an abort that never settles leaves the flight held with no successor; cancelling the flight cleans it up", async () => {
  let release!: () => void;
  const gate = new Promise<void>(resolve => { release = resolve; });
  let entered = false;
  let world!: Pre;
  const running = drive({
    budget: { actionCapMs: 600 },
    arrange: (_server, w) => { world = w; },
    transport: async (kind, init, next) => {
      if (kind !== "reserve") return next();
      entered = true;
      await gate; // never resolved by the cap abort
      throw abortError(init?.signal);
    },
  });
  try {
    const startedAt = Date.now();
    while (!entered) {
      if (Date.now() - startedAt > 60_000) assert.fail("the reserve request was never dispatched");
      await sleep(25);
    }
    await sleep(600 + 2_500);
    assert.equal(world.events.includes("prepare"), false, "no successor within a bounded wait after the cap");
    assert.equal(world.events.includes("model"), false);
    assert.equal(world.events.filter(e => e.startsWith("operate:")).length, 1);
  } finally {
    world?.runner.shutdown();
    release();
  }
  const r = await running;
  assert.equal(r.events.includes("prepare"), false, "the cancelled flight never created a successor");
  assert.equal(r.models(), 0);
});

it("the api commits the FINAL and the response stalls: the saved request stays journaled and an ordinary replay recovers the ACK", async () => {
  const r = await drive({
    budget: { actionCapMs: 3_500 },
    arrange: server => {
      server.hooks.release = ctx => {
        const request = JSON.parse(ctx.body.toString("utf8"));
        server.released.add(ctx.generation!); // the api committed the release ...
        server.receipts.set(ctx.generation!, request.final_disposition);
        return true; // ... and never answers
      };
    },
  });
  assertStopped(r, /boundary_exception/);
  assert.equal(pass(r).capped, true);
  assert.equal(postsOf(r, "release").length, 1);
  const stalled = await savedRecord(r, 2);
  assert.equal(stalled.finalRequest?.disposition.kind, "archive", "the exact FINAL request was journaled before the POST");
  assert.notEqual(stalled.finalAcknowledged, true);
  const saved = structuredClone(stalled.finalRequest);
  // The ordinary re-drive (the flight is over) replays exactly that request and learns the ACK.
  r.server.hooks.release = undefined;
  await r.recovery.resumePending(undefined, [stalled]);
  const bodies = r.server.releaseBodies.filter(b => b.generation === 2);
  assert.ok(bodies.length >= 2, "the api saw the replay");
  assert.deepEqual(bodies.at(-1)!.body.final_disposition, saved!.disposition);
  const after = (await r.recovery.inspect(r.claim.run_id)).find(x => x.generation === 2);
  assert.ok(after === undefined || after.finalAcknowledged === true, "the replay's ACK was journaled (or the acknowledged record was retired)");
});

// ── integrity failures through the real coordinator ──────────────────────────────────────────────────────────────────

/**
 * An integrity failure of the named kind, armed on the fake api or in the journal so it strikes inside the real operation.
 * Not covered here: local_bytes_unverified. Only a THIN archive's mint re-verifies the journaled payload, and the guarded
 * real-git producer makes a thin archive only past its size ceiling (produceRecoveryBundle takes a forge tip, which a
 * credential-free guarded capture never has); recovery-predecessor-final.test.ts covers that code against a thin fake producer.
 * A flipped payload byte on an already uploaded SELF-CONTAINED archive does not block its FINAL (the api holds the verified bytes).
 */
/** Rewrites a journal record with the coordinator's own MAC: the harness's stand-in for state an earlier pass produced. */
function patchRecord(world: Pre, record: RecoveryRecord, changes: Partial<RecoveryRecord>): void {
  const file = recordFile(world, record);
  const envelope = JSON.parse(fs.readFileSync(file, "utf8"));
  delete envelope.mac;
  Object.assign(envelope, changes);
  const mac = (world.recovery as unknown as { computeMac(r: unknown): string }).computeMac(envelope);
  fs.writeFileSync(file, JSON.stringify({ ...envelope, mac }));
}
/** The state generation 2's own retained claim left: an uploaded self-contained archive that has no FINAL request yet. */
async function uploadedWithoutFinal(world: Pre): Promise<RecoveryRecord> {
  const record = await savedRecord(world, 2);
  const outcome = await world.recovery.captureAndUpload({ record, barePath: world.bare, defaultBranch: "main" });
  assert.equal(outcome.state, "uploaded", "fixture: the archive uploaded");
  patchRecord(world, await savedRecord(world, 2), { finalRequest: undefined, finalAcknowledged: undefined, holdSettlement: undefined });
  world.server.released.clear(); world.server.receipts.clear(); world.server.releaseBodies.length = 0; world.server.hits.length = 0;
  const uploaded = await savedRecord(world, 2);
  assert.equal(uploaded.selfContained, true);
  assert.equal(uploaded.finalRequest, undefined);
  return uploaded;
}

const integrity: Array<[string, RegExp, Pick<Opts, "prestate" | "arrange">, ((server: Server, world: () => Pre) => void)?]> = [
  ["bundle header corruption", /integrity:bundle_header/, {
    prestate: async world => { fs.writeFileSync((await uploadedWithoutFinal(world)).bundlePath!, "not a bundle"); },
  }],
  ["a record removed after the observation selected it", /integrity:record_removed/, {}, (server, world) => {
    server.hooks.exact = async ctx => {
      if (ctx.generation !== 2) return false;
      fs.rmSync(recordFile(world(), await savedRecord(world(), 2)));
      return false;
    };
  }],
  ["auth loss on the record after selection", /integrity:record_unauthenticated/, {}, (server, world) => {
    server.hooks.exact = async ctx => {
      if (ctx.generation !== 2) return false;
      fs.writeFileSync(recordFile(world(), await savedRecord(world(), 2)), "{}");
      return false;
    };
  }],
];

for (const [name, reason, setup, arm] of integrity) {
  it(`integrity (${name}) through the real coordinator stops with custody retained and no successor`, async () => {
    let world!: Pre;
    const r = await drive({ ...setup, arrange: (server, w) => { world = w; arm?.(server, () => world); } });
    assertStopped(r, reason);
    assert.equal(postsOf(r, "release").length, 0, "no FINAL after the integrity failure");
    assert.equal(r.server.released.has(2), false, "the api's hold is still open");
    assert.equal(r.events.filter(e => e.startsWith("operate:")).length, 1);
  });
}

it("an integrity failure combined with a typed quota error on another candidate still stops", async () => {
  let world!: Pre;
  const r = await drive({
    generations: [1, 2],
    arrange: (server, w) => {
      world = w;
      server.uploadRefusal = generation => generation === 1
        ? { status: 507, body: JSON.stringify({ error: "quota exceeded", reason: "quota" }) } : undefined;
      server.hooks.exact = async ctx => {
        if (ctx.generation !== 2) return false;
        fs.rmSync(recordFile(world, await savedRecord(world, 2)));
        return false;
      };
    },
  });
  assert.deepEqual(r.events.filter(e => e.startsWith("operate:")), ["operate:1", "operate:2"],
    "the quota refusal of generation 1 is a clean skip: generation 2 is still attempted");
  assert.equal(r.operated[0]!.outcome?.reason, "storage_quota_exceeded");
  assertStopped(r, /integrity:record_removed/);
});

// ── ordinary refusals through the real coordinator ───────────────────────────────────────────────────────────────────

it("a failed grant-2 observation (the api's current claim generation equals the hold's) is a clean skip with zero upload and FINAL POSTs", async () => {
  const r = await drive({ arrange: server => { server.currentGeneration = generation => generation; } });
  assertProceeded(r);
  assert.equal(r.operated[0]!.outcome?.reason, "predecessor_observation_refused");
  assert.equal(postsOf(r, "exact").length > 0, true, "the observation was made against the api");
  assert.deepEqual([postsOf(r, "reserve").length, postsOf(r, "upload").length, postsOf(r, "release").length], [0, 0, 0]);
  assert.deepEqual([pass(r).verdict, pass(r).outcomes], ["proceed", ["2:kept/predecessor_observation_refused"]]);
});

it("a typed quota error (the api's quota reason) is a clean skip, journaled once, never retried", async () => {
  const r = await drive({
    arrange: server => { server.uploadRefusal = () => ({ status: 507, body: JSON.stringify({ error: "quota exceeded", reason: "quota" }) }); },
  });
  assertProceeded(r);
  assert.equal(postsOf(r, "upload").length, 1, "one dispatch, no retry");
  assert.equal(postsOf(r, "release").length, 0);
  assert.equal(pass(r).verdict, "proceed");
  assert.equal(r.operated[0]!.outcome?.reason, "storage_quota_exceeded");
  assert.match(String(pass(r).outcomes[0]), /storage_quota_exceeded/);
  const record = await savedRecord(r, 2);
  assert.equal(record.archiveRefusal, "quota");
  assert.equal(record.reason, "storage_quota_exceeded");
});

it("a bare 507 is not a quota refusal: it is the transient capture_error path, with one dispatch", async () => {
  const r = await drive({ arrange: server => { server.uploadRefusal = () => ({ status: 507, body: "Insufficient Storage" }); } });
  assertProceeded(r);
  assert.equal(pass(r).verdict, "proceed", "the transient failure is an ordinary skip: the predecessor hold stays unsettled for the ordinary pass");
  assert.equal(postsOf(r, "upload").length, 1);
  assert.equal(r.operated[0]!.outcome?.reason, "capture_error");
  const record = await savedRecord(r, 2);
  assert.equal(record.archiveRefusal, undefined);
  assert.equal(record.reason, "capture_error");
});

// ── deferred cleanup ─────────────────────────────────────────────────────────────────────────────────────────────────

it("after the flight the ordinary pass retires the acknowledged predecessor record; while the run executes it does not", async () => {
  const seen: { during?: RecoveryRecord | undefined } = {};
  const r = await drive({
    duringModel: async world => {
      const record = (await world.recovery.inspect(world.claim.run_id)).find(x => x.generation === 2);
      await world.recovery.resumePending(undefined, record ? [record] : []);
      seen.during = (await world.recovery.inspect(world.claim.run_id)).find(x => x.generation === 2);
    },
  });
  assertProceeded(r);
  assert.equal(seen.during?.finalAcknowledged, true, "while the run executes the acknowledged record is kept (its source is protected)");
  const acked = (await r.recovery.inspect(r.claim.run_id)).find(x => x.generation === 2);
  assert.equal(acked?.finalAcknowledged, true, "the flight's end retired nothing by itself");
  await r.recovery.resumePending(undefined, acked ? [acked] : []);
  const after = (await r.recovery.inspect(r.claim.run_id)).find(x => x.generation === 2);
  assert.equal(after, undefined, "the ordinary re-drive retired the acknowledged generation");
});
