import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { type CredentialFreeSettleOutcome, type RunContext } from "../src/executor.js";
import { CodexBoundaryError } from "../src/codex/safety.js";
import { CodexCredentialDeferredError } from "../src/codex/codex-executor.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import {
  api,
  client,
  fakeGitlab,
  git,
  gitlabClaim,
  installHarness,
  runnerWith,
  worktreeDirFor,
} from "./runner-harness.js";
import {
  commitInTree,
  spyPublishLands,
  SUBSCRIPTION,
  RELEASE_TOK,
  REFRESH_TOK,
  VAULT_BODY_SENTINEL,
  codexRig,
  FakeCodexExecutor,
  statuses,
  VAULT_FEATURE,
  VAULT_PUBLISHED,
  VAULT_LOCAL,
  VAULT_UNVERIFIED,
  VAULT_SETTLE_INCOMPLETE,
  headOf,
  trackingTip,
  feedTexts,
  parkReports,
  spyCustodySettle,
  VAULT_HELD,
  VAULT_CANCEL_REPORT_FAILED,
  VAULT_CONFIRM_UNKNOWN,
  trackedFile,
  enabledRecovery,
  workThenDefer,
  assertCaptured,
  receiptError,
  sessionKept,
  countPublishes,
  settleOnceIncomplete,
  installRecoveryRootCleanup,
} from "./runner-codex-sinks-fixture.js";

installHarness();
installRecoveryRootCleanup();

describe("RunRunner #1766 — a vault-locked Codex deferral parks the run for recovery", () => {
  it("(a) a finalize reconcile 409 vault_locked parks a TYPED recovery_wait when the api advertises it; never failed, no MR, tracking ref covers HEAD, no boundary or credential call after the deferral", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      client.protocolFeatures = [VAULT_FEATURE];
      const rig = codexRig({ vaultLocked: () => true });
      let head = "";
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "VAULT.txt", "work before the vault locked\n");
        head = headOf(ctx.worktreePath);
        return { branch: ctx.branch };
      }, rig.settle);
      const claim = gitlabClaim(1766);
      const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
      const custody = spyCustodySettle(runner);
      await runner.execute(claim);

      const parks = parkReports(claim.run_id);
      assert.equal(parks.length, 1, "exactly one recovery_wait park");
      assert.equal(parks[0]!.recovery_cause, "vault_locked", "the park is typed when the feature is advertised");
      assert.ok(!statuses(claim.run_id).includes("failed"), "a vault-locked deferral never fails the run");
      assert.ok(!statuses(claim.run_id).includes("completed"), "nothing completed");
      assert.equal(calls.length, 0, "no MR was created (phasePublish never ran)");
      assert.equal(trackingTip(1766), head, "the worker tracking ref covers the run's HEAD");
      assert.deepEqual(rig.boundaries, ["finalize"], "the capture opened no Codex boundary");
      assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (only the finalize reconcile)");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex at all");
      assert.equal(custody(), 0, "the credentialed custody settle never ran (the hold is kept)");
      assert.ok(feedTexts(claim.run_id).includes(VAULT_PUBLISHED), "the published vault park line is on the feed");
    } finally {
      restore();
    }
  });

  it("(a) without the advertised feature the park is UNTYPED recovery_wait (still never failed)", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [];
    const rig = codexRig({ vaultLocked: () => true });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "VAULT.txt", "work\n");
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1767);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    const parks = parkReports(claim.run_id);
    assert.equal(parks.length, 1);
    assert.equal(parks[0]!.recovery_cause, undefined, "an api without the feature gets the untyped park");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(calls.length, 0);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_LOCAL) || feed.includes(VAULT_PUBLISHED), `a vault park line; feed=${JSON.stringify(feed)}`);
  });

  it("a milestone-checkpoint reconcile 409 vault_locked parks instead of failing", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let head = "";
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "M1.txt", "milestone 1\n");
      head = headOf(ctx.worktreePath);
      await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1768);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "parked at recovery_wait");
    assert.equal(parkReports(claim.run_id)[0]!.recovery_cause, "vault_locked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the checkpoint deferral never fails the run");
    assert.deepEqual(rig.boundaries, ["checkpoint"], "no finalize and no capture boundary after the deferral");
    assert.equal(calls.length, 0);
    assert.equal(trackingTip(1768), head);
  });

  it("a post-approval epoch recreation CodexCredentialDeferredError reports running, then parks", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig();
    let statesAtThrow = -1;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "PLAN.txt", "approved plan work\n");
      statesAtThrow = api.states.length;
      throw new CodexCredentialDeferredError();
    }, rig.settle);
    const claim = gitlabClaim(1769);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    const after = api.states.slice(statesAtThrow).filter((s) => s.runId === claim.run_id).map((s) => s.body.status);
    assert.deepEqual(after, ["running", "recovery_wait"], "the arm confirms running before the park, nothing else");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.deepEqual(rig.boundaries, [], "no Codex boundary was opened by the park");
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 0, "no credential call from the park");
  });

  it("(d) a stale_claim ack stops silently and retains the clone when independent retirement proof blocks", async () => {
    const { gitlab, calls: mrCalls } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "STALE.txt", "work\n");
      deferred = true;
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1770);
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", {
      runStatus: "running",
      disposition: "stale_claim",
    });
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      quiesceRun: async () => ({
        process: { state: "unverified", processes: [], killed: [], detail: "independent retirement proof blocked" },
        docker: { state: "not_wired", removed: [], detail: "" },
      }),
    });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "a superseded claim is never parked");
    assert.ok(!statuses(claim.run_id).some((status) => status === "failed" || status === "completed"), "no terminal report");
    assert.equal(api.completionPermitRequests.length, 0, "no completion authority");
    assert.equal(mrCalls.length, 0, "no MR");
    assert.equal(custody(), 0, "custody held");
    assert.equal(fs.readFileSync(path.join(worktreeDirFor(1770), "STALE.txt"), "utf8"), "work\n", "blocked independent retirement proof retains the source");
  });

  it("(d) an ownership probe showing ANOTHER generation stops silently: no park, no failure", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const claim = gitlabClaim(1771, { claim_generation: 5 });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "GEN.txt", "work\n");
      deferred = true;
      api.setOwnershipStatus(claim.run_id, "running", 6);
      return { branch: ctx.branch };
    }, rig.settle);
    // The confirming running report is lost (a non-retried 400), so only the probe can decide.
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", { httpStatus: 400 });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "another generation owns the run: never parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "no failure report");
  });

  it("a lost running-transition reply: the probe reads running at THIS generation, so the run parks", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let deferred = false;
    const claim = gitlabClaim(1772, { claim_generation: 5 });
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LOST.txt", "work\n");
      deferred = true;
      api.setOwnershipStatus(claim.run_id, "running", 5);
      return { branch: ctx.branch };
    }, rig.settle);
    api.failStateWhen(claim.run_id, (b) => deferred && b.status === "running", { httpStatus: 400 });
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "the probe proved running at this generation: parked");
    assert.ok(!statuses(claim.run_id).includes("failed"));
  });

  it("(e) resume: the parked run re-claims on the same worker, reconciles ready and completes with an MR", async () => {
    const { gitlab, calls } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    let locked = true;
    const rig1 = codexRig({ vaultLocked: () => locked });
    const claim = gitlabClaim(1773);
    const exec1 = new FakeCodexExecutor(rig1.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "RESUME.txt", "work saved across the vault lock\n");
      return { branch: ctx.branch };
    }, rig1.settle);
    await runnerWith(() => ({ executor: exec1 }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1, "flight 1 parked");
    assert.equal(calls.length, 0, "flight 1 opened no MR");
    // The park carried no frozen-plan, contract or budget field: nothing the resume reads changed.
    const flight1 = api.states.filter((s) => s.runId === claim.run_id).map((s) => s.body as unknown as Record<string, unknown>);
    const parked = flight1.find((b) => b.status === "recovery_wait")!;
    assert.deepEqual(
      Object.keys(parked).filter((k) => !["status", "recovery_cause", "claim_generation", "session_id"].includes(k)),
      [],
      "the park report changes no milestone, contract or budget field",
    );

    locked = false;
    const rig2 = codexRig({ vaultLocked: () => locked });
    let resumedContent = "";
    const exec2 = new FakeCodexExecutor(rig2.safety, async (ctx) => {
      resumedContent = fs.readFileSync(path.join(ctx.worktreePath, "RESUME.txt"), "utf8");
      return { branch: ctx.branch };
    }, rig2.settle);
    await runnerWith(() => ({ executor: exec2 }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 })
      .execute({ ...claim, claim_generation: 2 });
    assert.equal(resumedContent, "work saved across the vault lock\n", "flight 2 reseeded the parked work");
    assert.equal(calls.length, 1, "flight 2 created the MR");
    assert.ok(statuses(claim.run_id).includes("completed"), "flight 2 completed");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.ok(rig2.refreshCalls() >= 1, "flight 2's finalize reconciled ready");
  });

  it("(f) a failed fetch-back and a failed park report retry visibly, then park; custody is never released", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let armed = false;
    let fetchFailures = 0;
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = (async (...args: Parameters<typeof fetch>) => {
      if (armed && fetchFailures === 0) {
        fetchFailures += 1;
        throw new Error("injected fetch-back failure");
      }
      return fetch(...args);
    }) as typeof git.fetchAgentBranch;
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "RETRY.txt", "work\n");
      armed = true;
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1774);
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", { httpStatus: 400 });
    let parkAttempts = 0;
    const original = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") parkAttempts += 1;
      return original(runId, body, signal);
    };
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(fetchFailures, 1, "the first fetch-back failed");
    assert.equal(parkAttempts, 2, "the failed park report was retried");
    assert.equal(parkReports(claim.run_id).length, 1, "then the run parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the run stayed running until the park");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_UNVERIFIED), `the unverified retry is visible; feed=${JSON.stringify(feed)}`);
    assert.ok(
      feed.some((t) => t.startsWith("The run owner's vault is locked. Could not record the pause yet")),
      `the failed park report is visible; feed=${JSON.stringify(feed)}`,
    );
    assert.equal(custody(), 0, "custody is never released");
    assert.equal(rig.refreshCalls(), 1, "no credential call while retrying");
  });

  it("issue #1789: a mid-turn CodexCredentialDeferredError after committed work, with a failed first park report, retries visibly then parks typed; custody is never released", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "MIDTURN.txt", "work done before the vault locked\n");
      throw new CodexCredentialDeferredError();
    }, rig.settle);
    const claim = gitlabClaim(1789);
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", { httpStatus: 400 });
    let parkAttempts = 0;
    const original = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") parkAttempts += 1;
      return original(runId, body, signal);
    };
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(parkAttempts, 2, "the failed park report was retried");
    const parks = parkReports(claim.run_id);
    assert.equal(parks.length, 1, "then the run parked");
    assert.equal(parks[0]!.recovery_cause, "vault_locked", "the park is typed");
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assert.equal(trackedFile(1789, "MIDTURN.txt"), "work done before the vault locked\n", "the committed work is in the tracking ref");
    const feed = feedTexts(claim.run_id);
    assert.ok(
      feed.some((t) => t.startsWith("The run owner's vault is locked. Could not record the pause yet")),
      `the failed park report is visible; feed=${JSON.stringify(feed)}`,
    );
    assert.equal(custody(), 0, "custody is never released");
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 0, "no credential call from the park");
  });

  it("(f) an incomplete settle (a surviving writer) never parks: work is retained and the settle retried", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let allow = false;
    let incomplete = 0;
    let reachedTwice!: () => void;
    const twice = new Promise<void>((r) => (reachedTwice = r));
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!allow) {
        incomplete += 1;
        if (incomplete === 2) reachedTwice();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      fs.writeFileSync(path.join(ctx.worktreePath, "UNCOMMITTED.txt"), "only copy\n");
      return { branch: ctx.branch };
    }, settle);
    const claim = gitlabClaim(1775);
    const runner = runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        twice,
        execution.then(() => { throw new Error("the flight ended before the settle retried"); }),
      ]);
      assert.equal(parkReports(claim.run_id).length, 0, "no park while the settle is incomplete");
      assert.equal(
        fs.readFileSync(path.join(worktreeDirFor(1775), "UNCOMMITTED.txt"), "utf8"),
        "only copy\n",
        "the local work is retained",
      );
      assert.ok(feedTexts(claim.run_id).includes(VAULT_SETTLE_INCOMPLETE), "the wait is visible on the feed");
      allow = true;
      await execution;
      assert.equal(parkReports(claim.run_id).length, 1, "parked once the settle completed");
      assert.ok(!statuses(claim.run_id).includes("failed"));
    } finally {
      allow = true;
      runner.shutdown();
      await execution;
    }
  });

  it("(g) no credential material leaks into state bodies, feed, logs or the boundary error", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const boundaryErrors: unknown[] = [];
    const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
    rig.safety.withBoundary = async (request, action) => {
      try {
        return await originalWithBoundary(request, action);
      } catch (e) {
        boundaryErrors.push(e);
        throw e;
      }
    };
    const { logger, lines } = recordingLogger();
    const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
      commitInTree(ctx.worktreePath, "LEAK.txt", "work\n");
      return { branch: ctx.branch };
    }, rig.settle);
    const claim = gitlabClaim(1776);
    await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 1);
    assert.equal(boundaryErrors.length, 1, "the finalize reconcile block was observed");
    const be = boundaryErrors[0] as { message: string; deferral?: string; errors: { message: string }[] };
    assert.equal(be.deferral, "vault_locked");
    const haystack = JSON.stringify({
      states: api.states.filter((s) => s.runId === claim.run_id),
      feed: api.messages(claim.run_id),
      lines,
      boundary: { message: be.message, errors: be.errors.map((e) => e.message) },
    });
    for (const secret of [
      SUBSCRIPTION.access_token,
      SUBSCRIPTION.capability,
      RELEASE_TOK,
      REFRESH_TOK,
      VAULT_BODY_SENTINEL,
    ]) {
      assert.ok(!haystack.includes(secret), `no ${secret} in any state body, feed line, log line or boundary error`);
    }
  });

  // Composition of #1764 and #1766: an ACKed owner pause (parkForPause answered `paused`, so the
  // executor returns pausedAt) must bypass the finalize boundary even with the vault locked. Were
  // it to enter the boundary, the per-sink reconcile would hit the 409 vault_locked and the
  // vault-deferral arm would park a durably paused run as recovery_wait. So: zero credential
  // calls, no permit, no recovery park, no vault feed line, and the run stays paused.
  it("(T11 x vault) an owner pause ACKed as paused makes no refreshCodex/releaseCodex call and mints no finalize permit while the vault is locked; it never becomes a vault park", async () => {
    const { gitlab, calls } = fakeGitlab();
    const restore = spyPublishLands();
    try {
      client.protocolFeatures = [VAULT_FEATURE];
      const rig = codexRig({ authMode: "subscription", vaultLocked: () => true });
      let parkedResult: boolean | undefined;
      const exec = new FakeCodexExecutor(rig.safety, async (ctx) => {
        commitInTree(ctx.worktreePath, "PAUSE.txt", "work before the owner pause\n");
        const at = { completedCount: 1, total: 2 };
        parkedResult = await ctx.parkForPause?.(at);
        return parkedResult ? { branch: ctx.branch, pausedAt: at } : { branch: ctx.branch };
      }, rig.settle);
      const claim = gitlabClaim(1764);
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
      assert.equal(parkedResult, true, "the owner pause was ACKed as paused");
      assert.deepEqual(rig.boundaries, [], `no finalize (or any) permit was minted; got ${JSON.stringify(rig.boundaries)}`);
      assert.equal(rig.refreshCalls(), 0, "no refreshCodex for an owner-paused run");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex for an owner-paused run");
      assert.equal(parkReports(claim.run_id).length, 0, "the owner pause never became a recovery_wait park");
      const feed = feedTexts(claim.run_id);
      assert.ok(!feed.some((t) => /vault/i.test(t)), `no vault park line on the feed; feed=${JSON.stringify(feed)}`);
      assert.equal(calls.length, 0, "no push/MR for a paused run");
      const st = statuses(claim.run_id);
      assert.ok(st.includes("paused"), "the run reported paused");
      assert.ok(!st.includes("completed") && !st.includes("failed"), `no terminal report; got ${JSON.stringify(st)}`);
      assert.deepEqual(rig.disposeBoundaries, ["terminal"], "the runner still disposed the Codex registry once");
    } finally {
      restore();
    }
  });
});

// ================================================================================
// Issue #1766 M3b rework: every vault-lock exit that ends the flight on a terminal status or would
// clear the preserve flags captures the run's work FIRST (credential-free), and each confirm /
// report branch keeps its posture. These are the branches a mutation used to survive.

describe("RunRunner milestone2 — pre-settle reap diagnostics", () => {
  const secret = "glpat-" + "milestone2fixture1234";
  const cases: Array<{ label: string; error: () => Error; diagnostic?: string }> = [
    {
      label: "real boundary diagnostic is redacted",
      error: () => new CodexBoundaryError("quiesce", [
        { category: "protocol", message: `unsettled provider work token=${secret}` },
      ]),
      diagnostic: "codex boundary failed at quiesce: unsettled provider work token=***REDACTED***",
    },
    ...[
      ["absent", undefined],
      ["non-string", 42],
      ["oversize", "x".repeat(501)],
      ["control character", "forged\nline"],
      ["format character", "forged\u202eline"],
    ].map(([label, diagnostic]) => ({
      label: `${label} diagnostic falls back to the error`,
      error: () => Object.assign(new Error("codex boundary failed at quiesce"), {
        name: "CodexBoundaryError",
        ...(diagnostic === undefined ? {} : { diagnostic }),
      }),
    })),
  ];

  for (const c of cases) {
    it(c.label, async () => {
      const { gitlab } = fakeGitlab();
      const rig = codexRig();
      const originalWithBoundary = rig.safety.withBoundary.bind(rig.safety);
      rig.safety.withBoundary = async (request, action) => {
        if (request.boundary === "terminal") throw c.error();
        return originalWithBoundary(request, action);
      };
      const { coord, archive } = enabledRecovery();
      const { logger, lines } = recordingLogger();
      const claim = gitlabClaim(2365, { claim_generation: 3, secrets: { forge_pat: secret } });
      const exec = new FakeCodexExecutor(rig.safety, async () => {
        throw new Error("execution failed");
      });
      await runnerWith(() => ({ executor: exec }), gitlab, undefined, logger, {
        recovery: coord,
      }).execute(claim);

      const warnings = lines.filter((line) =>
        (line as { msg?: string }).msg === "recovery: pre-settle reap failed; retaining the generation hold (reporting unaffected)",
      ) as Array<Record<string, unknown>>;
      assert.ok(warnings.length > 0, "the real pre-settle reap logged its failure");
      for (const warning of warnings) {
        assert.equal(warning.run_id, claim.run_id);
        assert.equal(warning.error, "codex boundary failed at quiesce");
        assert.equal(warning.diagnostic, c.diagnostic);
        assert.equal(Object.hasOwn(warning, "diagnostic"), c.diagnostic !== undefined);
      }
      assert.ok(!JSON.stringify(lines).includes(secret), "the run secret never reaches log records");
      assert.equal(archive.releaseCalls.length, 0, "blocked reap retains custody");
      assert.ok(statuses(claim.run_id).includes("failed"), "terminal reporting remains unaffected");
    });
  }
});

describe("RunRunner #1766 — vault-lock exits capture before cleanup, and every confirm branch holds", () => {
  it("a terminal ack on the confirming running report captures the unfetched commit and the dirty file before cleanup", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1780);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "a terminal run is never parked");
    assert.ok(!statuses(claim.run_id).includes("failed"), "nothing re-reported over the terminal status");
    assertCaptured(1780);
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral");
    assert.equal(rig.releaseCalls(), 0, "no releaseCodex");
    assert.equal(custody(), 0, "no custody settle");
  });

  it("a terminal ack whose exit capture cannot verify keeps the clone (never discards the only copy)", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    git.commitWipMarker = async () => false;
    const claim = gitlabClaim(1781);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1781), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "an unverified capture keeps the clone and its uncommitted edit",
    );
  });

  it("receipt_failed captures first and reports failed with no credentialed reap and no custody settle", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw receiptError();
      return report(runId, body, signal);
    };
    const claim = gitlabClaim(1782);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.ok(statuses(claim.run_id).includes("failed"), "a given-up receipt fails the run");
    assert.equal(parkReports(claim.run_id).length, 0, "never parked");
    assertCaptured(1782);
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (no credentialed pre-report reap)");
    assert.equal(rig.releaseCalls(), 0, "no releaseCodex after the deferral");
    assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
    assert.equal(custody(), 0, "no custody settle");
    assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
  });

  it("cancel during a vault park captures the unfetched commit and the dirty file, with no credentialed reap", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    // The run stays in the vault park loop (settle incomplete) until the cancel is seen.
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const claim = gitlabClaim(1783);
    // The first cancel report is lost in transport; the retry is visible and then lands.
    let cancelReports = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "failed" && body.failure_reason === "run cancelled" && ++cancelReports === 1) {
        throw new Error("cancel report lost");
      }
      return report(runId, body, signal);
    };
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const custody = spyCustodySettle(runner);
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel was reported",
      );
      assert.equal(parkReports(claim.run_id).length, 0, "a cancelled run is not parked");
      assert.equal(cancelReports, 2, "the lost cancel report was retried");
      assert.ok(feedTexts(claim.run_id).includes(VAULT_CANCEL_REPORT_FAILED), "the cancel-report retry is visible");
      assertCaptured(1783);
      assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral (no credentialed reap)");
      assert.equal(rig.releaseCalls(), 0, "no releaseCodex after the deferral");
      assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
      assert.equal(custody(), 0, "no custody settle");
      assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("held: a live non-running ack captures, posts an owner-neutral feed line, keeps the clone and never parks or fails", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1784);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "paused" });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "never parked over a live status");
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1784);
    assert.ok(fs.existsSync(worktreeDirFor(1784)), "the clone is kept");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(!feed.some((t) => /your vault/i.test(t)), `the feed is owner-neutral; feed=${JSON.stringify(feed)}`);
    assert.equal(rig.refreshCalls() + rig.releaseCalls(), 1, "no credential call after the deferral");
  });

  it("a probe transport failure is retried as unknown: the park waits for a later probe to confirm running", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    // The confirming running report never lands, so only the ownership probe can confirm. The first
    // probe after the deferral fails in transport; every later one reads running.
    const events: string[] = [];
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw new Error("running report lost");
      if (body.status === "recovery_wait") events.push("park");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    let probes = 0;
    client.getRunOwnership = async (runId) => {
      if (!w.deferred()) return probe(runId);
      if (++probes === 1) {
        events.push("probe-failed");
        throw new Error("ownership probe transport failure");
      }
      events.push("probe-ok");
      return probe(runId);
    };
    const claim = gitlabClaim(1785, { claim_generation: 5 });
    api.setOwnershipStatus(claim.run_id, "running", 5);
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    // unknown -> retry: a confirming probe (ok), then the loop's ownership read (ok), then the park.
    // Treating the failure as confirmed would park after ONE ok probe.
    assert.deepEqual(events, ["probe-failed", "probe-ok", "probe-ok", "park"]);
    const feed = feedTexts(claim.run_id);
    assert.equal(feed.filter((t) => t === VAULT_CONFIRM_UNKNOWN).length, 1, `the retry is visible once; feed=${JSON.stringify(feed)}`);
  });

  it("a failed ownership read in the park loop (after running is confirmed) retries visibly", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const probe = client.getRunOwnership.bind(client);
    let failures = 0;
    client.getRunOwnership = async (runId) => {
      if (w.deferred() && failures < 2) {
        failures += 1;
        throw new Error("ownership probe transport failure");
      }
      return probe(runId);
    };
    const claim = gitlabClaim(1790);
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(failures, 2);
    assert.equal(parkReports(claim.run_id).length, 1, "parked once the read recovered");
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_CONFIRM_UNKNOWN), `the loop's probe retry is visible; feed=${JSON.stringify(feed)}`);
  });

  it("a probe reading running with NO claim_generation is unknown: no park until this generation is confirmed", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1786, { claim_generation: 5 });
    api.setOwnershipStatus(claim.run_id, "running");
    let healthy = false;
    let genlessProbes = 0;
    let parksWhileUnconfirmed = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait" && !healthy) parksWhileUnconfirmed += 1;
      if (w.deferred() && body.status === "running" && !healthy) throw new Error("running report lost");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      const own = await probe(runId);
      if (w.deferred() && !healthy && ++genlessProbes === 3) {
        healthy = true;
        api.setOwnershipStatus(claim.run_id, "running", 5);
      }
      return own;
    };
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(genlessProbes, 3);
    assert.equal(parksWhileUnconfirmed, 0, "a generation-less running probe never parks");
    assert.equal(parkReports(claim.run_id).length, 1, "parked once running at this generation was confirmed");
  });

  it("a server wall park on the confirming running report retains the clone: no park, no failure", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1787);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", {
      runStatus: "paused",
      holdReason: "budget_exhausted",
      disposition: "stale_claim",
    });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "the server parked it; the worker reports nothing");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1787), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "the clone and its uncommitted edit are retained",
    );
  });

  it("a server wall park on the park report retains the clone and does not re-send the park", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1788);
    let parkAttempts = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") parkAttempts += 1;
      return report(runId, body, signal);
    };
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", {
      runStatus: "paused",
      holdReason: "budget_exhausted",
      disposition: "stale_claim",
    });
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkAttempts, 1, "the park is not retried over a server wall park");
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.ok(fs.existsSync(worktreeDirFor(1788)), "the clone is retained");
  });

  it("a 404 on the confirming probe keeps the clone and session, like the park loop's 404", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1791);
    api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { httpStatus: 400 });
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      if (w.deferred()) api.setOwnershipNotOwned(claim.run_id);
      return probe(runId);
    };
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1791), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "no verified capture exists, so the clone and its uncommitted edit are kept",
    );
  });

  it("the retry backoff doubles from recoveryRetryMs and caps at 16x", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    let healthy = false;
    let failedProbes = 0;
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running" && !healthy) throw new Error("running report lost");
      return report(runId, body, signal);
    };
    const probe = client.getRunOwnership.bind(client);
    client.getRunOwnership = async (runId) => {
      if (w.deferred() && !healthy) {
        if (++failedProbes === 7) healthy = true;
        throw new Error("ownership probe transport failure");
      }
      return probe(runId);
    };
    const claim = gitlabClaim(1789);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 });
    const waits: number[] = [];
    (runner as unknown as { waitRecoveryRetry: (f: unknown, c: boolean, ms?: number) => Promise<void> }).waitRecoveryRetry =
      async (_f, _c, ms) => {
        waits.push(ms ?? 5);
      };
    await runner.execute(claim);
    assert.deepEqual(waits.slice(0, 7), [5, 10, 20, 40, 80, 80, 80], "doubling, capped at 16x recoveryRetryMs");
    assert.equal(parkReports(claim.run_id).length, 1);
  });
});

// ================================================================================
// Issue #1766 M3b follow-up: the park LOOP's exits (after running is confirmed), a cancel whose
// capture is unverified, the loop's 404, the journaled keepCustody arm, `publish: false` on a
// terminal exit, and a live non-running park or cancel ack.

describe("RunRunner #1766 — the park loop's exits after running is confirmed", () => {
  it("a terminal ownership read in the loop (settle retried) captures the unfetched commit and the dirty file, unpublished", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1792);
    const w = workThenDefer(rig, settleOnceIncomplete(rig, () => api.setOwnershipStatus(claim.run_id, "failed")));
    const pubs = countPublishes(() => w.deferred());
    try {
      const { logger, lines } = recordingLogger();
      const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
      const custody = spyCustodySettle(runner);
      await runner.execute(claim);
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "running"),
        "running was confirmed before the loop",
      );
      assert.equal(parkReports(claim.run_id).length, 0, "a terminal run is never parked");
      assert.ok(!statuses(claim.run_id).includes("failed"), "nothing re-reported over the terminal status");
      assertCaptured(1792);
      assert.equal(pubs.count(), 0, "a run already known terminal publishes no checkpoint");
      assert.equal(sessionKept(lines), false, "a verified capture releases the session");
      assert.equal(custody(), 0, "no custody settle");
      assert.equal(rig.refreshCalls() + rig.releaseCalls(), 1, "no credential call after the deferral");
    } finally {
      pubs.restore();
    }
  });

  it("a terminal ownership read in the loop whose exit capture cannot verify keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1793);
    const w = workThenDefer(rig);
    // The in-loop capture fails (unverified, retried); on that failure the run turns terminal.
    git.commitWipMarker = async () => {
      api.setOwnershipStatus(claim.run_id, "failed");
      return false;
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(feedTexts(claim.run_id).includes(VAULT_UNVERIFIED), "the loop retried an unverified capture first");
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1793), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "an unverified exit capture keeps the clone and its uncommitted edit",
    );
    assert.ok(sessionKept(lines), "and keeps the session");
  });

  it("a paused ownership read in the loop after an in-loop capture posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1794);
    // The capture verifies, then the park report is lost and the run is paused underneath it.
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        api.setOwnershipStatus(claim.run_id, "paused");
        throw new Error("park report lost");
      }
      return report(runId, body, signal);
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0, "never parked over a live status");
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1794);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(fs.existsSync(path.join(worktreeDirFor(1794), "DIRTY.txt")), "the clone is kept after the in-loop capture");
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a 404 in the loop with no verified capture keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const claim = gitlabClaim(1795);
    const w = workThenDefer(rig, settleOnceIncomplete(rig, () => api.setOwnershipNotOwned(claim.run_id)));
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(
      fs.readFileSync(path.join(worktreeDirFor(1795), "DIRTY.txt"), "utf8"),
      "uncommitted edit\n",
      "the clone and its uncommitted edit are kept",
    );
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a 404 in the loop after a verified capture releases the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1796);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (body.status === "recovery_wait") {
        api.setOwnershipNotOwned(claim.run_id);
        throw new Error("park report lost");
      }
      return report(runId, body, signal);
    };
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.equal(parkReports(claim.run_id).length, 0);
    assertCaptured(1796);
    assert.equal(fs.existsSync(worktreeDirFor(1796)), false, "the verified capture lets the clone go");
    assert.equal(sessionKept(lines), false, "and the session");
  });

  it("a cancel whose capture is not verified (fetch-back fails) keeps the clone and session after a terminal ack", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = (async (...args: Parameters<typeof fetch>) => {
      if (w.deferred()) throw new Error("injected fetch-back failure");
      return fetch(...args);
    }) as typeof git.fetchAgentBranch;
    const claim = gitlabClaim(1797);
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assert.ok(
        api.states.some((s) => s.runId === claim.run_id && s.body.status === "failed" && s.body.failure_reason === "run cancelled"),
        "the cancel was reported",
      );
      assert.equal(trackedFile(1797, "DIRTY.txt"), null, "the capture never reached the tracking ref");
      assert.equal(
        fs.readFileSync(path.join(worktreeDirFor(1797), "DIRTY.txt"), "utf8"),
        "uncommitted edit\n",
        "the clone (the only copy) is kept",
      );
      assert.ok(sessionKept(lines), "the session is kept");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("a cancel ack with a live non-running status posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    let waiting!: () => void;
    const settleRetried = new Promise<void>((r) => (waiting = r));
    let incomplete = 0;
    let ctxRef: () => RunContext | undefined = () => undefined;
    const settle = async (ms: number): Promise<CredentialFreeSettleOutcome> => {
      if (!ctxRef()?.cancelRequested?.()) {
        if (++incomplete === 2) waiting();
        return { kind: "incomplete", errors: [{ category: "timeout", message: "a writer survived" }] };
      }
      return rig.settle(ms);
    };
    const w = workThenDefer(rig, settle);
    ctxRef = w.ctx;
    const claim = gitlabClaim(1798);
    api.failStateWhen(claim.run_id, (b) => b.status === "failed" && b.failure_reason === "run cancelled", {
      runStatus: "paused",
    });
    const { logger, lines } = recordingLogger();
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 });
    const execution = runner.execute(claim);
    try {
      await Promise.race([
        settleRetried,
        execution.then(() => { throw new Error("the flight ended before the park loop retried"); }),
      ]);
      api.setInputs(claim.run_id, [{ id: 1, kind: "cancel" }]);
      await execution;
      assertCaptured(1798);
      const feed = feedTexts(claim.run_id);
      assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
      assert.ok(fs.existsSync(path.join(worktreeDirFor(1798), "DIRTY.txt")), "the clone is kept");
      assert.ok(sessionKept(lines), "the session is kept");
    } finally {
      runner.shutdown();
      await execution;
    }
  });

  it("a park ack with a live non-running status posts the held line and keeps the clone and session", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const claim = gitlabClaim(1799);
    api.failStateWhen(claim.run_id, (b) => b.status === "recovery_wait", { runStatus: "awaiting_approval" });
    const { logger, lines } = recordingLogger();
    await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, logger, { recoveryRetryMs: 5 }).execute(claim);
    assert.ok(!statuses(claim.run_id).includes("failed"), "never failed");
    assertCaptured(1799);
    const feed = feedTexts(claim.run_id);
    assert.ok(feed.includes(VAULT_HELD), `the held line is on the feed; feed=${JSON.stringify(feed)}`);
    assert.ok(fs.existsSync(path.join(worktreeDirFor(1799), "DIRTY.txt")), "the clone is kept after the verified capture");
    assert.ok(sessionKept(lines), "the session is kept");
  });

  it("a terminal ack on the confirming running report publishes no checkpoint (publish: false)", async () => {
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const pubs = countPublishes(() => w.deferred());
    try {
      const claim = gitlabClaim(1800);
      api.failStateWhen(claim.run_id, (b) => w.deferred() && b.status === "running", { runStatus: "cancelled" });
      await runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), { recoveryRetryMs: 5 }).execute(claim);
      assertCaptured(1800);
      assert.equal(pubs.count(), 0, "a run already known terminal publishes no checkpoint");
    } finally {
      pubs.restore();
    }
  });

  it("receipt_failed over an already-resolved terminal keeps custody: no credentialed reap, no settle", async () => {
    // Reachable: a permanent message-failure trip (batcher.onPermanentFailureReport) can resolve
    // this generation's `failed` terminal while the finalize reconcile is deferring, and the
    // confirming running report then gives up its input receipt. The trip is modelled by latching
    // flight.terminalResolved at the confirm, which is exactly what journalAndSendTerminal leaves.
    const { gitlab } = fakeGitlab();
    client.protocolFeatures = [VAULT_FEATURE];
    const { coord, archive } = enabledRecovery();
    const rig = codexRig({ vaultLocked: () => true });
    const w = workThenDefer(rig);
    const report = client.reportState.bind(client);
    client.reportState = async (runId, body, signal) => {
      if (w.deferred() && body.status === "running") throw receiptError();
      return report(runId, body, signal);
    };
    const claim = gitlabClaim(1801);
    const runner = runnerWith(() => ({ executor: w.exec }), gitlab, undefined, nullLogger(), {
      recoveryRetryMs: 5,
      recovery: coord,
    });
    const r = runner as unknown as { confirmRunningForCredentialPark: (f: { terminalResolved?: boolean }, ...rest: unknown[]) => Promise<unknown> };
    const confirm = r.confirmRunningForCredentialPark.bind(runner);
    r.confirmRunningForCredentialPark = async (flight, ...rest) => {
      flight.terminalResolved = true;
      return confirm(flight, ...rest);
    };
    const custody = spyCustodySettle(runner);
    await runner.execute(claim);
    assert.ok(!statuses(claim.run_id).includes("failed"), "no second failed over the resolved terminal");
    assertCaptured(1801);
    assert.equal(custody(), 0, "no credentialed reap-then-settle");
    assert.equal(rig.refreshCalls(), 1, "no refreshCodex after the deferral");
    assert.deepEqual(rig.boundaries, ["finalize"], "no terminal boundary was opened for a reap");
    assert.equal(archive.releaseCalls.length, 0, "the custody hold is never released");
  });
});

// ================================================================================
// Issue #1900: the Codex finalize boundary runs under its own finite deadline sized for the full
// publish, and a finalize deadline failure names the finalize step that was running when it fired.
