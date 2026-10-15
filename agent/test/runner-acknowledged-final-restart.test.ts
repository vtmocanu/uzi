import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { createHash } from "node:crypto";
import { GitCache, type PositiveOwedCandidateContext } from "../src/git.js";
import { WorkerClient } from "../src/client.js";
import { RecoveryCoordinator, type RecoveryRecord, type InventorySourceBoundaryContext } from "../src/recovery.js";
import { RunRunner } from "../src/runner.js";
import { PredecessorSettler, SettlementJournal, type SettlementRecord } from "../src/recovery-settlement.js";
import { Worker } from "../src/worker.js";
import type { Config } from "../src/config.js";
import type { ChatRunner } from "../src/chat-runner.js";
import type { JudgeRunner } from "../src/judge-runner.js";
import type { ReviewRunner } from "../src/review-runner.js";
import { Outbox } from "../src/outbox.js";
import { mintAttemptId } from "../src/run-quiescence.js";
import { makeTerminalOutboxDeps, resolvePendingTerminal } from "../src/terminal-resolve.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { reportRetirementFixture } from "./report-retirement-fixture.js";
import { reportProofTiming } from "../src/report-proof-timing.js";
import { api, baseUrl, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();

function command(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", timeout: 10_000 }).trim();
}
type BoundaryRunner = {
  withReportSourceReservation(runId: string, action: (eligible: () => boolean) => Promise<void>): Promise<void>;
  withInventorySourceBoundary(context: InventorySourceBoundaryContext,
    action: (prove: () => Promise<boolean>) => Promise<void>): Promise<"passed" | "retained">;
};

// Report retirement refuses both proof paths off Linux (procfs process/no-follow checks).
const linuxReportSkip = process.platform === "linux" ? false : "report retirement proofs require Linux procfs";

// Both callers get independent stores and a complete production capture/release history.
// The source boundary's execution lane is acquired by RunRunner, never planted in a map.
for (const { caller, afterUnlinkFault, liveClient } of [
  { caller: "terminal replay", afterUnlinkFault: false, liveClient: false },
  { caller: "worker finalize retry", afterUnlinkFault: false, liveClient: false },
  { caller: "terminal replay", afterUnlinkFault: true, liveClient: false },
  { caller: "worker finalize retry", afterUnlinkFault: true, liveClient: false },
  { caller: "terminal replay", afterUnlinkFault: false, liveClient: true },
  { caller: "worker finalize retry", afterUnlinkFault: false, liveClient: true },
] as const) {
  it(`acknowledged archive with redundant settled FINAL retires ${caller} after runner loss${afterUnlinkFault ? " despite post-unlink proof failure" : ""}${liveClient ? " with active pre-release observation" : ""}`, { skip: linuxReportSkip }, async t => {
    const claim = gitlabClaim(2883, { claim_generation: 29, inventory_guarded: true });
    const generation = claim.claim_generation!, token = "acknowledged-final-restart-key";
    const branch = "agent/issue-2883";
    const captureId = "00000000-0000-4000-8000-000000002883";
    const holdId = "33333333-3333-4333-8333-333333333333";
    let open = true, status = "running", workerId = "", releases = 0;
    let delayNextHold = false;
    let finishHold!: () => void;
    let delayedHold: ReturnType<WorkerClient["listRecoveryHolds"]> | undefined;
    let holdSent!: () => void;
    const holdStarted = new Promise<void>(resolve => { holdSent = resolve; });
    let settlementNow = Date.now(), allowSettlementAck = false;
    const historicalHolds = new Map<string, { generation: number; open: boolean; request?: unknown; live: boolean }>();
    const settlementRequests: unknown[] = [];
    let settlementBare = "";
    let manifest: Parameters<WorkerClient["uploadRecoveryBundle"]>[2] | undefined;
    let archiveBytes: Buffer | undefined;
    api.setRegisterProtocolFeatures(["claim_generation_fence", "recovery_inventory_v1", "terminal_rejection_report"]);
    const originalFetch = globalThis.fetch;
    const custodyReads: unknown[] = [];
    let registrationOverride: string | undefined;
    let custodyControl: "healthy" | "empty" | "incomplete" | "open" | "sibling" = "healthy";
    const holdResponse = () => Response.json({ run_id: claim.run_id, holds: [{
      hold_id: holdId, generation, inventory_guarded: true, has_available_capture: !!manifest,
    }, ...[...historicalHolds].filter(([, h]) => h.open).map(([id, h]) => ({
      hold_id: id, generation: h.generation, inventory_guarded: false, has_available_capture: false,
    }))] });
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (url.origin === new URL(baseUrl).origin) {
        if (url.pathname === "/api/worker/register") {
          const response = await originalFetch(input, init);
          const body = await response.json() as { worker_id: string };
          // A restart under the same join token registers the same worker identity.
          body.worker_id = registrationOverride ?? (workerId || body.worker_id);
          return Response.json(body);
        }
        if (url.pathname === "/api/worker/runs/claim") {
          assert.equal(status, "running", "completed runs cannot be claimed after restart");
          return Response.json({ ...claim, repo: { ...claim.repo,
            id: "44444444-4444-4444-8444-444444444444" } });
        }
        if (liveClient && url.pathname === `/api/worker/runs/${claim.run_id}/state`) {
          const body = JSON.parse(String(init?.body));
          assert.equal(body.status, "completed");
          assert.equal(body.claim_generation, generation);
          status = "completed";
          return Response.json({ run: { id: claim.run_id, status, claim_generation: generation } });
        }
        const settlementPath = url.pathname.match(/\/recovery-holds\/([^/]+)\/(settle|settle-live)$/);
        if (settlementPath) {
          const [, id, kind] = settlementPath;
          const hold = historicalHolds.get(id!);
          assert.ok(hold, "settlement must name an actual older hold");
          assert.equal(kind === "settle-live", hold.live);
          const request = JSON.parse(String(init?.body)) as {
            predecessor_generation: number; successor_generation: number;
            source_sha: string; adopted_sha: string; pushed_sha?: string; published_sha?: string; target?: string;
          };
          settlementRequests.push({ id, kind, request });
          assert.equal(request.predecessor_generation, hold.generation);
          assert.equal(request.successor_generation, generation);
          if (hold.open) {
            assert.equal(status, hold.live ? "running" : "completed");
            const target = hold.live ? `refs/uzi-checkpoints/${branch}` : `refs/heads/${branch}`;
            const published = command(fx.originPath, ["rev-parse", target]);
            assert.equal(published, hold.live ? request.published_sha : request.pushed_sha);
            assert.equal(command(settlementBare, ["merge-base", "--is-ancestor", request.source_sha, published]), "");
            assert.equal(command(settlementBare, ["merge-base", "--is-ancestor", request.adopted_sha, published]), "");
            // The fixture forge actually contains the candidates. Store the original receipt
            // identity before dropping its transport ACK, just as the API's ancestry paths do.
            hold.request = request;
            hold.open = false;
          } else assert.deepEqual(request, hold.request, "released replay must match stored ancestry identity");
          if (!allowSettlementAck) return Response.json({ error: "lost historical settlement ACK" }, { status: 503 });
          return Response.json({ run_id: claim.run_id, hold_id: id, outcome: "released" });
        }
        if (url.pathname === `/api/worker/runs/${claim.run_id}/recovery-holds`) {
          const response = open ? holdResponse() : Response.json({ run_id: claim.run_id, holds: [] });
          if (delayNextHold) {
            delayNextHold = false;
            assert.equal(open, true, "delayed response is captured at send time, before release");
            const waiting = new Promise<void>(resolve => { finishHold = resolve; });
            holdSent();
            await waiting;
          }
          return response;
        }
      }
      if (url.origin !== new URL(baseUrl).origin ||
          url.pathname !== `/api/worker/runs/${claim.run_id}/terminal-rejection-custody`) {
        return originalFetch(input, init);
      }
      assert.equal(Number(url.searchParams.get("generation")), generation);
      assert.ok(workerId);
      const response = {
        run_id: claim.run_id, worker_id: workerId, generation,
        exact_holds: [{ id: holdId, state: open ? "open" : "released" }], sibling_holds: [],
        exact_count: 1, sibling_count: 0, complete: true, exact_complete: true,
        sibling_complete: true, outcome: open ? "retained" : "settled",
      };
      const controlled = custodyControl === "empty" ? { ...response, exact_holds: [], exact_count: 0, outcome: "unknown" }
        : custodyControl === "incomplete" ? { ...response, exact_count: 257, exact_holds: Array.from({ length: 256 },
          (_, i) => ({ id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`, state: "released" })),
          complete: false, exact_complete: false, outcome: "unknown" }
        : custodyControl === "open" ? { ...response, exact_holds: [{ id: holdId, state: "open" }], outcome: "retained" }
        : custodyControl === "sibling" ? { ...response, sibling_holds: [{
          id: "55555555-5555-4555-8555-555555555555", generation: generation + 1 }], sibling_count: 1, outcome: "unknown" }
        : response;
      custodyReads.push(controlled);
      return Response.json(controlled);
    });
    const configureServer = (c: WorkerClient) => {
      c.getRunOwnership = async runId => {
        assert.equal(runId, claim.run_id);
        return { status, claim_generation: generation, inventory_guarded: true };
      };
      c.reserveRecoveryCapture = async (runId, request) => {
        assert.equal(runId, claim.run_id);
        assert.equal(request.generation, generation);
        return { capture_id: captureId, state: "preparing" };
      };
      c.uploadRecoveryBundle = async (_runId, _id, supplied, stream) => {
        const chunks: Buffer[] = [];
        for await (const chunk of stream) chunks.push(Buffer.from(chunk));
        archiveBytes = Buffer.concat(chunks);
        assert.equal(archiveBytes.length, supplied.byte_size);
        assert.equal(createHash("sha256").update(archiveBytes).digest("hex"), supplied.checksum);
        manifest = supplied;
        return { capture_id: captureId, state: "available", manifest_bound: true };
      };
      c.getRecoveryCaptureStatus = async () => ({
        capture_id: captureId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
        checksum: manifest?.checksum, byte_size: manifest?.byte_size, expires_at: "2099-01-01T00:00:00Z",
      });
      c.releaseRecoveryCustody = async (runId, gen, _evidence, disposition) => {
        assert.equal(runId, claim.run_id);
        assert.equal(gen, generation);
        assert.equal(disposition?.kind, "archive", "only the archive is ACKed");
        assert.equal(open, true);
        releases++;
        open = false;
        return { run_id: runId, generation, released: true, holds_released: 1 };
      };
    };
    configureServer(client);
    workerId = (await client.register("test")).worker_id!;
    assert.ok(workerId);
    if (liveClient) {
      assert.equal((await client.claimRun())?.run_id, claim.run_id);
      assert.equal((await client.listRecoveryHolds(claim.run_id)).holds[0]?.hold_id, holdId);
      assert.equal(client.canStartPublicationCompletion(claim.run_id, generation), true);
      delayNextHold = true;
      delayedHold = client.listRecoveryHolds(claim.run_id);
      // Always drain the request, including after a failed assertion.
      t.after(async () => { finishHold(); await delayedHold; });
      await holdStarted;
    }
    const bare = await git.ensureClone(fx.originPath);
    const attemptId = mintAttemptId(generation);
    const clone = await git.runnerCloneForBranch(bare, branch, "issue-2883", {
      isLive: () => false,
      quiescent: async () => true,
      beforeFree: async () => { throw new Error("seed cannot free a clone"); },
    }, claim.run_id, false, undefined, {
      attemptId, isLive: () => false, beforeSeed: async paths => assert.deepEqual(paths, []),
      quiescent: async () => true,
    });
    fs.writeFileSync(path.join(clone.path, "delivered.txt"), "delivered\n");
    command(clone.path, ["add", "delivered.txt"]);
    command(clone.path, ["-c", "user.name=test", "-c", "user.email=test@example.test",
      "-c", "commit.gpgsign=false", "commit", "-m", "delivered"]);
    const head = command(clone.path, ["rev-parse", "HEAD"]);
    const context: PositiveOwedCandidateContext = {
      barePath: bare, runId: claim.run_id, generation, kind: "issue", branch,
      defaultIdentity: { ref: "refs/remotes/origin/main",
        sha: command(bare, ["rev-parse", "refs/remotes/origin/main"]) },
    };
    await git.fetchAgentBranch(bare, clone.path, branch, claim.run_id, { context });
    await git.markRecoveryCapture(bare, clone.path, branch, claim.run_id, attemptId);
    const physical = runner({ run: async () => { throw new Error("no model execution"); } },
      fakeGitlab().gitlab, token);
    const boundary = physical as unknown as BoundaryRunner;
    const coordinator = new RecoveryCoordinator({
      client, git, log: nullLogger(), recoveryRoot: git.recoveryRoot, workerToken: token,
      withInventorySourceBoundary: (ctx, action) => boundary.withInventorySourceBoundary(ctx, action),
    });
    assert.ok(await coordinator.pin({ runId: claim.run_id, generation, inventoryGuarded: true,
      kind: "issue", branch, sourceSha: head }));
    const settlement = new SettlementJournal({ root: git.recoverySettlementRoot, workerToken: token,
      log: nullLogger(), now: () => settlementNow });
    const seedSettler = new PredecessorSettler({ journal: settlement, client, log: nullLogger(),
      cleanup: {
        deleteSettlementRefs: (...args) => git.deleteSettlementRefs(...args),
        deleteRecoveryPin: (...args) => git.deleteRecoveryPin(...args),
        forgetGeneration: (...args) => coordinator.forgetGeneration(...args),
      },
      liveGit: {
        isAncestor: (...args) => git.isAncestorRef(...args),
        pinPublished: (b, r, h, sha) => git.pinSettlementRefs(b, r, h, { published: sha }),
        unpinPublished: (b, r, h) => git.deleteSettlementPin(b, r, h, "published"),
      },
    });
    const adoptions: SettlementRecord[] = [];
    if (liveClient) {
      settlementBare = bare;
      for (const [id, oldGeneration, live] of [
        ["77777777-7777-4777-8777-777777777777", generation - 1, false],
        ["88888888-8888-4888-8888-888888888888", generation - 2, true],
      ] as const) {
        // Both legacy holds belonged to this worker and close by ancestry BEFORE the
        // current archive ACK; neither is an open sibling at report retirement.
        historicalHolds.set(id, { generation: oldGeneration, open: true, live });
        const source = context.defaultIdentity.sha;
        const record = await coordinator.pin({ runId: claim.run_id, generation: oldGeneration,
          sourceSha: source, kind: "issue", branch, inventoryGuarded: false });
        assert.ok(record);
        assert.equal(await git.anchorRecoveryHead(bare, claim.run_id, oldGeneration, source), true);
        assert.equal(await git.pinSettlementRefs(bare, claim.run_id, id, { source, adopted: source }), true);
        const adoption: SettlementRecord = { version: 1, runId: claim.run_id, holdId: id,
          predecessorGeneration: oldGeneration, successorGeneration: generation,
          sourceSha: source, sourceCaptureId: record.captureId, adoptedSha: source,
          seededFrom: "tracking", branch, barePath: bare, createdAt: settlementNow, state: "adopted", attempts: 0 };
        assert.equal(await seedSettler.recordAdoption(adoption), true);
        adoptions.push(adoption);
      }
    }
    const inventory = await coordinator.freezeInventory({
      context, currentSha: head, defaultBranch: "main", locallyQuiescent: true,
    });
    assert.ok(inventory);
    assert.ok(inventory.originalRoots?.some(root => root.sha === head));
    let settledId = "";
    await boundary.withReportSourceReservation(claim.run_id, async eligible => {
      assert.equal(eligible(), true);
      assert.equal(physical.isExecuting(claim.run_id), true);
      assert.equal((await coordinator.captureAndUpload({
        record: inventory, barePath: bare, defaultBranch: "main",
      })).state, "uploaded");
      const uploaded = (await coordinator.inspect(claim.run_id)).find(r => r.captureId === inventory.captureId)!;
      assert.equal(uploaded.finalRequest, undefined, "boundary refuses before archive FINAL is minted");
      assert.equal(uploaded.reason, "inventory_source_not_quiescent:execution_tail_present");
      assert.equal(releases, 0);
      assert.equal(open, true);
      // Independently authorized publication to the local fixture forge, then production ancestry reconciliation.
      command(clone.path, ["push", fx.originPath, `HEAD:refs/heads/${branch}`]);
      assert.equal(command(fx.originPath, ["rev-parse", `refs/heads/${branch}`]), head);
      await git.reconcileOwedCandidates(bare, claim.run_id, head);
      assert.deepEqual(await git.enumerateOwedCandidates(bare, claim.run_id), []);
      if (liveClient) {
        command(clone.path, ["push", fx.originPath, `HEAD:refs/uzi-checkpoints/${branch}`]);
        assert.equal(await seedSettler.observeLivePublication(claim.run_id, generation, head, "checkpoint"), 2);
        // Only the live hold's leg is sent while running; the completed hold is
        // recorded after this live observation so it does not acquire a live leg.
        // Its unsent leg is cleared by a same-generation adoption through the real API.
        assert.equal(await seedSettler.recordAdoption(adoptions[0]!), true);
        await seedSettler.settleLive(claim.run_id);
        assert.equal(historicalHolds.get(adoptions[1]!.holdId)?.open, false);
        assert.equal(await git.pinSettlementRefs(bare, claim.run_id, adoptions[0]!.holdId, { pushed: head }), true);
        assert.equal(await seedSettler.recordPushedHead(adoptions[0]!, head), true);
      }
      if (liveClient) {
        const body = { status: "completed" as const, claim_generation: generation };
        const ack = await client.reportState(claim.run_id, body);
        assert.equal(ack.applied, true);
        assert.equal(status, "completed");
        await seedSettler.observeTerminalAck(claim.run_id, generation, body, ack);
        await seedSettler.settleRun(claim.run_id);
        assert.ok([...historicalHolds.values()].every(h => !h.open));
        assert.equal((await settlement.get(claim.run_id, adoptions[0]!.holdId))?.state, "pending_settle");
        assert.equal((await settlement.get(claim.run_id, adoptions[1]!.holdId))?.live?.sent, true);
      }
      status = "completed";
      const settled = await coordinator.freezeInventory({
        context, currentSha: head, defaultBranch: "main", settledEvidence: "publication",
      });
      assert.ok(settled, "settled identity is minted under the still-open hold");
      settledId = settled.captureId;
      assert.notEqual(settledId, inventory.captureId);
      assert.deepEqual(settled.originalRoots, []);
      assert.equal(settled.coverageDigest, createHash("sha256").update("").digest("hex"));
      assert.equal(settled.finalRequest?.disposition.kind, "settled");
      await coordinator.resumePending(undefined, [settled]);
      const pending = (await coordinator.inspect(claim.run_id)).find(r => r.captureId === settledId)!;
      assert.equal(pending.finalAcknowledged, undefined);
      assert.equal(pending.reason, "inventory_source_not_quiescent:execution_tail_present");
      assert.equal(open, true);
    });
    assert.equal(physical.isExecuting(claim.run_id), false);
    await coordinator.resumePending(undefined, [inventory]);
    const records = await coordinator.inspect(claim.run_id);
    const acknowledged = records.find(r => r.captureId === inventory.captureId)!;
    const redundant = records.find(r => r.captureId === settledId)!;
    assert.ok(acknowledged, "redundant unACKed FINAL retains archive ACK");
    assert.equal(acknowledged.finalAcknowledged, true);
    assert.equal(acknowledged.finalRequest?.disposition.kind, "archive");
    assert.equal(redundant.finalAcknowledged, undefined);
    assert.equal(redundant.finalRequest?.disposition.kind, "settled");
    assert.equal(await coordinator.inventoryCleanupState(claim.run_id, generation), "pending");
    assert.equal(open, false);
    assert.equal(releases, 1);
    assert.deepEqual((await client.listRecoveryHolds(claim.run_id)).holds, []);
    assert.deepEqual(acknowledged.coverageContext, context);
    assert.equal(await git.ancestry(bare, head, acknowledged.sourceSha), "ancestor");
    assert.ok(acknowledged.bundlePath && archiveBytes);
    assert.deepEqual(fs.readFileSync(acknowledged.bundlePath), archiveBytes);
    command(bare, ["bundle", "verify", acknowledged.bundlePath]);
    const attribution = command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]);
    const ledger = command(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]);
    assert.ok(ledger.split("\n").map(line => JSON.parse(line)).some(entry =>
      entry.attemptId === attemptId && entry.state === "live" && entry.clonePath === clone.path));
    const makeOutbox = async () => {
      const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
      await outbox.init();
      assert.equal(outbox.isDisabled(), false);
      return outbox;
    };
    const outbox = await makeOutbox();
    if (caller === "terminal replay") {
      assert.equal((await outbox.journalTerminal(claim.run_id, generation, "finalize", 0,
        { status: "completed", claim_generation: generation })).journaled, true);
    } else assert.equal((await outbox.journalFinalize(claim.run_id, generation)).written, true);
    if (liveClient && caller === "terminal replay") {
      // Preserve the originally journaled finalize from the terminal reporting history.
      assert.equal((await outbox.journalFinalize(claim.run_id, generation)).written, true);
    }
    const savedClone = path.join(fx.dataDir, "saved-physical-clone");
    const runnerRoot = path.join(fx.dataDir, "runner");
    if (liveClient && caller === "terminal replay") fs.cpSync(runnerRoot, savedClone, { recursive: true });
    fs.rmSync(runnerRoot, { recursive: true });
    fs.mkdirSync(runnerRoot);
    assert.equal(fs.existsSync(path.dirname(clone.path)), false);
    assert.equal(command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]), attribution);
    assert.equal(command(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]), ledger);
    const freshGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
    const freshClient = liveClient ? client : new WorkerClient(baseUrl, "tkn-runner-123456", "test", nullLogger());
    configureServer(freshClient);
    if (!liveClient) workerId = (await freshClient.register("test")).worker_id!;
    assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true);
    assert.deepEqual((await freshClient.listRecoveryHolds(claim.run_id)).holds, []);
    const key = `${claim.run_id}:${generation}`;
    const reservations = Reflect.get(freshClient, "completionReservations") as Map<string, { live: boolean }>;
    const tickets = Reflect.get(freshClient, "completionTickets") as Set<{ exclusions: Set<string> }>;
    assert.equal(tickets.size, liveClient ? 1 : 0);
    assert.equal(reservations.get(key)?.live, true);
    const completionSnapshot = () => ({
      claims: [...(Reflect.get(freshClient, "completionClaims") as Map<string, unknown>)],
      holds: [...(Reflect.get(freshClient, "completionHolds") as Map<string, unknown>)],
      reservations: [...reservations].map(([id, value]) => [id, value.live]),
      tickets: [...tickets].map(ticket => [...ticket.exclusions]),
    });
    const bindingsBefore = completionSnapshot();
    if (liveClient) {
      assert.equal((Reflect.get(freshClient, "completionClaims") as Map<string, { eligible: boolean }>).get(key)?.eligible, true);
      assert.equal((Reflect.get(freshClient, "completionHolds") as Map<string, unknown>).get(key), holdId);
      assert.deepEqual(bindingsBefore.tickets, [[]]);
    }
    const releaseReceipt = t.mock.method(freshClient, "releasePublicationCompletion");
    const releaseRetired = t.mock.method(freshClient, "releaseRetiredPublicationCompletion");
    const freshOutbox = await makeOutbox();
    let retryNow = Date.now();
    const freshRunner = new RunRunner(freshClient, freshGit, () => {
      throw new Error("restart must not execute a run");
    }, nullLogger(), 20, token, { outbox: freshOutbox, now: () => retryNow, settlement });
    const notification = t.mock.method(freshRunner, "notifyPublicationCompletionRetired");
    const outcomes: Awaited<ReturnType<RunRunner["retireRecoveryReport"]>>[] = [];
    const retire = freshRunner.retireRecoveryReport.bind(freshRunner);
    t.mock.method(freshRunner, "retireRecoveryReport", async (...args: Parameters<typeof retire>) => {
      const result = await retire(...args);
      outcomes.push(result);
      return result;
    });
    const injectAfterUnlinkFault = () => {
      if (!afterUnlinkFault) return;
      const recovery = Reflect.get(freshRunner, "recovery") as RecoveryCoordinator;
      const real = recovery.retireAcknowledgedFinalReport.bind(recovery);
      t.mock.method(recovery, "retireAcknowledgedFinalReport", async (...args: Parameters<typeof real>) => {
        await real(...args);
        const pending = caller === "terminal replay"
          ? await freshOutbox.readTerminalJournal(claim.run_id, generation)
          : freshOutbox.finalizeRecordIdentity(claim.run_id, generation);
        assert.equal(pending, undefined, "real proof must actually unlink before the injected settlement failure");
        throw new Error("post-unlink proof settlement failed");
      });
    };
    const freshCoordinator = new RecoveryCoordinator({ client: freshClient, git: freshGit,
      log: nullLogger(), recoveryRoot: freshGit.recoveryRoot, workerToken: token });
    assert.deepEqual(await freshCoordinator.inspect(claim.run_id), records, "MAC journals authenticate after restart");
    assert.equal(await freshCoordinator.inventoryCleanupState(claim.run_id, generation), "pending");
    assert.equal(await freshClient.hasRecoveryRetirementAuthority(claim.run_id, generation, "pending"), false,
      "released custody is not generic pending-discard retirement authority");
    const sourceRead = await freshGit.withReportProofBudget({
      signal: new AbortController().signal, deadline: Date.now() + 10_000,
    }, () => freshGit.withBareLock(bare,
      () => freshGit.readInventoryCloneHeadsUnderLock(bare, claim.run_id)));
    assert.equal(sourceRead.kind, "unknown", "physical report proof refuses missing clone parents");
    t.diagnostic(JSON.stringify({ caller, status, generation, releases, open,
      cleanup: await freshCoordinator.inventoryCleanupState(claim.run_id, generation),
      archiveACK: acknowledged.finalAcknowledged, settledACK: redundant.finalAcknowledged === true,
      reason: redundant.reason, cloneMissing: !fs.existsSync(clone.path), sourceRead }));
    // New-API controls are isolated from the first two BASE-compatible primary cases.
    if (afterUnlinkFault || liveClient) {
    const acknowledgedSources = await freshGit.withReportProofBudget({ signal: new AbortController().signal,
      deadline: Date.now() + 10_000 }, () => freshGit.withBareLock(bare,
        () => freshGit.readAcknowledgedReportSourcesUnderLock(bare, claim.run_id)));
    assert.equal(acknowledgedSources.kind, "verified");
    if (acknowledgedSources.kind === "verified") assert.ok(acknowledgedSources.snapshot.includes('"missing"'));
    }
    if (caller === "terminal replay") {
      if (!afterUnlinkFault && !liveClient) {
        const report = (await freshOutbox.readTerminalJournalForRetirement(claim.run_id, generation))!;
        const configFile = path.join(bare, "config");
        const configBytes = fs.readFileSync(configFile);
        const ledgerKey = `uzi-attempts.${branch}.entry`;
        const replacementPath = path.join(path.dirname(clone.path), `metadata-control.attempt-${attemptId}`);
        const replacementEntries = ledger.split("\n").map(raw => {
          const entry = JSON.parse(raw);
          return entry.attemptId === attemptId ? { ...entry, clonePath: replacementPath } : entry;
        });
        assert.notEqual(replacementPath, clone.path);
        assert.equal(fs.existsSync(replacementPath), false);
        assert.ok(replacementEntries.some(entry => entry.attemptId === attemptId));
        try {
          command(bare, ["config", "--unset-all", ledgerKey]);
          for (const entry of replacementEntries) command(bare, ["config", "--add", ledgerKey, JSON.stringify(entry)]);
          assert.deepEqual(command(bare, ["config", "--get-all", ledgerKey]).split("\n").map(raw => JSON.parse(raw)),
            replacementEntries, "replace every transition without appending conflicting producer history");
          assert.equal(command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]), attribution,
            "the valid journal still names the original producer path");
          assert.deepEqual(await freshRunner.retireRecoveryReport(claim.run_id, generation, report.context),
            { retired: false }, "same attempt at a different ledger clone path must retain the report");
          assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation));
          assert.deepEqual(completionSnapshot(), bindingsBefore);
          assert.deepEqual(await freshCoordinator.inspect(claim.run_id), records);
          assert.equal(releases, 1);
        } finally {
          fs.writeFileSync(configFile, configBytes);
        }
        const originalCustody = freshClient.getTerminalRejectionCustody;
        let custodyObserved = false;
        freshClient.getTerminalRejectionCustody = async (...args: Parameters<typeof originalCustody>) => {
          const response = await originalCustody.apply(freshClient, args);
          custodyObserved = true;
          fs.mkdirSync(clone.path, { recursive: true });
          return response;
        };
        try {
          assert.deepEqual(await freshRunner.retireRecoveryReport(claim.run_id, generation, report.context),
            { retired: false }, "clone appearing after fresh custody must retain the report");
          assert.equal(custodyObserved, true, "control delegates through the actual custody response");
          assert.equal(fs.existsSync(path.join(clone.path, ".git")), false);
          assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation));
          assert.deepEqual(completionSnapshot(), bindingsBefore);
          assert.equal(releases, 1);
        } finally {
          freshClient.getTerminalRejectionCustody = originalCustody;
          fs.rmSync(path.dirname(clone.path), { recursive: true });
        }
      }
      if (afterUnlinkFault) {
      const report = (await freshOutbox.readTerminalJournalForRetirement(claim.run_id, generation))!;
      const refuse = async (context = report.context) => {
        assert.deepEqual(await freshRunner.retireRecoveryReport(claim.run_id, generation, context), { retired: false });
        assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation));
        assert.deepEqual(completionSnapshot(), bindingsBefore);
        assert.equal(releases, 1);
      };
      // Nested node:test hooks would replace the shared harness while this history is live.
      // Run independent controls sequentially and restore their public transport seams each time.
      const originalOwnership = freshClient.getRunOwnership;
      const originalCaptureStatus = freshClient.getRecoveryCaptureStatus;
      const originalAncestry = freshGit.ancestry;
      const originalSourceReader = freshGit.readAcknowledgedReportSourcesUnderLock;
      const originalCustody = freshClient.getTerminalRejectionCustody;
      const originalUnlink = freshOutbox.retireTerminalIfEligible;
      const control = async (label: string, action: () => Promise<void>) => {
        try { await action(); t.diagnostic("refusal passed: " + label); } finally {
          freshClient.getRunOwnership = originalOwnership;
          freshClient.getRecoveryCaptureStatus = originalCaptureStatus;
          freshGit.ancestry = originalAncestry;
          freshGit.readAcknowledgedReportSourcesUnderLock = originalSourceReader;
          freshClient.getTerminalRejectionCustody = originalCustody;
          freshOutbox.retireTerminalIfEligible = originalUnlink;
        }
      };
      const journalDir = path.join(freshGit.recoveryRoot, claim.run_id);
      for (const [label, id] of [["missing archive ACK", acknowledged.captureId],
          ["zero redundant FINAL", redundant.captureId]] as const) {
        await control(label, async () => {
          const file = path.join(journalDir, id + ".json"), bytes = fs.readFileSync(file);
          fs.unlinkSync(file);
          try { await refuse(); } finally { fs.writeFileSync(file, bytes, { mode: 0o600 }); }
        });
      }
      for (const state of ["running", "failed", "cancelled"] as const) await control("ownership " + state, async () => {
        status = state;
        try { await refuse(); } finally { status = "completed"; }
      });
      await control("incorrect ownership generation", async () => {
        t.mock.method(freshClient, "getRunOwnership", async () => ({
          status: "completed", claim_generation: generation + 1, inventory_guarded: true,
        }));
        await refuse();
      });
      await control("unguarded ownership", async () => {
        t.mock.method(freshClient, "getRunOwnership", async () => ({
          status: "completed", claim_generation: generation, inventory_guarded: false,
        }));
        await refuse();
      });
      for (const mode of ["expired", "unbound", "checksum", "unavailable", "error"] as const) {
        await control("archive " + mode, async () => {
          const healthy = await freshClient.getRecoveryCaptureStatus(claim.run_id, captureId);
          t.mock.method(freshClient, "getRecoveryCaptureStatus", async () => {
            if (mode === "error") throw new Error("capture observation failed");
            return { ...healthy,
              ...(mode === "expired" ? { expires_at: "2000-01-01T00:00:00Z" } : {}),
              ...(mode === "unbound" ? { manifest_bound: false } : {}),
              ...(mode === "checksum" ? { checksum: "0".repeat(64) } : {}),
              ...(mode === "unavailable" ? { state: "preparing" } : {}),
            };
          });
          await refuse();
        });
      }
      for (const mode of ["empty", "incomplete", "open", "sibling"] as const) {
        await control("custody " + mode, async () => {
          custodyControl = mode;
          try { await refuse(); } finally { custodyControl = "healthy"; }
        });
      }
      await control("unsupported custody feature", async () => {
        api.setRegisterProtocolFeatures(["recovery_inventory_v1"]);
        await freshClient.register("test");
        assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true);
        try { await refuse(); } finally {
          api.setRegisterProtocolFeatures(["recovery_inventory_v1", "terminal_rejection_report"]);
          workerId = (await freshClient.register("test")).worker_id!;
          assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true);
        }
      });
      for (const relation of ["divergent", "unknown"] as const) {
        await control("coverage " + relation, async () => {
          t.mock.method(freshGit, "ancestry", async () => relation);
          await refuse();
        });
      }
      const configFile = path.join(bare, "config");
      for (const mode of ["journal", "ledger", "protected", "marker", "value cap", "entry cap", "raw cap", "unsafe path"] as const) {
        await control("metadata " + mode, async () => {
          const bytes = fs.readFileSync(configFile);
          try {
            if (mode === "journal") command(bare, ["config", "--add", `uzi-recovery.${branch}.clone`, "{}"]);
            if (mode === "ledger") command(bare, ["config", "--add", `uzi-attempts.${branch}.entry`, "{}"]);
            if (mode === "protected") command(bare, ["config", "--add", `uzi-retained.${claim.run_id}.journal`, "{}"]);
            if (mode === "marker") command(bare, ["config", "--add", `uzi-stopped.${claim.run_id}.entry`, "{}"]);
            if (mode === "value cap") fs.appendFileSync(configFile, "\n[probe]\nvalue=" + "x".repeat(65537) + "\n");
            if (mode === "entry cap") fs.appendFileSync(configFile, "\n[probe]\n" + "value=x\n".repeat(1025));
            if (mode === "raw cap") fs.appendFileSync(configFile, "\n#" + "x".repeat(1048576));
            if (mode === "unsafe path") {
              const entry = JSON.parse(ledger.split("\n")[0]!);
              command(bare, ["config", "--add", `uzi-attempts.${branch}.entry`,
                JSON.stringify({ ...entry, attemptId: mintAttemptId(generation), clonePath: path.join(fx.dataDir, "elsewhere") })]);
            }
            await refuse();
          } finally { fs.writeFileSync(configFile, bytes); }
        });
      }
      await control("metadata changes during reread", async () => {
        const bytes = fs.readFileSync(configFile);
        let reads = 0;
        t.mock.method(freshGit, "readAcknowledgedReportSourcesUnderLock", async (...args: Parameters<typeof originalSourceReader>) => {
          if (++reads === 2) fs.appendFileSync(configFile, "\n[probe]\nvalue=changed\n");
          return originalSourceReader.apply(freshGit, args);
        });
        try { await refuse(); } finally { fs.writeFileSync(configFile, bytes); }
        assert.ok(reads >= 2);
      });
      await control("present clone with missing git directory", async () => {
        fs.mkdirSync(clone.path, { recursive: true });
        try { await refuse(); } finally { fs.rmSync(path.dirname(clone.path), { recursive: true }); }
      });
      await control("changed registered worker during custody observation", async () => {
        const priorWorker = workerId;
        t.mock.method(freshClient, "getTerminalRejectionCustody", async (...args: Parameters<typeof originalCustody>) => {
          const response = await originalCustody.apply(freshClient, args);
          registrationOverride = "66666666-6666-4666-8666-666666666666";
          await freshClient.register("test");
          return response;
        });
        try {
          assert.deepEqual(await freshRunner.retireRecoveryReport(claim.run_id, generation, report.context), { retired: false });
          assert.ok(await freshOutbox.readTerminalJournal(claim.run_id, generation));
        } finally {
          registrationOverride = priorWorker;
          workerId = (await freshClient.register("test")).worker_id!;
          registrationOverride = undefined;
          assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true);
        }
      });
      await control("live source reservation", async () => {
        await (freshRunner as unknown as BoundaryRunner).withReportSourceReservation(claim.run_id, async () => {
          await refuse();
        });
      });
      await control("held terminal resolve", async () => {
        freshOutbox.holdTerminalResolve(claim.run_id, generation);
        try { await refuse(); } finally { freshOutbox.releaseTerminalResolve(claim.run_id, generation); }
      });
      await control("caller cancellation", async () => {
        const abort = new AbortController(); abort.abort();
        await refuse({ ...report.context, signal: abort.signal });
      });
      await control("caller deadline", async () => { await refuse({ ...report.context, deadline: Date.now() - 1 }); });
      await control("stale report identity", async () => { await refuse({ ...report.context, expectedIdentity: {} }); });
      await control("unlink refusal", async () => {
        t.mock.method(freshOutbox, "retireTerminalIfEligible", async () => false);
        await refuse();
      });
      assert.deepEqual(await freshCoordinator.inspect(claim.run_id), records);
      }
      const deps = makeTerminalOutboxDeps(freshOutbox, freshClient, {
        gapFillMax: 100, terminalMaxBytes: 64 * 1024, log: nullLogger(),
      });
      assert.ok(deps);
      let sends = 0;
      injectAfterUnlinkFault();
      await resolvePendingTerminal(freshRunner.protectRecoveryTerminalDeps(deps), {
        runId: claim.run_id, claimGeneration: generation,
        send: async body => { assert.equal(body.status, "completed"); sends++; return { applied: false, status: "completed" }; },
      });
      assert.equal(sends, 1);
      assert.ok(custodyReads.length > 0);
      assert.equal(await freshOutbox.readTerminalJournal(claim.run_id, generation), undefined,
        "acknowledged archive retires the terminal report despite redundant FINAL and lost clone");
    } else {
      injectAfterUnlinkFault();
      const worker = new Worker({} as Config, freshClient, freshRunner,
        {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
        () => ({ ok: true, missing: [] }), freshOutbox, new Map());
      assert.equal(await freshOutbox.readTerminalJournal(claim.run_id, generation), undefined,
        "finalize discovery runs without a companion terminal");
      await (worker as unknown as { sweepPendingFinalizes(): Promise<void> }).sweepPendingFinalizes();
      assert.deepEqual(freshOutbox.listPendingFinalizeGenerations(), [],
        "acknowledged archive retires worker finalize retry despite redundant FINAL and lost clone");
    }
    assert.deepEqual(outcomes.at(-1), { retired: true, authority: "acknowledged_final_report_only" });
    assert.equal(notification.mock.callCount(), 0);
    assert.equal(releaseReceipt.mock.callCount(), 0);
    assert.equal(releaseRetired.mock.callCount(), 0);
    assert.equal(reservations.get(key)?.live, true);
    assert.equal(tickets.size, liveClient ? 1 : 0);
    assert.deepEqual(completionSnapshot(), bindingsBefore);
    assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true);
    assert.equal(releases, 1, "report-only retirement sends no new FINAL");
    assert.deepEqual(await freshCoordinator.inspect(claim.run_id), records, "report retirement preserves recovery custody");
    if (liveClient) {
      finishHold();
      assert.equal((await delayedHold!).holds[0]?.hold_id, holdId,
        "the actual pre-release hold observation completes after report-only unlink");
      assert.equal(tickets.size, 0);
      assert.deepEqual(completionSnapshot(), { ...bindingsBefore, tickets: [] });
      assert.equal(freshClient.canStartPublicationCompletion(claim.run_id, generation), true,
        "the outstanding observation leaves the original binding usable");
    }
    if (!afterUnlinkFault) {
      // Instrument real per-record steps; no authority or cleanup outcome is replaced.
      type Steps = { resumeInventory(record: RecoveryRecord): Promise<unknown> };
      const instrument = (r: RunRunner) => {
        const recovery = Reflect.get(r, "recovery") as RecoveryCoordinator;
        const steps = t.mock.method(recovery as unknown as Steps, "resumeInventory");
        return { recovery, steps };
      };
      const refsBefore = command(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi"]);
      const assertStillRetained = async (recovery: RecoveryCoordinator) => {
        const current = await recovery.inspect(claim.run_id);
        assert.deepEqual(current.map(r => r.captureId).sort(), records.map(r => r.captureId).sort());
        for (const saved of records) {
          const r = current.find(r => r.captureId === saved.captureId)!;
          assert.deepEqual(r.finalRequest, saved.finalRequest);
          assert.equal(r.finalAcknowledged, saved.finalAcknowledged);
        }
        assert.equal(await recovery.inventoryCleanupState(claim.run_id, generation), "pending");
        assert.equal(command(bare, ["for-each-ref", "--format=%(refname) %(objectname)", "refs/uzi"]), refsBefore);
        assert.equal(command(bare, ["config", "--get", `uzi-recovery.${branch}.clone`]), attribution);
        assert.equal(command(bare, ["config", "--get-all", `uzi-attempts.${branch}.entry`]), ledger);
        assert.equal(releases, 1);
      };
      const live = instrument(freshRunner);
      await freshRunner.resumeLiveRecoveries(retryNow);
      assert.ok(live.steps.mock.calls.some(c => c.arguments[0].captureId === settledId),
        "real live sweep retries the unresolved authenticated FINAL");
      await assertStillRetained(live.recovery);
      const attempted = live.steps.mock.callCount();
      await freshRunner.resumeLiveRecoveries(retryNow);
      assert.equal(live.steps.mock.callCount(), attempted, "not-yet-due heartbeat does no retry");
      retryNow += 300_000;
      await freshRunner.resumeLiveRecoveries(retryNow);
      assert.ok(live.steps.mock.callCount() > attempted, "due heartbeat re-enters the real retry step");
      await assertStillRetained(live.recovery);

      const bootGit = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
      const bootClient = new WorkerClient(baseUrl, "tkn-runner-123456", "test", nullLogger());
      configureServer(bootClient);
      const bootOutbox = await makeOutbox();
      const bootSettlement = new SettlementJournal({ root: bootGit.recoverySettlementRoot, workerToken: token,
        log: nullLogger(), now: () => settlementNow });
      const bootRunner = new RunRunner(bootClient, bootGit, () => {
        throw new Error("boot resume must not execute a run");
      }, nullLogger(), 20, token, { outbox: bootOutbox, now: () => retryNow, settlement: bootSettlement });
      const boot = instrument(bootRunner);
      const snapshot = await bootRunner.snapshotBootRecoveries();
      assert.ok(snapshot.some(r => r.captureId === settledId), "fresh boot selects the unresolved FINAL");
      workerId = (await bootClient.register("test")).worker_id!;
      assert.equal(bootClient.canStartPublicationCompletion(claim.run_id, generation), true);
      assert.deepEqual((await bootClient.listRecoveryHolds(claim.run_id)).holds, []);
      await bootRunner.resumePendingRecoveries(undefined, snapshot);
      assert.ok(boot.steps.mock.calls.some(c => c.arguments[0].captureId === settledId),
        "fresh boot actually retries the same unresolved obligation");
      await assertStillRetained(boot.recovery);
      assert.equal(notification.mock.callCount(), 0);
      assert.equal(releaseReceipt.mock.callCount(), 0);
      assert.equal(releaseRetired.mock.callCount(), 0);
      t.diagnostic("downstream live/due/fresh-boot retries retained the authenticated FINAL and custody");
      if (liveClient) {
        const before = await settlement.listRun(claim.run_id);
        assert.equal(before.length, 2);
        const due = () => Math.max(...before.flatMap(r => [r.nextAttemptAt ?? 0, r.live?.nextAttemptAt ?? 0]));
        settlementNow = due();
        let requests = settlementRequests.length;
        await freshRunner.settlePendingPredecessors();
        assert.equal(settlementRequests.length, requests + 2, "completed and sent-live legs retry through real runner sweep");
        const retried = await settlement.listRun(claim.run_id);
        assert.equal(retried.find(r => r.holdId === adoptions[0]!.holdId)?.attempts, before[0]!.holdId === adoptions[0]!.holdId
          ? before[0]!.attempts + 1 : before[1]!.attempts + 1);
        const nextLiveAttempt = retried.find(r => r.holdId === adoptions[1]!.holdId)?.live?.nextAttemptAt;
        assert.ok(nextLiveAttempt !== undefined && nextLiveAttempt > settlementNow);
        requests = settlementRequests.length;
        await freshRunner.settlePendingPredecessors();
        assert.equal(settlementRequests.length, requests, "settlement backoff suppresses premature sends");
        await assertStillRetained(live.recovery);
        settlementNow = Math.max(...retried.flatMap(r => [r.nextAttemptAt ?? 0, r.live?.nextAttemptAt ?? 0]));
        await bootRunner.settlePendingPredecessors();
        assert.equal(settlementRequests.length, requests + 2, "fresh boot reconstructs both MAC settlement legs and resends their original identities");
        await assertStillRetained(boot.recovery);
        assert.equal(notification.mock.callCount(), 0);
        assert.equal(releaseReceipt.mock.callCount(), 0);
        assert.equal(releaseRetired.mock.callCount(), 0);

        // Deliver the genuine historical ancestry receipts. This independently cleans
        // only the older generations, leaving the current archive/redundant FINAL intact.
        const awaiting = await bootSettlement.listRun(claim.run_id);
        settlementNow = Math.max(...awaiting.flatMap(r => [r.nextAttemptAt ?? 0, r.live?.nextAttemptAt ?? 0]));
        allowSettlementAck = true;
        await freshRunner.settlePendingPredecessors();
        assert.deepEqual(await settlement.listRun(claim.run_id), []);
        const current = await live.recovery.inspect(claim.run_id);
        assert.ok(current.some(r => r.captureId === settledId && !r.finalAcknowledged));
        for (const adoption of adoptions) {
          assert.equal(current.some(r => r.captureId === adoption.sourceCaptureId), false);
          assert.equal(command(bare, ["for-each-ref", "--format=%(refname)", `refs/uzi-settle/${claim.run_id}/${adoption.holdId}`]), "");
        }
        assert.equal(reservations.get(key)?.live, true);
        assert.equal(notification.mock.callCount(), 0);
        assert.equal(releaseReceipt.mock.callCount(), 0);
        assert.equal(releaseRetired.mock.callCount(), 0);
        t.diagnostic("completed/live settlement replay and settled local obligation cleanup passed");
      }
      if (liveClient && caller === "terminal replay") {
        // Restore the genuinely covered original tree and negotiate a peer without the
        // new custody feature: only the pre-existing physical archive proof can retire
        // the companion saved at the start of this reporting history.
        fs.cpSync(savedClone, runnerRoot, { recursive: true });
        api.setRegisterProtocolFeatures(["recovery_inventory_v1"]);
        workerId = (await freshClient.register("test")).worker_id!;
        // The positive physical proof walks the restored clone repeatedly. Lend only
        // this later control bounded headroom through reportProofTiming; earlier
        // report-only and refusal checks keep the production budgets.
        t.mock.method(reportProofTiming, "proofBudgetMs", () => 10_000);
        t.mock.method(reportProofTiming, "finalizeAuthorityMs", () => 10_000);
        t.mock.method(reportProofTiming, "finalizePassMs", () => 10_000);
        assert.ok(freshOutbox.finalizeRecordIdentity(claim.run_id, generation));
        const worker = new Worker({} as Config, freshClient, freshRunner,
          {} as ChatRunner, {} as JudgeRunner, {} as ReviewRunner, nullLogger(),
          () => ({ ok: true, missing: [] }), freshOutbox, new Map());
        await (worker as unknown as { sweepPendingFinalizes(): Promise<void> }).sweepPendingFinalizes();
        assert.deepEqual(freshOutbox.listPendingFinalizeGenerations(), []);
        assert.deepEqual(outcomes.at(-1), { retired: true, authority: "existing_archive" });
        assert.equal(notification.mock.callCount(), 1, "later physical authority owns the notification");
        assert.equal(releaseReceipt.mock.callCount(), 0);
        assert.equal(releaseRetired.mock.callCount(), 1);
        assert.equal(reservations.has(key), false, "independent retirement detaches the actual preserved reservation");
        assert.equal((Reflect.get(freshClient, "completionClaims") as Map<string, unknown>).has(key), false);
        assert.equal((Reflect.get(freshClient, "completionHolds") as Map<string, unknown>).has(key), false);
        t.diagnostic("completed/live settlement replay and later independent physical detachment passed");
      }
    }
  });
}

it("new report proof transport failure preserves existing physical archive authority within the same budget", { skip: linuxReportSkip }, async t => {
  const f = await reportRetirementFixture(t);
  const recovery = Reflect.get(f.r, "recovery") as RecoveryCoordinator;
  const report = (await f.outbox.readTerminalJournalForRetirement(f.claim.run_id, f.generation))!;
  let firstBudget: { signal: AbortSignal; deadline: number } | undefined;
  t.mock.method(recovery, "retireAcknowledgedFinalReport", async (...[_run, _generation, budget]:
    Parameters<RecoveryCoordinator["retireAcknowledgedFinalReport"]>) => {
    firstBudget = budget;
    throw new Error("custody transport unavailable before unlink");
  });
  const oldProof = recovery.retireArchivedReport.bind(recovery);
  const old = t.mock.method(recovery, "retireArchivedReport", async (...args: Parameters<typeof oldProof>) => {
    assert.equal(args[2], firstBudget, "fallback shares the original signal and deadline");
    assert.ok(args[2].deadline <= Date.now() + 1000, "fallback gets no extended budget");
    return oldProof(...args);
  });
  const sources = t.mock.method(git, "readInventoryCloneHeads");
  assert.deepEqual(await f.r.retireRecoveryReport(f.claim.run_id, f.generation, report.context),
    { retired: true, authority: "existing_archive" });
  assert.equal(old.mock.callCount(), 1);
  assert.ok(sources.mock.callCount() >= 3, "real existing archive proof inspects and rereads physical sources");
  assert.ok(f.processObservation.calls >= 4, "existing proof observes the actual clone before and after content inspection");
  assert.equal(await f.outbox.readTerminalJournal(f.claim.run_id, f.generation), undefined);
  await f.assertCustody();
});
