import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import fsp from "node:fs/promises";
import net from "node:net";
import path from "node:path";
import { afterEach, test } from "node:test";
import { setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { makeFakeProcRoot, plantInScope } from "./fake-proc.js";
import type { RecoveryExactHold } from "../src/protocol.js";
import { recordingLogger } from "./helpers.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import {
  EPISODE_ATTEMPT, EPISODE_GENERATION, OTHER_RUN, RUN, allRefs, closedHold, createWorld, gitIn, treeDigest, type GenInfo, type World,
} from "./fixtures/recovery-local-record-e2e-world.js";

// issue #3885 M2 end-to-end regression. ONE data dir is shared by a real GitCache, the real RunRunner (with the real
// RecoveryCoordinator it builds) and the real RetainedSourceServer over its Unix socket; the operator speaks to it
// through the socket (the self-contained .mjs client for the local-record verbs, raw NDJSON for the rest).
// Only the API client (recorded) and the proc view are controlled seams.

const CLIENT_MJS = path.resolve(import.meta.dirname, "..", "src", "ops", "retained-source.mjs");
type Json = Record<string, any>;

const worlds: World[] = [];
const scratch: string[] = [];
const cleanups: Array<() => void> = [];
afterEach(async () => {
  restoreHermeticView();
  for (const undo of cleanups.splice(0)) undo();
  for (const w of worlds.splice(0)) await w.stop();
  for (const d of scratch.splice(0)) fs.rmSync(d, { recursive: true, force: true });
});
async function world(opts: Parameters<typeof createWorld>[0]): Promise<World> {
  const w = await createWorld(opts);
  worlds.push(w);
  return w;
}

/** One NDJSON request on a fresh connection, exactly as the client sends it. */
function send(sock: string, req: unknown): Promise<Json> {
  return new Promise((resolve, reject) => {
    const conn = net.createConnection(sock);
    const chunks: Buffer[] = [];
    conn.on("error", reject);
    conn.on("connect", () => conn.write(`${JSON.stringify(req)}\n`));
    conn.on("data", c => chunks.push(Buffer.from(c)));
    conn.on("end", () => { try { resolve(JSON.parse(Buffer.concat(chunks).toString("utf8")) as Json); } catch (err) { reject(err); } });
  });
}

/** The real .mjs client, output unparsed (async spawn: the server lives in this process, so the event loop must stay free). */
function operatorRaw(w: World, args: string[], extraEnv: Record<string, string> = {}):
  Promise<{ code: number | null; stdout: string; stderr: string }> {
  return new Promise((resolve, reject) => {
    const env = { ...process.env, ...extraEnv };
    delete env.NODE_TEST_CONTEXT;
    const child = spawn(process.execPath, [CLIENT_MJS, ...args, "--socket", w.sock], { env, stdio: ["ignore", "pipe", "pipe"] });
    let out = "", err = "";
    child.stdout.on("data", c => { out += c; });
    child.stderr.on("data", c => { err += c; });
    child.on("error", reject);
    child.on("close", code => resolve({ code, stdout: out, stderr: err }));
  });
}

async function operator(w: World, args: string[]): Promise<{ code: number | null; json: Json; stderr: string }> {
  const r = await operatorRaw(w, args);
  try { return { code: r.code, json: JSON.parse(r.stdout) as Json, stderr: r.stderr }; } catch (e) {
    throw new Error(`client output ${r.stdout} / ${r.stderr}: ${e}`);
  }
}

const inspectEpisode = async (w: World): Promise<Json> => {
  const r = await send(w.sock, { op: "inspect", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT });
  assert.equal(r.ok, true, JSON.stringify(r));
  assert.equal(r.sources.length, 1, JSON.stringify(r));
  return r.sources[0] as Json;
};
const localRecords = async (w: World, run = RUN): Promise<Json> => (await operator(w, ["local-records", "--run", run])).json;
const classes = (r: Json): Record<number, string> =>
  Object.fromEntries((r.result.generations as Json[]).map(g => [g.generation, g.classification]));
const digestOf = (r: Json, generation: number): string => {
  const g = (r.result.generations as Json[]).find(x => x.generation === generation);
  assert.match(g?.digest ?? "", /^[0-9a-f]{64}$/, JSON.stringify(r));
  return g!.digest;
};
const resolve = (w: World, generation: number, confirm: string, run = RUN, caseB = false) =>
  operator(w, ["resolve-local-record", "--run", run, "--generation", String(generation), "--confirm", confirm, ...(caseB ? ["--case-b"] : [])]);

const recoveryDir = (w: World, run = RUN): string => path.join(w.dir, "recovery", run);
/** The record and bundle files of one generation (read before it is retired). */
function filesOf(w: World, g: GenInfo): string[] {
  return fs.readdirSync(recoveryDir(w, g.runId)).filter(n => n.startsWith(g.record.captureId + ".")).map(n => path.join(recoveryDir(w, g.runId), n));
}
const refsOwnedBy = (w: World, g: GenInfo): string[] => allRefs(w.bare).filter(l =>
  l.startsWith(`refs/uzi-coverage/${g.runId}/${g.generation}/`) || l.startsWith(`refs/uzi-recovery-pin/${g.runId}/${g.generation} `));
/** Bounded first mismatch of two line lists. */
function sameLines(actual: string[], expected: string[], what: string): void {
  const n = Math.max(actual.length, expected.length);
  for (let i = 0; i < n; i++) if (actual[i] !== expected[i]) assert.fail(`${what}: first mismatch at ${i}: ${actual[i]} !== ${expected[i]} (${actual.length} vs ${expected.length} lines)`);
}
const without = (lines: string[], gone: string[]): string[] => lines.filter(l => !gone.includes(l));

test("a multi-generation run: local-records names each case, A1 then A2 retire, Case B and every unrelated byte stay", async () => {
  const w = await world({ gens: [{ generation: 7, kind: "ack" }, { generation: 8, kind: "replay" }, { generation: 9, kind: "caseB" }],
    sharedOwedPin: true, otherRun: true });
  const [a1, a2, b] = [w.gens.get(7)!, w.gens.get(8)!, w.gens.get(9)!];
  const other = w.other!;

  // 1. inspect: the retained protected attempt is named with its local records and the shared owed pin; the disposition is to resolve them.
  const before = await inspectEpisode(w);
  assert.equal(before.disposition.kind, "resolve_local_record", JSON.stringify(before.disposition));
  const recordGenerations = new Set((before.disposition.records as Json[]).filter(r => r.kind === "recovery" && r.generation !== null).map(r => r.generation));
  assert.deepEqual([...recordGenerations].sort(), [7, 8, 9]);
  assert.ok(before.consumers.some((c: string) => c.startsWith("owed_pins:")), before.consumers.join());
  assert.ok(before.consumers.includes("episode_pins:1"), before.consumers.join());
  assert.ok(before.consumers.includes("blocker:shared_owed_pin"), "the other run's owed pin of the same sha is named: " + before.consumers.join());
  assert.ok(w.other!.ownedPin && allRefs(w.bare).some(l => l.startsWith(other.ownedPin + " ")), "the shared pin exists");

  // 2. local-records (the real client): each generation by its own case.
  const described = await localRecords(w);
  assert.equal(described.ok, true);
  // Generation 9 is a Case B candidate by its records (the owner discarded its hold), but the other run's shared owed pin
  // makes its retained source not exclusive, so it is refused for source exclusivity and offers no digest or action.
  assert.deepEqual(classes(described), { 7: "case_a_ack", 8: "case_a_replay", 9: "source_not_exclusive:shared_owed_pin" });
  const digestA1 = digestOf(described, 7);
  digestOf(described, 8);
  assert.equal((described.result.generations as Json[]).find(g => g.generation === 9)?.digest, undefined, "generation 9 is refused for source exclusivity (source_not_exclusive:shared_owed_pin), so it offers no digest");

  // Everything that must not move: Case B's records and bundle, the other run's records and source, and every ref except the retired ones.
  const caseB = filesOf(w, b).map(f => [f, treeDigest(f)] as const);
  assert.ok(caseB.some(([f]) => f.endsWith(".bundle")) && caseB.some(([f]) => f.endsWith(".json")));
  const otherRecords = treeDigest(recoveryDir(w, OTHER_RUN));
  const otherSource = treeDigest(other.clonePath);
  const episodeTrees = [treeDigest(w.episode.sourcePath), treeDigest(w.episode.successorPath)];
  const refs0 = allRefs(w.bare);
  const a1Refs = refsOwnedBy(w, a1);
  assert.ok(a1Refs.length >= 2, "generation 7 owns coverage and recovery pins: " + a1Refs.join());
  const a1Files = filesOf(w, a1);

  // 3a. A1: the persisted ACK is retired with no release POST.
  const r1 = await resolve(w, 7, digestA1);
  assert.equal(r1.code, 0, r1.stderr);
  assert.deepEqual(r1.json.result, { kind: "retired", generation: 7, remaining: 2 });
  assert.equal(w.client.posts.length, 0, "the acknowledged generation never posts");
  for (const f of a1Files) assert.equal(fs.existsSync(f), false, f);
  sameLines(allRefs(w.bare), without(refs0, a1Refs), "refs after A1");

  // 3b. A2: the digest is fresh (the journal listing changed); exactly one release POST, then the cleanup.
  const refs1 = allRefs(w.bare);
  const a2Refs = refsOwnedBy(w, a2);
  assert.ok(a2Refs.length >= 2, a2Refs.join());
  const request = a2.record.finalRequest!;
  const described2 = await localRecords(w);
  assert.deepEqual(classes(described2), { 8: "case_a_replay", 9: "source_not_exclusive:shared_owed_pin" });
  const stale = await resolve(w, 8, digestA1);
  assert.equal(stale.code, 1);
  assert.deepEqual(stale.json.result, { kind: "refused", reason: "descriptor_changed" });
  assert.equal(w.client.posts.length, 0, "a stale digest posts nothing");
  const r2 = await resolve(w, 8, digestOf(described2, 8));
  assert.equal(r2.code, 0, r2.stderr + JSON.stringify(r2.json));
  assert.deepEqual(r2.json.result, { kind: "retired", generation: 8, remaining: 1 });
  assert.equal(w.client.posts.length, 1, "exactly one release POST");
  assert.deepEqual(w.client.posts[0], { runId: RUN, generation: 8, evidence: request.evidence, disposition: request.disposition });
  sameLines(allRefs(w.bare), without(refs1, a2Refs), "refs after A2");

  // 4. Case B remains: the shared owed pin still makes the source not exclusive, so the Case B disposition is refused by
  // name (with or without the action), and inspect still asks for local-record resolution.
  const after = await inspectEpisode(w);
  assert.equal(after.disposition.kind, "resolve_local_record", JSON.stringify(after.disposition));
  assert.deepEqual([...new Set((after.disposition.records as Json[]).filter(r => r.generation !== null).map(r => r.generation))], [9]);
  const left = await localRecords(w);
  assert.deepEqual(classes(left), { 9: "source_not_exclusive:shared_owed_pin" });
  const refusedB = await resolve(w, 9, "a".repeat(64));
  assert.deepEqual(refusedB.json.result, { kind: "refused", reason: "source_not_exclusive:shared_owed_pin" });
  const refusedBCaseB = await resolve(w, 9, "a".repeat(64), RUN, true);
  assert.equal(refusedBCaseB.code, 1);
  assert.deepEqual(refusedBCaseB.json.result, { kind: "refused", reason: "source_not_exclusive:shared_owed_pin" });
  assert.equal(w.client.posts.length, 1);
  assert.deepEqual(w.client.writes, ["release"], "the only api write of the whole test is generation 8's release POST");

  // 7. Byte equality of everything that was not retired.
  for (const [f, digest] of caseB) sameLines(treeDigest(f), digest, "Case B " + path.basename(f));
  sameLines(treeDigest(recoveryDir(w, OTHER_RUN)), otherRecords, "the other run's records");
  sameLines(treeDigest(other.clonePath), otherSource, "the other run's retained source");
  sameLines(treeDigest(w.episode.sourcePath), episodeTrees[0]!, "episode predecessor");
  sameLines(treeDigest(w.episode.successorPath), episodeTrees[1]!, "episode successor");
  const refs2 = allRefs(w.bare);
  assert.ok(refs2.some(l => l.startsWith(other.ownedPin + " ")), "the shared owed pin is untouched");
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-coverage/${RUN}/9/`)) && allRefs(w.bare).some(l => l.startsWith(`refs/uzi-recovery-pin/${RUN}/9 `)),
    "Case B keeps its pins");
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-coverage/${OTHER_RUN}/3/`)), "the other run keeps its pins");
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-owed/${RUN}/${w.episode.tip} `)), "the episode's owed pin is intact");
});

// ─── cooperative deadline and shutdown through the REAL coordinator ─────────────────────────────────────────────────

interface Delay { reached: Promise<void>; release: () => void; restore: () => void }
/** Wraps the `fs.promises.unlink` the coordinator's cleanup uses: the FIRST unlink of one of `files` signals `reached` and
 *  then waits for `release()`; every other path passes straight through. Restored by `restore()` and, as a backstop, afterEach. */
function delayFirstUnlinkOf(files: string[]): Delay {
  const original = fsp.unlink;
  const targets = new Set(files);
  let hit = false;
  let reachedResolve!: () => void;
  let releaseResolve!: () => void;
  const reached = new Promise<void>(r => { reachedResolve = r; });
  const released = new Promise<void>(r => { releaseResolve = r; });
  (fsp as { unlink: typeof fsp.unlink }).unlink = async (p: Parameters<typeof fsp.unlink>[0]) => {
    if (!hit && typeof p === "string" && targets.has(p)) {
      hit = true;
      reachedResolve();
      await released;
    }
    return original.call(fsp, p);
  };
  const restore = (): void => { (fsp as { unlink: typeof fsp.unlink }).unlink = original; releaseResolve(); };
  cleanups.push(restore);
  return { reached, release: releaseResolve, restore };
}

async function within<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  let timer: NodeJS.Timeout | undefined;
  try {
    return await Promise.race([p, new Promise<never>((_, rej) => { timer = setTimeout(() => rej(new Error(`timed out waiting for ${what}`)), ms); })]);
  } finally { clearTimeout(timer); }
}
async function until(cond: () => boolean, ms: number, what: string): Promise<void> {
  const end = Date.now() + ms;
  while (!cond()) {
    if (Date.now() > end) throw new Error(`timed out waiting for ${what}`);
    await new Promise(r => setTimeout(r, 20));
  }
}
const finishedLines = (lines: unknown[]): Json[] =>
  (lines as Json[]).filter(l => l.msg === "retained-source: local record resolution finished");

const DEADLINE_MS = 4000;
const CLIENT_IDLE_MS = 1500;
/** The Case B deadline: generous, so no phase before the delayed unlink can exhaust it on a loaded host; the test then waits it out. */
const CASE_B_DEADLINE_MS = 12_000;

test("a resolution that outlasts the server deadline and the client idle timeout: unknown to the client, finished in the log, gate closed until the delayed unlink settles", async () => {
  const { logger, lines } = recordingLogger();
  const w = await world({ gens: [{ generation: 7, kind: "ack" }, { generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true,
    serverLog: logger, serverTimeouts: { resolveDeadlineMs: DEADLINE_MS } });
  const [a1, b] = [w.gens.get(7)!, w.gens.get(9)!];
  const described = await localRecords(w);
  const digest0 = digestOf(described, 7);
  const a1Files = filesOf(w, a1);
  const a1Refs = refsOwnedBy(w, a1);
  assert.ok(a1Files.length >= 2 && a1Refs.length >= 2);
  const caseB = filesOf(w, b).map(f => [f, treeDigest(f)] as const);
  const otherRecords = treeDigest(recoveryDir(w, OTHER_RUN));
  const otherSource = treeDigest(w.other!.clonePath);
  const refs0 = allRefs(w.bare);

  const delay = delayFirstUnlinkOf(a1Files);
  const t0 = Date.now();
  const client = operatorRaw(w, ["resolve-local-record", "--run", RUN, "--generation", "7", "--confirm", digest0],
    { UZI_RETAINED_SOURCE_IDLE_TIMEOUT_MS: String(CLIENT_IDLE_MS) });
  try {
    await within(delay.reached, 30_000, "the delayed unlink");

    // While the unlink is pending: gate closed, a second resolution refused busy, the generation partly present.
    assert.equal(w.gate.heldBy("retained_ops"), true);
    assert.equal(w.gate.tryEnterClaim(), false, "claims are refused");
    const second = await send(w.sock, { op: "resolve-local-record", run: RUN, generation: 9, confirm: "a".repeat(64) });
    assert.deepEqual(second.result, { kind: "refused", reason: "busy" });
    assert.deepEqual(refsOwnedBy(w, a1), [], "the refs went before the file removal");
    for (const f of a1Files) assert.equal(fs.existsSync(f), true, "no file is removed yet: " + f);

    // (1) The client gives up first, with the unknown-outcome message and no result.
    const out = await within(client, 30_000, "the client to time out");
    assert.equal(out.code, 1, out.stderr);
    assert.equal(out.stdout, "");
    assert.match(out.stderr, /outcome is unknown/);
    assert.ok(Date.now() - t0 < DEADLINE_MS, "the client's idle timeout fired before the server deadline");

    // Let the server's deadline pass too; still nothing is logged and the gate stays closed (the operation is owned).
    await new Promise(r => setTimeout(r, Math.max(0, t0 + DEADLINE_MS + 300 - Date.now())));
    assert.equal(finishedLines(lines).length, 0, "not finished while the operation is pending");
    assert.equal(w.gate.heldBy("retained_ops"), true, "a spent deadline does not reopen the gate");
    assert.equal(w.gate.tryEnterClaim(), false);

    // (3)/(2) The operation settles: only then does the gate reopen, and the server logs the completion.
    delay.release();
    await until(() => finishedLines(lines).length === 1, 30_000, "the finished log line");
    const fin = finishedLines(lines)[0]!;
    assert.equal(fin.run_id, RUN);
    assert.equal(fin.generation, 7);
    assert.equal(fin.outcome, "partial", JSON.stringify(fin));
    assert.equal(fin.reason, "deadline_exhausted", JSON.stringify(fin));
    await until(() => !w.gate.heldBy("retained_ops"), 10_000, "the gate to reopen");
    assert.equal(w.gate.tryEnterClaim(), true);
    w.gate.exitClaim();
  } finally { delay.restore(); }

  // (4) A fresh view shows the post-mutation state: the bundle is gone, the descriptor changed, and inspect remembers the outcome.
  assert.equal(fs.existsSync(a1Files.find(f => f.endsWith(".bundle"))!), false);
  const fresh = await localRecords(w);
  const g7 = (fresh.result.generations as Json[]).find(g => g.generation === 7);
  assert.ok(g7, "the partly removed generation is still described: " + JSON.stringify(fresh));
  assert.notEqual(g7.digest, digest0, "the digest moved with the removed bundle");
  assert.equal((await inspectEpisode(w)).disposition.kind, "resolve_local_record");
  const last = (await send(w.sock, { op: "inspect", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT })).lastDiscards as Json[];
  assert.ok(last.some(d => d.generation === 7 && d.outcome === "partial" && d.reason === "deadline_exhausted"), JSON.stringify(last));

  // The stale digest is refused; the fresh one completes the retirement under a new deadline.
  const stale = await resolve(w, 7, digest0);
  assert.deepEqual(stale.json.result, { kind: "refused", reason: "descriptor_changed" });
  const done = await resolve(w, 7, g7.digest);
  assert.equal(done.code, 0, done.stderr + JSON.stringify(done.json));
  assert.equal(done.json.result.kind, "retired", JSON.stringify(done.json));
  for (const f of a1Files) assert.equal(fs.existsSync(f), false, f);
  sameLines(allRefs(w.bare), without(refs0, a1Refs), "refs after the retirement");
  for (const [f, d] of caseB) sameLines(treeDigest(f), d, "Case B " + path.basename(f));
  sameLines(treeDigest(recoveryDir(w, OTHER_RUN)), otherRecords, "the other run's records");
  sameLines(treeDigest(w.other!.clonePath), otherSource, "the other run's retained source");
});

test("close() during a delayed unlink does not reopen the claim gate before the operation settles", async () => {
  const { logger, lines } = recordingLogger();
  const w = await world({ gens: [{ generation: 7, kind: "ack" }, { generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true,
    serverLog: logger, serverTimeouts: { shutdownSettleMs: 250 } });
  const digest0 = digestOf(await localRecords(w), 7);
  const a1Files = filesOf(w, w.gens.get(7)!);
  const caseB = filesOf(w, w.gens.get(9)!).map(f => [f, treeDigest(f)] as const);
  const otherRecords = treeDigest(recoveryDir(w, OTHER_RUN));

  const delay = delayFirstUnlinkOf(a1Files);
  const client = operatorRaw(w, ["resolve-local-record", "--run", RUN, "--generation", "7", "--confirm", digest0]);
  try {
    await within(delay.reached, 30_000, "the delayed unlink");
    assert.equal(w.gate.heldBy("retained_ops"), true);

    // close() returns after its bounded settle wait, but neither settles the operation nor reopens the gate.
    const t = Date.now();
    await within(w.server.close(), 10_000, "close()");
    assert.ok(Date.now() - t >= 200, "close() waited for the in-flight operation (bounded)");
    assert.equal(w.gate.heldBy("retained_ops"), true, "close() did not reopen the gate");
    assert.equal(w.gate.tryEnterClaim(), false);
    assert.equal(finishedLines(lines).length, 0);
    for (const f of a1Files) assert.equal(fs.existsSync(f), true, f);
    const gone = await within(client, 30_000, "the client to see the connection close");
    assert.notEqual(gone.code, 0);
    assert.equal(gone.stdout, "");

    delay.release();
    await until(() => finishedLines(lines).length === 1, 30_000, "the finished log line");
    const fin = finishedLines(lines)[0]!;
    assert.equal(fin.generation, 7);
    assert.notEqual(fin.outcome, "retired", "the shutdown ended the resolution early: " + JSON.stringify(fin));
    await until(() => !w.gate.heldBy("retained_ops"), 10_000, "the gate to reopen after settlement");
  } finally { delay.restore(); }

  // The unlink that was pending completed (the work is owned); nothing unrelated moved.
  assert.equal(fs.existsSync(a1Files.find(f => f.endsWith(".bundle"))!), false);
  const left = (await w.coordinator.inspect(RUN)).filter(r => r.generation === 7 || r.generation === 9);
  assert.ok(left.some(r => r.generation === 9));
  for (const [f, d] of caseB) sameLines(treeDigest(f), d, "Case B " + path.basename(f));
  sameLines(treeDigest(recoveryDir(w, OTHER_RUN)), otherRecords, "the other run's records");
});

/** Resolve every generation of the run through the operator verbs (a fresh digest each time: the listing changes). */
async function resolveAll(w: World, generations: number[]): Promise<void> {
  for (const generation of generations) {
    const r = await resolve(w, generation, digestOf(await localRecords(w), generation));
    assert.equal(r.code, 0, r.stderr + JSON.stringify(r.json));
    assert.equal(r.json.result.kind, "retired", JSON.stringify(r.json));
  }
}

test("Case-A-only run: after A1 and A2 the attempt is a local_protected_discard; consumers refuse it and the protected discard is process-gated", async () => {
  const w = await world({ gens: [{ generation: 7, kind: "ack" }, { generation: 8, kind: "replay" }], sharedOwedPin: false, otherRun: true });
  const other = w.other!;
  const before = await inspectEpisode(w);
  assert.equal(before.disposition.kind, "resolve_local_record");

  await resolveAll(w, [7, 8]);
  assert.equal(w.client.posts.length, 1, "only the replayed generation posted");
  assert.equal(fs.existsSync(recoveryDir(w)), false, "the run's record directory is gone");
  assert.deepEqual((await localRecords(w)).result, { kind: "described", runId: RUN, generations: [], blockers: [] });

  // 5. Fresh inspect: nothing left to resolve.
  const fresh = await inspectEpisode(w);
  assert.deepEqual(fresh.disposition, { kind: "local_protected_discard" });
  assert.deepEqual(fresh.localRecords, []);
  assert.equal(fresh.kind, "protected");

  // 8a. A successor journal naming the source: the fresh inspect refuses with the consumer, and so does the discard.
  const journalKey = "uzi-recovery.agent/other.clone";
  gitIn(w.bare, ["config", "--local", journalKey, JSON.stringify({ runId: "run-other", clonePath: w.episode.sourcePath })]);
  const named = await inspectEpisode(w);
  assert.equal(named.disposition.kind, "refused", JSON.stringify(named.disposition));
  assert.ok((named.disposition.consumers as string[]).some(c => c.startsWith("named_by_other_journal")), JSON.stringify(named.disposition));
  const namedDiscard = await send(w.sock, { op: "discard", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT, confirm: named.descriptor, protected: true });
  assert.equal(namedDiscard.ok, false);
  assert.equal(namedDiscard.result.kind, "refused", JSON.stringify(namedDiscard));
  assert.ok(fs.existsSync(w.episode.sourcePath) && fs.existsSync(w.episode.successorPath), "the source is kept");
  gitIn(w.bare, ["config", "--local", "--unset-all", journalKey]);
  const again = await inspectEpisode(w);
  assert.deepEqual(again.disposition, { kind: "local_protected_discard" });

  // 6. A process attributed to the attempt: the protected discard is refused and nothing moves.
  const otherConfig = (): string[] => gitIn(w.bare, ["config", "--local", "--list"]).split("\n").filter(l => l.includes("issue-2512") || l.includes(OTHER_RUN));
  const refs0 = allRefs(w.bare);
  const trees0 = [treeDigest(w.episode.sourcePath), treeDigest(w.episode.successorPath)];
  const otherRecords0 = treeDigest(path.join(w.dir, "recovery", OTHER_RUN));
  const otherSource0 = treeDigest(other.clonePath);
  const otherConfig0 = otherConfig();
  assert.ok(otherConfig0.length > 0);
  const proc = makeFakeProcRoot();
  scratch.push(proc);
  plantInScope(proc, 4242, w.episode.successorPath);
  setQuiescenceViewForTests({ procRoot: proc });
  const gated = await send(w.sock, { op: "discard", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT, confirm: again.descriptor, protected: true });
  assert.equal(gated.ok, false, JSON.stringify(gated));
  assert.equal(gated.result.kind, "refused", JSON.stringify(gated));
  assert.equal(gated.result.reason, "not_quiescent", JSON.stringify(gated));
  assert.deepEqual(gated.result.consumers, ["writers_not_proven_gone"], JSON.stringify(gated));
  sameLines(allRefs(w.bare), refs0, "refs after the gated discard");
  sameLines(treeDigest(w.episode.sourcePath), trees0[0]!, "predecessor after the gated discard");
  sameLines(treeDigest(w.episode.successorPath), trees0[1]!, "successor after the gated discard");
  assert.deepEqual((await inspectEpisode(w)).disposition, { kind: "local_protected_discard" }, "the source is still offered");

  // The hermetic (empty) view again: the same attempt, with a fresh descriptor, is discarded.
  restoreHermeticView();
  const ready = await inspectEpisode(w);
  const pin = `refs/uzi-recovery-episode/${RUN}/${w.episode.tip}`;
  assert.ok(refs0.some(l => l.startsWith(pin + " ")), "the episode pin exists before the discard");
  const done = await send(w.sock, { op: "discard", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT, confirm: ready.descriptor, protected: true });
  assert.equal(done.ok, true, JSON.stringify(done));
  assert.equal(done.result.kind, "done", JSON.stringify(done));
  assert.equal(fs.existsSync(w.episode.sourcePath), false);
  assert.equal(fs.existsSync(w.episode.successorPath), false);
  const gone = await send(w.sock, { op: "inspect", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT });
  assert.equal(gone.ok, true);
  assert.deepEqual(gone.sources, []);
  assert.equal(gitIn(w.bare, ["config", "--local", "--list"]).split("\n").some(l => l.startsWith(`uzi-retained.${RUN}.`)), false, "the descriptor is gone");

  // 7. Exactly the discarded attempt's episode pin left; the owed pin, the other run's refs, records and source are byte-identical.
  sameLines(allRefs(w.bare), without(refs0, refs0.filter(l => l.startsWith(pin + " "))), "refs after the discard");
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-owed/${RUN}/${w.episode.tip} `)), "owed evidence is not the descriptor's to delete");
  sameLines(treeDigest(path.join(w.dir, "recovery", OTHER_RUN)), otherRecords0, "the other run's records");
  sameLines(treeDigest(other.clonePath), otherSource0, "the other run's retained source");
  sameLines(otherConfig(), otherConfig0, "the other run's journal, ledger and markers");
});

test("terminal protection and a settlement record keep a Case-A generation unresolved, then it retires once they are gone", async () => {
  const w = await world({ gens: [{ generation: 7, kind: "ack" }], sharedOwedPin: false, otherRun: false });
  w.flags.terminal = true;

  // inspect still wants record resolution, and now names terminal protection among the records.
  const ev = await inspectEpisode(w);
  assert.equal(ev.disposition.kind, "resolve_local_record", JSON.stringify(ev.disposition));
  const names = (ev.disposition.records as Json[]).map(r => `${r.kind}:${r.name}`);
  assert.ok(names.includes("terminal:terminal_protection"), names.join());
  const described = await localRecords(w);
  assert.deepEqual(classes(described), { 7: "terminal_protection" });
  assert.equal((described.result.generations as Json[])[0]!.digest, undefined);

  const refs0 = allRefs(w.bare), tree0 = treeDigest(recoveryDir(w));
  const refusedTerminal = await resolve(w, 7, "b".repeat(64));
  assert.equal(refusedTerminal.code, 1);
  assert.deepEqual(refusedTerminal.json.result, { kind: "refused", reason: "terminal_protection" });
  sameLines(allRefs(w.bare), refs0, "refs");
  sameLines(treeDigest(recoveryDir(w)), tree0, "records and bundle");
  assert.equal(w.client.posts.length, 0);

  // Terminal protection lifted; a settlement record under recovery-settlement/<run> is the next unsupported shape.
  w.flags.terminal = false;
  assert.deepEqual(classes(await localRecords(w)), { 7: "case_a_ack" });
  const settlementDir = path.join(w.dir, "recovery-settlement", RUN);
  fs.mkdirSync(settlementDir, { recursive: true });
  fs.writeFileSync(path.join(settlementDir, "hold-1.json"), JSON.stringify({ predecessorGeneration: 1 }));
  const settlement0 = treeDigest(settlementDir);
  assert.equal(settlement0.length, 2);
  const settled = await localRecords(w);
  assert.deepEqual(classes(settled), { 7: "settlement_record_unsupported" });
  assert.equal((settled.result.generations as Json[])[0]!.digest, undefined);
  const withSettlement = await inspectEpisode(w);
  assert.equal(withSettlement.disposition.kind, "resolve_local_record");
  assert.ok((withSettlement.disposition.records as Json[]).some(r => r.kind === "settlement"), JSON.stringify(withSettlement.disposition));
  const refusedSettlement = await resolve(w, 7, "c".repeat(64));
  assert.deepEqual(refusedSettlement.json.result, { kind: "refused", reason: "settlement_record_unsupported" });
  sameLines(allRefs(w.bare), refs0, "refs");
  sameLines(treeDigest(recoveryDir(w)), tree0, "records and bundle");
  sameLines(treeDigest(settlementDir), settlement0, "the settlement record itself");

  // Both consumers gone: the generation is resolvable again, and the attempt becomes a plain local_protected_discard.
  fs.rmSync(path.join(w.dir, "recovery-settlement"), { recursive: true });
  await resolveAll(w, [7]);
  assert.deepEqual((await inspectEpisode(w)).disposition, { kind: "local_protected_discard" });
});

// ─── Case B (issue #3885 m2): the owner-discarded generation's local records, through the real channel ───────────────

type Snap = Map<string, string>;
/** Every entry of the data directory (the Unix socket's own directory excluded) as path -> "type mode hash". */
function dataSnapshot(w: World): Snap {
  const m: Snap = new Map();
  for (const line of treeDigest(w.dir, rel => rel === "ops" || rel.startsWith("ops/"))) {
    const i = line.indexOf(" ");
    m.set(line.slice(0, i), line.slice(i + 1));
  }
  return m;
}
/** Bounded first mismatch of two snapshots. */
function sameSnap(actual: Snap, expected: Snap, what: string): void {
  for (const key of [...new Set([...actual.keys(), ...expected.keys()])].sort()) {
    if (actual.get(key) !== expected.get(key)) assert.fail(`${what}: first mismatch at ${key}: ${actual.get(key)} !== ${expected.get(key)} (${actual.size} vs ${expected.size} entries)`);
  }
}
/** `before` minus a generation's record and bundle files (read BEFORE the retirement: `files`) and, when set, the then-empty run directory. */
function minus(w: World, before: Snap, files: string[], runDirGone: boolean): Snap {
  const gone = new Set(files.map(f => path.relative(w.dir, f)));
  const dir = path.relative(w.dir, recoveryDir(w));
  return new Map([...before].filter(([k]) => !gone.has(k) && !(runDirGone && k === dir)));
}

/** Dirty and unpublished content in the run's retained predecessor clone: a modified tracked file, an untracked file and a
 *  commit the bare has never seen. Only the test writes it; the Case B disposition must never read or change it. */
function dirtySource(w: World): void {
  const src = w.episode.sourcePath;
  fs.writeFileSync(path.join(src, "unpushed.txt"), "an unpublished commit\n");
  gitIn(src, ["add", "unpushed.txt"]);
  gitIn(src, ["commit", "-m", "unpublished"]);
  fs.appendFileSync(path.join(src, "work.txt"), "modified after the commit\n");
  fs.writeFileSync(path.join(src, "untracked.txt"), "untracked bytes\n");
  const status = gitIn(src, ["status", "--porcelain"]);
  assert.ok(/^ ?M work\.txt$/m.test(status) && /^\?\? untracked\.txt$/m.test(status), status);
  const head = gitIn(src, ["rev-parse", "HEAD"]);
  assert.notEqual(head, w.episode.tip, "the clone is ahead of the retained episode tip");
  assert.throws(() => gitIn(w.bare, ["cat-file", "-e", `${head}^{commit}`]), "the unpublished commit is in no bare");
}

/** The state a Case B request may never move, as the unfiltered footprint plus the api's observed writes. */
function assertNoApiWrites(w: World, what: string): void {
  assert.deepEqual(w.client.writes, [], `${what}: no reserve, upload, release or held publication`);
  assert.deepEqual(w.client.posts, [], `${what}: no release, upload or ACK POST`);
}

interface Probe { e2Ms: number[]; jMs: Array<{ ms: number; underS: boolean }> }
/** Test-only timestamps around existing seams (the instance properties the tests already override); no production change. */
function instrument(w: World): Probe {
  const probe: Probe = { e2Ms: [], jMs: [] };
  const realInspect = w.git.inspectRetainedSources.bind(w.git);
  Object.assign(w.git, { inspectRetainedSources: async (...args: Parameters<typeof realInspect>) => {
    const t = performance.now();
    try { return await realInspect(...args); } finally { probe.e2Ms.push(performance.now() - t); }
  } });
  const c = w.coordinator as unknown as {
    withJournalLock: (...a: unknown[]) => Promise<unknown>;
    withInventorySourceBoundary: (...a: unknown[]) => Promise<unknown>;
  };
  let underS = 0;
  const realJ = c.withJournalLock.bind(c), realS = c.withInventorySourceBoundary.bind(c);
  Object.assign(c, {
    withJournalLock: async (...args: unknown[]) => {
      const t = performance.now(), s = underS > 0;
      try { return await realJ(...args); } finally { probe.jMs.push({ ms: performance.now() - t, underS: s }); }
    },
    withInventorySourceBoundary: async (...args: unknown[]) => {
      underS++;
      try { return await realS(...args); } finally { underS--; }
    },
  });
  return probe;
}
const ms = (n: number): string => n.toFixed(1);

test("Case B owner-discard: dirty and unpublished content stays, the generation's records and bundles retire, and nothing else moves (real RunRunner, server and CLI)", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await world({ gens: [{ generation: 7, kind: "ack" }, { generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true });
  const [a1, b] = [w.gens.get(7)!, w.gens.get(9)!];
  dirtySource(w);
  const probe = instrument(w);

  // The genuine #4002 shape: discarded by the owner, no final receipt, no live pointers, no open hold at any generation.
  const exact = b.exact!;
  assert.deepEqual([exact.state, exact.release_evidence, exact.final_receipt, exact.live_worker_id, exact.live_run_id],
    ["discarded", "owner_discard", undefined, undefined, undefined]);
  const holds = await w.client.listRecoveryHolds(RUN);
  assert.deepEqual([holds.holds.length, holds.run_open_holds?.count], [0, 0]);

  // inspect (E2): the dirty source carries its local records and no blocker of its own.
  const inspected = await inspectEpisode(w);
  assert.equal(inspected.disposition.kind, "resolve_local_record", JSON.stringify(inspected.disposition));
  assert.deepEqual((inspected.consumers as string[]).filter(c => c.startsWith("blocker:")), [], inspected.consumers.join());

  const t0 = performance.now();
  const described = await localRecords(w);
  const describeMs = performance.now() - t0;
  assert.equal(described.ok, true, JSON.stringify(described));
  assert.deepEqual(classes(described), { 7: "case_a_ack", 9: "case_b_owner_discard" });
  const g9 = (described.result.generations as Json[]).find(g => g.generation === 9)!;
  assert.match(g9.digest, /^[0-9a-f]{64}$/);
  assert.equal(g9.action, "case_b");
  assert.deepEqual(g9.laterCleanup, []);
  assert.equal(g9.holdId, b.holdId);

  const files = filesOf(w, b);
  assert.ok(files.some(f => f.endsWith(".bundle")) && files.some(f => f.endsWith(".json")));
  const before = dataSnapshot(w);
  const refs0 = allRefs(w.bare);
  const pins = refs0.filter(l => l.startsWith(`refs/uzi-coverage/${RUN}/9/`) || l.startsWith(`refs/uzi-recovery-pin/${RUN}/9 `) || l.startsWith("refs/uzi-owed/") ||
    l.startsWith("refs/uzi-recovery-episode/"));
  assert.ok(pins.length >= 4, pins.join());

  // Without the action the disposition is refused by name; Case B's action on a Case A generation is a mismatch. Nothing moves.
  const bare = await resolve(w, 9, g9.digest);
  assert.equal(bare.code, 1);
  assert.deepEqual(bare.json.result, { kind: "refused", reason: "case_b_disposition_required" });
  const mismatch = await resolve(w, 7, digestOf(described, 7), RUN, true);
  assert.equal(mismatch.code, 1);
  assert.deepEqual(mismatch.json.result, { kind: "refused", reason: "case_b_action_mismatch" });
  sameSnap(dataSnapshot(w), before, "after the refusals");
  assertNoApiWrites(w, "after the refusals");

  probe.e2Ms.length = 0;
  probe.jMs.length = 0;
  const t1 = performance.now();
  const retired = await resolve(w, 9, g9.digest, RUN, true);
  const resolveMs = performance.now() - t1;
  assert.equal(retired.code, 0, retired.stderr + JSON.stringify(retired.json));
  assert.deepEqual(retired.json.result, { kind: "retired", generation: 9, remaining: 1 });

  // The records and bundle of generation 9 are gone; everything else is byte-identical (tree, refs, config, journals, reports,
  // generation 7's records and bundle, the other run's records, bundles, refs and source).
  for (const f of files) assert.equal(fs.existsSync(f), false, f);
  sameSnap(dataSnapshot(w), minus(w, before, files, false), "after the Case B retirement");
  sameLines(allRefs(w.bare), refs0, "refs (pins, owed and episode refs included)");
  for (const f of filesOf(w, a1)) assert.equal(fs.existsSync(f), true, "generation 7 is untouched: " + f);
  assertNoApiWrites(w, "after the retirement");
  assert.equal(gitIn(w.episode.sourcePath, ["status", "--porcelain"]).split("\n").length, 2, "the dirty tree is still dirty");

  const e2 = probe.e2Ms.reduce((a, c) => Math.max(a, c), 0);
  const jUnderS = probe.jMs.filter(j => j.underS).reduce((a, c) => Math.max(a, c.ms), 0);
  console.log(`MEASURE case-b describe(local-records, CLI round trip)=${ms(describeMs)}ms resolve(CLI round trip)=${ms(resolveMs)}ms ` +
    `E2(inspectRetainedSources, per call max over ${probe.e2Ms.length} calls)=${ms(e2)}ms J-hold-under-S=${ms(jUnderS)}ms ` +
    `(J holds: ${probe.jMs.map(j => `${j.underS ? "S" : "-"}${ms(j.ms)}`).join(",")})`);
});

test("Case B on a dirty source is refused twice, each by name: a writer that cannot be proven gone, and a shared or foreign-named source", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  // 1. A process attributed to the retained attempt: the in-boundary writer proof fails.
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true });
  const b = w.gens.get(9)!;
  dirtySource(w);
  const described = await localRecords(w);
  assert.deepEqual(classes(described), { 9: "case_b_owner_discard" });
  const digest = digestOf(described, 9);
  const before = dataSnapshot(w);
  const files = filesOf(w, b);
  const proc = makeFakeProcRoot();
  scratch.push(proc);
  plantInScope(proc, 4242, w.episode.successorPath);
  setQuiescenceViewForTests({ procRoot: proc });
  const gated = await resolve(w, 9, digest, RUN, true);
  assert.equal(gated.code, 1);
  assert.equal(gated.json.result.kind, "refused", JSON.stringify(gated.json));
  assert.equal(gated.json.result.reason, "inventory_source_not_quiescent:process_not_quiescent_before_git", JSON.stringify(gated.json));
  sameSnap(dataSnapshot(w), before, "after the writer-proof refusal");
  assertNoApiWrites(w, "writer-proof refusal");

  // The proc view lifted, the very same confirmation completes: the refusal was the writer proof and nothing else.
  restoreHermeticView();
  const done = await resolve(w, 9, digest, RUN, true);
  assert.equal(done.code, 0, done.stderr + JSON.stringify(done.json));
  assert.deepEqual(done.json.result, { kind: "retired", generation: 9, remaining: 0 });
  sameSnap(dataSnapshot(w), minus(w, before, files, true), "after the retirement");

  // 2a. A shared owed pin (the other run owes the same sha): the source is not exclusive.
  const shared = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: true, otherRun: true });
  dirtySource(shared);
  const sharedView = await localRecords(shared);
  assert.deepEqual(classes(sharedView), { 9: "source_not_exclusive:shared_owed_pin" });
  assert.equal((sharedView.result.generations as Json[])[0]!.digest, undefined);
  const sharedBefore = dataSnapshot(shared);
  const refusedShared = await resolve(shared, 9, "a".repeat(64), RUN, true);
  assert.equal(refusedShared.code, 1);
  assert.deepEqual(refusedShared.json.result, { kind: "refused", reason: "source_not_exclusive:shared_owed_pin" });
  sameSnap(dataSnapshot(shared), sharedBefore, "after the shared-pin refusal");
  assertNoApiWrites(shared, "shared-pin refusal");

  // 2b. A foreign journal that names the retained clone path.
  const foreign = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true });
  dirtySource(foreign);
  gitIn(foreign.bare, ["config", "--local", "uzi-recovery.agent/other.clone", JSON.stringify({ runId: "run-other", clonePath: foreign.episode.sourcePath })]);
  const foreignView = await localRecords(foreign);
  assert.deepEqual(classes(foreignView), { 9: "source_not_exclusive:named_by_other_journal:active:agent_other:run-other" });
  const foreignBefore = dataSnapshot(foreign);
  const refusedForeign = await resolve(foreign, 9, "b".repeat(64), RUN, true);
  assert.equal(refusedForeign.code, 1);
  assert.deepEqual(refusedForeign.json.result, { kind: "refused", reason: "source_not_exclusive:named_by_other_journal:active:agent_other:run-other" });
  sameSnap(dataSnapshot(foreign), foreignBefore, "after the foreign-journal refusal");
  assertNoApiWrites(foreign, "foreign-journal refusal");
});

test("Case B released mismatch: a released hold whose receipt is not the saved FINAL's needs the action; with it the generation retires", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true });
  const b = w.gens.get(9)!;
  b.exact = { ...closedHold(b), state: "released", release_evidence: "publication",
    final_receipt: { kind: "settled", coverage_digest: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" } } as RecoveryExactHold;
  const described = await localRecords(w);
  assert.deepEqual(classes(described), { 9: "case_b_released_mismatch" });
  const digest = digestOf(described, 9);
  assert.equal((described.result.generations as Json[])[0]!.action, "case_b");
  const before = dataSnapshot(w);
  const files = filesOf(w, b);
  const refs0 = allRefs(w.bare);

  const missing = await resolve(w, 9, digest);
  assert.equal(missing.code, 1);
  assert.deepEqual(missing.json.result, { kind: "refused", reason: "case_b_disposition_required" });
  sameSnap(dataSnapshot(w), before, "after the refusal without the action");

  const retired = await resolve(w, 9, digest, RUN, true);
  assert.equal(retired.code, 0, retired.stderr + JSON.stringify(retired.json));
  assert.deepEqual(retired.json.result, { kind: "retired", generation: 9, remaining: 0 });
  sameSnap(dataSnapshot(w), minus(w, before, files, true), "after the retirement");
  sameLines(allRefs(w.bare), refs0, "refs");
  assertNoApiWrites(w, "released mismatch");
});

const caseBGenerationFiles = (w: World, g: GenInfo): string[] => filesOf(w, g);
const rawRequest = (w: World, req: unknown): Promise<net.Socket> => new Promise((resolve, reject) => {
  const conn = net.createConnection(w.sock);
  conn.on("error", () => undefined);
  conn.on("connect", () => { conn.write(`${JSON.stringify(req)}\n`); resolve(conn); });
  conn.on("error", reject);
});

test("Case B with a client that gives up after the boundary and a spent deadline: the claim gate stays closed until the delayed first unlink settles, the final result is recorded, and a fresh confirmation completes", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const { logger, lines } = recordingLogger();
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true,
    serverLog: logger, serverTimeouts: { resolveDeadlineMs: CASE_B_DEADLINE_MS } });
  const b = w.gens.get(9)!;
  dirtySource(w);
  const described = await localRecords(w);
  const digest0 = digestOf(described, 9);
  const files = caseBGenerationFiles(w, b);
  assert.ok(files.length >= 2);
  const before = dataSnapshot(w);
  const refs0 = allRefs(w.bare);

  const delay = delayFirstUnlinkOf(files);
  // Event-driven: the client stays connected until the first unlink is pending (the last client checkpoint is behind it), and
  // only then gives up. The server deadline runs from the request's receipt (before `reachedAt`), so waiting `reachedAt + deadline` always outlasts it.
  const requestedAt = Date.now();
  let conn: net.Socket | undefined;
  try {
    conn = await rawRequest(w, { op: "resolve-local-record", run: RUN, generation: 9, confirm: digest0, action: "case_b" });
    await within(delay.reached, 60_000, "the delayed Case B unlink");
    const reachedAt = Date.now();
    assert.ok(reachedAt < requestedAt + CASE_B_DEADLINE_MS, "the unlink was reached before the deadline (else the host is too slow for this test)");
    assert.equal(w.gate.heldBy("retained_ops"), true);
    assert.equal(w.gate.tryEnterClaim(), false, "claims are refused");
    const second = await send(w.sock, { op: "resolve-local-record", run: RUN, generation: 9, confirm: "a".repeat(64), action: "case_b" });
    assert.deepEqual(second.result, { kind: "refused", reason: "busy" });
    for (const f of files) assert.equal(fs.existsSync(f), true, "no file is removed yet: " + f);
    sameLines(allRefs(w.bare), refs0, "Case B never touches a ref");

    // The client gives up. That proves neither that the helper exited nor that intake may reopen.
    conn.destroy();
    await new Promise(r => setTimeout(r, Math.max(0, reachedAt + CASE_B_DEADLINE_MS + 300 - Date.now())));
    assert.equal(finishedLines(lines).length, 0, "not finished while the unlink is pending");
    assert.equal(w.gate.heldBy("retained_ops"), true, "a spent deadline and a gone client do not reopen the gate");
    assert.equal(w.gate.tryEnterClaim(), false);

    delay.release();
    await until(() => finishedLines(lines).length === 1, 30_000, "the finished log line");
    const fin = finishedLines(lines)[0]!;
    assert.equal(fin.run_id, RUN);
    assert.equal(fin.generation, 9);
    assert.equal(fin.outcome, "partial", JSON.stringify(fin));
    assert.equal(fin.reason, "deadline_exhausted", JSON.stringify(fin));
    await until(() => !w.gate.heldBy("retained_ops"), 10_000, "the gate to reopen");
    assert.equal(w.gate.tryEnterClaim(), true);
    w.gate.exitClaim();
  } finally { delay.restore(); conn?.destroy(); }

  // The final result is recorded (inspect remembers it) and the post-mutation state is a new observation.
  const last = (await send(w.sock, { op: "inspect", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT })).lastDiscards as Json[];
  assert.ok(last.some(d => d.generation === 9 && d.outcome === "partial" && d.reason === "deadline_exhausted"), JSON.stringify(last));
  assert.equal(fs.existsSync(files.find(f => f.endsWith(".bundle"))!), false);
  const fresh = await localRecords(w);
  assert.deepEqual(classes(fresh), { 9: "case_b_owner_discard" });
  const digest1 = digestOf(fresh, 9);
  assert.notEqual(digest1, digest0, "the digest moved with the removed bundle");
  const stale = await resolve(w, 9, digest0, RUN, true);
  assert.deepEqual(stale.json.result, { kind: "refused", reason: "descriptor_changed" });
  const done = await resolve(w, 9, digest1, RUN, true);
  assert.equal(done.code, 0, done.stderr + JSON.stringify(done.json));
  assert.deepEqual(done.json.result, { kind: "retired", generation: 9, remaining: 0 });
  sameSnap(dataSnapshot(w), minus(w, before, files, true), "after the completed retirement");
  assertNoApiWrites(w, "timeout then completion");
});

test("Case B with a client that disconnects mid-disposition: the gate stays closed until the coordinator settles, and the work still retires", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const { logger, lines } = recordingLogger();
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true, serverLog: logger });
  const b = w.gens.get(9)!;
  dirtySource(w);
  const digest = digestOf(await localRecords(w), 9);
  const files = caseBGenerationFiles(w, b);
  const before = dataSnapshot(w);

  const delay = delayFirstUnlinkOf(files);
  let conn: net.Socket | undefined;
  try {
    conn = await rawRequest(w, { op: "resolve-local-record", run: RUN, generation: 9, confirm: digest, action: "case_b" });
    await within(delay.reached, 30_000, "the delayed Case B unlink");
    conn.destroy();
    await new Promise(r => setTimeout(r, 300));
    assert.equal(w.gate.heldBy("retained_ops"), true, "the gone client does not reopen the gate");
    assert.equal(w.gate.tryEnterClaim(), false);
    assert.equal(finishedLines(lines).length, 0);
    for (const f of files) assert.equal(fs.existsSync(f), true, f);
    delay.release();
    await until(() => finishedLines(lines).length === 1, 30_000, "the finished log line");
    const fin = finishedLines(lines)[0]!;
    assert.equal(fin.outcome, "retired", "the boundary was crossed, so the client cannot interrupt: " + JSON.stringify(fin));
    await until(() => !w.gate.heldBy("retained_ops"), 10_000, "the gate to reopen after settlement");
  } finally { delay.restore(); conn?.destroy(); }
  sameSnap(dataSnapshot(w), minus(w, before, files, true), "after the settled retirement");
  const last = (await send(w.sock, { op: "inspect", run: RUN, generation: EPISODE_GENERATION, attempt: EPISODE_ATTEMPT })).lastDiscards as Json[];
  assert.ok(last.some(d => d.generation === 9 && d.outcome === "retired"), JSON.stringify(last));
  assertNoApiWrites(w, "disconnect");
});

test("a Case B retirement leaves no trace for a later boot or live discovery pass to recreate, while the pins stay", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: false });
  dirtySource(w);
  const digest = digestOf(await localRecords(w), 9);
  const retired = await resolve(w, 9, digest, RUN, true);
  assert.deepEqual(retired.json.result, { kind: "retired", generation: 9, remaining: 0 });
  const after = dataSnapshot(w);
  const pins = allRefs(w.bare).filter(l => l.startsWith(`refs/uzi-coverage/${RUN}/9/`) || l.startsWith(`refs/uzi-recovery-pin/${RUN}/9 `));
  assert.ok(pins.length >= 2, "the generation's pins stay after the record disposition: " + pins.join());

  // The boot discovery (materializeBootInventory) really runs; the live pass is the existing heartbeat re-drive.
  const coordinator = w.coordinator as unknown as { materializeBootInventory: (...a: unknown[]) => Promise<unknown[]> };
  const realMaterialize = coordinator.materializeBootInventory.bind(coordinator);
  const materialized: unknown[][] = [];
  Object.assign(coordinator, { materializeBootInventory: async (...args: unknown[]) => {
    const found = await realMaterialize(...args);
    materialized.push(found);
    return found;
  } });
  await w.runner.resumePendingRecoveries();
  await w.runner.resumeLiveRecoveries(Date.now());
  assert.ok(materialized.length >= 1, "the discovery ran (boot and live passes)");
  assert.deepEqual(materialized.flat(), [], "and found nothing to recreate");
  assert.deepEqual(await w.coordinator.inspect(RUN), [], "no record is recreated from the pins");
  assert.equal(fs.existsSync(recoveryDir(w)), false);
  sameSnap(dataSnapshot(w), after, "after the boot and live passes");
  assertNoApiWrites(w, "discovery passes");
  assert.deepEqual((await localRecords(w)).result, { kind: "described", runId: RUN, generations: [], blockers: [] });
});

test("after a Case B retirement the existing inspect and discard take over: the real predicates decide, with no raw deletion or pin drop", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true });
  dirtySource(w);
  const retired = await resolve(w, 9, digestOf(await localRecords(w), 9), RUN, true);
  assert.deepEqual(retired.json.result, { kind: "retired", generation: 9, remaining: 0 });
  const refs0 = allRefs(w.bare);

  const otherRecords = treeDigest(recoveryDir(w, OTHER_RUN));
  const otherSource = treeDigest(w.other!.clonePath);
  const inspected = await operator(w, ["inspect", "--run", RUN, "--generation", String(EPISODE_GENERATION), "--attempt", EPISODE_ATTEMPT]);
  assert.equal(inspected.code, 0, inspected.stderr);
  const source = inspected.json.sources[0] as Json;
  // The records are gone, so the existing predicates now offer the protected discard of the (still dirty) source; its
  // remaining consumers are the pins, which the discard does not own.
  assert.deepEqual(source.localRecords, []);
  assert.deepEqual(source.disposition, { kind: "local_protected_discard" });
  assert.ok((source.consumers as string[]).includes("owed_pins:1") && (source.consumers as string[]).includes("episode_pins:1"), source.consumers.join());
  assert.deepEqual((source.consumers as string[]).filter(c => c.startsWith("blocker:")), []);

  const discarded = await operator(w, ["discard", "--run", RUN, "--generation", String(EPISODE_GENERATION), "--attempt", EPISODE_ATTEMPT,
    "--confirm", source.descriptor, "--protected"]);
  assert.equal(discarded.code, 0, discarded.stderr + JSON.stringify(discarded.json));
  assert.equal(discarded.json.result.kind, "done", JSON.stringify(discarded.json));
  assert.deepEqual((discarded.json.result.sources as Json[]).map(s => s.outcome), ["removed", "removed"]);
  assert.equal(fs.existsSync(w.episode.sourcePath), false);
  assert.equal(fs.existsSync(w.episode.successorPath), false);

  // Only the discard's own episode pin left; the generation's pins, the owed pin and the other run are untouched.
  const episodePin = refs0.filter(l => l.startsWith(`refs/uzi-recovery-episode/${RUN}/`));
  assert.equal(episodePin.length, 1);
  sameLines(allRefs(w.bare), without(refs0, episodePin), "refs after the discard");
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-coverage/${RUN}/9/`)) && allRefs(w.bare).some(l => l.startsWith(`refs/uzi-recovery-pin/${RUN}/9 `)));
  assert.ok(allRefs(w.bare).some(l => l.startsWith(`refs/uzi-owed/${RUN}/${w.episode.tip} `)));
  sameLines(treeDigest(recoveryDir(w, OTHER_RUN)), otherRecords, "the other run's records");
  sameLines(treeDigest(w.other!.clonePath), otherSource, "the other run's retained source");
  assertNoApiWrites(w, "follow-on discard");
});

// ─── Case B against the runner's REAL outbox (issue #3885 repair R1) ─────────────────────────────────────────────────
// The world's `outbox: true` wires a real, initialised Outbox into the real RunRunner, so `pendingGenerationReport` is the
// runner's own predicate (isDisabled / hasPendingTerminal / finalizeRecordIdentity), not a stub.

const POLICY_OUTCOME = "5c1f0000-0000-4000-8000-000000000001";

test("Case B is refused as pending_generation_report while the real outbox holds an UNKNOWN policy outcome or a finalize record for the generation; every byte stays", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const kinds: Array<[string, (w: World) => Promise<void>]> = [
    ["an UNKNOWN policy outcome (reserved, then selected)", async w => {
      const token = w.outbox!.reservePolicyFlight(RUN, 9);
      assert.ok(token, "the real outbox admits the policy flight");
      assert.equal(w.outbox!.selectPolicyFlight(token, POLICY_OUTCOME), true);
      assert.equal(w.outbox!.hasPendingTerminal(RUN, 9), true);
    }],
    ["a journaled finalize record", async w => {
      assert.deepEqual(await w.outbox!.journalFinalize(RUN, 9), { written: true });
      assert.ok(w.outbox!.finalizeRecordIdentity(RUN, 9));
    }],
  ];
  for (const [name, seed] of kinds) {
    const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true, outbox: true });
    // Described and digested while nothing is pending; the report appears afterwards.
    const described = await localRecords(w);
    assert.deepEqual(classes(described), { 9: "case_b_owner_discard" }, name);
    const digest = digestOf(described, 9);
    await seed(w);
    const before = dataSnapshot(w);
    const refs0 = allRefs(w.bare);
    const refused = await resolve(w, 9, digest, RUN, true);
    assert.equal(refused.code, 1, name + refused.stderr);
    assert.deepEqual(refused.json.result, { kind: "refused", reason: "pending_generation_report" }, name);
    const view = await localRecords(w);
    assert.equal(view.result.generations[0].classification, "pending_generation_report", name);
    sameSnap(dataSnapshot(w), before, "after the pending-report refusal: " + name);
    sameLines(allRefs(w.bare), refs0, "refs: " + name);
    assertNoApiWrites(w, name);
  }
});

test("Case B owner-discard with a real outbox: unrelated policy and finalize artifacts of another run or generation stay byte-identical", {
  skip: process.platform === "linux" ? false : "Linux only: the production source proof refuses non_linux_host",
}, async () => {
  const w = await world({ gens: [{ generation: 9, kind: "caseB" }], sharedOwedPin: false, otherRun: true, outbox: true });
  const b = w.gens.get(9)!;
  // Another run, and another generation of this run: real finalize records on disk plus a selected policy outcome in memory.
  assert.deepEqual(await w.outbox!.journalFinalize(OTHER_RUN, 3), { written: true });
  assert.deepEqual(await w.outbox!.journalFinalize(RUN, 8), { written: true });
  const token = w.outbox!.reservePolicyFlight(OTHER_RUN, 3);
  assert.ok(token);
  assert.equal(w.outbox!.selectPolicyFlight(token, POLICY_OUTCOME), true);
  const outboxDir = path.join(w.dir, "outbox");
  const outboxBefore = treeDigest(outboxDir);
  assert.ok(outboxBefore.some(l => l.includes("finalize-3.json")) && outboxBefore.some(l => l.includes("finalize-8.json")), outboxBefore.join("\n"));
  const described = await localRecords(w);
  assert.deepEqual(classes(described), { 9: "case_b_owner_discard" });
  const before = dataSnapshot(w);
  const files = filesOf(w, b);
  const refs0 = allRefs(w.bare);

  const retired = await resolve(w, 9, digestOf(described, 9), RUN, true);
  assert.equal(retired.code, 0, retired.stderr + JSON.stringify(retired.json));
  assert.deepEqual(retired.json.result, { kind: "retired", generation: 9, remaining: 0 });
  sameLines(treeDigest(outboxDir), outboxBefore, "the outbox (policy and finalize journals of other runs and generations)");
  assert.equal(w.outbox!.hasPendingTerminal(OTHER_RUN, 3), true, "the other run's policy outcome is still pending");
  assert.ok(w.outbox!.finalizeRecordIdentity(RUN, 8) && w.outbox!.finalizeRecordIdentity(OTHER_RUN, 3));
  sameSnap(dataSnapshot(w), minus(w, before, files, true), "after the retirement");
  sameLines(allRefs(w.bare), refs0, "refs");
  assertNoApiWrites(w, "real outbox retirement");
});
