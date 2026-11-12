// issue #3885 M2 U3: the worker-process half of the restart test (test/recovery-local-record-restart.test.ts).
//
// Run as `node --import tsx --import ./test/setup/hermetic-proc.ts <this file> <spec.json>` from agent/. It builds
// the production shape of the operator path over a data directory the parent seeded: a real AuthorityRuntime (the
// worker lifetime lock W is taken for real), a real GitCache bound to its hooks with `authorityRequired`, and a
// RecoveryCoordinator whose only fakes are the physical source boundary and the coordinator's own client slot, which
// carries the REAL WorkerClient (registered, then speaking HTTP to the fake worker API the parent hosts on 127.0.0.1;
// the parent persists that API's state in a JSON file, so a later, fresh process meets the earlier POSTs). At the named seam the child writes a marker file and blocks forever; the parent kills it with a
// real SIGKILL (no handler, no finally, no atexit runs).
import fsSync from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { acquireWorkerLifetime } from "../../src/authority-lock.js";
import { AuthorityRuntime } from "../../src/authority-runtime.js";
import { WorkerClient } from "../../src/client.js";
import { GitCache } from "../../src/git.js";
import type { RecoveryRecord } from "../../src/recovery.js";
import { testLockOptions } from "../authority-test-support.js";
import { nullLogger } from "../helpers.js";
import { environment, type Fixture } from "./recovery-bindingless-compat.js";
import { fakeSourceEvidence } from "./recovery-case-b-evidence.js";

export const CHILD_AUTHORITY_TOKEN = "restart-child-authority-token";

/** Every point at which the child can be told to stop forever. */
export type KillAt =
  | "pre-post"            // inside the inventory source boundary, before the release POST leaves the process
  | "post-before-ack"     // the server recorded and answered the POST, the real client returned it; the ACK write has not started
  | "ack-tmp-written"     // the ACK temp file is on disk; the rename has not run
  | "ack-renamed"         // the ACK is durable; no cleanup has started
  | "after-begin-bare"    // the first shared-bare deletion is registered with the authority; the ref is untouched
  | "after-bundle-unlink" // the generation's bundle is gone; no record has been unlinked
  // issue #3885 Case B: stop at the n-th step of the record disposition (the m1 seam onOperatorDisposeStepForTest), counted from 0
  // within the one resolution that runs. The parent learns the step sequence of a world from an unkilled control child.
  | `case-b-step:${number}`;

export interface ChildSpec {
  root: string;
  runId: string;
  generation: number;
  context: Fixture["context"];
  head: string;
  attempts: string[];
  action: "describe" | "resolve";
  killAt?: KillAt;
  /** issue #3885: resolve as the Case B disposition (the `case_b` action), with the injected fake source evidence. */
  caseB?: boolean;
  /** issue #3885: a digest to try (and have refused) before the freshly described one is confirmed. */
  staleConfirm?: string;
  marker: string;
  result: string;
  /** Base URL of the fake worker API the parent hosts (http://127.0.0.1:<port>). */
  apiUrl: string;
}

/** The fake worker API's persisted state (the parent owns the file; the child never touches it). */
export interface ServerState { posts: Array<{ evidence: unknown; disposition: unknown }> }

const spec = JSON.parse(fsSync.readFileSync(process.argv[2]!, "utf8")) as ChildSpec;
// A child that outlives its parent for any reason ends itself.
setTimeout(() => process.exit(97), 150_000);

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

// The REAL WorkerClient, registered against the parent's HTTP fake: the exact-hold read and the release POST are its
// own request construction, validation and response handling. The parent's API models the real replay rule
// (api/internal/recovery/final_inventory.go): an identical released request is answered with the same ack, a different one
// is refused with a conflict.
const real = new WorkerClient(spec.apiUrl, "restart-child-api-token", "test", nullLogger());
await real.register("worker", undefined, 1, undefined, ["recovery_held_publication_v2"], undefined, 0);
client.getExactRecoveryHold = (...args: Parameters<WorkerClient["getExactRecoveryHold"]>) => real.getExactRecoveryHold(...args);
client.releaseRecoveryCustody = async (...args: Parameters<WorkerClient["releaseRecoveryCustody"]>) => {
  if (at("pre-post")) await block("pre-post");
  const ack = await real.releaseRecoveryCustody(...args);
  // The HTTP response has been received and decoded by the client; the coordinator has not yet persisted the ACK.
  if (at("post-before-ack")) await block("post-before-ack");
  return ack;
};

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

// issue #3885 Case B: the physical source evidence is the shared injected fake (the same bytes in every process, so a digest
// described by one process is the digest a fresh one computes); each disposition step is a named stop point.
const steps: string[] = [];
if (spec.caseB) {
  let n = 0;
  Object.assign(coordinator, {
    operatorSourceEvidence: async () => fakeSourceEvidence(spec.runId, spec.context.barePath),
    onOperatorDisposeStepForTest: async (step: { kind: string; path: string; phase: string }) => {
      steps.push(`${step.kind}:${step.phase}:${path.basename(step.path)}`);
      const point = `case-b-step:${n++}` as const;
      if (spec.killAt === point) await block(point);
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
  const action = spec.caseB ? { action: "case_b" as const } : {};
  if (spec.staleConfirm !== undefined) {
    out["stale"] = await coordinator.resolveLocalRecord({ runId: spec.runId, generation: spec.generation, confirm: spec.staleConfirm, ...action, ...sig });
    writeResult(out);
  }
  out["resolved"] = await coordinator.resolveLocalRecord({ runId: spec.runId, generation: spec.generation, confirm: digest, ...action, ...sig });
  out["steps"] = steps;
  writeResult(out);
}
process.exit(0);
