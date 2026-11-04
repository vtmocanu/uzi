import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { spawn, type ChildProcess } from "node:child_process";
import fsSync from "node:fs";
import http from "node:http";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { afterEach, test } from "node:test";
import { binding, closed, environment, fixture, git, inventory, patch, type Fixture } from "./fixtures/recovery-bindingless-compat.js";
import type { ChildSpec, KillAt, ServerState } from "./fixtures/recovery-local-record-restart-child.js";

// issue #3885 M2 U3: the approved failure model is an abrupt worker-process kill (no handler, no finally, no
// atexit runs); storage and power loss are unspecified. Every kill below is a REAL SIGKILL of a child process
// that stopped at a named seam, then a SECOND, fresh process over the same data directory describes and resolves.
// An injected throw would run the post-POST catch of retainFinal and prove nothing about a crash.

const roots: string[] = [];
const children = new Set<ChildProcess>();
const apis = new Set<http.Server>();
afterEach(async () => {
  for (const api of apis) await new Promise<void>(resolve => { api.close(() => resolve()); api.closeAllConnections(); });
  apis.clear();
  for (const child of children) if (child.exitCode === null && child.signalCode === null) child.kill("SIGKILL");
  children.clear();
  for (const root of roots.splice(0)) await fs.rm(root, { recursive: true, force: true });
});

const SEAM_WAIT_MS = 40_000;
const CHILD_WALL_MS = 100_000;
const LEGACY_RUN = "31050000-0000-4000-8000-0000000000bb";
const AGENT_DIR = path.resolve(import.meta.dirname, "..");
const CHILD = path.resolve(import.meta.dirname, "fixtures/recovery-local-record-restart-child.ts");

type Described = { kind: string; generations?: Array<{ generation: number; classification: string; digest?: string }>; blockers?: string[] };
type Resolved = { kind: string; reason?: string; generation?: number; remaining?: number };
interface ChildOutcome { described?: Described; resolved?: Resolved; exit: { code: number | null; signal: NodeJS.Signals | null }; stderr: string }

/**
 * The fake worker API the real WorkerClient in the child talks to (real HTTP on 127.0.0.1). It serves only what the
 * resolution needs: register, the exact-hold read (the closed hold released under the saved FINAL's receipt) and the
 * release POST. The release models api/internal/recovery/final_inventory.go: an identical released request is answered
 * with the same ack, a different one is a conflict. Its state is a JSON file, so a fresh child meets earlier POSTs and
 * the parent reads them without asking the server.
 */
async function startApi(f: Fixture, stateFile: string): Promise<string> {
  const request = f.record.finalRequest!;
  const hold = { ...closed(f), state: "released", run_status: "failed", release_evidence: request.evidence ?? "archive",
    final_receipt: request.disposition };
  const read = (): ServerState => { try { return JSON.parse(fsSync.readFileSync(stateFile, "utf8")) as ServerState; } catch { return { posts: [] }; } };
  const server = http.createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on("data", (c: Buffer) => chunks.push(c));
    req.on("end", () => {
      const reply = (status: number, body: unknown): void => { res.writeHead(status, { "Content-Type": "application/json" }); res.end(JSON.stringify(body)); };
      const url = new URL(req.url!, "http://127.0.0.1");
      if (req.method === "POST" && url.pathname.endsWith("/register")) {
        return reply(200, { worker_id: hold.original_worker_id, protocol_features: ["recovery_inventory_v1"] });
      }
      if (req.method === "GET" && url.pathname.endsWith("/recovery-holds")) return reply(200, { run_id: f.context.runId, holds: [], exact_hold: hold });
      if (req.method === "POST" && url.pathname.endsWith("/archives/release")) {
        const body = JSON.parse(Buffer.concat(chunks).toString("utf8")) as { generation: number; release_evidence?: unknown; final_disposition?: unknown };
        const entry = { evidence: body.release_evidence, disposition: body.final_disposition };
        const state = read();
        const first = state.posts[0];
        if (first && JSON.stringify(first) !== JSON.stringify(entry)) return reply(409, { error: "conflicting release disposition" });
        state.posts.push(entry);
        fsSync.writeFileSync(stateFile + ".tmp", JSON.stringify(state));
        fsSync.renameSync(stateFile + ".tmp", stateFile);
        return reply(200, { run_id: f.context.runId, generation: body.generation, released: true, holds_released: 1 });
      }
      reply(404, { error: `unexpected ${req.method} ${url.pathname}` });
    });
  });
  apis.add(server);
  await new Promise<void>(resolve => server.listen(0, "127.0.0.1", resolve));
  return `http://127.0.0.1:${(server.address() as { port: number }).port}`;
}

/** A temp data directory with a real bare, one guarded saved-FINAL generation and an unrelated run beside it. */
async function seed() {
  const root = await fs.realpath(await fs.mkdtemp(path.join(os.tmpdir(), "uzi-local-restart-")));
  roots.push(root);
  const f = await fixture(root, "saved-final");
  const bare = f.context.barePath;
  await fs.chmod(path.dirname(bare), 0o700);
  await fs.chmod(bare, 0o700);
  await fs.chmod(path.join(bare, "config"), 0o600);
  const { coordinator } = environment(f);
  f.record = await patch(coordinator, f.record, { exactHoldBinding: binding });
  // Unrelated state in the same data directory: another run's record, an unbound file and bundle-shaped file in
  // its directory, and a ref that belongs to no generation.
  assert.ok(await coordinator.pin({ runId: LEGACY_RUN, sourceSha: f.head, kind: "issue", branch: "agent/issue-1", generation: 3 }));
  const legacyDir = path.join(root, "data", "recovery", LEGACY_RUN);
  await fs.writeFile(path.join(legacyDir, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa.bundle"), "unrelated bundle bytes");
  await fs.writeFile(path.join(legacyDir, "stray.note"), "unrelated stray");
  git(bare, "update-ref", "refs/owed/unrelated-other-run", f.head);
  const ctl = path.join(root, "ctl");
  await fs.mkdir(ctl);
  const server = path.join(ctl, "server.json");
  await fs.writeFile(server, JSON.stringify({ posts: [] } satisfies ServerState));
  const apiUrl = await startApi(f, server);
  return { f, root, bare, runId: f.context.runId, ctl, server, apiUrl, counter: { n: 0 } };
}
type World = Awaited<ReturnType<typeof seed>>;

const posts = (w: World): ServerState["posts"] => (JSON.parse(fsSync.readFileSync(w.server, "utf8")) as ServerState).posts;
const sha = (bytes: Buffer | string): string => createHash("sha256").update(bytes).digest("hex");
const recDir = (w: World): string => path.join(w.root, "data", "recovery", w.runId);

interface Snapshot {
  /** name -> content hash for every entry of the generation's run directory */
  gen: Record<string, string>;
  /** every ref line that does not name this run (fixture refs, the unrelated ref, the other run's pins) */
  unrelatedRefs: string;
  /** all refs */
  refs: string;
  /** state no resolution of this generation may touch */
  unrelated: { legacy: Awaited<ReturnType<typeof inventory>>; reports: Awaited<ReturnType<typeof inventory>> };
  acknowledged: number;
}
async function snapshot(w: World): Promise<Snapshot> {
  const gen: Record<string, string> = {};
  let acknowledged = 0;
  for (const name of await fs.readdir(recDir(w)).catch(() => [] as string[])) {
    const bytes = await fs.readFile(path.join(recDir(w), name));
    gen[name] = sha(bytes);
    if (name.endsWith(".json") && JSON.parse(bytes.toString("utf8")).finalAcknowledged === true) acknowledged++;
  }
  const refs = git(w.bare, "for-each-ref", "--format=%(refname) %(objectname)");
  return {
    gen, refs, acknowledged,
    unrelatedRefs: refs.split("\n").filter(line => !line.includes(w.runId)).join("\n"),
    unrelated: {
      legacy: await inventory(path.join(w.root, "data", "recovery", LEGACY_RUN)),
      reports: await inventory(path.join(w.root, "reports")),
    },
  };
}
/** What a crash may never do: touch another run, delete an unbound file or invent an acknowledgement. */
function assertBoundary(before: Snapshot, after: Snapshot, acknowledgedPosts: number) {
  assert.deepEqual(after.unrelated, before.unrelated, "the unrelated run's records, bundle, stray file and the reports are byte-identical");
  assert.equal(after.unrelatedRefs, before.unrelatedRefs, "every ref that does not belong to the generation is unchanged");
  if (acknowledgedPosts === 0) assert.equal(after.acknowledged, 0, "no record gains finalAcknowledged without a recorded POST ack");
}

function waitExit(child: ChildProcess): Promise<{ code: number | null; signal: NodeJS.Signals | null }> {
  if (child.exitCode !== null || child.signalCode !== null) return Promise.resolve({ code: child.exitCode, signal: child.signalCode });
  return new Promise(resolve => child.once("exit", (code, signal) => resolve({ code, signal })));
}
const sleep = (ms: number) => new Promise(resolve => setTimeout(resolve, ms));
function gone(pid: number): boolean {
  try { process.kill(pid, 0); return false; } catch (err) { return (err as NodeJS.ErrnoException).code === "ESRCH"; }
}

/** One child over the world. With `killAt` it waits for the seam marker (bounded) and SIGKILLs its own child. */
async function runChild(w: World, action: ChildSpec["action"], killAt?: KillAt): Promise<ChildOutcome> {
  const tag = `${action}-${killAt ?? "run"}-${++w.counter.n}`;
  const spec: ChildSpec = { root: w.root, runId: w.runId, generation: 7, context: w.f.context, head: w.f.head,
    attempts: w.f.attempts, action, ...(killAt ? { killAt } : {}),
    marker: path.join(w.ctl, `${tag}.marker`), result: path.join(w.ctl, `${tag}.result.json`), apiUrl: w.apiUrl };
  const specFile = path.join(w.ctl, `${tag}.spec.json`);
  await fs.writeFile(specFile, JSON.stringify(spec));
  const child = spawn(process.execPath, ["--import", "tsx", "--import", "./test/setup/hermetic-proc.ts", CHILD, specFile], {
    cwd: AGENT_DIR, stdio: ["ignore", "ignore", "pipe"],
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  });
  children.add(child);
  let stderr = "";
  child.stderr!.on("data", (chunk: Buffer) => { if (stderr.length < 8000) stderr += chunk.toString("utf8"); });
  const exited = waitExit(child);
  const deadline = Date.now() + (killAt ? SEAM_WAIT_MS : CHILD_WALL_MS);
  if (killAt) {
    let ended = false;
    void exited.then(() => { ended = true; });
    while (!fsSync.existsSync(spec.marker)) {
      if (ended) assert.fail(`child exited before reaching seam ${killAt}: ${stderr}`);
      if (Date.now() > deadline) { child.kill("SIGKILL"); await exited; assert.fail(`seam ${killAt} not reached in ${SEAM_WAIT_MS} ms: ${stderr}`); }
      await sleep(25);
    }
    assert.equal(fsSync.readFileSync(spec.marker, "utf8"), killAt);
    assert.equal(child.kill("SIGKILL"), true);
  } else {
    const timer = setTimeout(() => child.kill("SIGKILL"), CHILD_WALL_MS);
    void exited.finally(() => clearTimeout(timer));
  }
  const exit = await exited;
  assert.equal(gone(child.pid!), true, "the child process is gone");
  children.delete(child);
  if (killAt) assert.equal(exit.signal, "SIGKILL", "the worker died by SIGKILL, not a cooperative exit");
  else assert.deepEqual(exit, { code: 0, signal: null }, stderr);
  let result: { described?: Described; resolved?: Resolved } = {};
  try { result = JSON.parse(fsSync.readFileSync(spec.result, "utf8")); } catch { /* a killed child may not have described yet */ }
  return { ...result, exit, stderr };
}

const classification = (d: Described | undefined): string | undefined => d?.generations?.[0]?.classification;
const digest = (d: Described | undefined): string | undefined => d?.generations?.[0]?.digest;

/** The harness itself: the same child, never killed, retires the generation with one POST. */
test("control: an unkilled worker process describes, replays once and retires the generation", async () => {
  const w = await seed();
  const out = await runChild(w, "resolve");
  assert.equal(classification(out.described), "case_a_replay");
  assert.deepEqual(out.resolved, { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(posts(w).length, 1);
  assert.doesNotMatch(git(w.bare, "for-each-ref", "--format=%(refname)"), new RegExp(`refs/uzi-coverage/${w.runId}/7/`));
});

test("kill 1: inside the source boundary, before the POST: bytes unchanged, the fresh process offers the same replay", async () => {
  const w = await seed();
  const before = await snapshot(w);
  const killed = await runChild(w, "resolve", "pre-post");
  assert.equal(classification(killed.described), "case_a_replay");
  assert.equal(killed.resolved, undefined);
  assert.equal(posts(w).length, 0, "the POST never left the process");
  const after = await snapshot(w);
  assert.deepEqual(after.gen, before.gen, "the generation's record and bundle are byte-identical");
  assert.equal(after.refs, before.refs);
  assertBoundary(before, after, 0);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "case_a_replay");
  assert.equal(digest(fresh.described), digest(killed.described), "the same digest as before the kill");
  assert.deepEqual(fresh.resolved, { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(posts(w).length, 1);
  assertBoundary(before, await snapshot(w), 1);
});

test("kill 2: after the server answered the POST, before the ACK write: the fresh process replays the identical request", async () => {
  const w = await seed();
  const before = await snapshot(w);
  const killed = await runChild(w, "resolve", "post-before-ack");
  assert.equal(posts(w).length, 1, "the server recorded the POST");
  const after = await snapshot(w);
  assert.deepEqual(after.gen, before.gen, "no ACK, no temp file: the record is byte-identical");
  assert.equal(after.refs, before.refs);
  assertBoundary(before, after, 0);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "case_a_replay");
  assert.equal(digest(fresh.described), digest(killed.described));
  assert.deepEqual(fresh.resolved, { kind: "retired", generation: 7, remaining: 0 });
  const sent = posts(w);
  assert.equal(sent.length, 2, "the replay re-sends once");
  assert.equal(JSON.stringify(sent[1]), JSON.stringify(sent[0]), "byte-identical evidence and disposition");
  assertBoundary(before, await snapshot(w), 1);
});

test("kill 3: after the ACK temp write, before the rename: the temp file is an unbound artifact and resolve refuses", async () => {
  const w = await seed();
  const before = await snapshot(w);
  await runChild(w, "resolve", "ack-tmp-written");
  assert.equal(posts(w).length, 1);
  const after = await snapshot(w);
  const tmp = Object.keys(after.gen).filter(name => !(name in before.gen));
  assert.equal(tmp.length, 1);
  assert.match(tmp[0]!, /^[0-9a-f-]{36}\.json\.[0-9a-f-]{36}\.tmp$/);
  for (const [name, hash] of Object.entries(before.gen)) assert.equal(after.gen[name], hash, `${name} unchanged`);
  assert.equal(after.acknowledged, 0, "the record itself is not acknowledged");
  assert.equal(after.refs, before.refs);
  assertBoundary(before, after, 0);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "unbound_artifact:" + tmp[0]);
  assert.deepEqual(fresh.resolved, { kind: "refused", reason: "unbound_artifact:" + tmp[0] });
  assert.equal(posts(w).length, 1, "a refused generation does not replay");
  const last = await snapshot(w);
  assert.deepEqual(last.gen, after.gen, "temp file and record retained, byte-identical");
  assert.equal(last.refs, before.refs);
  assertBoundary(before, last, 1);
});

test("kill 4: after the ACK rename, before the cleanup: the fresh process sees case_a_ack and retires without a POST", async () => {
  const w = await seed();
  const before = await snapshot(w);
  await runChild(w, "resolve", "ack-renamed");
  assert.equal(posts(w).length, 1);
  const after = await snapshot(w);
  assert.equal(after.acknowledged, 1, "the ACK is durable");
  assert.deepEqual(Object.keys(after.gen).sort(), Object.keys(before.gen).sort(), "no temp file is left");
  assert.equal(after.refs, before.refs, "no cleanup had started");
  assertBoundary(before, after, 1);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "case_a_ack");
  assert.deepEqual(fresh.resolved, { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(posts(w).length, 1, "an acknowledged generation is never re-POSTed");
  assertBoundary(before, await snapshot(w), 1);
});

test("kill 5: after the first shared-bare deletion was registered: the fresh process refuses and everything is retained", async () => {
  const w = await seed();
  const before = await snapshot(w);
  await runChild(w, "resolve", "after-begin-bare");
  const after = await snapshot(w);
  assert.equal(after.refs, before.refs, "the registered deletion never ran");
  assert.equal(after.acknowledged, 1);
  assertBoundary(before, after, 1);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "case_a_ack");
  assert.deepEqual(fresh.resolved, { kind: "refused", reason: "bare_mutation_unresolved" });
  const last = await snapshot(w);
  assert.deepEqual(last.gen, after.gen, "every record and the bundle are retained");
  assert.equal(last.refs, before.refs);
  assert.equal(posts(w).length, 1);
  assertBoundary(before, last, 1);
});

test("kill 6: after the bundle unlinks, before the record deletes: the ACK record remains and nothing unbound is deleted", async () => {
  const w = await seed();
  const before = await snapshot(w);
  const killed = await runChild(w, "resolve", "after-bundle-unlink");
  const after = await snapshot(w);
  const removed = Object.keys(before.gen).filter(name => !(name in after.gen));
  assert.deepEqual(removed, [Object.keys(before.gen).find(name => name.endsWith(".bundle"))], "only the generation's bundle is gone");
  assert.equal(after.acknowledged, 1, "the ACK record remains");
  assert.doesNotMatch(after.refs, new RegExp(`refs/uzi-coverage/${w.runId}/7/`), "the cleanup had already removed the generation's pins");
  assertBoundary(before, after, 1);

  const fresh = await runChild(w, "resolve");
  assert.equal(classification(fresh.described), "case_a_ack", "the surviving ACK record is still described as retirable");
  assert.notEqual(digest(fresh.described), digest(killed.described), "the bundle and pins are gone, so the observation is a new digest");
  assert.deepEqual(fresh.resolved, { kind: "retired", generation: 7, remaining: 0 });
  assert.equal(posts(w).length, 1);
  const last = await snapshot(w);
  assert.deepEqual(last.gen, {}, "the ACK record is retired last");
  assertBoundary(before, last, 1);
});
