import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { Readable } from "node:stream";
import { createHash } from "node:crypto";
import { StubExecutor } from "../src/executor.js";
import { api, fx, git, fakeGitlab, gitlabClaim, installHarness, runner, runnerWith, homeDir } from "./runner-harness.js";
import type { PositiveOwedCandidateContext, PublicationCandidate } from "../src/git.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { nullLogger, recordingLogger } from "./helpers.js";

installHarness();

it("a rewritten run head cannot be replaced by a concurrent shared tracking write", async () => {
  const branch = "agent/issue-2404";
  const remote = path.join(fx.dataDir, "remote.git");
  cmd(fx.dataDir, "clone", "--bare", fx.originPath, remote);
  const writer = path.join(fx.dataDir, "writer");
  cmd(fx.dataDir, "clone", remote, writer);
  cmd(writer, "checkout", "-b", branch);
  cmd(writer, "commit", "--allow-empty", "-m", "P");
  cmd(writer, "push", "origin", branch);
  const P = cmd(writer, "rev-parse", "HEAD");
  const claim = gitlabClaim(2404, { kind: "ci_fix", branch, base_branch: "main",
    repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: remote } });
  const fetch = git.fetchAgentBranch.bind(git);
  let importedH = "";
  git.fetchAgentBranch = async (...args) => {
    // Rewrite below P just before the ownership-aware import.
    cmd(args[1], "reset", "--hard", "main");
    cmd(args[1], "commit", "--allow-empty", "-m", "rewritten H");
    importedH = cmd(args[1], "rev-parse", "HEAD");
    const tracking = await fetch(...args);
    assert.ok(tracking.kind === "updated", JSON.stringify(tracking));
    assert.equal(tracking.candidateSha, importedH);
    assert.equal(tracking.trackingRef, `refs/uzi-runner/${branch}`);
    return tracking;
  };
  const push = git.pushBranch.bind(git);
  let provenH = "";
  git.pushBranch = async (...args) => {
    provenH = args[5]?.originalHead ?? "";
    const promotion = await git.updateTrackingRef(args[0], branch, P, { context: {
      runId: "foreign-run", generation: 8, branch, kind: "ci_fix", barePath: args[0],
      defaultIdentity: { ref: "refs/remotes/origin/main", sha: cmd(args[0], "rev-parse", "refs/remotes/origin/main") },
    } });
    assert.ok(promotion.kind === "updated", JSON.stringify(promotion));
    cmd(writer, "commit", "--allow-empty", "-m", "R");
    cmd(writer, "push", "origin", branch);
    return push(...args);
  };
  await runner(new StubExecutor(nullLogger()), fakeGitlab().gitlab).execute(claim);
  const terminal = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
  assert.ok(terminal);
  assert.equal(terminal.branch_moved, undefined, "rewritten H never gets benign supersession");
  assert.equal(provenH, importedH, "proof uses the reaped clone's H, not mutable tracking");
  assert.match(terminal.failure_reason ?? "", /remote_candidate_diverged/);
});

// Each case makes one real transport rejection; the production forge retry waits 1000ms.
// Promotion is awaited only after pushBranch has rejected and released its bare lock.
for (const move of ["unchanged", "foreign", "same-generation"] as const) {
  it(`runner retry ${move} preserves the frozen candidate and checks ownership again`, async () => {
    const branch = "agent/issue-2404";
    const remote = path.join(fx.dataDir, "retry-remote.git");
    cmd(fx.dataDir, "clone", "--bare", fx.originPath, remote);
    const writer = path.join(fx.dataDir, "retry-writer");
    cmd(fx.dataDir, "clone", remote, writer);
    cmd(writer, "checkout", "-b", branch);
    cmd(writer, "commit", "--allow-empty", "-m", "P");
    cmd(writer, "push", "origin", branch);
    const P = cmd(writer, "rev-parse", "HEAD");
    const claim = gitlabClaim(2404, { kind: "issue", branch, claim_generation: 7,
      config: { completion_contract_version: 1, contract_revision: 1 },
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: remote } });
    api.setCompletionPermitResponse(true);
    const forge = fakeGitlab();
    const { logger, lines } = recordingLogger();
    const recoveryClient = new FakeRecoveryClient();
    const recoveryOptions = { client: recoveryClient, git, log: nullLogger(),
      recoveryRoot: git.recoveryRoot, workerToken: "publication-retry-worker-fixture" };
    const recovery = new RecoveryCoordinator(recoveryOptions);
    const marker = path.join(homeDir, "session-marker");
    fs.writeFileSync(marker, "retained session");
    let clone = "";
    let context: PositiveOwedCandidateContext | undefined;
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = async (...args) => {
      const result = await fetch(...args);
      assert.ok(result.kind === "updated", JSON.stringify(result));
      assert.equal(result.candidateSha, cmd(args[1], "rev-parse", branch));
      assert.ok(args[4]);
      const fetchedContext = args[4].context;
      assert.ok(fetchedContext.generation !== null && fetchedContext.legacy === undefined,
        "generation-7 fetch supplies a positive, non-legacy context");
      assert.equal(fetchedContext.runId, claim.run_id);
      assert.equal(fetchedContext.generation, 7);
      assert.equal(fetchedContext.kind, claim.kind);
      assert.equal(fetchedContext.branch, branch);
      assert.equal(fetchedContext.barePath, args[0]);
      assert.deepEqual(fetchedContext.defaultIdentity, {
        ref: "refs/remotes/origin/main",
        sha: cmd(args[0], "rev-parse", "refs/remotes/origin/main"),
      });
      context = fetchedContext;
      return result;
    };

    const order: string[] = [];
    const retryChecks: Array<{ expected: string | undefined; kind: string }> = [];
    const ownership = git.committedTrackingOwnership.bind(git);
    let failedWire = false;
    let retryObserved = false;
    git.committedTrackingOwnership = async (...args) => {
      const firstRetry = args[3] !== undefined && failedWire && !retryObserved;
      if (firstRetry) {
        retryObserved = true;
        order.push("retry ownership");
      } else if (args[3] !== undefined && !failedWire) {
        order.push("ownership");
      }
      const result = await ownership(...args);
      if (firstRetry) retryChecks.push({ expected: args[3], kind: result.kind });
      return result;
    };
    const seam = git as unknown as {
      runGit(cwd: string, args: string[], ...rest: unknown[]): Promise<string>;
    };
    const wire = seam.runGit.bind(git);
    const refspecs: string[] = [];
    seam.runGit = async (cwd, args, ...rest) => {
      if (args[0] === "push") {
        refspecs.push(args[2]!);
        if (refspecs.length === 1) {
          order.push("failed wire");
          failedWire = true;
          throw new Error("connection reset before send");
        }
        order.push("successful wire");
      }
      return wire(cwd, args, ...rest);
    };
    const push = git.pushBranch.bind(git);
    const publications: PublicationCandidate[] = [];
    let D = "";
    let promotedOwned = false;
    let expectedRefused = false;
    git.pushBranch = async (...args) => {
      assert.ok(args[5]);
      publications.push(args[5]);
      try {
        await push(...args);
        forge.pr.head = args[5].candidate;
      } catch (error) {
        assert.equal(publications.length, 1, "only the first transport is injected");
        if (move !== "unchanged") {
          assert.ok(context, "fetch supplied this flight's trusted context");
          const C = args[5].candidate;
          const tree = cmd(args[0], "rev-parse", C + "^{tree}");
          D = cmd(args[0], "commit-tree", tree, "-p", C, "-m", "descendant D");
          assert.notEqual(D, C);
          const promoted = await git.updateTrackingRef(args[0], branch, D, { context: move === "foreign"
            ? { ...context, runId: "foreign-run", generation: 8 } : context });
          assert.ok(promoted.kind === "updated", JSON.stringify(promoted));
          assert.equal(promoted.candidateSha, D);
          order.push("outside-lock promotion");
          // Observe real ownership independently, without adding to retryChecks.
          const own = await ownership(args[0], branch, claim.run_id, undefined, 7);
          promotedOwned = own.kind === "owned" && own.sha === D &&
            own.context.runId === claim.run_id && own.context.generation === 7;
          expectedRefused = (await ownership(args[0], branch, claim.run_id, C, 7)).kind === "not_owned";
        }
        throw error;
      }
    };
    await runnerWith(() => ({ executor: {
      run: async ctx => {
        clone = ctx.worktreePath;
        return new StubExecutor(nullLogger()).run(ctx);
      },
    }, homeDir }), forge.gitlab, undefined, logger, { recovery }).execute(claim);

    if (move === "same-generation") {
      assert.equal(publications.length, 1,
        "same-run/same-generation candidate move must refuse before a SECOND pushBranch invocation");
    }
    const publication = publications[0];
    assert.ok(publication, "the flight reached its first pushBranch boundary");
    const C = publication.candidate;
    assert.equal(publication.originalHead, C);
    assert.deepEqual(publication.floor, { kind: "pinned", oid: P });
    assert.ok(Object.isFrozen(publication));
    assert.equal(expectedRefused, move !== "unchanged");
    assert.equal(promotedOwned, move === "same-generation",
      "same-run/same-generation D remains OWNED without an expected SHA");
    assert.ok(retryChecks.some(check => check.expected === C && check.kind ===
      (move === "unchanged" ? "owned" : "not_owned")), "actual retry ownership check binds frozen C");
    const retryDelays = lines.filter(line =>
      (line as { msg?: string }).msg === "transient forge error; retrying push/MR-create")
      .map(line => ({ attempt: (line as { attempt: number }).attempt,
        delay: (line as { delay_ms: number }).delay_ms }));
    assert.deepEqual(retryDelays, [{ attempt: 1, delay: 1000 }],
      "the production forge retry requests its real first 1000ms delay");
    const failedIndex = order.indexOf("failed wire");
    assert.ok(order.slice(0, failedIndex).includes("ownership"), "ownership precedes first wire");
    assert.deepEqual(order.slice(failedIndex), move === "unchanged"
      ? ["failed wire", "retry ownership", "successful wire"]
      : ["failed wire", "outside-lock promotion", "retry ownership"]);
    if (move === "unchanged") {
      assert.equal(publications.length, 2, "two pushBranch callbacks reached the real transport");
      assert.strictEqual(publications[1], publication, "retry retains the same frozen C/H/P object");
      assert.deepEqual(refspecs, [`${C}:refs/heads/${branch}`, `${C}:refs/heads/${branch}`]);
      assert.equal(cmd(remote, "rev-parse", branch), C);
      assert.equal(api.completionPermitRequests.length, 1);
      assert.equal(api.completionPermitRequests[0]!.body.head, C);
      const completed = api.states.find(s => s.runId === claim.run_id && s.body.status === "completed")?.body;
      assert.ok(completed);
      assert.equal(completed.head, C);
    } else {
      assert.equal(publications.length, 1, "retry refuses before the second pushBranch callback");
      assert.deepEqual(refspecs, [`${C}:refs/heads/${branch}`], "exactly one failed wire attempt");
      const terminal = api.states.find(s => s.runId === claim.run_id && s.body.status === "failed")?.body;
      assert.ok(terminal);
      assert.match(terminal.failure_reason ?? "", /tracking preservation refused/);
      assert.equal(terminal.branch_moved, undefined);
      assert.equal(cmd(remote, "rev-parse", branch), P);
      assert.equal(api.states.some(s => s.body.status === "completed"), false);
      assert.equal(api.completionPermitRequests.length, 0, "neither C nor D gets a completion permit");
      assert.equal(fs.existsSync(clone), true, "preservation refusal retains the clone");
      assert.equal(fs.readFileSync(marker, "utf8"), "retained session", "per-run HOME session survives");
      const bare = git.barePathFor(remote);
      const owed = (await git.enumerateOwedCandidates(bare, claim.run_id)).find(c => c.sha === publication.originalHead);
      const records = await new RecoveryCoordinator(recoveryOptions).inspect(claim.run_id);
      assert.ok(owed?.contexts.some(c => c.runId === claim.run_id && c.generation === 7) ||
        records.some(r => r.generation === 7 && r.sourceSha === publication.originalHead),
      "positive generation-specific custody preserves original H");
      assert.deepEqual(recoveryClient.releaseCalls, [], "retained H custody is never released as publication");
    }
  });
}

function cmd(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, "-c", "maintenance.auto=false", "-c", "gc.auto=0",
    "-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", ...args],
    { encoding: "utf8", stdio: "pipe", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" } }).trim();
}

for (const kind of ["mr_rework", "ci_fix", "issue"] as const) {
  for (const captureFailure of kind === "issue" ? [false] : [false, true]) {
  it(`${kind} uses real publication proof with ${captureFailure ? "failed capture and retained custody" : "verified recovery capture"}`, async () => {
    const branch = "agent/issue-2404";
    const remote = path.join(fx.dataDir, "remote.git");
    cmd(fx.dataDir, "clone", "--bare", fx.originPath, remote);
    cmd(remote, "config", "maintenance.auto", "false");
    const writer = path.join(fx.dataDir, "writer");
    cmd(fx.dataDir, "clone", remote, writer);
    cmd(writer, "checkout", "-b", branch);
    fs.writeFileSync(path.join(writer, "floor.txt"), "floor\n");
    cmd(writer, "add", "floor.txt");
    cmd(writer, "commit", "-m", "P");
    cmd(writer, "push", "origin", branch);
    const claim = gitlabClaim(2404, { kind, branch, claim_generation: 7, base_branch: "main",
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: remote } });
    const original = git.pushBranch.bind(git);
    let R = "";
    let C = "";
    let H = "";
    git.pushBranch = async (...args) => {
      C = args[5]?.candidate ?? "";
      H = args[5]?.originalHead ?? "";
      cmd(writer, "commit", "--allow-empty", "-m", "R");
      cmd(writer, "push", "origin", branch);
      R = cmd(writer, "rev-parse", "HEAD");
      // Updating shared tracking must not replace the flight's captured P.
      cmd(args[0], "fetch", "origin");
      return original(...args);
    };
    const { gitlab, calls } = fakeGitlab();
    const recoveryClient = new FakeRecoveryClient();
    const archivedBundle = path.join(fx.dataDir, "uploaded.bundle");
    const upload = recoveryClient.uploadRecoveryBundle.bind(recoveryClient);
    recoveryClient.uploadRecoveryBundle = async (runId, captureId, manifest, stream) => {
      const chunks: Buffer[] = [];
      for await (const chunk of stream) chunks.push(Buffer.from(chunk));
      const bytes = Buffer.concat(chunks);
      assert.equal(bytes.length, manifest.byte_size);
      assert.equal(createHash("sha256").update(bytes).digest("hex"), manifest.checksum);
      fs.writeFileSync(archivedBundle, bytes);
      return upload(runId, captureId, manifest, Readable.from([bytes]));
    };
    const recoveryOptions = { client: recoveryClient, git, log: nullLogger(),
      recoveryRoot: git.recoveryRoot, workerToken: "publication-proof-worker-fixture" };
    const recovery = new RecoveryCoordinator(recoveryOptions);
    if (captureFailure) {
      git.produceRecoveryBundle = async () => { throw new Error("injected bundle production failure"); };
    }
    await runner(new StubExecutor(nullLogger()), gitlab, undefined, { recovery }).execute(claim);
    const terminal = api.states.find((s) => s.runId === claim.run_id && s.body.status === "failed")?.body;
    assert.ok(terminal);
    assert.equal(terminal.fail_origin, undefined);
    assert.equal(terminal.branch_moved, kind === "issue" ? undefined : true);
    assert.equal(terminal.failure_reason, kind === "issue"
      ? "scratch_publication_refused: candidate history cannot be published (remote_branch_advanced at floor_refresh)"
      : `branch_moved: remote_branch_advanced; superseding_tip=${R}`);
    assert.match(C, /^[0-9a-f]{40}$/);
    assert.equal(H, C, "the plain flight retains the original head separately from candidate identity");
    assert.equal(cmd(remote, "rev-parse", branch), R);
    assert.equal(calls.length, 0);
    const bare = git.barePathFor(remote);
    // Reopen the authenticated coordinator journal as a restarted worker would.
    const records = await new RecoveryCoordinator(recoveryOptions).inspect(claim.run_id);
    assert.equal(records.length, 1);
    const record = records[0]!;
    assert.equal(record.generation, 7);
    assert.equal(record.sourceSha, H, "the recovery journal names original H");
    assert.equal(record.finalizationPin, true);
    assert.deepEqual(recoveryClient.releaseCalls, [], "supersession never claims publication custody release");
    if (captureFailure) {
      assert.equal(record.state, "needs_action");
      assert.equal(record.reason, "bundle_failed");
      assert.equal(recoveryClient.uploadCalls.length, 0);
      assert.equal(cmd(bare, "rev-parse", `refs/uzi-runner/${branch}`), H,
        "the retained tracking custody ref anchors the journaled original head");
      cmd(bare, "gc", "--prune=now");
      assert.equal(cmd(bare, "rev-parse", `${H}^{commit}`), H,
        "journaled source remains reachable after terminal cleanup and object pruning");
      assert.equal(fs.existsSync(archivedBundle), false);
    } else {
      assert.equal(record.state, "uploaded");
      assert.equal(recoveryClient.uploadCalls.length, 1);
      assert.equal(recoveryClient.reserveCalls[0]!.source_sha, C);
      assert.equal(recoveryClient.reserveCalls[0]!.generation, 7);
      assert.ok(record.bundlePath);
      assert.ok(fs.statSync(archivedBundle).size > 0);
      cmd(bare, "bundle", "verify", archivedBundle);
      assert.match(cmd(bare, "bundle", "list-heads", archivedBundle), new RegExp(`^${C} `));
    }
  });
  }
}
