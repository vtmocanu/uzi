// issue #3885 M2 U3: the worker-process half of the restart test (test/recovery-local-record-restart.test.ts).
//
// Run as `node --import tsx --import ./test/setup/hermetic-proc.ts <this file> <spec.json>` from agent/. It builds
// the production shape of the operator path over a data directory the parent seeded: a real AuthorityRuntime (the
// worker lifetime lock W is taken for real), a real GitCache bound to its hooks with `authorityRequired`, and a
// RecoveryCoordinator whose only fakes are the API client and the physical source boundary. The "server" is a
// JSON file the parent and every child share, so a release POST is observable to the parent and to a later,
// fresh process. At the named seam the child writes a marker file and blocks forever; the parent kills it with a
// real SIGKILL (no handler, no finally, no atexit runs).
import fsSync from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { acquireWorkerLifetime } from "../../src/authority-lock.js";
import { AuthorityRuntime } from "../../src/authority-runtime.js";
import { GitCache } from "../../src/git.js";
import type { RecoveryExactHold } from "../../src/protocol.js";
import type { RecoveryRecord } from "../../src/recovery.js";
import { testLockOptions } from "../authority-test-support.js";
import { nullLogger } from "../helpers.js";
import { closed, environment, type Fixture } from "./recovery-bindingless-compat.js";

export const CHILD_AUTHORITY_TOKEN = "restart-child-authority-token";

/** Every point at which the child can be told to stop forever. */
export type KillAt =
  | "pre-post"            // inside the inventory source boundary, before the release POST leaves the process
  | "post-before-ack"     // the server recorded and answered the POST; the ACK write has not started
  | "ack-tmp-written"     // the ACK temp file is on disk; the rename has not run
  | "ack-renamed"         // the ACK is durable; no cleanup has started
  | "after-begin-bare"    // the first shared-bare deletion is registered with the authority; the ref is untouched
  | "after-bundle-unlink"; // the generation's bundle is gone; no record has been unlinked

export interface ChildSpec {
  root: string;
  runId: string;
  generation: number;
  context: Fixture["context"];
  head: string;
  attempts: string[];
  action: "describe" | "resolve";
  killAt?: KillAt;
  marker: string;
  result: string;
  server: string;
}

/** The fake server state both processes read and append to. */
export interface ServerState { posts: Array<{ evidence: unknown; disposition: unknown }> }

const spec = JSON.parse(fsSync.readFileSync(process.argv[2]!, "utf8")) as ChildSpec;
// A child that outlives its parent for any reason ends itself.
setTimeout(() => process.exit(97), 150_000);

function readServer(): ServerState {
  try { return JSON.parse(fsSync.readFileSync(spec.server, "utf8")) as ServerState; } catch { return { posts: [] }; }
}
function writeServer(state: ServerState): void {
  const tmp = spec.server + ".tmp";
  fsSync.writeFileSync(tmp, JSON.stringify(state));
  fsSync.renameSync(tmp, spec.server);
}
function writeResult(value: unknown): void {
  const tmp = spec.result + ".tmp";
  fsSync.writeFileSync(tmp, JSON.stringify(value));
  fsSync.renameSync(tmp, spec.result);
}

/** Announce the seam and stop here forever. The interval keeps the loop alive: only SIGKILL ends this. */
async function block(point: KillAt): Promise<never> {
  setInterval(() => undefined, 1000);
  fsSync.writeFileSync(spec.marker, point);
  await new Promise<never>(() => undefined);
  throw new Error("unreachable");
}
const at = (point: KillAt): boolean => spec.killAt === point;

const dataDir = path.join(spec.root, "data");
const recoveryRoot = path.join(dataDir, "recovery");
const lifetime = await acquireWorkerLifetime(dataDir, testLockOptions());
const authority = new AuthorityRuntime({ lifetime, workerToken: CHILD_AUTHORITY_TOKEN, timeoutMs: 5000 });
const f: Fixture = { root: spec.root, context: spec.context, head: spec.head, attempts: spec.attempts,
  record: undefined as unknown as RecoveryRecord };
const e = environment(f);
const coordinator = e.coordinator;
const producer = coordinator["git"], client = coordinator["client"];
const cache = new GitCache(dataDir, nullLogger(), undefined, { authority: authority.hooks, authorityRequired: true });
producer.recoveryRefSnapshot = cache.recoveryRefSnapshot.bind(cache);
producer.assertRecoveryBareClear = cache.assertRecoveryBareClear.bind(cache);
producer.verifyRecoveryCoverage = cache.verifyRecoveryCoverage.bind(cache);
producer.cleanupRecoveryGeneration = (...args) => cache.cleanupRecoveryGeneration(...args);
Object.assign(coordinator, { authority });
e.state.open = false;
f.record = (await coordinator.inspect(spec.runId))[0]!;

// The closed hold carries the receipt equal to the saved FINAL request (the case-A evidence), from the start.
client.getExactRecoveryHold = async (): Promise<RecoveryExactHold | undefined> => {
  const request = (await coordinator.inspect(spec.runId))[0]?.finalRequest;
  if (!request) return undefined;
  return { ...closed(f), state: "released", run_status: "failed", release_evidence: request.evidence,
    final_receipt: request.disposition };
};
// The real API treats an identical released disposition as a replay (api/internal/recovery/final_inventory.go):
// the same request is answered with the same ack; a different one is refused.
client.releaseRecoveryCustody = async (...args: unknown[]) => {
  if (at("pre-post")) await block("pre-post");
  const entry = { evidence: args[2], disposition: args[3] };
  const state = readServer();
  const first = state.posts[0];
  if (first && JSON.stringify(first) !== JSON.stringify(entry)) throw new Error("fixture server: conflicting release disposition");
  state.posts.push(entry);
  writeServer(state);
  return { run_id: spec.runId, generation: spec.generation, released: true, holds_released: 1 };
};

if (at("post-before-ack")) {
  const write = coordinator["writeRecordUnlocked"].bind(coordinator);
  Object.assign(coordinator, { writeRecordUnlocked: async (record: RecoveryRecord) => {
    if (record.finalAcknowledged === true) await block("post-before-ack");
    return write(record);
  } });
}
if (at("after-begin-bare")) {
  const begin = cache["beginBare"].bind(cache);
  Object.assign(cache, { beginBare: async (...args: Parameters<typeof begin>) => {
    const settlement = await begin(...args);
    return block("after-begin-bare").then(() => settlement);
  } });
}
if (at("ack-tmp-written") || at("ack-renamed") || at("after-bundle-unlink")) {
  // src modules call fs.<fn> on this very object, so a patch here is the seam. Only paths under the recovery root.
  const inRecovery = (p: unknown): boolean => String(p).startsWith(recoveryRoot + path.sep);
  const rename = fsp.rename.bind(fsp), unlink = fsp.unlink.bind(fsp);
  let bundleGone = false;
  Object.assign(fsp, {
    rename: async (from: string, to: string) => {
      const record = inRecovery(from) && String(from).endsWith(".tmp") && String(to).endsWith(".json");
      if (record && at("ack-tmp-written")) await block("ack-tmp-written");
      await rename(from, to);
      if (record && at("ack-renamed")) await block("ack-renamed");
    },
    unlink: async (p: string) => {
      if (at("after-bundle-unlink") && bundleGone && inRecovery(p) && String(p).endsWith(".json")) await block("after-bundle-unlink");
      await unlink(p);
      if (inRecovery(p) && String(p).endsWith(".bundle")) bundleGone = true;
    },
  });
}

const sig = { requestSignal: new AbortController().signal, shutdownSignal: new AbortController().signal,
  deadlineAt: Date.now() + 90_000 };
const described = await coordinator.describeLocalRecords(spec.runId, sig);
const out: Record<string, unknown> = { described };
writeResult(out);
if (spec.action === "resolve") {
  const generation = described.kind === "described" ? described.generations.find(g => g.generation === spec.generation) : undefined;
  const digest = generation?.digest ?? "0".repeat(64);
  out["resolved"] = await coordinator.resolveLocalRecord({ runId: spec.runId, generation: spec.generation, confirm: digest, ...sig });
  writeResult(out);
}
process.exit(0);
