import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import fs from "node:fs";
import net from "node:net";
import path from "node:path";
import { afterEach, test } from "node:test";
import { setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { makeFakeProcRoot, plantInScope } from "./fake-proc.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import {
  EPISODE_ATTEMPT, EPISODE_GENERATION, OTHER_RUN, RUN, allRefs, createWorld, gitIn, treeDigest, type GenInfo, type World,
} from "./fixtures/recovery-local-record-e2e-world.js";

// issue #3885 M2 end-to-end regression. ONE data dir is shared by a real GitCache, the real RunRunner (with the real
// RecoveryCoordinator it builds) and the real RetainedSourceServer over its Unix socket; the operator speaks to it
// through the socket (the self-contained .mjs client for the local-record verbs, raw NDJSON for the rest).
// Only the API client (recorded) and the proc view are controlled seams.

const CLIENT_MJS = path.resolve(import.meta.dirname, "..", "src", "ops", "retained-source.mjs");
type Json = Record<string, any>;

const worlds: World[] = [];
const scratch: string[] = [];
afterEach(async () => {
  restoreHermeticView();
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

/** The real .mjs client (async spawn: the server lives in this process, so the event loop must stay free). */
function operator(w: World, args: string[]): Promise<{ code: number | null; json: Json; stderr: string }> {
  return new Promise((resolve, reject) => {
    const env = { ...process.env };
    delete env.NODE_TEST_CONTEXT;
    const child = spawn(process.execPath, [CLIENT_MJS, ...args, "--socket", w.sock], { env, stdio: ["ignore", "pipe", "pipe"] });
    let out = "", err = "";
    child.stdout.on("data", c => { out += c; });
    child.stderr.on("data", c => { err += c; });
    child.on("error", reject);
    child.on("close", code => {
      try { resolve({ code, json: JSON.parse(out) as Json, stderr: err }); } catch (e) { reject(new Error(`client output ${out} / ${err}: ${e}`)); }
    });
  });
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
const resolve = (w: World, generation: number, confirm: string, run = RUN) =>
  operator(w, ["resolve-local-record", "--run", run, "--generation", String(generation), "--confirm", confirm]);

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
  assert.deepEqual(classes(described), { 7: "case_a_ack", 8: "case_a_replay", 9: "case_b_pending_owner_review" });
  const digestA1 = digestOf(described, 7);
  digestOf(described, 8);
  assert.equal((described.result.generations as Json[]).find(g => g.generation === 9)?.digest, undefined, "Case B offers no digest");

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
  assert.deepEqual(classes(described2), { 8: "case_a_replay", 9: "case_b_pending_owner_review" });
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

  // 4. Case B remains: inspect still asks for local-record resolution, and local-records still holds it back.
  const after = await inspectEpisode(w);
  assert.equal(after.disposition.kind, "resolve_local_record", JSON.stringify(after.disposition));
  assert.deepEqual([...new Set((after.disposition.records as Json[]).filter(r => r.generation !== null).map(r => r.generation))], [9]);
  const left = await localRecords(w);
  assert.deepEqual(classes(left), { 9: "case_b_pending_owner_review" });
  const refusedB = await resolve(w, 9, "a".repeat(64));
  assert.deepEqual(refusedB.json.result, { kind: "refused", reason: "case_b_pending_owner_review" });
  assert.equal(w.client.posts.length, 1);

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
  assert.notEqual(gated.result.kind, "done", JSON.stringify(gated));
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
