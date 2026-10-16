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
import type { RecoveryRecord } from "../src/recovery.js";
import { reportProofTiming } from "../src/report-proof-timing.js";
import type { PositiveOwedCandidateContext } from "../src/git.js";
import type { RecoveryUploadManifest } from "../src/protocol.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { nullLogger } from "./helpers.js";
import { api, baseUrl, client, deferred, fakeGitlab, fx, git, gitlabClaim, runner } from "./runner-harness.js";

/** Real git, MAC journals, archive bytes, Outbox and runner; server responses and process observations are fixtures.
 * By default no FINAL or cleanup authority is supplied. acknowledgeArchive drives real FINAL
 * and generation cleanup for the producer-ordering regression. */
export async function reportRetirementFixture(t: TestContext, options: {
  archiveGeneration?: number;
  capturedWip?: boolean;
  invalidJournal?: boolean;
  liveFlight?: boolean;
  thinArchive?: boolean;
  olderOpenHold?: boolean;
  currentArchiveReleased?: boolean;
  olderSource?: boolean;
  priorPendingArchiveFinal?: boolean;
  priorArchiveWithoutFinal?: boolean;
  coverPriorFinal?: boolean;
  policyCustody?: boolean;
  policyLegacyCurrent?: boolean;
} = {}) {
  const generation = 2, archiveGeneration = options.archiveGeneration ?? generation;
  const claim = gitlabClaim(2652, { claim_generation: generation, inventory_guarded: options.policyLegacyCurrent !== true });
  const token = "report-retirement-journal-key";
  const captureId = "00000000-0000-4000-8000-000000002652";
  const holdId = "33333333-3333-4333-8333-333333333333";
  let status = "running";
  let manifest: RecoveryUploadManifest | undefined;
  let uploadedBytes: Buffer | undefined;
  const calls = { final: 0, cloneCleanup: 0, sourceDelete: 0, pinDelete: 0, sends: 0, cancelledAcks: 0, priorArchiveFinal: 0 };
  const ownership = { mode: "healthy" as "healthy" | "unknown" | "throw", generation };
  const capture = { mode: "healthy" as "healthy" | "throw" | "expired" | "unbound" | "checksum" };
  const originalFetch = globalThis.fetch;
  let workerId = "";
  const policyCustody = { openGeneration: undefined as number | undefined, unknown: false, generations: [] as number[] };
  api.setRegisterProtocolFeatures(["recovery_inventory_v1", "terminal_rejection_report"]);
  t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
    const url = new URL(input instanceof Request ? input.url : String(input));
    if (url.origin !== new URL(baseUrl).origin ||
        url.pathname !== `/api/worker/runs/${claim.run_id}/terminal-rejection-custody`) {
      return originalFetch(input, init);
    }
    const requestedGeneration = Number(url.searchParams.get("generation"));
    if (options.policyCustody) {
      assert.ok(Number.isSafeInteger(requestedGeneration) && requestedGeneration > 0);
      policyCustody.generations.push(requestedGeneration);
      const open = policyCustody.openGeneration === requestedGeneration;
      return Response.json({
        run_id: claim.run_id, worker_id: workerId, generation: requestedGeneration,
        exact_holds: policyCustody.unknown ? [] : [{ id: holdId, state: open ? "open" : "released" }],
        sibling_holds: [], exact_count: policyCustody.unknown ? 0 : 1, sibling_count: 0,
        complete: true, exact_complete: true, sibling_complete: true,
        outcome: policyCustody.unknown ? "unknown" : open ? "retained" : "settled",
      });
    }
    assert.equal(requestedGeneration, generation);
    assert.ok(workerId);
    return Response.json({
      run_id: claim.run_id, worker_id: workerId, generation,
      exact_holds: [{ id: holdId, state: options.currentArchiveReleased ? "released" : "open" }],
      sibling_holds: options.olderOpenHold ? [{ id: "44444444-4444-4444-8444-444444444444", state: "open" }] : [],
      exact_count: 1, sibling_count: options.olderOpenHold ? 1 : 0, complete: true, exact_complete: true,
      sibling_complete: true, outcome: "retained",
    });
  });
  const registeredWorkerId = (await client.register("test")).worker_id;
  assert.ok(registeredWorkerId);
  workerId = registeredWorkerId;
  client.getRunOwnership = async runId => {
    assert.equal(runId, claim.run_id);
    if (ownership.mode === "throw") throw new Error("ownership unavailable");
    // SYNTHETIC SERVER: lose ownership visibility after real G1 upload, before FINAL mint.
    const beforePriorFinal = options.priorArchiveWithoutFinal && ownership.generation === 1 &&
      captures.has("00000000-0000-4000-8000-000000002651");
    return { status: ownership.mode === "unknown" || beforePriorFinal ? "unknown" : status,
      claim_generation: ownership.generation, inventory_guarded: options.policyLegacyCurrent !== true };
  };
  const captures = new Map<string, { manifest: RecoveryUploadManifest; bytes: Buffer }>();
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: [...(options.currentArchiveReleased || (options.priorPendingArchiveFinal && ownership.generation < generation) ? [] : [{
    hold_id: holdId, generation: archiveGeneration, inventory_guarded: true,
    has_available_capture: !!manifest,
  }]), ...(options.olderOpenHold ? [{ hold_id: "44444444-4444-4444-8444-444444444444",
    generation: 1, inventory_guarded: true,
    has_available_capture: options.priorPendingArchiveFinal === true && captures.has("00000000-0000-4000-8000-000000002651") }] : [])] });
  const historyEvents: string[] = [];
  let admissionCapture = false;
  let unavailableAdmissionSource: string | undefined;
  let reserved: { sourceSha: string; digest: string; generation: number } | undefined;
  client.reserveRecoveryCapture = async (runId, req) => {
    assert.equal(runId, claim.run_id);
    assert.equal(req.run_id, claim.run_id);
    assert.match(req.source_sha, /^[0-9a-f]{40}$/);
    assert.match(req.coverage_digest!, /^[0-9a-f]{64}$/);
    if (options.priorPendingArchiveFinal && (admissionCapture || req.source_sha === unavailableAdmissionSource)) {
      unavailableAdmissionSource = req.source_sha;
      historyEvents.push("G2 admission reserve unavailable; verified local bundle retained");
      throw new Error("transient admission reserve unavailable");
    }
    assert.equal(req.generation, options.priorPendingArchiveFinal ? ownership.generation : archiveGeneration);
    reserved = { sourceSha: req.source_sha, digest: req.coverage_digest!, generation: req.generation! };
    return { capture_id: options.priorPendingArchiveFinal && req.generation === 1
      ? "00000000-0000-4000-8000-000000002651" : captureId, state: "preparing" };
  };
  client.uploadRecoveryBundle = async (_run, id, m, stream) => {
    const chunks: Buffer[] = [];
    for await (const chunk of stream) chunks.push(Buffer.from(chunk));
    uploadedBytes = Buffer.concat(chunks);
    assert.equal(uploadedBytes.length, m.byte_size);
    assert.equal(createHash("sha256").update(uploadedBytes).digest("hex"), m.checksum);
    manifest = m;
    captures.set(id, { manifest: m, bytes: uploadedBytes });
    historyEvents.push(`G${ownership.generation} real archive uploaded`);
    return { capture_id: id, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async (runId, requestedCapture) => {
    assert.equal(runId, claim.run_id);
    const observed = captures.get(requestedCapture)?.manifest;
    assert.ok(observed);
    if (capture.mode === "throw") throw new Error("capture status unavailable");
    return { capture_id: requestedCapture, state: "available",
      manifest_bound: capture.mode !== "unbound",
      checksum: capture.mode === "checksum" ? "0".repeat(64) : observed.checksum,
      byte_size: observed.byte_size,
      expires_at: capture.mode === "expired" ? "2000-01-01T00:00:00Z" : "2099-01-01T00:00:00Z" };
  };
  client.releaseRecoveryCustody = async (runId, gen, _evidence, disposition) => {
    if (options.priorPendingArchiveFinal && gen === 1) {
      assert.equal(runId, claim.run_id);
      assert.equal(disposition?.kind, "archive");
      calls.priorArchiveFinal++;
      historyEvents.push("G1 archive FINAL RPC unavailable (unacknowledged)");
      throw new Error("archive FINAL unavailable");
    }
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
  let priorFinal: RecoveryRecord | undefined;
  let predecessorClone: string | undefined;
  let predecessorH1 = "", predecessorH2 = "";
  let successorAdmission: { head: string; clone: string; stage?: string; restoreTip?: string; attemptId?: string } | undefined;
  if (options.priorPendingArchiveFinal) {
    assert.equal(options.olderOpenHold, true, "earlier producer requires its exact-generation hold");
    ownership.generation = 1;
    const predecessorStartedAt = Date.now();
    let predecessorNow = predecessorStartedAt;
    const predecessor = runner({ run: async ctx => {
      clone = ctx.worktreePath;
      predecessorClone = clone;
      head = predecessorH1 = commit("predecessor-H1.txt");
      await ctx.checkpoint!({ reap: false });
      historyEvents.push("G1 SDK H1 checkpoint unconfirmed, owed");
      head = predecessorH2 = commit("predecessor-H2.txt");
      // Test clock crosses the harness's 20-second wall budget without a real wait.
      // SetRunWallPark requires an expired deadline, not merely a pause request.
      predecessorNow = predecessorStartedAt + 21_000;
      assert.equal(await ctx.parkForWall!({ completedCount: 2, total: 3 }), "parked");
      status = "paused";
      historyEvents.push("G1 actual wall park accepted paused (checkpoint unconfirmed)");
      return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
    } }, fakeGitlab().gitlab, token, { outbox, checkpointIntervalMs: 1, now: () => predecessorNow,
      // SYNTHETIC PROCESS OBSERVATIONS only; park, producer, bundles and Git stay real.
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      }),
    });
    await predecessor.execute({ ...claim, claim_generation: 1 });
    assert.equal(predecessor.isExecuting(claim.run_id), false);
    assert.equal(api.wallParkRequests.length, 1);
    assert.equal(api.wallParkRequests[0]!.body.published, false);
    const bootRecords = await predecessor.snapshotBootRecoveries();
    await predecessor.resumePendingRecoveries(undefined, bootRecords);
    const reader = new RecoveryCoordinator({ client, git, log: nullLogger(),
      recoveryRoot: git.recoveryRoot, workerToken: token });
    const priorRecords = await reader.inspect(claim.run_id);
    priorFinal = priorRecords.find(row => row.generation === 1 && row.state === "uploaded");
    assert.ok(priorFinal, "actual paused flight and due replay uploaded G1 archive");
    if (options.priorArchiveWithoutFinal) {
      assert.equal(priorFinal.finalRequest, undefined, "ownership unavailable prevented FINAL mint");
      assert.equal(calls.priorArchiveFinal, 0);
      historyEvents.push("G1 real uploaded wrapper retained without FINAL (synthetic ownership unavailable)");
    } else {
      assert.ok(priorFinal.finalRequest, "actual paused flight and due replay journal archive FINAL");
      assert.equal(priorFinal.finalRequest.disposition.kind, "archive");
      assert.ok(calls.priorArchiveFinal > 0, "real archive FINAL was attempted and unavailable");
    }
    assert.notEqual(priorFinal.finalAcknowledged, true);
    assert.deepEqual(priorFinal.originalRoots!.map(root => root.sha).sort(),
      [predecessorH1, predecessorH2].sort(), "exact SDK H1/H2 roots frozen by G1");
    assert.equal(priorFinal.inventoryCurrentSha, predecessorH2);
    const source = await git.discoverRetainedRecovery(fx.originPath,
      cmd(predecessorClone!, ["branch", "--show-current"]), "issue-2652", claim.run_id);
    assert.ok(source, "real retained G1 source discoverable before successor admission");
    assert.equal(source.journal.clonePath, predecessorClone);
    // SYNTHETIC SERVER: owner extends the exhausted worker-side wall park within its cap.
    // runtime.sql ExtendAndResumeWallPark adds budget and queues it; ClaimRun increments to 2.
    // A bare ResumeRunNow would refuse the exhausted budget (api/internal/handler/resume.go).
    assert.equal(status, "paused");
    status = "queued";
    historyEvents.push("supported owner ExtendAndResumeWallPark adds budget -> queued (synthetic server)");
    ownership.generation = generation;
    status = "running";
    admissionCapture = true;
    historyEvents.push("supported queued ClaimRun -> running G2 (synthetic server)");
    client.publishCheckpoint = async (runId, tip, pack) => {
      assert.equal(runId, claim.run_id);
      const chunks: Buffer[] = [];
      for await (const chunk of pack) chunks.push(Buffer.from(chunk));
      const bytes = Buffer.concat(chunks);
      assert.ok(bytes.length);
      // Broker consumes the actual producer pack and updates the local forge before ACK.
      execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin"], {
        input: bytes, timeout: 10_000,
        env: Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.startsWith("GIT_"))),
      });
      const branch = cmd(predecessorClone!, ["branch", "--show-current"]);
      const ref = `refs/uzi-checkpoints/${branch}`;
      cmd(fx.originPath, ["update-ref", ref, tip]);
      assert.equal(cmd(fx.originPath, ["rev-parse", ref]), tip);
      historyEvents.push("G2 admission checkpoint H2 confirmed on physical forge before freeze");
      return { ok: true, body: { published: true, ref, tip } };
    };
  }
  api.refuseStateWith409(claim.run_id);
  let foreground = !options.priorPendingArchiveFinal;
  const processObservation = { killed: [] as number[], calls: 0 };
  const r = runner({ run: async ctx => {
    executions++;
    foreground = true;
    if (executions > 1) throw new Error("cancelled queued duplicate SDK flight");
    clone = ctx.worktreePath;
    if (options.priorPendingArchiveFinal) {
      const admitted = await git.discoverRetainedRecovery(fx.originPath,
        cmd(clone, ["branch", "--show-current"]), "issue-2652", claim.run_id);
      assert.ok(admitted, "public discovery reads the actual admitted successor");
      successorAdmission = { head: cmd(clone, ["rev-parse", "HEAD"]), clone,
        stage: admitted.journal.recovery?.stage, restoreTip: admitted.journal.recovery?.restoreTip,
        attemptId: admitted.journal.attemptId };
      admissionCapture = false;
      historyEvents.push("G2 SDK admitted on actual fresh attempt");
      client.publishCheckpoint = async (_run, _tip, pack) => {
        for await (const chunk of pack) assert.ok(chunk.length);
        return { ok: true, body: { published: false, ref: `refs/uzi-checkpoints/${ctx.branch}`, reason: "workflow_scope" } };
      };
    }
    if (options.coverPriorFinal) {
      assert.ok(priorFinal?.bundlePath && priorFinal.finalRequest);
      // Real SDK Git operations import and merge the old wrapper; no ancestry seam.
      cmd(clone, ["fetch", priorFinal.bundlePath, RECOVERY_BUNDLE_REF + ":refs/heads/prior-archive"]);
      cmd(clone, ["-c", "user.name=tester", "-c", "user.email=tester@example.test",
        "-c", "commit.gpgsign=false", "merge", "--no-ff", "-m", "merge prior archive", "prior-archive"]);
      historyEvents.push("G2 SDK merged actual G1 FINAL wrapper before H3");
    }
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
    if (options.priorPendingArchiveFinal) foreground = false;
    throw new Error("cancelled unpublished SDK error");
  } }, fakeGitlab().gitlab, token, { outbox, activeRuns: registry, checkpointIntervalMs: 1,
    // SYNTHETIC PROCESS PROOF: fixture observation only; Git source/content proof stays real.
    // The foreground error retains its source. Later replay gets healthy physical quiescence.
    quiesceRun: async () => {
      processObservation.calls++;
      return {
        process: { state: foreground ? "unverified" : "quiescent", processes: [], killed: [...processObservation.killed], detail: "" },
        docker: { state: "not_wired", removed: [], detail: "" },
      };
    },
  });
  // Keep the ordinary terminal control pending independently of the policy proof.
  if (options.policyLegacyCurrent) r.recoveryInventoryPending = async () => true;
  const flight = r.execute(claim);
  // Every test has bounded node-test timeout; cleanup releases both event barriers on assertion failure.
  t.after(async () => { leave.resolve(); await flight; });
  await Promise.race([entered.promise, flight.then(() => {
    throw new Error("execute settled before reaching the SDK fixture barrier");
  })]);
  assert.equal(r.isExecuting(claim.run_id), true, "actual SDK flight owns its execution tail");
  if (!options.priorPendingArchiveFinal) assert.ok(registry.build().active.some(e => e.run_id === claim.run_id && e.claim_generation === generation));
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

  if (options.priorPendingArchiveFinal) {
    const pending = await r.snapshotBootRecoveries();
    await r.resumePendingRecoveries(undefined, pending.filter(record => record.generation === generation));
  }
  const bare = git.barePathFor(fx.originPath);
  const branch = cmd(clone, ["branch", "--show-current"]);
  const context: PositiveOwedCandidateContext = {
    barePath: bare, runId: claim.run_id, generation: archiveGeneration, kind: "issue", branch,
    defaultIdentity: { ref: "refs/remotes/origin/main", sha: cmd(bare, ["rev-parse", "refs/remotes/origin/main"]) },
  };
  const coordinator = new RecoveryCoordinator({ client, git, log: nullLogger(),
    recoveryRoot: git.recoveryRoot, workerToken: token });
  let archive: RecoveryRecord;
  if (options.priorPendingArchiveFinal) {
    // Actual terminal journal came from execute; normal due replay captured the G2 inventory.
    archive = (await coordinator.inspect(claim.run_id)).find(row => row.generation === archiveGeneration && row.state === "uploaded")!;
  } else {
    await git.fetchAgentBranch(bare, clone, branch, claim.run_id, { context });
    if (options.olderOpenHold && options.olderSource !== false) assert.ok(await coordinator.pin({ runId: claim.run_id, generation: 1,
      inventoryGuarded: true, kind: "issue", branch, sourceSha: head }));
    assert.ok(await coordinator.pin({ runId: claim.run_id, generation: archiveGeneration,
      inventoryGuarded: true, kind: "issue", branch, sourceSha: head }));
    const inventory = await coordinator.freezeInventory({ context, currentSha: head, defaultBranch: "main" });
    assert.ok(inventory, "real frozen inventory");
    assert.equal((await coordinator.captureAndUpload({ record: inventory, barePath: bare, defaultBranch: "main" })).state, "uploaded");
    archive = (await coordinator.inspect(claim.run_id)).find(row => row.captureId === inventory.captureId)!;
  }
  assert.ok(archive, "real uploaded archive exists");
  assert.equal(archive.generation, archiveGeneration);
  assert.equal(archive.finalAcknowledged === true, false);
  assert.ok(archive.coverageDigest && archive.bundlePath && uploadedBytes);
  assert.deepEqual(fs.readFileSync(archive.bundlePath), captures.get(archive.serverCaptureId!)!.bytes);
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
  assert.equal(calls.final, options.priorPendingArchiveFinal ? 1 : 0);
  const finalCallsBeforeProof = calls.final;
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
    { final: finalCallsBeforeProof, cloneCleanup: 0, sourceDelete: 0, pinDelete: 0 });
    assert.equal(fs.statSync(clone).isDirectory(), true);
    assert.equal(cmd(bare, ["show-ref"]), refs, "source and recovery pins remain");
    for (const [file, bytes] of files) assert.deepEqual(fs.readFileSync(file), bytes, file);
    assert.equal((await client.listRecoveryHolds(claim.run_id)).holds.length,
      (options.currentArchiveReleased ? 0 : 1) + (options.olderOpenHold ? 1 : 0));
    assert.equal((await coordinator.inspect(claim.run_id)).find(row => row.captureId === archive.captureId)?.finalAcknowledged === true, false);
  };
  const assertPending = async () => {
    assert.ok(await outbox.readTerminalJournal(claim.run_id, generation), "unsafe report must remain pending");
    assert.deepEqual(outbox.listPendingTerminals().map(p => [p.run_id, p.claim_generation]), [[claim.run_id, generation]]);
    const settled = outbox.listPendingTerminals().find(e => e.claim_generation === generation)?.server_settled === true;
    assert.equal(registry.build().active.some(e => e.run_id === claim.run_id &&
      e.claim_generation === generation && e.terminal_pending), !settled,
    "retained custody evidence advertises only an unresolved server outcome");
    await assertCustody();
  };
  // Positive stages lend the three production wall-clock bounds headroom so a slow runner cannot
  // expire a proof that is otherwise valid. The deliberate-expiry test (expireNextProof) also uses
  // headroom, to reach its first budgeted boundary on any runner speed, and then fires the captured
  // production deadline callback by hand instead of waiting for the real 1 s budget. Mocks are
  // restored by the caller (positiveStage, expireNextProof's restore) or at test end.
  // Distinct values let captureTimers tell the three production timers apart.
  const headroom = { proofMs: 61_001, authorityMs: 61_002, passMs: 61_003 };
  const headroomMs = headroom.proofMs;
  let headroomActive = false;
  const useHeadroom = () => {
    if (headroomActive) return () => {};
    headroomActive = true;
    const mocks = [
      t.mock.method(reportProofTiming, "proofBudgetMs", () => headroom.proofMs),
      t.mock.method(reportProofTiming, "finalizeAuthorityMs", () => headroom.authorityMs),
      t.mock.method(reportProofTiming, "finalizePassMs", () => headroom.passMs),
    ];
    return () => {
      if (!headroomActive) return;
      headroomActive = false;
      for (const mock of mocks) mock.mock.restore();
    };
  };
  // Records the callbacks of production setTimeout calls whose delay matches, so a test can fire
  // exactly one production deadline callback on demand. The real timer is still scheduled (and
  // cleared by production code), and `restore` puts the global back.
  const captureTimers = (match: (ms: number) => boolean) => {
    const real = globalThis.setTimeout;
    const captured: Array<() => void> = [];
    const mock = t.mock.method(globalThis, "setTimeout", ((fn: (...a: unknown[]) => void, ms?: number, ...args: unknown[]) => {
      if (typeof fn === "function" && typeof ms === "number" && match(ms)) captured.push(() => fn(...args));
      return (real as (...a: unknown[]) => NodeJS.Timeout).call(globalThis, fn, ms, ...args);
    }) as unknown as typeof setTimeout);
    return {
      count: () => captured.length,
      fire: () => { for (const callback of captured.splice(0)) callback(); },
      restore: () => mock.mock.restore(),
    };
  };
  // Three fresh attempts with headroom and actual settlement; the node-test timeout caps stalled
  // settlement. Failed assertions stop retries.
  const positiveStage = async (label: string, attempt: () => Promise<unknown>,
    reached: () => boolean, retained: () => Promise<void>) => {
    const restore = useHeadroom();
    try {
      for (let n = 1; n <= 3; n++) {
        await attempt();
        await assertCustody();
        if (reached()) return n;
        await retained();
      }
    } finally { restore(); }
    assert.fail(`${label}: persistent refusal after 3 fresh bounded attempts; sends=${calls.sends}, observations=${processObservation.calls}`);
  };
  const replayPositive = () => positiveStage("terminal retirement", replay,
    () => !outbox.hasPendingTerminal(claim.run_id, generation), assertPending);
  const assertFinalizePending = async () => {
    assert.ok(outbox.finalizeRecordIdentity(claim.run_id, generation), "finalize must remain pending");
    if (options.currentArchiveReleased) {
      assert.equal((await coordinator.inspect(claim.run_id)).find(row => row.captureId === archive.captureId)?.finalAcknowledged, true);
      assert.equal(fs.existsSync(archive.bundlePath!), false);
      assert.equal(cmd(bare, ["show-ref"]), refs, "ancestry pins stay available");
      assert.equal(calls.sourceDelete, 0);
    } else await assertCustody();
  };
  // Deterministic first-proof expiry. Headroom keeps the production budget from lapsing on a slow
  // runner before the proof reaches its first budgeted boundary (the hooked withReportProofBudget).
  // At that boundary the captured PRODUCTION deadline callback (retireRecoveryReport's timer) is
  // fired by hand, so the abort is the production one and no real time passes. `observation` says
  // whether the boundary was reached and the signal aborted; the caller asserts both. `restore`
  // must run on every exit path (it also unhooks a boundary that was never reached, so the mock
  // cannot intercept a later fresh replay).
  const expireNextProof = () => {
    const restoreHeadroom = useHeadroom();
    const timer = captureTimers(ms => ms > headroom.proofMs - 5_000 && ms <= headroom.proofMs);
    const real = git.withReportProofBudget.bind(git);
    const observation = { reached: false, timersCaptured: 0, abortedBefore: undefined as boolean | undefined,
      aborted: false, reason: undefined as string | undefined };
    const mock = t.mock.method(git, "withReportProofBudget", <T>(
      budget: { signal: AbortSignal; deadline: number }, action: () => Promise<T>,
    ): Promise<T> => {
      mock.mock.restore(); // Only this proof expires; the next attempt gets a fresh budget.
      observation.reached = true;
      observation.timersCaptured = timer.count();
      observation.abortedBefore = budget.signal.aborted;
      timer.fire();
      observation.aborted = budget.signal.aborted;
      observation.reason = budget.signal.reason instanceof Error ? budget.signal.reason.message : String(budget.signal.reason);
      return real(budget, action);
    });
    const restore = () => { mock.mock.restore(); timer.restore(); restoreHeadroom(); };
    return { observation, restore };
  };
  const liveFlight = { flight, release: leave.resolve, executions: () => executions };
  const acknowledgeArchive = async (owner = r) => {
    client.releaseRecoveryCustody = async (runId, gen) => {
      assert.equal(runId, claim.run_id);
      assert.equal(gen, generation);
      calls.final++;
      options.currentArchiveReleased = true;
      return { run_id: runId, generation: gen, released: true, holds_released: 1 };
    };
    // Drive the runner's real FINAL producer and its generation cleanup, not a metadata deletion.
    await owner.resumePendingRecoveries(undefined, [archive]);
    assert.equal(calls.final, 1, "archive FINAL acknowledged once");
  };
  const recoveryOf = (owner: typeof r) => (owner as unknown as { recovery: RecoveryCoordinator }).recovery;
  const restartedRunner = async (opts: { root?: string; brokenRoot?: boolean } = {}) => {
    const root = opts.root ?? path.join(fx.dataDir, "outbox");
    if (opts.brokenRoot) fs.writeFileSync(root, "outbox init failure");
    const observations = { calls: 0 };
    const restarted = new Outbox({ root, log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await restarted.init();
    return { root, observations, outbox: restarted, r: runner({ run: async () => { throw new Error("no execution on restart"); } },
      fakeGitlab().gitlab, token, { outbox: restarted,
        // SYNTHETIC PROCESS PROOF; physical source and content inspection remain production code.
        quiesceRun: async () => {
          observations.calls++;
          return { process: { state: "quiescent", processes: [], killed: [], detail: "" },
            docker: { state: "not_wired", removed: [], detail: "" } };
        } }) };
  };
  return { policyCustody, historyEvents, predecessorH1, predecessorH2, priorFinal, predecessorClone, successorAdmission, recoveryOf, acknowledgeArchive, restartedRunner, headroomMs, headroom, useHeadroom, captureTimers, claim, generation, archive, outbox, registry, r, coordinator, replay, expireNextProof, replayPositive, positiveStage, assertFinalizePending, assertPending,
    assertCustody, ownership, capture, clone, bare, head, cmd, commit, context, calls, liveFlight, processObservation };
}
