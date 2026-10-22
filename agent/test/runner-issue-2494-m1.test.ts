import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { Outbox } from "../src/outbox.js";
import { resolvePendingTerminal, makeTerminalOutboxDeps } from "../src/terminal-resolve.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, baseUrl, client, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { HELD_LINUX_ONLY, JOIN_TOKEN, commit, deferTerminalEvidence, frozenRecords, heldServer, recoveryOf, skipCheckpoints } from "./runner-held-publication-fixture.js";

installHarness();

async function outboxAt(root: string) {
  const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 64 << 20,
    maxBytes: 128 << 20, retentionMs: 86_400_000 });
  await outbox.init();
  assert.equal(outbox.isDisabled(), false);
  return outbox;
}

for (const entry of ["boot", "live"] as const) for (const ack of ["receipt", "reason", "neither"] as const) {
  it(`M1 completion outage restart before sweep ${entry} ${ack}`, { skip: HELD_LINUX_ONLY }, async t => {
    const claim = gitlabClaim(2494, { claim_generation: 71, inventory_guarded: true });
    claim.repo.id = "00000000-0000-4000-8000-000000000072";
    const features = ["claim_generation_fence", "recovery_inventory_v1", "recovery_completed_publication_v1", "terminal_rejection_report"];
    api.setRegisterProtocolFeatures(features);
    const registration = await client.register("m1");
    api.enqueueClaim(claim);
    await client.claimRun();
    const server = heldServer(claim, features);
    let down = false, terminal = false, receiptDelivered = false, head = "", now = Date.now();
    const events: unknown[] = [];
    const log = nullLogger();
    log.warn = (message, fields) => events.push({ message, fields });
    log.info = (message, fields) => events.push({ message, fields });
    client.getRunOwnership = async () => {
      events.push({ rpc: "ownership", down, now });
      if (down) throw new Error("M1 API down");
      return { status: terminal ? "completed" : "running", claim_generation: 71, inventory_guarded: true };
    };
    for (const name of ["listRecoveryHolds", "reserveRecoveryCapture", "getRecoveryCaptureStatus", "uploadRecoveryBundle"] as const) {
      const original = client[name].bind(client) as (...args: never[]) => Promise<unknown>;
      Object.assign(client, { [name]: async (...args: never[]) => {
        events.push({ rpc: name, down, now });
        if (down) throw new Error("M1 API down");
        if (name === "listRecoveryHolds" && receiptDelivered) return { run_id: claim.run_id, holds: [] };
        return original(...args);
      } });
    }
    const receiptTemplate = JSON.parse(fs.readFileSync(new URL("../../fixtures/completed-publication/state-ack.json", import.meta.url), "utf8")).ack.completed_publication_receipt;
    const bodies: unknown[] = [];
    const originalFetch = globalThis.fetch;
    t.mock.method(globalThis, "fetch", async (input: string | URL | Request, init?: RequestInit) => {
      const url = new URL(input instanceof Request ? input.url : String(input));
      if (url.origin === new URL(baseUrl).origin && url.pathname.endsWith("/state")) {
        const body = JSON.parse(String(init?.body));
        if (body.status === "completed") {
          terminal = true; bodies.push(body);
          events.push({ rpc: "completed", down, now, body });
          if (down) throw new Error("M1 API down");
          receiptDelivered = ack === "receipt";
          return Response.json({ run: { id: claim.run_id, status: "completed", claim_generation: 71, worker_id: registration.worker_id },
            ...(ack === "receipt" ? { completed_publication_receipt: { ...receiptTemplate,
              run_id: claim.run_id, generation: 71, worker_id: registration.worker_id, repo_id: claim.repo.id,
              branch: body.branch, mr_iid: body.mr_iid, final_head: body.completion_final_head } }
              : ack === "reason" ? { completed_publication_reason: "mr_missing" } : {}) });
        }
      }
      if (terminal && url.origin === new URL(baseUrl).origin && url.pathname.endsWith("/messages")) {
        events.push({ rpc: "messages", down, now });
        return Response.json({ disposition: "run_not_running" }, { status: 409 });
      }
      return originalFetch(input, init);
    });
    const root = path.join(fx.dataDir, "outbox");
    const outbox = await outboxAt(root);
    skipCheckpoints();
    const r = runnerWith(() => ({ executor: { run: async ctx => {
      head = commit(ctx.worktreePath, "outage.txt");
      // Keep ownership running while finalization prepares; drop transport at the terminal send.
      return { branch: ctx.branch };
    } } }), fakeGitlab().gitlab, JOIN_TOKEN, log, { outbox, now: () => now });
    const fallbackCalls: Array<{ down: boolean; now: number }> = [];
    const prepareFallback = r["prepareCompletionFallback"].bind(r);
    r["prepareCompletionFallback"] = async flight => {
      fallbackCalls.push({ down, now });
      return prepareFallback(flight);
    };
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      if (args[1].status === "completed" && !terminal) down = true;
      return report(...args);
    });
    deferTerminalEvidence(r, t);
    await r.execute(claim);
    const saved = await outbox.readTerminalJournal(claim.run_id, 71);
    t.diagnostic(JSON.stringify({ stage: "outage", head, saved: saved?.body, bodies, events }));
    assert.ok(saved, "outage retains the signed terminal");
    assert.equal(saved.body.completion_final_head, head);
    assert.ok(fallbackCalls.length > 0, "foreground fallback was attempted");
    assert.ok(fallbackCalls.every(call => call.down), "prepareCompletionFallback only ran during the outage");
    // Replay the fenced feed and completion in the original process, then restart before a sweep.
    const lastMessage = api.messages(claim.run_id).at(-1)!;
    assert.ok(lastMessage);
    await outbox.appendSegment(claim.run_id, 71, [lastMessage]);
    down = false;
    const beforeRestartDrain = await outbox.drainRun(claim.run_id, async (messages, generation) => {
      await client.postMessages(claim.run_id, messages, generation);
    });
    assert.equal(beforeRestartDrain.retired, false, "fenced feed remains for original terminal replay");
    const originalDeps = makeTerminalOutboxDeps(outbox, client,
      { gapFillMax: 100, terminalMaxBytes: 1 << 20, log })!;
    await resolvePendingTerminal(r.protectRecoveryTerminalDeps(originalDeps), {
      runId: claim.run_id, claimGeneration: 71,
      send: async body => {
        const answer = await client.reportState(claim.run_id, { ...body, claim_generation: 71 });
        await r.observeSettlementTerminalAck(claim.run_id, 71, body, answer);
        return answer;
      },
    });
    const pendingBeforeRestart = !!(await outbox.readTerminalJournal(claim.run_id, 71));
    assert.equal(pendingBeforeRestart, ack !== "receipt");
    assert.equal(server.archiveUploads(), 0, "restart precedes any recovery sweep");
    const freshGit = new GitCache(fx.dataDir, log, undefined, testGitCacheOptions());
    const freshOutbox = await outboxAt(root);
    const fresh = new RunRunner(client, freshGit, () => { throw new Error("restart cannot execute"); },
      log, 20, JOIN_TOKEN, { outbox: freshOutbox, now: () => now });
    down = false;
    const drained = await freshOutbox.drainRun(claim.run_id, async (messages, generation) => {
      await client.postMessages(claim.run_id, messages, generation);
    });
    events.push({ rpc: "fenced-drain", drained });
    const boot = await fresh.snapshotBootRecoveries();
    const discovered = await freshGit.discoverOwedCandidates();
    const deps = makeTerminalOutboxDeps(freshOutbox, client, { gapFillMax: 100, terminalMaxBytes: 1 << 20, log })!;
    const replay = () => resolvePendingTerminal(fresh.protectRecoveryTerminalDeps(deps), {
      runId: claim.run_id, claimGeneration: 71,
      send: async body => {
        const answer = await client.reportState(claim.run_id, { ...body, claim_generation: 71 });
        await fresh.observeSettlementTerminalAck(claim.run_id, 71, body, answer);
        return answer;
      },
    });
    await replay();
    const replayPending = !!(await freshOutbox.readTerminalJournal(claim.run_id, 71));
    if (entry === "boot") await fresh.resumePendingRecoveries(undefined, boot);
    const passes = [], start = now;
    for (const elapsed of [0, 30_000, 60_000, 120_000, 180_000]) {
      now = start + elapsed;
      await fresh.resumeLiveRecoveries(now);
      passes.push({ now, records: await recoveryOf(fresh).inspect(claim.run_id) });
    }
    await replay();
    t.diagnostic(JSON.stringify({ entry, ack, fallbackCalls, pendingBeforeRestart, discovered, replayPending, passes,
      pending: !!(await freshOutbox.readTerminalJournal(claim.run_id, 71)), archiveUploads: server.archiveUploads(),
      finals: server.finals, events }));
    assert.deepEqual(bodies.at(-1), { ...saved.body, claim_generation: 71 }, "replay preserves exact immutable terminal body");
    if (ack === "receipt") {
      assert.equal(replayPending, false, "receipt retires before the recovery sweep");
      assert.equal(server.archiveUploads(), 0, "completed publication receipt needs no fallback archive");
    } else {
      assert.equal(replayPending, true, "without a receipt, pre-sweep replay retains guarded custody");
      assert.equal(server.archiveUploads(), 1, "existing discovery materializes the archive fallback");
      assert.equal(server.acked(), 1, "fallback archive recovers through existing inventory");
      const frozen = passes.flatMap(pass => pass.records).find(record => record.coverageDigest);
      assert.ok(frozen, "reconstruction freezes authenticated owed context");
      assert.equal(frozen.exactHoldBinding, undefined, "reconstruction gains no trusted hold binding");
      assert.equal(frozen.heldPolicy, undefined, "reconstruction stays archive-only");
      assert.equal(server.finals[0]?.disposition?.kind, "archive");
    }
  });
}

for (const cancellation of ["archive", "held", "unknown"] as const) {
  it(`M1 cancelled gated issue LIVE ${cancellation}`, { skip: HELD_LINUX_ONLY }, async t => {
    const claim = gitlabClaim(2496, { claim_generation: 71, inventory_guarded: true });
    claim.repo.id = "00000000-0000-4000-8000-000000000072";
    claim.secrets.codex = { auth_mode: "api_key", access_token: "fixture-key", capability: "fixture-capability" };
    const features = ["recovery_inventory_v1", "recovery_held_publication_v1", "recovery_held_publication_v2"];
    api.setRegisterProtocolFeatures(features);
    const registration = await client.register("m1-cancel", undefined, undefined, undefined, ["recovery_held_publication_v2"]);
    const workerId = registration.worker_id;
    assert.ok(workerId);
    const server = heldServer(claim, features);
    client.getRunOwnership = async () => ({
      status: server.failedAcked() ? "cancelled" : "running",
      claim_generation: 71, inventory_guarded: true,
    });
    const holdId = "00000000-0000-4000-8000-000000000071";
    client.listRecoveryHolds = async () => ({ run_id: claim.run_id, holds: server.holdOpen() ? [{
      hold_id: holdId, generation: 71, inventory_guarded: true, has_available_capture: server.archiveUploads() > 0,
    }] : [] });
    let exactUnknown = false;
    client.getExactRecoveryHold = async () => exactUnknown ? undefined : ({
      run_id: claim.run_id, generation: 71, hold_id: holdId, repo_id: claim.repo.id, inventory_guarded: true,
      original_worker_id: workerId, original_worker_identity: "original",
      state: "open", live_worker_id: workerId, live_run_id: claim.run_id,
      run_status: server.failedAcked() ? "cancelled" : "running", current_claim_generation: 71,
      claim_released: server.failedAcked(),
    });
    if (cancellation === "archive") server.heldAnswer = () => ({ ok: false, httpStatus: 409, reason: "ci_config_guard" });
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      const ack = await report(...args);
      // Freeze known exact custody first; lose its observation only after cancellation.
      if (args[1].status === "failed" && cancellation === "unknown") exactUnknown = true;
      return args[1].status === "failed" ? { ...ack, status: "cancelled" } : ack;
    });
    const outbox = await outboxAt(path.join(fx.dataDir, "outbox"));
    let now = Date.now(), models = 0;
    const events: unknown[] = [], log = nullLogger();
    log.warn = (message, fields) => events.push({ message, fields });
    log.info = (message, fields) => events.push({ message, fields });
    const r = runnerWith(() => ({ executor: {
      run: async ctx => { if (ctx.cancelRequested?.()) throw new Error("run cancelled"); models++; throw new Error("cancelled model must not run"); },
      killAgentTree() {},
      settleForCredentialFreeCapture: async () => ({ kind: "observed_empty" as const }),
    } }), fakeGitlab().gitlab, JOIN_TOKEN, log, { outbox, now: () => now,
      quiesceRun: async () => ({
        process: { state: "quiescent", processes: [], killed: [], detail: "M1 empty registry" },
        docker: { state: "not_wired", removed: [], detail: "M1 no Docker" },
      }),
    });
    let flight: ReturnType<typeof r["buildFlight"]>;
    const build = r["buildFlight"].bind(r);
    r["buildFlight"] = (...args) => flight = build(...args);
    const clone = r["runnerCloneForClaim"].bind(r);
    r["runnerCloneForClaim"] = async (...args) => {
      const result = await clone(...args);
      r["observeCancellation"](flight);
      return result;
    };
    deferTerminalEvidence(r, t);
    await r.execute(claim);
    assert.equal(models, 0);
    const original = await frozenRecords(claim);
    assert.ok(original[0]?.exactHoldBinding, "cancellation starts with frozen exact authority");
    assert.equal((await client.getRunOwnership(claim.run_id)).status, "cancelled");
    const passes = [], start = now;
    for (const elapsed of [0, 30_000, 60_000, 120_000, 150_000, 180_000]) {
      now = start + elapsed;
      await r.resumeLiveRecoveries(now);
      passes.push({ elapsed, executing: r.isExecuting(claim.run_id), records: await frozenRecords(claim) });
    }
    t.diagnostic(JSON.stringify({ cancellation, original, passes, uploads: server.archiveUploads(),
      held: server.heldCalls.map(c => ({ tip: c.tip, coverage: c.coverage })),
      finals: server.finals, pending: !!(await outbox.readTerminalJournal(claim.run_id, 71)), events }));
    if (cancellation === "unknown") {
      assert.equal(server.acked(), 0, "unknown exact custody cannot create a release");
      const retained = await frozenRecords(claim);
      assert.equal(retained.length, original.length);
      assert.equal(retained[0]!.captureId, original[0]!.captureId);
      assert.deepEqual(retained[0]!.exactHoldBinding, original[0]!.exactHoldBinding);
      assert.notEqual(retained[0]!.finalAcknowledged, true);
      assert.ok(await outbox.readTerminalJournal(claim.run_id, 71), "unknown custody keeps the protected terminal");
      // Unknown custody forbids release; retaining an archive is still permitted.
    } else {
      assert.equal(original.length, 1);
      assert.equal(server.acked(), 1, "LIVE settles cancellation after execution tail closes");
      assert.equal(server.heldCalls.length, 1, "live recovery does not create another publication");
      assert.equal(server.finals.length, 1, "the original FINAL settles once");
      if (original[0]!.finalRequest) assert.deepEqual(server.finals[0]!.disposition, original[0]!.finalRequest.disposition);
      if (cancellation === "archive") {
        assert.equal(original[0]!.state, "uploaded");
        assert.equal(original[0]!.selfContained, true);
        assert.equal(server.archiveUploads(), 1);
        assert.equal(server.finals[0]!.disposition?.kind, "archive");
      }
    }
  });
}

for (const overlap of ["after-exit", "during-exit", "report-before-reservation"] as const) {
  it(`M1 completed ci_fix uploaded archive live ${overlap}`, { skip: HELD_LINUX_ONLY }, async t => {
    const claim = gitlabClaim(2495, { kind: "ci_fix", claim_generation: 71, inventory_guarded: true });
    const server = heldServer(claim, ["recovery_inventory_v1"]);
    let completed = false, now = Date.now();
    client.getRunOwnership = async () => ({ status: completed ? "completed" : "running",
      claim_generation: 71, inventory_guarded: true });
    const root = path.join(fx.dataDir, "outbox");
    const outbox = await outboxAt(root);
    const log = nullLogger(), events: unknown[] = [];
    log.warn = (message, fields) => events.push({ message, fields });
    log.info = (message, fields) => events.push({ message, fields });
    skipCheckpoints();
    const r = runnerWith(() => ({ executor: { run: async ctx => {
      commit(ctx.worktreePath, "ci-fix.txt"); return { branch: ctx.branch };
    } } }), fakeGitlab().gitlab, JOIN_TOKEN, log, { outbox, now: () => now });
    const report = client.reportState.bind(client);
    t.mock.method(client, "reportState", async (...args: Parameters<typeof report>) => {
      const result = await report(...args);
      if (args[1].status === "completed") {
        completed = true;
        if (overlap === "during-exit") {
          events.push({ phase: "heartbeat-before-exit", executing: r.isExecuting(claim.run_id),
            reservation: r["inventoryReservations"].has(claim.run_id) });
          await r.resumeLiveRecoveries(now);
        }
      }
      return result;
    });
    const boundary = r["withInventorySourceBoundary"].bind(r);
    r["withInventorySourceBoundary"] = async (...args: Parameters<typeof boundary>) => {
      events.push({ phase: "source-boundary-entry", executing: r.isExecuting(claim.run_id),
        reservation: r["inventoryReservations"].has(claim.run_id) });
      return boundary(...args);
    };
    deferTerminalEvidence(r, t);
    await r.execute(claim);
    const original = await frozenRecords(claim);
    assert.equal(original[0]?.reason, "inventory_source_not_quiescent:execution_tail_present");
    if (overlap === "report-before-reservation") {
      const retirement = await outbox.readTerminalJournalForRetirement(claim.run_id, 71);
      assert.ok(retirement, "report retirement uses the authenticated opaque journal identity");
      const reserve = r["withReportSourceReservation"];
      const boundReserve = reserve.bind(r);
      let release!: () => void, arrived!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      const started = new Promise<void>(resolve => { arrived = resolve; });
      let paused = false;
      r["withReportSourceReservation"] = async (...args: Parameters<typeof boundReserve>) => {
        if (!paused) {
          paused = true;
          arrived();
          await gate;
        }
        return boundReserve(...args);
      };
      t.after(() => { release(); r["withReportSourceReservation"] = reserve; });
      const pending = r.retireRecoveryReport(claim.run_id, 71, retirement.context);
      let result: Awaited<typeof pending>;
      try {
        await Promise.race([started, pending.then(() => {
          throw new Error("report retirement refused before the pre-reservation barrier");
        })]);
        assert.equal(r.isExecuting(claim.run_id), false, "report owns C before any execution tail");
        assert.equal(r["inventoryReservations"].has(claim.run_id), false);
        const attempts = recoveryOf(r)["lastAttemptAt"];
        assert.notEqual(attempts.get(original[0]!.captureId), now);
        const finals = server.finals.length;
        // Defer discovery materialization for this pass: freezeInventory waits on C,
        // whereas the existing uploaded candidate below must exercise skip mode.
        const listHolds = client.listRecoveryHolds;
        let discoveryDeferred = false;
        client.listRecoveryHolds = async runId => {
          discoveryDeferred = true;
          return { run_id: runId, holds: [] };
        };
        try { await r.resumeLiveRecoveries(now); }
        finally { client.listRecoveryHolds = listHolds; }
        assert.equal(discoveryDeferred, true);
        assert.equal(attempts.get(original[0]!.captureId), now, "busy C rotates the original capture");
        assert.equal(server.finals.length, finals, "live skip sends no FINAL while report owns C");
        assert.equal(server.acked(), 0);
        assert.deepEqual(await outbox.readTerminalJournal(claim.run_id, 71), retirement.journal);
        events.push({ phase: "report-before-reservation", executing: r.isExecuting(claim.run_id),
          lastAttemptAt: attempts.get(original[0]!.captureId), finals });
      } finally {
        release();
        try { result = await pending; }
        finally { r["withReportSourceReservation"] = reserve; }
      }
      if (result.retired) {
        assert.ok(["existing_archive", "acknowledged_final_report_only"].includes(result.authority));
        assert.equal(await outbox.readTerminalJournal(claim.run_id, 71), undefined);
      } else {
        assert.deepEqual(await outbox.readTerminalJournal(claim.run_id, 71), retirement.journal,
          "refused report proof retains the exact terminal");
      }
      events.push({ phase: "report-released", result });
    } else {
      // An admitted maintenance reservation is also an execution tail, but the coordinator's
      // default predicate discounts it. The heartbeat passes the raw runner predicate instead.
      let release!: () => void, arrived!: () => void;
      const gate = new Promise<void>(resolve => { release = resolve; });
      const started = new Promise<void>(resolve => { arrived = resolve; });
      const reservation = r["withInventorySourceBoundary"]({ runId: claim.run_id, generation: 71,
        barePath: git.barePathFor(fx.originPath) }, async () => {
        arrived(); await gate;
      });
      try {
        await Promise.race([started, reservation.then(() => { throw new Error("maintenance source reservation refused before barrier"); })]);
        events.push({ phase: "maintenance-reservation", executing: r.isExecuting(claim.run_id),
          reservation: r["inventoryReservations"].has(claim.run_id) });
        await r.resumeLiveRecoveries(now);
        assert.equal(server.acked(), 0, "raw heartbeat predicate skips a maintenance reservation");
      } finally { release(); await reservation; }
    }
    const passes = [];
    const start = now;
    for (const elapsed of [0, 30_000, 60_000, 120_000, 150_000, 180_000]) {
      now = start + elapsed;
      await r.resumeLiveRecoveries(now);
      passes.push({ now, executing: r.isExecuting(claim.run_id), records: await frozenRecords(claim) });
      if (overlap === "report-before-reservation" && elapsed === 30_000) {
        assert.equal(server.acked(), 1, "next spaced pass ACKs the original archive after report releases C");
        assert.equal(server.archiveUploads(), 1, "next spaced pass needs no new upload");
      }
    }
    t.diagnostic(JSON.stringify({ completed, original, uploads: server.archiveUploads(), finals: server.finals,
      pending: !!(await outbox.readTerminalJournal(claim.run_id, 71)), passes, events }));
    assert.equal(completed, true);
    assert.equal(original.length, 1);
    assert.equal(original[0]!.state, "uploaded");
    assert.equal(original[0]!.selfContained, true);
    assert.deepEqual(original[0]!.prerequisiteShas, []);
    assert.equal(server.archiveUploads(), 1, "live retries reuse original archive");
    for (const pass of passes) for (const record of pass.records) {
      assert.equal(record.captureId, original[0]!.captureId);
      assert.equal(record.sourceSha, original[0]!.sourceSha);
      assert.equal(record.coverageDigest, original[0]!.coverageDigest);
    }
    assert.equal(server.acked(), 1, "heartbeat live path settles original archive after execute exits");
  });
}
