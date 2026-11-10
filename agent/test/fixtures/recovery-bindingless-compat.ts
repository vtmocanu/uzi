import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs/promises";
import path from "node:path";
import { GitCache, type PositiveOwedCandidateContext } from "../../src/git.js";
import { RecoveryCoordinator, type RecoveryArchiveClient, type RecoveryBundleProducer, type RecoveryRecord } from "../../src/recovery.js";
import type { RecoveryExactHold } from "../../src/protocol.js";
import { nullLogger } from "../helpers.js";

const workerToken = "bindingless-compat-fixture-worker";
const id = (n: number) => `31050000-0000-4000-8000-${String(n).padStart(12, "0")}`;
export const binding = { holdId: id(1), repoId: id(2) };
export type Variant = "pinned" | "needs_action" | "uploaded" | "saved-final" | "acknowledged" | "completion-receipt";
export type Reader = typeof RecoveryCoordinator;
export interface Fixture {
  root: string;
  context: PositiveOwedCandidateContext;
  head: string;
  attempts: string[];
  record: RecoveryRecord;
}
export function git(bare: string, ...args: string[]): string {
  return execFileSync("git", ["--no-replace-objects", "-C", bare,
    "-c", "maintenance.auto=false", "-c", "gc.auto=0", "-c", "core.fsmonitor=false",
    "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid",
    "-c", "commit.gpgsign=false", ...args], {
    encoding: "utf8", stdio: "pipe", timeout: 10_000,
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
}

/** Real Git production/proofs; the API and physical source boundary are controlled seams. */
export function environment(f: Fixture, ReaderClass: Reader = RecoveryCoordinator, token = workerToken) {
  const calls = { produce: 0, reserve: 0, upload: 0, final: 0, held: 0, cleanup: 0 };
  const warnings: string[] = [];
  const log = nullLogger();
  log.warn = message => { warnings.push(message); };
  const state = { failProduce: false, allowFinal: false, boundary: true, healthy: true, open: true };
  const cache = new GitCache(path.join(f.root, "data"), nullLogger());
  const bare = f.context.barePath;
  const refs = ["refs/owed/fixture", "refs/recovery/fixture", "refs/attempted/one", "refs/attempted/two"];
  const candidates = () => [{ sha: git(bare, "rev-parse", refs[0]!), pinRef: refs[0]!, contexts: [f.context] }];
  const client: RecoveryArchiveClient = {
    hasFeature: () => true,
    getRunOwnership: async () => ({ status: "failed", claim_generation: 7, inventory_guarded: true }),
    listRecoveryHolds: async () => ({ run_id: f.context.runId, holds: state.open ? [{
      hold_id: binding.holdId, generation: 7, inventory_guarded: true, has_available_capture: false,
    }] : [], run_open_holds: { run_id: f.context.runId, count: state.open ? 1 : 0 } }),
    reserveRecoveryCapture: async () => {
      calls.reserve++;
      return { capture_id: id(3), state: "preparing" };
    },
    uploadRecoveryBundle: async (_run, _capture, _manifest, stream) => {
      calls.upload++;
      for await (const chunk of stream) assert.ok(chunk);
      return { capture_id: id(3), state: "available", manifest_bound: true };
    },
    getRecoveryCaptureStatus: async () => {
      const latest = (await coordinator.inspect(f.context.runId))[0] ?? f.record;
      return {
        capture_id: id(3), state: "available", manifest_bound: true,
        checksum: latest.checksum, byte_size: latest.byteSize,
        expires_at: state.healthy ? "2099-01-01T00:00:00Z" : "1970-01-01T00:00:00Z",
      };
    },
    releaseRecoveryCustody: async () => {
      calls.final++;
      if (!state.allowFinal) throw new Error("fixture: lost FINAL ACK");
      return { run_id: f.context.runId, generation: 7, released: true, holds_released: 1 };
    },
    publishHeld: async () => { calls.held++; throw new Error("unexpected held publication"); },
  };
  const producer: RecoveryBundleProducer = {
    enumerateOwedCandidates: async () => candidates(),
    buildRecoveryCoverage: cache.buildRecoveryCoverage.bind(cache),
    ancestry: cache.ancestry.bind(cache),
    readInventoryCloneHeads: async () => ({ kind: "verified", heads: [], clones: [], foreignOwners: [] }),
    resolveRecoveryBareDir: async name => name === path.basename(bare) ? bare : undefined,
    fetchDefaultTip: async () => { throw new Error("credentialed fetch forbidden"); },
    produceRecoveryBundle: async (dir, options, signal) => {
      calls.produce++;
      if (state.failProduce) throw new Error("fixture bundle failure");
      const result = await cache.produceRecoveryBundle(dir, { sourceSha: options.sourceSha, outPath: options.outPath }, signal);
      git(bare, "update-ref", refs[1]!, options.sourceSha);
      return result;
    },
  };
  const make = () => new ReaderClass({
    recoveryRoot: path.join(f.root, "data", "recovery"), workerToken: token,
    client, git: producer, log, now: () => 1000,
    withCompletionSourceBoundary: async (source, action) => {
      assert.equal(git(source.clonePath, "rev-parse", "HEAD"), source.expectedHead);
      assert.equal(await fs.readFile(path.join(source.clonePath, "retained.txt"), "utf8"), "retained source before rollback\n");
      await fs.rm(source.clonePath, { recursive: true });
      await action(() => true);
      return "passed";
    },
    withInventorySourceBoundary: async (_context, action) => {
      if (!state.boundary) return "retained";
      await action(async () => true);
      return "passed";
    },
  });
  const coordinator = make();
  const cleanup = () => {
    producer.cleanupRecoveryGeneration = async (_bare, _context, _roots, _pins, _sources, _head, canDelete = () => true, mode) => {
      calls.cleanup++;
      assert.equal(canDelete(), true);
      assert.equal(git(bare, "rev-parse", refs[0]!), f.head);
      if (mode === "completion-check") return "verified";
      for (const ref of refs) git(bare, "update-ref", "-d", ref);
      return "removed";
    };
  };
  return { coordinator, state, calls, cleanup, refs, warnings };
}

/** Private access follows the recovery suite convention, but retains the real lock and MAC writer. */
export async function patch(coordinator: RecoveryCoordinator, record: RecoveryRecord, fields: Partial<RecoveryRecord>) {
  return coordinator["writeExistingRecord"](record, current => ({ ...current, ...fields }));
}
export function closed(f: Fixture): RecoveryExactHold {
  return {
    run_id: f.context.runId, generation: 7, hold_id: binding.holdId, repo_id: binding.repoId,
    inventory_guarded: true, state: "discarded", run_status: "failed",
    current_claim_generation: 7, claim_released: true, release_evidence: "owner_discard",
    original_worker_id: id(5), original_worker_identity: "compat-fixture-worker",
  };
}
export async function fixture(root: string, variant: Variant): Promise<Fixture> {
  const bare = path.join(root, "data", "repos", "fixture.git");
  await fs.mkdir(bare, { recursive: true, mode: 0o700 });
  git(bare, "init", "--bare", "--initial-branch=main");
  // Write actual source content and executable mode into the Git object graph.
  const source = path.join(root, "source.txt");
  await fs.writeFile(source, "retained source before rollback\n", { mode: 0o600 });
  const blob = git(bare, "hash-object", "-w", source);
  const tree = execFileSync("git", ["-C", bare, "mktree"], {
    input: `100755 blob ${blob}\tretained.txt\n`, encoding: "utf8", timeout: 10_000,
    env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  }).trim();
  const base = git(bare, "commit-tree", tree, "-m", "base");
  const head = git(bare, "commit-tree", tree, "-p", base, "-m", "original");
  const one = git(bare, "commit-tree", tree, "-p", head, "-m", "attempt one");
  const two = git(bare, "commit-tree", tree, "-p", one, "-m", "attempt two");
  git(bare, "update-ref", "refs/remotes/origin/main", base);
  git(bare, "update-ref", "refs/heads/main", base);
  git(bare, "update-ref", "refs/owed/fixture", head);
  git(bare, "update-ref", "refs/attempted/one", one);
  git(bare, "update-ref", "refs/attempted/two", two);
  const context: PositiveOwedCandidateContext = {
    runId: id(4), generation: 7, kind: "issue", branch: "agent/issue-3105", barePath: bare,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: base },
  };
  const f: Fixture = { root, context, head, attempts: [one, two], record: undefined as unknown as RecoveryRecord };
  const env = environment(f);
  const frozen = await env.coordinator.freezeInventory({ context, currentSha: head, defaultBranch: "main" });
  assert.ok(frozen, "coordinator must create the initial journal");
  f.record = await patch(env.coordinator, frozen, { attemptedHeadSha: one, attemptedHeadShas: [one, two] });
  if (variant !== "pinned") {
    env.state.failProduce = variant === "needs_action";
    // Let upload finish but pause FINAL at the physical boundary for the no-FINAL pair.
    env.state.boundary = variant === "saved-final" || variant === "acknowledged";
    env.state.allowFinal = variant === "acknowledged";
    await env.coordinator.captureAndUpload({ record: f.record, barePath: bare, defaultBranch: "main",
      onArchiveAccepted: record => { f.record = record; } });
  }
  f.record = (await env.coordinator.inspect(context.runId))[0]!;
  assert.ok(f.record);
  if (variant === "completion-receipt") {
    const clonePath = path.join(root, "completing-source");
    execFileSync("git", ["clone", "--no-hardlinks", "--no-checkout", bare, clonePath], { stdio: "pipe", timeout: 10_000 });
    git(clonePath, "checkout", "--detach", head);
    assert.equal(await env.coordinator.bindCompletionSource({ context, clonePath, expectedHead: head,
      roots: [{ sha: head, contexts: [context] }] }), true);
    const receipt = JSON.parse(await fs.readFile(new URL("../../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8")).ack.completed_publication_receipt;
    assert.equal(await env.coordinator.persistCompletionReceipt({ ...receipt, run_id: context.runId,
      generation: 7, branch: context.branch, final_head: head, observed_branch_head: head }), true);
    f.record = (await env.coordinator.inspect(context.runId))[0]!;
  }
  assert.equal(f.record.state, variant === "pinned" ? "pinned" : variant === "needs_action" ? "needs_action" : "uploaded");
  assert.equal(!!f.record.finalRequest, variant === "saved-final" || variant === "acknowledged");
  assert.equal(f.record.finalAcknowledged === true, variant === "acknowledged");
  assert.deepEqual(f.record.attemptedHeadShas, [one, two]);
  await fs.mkdir(path.join(root, "reports"), { mode: 0o700 });
  await fs.writeFile(path.join(root, "reports", "pending.json"), JSON.stringify({ runId: context.runId, generation: 7 }), { mode: 0o600 });
  return f;
}

interface Entry { path: string; mode: number; kind: string; bytes?: string }
export async function inventory(root: string): Promise<Entry[]> {
  const entries: Entry[] = [];
  // Finite fixture tree, visited once; any failed read aborts the inventory.
  async function visit(relative: string): Promise<void> {
    const file = path.join(root, relative), stat = await fs.lstat(file);
    assert.equal(stat.isSymbolicLink(), false, file);
    entries.push({ path: relative, mode: stat.mode & 0o7777, kind: stat.isDirectory() ? "directory" : "file",
      ...(stat.isFile() ? { bytes: (await fs.readFile(file)).toString("base64") } : {}) });
    if (stat.isDirectory()) for (const name of (await fs.readdir(file)).sort()) await visit(path.join(relative, name));
  }
  await visit(".");
  return entries;
}
export async function equalInventory(root: string, expected: Entry[], diagnosticJournal?: string): Promise<void> {
  const actual = await inventory(root);
  assert.equal(actual.length, expected.length, "tree inventory entry count");
  for (let i = 0; i < expected.length; i++) {
    if (actual[i]!.path === diagnosticJournal) actual[i]!.bytes = expected[i]!.bytes;
    assert.ok(JSON.stringify(actual[i]) === JSON.stringify(expected[i]), `first inventory mismatch: ${expected[i]!.path}`);
  }
}
export async function snapshot(root: string, destination: string) {
  const entries = await inventory(root);
  await fs.cp(root, destination, { recursive: true, preserveTimestamps: true });
  await equalInventory(destination, entries);
  await fs.writeFile(destination + ".inventory.json", JSON.stringify(entries));
  return { directory: destination, entries };
}
export async function restore(root: string, saved: Awaited<ReturnType<typeof snapshot>>) {
  await fs.rm(root, { recursive: true, force: true });
  await fs.cp(saved.directory, root, { recursive: true, preserveTimestamps: true });
  await equalInventory(root, saved.entries);
}
export async function sourceProof(f: Fixture) {
  assert.equal(git(f.context.barePath, "show", f.head + ":retained.txt"), "retained source before rollback");
  assert.match(git(f.context.barePath, "ls-tree", f.head), /^100755 blob /);
  for (const [ref, sha] of [["refs/owed/fixture", f.head], ["refs/attempted/one", f.attempts[0]!],
    ["refs/attempted/two", f.attempts[1]!]]) assert.equal(git(f.context.barePath, "rev-parse", ref!), sha);
  if (f.record.bundlePath) {
    const bytes = await fs.readFile(f.record.bundlePath);
    assert.equal(createHash("sha256").update(bytes).digest("hex"), f.record.checksum);
    assert.equal(bytes.length, f.record.byteSize);
    git(f.context.barePath, "bundle", "verify", f.record.bundlePath);
    assert.match(git(f.context.barePath, "bundle", "list-heads", f.record.bundlePath), new RegExp(f.record.sourceSha));
  }
}
