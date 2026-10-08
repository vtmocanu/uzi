import { it } from "node:test";
import { RequestError } from "../src/client.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { LimitReachedError } from "../src/limit.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { ScratchPublicationError } from "../src/git.js";
import { api, client, deferred, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";

installHarness();
function command(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();
}
function commit(dir: string, name: string): string {
  fs.writeFileSync(path.join(dir, name), name + "\n");
  command(dir, ["add", name]);
  command(dir, ["-c", "user.name=test", "-c", "user.email=test@example.com",
    "-c", "commit.gpgsign=false", "commit", "-m", name]);
  return command(dir, ["rev-parse", "HEAD"]);
}

for (const failure of ["prepack", "skipped", "ack-lost"] as const) {
  it(`${failure} checkpoint preserves H through H2/H3 rewrites without checkpoint confirmation`, async () => {
    const claim = gitlabClaim(1940);
    claim.claim_generation = 7;
    const { gitlab } = fakeGitlab();
    let scratchRefusals = 0, brokerCalls = 0;
    if (failure === "prepack") {
      const produce = git.checkpointPack.bind(git);
      git.checkpointPack = async (...args) => {
        try { return await produce(...args); }
        catch (error) {
          if (error instanceof ScratchPublicationError) scratchRefusals++;
          throw error;
        }
      };
    }
    client.publishCheckpoint = async (_run, _tip, pack) => {
      brokerCalls++;
      for await (const chunk of pack) assert.ok(chunk.length);
      if (failure === "ack-lost") throw new Error("broker response lost");
      return { ok: true, body: { ref: "refs/uzi-checkpoints/agent/issue-1940", published: false, reason: "workflow_scope" } };
    };
    const heads: string[] = [];
    const r = runner({ run: async ctx => {
      const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
      for (const name of ["H.txt", "H2.txt", "H3.txt"]) {
        command(ctx.worktreePath, ["reset", "--hard", base]);
        let sourceName = name;
        if (failure === "prepack") {
          // The runner provisioned this exclusion; remove it only in this private fixture
          // so the actual pack preflight receives forbidden scratch history.
          const exclude = path.join(ctx.worktreePath, ".git", "info", "exclude");
          fs.writeFileSync(exclude, fs.readFileSync(exclude, "utf8").split("\n")
            .filter(line => line !== "/.uzi/scratch/").join("\n"));
          fs.mkdirSync(path.join(ctx.worktreePath, ".uzi", "scratch"), { recursive: true });
          sourceName = ".uzi/scratch/" + name;
        }
        heads.push(commit(ctx.worktreePath, sourceName));
        await ctx.checkpoint!({ reap: false });
      }
      throw new Error("stop after rewrites");
    } }, gitlab, undefined, { checkpointIntervalMs: 1 });
    await r.execute(claim);
    if (failure === "prepack") {
      assert.ok(scratchRefusals > 0, "real Git scratch preflight refused before pack creation");
      assert.equal(brokerCalls, 0, "scratch history never reached the broker");
    }
    const candidates = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
    for (const head of heads) assert.ok(candidates.some(c => c.sha === head), head);
    assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
    const feed = JSON.stringify(api.messages(claim.run_id));
    for (const head of heads.slice(0, 2)) assert.ok(feed.includes(head.slice(0, 12)));
    assert.match(feed, /Rewrite\/divergence/);
    assert.match(feed, /uzi run recovery/);
    assert.match(feed, /worker-local/);
  });
}

it("published checkpoint reconciles only roots contained by its actual packed tip", async () => {
  const claim = gitlabClaim(1941);
  claim.claim_generation = 8;
  const { gitlab } = fakeGitlab();
  let published = false;
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    return { ok: true, body: { published, ref: "refs/uzi-checkpoints/test", reason: published ? undefined : "workflow_scope" } };
  };
  let h = "", h2 = "";
  await runner({ run: async ctx => {
    const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
    h = commit(ctx.worktreePath, "H.txt");
    await ctx.checkpoint!({ reap: false });
    command(ctx.worktreePath, ["reset", "--hard", base]);
    h2 = commit(ctx.worktreePath, "H2.txt");
    published = true;
    await ctx.checkpoint!({ reap: false });
    const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
    assert.ok(roots.some(c => c.sha === h));
    assert.equal(roots.some(c => c.sha === h2), false);
    throw new Error("stop after confirmation");
  } }, gitlab, undefined, { checkpointIntervalMs: 1 }).execute(claim);
});

for (const terminal of ["failed", "completed"] as const) {
  it(`${terminal} freezes before terminal reporting and retains pins when capture fails`, async () => {
    const claim = gitlabClaim(1943);
    claim.claim_generation = 10;
    claim.inventory_guarded = true;
    const { gitlab } = fakeGitlab();
    const feature = client.hasFeature.bind(client);
    client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
    client.listRecoveryHolds = async runId => ({ run_id: runId, holds: [
      { hold_id: "exact-generation-hold", generation: 10, inventory_guarded: true, has_available_capture: false },
    ] });
    client.getRunOwnership = async () => ({
      status: api.states.some(s => s.body.status === terminal) ? terminal : "running",
      claim_generation: 10, inventory_guarded: true,
    });
    let beforeReport = false;
    const currentTrees: string[] = [];
    const coverage = git.buildRecoveryCoverage.bind(git);
    git.buildRecoveryCoverage = async (...args) => {
      if (!api.states.some(s => s.body.status === terminal)) beforeReport = true;
      currentTrees.push(args[3]);
      return coverage(...args);
    };
    git.produceRecoveryBundle = async () => { throw new Error("capture storage unavailable"); };
    let clone = "", head = "";
    const r = runner({ run: async ctx => {
      clone = ctx.worktreePath;
      head = commit(clone, "terminal.txt");
      if (terminal === "failed") throw new Error("executor failed");
      return { branch: ctx.branch };
    } }, gitlab, "journal-key");
    await r.execute(claim);
    assert.equal(beforeReport, true, "immutable coverage precedes terminal RPC");
    assert.ok(currentTrees.length > 0);
    assert.ok(currentTrees.every(sha => sha === head), "all snapshots use the verified current tree, not an earlier adoption source");
    assert.ok(api.states.some(s => s.body.status === terminal), "capture failure never suppresses honest reporting");
    assert.equal(fs.existsSync(clone), true, "capture failure preserves source clone");
    const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
    assert.ok(roots.some(c => c.sha === head), "full push is never checkpoint confirmation");
    const records = await r.snapshotBootRecoveries();
    assert.ok(records.some(record => record.coverageDigest && record.originalSourceSha === head));
  });
}

it("failed terminal transfer journals clone-only HEAD and does not settle an older inventory", async () => {
  const claim = gitlabClaim(1960, { claim_generation: 27, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  const guard = guardedApi(claim, () => api.states.some(s => s.body.status === "failed") ? "failed" : "running");
  let clone = "", head = "", snapshots = 0, failedTransfers = 0;
  const coverage = git.buildRecoveryCoverage.bind(git);
  git.buildRecoveryCoverage = async (...args) => { snapshots++; return coverage(...args); };
  const r = runner({ run: async ctx => {
    clone = ctx.worktreePath;
    head = commit(clone, "clone-only.txt");
    git.fetchAgentBranch = async () => {
      failedTransfers++;
      throw new Error("injected metadata/pin transfer failure");
    };
    throw new Error("terminal failure");
  } }, gitlab, "journal-key");
  await r.execute(claim);
  assert.ok(failedTransfers > 0);
  assert.equal(snapshots, 0, "failed transfer cannot freeze old pins as the whole FINAL");
  assert.equal(guard.finals(), 0);
  assert.equal(fs.existsSync(clone), true);
  const records = await r.snapshotBootRecoveries();
  assert.ok(records.some(record => !record.coverageDigest && record.sourceSha === head &&
    record.generation === 27), "existing MAC pin records the actual clone-only source");
  const bare = git.barePathFor(fx.originPath);
  assert.equal((await git.enumerateOwedCandidates(bare, claim.run_id)).some(c => c.sha === head), false);
  assert.throws(() => command(bare, ["cat-file", "-t", head]));
});

it("issue1924 locally quiescent terminal captures original roots after foreign ownership404", async () => {
  const claim = gitlabClaim(1961, { claim_generation: 28, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  guardedApi(claim, () => "failed");
  let head = "", productions = 0;
  const produce = git.produceRecoveryBundle.bind(git);
  git.produceRecoveryBundle = async (...args) => { productions++; return produce(...args); };
  const r = runner({ run: async ctx => {
    head = commit(ctx.worktreePath, "foreign-reclaim.txt");
    client.getRunOwnership = async () => {
      throw new RequestError("GET", "/api/worker/runs/" + claim.run_id + "/ownership", 404,
        JSON.stringify({ error: "run not found " + "for this worker" }));
    };
    throw new Error("terminal failure after reclaim");
  } }, gitlab, "journal-key");
  await r.execute(claim);
  assert.ok(productions > 0);
  const records = await r.snapshotBootRecoveries();
  assert.ok(records.some(record => record.coverageDigest &&
    record.originalRoots?.some(root => root.sha === head)));
});

function guardedApi(claim: ReturnType<typeof gitlabClaim>, status: () => string) {
  const feature = client.hasFeature.bind(client);
  client.hasFeature = name => name === "recovery_inventory_v1" || feature(name);
  client.getRunOwnership = async () => ({
    status: status(), claim_generation: claim.claim_generation, inventory_guarded: true,
  });
  const serverId = "00000000-0000-4000-8000-000000001944";
  let manifest: { checksum: string; byte_size: number } | undefined;
  let finals = 0, open = true;
  client.listRecoveryHolds = async runId => ({ run_id: runId, holds: open ? [{
    hold_id: "inventory-hold", generation: claim.claim_generation!, inventory_guarded: true,
    has_available_capture: false,
  }] : [] });
  client.reserveRecoveryCapture = async () => ({ capture_id: serverId, state: "preparing" });
  client.uploadRecoveryBundle = async (_run, id, m, stream) => {
    assert.equal(id, serverId);
    for await (const chunk of stream) assert.ok(chunk);
    manifest = m;
    return { capture_id: serverId, state: "available", manifest_bound: true };
  };
  client.getRecoveryCaptureStatus = async () => ({
    capture_id: serverId, state: manifest ? "available" : "preparing", manifest_bound: !!manifest,
    checksum: manifest?.checksum, byte_size: manifest?.byte_size,
    expires_at: new Date(Date.now() + 60_000).toISOString(),
  });
  client.releaseRecoveryCustody = async () => {
    finals++;
    open = false;
    return { run_id: claim.run_id, generation: claim.claim_generation!,
      released: true, holds_released: 1 };
  };
  return { serverId, finals: () => finals };
}

for (const park of ["limit_wait", "recovery_wait"] as const) {
  it(`guarded ${park} archives divergent skipped checkpoints before feed close`, async () => {
    const claim = gitlabClaim(1950, { claim_generation: 17, inventory_guarded: true, wait_on_limit: true });
    const { gitlab } = fakeGitlab();
    const guard = guardedApi(claim, () =>
      api.states.some(s => s.body.status === park) ? park : "running");
    let h = "", h2 = "", checkpointCalls = 0, frozen = 0, notices = 0, produced = 0;
    client.publishCheckpoint = async (_run, tip, pack) => {
      checkpointCalls++;
      for await (const chunk of pack) assert.ok(chunk.length);
      assert.ok([h, h2].includes(tip), "broker only sees an actual checkpoint tip");
      return { ok: true, body: { published: false, ref: "refs/uzi-checkpoints/test", reason: "workflow_scope" } };
    };
    const snapshots: { accepted: boolean; roots: string[]; current: string }[] = [];
    const coverage = git.buildRecoveryCoverage.bind(git);
    git.buildRecoveryCoverage = async (...args) => {
      snapshots.push({ accepted: api.states.some(s => s.body.status === park), roots: [...args[2]], current: args[3] });
      frozen++;
      return coverage(...args);
    };
    const produce = git.produceRecoveryBundle.bind(git);
    git.produceRecoveryBundle = async (...args) => {
      assert.equal(args[1].forgeTip, undefined, "guarded archive is self-contained");
      produced++;
      return produce(...args);
    };
    const post = client.postMessages.bind(client);
    client.postMessages = async (...args) => {
      if (JSON.stringify(args[1]).includes(guard.serverId)) {
        notices++;
        const records = await r.snapshotBootRecoveries();
        const archive = records.find(record => record.coverageDigest && record.serverCaptureId === guard.serverId);
        assert.ok(archive);
        assert.equal(archive.state, "uploaded", "archive exists before the notice is delivered");
        assert.deepEqual(archive.originalRoots?.map(root => root.sha).sort(), [h, h2].sort());
        assert.equal(command(git.barePathFor(fx.originPath), ["rev-parse", archive.sourceSha + "^{tree}"]),
          command(git.barePathFor(fx.originPath), ["rev-parse", h2 + "^{tree}"]));
      }
      return post(...args);
    };
    const r = runner({ run: async ctx => {
      const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
      h = commit(ctx.worktreePath, "park-H.txt");
      await ctx.checkpoint!({ reap: false });
      command(ctx.worktreePath, ["reset", "--hard", base]);
      h2 = commit(ctx.worktreePath, "park-H2.txt");
      await ctx.checkpoint!({ reap: false });
      if (park === "limit_wait") throw new LimitReachedError({
        resetsAtMs: Date.now() + 3600_000, rateLimitType: "five_hour",
      });
      throw new TransientRecoveryError("ordinary transient executor exhaustion");
    } }, gitlab, "journal-key", { checkpointIntervalMs: 1 });
    await r.execute(claim);
    assert.ok(checkpointCalls >= 2);
    assert.ok(api.states.some(s => s.body.status === park));
    assert.equal(api.states.some(s => s.body.status === "failed"), false);
    assert.equal(api.states.some(s => s.body.checkpoint_contains_latest === true), false);
    assert.ok(frozen > 0);
    for (const snapshot of snapshots) {
      assert.equal(snapshot.accepted, true, "accepted park precedes immutable coverage");
      assert.ok([h, h2].every(sha => snapshot.roots.includes(sha)), "freeze covers both unrelated roots");
      assert.equal(snapshot.current, h2, "current tree is H2 rather than the stale checkpoint H");
    }
    assert.equal(produced, 1);
    assert.equal(notices, 1, "SERVER UUID archive notice reaches the feed before close");
    const feed = JSON.stringify(api.messages(claim.run_id));
    assert.ok(feed.includes(guard.serverId));
    for (const sha of [h, h2]) assert.ok(feed.includes(sha.slice(0, 12)));
    assert.match(feed, /uzi run export/);
    assert.equal(guard.finals(), 0, "registered execution retains custody until retirement");
  });
}

for (const accepted of [true, false]) {
  it(`guarded pause ${accepted ? "archives divergent roots before feed close" : "refusal keeps inventory unsealed"}`, async () => {
    const claim = gitlabClaim(1944, { claim_generation: 11, inventory_guarded: true });
    const { gitlab } = fakeGitlab();
    let parked = false;
    const guard = guardedApi(claim, () => parked ? "paused" : "running");
    let published = false, h = "", h2 = "";
    client.publishCheckpoint = async (_run, _tip, pack) => {
      for await (const chunk of pack) assert.ok(chunk);
      return { ok: true, body: { published, ref: "refs/uzi-checkpoints/test", reason: "workflow_scope" } };
    };
    const coverage = git.buildRecoveryCoverage.bind(git);
    let frozen = 0;
    git.buildRecoveryCoverage = async (...args) => {
      frozen++;
      assert.ok(api.states.some(s => s.body.status === "paused"), "capture follows accepted ACK");
      return coverage(...args);
    };
    const r = runner({ run: async ctx => {
      const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
      h = commit(ctx.worktreePath, "H.txt");
      await ctx.checkpoint!({ reap: false });
      command(ctx.worktreePath, ["reset", "--hard", base]);
      h2 = commit(ctx.worktreePath, "H2.txt");
      published = accepted;
      parked = accepted;
      const at = { completedCount: 1 };
      assert.equal(await ctx.parkForPause!(at), accepted);
      if (!accepted) {
        assert.equal(frozen, 0);
        assert.equal(guard.finals(), 0);
        assert.equal((await r.snapshotBootRecoveries()).some(record => !!record.coverageDigest), false);
        // Leave this still-running generation through bounded shutdown, not terminal failure.
        r.shutdown();
        throw new Error("shutdown after refused pause");
      }
      return { branch: ctx.branch, pausedAt: at };
    } }, gitlab, "journal-key", { checkpointIntervalMs: 1 });
    await r.execute(claim);
    const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
    assert.ok(roots.some(root => root.sha === h));
    if (accepted) {
      assert.equal(roots.some(root => root.sha === h2), false);
      assert.ok(frozen > 0);
      const feed = JSON.stringify(api.messages(claim.run_id));
      assert.ok(feed.includes(guard.serverId), "delivered feed names the SERVER capture UUID");
      assert.ok(feed.includes(h.slice(0, 12)));
      assert.match(feed, /uzi run export/);
      assert.match(feed, /local sources retained/);
      assert.equal(guard.finals(), 0, "the still-registered flight cannot finalize inventory");
    } else {
      assert.equal(frozen, 0);
      assert.equal(guard.finals(), 0);
    }
  });
}

it("uploaded guarded thin archive preserves the source at retirement while custody is pending", async () => {
  const claim = gitlabClaim(1962, { claim_generation: 29, inventory_guarded: true });
  // Published history exceeds the capture cap but its current tree is small.
  fs.writeFileSync(path.join(fx.originPath, "public-large"), randomBytes(256 * 1024));
  command(fx.originPath, ["add", "public-large"]);
  command(fx.originPath, ["-c", "commit.gpgsign=false", "commit", "-m", "public large history"]);
  fs.unlinkSync(path.join(fx.originPath, "public-large"));
  command(fx.originPath, ["add", "-u"]);
  command(fx.originPath, ["-c", "commit.gpgsign=false", "commit", "-m", "public deletion"]);
  const publicTip = command(fx.originPath, ["rev-parse", "HEAD"]);
  const { gitlab } = fakeGitlab();
  const guard = guardedApi(claim, () =>
    api.states.some(s => s.body.status === "failed") ? "failed" : "running");
  const produce = git.produceRecoveryBundle.bind(git);
  git.produceRecoveryBundle = (bare, opts) => produce(bare, { ...opts, maxBytes: 32 * 1024 });
  let clone = "", head = "";
  const r = runner({ run: async ctx => {
    clone = ctx.worktreePath;
    head = commit(clone, "thin-private.txt");
    throw new Error("terminal failure with unpublished output");
  } }, gitlab, "journal-key");
  // Observe the real disposition's flight flag before executeClaim's retirement logic.
  const internals = r as unknown as {
    settleGuardedInventory(...args: [typeof claim, { preserveRecoveryClone: boolean }, ...unknown[]]): Promise<void>;
  };
  const settle = internals.settleGuardedInventory.bind(r);
  const retirementFlags: boolean[] = [];
  internals.settleGuardedInventory = async (...args) => {
    await settle(...args);
    if (args[5] === true) retirementFlags.push(args[1].preserveRecoveryClone);
  };
  await r.execute(claim);
  const records = await r.snapshotBootRecoveries();
  const uploaded = records.find(record => record.state === "uploaded" && record.coverageDigest);
  assert.ok(uploaded, "real upload persists an available guarded capture");
  assert.equal(uploaded.serverCaptureId, guard.serverId);
  assert.equal(uploaded.selfContained, false);
  assert.deepEqual(uploaded.prerequisiteShas, [publicTip]);
  assert.notEqual(uploaded.finalAcknowledged, true);
  assert.ok(retirementFlags.length > 0, "terminal retirement disposition was observed");
  assert.ok(retirementFlags.every(flag => flag), "pending uploaded custody sets preserveRecoveryClone before retirement");
  assert.equal(fs.existsSync(clone), true, "actual source clone survives retirement");
  assert.equal(command(clone, ["rev-parse", "HEAD"]), head);
  assert.equal(command(clone, ["show", "HEAD:thin-private.txt"]), "thin-private.txt");
  const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
  assert.ok(roots.some(root => root.sha === head && root.contexts.some(c => c.generation === 29)));
  assert.equal(guard.finals(), 0, "thin capture cannot send FINAL");
});

it("guarded credential-switch give-up keeps inventory open until normal completion", async () => {
  const claim = gitlabClaim(1948, { claim_generation: 13, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  const guard = guardedApi(claim, () =>
    api.states.some(s => s.body.status === "completed") ? "completed" : "running");
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk.length);
    throw new Error("checkpoint ACK lost");
  };
  let continued = false, frozen = 0, h = "", h2 = "";
  const coverage = git.buildRecoveryCoverage.bind(git);
  git.buildRecoveryCoverage = async (...args) => {
    assert.equal(continued, true, "coverage cannot seal while the executor continues");
    frozen++;
    return coverage(...args);
  };
  const r = runner({ run: async ctx => {
    const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
    h = commit(ctx.worktreePath, "switch-H.txt");
    await ctx.checkpoint!({ reap: false });
    command(ctx.worktreePath, ["reset", "--hard", base]);
    h2 = commit(ctx.worktreePath, "switch-H2.txt");
    await ctx.checkpoint!({ reap: false });
    const verify = git.verifyRunnerTrackingCovers.bind(git);
    git.verifyRunnerTrackingCovers = async () => false;
    assert.equal(await ctx.attemptCredentialSwitch!(), "gave_up");
    git.verifyRunnerTrackingCovers = verify;
    assert.equal(frozen, 0);
    assert.equal(guard.finals(), 0, "continuing switch cannot release custody");
    const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
    for (const head of [h, h2]) assert.ok(roots.some(root => root.sha === head));
    assert.equal((await r.snapshotBootRecoveries()).some(record => !!record.coverageDigest), false);
    continued = true;
    return { branch: ctx.branch, reportOnly: true, summary: "continued after switch" };
  } }, gitlab, "journal-key", { checkpointIntervalMs: 1, recoveryRetryMs: 1 });
  await r.execute(claim);
  assert.ok(api.states.some(s => s.body.status === "completed"));
  assert.ok(frozen > 0, "actual terminal boundary seals the inventory");
  assert.ok((await r.snapshotBootRecoveries()).some(record => record.coverageDigest &&
    record.originalRoots?.some(root => root.sha === h)));
});

it("guarded shutdown retains owned source without sealing or waiting for archives", async () => {
  const claim = gitlabClaim(1945, { claim_generation: 12, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  const guard = guardedApi(claim, () => "running");
  let productions = 0, head = "";
  git.produceRecoveryBundle = async () => { productions++; throw new Error("must not archive running generation"); };
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk);
    throw new Error("checkpoint ACK lost");
  };
  const started = deferred();
  const r = runner({ run: async ctx => {
    head = commit(ctx.worktreePath, "shutdown.txt");
    started.resolve();
    const signal = ctx.signal;
    assert.ok(signal);
    await new Promise<void>(resolve => signal.addEventListener("abort", () => resolve(), { once: true }));
    throw new Error("shutdown");
  } }, gitlab, "journal-key", { shutdownPublishTimeoutMs: 1000 });
  const execution = r.execute(claim);
  await started.promise;
  r.shutdown();
  await execution;
  assert.equal(productions, 0);
  assert.equal(guard.finals(), 0);
  const records = await r.snapshotBootRecoveries();
  assert.ok(records.some(record => record.originalSourceSha === head || record.sourceSha === head));
  assert.equal(records.some(record => !!record.coverageDigest), false);
  const roots = await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id);
  assert.ok(roots.some(root => root.sha === head && root.contexts.some(c => c.generation === 12)));
});

it("fresh same-worker generation reconciles server checkpoint only, retaining divergent head", async () => {
  const claim = gitlabClaim(1946, { claim_generation: 1 });
  const { gitlab } = fakeGitlab();
  let h = "", h2 = "";
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk);
    throw new Error("ACK lost after actual pack drain");
  };
  const first = runner({ run: async ctx => {
    const base = command(ctx.worktreePath, ["rev-parse", "HEAD"]);
    h = commit(ctx.worktreePath, "H.txt");
    await ctx.checkpoint!({ reap: false });
    command(ctx.worktreePath, ["reset", "--hard", base]);
    h2 = commit(ctx.worktreePath, "H2.txt");
    await ctx.checkpoint!({ reap: false });
    first.shutdown();
    throw new Error("leave for requeue");
  } }, gitlab, "journal-key", { checkpointIntervalMs: 1 });
  await first.execute(claim);
  const bare = git.barePathFor(fx.originPath);
  let roots = await git.enumerateOwedCandidates(bare, claim.run_id);
  assert.ok(roots.some(root => root.sha === h));
  assert.ok(roots.some(root => root.sha === h2));
  const resumed = gitlabClaim(1946, { claim_generation: 2, checkpoint_tip: h });
  const second = runner({ run: async () => {
    roots = await git.enumerateOwedCandidates(bare, claim.run_id);
    assert.equal(roots.some(root => root.sha === h), false);
    assert.ok(roots.some(root => root.sha === h2));
    second.shutdown();
    throw new Error("stop resumed generation");
  } }, gitlab, "journal-key");
  await second.execute(resumed);
  assert.ok(JSON.stringify(api.messages(claim.run_id)).includes(h2.slice(0, 12)));
});

it("local bridge checkpoint floor containing H never confirms or clears its owed pin", async () => {
  const claim = gitlabClaim(1947, { claim_generation: 3 });
  const branch = "agent/issue-1947";
  command(fx.originPath, ["checkout", "-b", branch]);
  commit(fx.originPath, "remote-P.txt");
  command(fx.originPath, ["checkout", "main"]);
  const { gitlab } = fakeGitlab();
  client.publishCheckpoint = async (_run, _tip, pack) => {
    for await (const chunk of pack) assert.ok(chunk);
    throw new Error("no remote confirmation");
  };
  let h = "", floor = "";
  const r = runner({ run: async ctx => {
    const main = command(ctx.worktreePath, ["rev-parse", "refs/remotes/origin/main"]);
    command(ctx.worktreePath, ["reset", "--hard", main]);
    h = commit(ctx.worktreePath, "local-H.txt");
    await ctx.checkpoint!({ reap: true, progress: { completed: ["m1"], in_progress: [] } });
    const bare = git.barePathFor(fx.originPath);
    floor = (await git.trackingTip(bare, branch))!;
    assert.notEqual(floor, h, "actual bridge advanced the local checkpoint floor");
    assert.equal(await git.ancestry(bare, h, floor), "ancestor");
    assert.ok((await git.enumerateOwedCandidates(bare, claim.run_id)).some(root => root.sha === h));
    r.shutdown();
    throw new Error("leave local floor unconfirmed");
  } }, gitlab, "journal-key", { checkpointIntervalMs: 1 });
  await r.execute(claim);
  assert.ok(h && floor);
  assert.ok((await git.enumerateOwedCandidates(git.barePathFor(fx.originPath), claim.run_id))
    .some(root => root.sha === h));
});

it("boot discovery failure still snapshots independent authenticated journals", async () => {
  const { gitlab } = fakeGitlab();
  const claim = gitlabClaim(1942);
  claim.claim_generation = 9;
  const r = runner({ run: async ctx => { commit(ctx.worktreePath, "journal.txt"); throw new Error("stop"); } }, gitlab, "journal-key");
  await r.execute(claim);
  git.discoverOwedCandidates = async () => { throw new Error("inventory unreadable"); };
  const records = await r.snapshotBootRecoveries();
  assert.ok(records.some(record => record.runId === claim.run_id && record.generation === 9));
  let materialized = false;
  git.discoverOwedCandidates = async () => { materialized = true; return []; };
  await r.resumePendingRecoveries(undefined, []);
  // The discovery error did not make the public resume leg throw.
  assert.equal(materialized, false);
});
