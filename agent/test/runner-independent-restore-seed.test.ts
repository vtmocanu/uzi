// Issue #3644 Gate B (m4b part 1): the independent restore. Production RunRunner + real Git + the durable authority, driven through
// RunRunner.execute() with the m4a rig (restore-route-rig.ts): the predecessor P is a real attempt clone with unpinned commits and
// dirty bytes, its slot ACTIVE under a dead incarnation; the successor S is the runner's own AuthorityRuntime.
//
// An eligible verdict now seeds ONE fresh attempt from the verified pin (never from P's clone), swaps the flight onto an executor
// and HOME of its own (`<runId>.g<N>`), and runs. Every refusal is a nonterminal stop that keeps P and writes nothing but what the
// seed's windows document.
import assert from "node:assert/strict";
import { createHash, randomUUID } from "node:crypto";
import fsp from "node:fs/promises";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { afterEach, beforeEach, describe, it, mock, type TestContext } from "node:test";
import type { Executor } from "../src/executor.js";
import { StubExecutor } from "../src/executor.js";
import { AuthorityRuntime } from "../src/authority-runtime.js";
import { readRetainedHomeMarker } from "../src/retained-homes.js";
import { parseRestoreValue } from "../src/restore-journal.js";
import type { RecoveryRecord } from "../src/recovery.js";
import type { SlotSnapshot } from "../src/completion-authority.js";
import { boundaryFileName } from "../src/restore-boundary.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { nullLogger } from "./helpers.js";
import { Outbox } from "../src/outbox.js";
import { REPO_AGENT_SOURCE_FEATURE, type StateRequest } from "../src/protocol.js";
import { api, baseUrl, client, fakeGitlab, fx, git, installHarness, runnerWith } from "./runner-harness.js";
import {
  AUTH_TOKEN, BRANCH, CID, FEATURES, PARK_FEATURE, armTraps, buildRig, cfgAdd, cfgAll, cfgGet, cfgSet, cleanupRig, failed, gitIn, journalOf, logged, makeProc, refusals, resetRigRoots, rigDefaults, slotOf,
  treeDigest, type Rig, type RigOptions,
} from "./restore-route-rig.js";

installHarness();
rigDefaults.independentRestore = true;

afterEach(() => { cleanupRig(); });

const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];

/** P's clone gets a genuinely newer, UNPINNED commit; its dirty `ONLY_COPY.txt` is already there. Returns the new HEAD. */
function advanceP(r: Rig): string {
  fs.writeFileSync(path.join(r.pPath, "UNPINNED.txt"), "newer than the pin\n");
  gitIn(r.pPath, ["add", "UNPINNED.txt"]);
  gitIn(r.pPath, [...IDENT, "commit", "-m", "unpinned work"]);
  fs.writeFileSync(path.join(r.pPath, "DIRTY.txt"), "dirty bytes that only the predecessor's clone holds\n");
  return gitIn(r.pPath, ["rev-parse", "HEAD"]);
}

const sha256 = (file: string): string => createHash("sha256").update(fs.readFileSync(file)).digest("hex");

/** Everything the restore must leave byte-identical on and around P. */
async function pFacts(r: Rig) {
  return {
    journal: cfgGet(r, r.journalKey), pLedger: cfgAll(r, r.ledgerKey)[0], slot: await slotOf(r, r.pPath),
    clone: treeDigest(r.pPath), home: treeDigest(r.runHome), provision: treeDigest(r.provisionDir),
  };
}

/** The bare's config, but for the keys a restore legitimately adds. */
const configText = (r: Rig): string => fs.readFileSync(path.join(r.bare, "config"), "utf8");
const markerFile = (r: Rig): string => path.join(r.dataDir, "retained-homes", `${r.runId}.json`);
const restoreKey = `uzi-restore.${BRANCH}.journal`;
const restoreValue = (r: Rig): ReturnType<typeof parseRestoreValue> => parseRestoreValue(cfgGet(r, restoreKey)!);

/** An executor that records what S sees when it runs, then completes through the real stub. */
function observingExecutor(r: { current?: Rig }, seen: Record<string, unknown>): Executor {
  const stub = new StubExecutor(nullLogger());
  const wrapped = Object.create(stub) as Executor;
  wrapped.run = async ctx => {
    seen.worktree = ctx.worktreePath;
    seen.head = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
    seen.tracked = gitIn(ctx.worktreePath, ["ls-files"]).split("\n");
    seen.untracked = fs.existsSync(path.join(ctx.worktreePath, "ONLY_COPY.txt")) || fs.existsSync(path.join(ctx.worktreePath, "DIRTY.txt"));
    const rig = r.current!;
    seen.restoreValue = cfgGet(rig, restoreKey);
    seen.ledger = cfgAll(rig, rig.ledgerKey);
    seen.journalP = cfgGet(rig, rig.journalKey);
    seen.marker = fs.existsSync(markerFile(rig)) ? fs.readFileSync(markerFile(rig), "utf8") : undefined;
    fs.mkdirSync(path.join(rig.homeRoot, `${rig.runId}.g${rig.claim.claim_generation}`), { recursive: true });
    return stub.run(ctx);
  };
  return wrapped;
}

async function restoreRig(o: RigOptions = {}): Promise<{ r: Rig; seen: Record<string, unknown> }> {
  const holder: { current?: Rig } = {};
  const seen: Record<string, unknown> = {};
  const r = await buildRig({ addrAware: true, successorExecutor: observingExecutor(holder, seen), ...o });
  holder.current = r;
  return { r, seen };
}

describe("the independent restore (issue #3644 m4b)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("seeds one distinct attempt from the verified pin, runs it on HOME <runId>.g<N>, and leaves the predecessor byte-identical", async () => {
    const { r, seen } = await restoreRig();
    const pHead = advanceP(r);
    assert.notEqual(pHead, r.pinSha, "the predecessor holds work newer than the pin");
    const homeBytes = sha256(path.join(r.runHome, "session-proof"));
    const before = await pFacts(r);
    const traps = armTraps(r);
    const seedingAt: number[] = [];
    const gitLog = (git as unknown as { log: { info(msg: string, ...rest: unknown[]): void } }).log;
    const logInfo = gitLog.info.bind(gitLog);
    mock.method(gitLog, "info", (msg: string, ...rest: unknown[]) => {
      if (msg === "runner clone: seeding") seedingAt.push(r.logs.lines.length);
      return logInfo(msg, ...rest);
    });
    try { await r.runner.execute(r.claim); } finally { traps.armed = false; }

    // P is never read, captured, reserved, admitted or reaped.
    assert.deepEqual(traps.hits.slice(0, 5), [], "no fs or git call named P's path");
    assert.deepEqual(traps.calls, {}, "no reservation, episode block, capture, predecessor admission or legacy discovery");

    // The route's contract.
    assert.equal(failed(r).length, 0);
    assert.equal(refusals(r).length, 0, JSON.stringify(refusals(r)));
    const seeded = logged(r, "independent restore seeded");
    assert.equal(seeded.length, 1);
    const note = seeded[0]!;
    assert.equal(note.pinGeneration, 1);
    assert.equal(note.sourceSha, r.pinSha);
    assert.equal(note.binding, "on_default");
    assert.equal(note.recordState, "pinned");
    assert.equal(note.seededHead, r.pinSha, "S starts at the pin, not at P's newer commit");
    assert.equal((note.verdict as { predecessorIncarnation: string }).predecessorIncarnation, r.pInc);
    assert.match(String(note.captureId), /./);

    // S: a distinct -g2- attempt at the pin; P's unpinned work is not in it.
    assert.match(String(note.attemptPath), /\.attempt-[0-9TZ]+-g2-/);
    assert.notEqual(note.attemptPath, r.pPath);
    assert.equal(seen.worktree, note.attemptPath);
    assert.equal(seen.head, r.pinSha);
    assert.equal((seen.tracked as string[]).includes("UNPINNED.txt"), false);
    assert.equal(seen.untracked, false, "P's dirty bytes were never copied");

    // The basis and the entry the seed persisted before S ran.
    const during = parseRestoreValue(seen.restoreValue as string);
    assert.equal(during.kind, "entry");
    if (during.kind !== "entry") return;
    assert.equal(during.stage, "ready");
    assert.equal(during.basis.restoreTip, r.pinSha);
    assert.equal(during.basis.pin.sourceSha, r.pinSha);
    assert.equal(during.basis.pin.generation, 1);
    assert.equal(during.basis.predecessor.clonePath, r.pPath);
    assert.equal(during.basis.predecessor.attemptId, r.pAttempt);
    assert.equal(during.basis.slotOwner.worker, r.pInc);
    assert.match(during.basis.boundary.predecessor, /^[0-9a-f]{64}$/);
    assert.equal(during.remainder.clonePath, note.attemptPath);
    assert.match(String(during.remainder.attemptId), /-g2-/);
    assert.equal(seen.journalP, before.journal, "P's key is never written");
    assert.deepEqual((seen.ledger as string[])[0], before.pLedger);
    assert.equal((seen.ledger as string[]).length, 2, "S's live entry sits beside P's");
    assert.deepEqual(JSON.parse(seen.marker as string), {
      version: 1, runId: r.runId, predecessorGeneration: 1, predecessorAttemptId: r.pAttempt, barePath: r.bare });

    // The executors: the initial build, then exactly one successor build at {generation: 2}.
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined, 2]);
    assert.equal(r.factoryCalls[1]!.home, path.join(r.homeRoot, `${r.runId}.g2`));

    // P and everything around it is untouched, after the run too.
    assert.deepEqual(await pFacts(r), before);
    assert.equal(sha256(path.join(r.runHome, "session-proof")), homeBytes);
    assert.equal(fs.existsSync(path.join(r.homeRoot, `${r.runId}.g2`)), false, "S's own HOME is what teardown removed");
    assert.equal(fs.readFileSync(path.join(r.pPath, "DIRTY.txt"), "utf8").length > 0, true);
    assert.equal(gitIn(r.pPath, ["rev-parse", "HEAD"]), pHead);
    const marker = await readRetainedHomeMarker(r.dataDir, r.runId);
    assert.equal(marker.kind, "marker");

    // The feed line: honest about what was and was not restored, bounded.
    const feed = api.messages(r.runId).map(m => JSON.stringify(m.payload)).filter(t => t.includes("continues from verified saved work"));
    assert.equal(feed.length, 1);
    assert.match(feed[0]!, /pin [^ ]+ \(generation 1, sha [0-9a-f]{12}\)/);
    assert.match(feed[0]!, /later unpinned edits may remain only in the retained predecessor on the worker \(not measured\)/);
    assert.match(feed[0]!, /merging closes only the server custody holds; the retained predecessor's worker-side copy stays until the owner's guarded discard/);

    // Order: the claim, then the git cache's own seeding line (its logger is not the run's, so it is positioned by how many run
    // lines existed when it was written), then the restore's seeded line.
    const runMsgs = r.logs.lines.map(l => (l as { msg: string }).msg);
    const claimedAt = runMsgs.indexOf("run claimed");
    const seededAt = runMsgs.indexOf("independent restore seeded");
    assert.ok(claimedAt >= 0 && seededAt > claimedAt, `run claimed precedes independent restore seeded: ${runMsgs.slice(0, 6).join(" | ")}`);
    assert.equal(seedingAt.length, 1, "the git cache logged `runner clone: seeding` once");
    assert.ok(seedingAt[0]! > claimedAt && seedingAt[0]! <= seededAt, `seeding at run-line ${seedingAt[0]}, claimed ${claimedAt}, seeded ${seededAt}`);
    assert.ok(feed[0]!.length < 700);
    assert.equal(api.messages(r.runId).map(m => JSON.stringify(m.payload)).filter(t => /recovered \d+ commit/.test(t)).length, 0,
      "no 'recovered N commits' claim");
    assert.equal(r.logs.lines.filter(l => /bundle uploaded/.test(String((l as { msg: string }).msg))).length, 0, "P is never captured");
  });

  it("a later claim while S's entry is live stops nonterminally and writes nothing", async () => {
    const { r } = await restoreRig({ executor: undefined });
    advanceP(r);
    await r.runner.execute(r.claim);
    // S completed without a certified retirement in this rig, so its entry is still live.
    const stage = restoreValue(r);
    assert.equal(stage.kind, "entry");
    const config = configText(r);
    const before = await pFacts(r);
    const second = { ...r.claim, claim_generation: 3 };
    api.setOwnershipStatus(r.runId, "running", 3);
    await r.runner.execute(second);
    assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
    assert.equal(failed(r).length, 0);
    assert.equal(configText(r), config);
    assert.deepEqual(await pFacts(r), before);
    assert.equal(r.factoryCalls.filter(c => c.generation === 3).length, 0, "no successor executor was built for it");
  });
});

describe("the restored attempt completes through the certified completion path (issue #3644 3.9)", () => {
  beforeEach(() => { resetRigRoots(); });

  const wire = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8"));
  const literal = wire.ack.completed_publication_receipt;
  const FEATURES = ["active_run_snapshot", "claim_generation_fence", "recovery_inventory_v1", "recovery_completed_publication_v1", "terminal_rejection_report"];

  function commit(dir: string, name: string): string {
    fs.writeFileSync(path.join(dir, name), name + "\n");
    gitIn(dir, ["add", name]);
    gitIn(dir, [...IDENT, "commit", "-m", name]);
    return gitIn(dir, ["rev-parse", "HEAD"]);
  }

  /** The certified completion of a restored attempt (P unpinned work, S through checkpoint, PR and receipt). Returns the rig and the facts. */
  async function completeRestore(t: TestContext, interrupt?: { arm: (r: Rig) => void; resume?: boolean }) {
    api.setRegisterProtocolFeatures(FEATURES);
    const forge = fakeGitlab();
    let finalHead = "", checkpointHead = "", clone = "";
    const holder: { current?: Rig } = {};
    const successor: Executor = {
      run: async ctx => {
        clone = ctx.worktreePath;
        assert.equal(gitIn(clone, ["rev-parse", "HEAD"]), holder.current!.pinSha, "S starts at the pin");
        checkpointHead = commit(clone, "checkpoint.txt");
        await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
        finalHead = commit(clone, "post-checkpoint.txt");
        fs.writeFileSync(path.join(clone, "deliberate-leftover.txt"), "discard only after safe retirement\n");
        forge.pr.head = finalHead;
        return { branch: ctx.branch };
      },
    };
    const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(), runMaxBytes: 64 << 20, maxBytes: 128 << 20, retentionMs: 86_400_000 });
    await outbox.init();
    const r = await buildRig({ addrAware: true, successorExecutor: successor, features: FEATURES, gitlab: forge.gitlab,
      claim: { repo: { id: literal.repo_id, url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, default_branch: "main", forge_type: "gitlab" } },
      runnerExtra: { outbox, checkpointIntervalMs: 1, checkpointTickIntervalMs: 0 } });
    holder.current = r;
    advanceP(r);
    const registration = await client.register("restore-completion", undefined, undefined, undefined, FEATURES);
    assert.ok(registration.worker_id);
    const claim = r.claim;
    let open = true, manifest: { checksum: string; byte_size: number } | undefined;
    client.getRunOwnership = async () => ({ status: api.states.some(s => s.body.status === "completed") ? "completed" : "running",
      claim_generation: claim.claim_generation, inventory_guarded: true });
    client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
      hold_id: literal.hold_id, generation: claim.claim_generation!, inventory_guarded: true, has_available_capture: !!manifest }] : [] });
    client.reserveRecoveryCapture = async () => ({ capture_id: "00000000-0000-4000-8000-000000002507", state: "preparing" });
    client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
      for await (const chunk of stream) assert.ok(chunk);
      manifest = m;
      return { capture_id: "00000000-0000-4000-8000-000000002507", state: "available", manifest_bound: true };
    };
    client.getRecoveryCaptureStatus = async () => ({ capture_id: "00000000-0000-4000-8000-000000002507", state: manifest ? "available" : "preparing",
      manifest_bound: !!manifest, checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: new Date(Date.now() + 60_000).toISOString() });
    client.releaseRecoveryCustody = async (runId, generation) => {
      open = false;
      return { run_id: runId, generation, released: true, holds_released: 1 };
    };
    client.publishCheckpoint = async (_run, tip, pack) => {
      const chunks: Buffer[] = [];
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"], { input: Buffer.concat(chunks), env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } });
      gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${BRANCH}`, tip]);
      return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${BRANCH}` } };
    };
    const originalFetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (url.origin === new URL(baseUrl).origin && url.pathname.endsWith("/terminal-rejection-custody")) {
        return Response.json({ run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
          exact_holds: [{ id: literal.hold_id, state: open ? "open" : "released" }], sibling_holds: [], exact_count: 1, sibling_count: 0,
          complete: true, exact_complete: true, sibling_complete: true, outcome: open ? "retained" : "settled" });
      }
      const response = await originalFetch(input, init);
      if (url.origin !== new URL(baseUrl).origin || url.pathname !== `/api/worker/runs/${claim.run_id}/state`) return response;
      const body = JSON.parse(String(init?.body)) as StateRequest;
      if (body.status !== "completed") return response;
      open = false;
      return Response.json({ ...wire.ack, run: { ...wire.ack.run, id: claim.run_id, worker_id: registration.worker_id },
        completed_publication_receipt: { ...literal, run_id: claim.run_id, worker_id: registration.worker_id, generation: claim.claim_generation,
          repo_id: claim.repo!.id, branch: BRANCH, final_head: finalHead, observed_branch_head: finalHead, mr_iid: body.mr_iid } });
    });
    const retires = t.mock.method(git, "retireRunnerClone");
    const before = await pFacts(r);
    if (interrupt) {
      // A crash window: the caller injects the interruption; nothing below asserts the happy path.
      interrupt.arm(r);
      await r.runner.execute(claim);
      if (interrupt.resume) await r.runner.resumePendingRecoveries();
      return { r, claim, clone, before, checkpointHead, finalHead, retires, registration };
    }
    await r.runner.execute(claim);
    assert.equal(failed(r).length, 0, JSON.stringify(api.states.filter(s => s.body.status === "failed").map(s => s.body)));
    assert.equal(api.states.filter(s => s.body.status === "completed").length, 1);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    await r.runner.resumePendingRecoveries();
    return { r, claim, clone, before, checkpointHead, finalHead, retires, registration };
  }

  it("retires S under its certified receipt: the entry becomes a tombstone, P's key and slot are never cleared, and a restart changes nothing", async t => {
    const { r, claim, clone, before, checkpointHead, finalHead, retires } = await completeRestore(t);
    const sRetire = retires.mock.calls.filter(c => (c.arguments[1] as string) === clone);
    assert.ok(sRetire.length >= 1, "S's clone was retired");
    assert.ok(sRetire.some(c => (c.arguments[4] as { completionSource?: unknown }).completionSource !== undefined), "through the certified completion source");
    assert.equal(retires.mock.calls.filter(c => (c.arguments[1] as string) === r.pPath).length, 0, "P is never retired");
    assert.equal(fs.existsSync(clone), false, "S's clone is gone");
    assert.notEqual(checkpointHead, finalHead);
    assert.equal(gitIn(fx.originPath, ["rev-parse", `refs/uzi-checkpoints/${BRANCH}`]), checkpointHead, "the real checkpoint was delivered");

    const tomb = restoreValue(r);
    assert.equal(tomb.kind, "tombstone");
    if (tomb.kind !== "tombstone") return;
    assert.equal(tomb.basis.restoreTip, r.pinSha);
    assert.equal(tomb.successor.clonePath, clone);
    const ledgers = new Map(cfgAll(r, r.ledgerKey).map(l => JSON.parse(l) as { attemptId: string; state: string; clonePath: string }).map(l => [l.clonePath, l.state]));
    assert.equal(ledgers.get(clone), "retired", "the last ledger value of S is retired");
    assert.equal(ledgers.get(r.pPath), "live", "P's ledger entry is never touched");
    assert.deepEqual(await pFacts(r), before, "P's journal, ledger, slot, clone, HOME and provision directory are byte-identical");

    // Another restart: a new authority runtime and runner meet the tombstone and change nothing.
    const after = { config: configText(r), p: await pFacts(r), marker: fs.readFileSync(markerFile(r), "utf8") };
    const again = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: AUTH_TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
    const runner2 = runnerWith(() => ({ executor: r.executor, homeDir: r.runHome }), fakeGitlab().gitlab, AUTH_TOKEN, r.logs.logger, {
      authority: again, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5, restoreIdentity: { procRoot: r.sProc, hostname: () => CID.slice(0, 12) } });
    api.setOwnershipStatus(r.runId, "running", 3);
    await runner2.execute({ ...claim, claim_generation: 3 });
    assert.equal(configText(r), after.config, "the tombstone and every attribution key are unchanged");
    assert.deepEqual(await pFacts(r), after.p);
    assert.equal(fs.readFileSync(markerFile(r), "utf8"), after.marker);
    assert.equal(cfgGet(r, r.journalKey), before.journal, "P's key is never cleared");
  });

  // ---- crash windows of the completion path: a restart meets a half-retired S and must neither seed nor touch anything ----
  /** A restarted worker: a fresh runner over a fresh-incarnation authority on the same data dir and bare. */
  function restarted(r: Rig): Rig["runner"] {
    const again = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: AUTH_TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
    return runnerWith((_runId, _codex, addr) => {
      r.factoryCalls.push({ generation: addr?.generation, home: r.runHome });
      return { executor: r.executor, homeDir: r.runHome };
    }, fakeGitlab().gitlab, AUTH_TOKEN, r.logs.logger, {
      authority: again, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5, independentRestore: true,
      restoreIdentity: { procRoot: r.sProc, hostname: () => CID.slice(0, 12) } });
  }

  /** The next claim after a restart stops nonterminally, never seeds and writes nothing to P or S. */
  async function assertRestartStopsUntouched(r: Rig, claim: Rig["claim"], sClone: string): Promise<void> {
    const config = configText(r);
    const entry = cfgGet(r, restoreKey);
    const ledger = cfgAll(r, r.ledgerKey);
    const marker = fs.existsSync(markerFile(r)) ? fs.readFileSync(markerFile(r), "utf8") : undefined;
    const p = await pFacts(r);
    const sDigest = fs.existsSync(sClone) ? treeDigest(sClone) : undefined;
    const dirs = fs.readdirSync(path.dirname(r.pPath)).sort();
    const seededBefore = logged(r, "independent restore seeded").length;
    const factoryBefore = r.factoryCalls.length;
    const completedBefore = api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length;
    const stopsBefore = logged(r, "restore entry present; claim stopped").length;
    const runner2 = restarted(r);
    api.setOwnershipStatus(r.runId, "running", 3);
    await runner2.execute({ ...claim, claim_generation: 3 });
    assert.equal(logged(r, "restore entry present; claim stopped").length, stopsBefore + 1, "the claim stopped on S's live restore entry");
    assert.equal(failed(r).length, 0, "no failed report");
    assert.equal(api.states.filter(s => s.runId === r.runId && s.body.status === "completed").length, completedBefore, "no new completed report");
    assert.equal(logged(r, "independent restore seeded").length, seededBefore, "no new seed");
    assert.equal(r.factoryCalls.slice(factoryBefore).filter(c => c.generation !== undefined).length, 0,
      "no successor executor was built for the restarted claim");
    assert.equal(cfgAll(r, r.ledgerKey).length, ledger.length, "no new attempt ledger entry");
    assert.deepEqual(cfgAll(r, r.ledgerKey), ledger, "the attempt ledger is unchanged");
    assert.equal(cfgGet(r, restoreKey), entry, "S's restore value is byte-identical");
    assert.equal(configText(r), config, "the bare's config is byte-identical");
    assert.deepEqual(await pFacts(r), p, "P's key, slot, clone, HOME and provision are unchanged");
    assert.equal(fs.existsSync(markerFile(r)) ? fs.readFileSync(markerFile(r), "utf8") : undefined, marker);
    assert.equal(fs.existsSync(sClone) ? treeDigest(sClone) : undefined, sDigest, "S's clone is byte-identical");
    assert.deepEqual(fs.readdirSync(path.dirname(r.pPath)).sort(), dirs, "no new attempt directory");
  }

  it("crash between the completion source fence and its publication: a restarted worker stops nonterminally, never seeds, and S stays a fenced entry", async t => {
    const fired: string[] = [];
    const { r, claim, clone, before } = await completeRestore(t, { arm: () => {
      const original = git.installCompletionSourceFence.bind(git);
      t.mock.method(git, "installCompletionSourceFence", async (...args: Parameters<typeof original>) => {
        await original(...args);
        fired.push("fence installed");
        throw new Error("simulated crash right after the completion source fence");
      });
    } });
    assert.deepEqual(fired, ["fence installed"], "the fence was written and the flight died right after it");
    // The window is on disk: S's entry carries the fence, nothing was published or retired.
    const fenced = restoreValue(r);
    assert.equal(fenced.kind, "entry", "S's entry is not a tombstone");
    assert.equal(fenced.kind === "entry" ? fenced.stage : undefined, "ready");
    assert.notEqual(fenced.kind === "entry" ? fenced.remainder.completionSourceFence : undefined, undefined, "S's journal carries its completion source fence");
    // The flight degrades to an unpublished source (it may still report completion); the slot never reached its published state.
    const slot = await slotOf(r, clone);
    assert.ok(slot, "S's authority slot still exists after the crashed completion");
    assert.notEqual(slot.state, "claude_ready", "the slot never reached its published state");
    assert.equal(slot.publication, null, "no publication was committed");
    const sLedger = cfgAll(r, r.ledgerKey).map(l => JSON.parse(l) as { clonePath: string; state: string }).find(l => l.clonePath === clone);
    assert.equal(sLedger?.state, "live", "S is not retired");
    assert.equal(fs.existsSync(clone), true, "S's clone is still there");
    t.mock.restoreAll();
    assert.deepEqual(await pFacts(r), before, "P is untouched by the crashed completion");
    const fenceRaw = JSON.stringify((fenced as { remainder: Record<string, unknown> }).remainder.completionSourceFence);
    await assertRestartStopsUntouched(r, claim, clone);
    const after = restoreValue(r);
    assert.equal(after.kind, "entry");
    assert.equal(JSON.stringify((after as { remainder: Record<string, unknown> }).remainder.completionSourceFence), fenceRaw, "S's fence is intact");
  });

  it("retirement interrupted between the S ledger `retired` and the tombstone: the entry stays ready, a restarted worker stops nonterminally and never seeds", async t => {
    const fired: Array<{ ledger: string | undefined; fromRetire: boolean }> = [];
    let sPath = "";
    const { r, claim, clone, before } = await completeRestore(t, { arm: r2 => {
      const target = git as unknown as { advanceRestoreEntryUnderLock(...a: unknown[]): Promise<void> };
      const original = target.advanceRestoreEntryUnderLock.bind(git);
      t.mock.method(target, "advanceRestoreEntryUnderLock", async (...args: unknown[]) => {
        if (args[2] === "retired") {
          const entries = cfgAll(r2, r2.ledgerKey).map(l => JSON.parse(l) as { clonePath: string; state: string });
          const last = entries.filter(e => e.clonePath !== r2.pPath).pop();
          sPath = last?.clonePath ?? "";
          fired.push({ ledger: last?.state, fromRetire: (new Error().stack ?? "").includes("retireRunnerClone") });
          // Persistent (not EIO, so the one-shot metadata retry does not apply): the tombstone write never lands.
          throw new Error("simulated crash before the tombstone");
        }
        return original(...args);
      });
    }, resume: true });
    t.mock.restoreAll();
    assert.ok(fired.length >= 1, "the retirement's tombstone write was reached");
    assert.deepEqual(fired.filter(f => f.ledger !== "retired" || !f.fromRetire), [], "every injected call is the completion retirement's, after the ledger said retired");
    assert.equal(sPath, clone);
    // On disk: the window.
    const entry = restoreValue(r);
    assert.equal(entry.kind, "entry", "the successor is still an entry, not a tombstone");
    assert.equal(entry.kind === "entry" ? entry.stage : undefined, "ready", "restoreStage is still ready");
    const sLedger = cfgAll(r, r.ledgerKey).map(l => JSON.parse(l) as { clonePath: string; state: string }).filter(l => l.clonePath === clone);
    assert.equal(sLedger.at(-1)?.state, "retired", "S's matching ledger entry is retired");
    assert.deepEqual(await pFacts(r), before, "P is untouched");
    await assertRestartStopsUntouched(r, claim, clone);
    assert.equal(restoreValue(r).kind, "entry", "still no tombstone after the restart");
  });

  // ---- 3.9: the predecessor's generation G stays held after S has stopped executing ------------------------------------------
  const SYNTHETIC_G = (r: Rig, generation: number | undefined): RecoveryRecord => ({
    runId: r.runId, generation, captureId: randomUUID(), sourceSha: r.pinSha, inventoryGuarded: true,
    coverageContext: { barePath: r.bare } } as unknown as RecoveryRecord);

  /** The boundary proof exactly as FINAL enters it: the runner's ordinary inventory source boundary. */
  async function boundary(runner: Rig["runner"], r: Rig, generation: number): Promise<{ result: string; checks: string[] }> {
    const checks: string[] = [];
    const result = await (runner as unknown as { withInventorySourceBoundary(c: unknown, a: (p: () => Promise<boolean>) => Promise<void>): Promise<string> })
      .withInventorySourceBoundary({ runId: r.runId, generation, barePath: r.bare, reportDiagnostic: (d: { check: string }) => { checks.push(d.check); } },
        async prove => { await prove(); });
    return { result, checks };
  }

  it("G stays held after S completes: the boundary, ancestry, unadopted settlement and report retirement refuse for G, S's own generation settles, and a restart changes nothing", async t => {
    const { r, claim } = await completeRestore(t);
    const tombstone = restoreValue(r);
    assert.equal(tombstone.kind, "tombstone", "S has stopped executing and is retired");
    // Only the opaque predecessor's attribution is left: the inventory reports it and nothing else.
    const inventory = await git.readInventoryCloneHeads(r.bare, r.runId);
    assert.equal(inventory.kind, "verified");
    if (inventory.kind !== "verified") return;
    assert.deepEqual(inventory.clones, []);
    assert.deepEqual(inventory.retainedPredecessors?.map(p => [p.clonePath, p.generation]), [[r.pPath, 1]]);

    const checkAll = async (runner: Rig["runner"]): Promise<void> => {
      const g = await boundary(runner, r, 1);
      assert.equal(g.result, "retained", "FINAL's source boundary refuses P's generation");
      assert.deepEqual(g.checks, ["physical_sources_without_boundary"]);
      const n = await boundary(runner, r, claim.claim_generation!);
      assert.equal(n.result, "passed", "S's own generation proves empty");
      assert.deepEqual(n.checks, []);
      const coordinator = (runner as unknown as { recovery: { frozenSourceAncestry(rec: RecoveryRecord, owed: unknown[]): Promise<string> } }).recovery;
      assert.equal(await coordinator.frozenSourceAncestry(SYNTHETIC_G(r, 1), []), "unproven", "ancestry of P's generation is never proven");
      assert.equal(await coordinator.frozenSourceAncestry(SYNTHETIC_G(r, claim.claim_generation!), []), "verified", "S's generation still is");
    };
    await checkAll(r.runner);

    // The coordinator's own callback-free boundary (no runner wiring) refuses the predecessor's generation the same way.
    {
      const coordinator = r.recovery() as unknown as { withInventorySourceBoundary: unknown;
        inventorySourceBoundary(c: unknown, a: (p: () => Promise<boolean>) => Promise<void>): Promise<string> };
      const wired = coordinator.withInventorySourceBoundary;
      coordinator.withInventorySourceBoundary = undefined;
      try {
        const run = (generation: number) => coordinator.inventorySourceBoundary({ runId: r.runId, generation, barePath: r.bare }, async prove => { await prove(); });
        assert.equal(await run(1), "retained");
        assert.equal(await run(claim.claim_generation!), "passed");
      } finally { coordinator.withInventorySourceBoundary = wired; }
    }

    // The unadopted-generation settlement: no record, no owed candidate and no clone, yet P's generation keeps its hold.
    const records = await r.recovery().inspect(r.runId);
    for (const rec of records) fs.rmSync(recordFile(r, rec), { force: true });
    let openGeneration = 1;
    const released: number[] = [];
    client.listRecoveryHolds = async runId => ({ run_id: runId, holds: [{ hold_id: randomUUID(), generation: openGeneration, inventory_guarded: true,
      has_available_capture: false }] });
    client.releaseRecoveryCustody = async (runId, generation) => {
      released.push(generation as number);
      return { run_id: runId, generation, released: true, holds_released: 1 };
    };
    assert.equal(await r.recovery().settleUnadoptedGuardedGeneration({ runId: r.runId, generation: 1, barePath: r.bare }), "retained");
    assert.deepEqual(released, [], "no release for P's generation");
    openGeneration = claim.claim_generation!;
    assert.equal(await r.recovery().settleUnadoptedGuardedGeneration({ runId: r.runId, generation: openGeneration, barePath: r.bare }), "settled");
    assert.deepEqual(released, [openGeneration], "S's own generation is released");

    // Report retirement: both modes' source inspection refuses P's generation and accepts S's.
    const inspections: Array<[string, number, boolean]> = [];
    const budget = { signal: new AbortController().signal, deadline: Date.now() + 60_000 };
    for (const mode of ["retireAcknowledgedFinalReport", "retireArchivedReport"] as const) {
      mock.method(r.recovery(), mode, (async (_run: string, generation: number, b: typeof budget, _reserve: unknown,
        inspect: (bare: string, covered: (h: string) => Promise<boolean>, later: (h: string) => boolean) => Promise<boolean>) => {
        inspections.push([mode, generation, await git.withReportProofBudget(b, () => inspect(r.bare, async () => true, () => false))]);
      }) as never);
    }
    for (const generation of [1, claim.claim_generation!])
      await (r.runner as unknown as { retireRecoveryReport(...a: unknown[]): Promise<unknown> })
        .retireRecoveryReport(r.runId, generation, { expectedIdentity: {} }, "terminal", {});
    assert.deepEqual(inspections.filter(i => i[1] === 1).map(i => i[2]), [false, false], JSON.stringify(inspections));
    // (The archived-report read under a proof budget also refuses S's retired-but-absent ledger path, a pre-existing property of
    // that read; the acknowledged-report read accepts S's generation, which is what shows the refusal above is the predecessor's.)
    assert.deepEqual(inspections.filter(i => i[1] !== 1 && i[0] === "retireAcknowledgedFinalReport").map(i => i[2]), [true], JSON.stringify(inspections));
    mock.restoreAll();

    // A restart (a new authority runtime and runner) reads the same attribution and refuses the same way.
    const again = new AuthorityRuntime({ lifetime: r.lifetime, workerToken: AUTH_TOKEN, workerIncarnation: randomUUID(), timeoutMs: 5000 });
    const runner2 = runnerWith(() => ({ executor: r.executor, homeDir: r.runHome }), fakeGitlab().gitlab, AUTH_TOKEN, r.logs.logger, {
      authority: again, codexBoundaryDeadlineMs: 5000, recoveryRetryMs: 5, restoreIdentity: { procRoot: r.sProc, hostname: () => CID.slice(0, 12) } });
    await checkAll(runner2);
  });

  it("a restore predecessor is never offered to the retained-predecessor FINAL pass (issue #2969 scope)", async t => {
    const { r, claim } = await completeRestore(t);
    const settle = (runner: Rig["runner"]) => (runner as unknown as { settlePredecessorHolds(...a: unknown[]): Promise<string> });
    const flight = { claimGeneration: claim.claim_generation! + 1, runLog: r.logs.logger };
    let offered: RecoveryRecord[] = [];
    mock.method(r.recovery(), "predecessorRecords", async () => offered);
    const drive = async (records: RecoveryRecord[]): Promise<string> => {
      offered = records;
      const next = { ...claim, claim_generation: claim.claim_generation! + 1, inventory_guarded: true };
      return settle(r.runner).settlePredecessorHolds(next, flight, r.bare, {}, { attempts: 0, deadline: Date.now() + 3_600_000 }, new AbortController().signal);
    };
    assert.equal(await drive([SYNTHETIC_G(r, 1)]), "proceed");
    assert.equal(logged(r, "retained predecessor FINAL skipped").length, 0,
      "no candidate survived the restore filter, so the pass never reached its gate (nor any boundary, scope or exemption): " + JSON.stringify(logged(r, "retained predecessor FINAL skipped")));
    // S's own generation N is NOT filtered: it is still offered, so the pass reaches its disabling gate and logs the skip.
    assert.equal(await drive([SYNTHETIC_G(r, 1), SYNTHETIC_G(r, claim.claim_generation!)]), "proceed");
    const skips = logged(r, "retained predecessor FINAL skipped");
    assert.equal(skips.length, 1, JSON.stringify(skips));
    assert.equal((skips[0] as { reason?: string } | undefined)?.reason, "command_uid_unproven", JSON.stringify(skips));
  });
});

// ---- bindings, profiles and the flag table -------------------------------------------------------------------------------

/** Captures the flight the runner builds (registerEvidenceExecutor runs once at flight start, again at each executor swap). */
function captureFlight<H extends { flight?: Record<string, unknown> }>(r: Rig, holder: H = {} as H): H {
  const runner = r.runner as unknown as { registerEvidenceExecutor(f: Record<string, unknown>): void };
  const original = runner.registerEvidenceExecutor.bind(runner);
  mock.method(runner, "registerEvidenceExecutor", (f: Record<string, unknown>) => { holder.flight ??= f; return original(f); });
  return holder;
}

/** Runs `before` and then the real seed. */
function hookSeed(before: () => Promise<void> | void): { calls: number } {
  const state = { calls: 0 };
  const original = git.seedIndependentAttempt.bind(git);
  mock.method(git, "seedIndependentAttempt", async (...args: Parameters<typeof original>) => {
    state.calls++;
    await before();
    return original(...args);
  });
  return state;
}

/** Runs `before` and then the real J acquisition of the restore (after the eligibility pass, before the pin re-authentication). */
function hookJournalLock(r: Rig, before: () => Promise<void> | void): void {
  const recovery = r.recovery();
  const original = recovery.withRestoreJournalLock.bind(recovery);
  mock.method(recovery, "withRestoreJournalLock", async (...args: Parameters<typeof original>) => {
    await before();
    return original(...args);
  });
}

const recordFile = (r: Rig, rec: RecoveryRecord): string =>
  (r.recovery() as unknown as { recordPath(x: RecoveryRecord): string }).recordPath(rec);
const writeRecord = (r: Rig, rec: RecoveryRecord): Promise<void> =>
  (r.recovery() as unknown as { writeRecordUnlocked(x: RecoveryRecord): Promise<void> }).writeRecordUnlocked(rec);

describe("pin bindings, profiles and the flag table (issue #3644 3.6)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("a finalization pin bound by an owed ref restores exactly the pinned head, not P's newer commit", async () => {
    const { r } = await restoreRig({ pin: false });
    // The predecessor's finalization pin H: a commit of its own work, owed to the bare at generation 1 through the real path.
    fs.writeFileSync(path.join(r.pPath, "PINNED.txt"), "pinned work\n");
    gitIn(r.pPath, ["add", "PINNED.txt"]);
    gitIn(r.pPath, [...IDENT, "commit", "-m", "pinned work"]);
    const pinned = gitIn(r.pPath, ["rev-parse", "HEAD"]);
    const base = gitIn(r.bare, ["rev-parse", "refs/remotes/origin/main"]);
    const updated = await git.fetchAgentBranch(r.bare, r.pPath, BRANCH, r.runId, { context: {
      runId: r.runId, generation: 1, branch: BRANCH, kind: "issue", barePath: r.bare, defaultIdentity: { ref: "refs/remotes/origin/main", sha: base } } });
    assert.equal(updated.kind, "updated");
    assert.ok(await r.pin({ sourceSha: pinned, finalizationPin: true, bareDir: path.basename(r.bare), defaultBranch: "main" }));
    const newer = advanceP(r);
    assert.notEqual(newer, pinned);
    await r.runner.execute(r.claim);
    assert.equal(refusals(r).length, 0, JSON.stringify(refusals(r)));
    const note = logged(r, "independent restore seeded")[0]!;
    assert.equal(note.binding, "owed");
    assert.equal(note.sourceSha, pinned);
    assert.equal(note.seededHead, pinned, "S starts at the pinned head");
    assert.equal(failed(r).length, 0);
  });

  it("an early pin bound by its recovery pin ref restores that sha", async () => {
    const { r, seen } = await restoreRig({ pin: false });
    fs.writeFileSync(path.join(r.pPath, "EARLY.txt"), "early\n");
    gitIn(r.pPath, ["add", "EARLY.txt"]);
    gitIn(r.pPath, [...IDENT, "commit", "-m", "early pin target"]);
    const pinned = gitIn(r.pPath, ["rev-parse", "HEAD"]);
    gitIn(r.bare, ["fetch", "-q", r.pPath, `HEAD:refs/uzi-recovery-pin/${r.runId}/1`]);
    assert.ok(await r.pin({ sourceSha: pinned }));
    advanceP(r);
    await r.runner.execute(r.claim);
    const note = logged(r, "independent restore seeded")[0]!;
    assert.equal(refusals(r).length, 0, JSON.stringify(refusals(r)));
    assert.equal(note.binding, "recovery_pin_ref");
    assert.equal(note.seededHead, pinned);
    assert.equal(seen.head, pinned);
  });

  it("the Claude profile: the successor executor is built at {generation} with the claim's (absent) Codex binding, and the initial one is simply dropped", async () => {
    const { r } = await restoreRig();
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined, 2]);
    assert.equal(r.counters.ran, 0, "the initial executor never runs a model");
  });

  it("the Codex profile: the initial executor's safety is disposed before the swap, the successor is built from the same Codex binding, and the cache target stays unbound", async () => {
    let disposals = 0;
    const safety = { dispose: async () => { disposals++; return { kind: "disposed" }; } };
    const initial = Object.assign(Object.create(new StubExecutor(nullLogger())) as Executor, { safety: safety as never });
    const seenCodex: unknown[] = [];
    const codex = { access_token: "codex-access-token-do-not-scan", capability: "codex-capability-do-not-scan", auth_mode: "subscription" };
    const { r } = await restoreRig({ executor: initial, governor: true,
      claim: { secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan", codex } as never },
      factory: (rig, id, c, addr) => {
        seenCodex.push(c);
        return addr?.generation !== undefined
          ? { executor: new StubExecutor(nullLogger()), homeDir: path.join(rig.homeRoot, `${id}.g${addr.generation}`) }
          : { executor: initial, homeDir: rig.runHome };
      } });
    await r.runner.execute(r.claim);
    assert.equal(disposals >= 1, true, "the initial executor was disposed");
    assert.deepEqual(seenCodex.map(c => (c as { capability?: string } | undefined)?.capability), [codex.capability, codex.capability]);
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined, 2]);
    assert.equal(logged(r, "independent restore seeded").length, 1);
  });

  it("flag table: after the swap S's flight is a fresh attempt (nothing retained, nothing captured); a refusal keeps every original value", async () => {
    const flags = ["predecessorCapture", "retainedEpisode", "retainedEpisodeCustody", "retainedLocalCustody", "remediationPredecessorAttempt",
      "preserveRecoveryClone", "preserveSession", "keepGuardedInventoryOpen", "effectiveAttemptPaths", "worktreePath", "runHome"] as const;
    const read = (f: Record<string, unknown>): Record<string, unknown> => Object.fromEntries(flags.map(k => [k, f[k]]));
    // success
    const holder: { current?: Rig; flight?: Record<string, unknown> } = {};
    const seenInRun: Record<string, unknown> = {};
    const stub = new StubExecutor(nullLogger());
    const sExec = Object.create(stub) as Executor;
    sExec.run = async ctx => { seenInRun.flags = read(holder.flight!); seenInRun.clone = ctx.worktreePath; return stub.run(ctx); };
    const r = await buildRig({ addrAware: true, successorExecutor: sExec });
    holder.current = r;
    captureFlight(r, holder);
    await r.runner.execute(r.claim);
    assert.deepEqual(seenInRun.flags, {
      predecessorCapture: false, retainedEpisode: false, retainedEpisodeCustody: false, retainedLocalCustody: false,
      remediationPredecessorAttempt: undefined, preserveRecoveryClone: false, preserveSession: false, keepGuardedInventoryOpen: false,
      effectiveAttemptPaths: true, worktreePath: seenInRun.clone, runHome: path.join(r.homeRoot, `${r.runId}.g2`) });
    assert.notEqual(seenInRun.clone, r.pPath);
  });

  it("flag table: a refusal keeps every original flag, including the predecessor's path", async () => {
    const r = await buildRig({ addrAware: true, pBoundary: "failed" });
    const holder = captureFlight(r);
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "cross_check_failed");
    const f = holder.flight!;
    assert.equal(f.predecessorCapture, true);
    assert.equal(f.retainedEpisode, true);
    assert.equal(f.retainedEpisodeCustody, true);
    assert.equal(f.retainedLocalCustody, true);
    assert.equal(f.remediationPredecessorAttempt, r.pAttempt);
    assert.equal(f.preserveRecoveryClone, true);
    assert.equal(f.preserveSession, true);
    assert.equal(f.worktreePath, r.pPath);
    assert.equal(f.runHome, r.runHome, "no swap happened");
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined], "no successor executor was built");
  });

  it("S built on the predecessor's HOME is refused: nothing is written and the predecessor's HOME is untouched", async () => {
    const r = await buildRig({ addrAware: false });
    const before = await pFacts(r);
    const config = configText(r);
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "executor_unavailable", JSON.stringify(refusals(r)));
    assert.equal(configText(r), config);
    assert.equal(fs.existsSync(markerFile(r)), false);
    assert.deepEqual(await pFacts(r), before);
    assert.equal(failed(r).length, 0);
  });
});

// ---- the disk governor's cache target across the swap (plan 3.3) ---------------------------------------------------------

describe("the governor's cache target across the executor swap (issue #3644 3.3)", () => {
  beforeEach(() => { resetRigRoots(); });

  const homeOf = (run: { home: string | (() => string | undefined) }): string | undefined => typeof run.home === "function" ? run.home() : run.home;

  function executors(calls: string[], withReclaim: boolean) {
    const stub = new StubExecutor(nullLogger());
    const p: Executor = {
      run: async () => { throw new Error("the predecessor's executor never runs"); },
      reclaimCachesInPlace: async () => { calls.push("P"); return { dropped: [], absent: [], failed: [], skipped: [] }; },
    };
    const s = Object.create(stub) as Executor;
    if (withReclaim) s.reclaimCachesInPlace = async () => { calls.push("S"); return { dropped: [".npm"], absent: [], failed: [], skipped: [] }; };
    return { p, s };
  }

  for (const withReclaim of [true, false]) {
    it(`a forced reclaim while the seed is suspended after the swap reaches only S (${withReclaim ? "the executor's own reclaim" : "the dropRunCaches fallback"})`, async () => {
      const calls: string[] = [];
      const { p, s } = executors(calls, withReclaim);
      const { r } = await restoreRig({ governor: true, executor: p, successorExecutor: s });
      const sHome = path.join(r.homeRoot, `${r.runId}.g2`);
      fs.mkdirSync(path.join(sHome, ".npm", "_cacache"), { recursive: true });
      fs.writeFileSync(path.join(sHome, ".npm", "_cacache", "blob"), "successor cache bytes");
      const pHomeBefore = treeDigest(r.runHome);
      const pProvisionBefore = treeDigest(r.provisionDir);
      let release!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      const observed: { home?: string | undefined; dropped?: unknown; withHome?: string | undefined; measures?: string[]; cacheKept?: boolean } = {};
      hookSeed(async () => {
        // The executor and HOME are installed and bound; the seed has not run; phaseClone has not returned.
        observed.home = homeOf(r.governor.run!);
        observed.withHome = await r.governor.run!.withHome!(async h => h);
        observed.dropped = await r.governor.run!.reclaimInPlace();
        observed.cacheKept = fs.existsSync(path.join(r.runHome, ".npm", "_cacache", "blob"));
        await gate;
      });
      const done = r.runner.execute(r.claim);
      while (r.logs.lines.length < 3 || observed.home === undefined) await new Promise(resolve => setTimeout(resolve, 20));
      release();
      await done;
      assert.equal(observed.home, sHome, "the target is S's HOME and nothing else");
      assert.equal(observed.withHome, sHome);
      assert.deepEqual(calls, withReclaim ? ["S"] : [], "zero calls with the predecessor's executor");
      assert.equal(observed.cacheKept, true, "the predecessor's caches survive the forced reclaim");
      if (!withReclaim) assert.equal(fs.existsSync(path.join(sHome, ".npm", "_cacache", "blob")), false, "the fallback dropped S's caches");
      assert.deepEqual(r.governor.measures.filter(h => h === r.runHome), []);
      assert.deepEqual(r.governor.trims.filter(h => h === r.runHome), []);
      assert.equal(treeDigest(r.runHome), pHomeBefore, "P's HOME is byte-identical");
      assert.equal(treeDigest(r.provisionDir), pProvisionBefore, "P's provision directory is byte-identical");
    });
  }

  it("an operation started while unbound refuses, and the swap waits for an operation that holds the previous target", async () => {
    const { r } = await restoreRig({ governor: true });
    const holder = captureFlight(r);
    const order: string[] = [];
    let unblock!: () => void;
    const blocker = new Promise<void>(resolve => { unblock = resolve; });
    let lateBody = 0;
    let held: Promise<string | undefined> | undefined;
    const opaque = git.discoverOpaque.bind(git);
    mock.method(git, "discoverOpaque", async (...args: Parameters<typeof opaque>) => {
      const found = await opaque(...args);
      // 1: a callback started while the restore route is unbound refuses and runs nothing.
      const refused = await r.governor.run!.withHome!(async h => { lateBody++; return h; });
      order.push(`unbound:${refused === undefined}`);
      // 2: a leftover holder of a previous target (synthetic: the route never binds before the swap).
      const cacheTarget = holder.flight!.cacheTarget as { bind(t: { executor: Executor; home: string }): void };
      cacheTarget.bind({ executor: r.executor, home: r.runHome });
      held = r.governor.run!.withHome!(async h => { order.push("held:start"); await blocker; order.push("held:end"); return h; });
      return found;
    });
    hookSeed(() => { order.push("seed"); });
    const done = r.runner.execute(r.claim);
    while (!order.includes("held:start") || r.factoryCalls.length < 2) await new Promise(resolve => setTimeout(resolve, 20));
    await new Promise(resolve => setTimeout(resolve, 100));
    assert.equal(order.includes("seed"), false, "the swap is still draining: the seed has not started");
    assert.equal(homeOf(r.governor.run!), undefined, "the governor sees no target while the swap drains");
    unblock();
    await done;
    assert.equal(await held, r.runHome, "the holder finished on the previous target");
    assert.deepEqual(order.slice(0, 1), ["unbound:true"]);
    assert.ok(order.indexOf("held:end") < order.indexOf("seed"), "the bind waited for the holder");
    assert.equal(lateBody, 0);
    assert.equal(logged(r, "independent restore seeded").length, 1);
  });

  it("executor replacement failure: the target stays unbound, the claim is refused and nothing is written", async () => {
    const calls: string[] = [];
    const { p } = executors(calls, true);
    const r = await buildRig({ governor: true, executor: p,
      factory: (rig, _id, _codex, addr) => {
        if (addr?.generation !== undefined) throw new Error("no executor can be built");
        return { executor: p, homeDir: rig.runHome };
      } });
    const config = configText(r);
    const before = await pFacts(r);
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "executor_unavailable");
    assert.equal(configText(r), config);
    const run = r.governor.run!;
    assert.equal(homeOf(run), undefined, "still unbound");
    assert.equal(await run.reclaimInPlace(), undefined);
    assert.equal(await run.withHome!(async h => h), undefined);
    assert.deepEqual(calls, [], "nothing ever reached the predecessor's executor");
    assert.deepEqual(r.governor.measures, []);
    assert.deepEqual(await pFacts(r), before);
    assert.equal(failed(r).length, 0);
  });
});

// ---- lock order A -> J -> B (plan 3.4) ------------------------------------------------------------------------------------

describe("the authoritative pass: lock order and the passes between (issue #3644 3.4)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("A is taken first, J second and held across the seed, and no seed callback takes J again", async () => {
    const { r } = await restoreRig();
    const events: string[] = [];
    let depthA = 0, depthJ = 0, maxJ = 0;
    const authority = r.sRt;
    const tx = authority.withTransaction.bind(authority);
    mock.method(authority, "withTransaction", async (...args: Parameters<typeof tx>) => {
      depthA++; events.push("A+");
      try { return await (tx as (...a: unknown[]) => Promise<unknown>)(...args); } finally { depthA--; events.push("A-"); }
    });
    const recovery = r.recovery() as unknown as { withJournalLock(runId: string, fn: () => Promise<unknown>, signal?: AbortSignal): Promise<unknown> };
    const lock = recovery.withJournalLock.bind(recovery);
    mock.method(recovery, "withJournalLock", async (runId: string, fn: () => Promise<unknown>, signal?: AbortSignal) => {
      if (depthJ > 0) events.push("J-NESTED");
      depthJ++; maxJ = Math.max(maxJ, depthJ); events.push(`J+@A${depthA}`);
      try { return await lock(runId, fn, signal); } finally { depthJ--; events.push("J-"); }
    });
    const seed = git.seedIndependentAttempt.bind(git);
    let insideSeed: { a: number; j: number } | undefined;
    mock.method(git, "seedIndependentAttempt", async (...args: Parameters<typeof seed>) => {
      const input = args[0];
      insideSeed = { a: depthA, j: depthJ };
      // Every callback runs with J held by the restore and must not take it again.
      for (const name of ["predecessorUnchanged", "census", "beforeRestoreAttribution", "beforeSeed"] as const) {
        const original = input[name] as (...a: unknown[]) => Promise<unknown>;
        (input as unknown as Record<string, unknown>)[name] = async (...a: unknown[]) => {
          events.push(`cb:${name}:J${depthJ}`);
          return original(...a);
        };
      }
      return seed(input);
    });
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    assert.deepEqual(insideSeed, { a: 1, j: 1 }, "the seed runs inside A and inside J");
    assert.equal(events.includes("J-NESTED"), false, "no nested acquisition of J anywhere in the run");
    assert.equal(maxJ, 1);
    for (const cb of ["predecessorUnchanged", "census", "beforeRestoreAttribution", "beforeSeed"]) {
      assert.ok(events.includes(`cb:${cb}:J1`), `${cb} ran with exactly the restore's J held`);
    }
    const firstJ = events.findIndex(e => e.startsWith("J+"));
    assert.match(events[firstJ]!, /@A[1-9]/, "J is taken beneath A");
  });

  it("a competing J-then-B recovery cycle runs concurrently without deadlock and serializes after the seed", async () => {
    const { r } = await restoreRig();
    const order: string[] = [];
    let competing: Promise<unknown> | undefined;
    hookSeed(() => {
      order.push("seed:start");
      // Same run, another generation: takes J, then writes under it. J is held by the restore, so it waits.
      competing = r.pin({ generation: 3 }).then(rec => { order.push("pin:done"); return rec; });
    });
    const seed = git.seedIndependentAttempt;
    const ordered = mock.method(git, "seedIndependentAttempt", async (...args: Parameters<typeof seed>) => {
      try { return await seed.call(git, ...args); } finally { order.push("seed:end"); }
    });
    void ordered;
    await r.runner.execute(r.claim);
    assert.ok(competing, "the competing cycle was started");
    assert.ok(await competing, "and finished: no deadlock");
    assert.ok(order.indexOf("seed:end") < order.indexOf("pin:done"), `serialized after the seed: ${order.join(",")}`);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    assert.equal(failed(r).length, 0);
  });

  it("a competing cycle that finished first is seen by the pin re-authentication: the advanced record refuses with nothing written", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    const config = configText(r);
    hookJournalLock(r, async () => {
      const rec = (await r.recovery().inspect(r.runId))[0]!;
      await writeRecord(r, { ...rec, state: "bundled" as never });
    });
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.reason, "pin_changed", JSON.stringify(refusals(r)));
    assert.equal(configText(r), config);
    assert.deepEqual(await pFacts(r), before);
    assert.equal(fs.existsSync(markerFile(r)), false);
  });

  describe("a pin record that changes after the eligibility pass and before the seed refuses pin_changed", () => {
    const cases: Array<{ name: string; mutate: (r: Rig, rec: RecoveryRecord) => Promise<void> | void }> = [
      { name: "advanced to another state", mutate: (r, rec) => writeRecord(r, { ...rec, state: "uploaded" as never }) },
      { name: "retargeted to another sha (the old sha stays in Git)", mutate: (r, rec) => writeRecord(r, { ...rec, sourceSha: "a".repeat(40) }) },
      { name: "removed", mutate: (r, rec) => { fs.unlinkSync(recordFile(r, rec)); } },
      { name: "corrupted (its MAC no longer verifies)", mutate: (r, rec) => { fs.writeFileSync(recordFile(r, rec), "{ not an authenticated record"); } },
      { name: "settled by a final request", mutate: (r, rec) => writeRecord(r, { ...rec, finalRequest: { disposition: { kind: "settled" } } as never }) },
      { name: "settled (the final is acknowledged)", mutate: (r, rec) => writeRecord(r, { ...rec, finalAcknowledged: true }) },
      { name: "joined by a second ordinary record of the generation", mutate: (r, rec) => writeRecord(r, { ...rec, captureId: randomUUID() }) },
      { name: "completed (a completion receipt)", mutate: (r, rec) => writeRecord(r, { ...rec, completionReceipt: { hold_id: randomUUID() } as never }) },
    ];
    for (const c of cases) {
      it(c.name, async () => {
        const { r } = await restoreRig();
        const before = await pFacts(r);
        const config = configText(r);
        hookJournalLock(r, async () => { await c.mutate(r, (await r.recovery().inspect(r.runId))[0]!); });
        await r.runner.execute(r.claim);
        assert.equal(refusals(r)[0]?.reason, "pin_changed", JSON.stringify(refusals(r)));
        assert.equal(configText(r), config, "nothing was written to the bare");
        assert.equal(fs.existsSync(markerFile(r)), false);
        assert.deepEqual(await pFacts(r), before);
        assert.equal(failed(r).length, 0);
        assert.equal(logged(r, "independent restore seeded").length, 0);
      });
    }
  });

  describe("evidence that changes between the passes refuses before the first write (predecessorUnchanged)", () => {
    /** Answers the Nth read of `target` made from the eligibility pass; the first (preliminary) read passes through. */
    function peekChange(r: Rig, target: string, impl: (real: SlotSnapshot | null) => SlotSnapshot | null | Promise<SlotSnapshot | null>): void {
      const original = r.sRt.peekSlot.bind(r.sRt);
      let seen = 0;
      mock.method(r.sRt, "peekSlot", async (p: string) => {
        const real = await original(p);
        if (p === target && (new Error().stack ?? "").includes("evaluateRestoreRoute") && ++seen >= 2) return impl(real);
        return real;
      });
    }
    const cases: Array<{ name: string; arrange: (r: Rig) => void; mutate?: (r: Rig) => Promise<void> | void }> = [
      { name: "the slot's owner changed", arrange: r => peekChange(r, r.pPath, real => real && ({ ...real, owner: { ...real.owner, worker: "another-incarnation" } })) },
      { name: "the slot's epoch changed", arrange: r => peekChange(r, r.pPath, real => real && ({ ...real, epoch: "another-epoch" })) },
      { name: "the slot gained a pending footprint", arrange: r => peekChange(r, r.pPath, real => real && ({ ...real, pending: { status: "running", kind: "destructive", operation: "late" } as never })) },
      { name: "a pending bare record appeared", arrange: r => peekChange(r, r.bare, real => ({ ...(real ?? ({ state: "active", owner: { worker: r.sInc }, epoch: "e", seq: 1, pending: null, publication: null } as unknown as SlotSnapshot)),
        pending: { status: "running", kind: "bare_mutation", operation: "late" } as never })) },
      { name: "a census slot changed (another source became foreign-active)", arrange: r => {
        const other = path.join(path.dirname(r.pPath), `issue-77.attempt-${mintAttemptId(1)}`);
        cfgAdd(r, "uzi-attempts.agent/issue-77.entry", JSON.stringify({ attemptId: path.basename(other).replace("issue-77.attempt-", ""), runId: randomUUID(), clonePath: other, state: "retired" }));
        peekChange(r, other, real => ({ ...(real ?? ({ state: "active", owner: { worker: "x" }, epoch: "e", seq: 1, pending: null, publication: null } as unknown as SlotSnapshot)),
          state: "active", owner: { worker: "a-dead-incarnation" } as never })); } },
      { name: "a protected descriptor appeared", arrange: () => undefined, mutate: r => {
        cfgSet(r, `uzi-retained.${r.runId}.journal`, JSON.stringify({ version: 1, branch: BRANCH, key: "issue-3644", journal: journalOf(r) })); } },
      { name: "P's boundary record was rewritten byte-differently (same content, another spelling)", arrange: () => undefined, mutate: r => {
        const file = path.join(r.dataDir, ".uzi-authority", "boundaries", boundaryFileName(r.pInc));
        const parsed = JSON.parse(fs.readFileSync(file, "utf8")) as Record<string, unknown>;
        const reordered = JSON.stringify(Object.fromEntries(Object.entries(parsed).reverse()));
        assert.notEqual(reordered, fs.readFileSync(file, "utf8"));
        fs.writeFileSync(file, reordered); } },
      { name: "S's boundary record was rewritten byte-differently", arrange: () => undefined, mutate: r => {
        const file = path.join(r.dataDir, ".uzi-authority", "boundaries", boundaryFileName(r.sInc));
        const parsed = JSON.parse(fs.readFileSync(file, "utf8")) as Record<string, unknown>;
        fs.writeFileSync(file, JSON.stringify(Object.fromEntries(Object.entries(parsed).reverse()))); } },
      { name: "P's journal was rewritten (same meaning, another spelling)", arrange: () => undefined, mutate: r => {
        const j = journalOf(r);
        cfgSet(r, r.journalKey, JSON.stringify({ attemptId: j.attemptId, clonePath: j.clonePath, runId: j.runId })); } },
      { name: "S's live identity changed (its process start time moved)", arrange: () => undefined, mutate: async r => { await makeProc(r.sProc, { ticks: "9000" }); } },
      { name: "P's clone root was replaced (another dev/ino)", arrange: () => undefined, mutate: r => {
        fs.renameSync(r.pPath, `${r.pPath}.moved`); fs.mkdirSync(r.pPath); } },
    ];
    for (const c of cases) {
      it(c.name, async () => {
        const { r } = await restoreRig();
        c.arrange(r);
        hookSeed(async () => { await c.mutate?.(r); });
        await r.runner.execute(r.claim);
        if (fs.existsSync(`${r.pPath}.moved`)) { fs.rmSync(r.pPath, { recursive: true, force: true }); fs.renameSync(`${r.pPath}.moved`, r.pPath); }
        const reason = refusals(r)[0]?.reason;
        assert.ok(reason === "predecessor_classification_changed" || reason === "predecessor_changed", `${reason}: ${JSON.stringify(refusals(r))}`);
        assert.equal(logged(r, "independent restore seeded").length, 0);
        assert.equal(fs.existsSync(markerFile(r)), false, "no marker was written");
        assert.equal(cfgAll(r, r.ledgerKey).length, 1, "no ledger entry was appended");
        assert.equal(cfgGet(r, restoreKey), undefined, "no restore entry exists");
        assert.equal(failed(r).length, 0);
      });
    }
  });
});

// ---- the seed's windows and the claims after them (plan 3.5, 3.10) --------------------------------------------------------

describe("a seed that refuses or fails is a nonterminal stop that leaves exactly its window (issue #3644 3.5)", () => {
  beforeEach(() => { resetRigRoots(); });

  /** Makes the Nth durability call of the restore entry's writes fail (1: ledger file, 2: ledger dir, 3: entry file, 4: entry dir). */
  function failSync(nth: number): void {
    let n = 0;
    (git as unknown as { restoreSyncSeam: { sync(target: string, what: string): Promise<void> } }).restoreSyncSeam = {
      sync: async (target, what) => {
        if (++n === nth) throw new Error("injected fsync failure");
        const handle = await fsp.open(target, what === "file" ? "r" : fs.constants.O_RDONLY | fs.constants.O_DIRECTORY);
        try { await handle.sync(); } finally { await handle.close(); }
      },
    };
  }
  afterEach(() => { (git as unknown as { restoreSyncSeam: unknown }).restoreSyncSeam = undefined; });

  async function nextClaim(r: Rig, generation = 3): Promise<void> {
    api.setOwnershipStatus(r.runId, "running", generation);
    await r.runner.execute({ ...r.claim, claim_generation: generation });
  }

  it("marker_only: the marker write refused (a different marker is already there); nothing else was written, and once the marker is the right one the restore proceeds", async () => {
    const { r, seen } = await restoreRig();
    fs.mkdirSync(path.dirname(markerFile(r)), { recursive: true, mode: 0o700 });
    fs.writeFileSync(markerFile(r), JSON.stringify({ version: 1, runId: r.runId, predecessorGeneration: 1, predecessorAttemptId: "another", barePath: r.bare }), { mode: 0o600 });
    const config = configText(r);
    const before = await pFacts(r);
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.reason, "marker_failed");
    assert.equal(refusal.window, "marker_only");
    assert.equal(configText(r), config, "the bare was not written");
    assert.deepEqual(await pFacts(r), before);
    assert.equal(failed(r).length, 0);
    // The window allows the restore to run again: the marker as a prior partial run would have left it (identical).
    fs.writeFileSync(markerFile(r), JSON.stringify({ version: 1, runId: r.runId, predecessorGeneration: 1, predecessorAttemptId: r.pAttempt, barePath: r.bare }));
    await nextClaim(r);
    assert.equal(logged(r, "independent restore seeded").length, 1, JSON.stringify(refusals(r)));
    assert.match(String(seen.head), /^[0-9a-f]{40}$/);
  });

  it("ledger_live: S's live entry may exist and nothing else; the next claim is refused by the relationship check", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    failSync(1);
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.window, "ledger_live", JSON.stringify(refusal));
    assert.equal(cfgGet(r, restoreKey), undefined, "no restore entry");
    assert.equal(cfgAll(r, r.ledgerKey).length, 2, "S's live entry is the one extra write");
    assert.equal(cfgGet(r, r.journalKey), before.journal);
    assert.deepEqual({ ...(await pFacts(r)), pLedger: undefined }, { ...before, pLedger: undefined });
    assert.equal(failed(r).length, 0);
    (git as unknown as { restoreSyncSeam: unknown }).restoreSyncSeam = undefined;
    await nextClaim(r);
    assert.equal(refusals(r).at(-1)?.reason, "conflicting_attribution", "a second live attempt of the run in the ledger refuses the restore");
    assert.equal(logged(r, "independent restore seeded").length, 0);
  });

  it("seeding: the restore entry exists (not proven durable); the next claim stops on the composite", async () => {
    const { r } = await restoreRig();
    failSync(3);
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.window, "seeding", JSON.stringify(refusal));
    assert.ok(cfgGet(r, restoreKey), "the entry is there");
    assert.equal(failed(r).length, 0);
    (git as unknown as { restoreSyncSeam: unknown }).restoreSyncSeam = undefined;
    const config = configText(r);
    await nextClaim(r);
    assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
    assert.equal(configText(r), config, "the second claim wrote nothing");
    assert.equal(r.factoryCalls.filter(c => c.generation === 3).length, 0);
  });

  for (const [nth, window] of [[2, "ledger_live"], [4, "seeding"]] as const) {
    it(`a failing ${nth === 2 ? "directory sync of the ledger append" : "directory sync of the restore entry"} (durability call #${nth}) leaves the ${window} window`, async () => {
      const { r } = await restoreRig();
      failSync(nth);
      await r.runner.execute(r.claim);
      assert.equal(refusals(r)[0]?.window, window, JSON.stringify(refusals(r)));
      assert.equal(failed(r).length, 0);
    });
  }

  it("a read-back failure of the restore entry (the value is not what was written) leaves the seeding window", async () => {
    const { r } = await restoreRig();
    let n = 0;
    (git as unknown as { restoreSyncSeam: { sync(target: string, what: string): Promise<void> } }).restoreSyncSeam = {
      sync: async () => {
        // After the entry's own durability calls (#3 file, #4 directory) the value disappears, as a torn write would.
        if (++n === 4) gitIn(r.bare, ["config", "--local", "--unset-all", restoreKey]);
      },
    };
    await r.runner.execute(r.claim);
    assert.equal(refusals(r)[0]?.window, "seeding", JSON.stringify(refusals(r)));
    assert.match(String(refusals(r)[0]?.detail), /read-back differs/);
    assert.equal(failed(r).length, 0);
  });

  /** P's journal, ledger entry, slot, bytes, HOME and provision directory are unchanged after a failed seed, and the run is not failed. */
  async function assertPKept(r: Rig, before: Awaited<ReturnType<typeof pFacts>>): Promise<void> {
    assert.equal(failed(r).length, 0, "no failed report");
    assert.deepEqual(await pFacts(r), before, "P's journal, ledger entry, slot, clone, HOME and provision directory are byte-identical");
    assert.equal(logged(r, "independent restore seeded").length, 0);
  }

  /** S's clone path as its own ledger entry (the last value) names it. */
  const successorPath = (r: Rig): string => (JSON.parse(cfgAll(r, r.ledgerKey).at(-1)!) as { clonePath: string }).clonePath;

  it("before_seed: the quiesce refuses (a protected process blocks the seed); the runner forwards P's marker to quiesceRun in seed mode, the refusal is nonterminal, and the clone never exists", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    const calls: Array<Record<string, unknown>> = [];
    type Quiesce = (flight: unknown, executor: unknown, opts: Record<string, unknown>) => Promise<unknown>;
    const target = r.runner as unknown as { quiesceRun: Quiesce };
    const original = target.quiesceRun.bind(r.runner);
    mock.method(target, "quiesceRun", async (flight: unknown, executor: unknown, opts: Record<string, unknown>) => {
      if (opts.mode !== "seed") return original(flight, executor, opts);
      calls.push(opts);
      return { blocked: true, outcome: { process: { state: "unverified", processes: [], killed: [], detail: "a protected attempt marker is still running" } } };
    });
    await r.runner.execute(r.claim);
    assert.equal(calls.length, 1, "the seed's sweep ran once");
    assert.deepEqual(calls[0]!.protectedMarkers, [`${r.runId}:${r.pAttempt}`], "P's marker is protected");
    assert.equal(calls[0]!.site, "attempt_seed");
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.reason, "before_seed_refused", JSON.stringify(refusal));
    assert.match(String(refusal.detail), /a protected attempt marker is still running/);
    assert.equal(refusal.window, "seeding");
    assert.equal(fs.existsSync(successorPath(r)), false, "no successor clone was seeded");
    await assertPKept(r, before);
  });

  it("pin_recheck: a pin that stops verifying after the callbacks refuses before the clone is seeded", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    let armed = false;
    type Quiesce = (flight: unknown, executor: unknown, opts: Record<string, unknown>) => Promise<unknown>;
    const target = r.runner as unknown as { quiesceRun: Quiesce };
    const original = target.quiesceRun.bind(r.runner);
    mock.method(target, "quiesceRun", async (flight: unknown, executor: unknown, opts: Record<string, unknown>) => {
      if (opts.mode === "seed") armed = true; // the sweep ran: from here the pin no longer verifies
      return original(flight, executor, opts);
    });
    const verify = (git as unknown as { verifyRestorePinRefsUnderLock(...a: unknown[]): Promise<unknown> }).verifyRestorePinRefsUnderLock.bind(git);
    mock.method(git as unknown as { verifyRestorePinRefsUnderLock(...a: unknown[]): Promise<unknown> }, "verifyRestorePinRefsUnderLock",
      async (...args: unknown[]) => armed ? { ok: false, reason: "pin_unbound" } : verify(...args));
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.reason, "pin_unbound", JSON.stringify(refusal));
    assert.equal(refusal.window, "seeding");
    assert.equal(fs.existsSync(successorPath(r)), false, "no successor clone was seeded");
    await assertPKept(r, before);
  });

  it("seed: a failing clone leaves the seeding window and a nonterminal refusal", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    type Exec = (command: string, args: string[], options: unknown) => Promise<unknown>;
    const target = git as unknown as { execScoped: Exec };
    const exec = target.execScoped.bind(git);
    mock.method(target, "execScoped", async (command: string, args: string[], options: unknown) => {
      if (args.includes("clone") && args.includes("--no-checkout")) throw Object.assign(new Error("injected clone failure"), { code: 128, stdout: "", stderr: "x" });
      return exec(command, args, options);
    });
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.window, "seeding", JSON.stringify(refusal));
    assert.equal(refusal.reason, "seed_failed");
    assert.ok(cfgGet(r, restoreKey), "the restore entry is left in its seeding stage");
    await assertPKept(r, before);
  });

  it("ready: the final entry write is not durable; the clone exists, the refusal is nonterminal and P is untouched", async () => {
    const { r } = await restoreRig();
    const before = await pFacts(r);
    failSync(5);
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.window, "seeding", JSON.stringify(refusal));
    assert.match(String(refusal.detail), /injected fsync failure/);
    assert.ok(cfgGet(r, restoreKey), "the restore entry exists");
    await assertPKept(r, before);
  });

  it("an authority refusal inside the seed (assertBareClear) is a refusal with window none and nothing written", async () => {
    const { r } = await restoreRig();
    const config = configText(r);
    const original = (git as unknown as { assertBareClear(bare: string, op: string): Promise<void> }).assertBareClear.bind(git);
    mock.method(git as unknown as { assertBareClear(bare: string, op: string): Promise<void> }, "assertBareClear", async (bare: string, op: string) => {
      if (op === "seed-attempt") throw new Error("injected authority refusal");
      return original(bare, op);
    });
    await r.runner.execute(r.claim);
    const refusal = refusals(r)[0]!;
    assert.equal(refusal.reason, "authority_refused");
    assert.equal(refusal.window, "none");
    assert.equal(configText(r), config);
    assert.equal(fs.existsSync(markerFile(r)), false);
  });
});

describe("a predecessor at the canonical clone path is not restorable (issue #3644 3.2)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("the real canonical shape {runId, clonePath} is the canonical unsupported class: parked for its owner during classification, with nothing built, swapped or written", async () => {
    // What markRecoveryCapture(..., undefined) writes for an attempt-less source: no attemptId, no ledger entry, a slot keyed at the canonical path.
    const { r } = await restoreRig({ pShape: "canonical", features: [...FEATURES, PARK_FEATURE] });
    assert.deepEqual(Object.keys(journalOf(r)).sort(), ["clonePath", "runId"], "the fixture is the real canonical shape");
    assert.equal(cfgAll(r, r.ledgerKey).length, 0, "an attempt-less source has no ledger entry");
    const realReport = client.reportState.bind(client) as unknown as (...a: unknown[]) => Promise<Record<string, unknown>>;
    mock.method(client, "reportState", (async (...args: unknown[]) => {
      const body = args[1] as Record<string, unknown>;
      const ack = await realReport(...args);
      return body.status === "recovery_wait" && typeof body.recovery_cause === "string" ? { ...ack, recoveryCause: body.recovery_cause } : ack;
    }) as never);
    const config = configText(r);
    const before = await pFacts(r);
    const swaps = mock.method(r.runner as unknown as { swapRestoreExecutor(...a: unknown[]): Promise<void> }, "swapRestoreExecutor");
    await r.runner.execute(r.claim);
    const refusal = refusals(r);
    assert.equal(refusal.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore/i.test(String((l as { msg: string }).msg)))));
    assert.equal(refusal[0]!.reason, "predecessor_not_plain");
    assert.equal(refusal[0]!.unsupported, "canonical");
    const parks = api.states.filter(x => x.runId === r.runId && x.body.status === "recovery_wait");
    assert.equal(parks.length, 1, "one owner-hold park");
    assert.equal(parks[0]!.body.recovery_cause, "retained_recovery_adoption_failed");
    assert.equal(logged(r, "restore unsupported predecessor held").length, 1);
    assert.equal(failed(r).length, 0, "nonterminal: the run is not failed");
    assert.equal(swaps.mock.callCount(), 0, "no successor executor was swapped in");
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined], "no successor executor was built");
    assert.equal(logged(r, "independent restore seeded").length, 0);
    assert.equal(configText(r), config, "nothing was written to the bare");
    assert.equal(fs.existsSync(markerFile(r)), false);
    assert.deepEqual(await pFacts(r), before, "P's key, slot, clone, HOME and provision directory are unchanged");
  });

  it("a journal that keeps an attempt id on the canonical path is not the canonical shape: predecessor_path, the unchanged refusal, no park", async () => {
    const { r } = await restoreRig({ features: [...FEATURES, PARK_FEATURE] });
    // P's journal and `live` ledger entry name the canonical path while the journal still carries an attempt id; its active foreign slot is keyed there.
    const canonical = r.pPath.replace(/\.attempt-.*$/, "");
    assert.notEqual(canonical, r.pPath);
    fs.mkdirSync(canonical, { recursive: true });
    await r.pRt.withTransaction(() => r.pRt.hooks.admitSourceUse(canonical, "seed-canonical"));
    const ledger = JSON.parse(cfgAll(r, r.ledgerKey)[0]!) as Record<string, unknown>;
    cfgSet(r, r.ledgerKey, JSON.stringify({ ...ledger, clonePath: canonical }));
    cfgSet(r, r.journalKey, JSON.stringify({ ...journalOf(r), clonePath: canonical }));
    const config = configText(r);
    const before = await pFacts(r);
    const swaps = mock.method(r.runner as unknown as { swapRestoreExecutor(...a: unknown[]): Promise<void> }, "swapRestoreExecutor");
    await r.runner.execute(r.claim);
    const refusal = refusals(r);
    assert.equal(refusal.length, 1, JSON.stringify(r.logs.lines.filter(l => /restore/i.test(String((l as { msg: string }).msg)))));
    assert.equal(refusal[0]!.reason, "predecessor_path");
    assert.equal(refusal[0]!.unsupported, undefined, "an item-7 refusal carries no class");
    assert.equal(api.states.filter(x => x.runId === r.runId && x.body.status === "recovery_wait").length, 0, "no park");
    assert.equal(failed(r).length, 0, "nonterminal: the run is not failed");
    assert.equal(swaps.mock.callCount(), 0, "no successor executor was swapped in");
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined], "no successor executor was built");
    assert.equal(logged(r, "independent restore seeded").length, 0);
    assert.equal(configText(r), config, "nothing was written to the bare");
    assert.equal(fs.existsSync(markerFile(r)), false);
    assert.deepEqual(await pFacts(r), before, "P's key, ledger, slot, clone, HOME and provision directory are unchanged");
  });
});

describe("later claims never reach the legacy path (issue #3644 3.10)", () => {
  beforeEach(() => { resetRigRoots(); });

  it("with the restore disabled an eligible predecessor stops nonterminally and nothing is written", async () => {
    const { r } = await restoreRig({ independentRestore: false });
    const config = configText(r);
    const before = await pFacts(r);
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore eligible; seeding disabled for this runner instance").length, 1);
    assert.equal(logged(r, "independent restore seeded").length, 0);
    assert.equal(configText(r), config);
    assert.deepEqual(await pFacts(r), before);
    assert.deepEqual(r.factoryCalls.map(c => c.generation), [undefined]);
    assert.equal(failed(r).length, 0);
  });

  it("with the restore feature off (no active_run_snapshot) and a live restore entry present, the claim stops on the entry and never takes the legacy route", async () => {
    const { r } = await restoreRig();
    await r.runner.execute(r.claim);
    assert.equal(logged(r, "independent restore seeded").length, 1);
    const config = configText(r);
    const before = await pFacts(r);
    client.protocolFeatures = ["claim_generation_fence"];
    const reserve = mock.method(git, "reserveRecoveryIteration");
    const discover = mock.method(git, "discoverRetainedRecovery");
    api.setOwnershipStatus(r.runId, "running", 3);
    await r.runner.execute({ ...r.claim, claim_generation: 3 });
    assert.equal(logged(r, "restore entry present; claim stopped").length, 1);
    assert.equal(reserve.mock.callCount(), 0);
    assert.equal(discover.mock.callCount(), 0);
    assert.equal(configText(r), config);
    assert.deepEqual(await pFacts(r), before);
    assert.equal(failed(r).length, 0);
  });
});


describe("the restored flight's repo-agent source and required base (issue #3644 review fixes)", () => {
  beforeEach(() => { resetRigRoots(); });

  const SOURCE_FEATURES = [...FEATURES, REPO_AGENT_SOURCE_FEATURE];

  /** Spies the repo-agent source hold request; the restore itself must reach no fetch (ensureClone / ensureCommitPresent). */
  function spies() {
    const holds: Array<{ runId: string; commit: string }> = [];
    const orig = client.requestRepoAgentSourceHold.bind(client);
    (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = async (runId: string, args: { commit: string }) => {
      holds.push({ runId, commit: args.commit });
      return { outcome: "held", status: "paused" };
    };
    const ensureClone = mock.method(git, "ensureClone");
    const ensureCommitPresent = mock.method(git, "ensureCommitPresent");
    return { holds, ensureClone, ensureCommitPresent, restore: () => { (client as unknown as { requestRepoAgentSourceHold: unknown }).requestRepoAgentSourceHold = orig; } };
  }

  it("an unpinned restored claim resolves its repo-agent source from the bare's origin default-branch ref, with no fetch", async () => {
    const { r } = await restoreRig({ features: SOURCE_FEATURES });
    const spy = spies();
    try { await r.runner.execute(r.claim); } finally { spy.restore(); }
    assert.deepEqual(spy.holds, [], "no repo-agent source hold");
    assert.equal(logged(r, "independent restore seeded").length, 1);
    assert.equal(failed(r).length, 0);
    assert.equal(api.repoAgentSourceByRun.get(r.runId), r.pinSha, "the source is the bare's origin default-branch tip");
    assert.equal(spy.ensureClone.mock.callCount(), 0, "no ensureClone after the seed");
    assert.equal(spy.ensureCommitPresent.mock.callCount(), 0, "no fetch");
  });

  it("a default branch absent from the bare still fails closed into the repo-agent source hold", async () => {
    const { r } = await restoreRig({ features: SOURCE_FEATURES });
    gitIn(r.bare, ["update-ref", "-d", "refs/remotes/origin/main"]);
    const spy = spies();
    try { await r.runner.execute(r.claim); } finally { spy.restore(); }
    assert.equal(logged(r, "independent restore seeded").length, 1, "the seed itself succeeded");
    assert.equal(spy.holds.length, 1, "the run is held");
    assert.equal(api.repoAgentSourceByRun.has(r.runId), false);
    assert.equal(spy.ensureClone.mock.callCount(), 0);
  });

  it("a claim that already records its repo-agent source commit is read at that commit, as before", async () => {
    const { r } = await restoreRig({ features: SOURCE_FEATURES });
    r.claim.repo_agent_source_commit = r.pinSha;
    const spy = spies();
    try { await r.runner.execute(r.claim); } finally { spy.restore(); }
    assert.deepEqual(spy.holds, []);
    assert.equal(failed(r).length, 0);
    assert.equal(api.repoAgentSourceByRun.get(r.runId), r.pinSha, "the roster is reported at the recorded commit");
    assert.equal(spy.ensureClone.mock.callCount(), 0);
  });

  it("a restored flight is a continuation: a --require-base planned commit that differs from the pin holds for attestation instead of diverging", async () => {
    const { r } = await restoreRig();
    const planned = gitIn(r.bare, [...IDENT, "commit-tree", `${r.pinSha}^{tree}`, "-p", r.pinSha, "-m", "planned base"]);
    assert.notEqual(planned, r.pinSha);
    r.claim.require_base_match = true;
    r.claim.planned_base_commit = planned;
    const holds: Array<{ runId: string }> = [];
    const orig = client.requestBaseAttestationHold.bind(client);
    (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = async (runId: string) => {
      holds.push({ runId });
      return { outcome: "held", status: "paused" };
    };
    try { await r.runner.execute(r.claim); } finally { (client as unknown as { requestBaseAttestationHold: unknown }).requestBaseAttestationHold = orig; }
    assert.equal(failed(r).length, 0, "not failed base_diverged");
    assert.equal(holds.length, 1, "the BaseAttestationMissing owner hold was requested");
  });
});
