import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash, randomBytes } from "node:crypto";
import { RECOVERY_BUNDLE_REF, readRecoveryBundleHeader } from "../src/git.js";
import type { TestContext } from "node:test";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { Outbox } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import type { RecoveryUploadManifest } from "../src/protocol.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { nullLogger } from "./helpers.js";
import { api, baseUrl, client, deferred, fakeGitlab, fx, git, gitlabClaim, runner } from "./runner-harness.js";

/** Real git, MAC journals, archive bytes, Outbox and runner; only server responses are fixtures.
 * No FINAL or source cleanup is supplied as report-retirement authority. */
export async function reportRetirementFixture(t: TestContext, options: {
  archiveGeneration?: number;
  capturedWip?: boolean;
  invalidJournal?: boolean;
  liveFlight?: boolean;
  thinArchive?: boolean;
} = {}) {
  const generation = 2, archiveGeneration = options.archiveGeneration ?? generation;
  const claim = gitlabClaim(2652, { claim_generation: generation, inventory_guarded: true });
  const token = "report-retirement-journal-key";
  const captureId = "00000000-0000-4000-8000-000000002652";
  const holdId = "33333333-3333-4333-8333-333333333333";
  let status = "running";
  let manifest: RecoveryUploadManifest | undefined;
  let uploadedBytes: Buffer | undefined;
  const calls = { final: 0, cloneCleanup: 0, sourceDelete: 0, pinDelete: 0, sends: 0, cancelledAcks: 0 };
  const ownership = { mode: "healthy" as "healthy" | "unknown" | "throw", generation };
  const capture = { mode: "healthy" as "healthy" | "throw" | "expired" | "unbound" | "checksum" };
  const originalFetch = globalThis.fetch;
  let workerId = "";
  api.setRegisterProtocolFeatures(["recovery_inventory_v1", "terminal_rejection_report"]);
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin !== new URL(baseUrl).origin ||
        url.pathname !== `/api/worker/runs/${claim.run_id}/terminal-rejection-custody`) {
      return originalFetch(input, init);
    }
    assert.equal(Number(url.searchParams.get("generation")), generation);
    assert.ok(workerId);
    return Response.json({
      run_id: claim.run_id, worker_id: workerId, generation,
      exact_holds: [{ id: holdId, state: "open" }], sibling_holds: [],
      exact_count: 1, sibling_count: 0, complete: true, exact_complete: true,
      sibling_complete: true, outcome: "retained",
    });
  });
  const registeredWorkerId = (await client.register("test")).worker_id;
  assert.ok(registeredWorkerId);
  workerId = registeredWorkerId;
  client.getRunOwnership = async runId => {
    assert.equal(runId, claim.run_id);
    if (ownership.mode === "throw") throw new Error("ownership unavailable");
    return { status: ownership.mode === "unknown" ? "unknown" : status,
      claim_generation: ownership.generation, inventory_guarded: true };
  };
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: [{
    hold_id: holdId, generation: archiveGeneration, inventory_guarded: true,
    has_available_capture: !!manifest,
  }] });
  let reserved: { sourceSha: string; digest: string; generation: number } | undefined;
  client.reserveRecoveryCapture = async (runId, req) => {
    assert.equal(runId, claim.run_id);
    assert.equal(req.run_id, claim.run_id);
    assert.equal(req.generation, archiveGeneration);
    assert.match(req.source_sha, /^[0-9a-f]{40}$/);
    assert.match(req.coverage_digest!, /^[0-9a-f]{64}$/);
    reserved = { sourceSha: req.source_sha, digest: req.coverage_digest!, generation: req.generation! };
    return { capture_id: captureId, state: "preparing" };
  };
  client.uploadRecoveryBundle = async (_run, _id, m, stream) => {
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    uploadedBytes = Buffer.concat(chunks);
    assert.equal(uploadedBytes.length, m.byte_size);
    assert.equal(createHash("sha256").update(uploadedBytes).digest("hex"), m.checksum);
    manifest = m;
    return { capture_id: captureId, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async (runId, requestedCapture) => {
    assert.equal(runId, claim.run_id);
    assert.equal(requestedCapture, captureId);
    if (capture.mode === "throw") throw new Error("capture status unavailable");
    return { capture_id: captureId, state: "available",
      manifest_bound: capture.mode !== "unbound",
      checksum: capture.mode === "checksum" ? "0".repeat(64) : manifest?.checksum,
      byte_size: manifest?.byte_size,
      expires_at: capture.mode === "expired" ? "2000-01-01T00:00:00Z" : "2099-01-01T00:00:00Z" };
  };
  client.releaseRecoveryCustody = async () => {
    calls.final++;
    return { run_id: claim.run_id, generation, released: false, holds_released: 0 };
  };
  // Spies delegate: a regression that deletes anything fails both counters and physical checks.
  for (const [method, counter] of [
    ["removeRunnerClone", "cloneCleanup"], ["cleanupRecoveryGeneration", "sourceDelete"],
    ["deleteRecoveryPin", "pinDelete"],
  ] as const) {
    const real = git[method].bind(git);
    t.mock.method(git, method, async (...args: Parameters<typeof real>) => {
      calls[counter]++;
      return (real as (...args: Parameters<typeof real>) => ReturnType<typeof real>)(...args);
    });
  }
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    return { ok: true, body: { published: false, ref: "refs/uzi-checkpoints/tester", reason: "workflow_scope" } };
  };
  const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
    runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
  await outbox.init();
  assert.equal(outbox.isDisabled(), false);
  const registry = new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 100);
  const entered = deferred(), leave = deferred();
  let clone = "", head = "", executions = 0;
  const cmd = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], {
    encoding: "utf8", timeout: 10_000,
    env: Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("GIT_"))),
  }).trim();
  const commit = (name: string) => {
    fs.writeFileSync(path.join(clone, name), name + "\n");
    cmd(clone, ["add", name]);
    cmd(clone, ["-c", "user.name=tester", "-c", "user.email=tester@example.test",
      "-c", "commit.gpgsign=false", "commit", "-m", name]);
    return cmd(clone, ["rev-parse", "HEAD"]);
  };
  if (options.thinArchive) {
    // Real public history makes full-first exceed this test contract cap.
    fs.writeFileSync(path.join(fx.originPath, "public-history.bin"), randomBytes(256 * 1024));
    cmd(fx.originPath, ["add", "public-history.bin"]);
    cmd(fx.originPath, ["commit", "-m", "public history"]);
    fs.unlinkSync(path.join(fx.originPath, "public-history.bin"));
    cmd(fx.originPath, ["add", "-u"]);
    cmd(fx.originPath, ["commit", "-m", "public deletion"]);
    const produce = git.produceRecoveryBundle.bind(git);
    t.mock.method(git, "produceRecoveryBundle", (barePath: string, opts: Parameters<typeof produce>[1]) =>
      produce(barePath, { ...opts, maxBytes: 32 * 1024 }));
  }
  api.refuseStateWith409(claim.run_id);
  let foreground = true;
  const processObservation = { killed: [] as number[], calls: 0 };
  const r = runner({ run: async ctx => {
    executions++;
    if (executions > 1) throw new Error("cancelled queued duplicate SDK flight");
    clone = ctx.worktreePath;
    head = commit("unpublished.txt");
    if (options.capturedWip) {
      fs.writeFileSync(path.join(clone, "marker.txt"), "captured WIP\n");
      assert.equal(await git.commitWipMarker(clone), true);
      head = cmd(clone, ["rev-parse", "HEAD"]);
      assert.match(cmd(clone, ["log", "-1", "--format=%s"]), /^wip\(park\):/);
    }
    await ctx.checkpoint!({ reap: false });
    entered.resolve();
    await leave.promise;
    throw new Error("cancelled unpublished SDK error");
  } }, fakeGitlab().gitlab, token, { outbox, activeRuns: registry, checkpointIntervalMs: 1,
    // The foreground error retains its source. Later replay gets healthy physical quiescence.
    quiesceRun: async () => {
      processObservation.calls++;
      return {
        process: { state: foreground ? "unverified" : "quiescent", processes: [], killed: [...processObservation.killed], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    },
  });
  const flight = r.execute(claim);
  // Every test has bounded node-test timeout; cleanup releases both event barriers on assertion failure.
  t.after(async () => { leave.resolve(); await flight; });
  await Promise.race([entered.promise, flight.then(() => {
    throw new Error("execute settled before reaching the SDK fixture barrier");
  })]);
  assert.equal(r.isExecuting(claim.run_id), true, "actual SDK flight owns its execution tail");
  assert.ok(registry.build().active.some(e => e.run_id === claim.run_id && e.claim_generation === generation));
  status = "cancelled";
  if (options.liveFlight) {
    // Seed a report while the actual SDK remains stopped at the event barrier.
    // The standalone producer manufactures bytes only; the real runner owns replay authority.
    assert.equal((await outbox.journalTerminal(claim.run_id, generation, "running", 0,
      { status: "failed", failure_reason: "cancelled unpublished SDK error" })).journaled, true);
  } else {
    leave.resolve();
    await flight;
    foreground = false;
    assert.equal(r.isExecuting(claim.run_id), false, "execute finally released its own tail");
    assert.ok(await outbox.readTerminalJournal(claim.run_id, generation), "real failed report remains pending");
  }

  const bare = git.barePathFor(fx.originPath);
  const branch = cmd(clone, ["branch", "--show-current"]);
  const context: PositiveOwedCandidateContext = {
    barePath: bare, runId: claim.run_id, generation: archiveGeneration, kind: "issue", branch,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: cmd(bare, ["rev-parse", "refs/remotes/origin/main"]) },
  };
  await git.fetchAgentBranch(bare, clone, branch, claim.run_id, { context });
  const coordinator = new RecoveryCoordinator({ client, git, log: nullLogger(),
    recoveryRoot: git.recoveryRoot, workerToken: token });
  assert.ok(await coordinator.pin({ runId: claim.run_id, generation: archiveGeneration,
    inventoryGuarded: true, kind: "issue", branch, sourceSha: head }));
  const inventory = await coordinator.freezeInventory({ context, currentSha: head, defaultBranch: "main" });
  assert.ok(inventory, "real frozen inventory");
  assert.equal((await coordinator.captureAndUpload({ record: inventory, barePath: bare, defaultBranch: "main" })).state, "uploaded");
  const archive = (await coordinator.inspect(claim.run_id)).find(row => row.captureId === inventory.captureId)!;
  assert.equal(archive.generation, archiveGeneration);
  assert.equal(archive.finalAcknowledged === true, false);
  assert.ok(archive.coverageDigest && archive.bundlePath && uploadedBytes);
  assert.deepEqual(fs.readFileSync(archive.bundlePath), uploadedBytes);
  cmd(bare, ["bundle", "verify", archive.bundlePath]);
  if (options.thinArchive) {
    const header = await readRecoveryBundleHeader(archive.bundlePath, archive.sourceSha);
    assert.equal(header.selfContained, false);
    assert.ok(header.prerequisiteShas.length > 0, "actual bundle has prerequisites");
    const fresh = path.join(fx.dataDir, "thin-import");
    cmd(fx.dataDir, ["clone", "--no-local", fx.originPath, fresh]);
    assert.throws(() => cmd(fresh, ["cat-file", "-e", head]));
    cmd(fresh, ["bundle", "verify", archive.bundlePath]);
    cmd(fresh, ["fetch", archive.bundlePath, RECOVERY_BUNDLE_REF + ":refs/heads/recovered"]);
    assert.equal(cmd(fresh, ["show", "recovered:unpublished.txt"]), "unpublished.txt");
    cmd(fresh, ["merge-base", "--is-ancestor", head, "recovered"]);
  }
  assert.equal(cmd(clone, ["status", "--porcelain"]), "", "clean captured source");
  assert.equal(calls.final, 0);
  assert.equal(calls.cloneCleanup, 0);
  assert.deepEqual(reserved, { sourceSha: archive.sourceSha, digest: archive.coverageDigest,
    generation: archive.generation });
  if (options.invalidJournal) {
    const journalPath = path.join(git.recoveryRoot, claim.run_id, archive.captureId + ".json");
    const journal = JSON.parse(fs.readFileSync(journalPath, "utf8"));
    assert.match(journal.mac, /^[0-9a-f]{64}$/);
    journal.mac = "0".repeat(64);
    fs.writeFileSync(journalPath, JSON.stringify(journal));
  }
  assert.equal(r.isExecuting(claim.run_id), options.liveFlight === true);
  const refs = cmd(bare, ["show-ref"]);
  const files = new Map<string, Buffer>();
  const collect = (dir: string) => {
    for (const item of fs.readdirSync(dir, { withFileTypes: true })) {
      const file = path.join(dir, item.name);
      if (item.isDirectory()) collect(file);
      else if (item.isFile()) files.set(file, fs.readFileSync(file));
    }
  };
  collect(git.recoveryRoot);
  const deps = makeTerminalOutboxDeps(outbox, client, {
    gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger(),
  });
  assert.ok(deps);
  const replay = async () => {
    const before = calls.cancelledAcks;
    await resolvePendingTerminal(r.protectRecoveryTerminalDeps(deps), {
      runId: claim.run_id, claimGeneration: generation,
      send: async body => {
        calls.sends++;
        assert.equal(body.status, "failed");
        calls.cancelledAcks++;
        return { applied: false, status: "cancelled" };
      },
    });
    // resolve swallows send errors; assert outside it so a broken ACK fixture cannot pass retention.
    assert.equal(calls.cancelledAcks, before + 1, "replay returned the terminal cancelled ACK");
  };
  const assertCustody = async () => {
    assert.deepEqual({ final: calls.final, cloneCleanup: calls.cloneCleanup,
      sourceDelete: calls.sourceDelete, pinDelete: calls.pinDelete },
    { final: 0, cloneCleanup: 0, sourceDelete: 0, pinDelete: 0 });
    assert.equal(fs.statSync(clone).isDirectory(), true);
    assert.equal(cmd(bare, ["show-ref"]), refs, "source and recovery pins remain");
    for (const [file, bytes] of files) assert.deepEqual(fs.readFileSync(file), bytes, file);
    assert.equal((await client.listRecoveryHolds(claim.run_id)).holds.length, 1);
    assert.equal((await coordinator.inspect(claim.run_id)).find(row => row.captureId === archive.captureId)?.finalAcknowledged === true, false);
  };
  const assertPending = async () => {
    assert.ok(await outbox.readTerminalJournal(claim.run_id, generation), "unsafe report must remain pending");
    assert.deepEqual(outbox.listPendingTerminals().map(p => [p.run_id, p.claim_generation]), [[claim.run_id, generation]]);
    assert.ok(registry.build().active.some(e => e.run_id === claim.run_id &&
      e.claim_generation === generation && e.terminal_pending));
    await assertCustody();
  };
  // Three fresh attempts retain the production budget and await actual settlement;
  // the node-test timeout caps stalled settlement. Failed assertions stop retries.
  const positiveStage = async (label: string, attempt: () => Promise<unknown>,
    reached: () => boolean, retained: () => Promise<void>) => {
    for (let n = 1; n <= 3; n++) {
      await attempt();
      await assertCustody();
      if (reached()) return n;
      await retained();
    }
    assert.fail(`${label}: persistent refusal after 3 fresh bounded attempts; sends=${calls.sends}, observations=${processObservation.calls}`);
  };
  const replayPositive = () => positiveStage("terminal retirement", replay,
    () => !outbox.hasPendingTerminal(claim.run_id, generation), assertPending);
  const assertFinalizePending = async () => {
    assert.ok(outbox.finalizeRecordIdentity(claim.run_id, generation), "finalize must remain pending");
    await assertCustody();
  };
  const expireNextProof = () => {
    const real = git.withReportProofBudget.bind(git);
    const observation = { expired: false };
    const mock = t.mock.method(git, "withReportProofBudget", async <T>(
      budget: { signal: AbortSignal; deadline: number }, action: () => Promise<T>,
    ): Promise<T> => {
      mock.mock.restore(); // Only this proof stalls; the next attempt gets a fresh budget.
      if (!budget.signal.aborted) await new Promise<void>(resolve =>
        budget.signal.addEventListener("abort", () => resolve(), { once: true }));
      observation.expired = budget.signal.aborted && Date.now() >= budget.deadline;
      return real(budget, action);
    });
    return observation;
  };
  const liveFlight = { flight, release: leave.resolve, executions: () => executions };
  return { claim, generation, archive, outbox, registry, r, coordinator, replay, expireNextProof, replayPositive, positiveStage, assertFinalizePending, assertPending,
    assertCustody, ownership, capture, clone, bare, head, cmd, commit, context, calls, liveFlight, processObservation };
}
