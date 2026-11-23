import { beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { PublicationContextRequest } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush, sourcePublicationContext } from "./publication-fixture.js";
import {
  api, client, fx, git, gitlabClaim, installHarness, runner, fakeGitlab, fakeGitHub,
} from "./runner-harness.js";
import {
  HELD_FEATURES, HELD_LINUX_ONLY, JOIN_TOKEN, assertCustodyRetained, deferTerminalEvidence, heldServer, redrive, retained,
  type Work,
} from "./runner-held-publication-fixture.js";

// Issue #4616 M4: the api answers `{status:"unknown", reason:"target_terminal"}` when the target MR
// of a ci_fix / mr_rework run has merged or closed. The worker keeps its existing conservative
// refusal (terminal failed report, no push, retained source); the only new behaviour is that the
// reason is now named in the diagnostic. Helpers mirror runner-refresh-publication.test.ts.

installHarness({ httpTimeoutMs: 500 });
beforeEach(async () => {
  api.setRegisterProtocolFeatures(["claim_generation_fence", "refresh_publication_context_v1"]);
  await client.register("refresh-publication-target-terminal");
});

const BRANCH = "agent/issue-3120";
const SOURCE_RUN = "31200000-0000-4000-8000-000000000001";
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"];
type Lane = "mr_rework" | "ci_fix";
const LANES: readonly Lane[] = ["mr_rework", "ci_fix"];
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], {
  encoding: "utf8",
  env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  stdio: ["ignore", "pipe", "pipe"],
}).trim();

function history(message: string) {
  gitIn(fx.originPath, ["checkout", "-b", BRANCH]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", "Historical source publication"]);
  const original = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "-b", "fixture-candidate"]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", message]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", "Candidate tail"]);
  const candidate = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  assert.notEqual(original, candidate);
  return { original, candidate };
}

function claimFor(lane: Lane, original: string, extra: Record<string, unknown> = {}) {
  return gitlabClaim(3120, {
    run_id: randomUUID(),
    kind: lane,
    issue_iid: null,
    issue_title: "Refresh existing delivery",
    issue_description: "Apply review feedback",
    claim_generation: 7,
    branch: BRANCH,
    base_branch: "main",
    repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    ...(lane === "ci_fix" ? {
      pipeline: { id: 17, ref: BRANCH, sha: original, web_url: "https://example.test/pipeline/17", failed_jobs: [] },
    } : {}),
    ...extra,
  });
}

function candidateExecutor(candidate: string) {
  const seen: { clone?: string; head?: string } = {};
  const executor = new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      gitIn(ctx.worktreePath, ["-c", "protocol.file.allow=user", "fetch", "--no-tags",
        fx.originPath, "refs/heads/fixture-candidate"]);
      gitIn(ctx.worktreePath, ["merge", "--ff-only", "FETCH_HEAD"]);
      seen.clone = ctx.worktreePath;
      seen.head = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      assert.equal(seen.head, candidate);
      return { branch: ctx.branch };
    }
  }(nullLogger());
  return { executor, seen };
}

function forgeFor(original: string) {
  const opts = { head: original, existing: "Closes #3120", existingTitle: "Existing delivery" };
  const gl = fakeGitlab(opts);
  const gh = fakeGitHub(opts);
  followSuccessfulPush(git, gh.pr);
  return { gl, gh };
}

const terminal = (id: string) => api.states.filter(s =>
  s.runId === id && ["completed", "failed"].includes(s.body.status));

/** Context observations in order: "non_closing" resolves, anything else is an unknown reason. */
function sequence(original: string, steps: readonly string[]) {
  let index = 0;
  api.publicationContextWire = ({ runId, body }) => {
    assert.ok(index < steps.length, "unexpected additional context observation");
    const step = steps[index++]!;
    if (step !== "non_closing") return { status: 200, body: { version: 1, status: "unknown", reason: step } };
    return { status: 200, body: sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
      runId: SOURCE_RUN, publishedHead: original, mrIid: 42, issueIid: 3120, intent: "non_closing",
    }) };
  };
}

/** A default workflow-only advance forces real alignment so the exact candidate differs from H. */
function alignedCandidate(t: TestContext) {
  const workflow = path.join(fx.originPath, ".github/workflows/ci.yml");
  fs.mkdirSync(path.dirname(workflow), { recursive: true });
  fs.writeFileSync(workflow, "name: ci\non: [push]\njobs: {}\n");
  gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
  gitIn(fx.originPath, [...IDENT, "commit", "-m", "seed workflow"]);
  const h = history("Refs #3120");
  const probe = candidateExecutor(h.candidate);
  const execute = probe.executor.run.bind(probe.executor);
  t.mock.method(probe.executor, "run", async (ctx: RunContext) => {
    const result = await execute(ctx);
    fs.writeFileSync(workflow, "name: ci\non: [pull_request]\njobs: {}\n");
    gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "default workflow advances"]);
    return result;
  });
  const realAlign = git.alignBranchWithDefault.bind(git);
  const aligned: { exact?: string } = {};
  t.mock.method(git, "alignBranchWithDefault", async (...args: Parameters<typeof realAlign>) => {
    const result = await realAlign(...args);
    if (result === "aligned") {
      gitIn(args[0], [...IDENT, "commit", "--amend", "-m", "Refs #3120 aligned"]);
      aligned.exact = gitIn(args[0], ["rev-parse", "HEAD"]);
    }
    return result;
  });
  return { h, probe, aligned };
}

/** Common refusal assertions: remote untouched, failed terminal, clone retained, no hold minted. */
function assertRefused(id: string, original: string, probe: ReturnType<typeof candidateExecutor>, reason: RegExp) {
  assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), original, "remote branch unchanged");
  assert.deepEqual(terminal(id).map(s => s.body.status), ["failed"]);
  const failure = terminal(id)[0]!.body.failure_reason!;
  assert.match(failure, reason);
  assert.ok(failure.length < 1024);
  assert.equal(api.completionHoldRequests.length, 0);
  assert.equal(api.completionPermitRequests.length, 0);
  assert.equal(api.completionAttemptRequests.length, 0);
  assert.ok(probe.seen.clone && fs.existsSync(probe.seen.clone), "failed run preserves local recovery source");
  return failure;
}

describe("#4616 M4 refresh publication: target_terminal context refusal", () => {
  for (const lane of LANES) {
    // Red on base: the reason is dropped from the diagnostic there ("could not be confirmed for candidate").
    it(`${lane}/github: target_terminal refuses, names the reason, and preserves remote and local source`, async t => {
      const h = history("Fixes #3120");
      const f = forgeFor(h.original);
      sequence(h.original, ["target_terminal"]);
      const push = t.mock.method(git, "pushBranch");
      const claim = claimFor(lane, h.original);
      const probe = candidateExecutor(h.candidate);
      await runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh.github }).execute(claim);
      assert.equal(push.mock.callCount(), 0, "no network push to the target branch");
      const failure = assertRefused(claim.run_id, h.original, probe, /source context could not be confirmed/);
      assert.ok(failure.includes(`source context could not be confirmed (target_terminal) for candidate ${h.candidate}`), failure);
      assert.ok(failure.includes("publication stopped"));
      assert.equal(gitIn(probe.seen.clone!, ["rev-parse", "HEAD"]), h.candidate, "local source retained at H");
      assert.equal(api.publicationContextRequests.filter(r => r.runId === claim.run_id).length, 1, "refusal is not retried");
      // The feed status (composePrepushRefusalStatus) names the reason and points at retained work.
      const statuses = api.messages(claim.run_id)
        .filter(m => m.kind === "status")
        .map(m => String((m.payload as { text?: unknown }).text ?? ""))
        .filter(text => text.includes("refresh publication refused"));
      assert.equal(statuses.length, 1, JSON.stringify(statuses));
      const status = statuses[0]!;
      assert.ok(status.includes("(target_terminal)"), status);
      assert.ok(status.includes(`uzi run held ${claim.run_id} or uzi run recovery ${claim.run_id}`), status);
    });

    // Compatibility control (green on base): an older worker rejects an unknown reason string and
    // still refuses with the bare diagnostic. A future reason behaves the same on this worker.
    it(`${lane}/github: unrecognised reason target_terminal_v2 still refuses with the bare diagnostic`, async t => {
      const h = history("Fixes #3120");
      const f = forgeFor(h.original);
      sequence(h.original, ["target_terminal_v2"]);
      const push = t.mock.method(git, "pushBranch");
      const claim = claimFor(lane, h.original);
      const probe = candidateExecutor(h.candidate);
      await runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh.github }).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      const failure = assertRefused(claim.run_id, h.original, probe, /source context could not be confirmed for candidate /);
      assert.ok(!/confirmed \(/.test(failure), failure);
      assert.ok(!failure.includes("target_terminal"));
    });

    // Red on base for the reason assertion; the retention assertions mirror the M2 archive cases.
    for (const captureFailure of [true, false]) {
      it(`${lane}/github: target_terminal at the exact stage, ${captureFailure ? "failed capture retains H" : "verified capture archives H"}`, async t => {
        const rig = alignedCandidate(t);
        sequence(rig.h.original, ["non_closing", "target_terminal"]);
        const f = forgeFor(rig.h.original);
        const push = t.mock.method(git, "pushBranch");
        const archiveClient = new FakeRecoveryClient();
        const options = { client: archiveClient, git, log: nullLogger(),
          recoveryRoot: git.recoveryRoot, workerToken: "refresh-recovery-worker-fixture" };
        const recovery = new RecoveryCoordinator(options);
        if (captureFailure) t.mock.method(git, "produceRecoveryBundle", async () => {
          throw new Error("injected bundle production failure");
        });
        const claim = claimFor(lane, rig.h.original);
        await runner(rig.probe.executor, f.gl.gitlab, undefined, { github: f.gh.github, recovery }).execute(claim);
        const H = rig.probe.seen.head!;
        assert.equal(H, rig.h.candidate);
        assert.ok(rig.aligned.exact && rig.aligned.exact !== H, "real alignment produced a distinct exact candidate");
        assert.equal(push.mock.callCount(), 0);
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), rig.h.original);
        assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["failed"]);
        const reason = terminal(claim.run_id)[0]!.body.failure_reason!;
        assert.ok(reason.includes(`source context could not be confirmed (target_terminal) for candidate ${rig.aligned.exact}`), reason);
        assert.equal(api.completionHoldRequests.length, 0);
        const records = await new RecoveryCoordinator(options).inspect(claim.run_id);
        assert.equal(records.length, 1);
        assert.equal(records[0]!.sourceSha, H, "archive identity remains the executor head H");
        if (captureFailure) {
          assert.equal(records[0]!.state, "needs_action");
          assert.equal(records[0]!.reason, "bundle_failed");
          assert.equal(archiveClient.uploadCalls.length, 0);
          assert.ok(fs.existsSync(rig.probe.seen.clone!), "failed capture keeps the local clone");
          assert.equal(gitIn(rig.probe.seen.clone!, ["cat-file", "-t", H]), "commit");
        } else {
          assert.equal(records[0]!.state, "uploaded");
          assert.equal(archiveClient.uploadCalls.length, 1);
          assert.equal(fs.existsSync(rig.probe.seen.clone!), false, "verified archive permits physical cleanup");
        }
      });
    }

    // Held publication (the failed-run refs/uzi-held path, HELD_PUBLICATION_FEATURE) IS reachable after a
    // refresh refusal: refusePrepush's non-hold branch journals a failed terminal, and the guarded
    // settlement defers the capture for the held step (heldEligibleTerminal). So inject failures into
    // the held POST and the held FINAL and prove custody stays recoverable either way.
    for (const failing of ["held POST", "held FINAL"] as const) {
      it(`${lane}/github: target_terminal then failing ${failing} keeps custody recoverable`, { skip: HELD_LINUX_ONLY }, async t => {
        const h = history("Fixes #3120");
        const f = forgeFor(h.original);
        sequence(h.original, ["target_terminal"]);
        const claim = claimFor(lane, h.original, { inventory_guarded: true });
        const server = heldServer(claim, HELD_FEATURES);
        if (failing === "held POST") server.heldAnswer = () => ({ ok: false, httpStatus: 503, reason: "forge_unavailable" });
        else server.finalAnswer = () => retained(claim, "held_publication_unavailable");
        const push = t.mock.method(git, "pushBranch");
        const probe = candidateExecutor(h.candidate);
        const r = runner(probe.executor, f.gl.gitlab, JOIN_TOKEN, { github: f.gh.github });
        deferTerminalEvidence(r, t);
        await r.execute(claim);
        assert.equal(push.mock.callCount(), 0);
        assertRefused(claim.run_id, h.original, probe, /\(target_terminal\)/);
        const work: Partial<Work> = { clone: probe.seen.clone! };
        // The held step was genuinely attempted (not vacuous), and the failure left everything in place.
        assert.equal(server.heldCalls.length, 1, "the held publication was attempted after the refusal");
        const record = await assertCustodyRetained(server, claim, work, `${failing} injected, after execute()`);
        if (failing === "held POST") {
          assert.equal(server.archiveUploads(), 1, "a failed held POST falls back to the archive on the same record");
          assert.equal(record.state, "uploaded");
        }
        await redrive(r);
        // A retained-FINAL answer (or the archive FINAL replay) is never a destructive outcome.
        assert.ok(fs.existsSync(probe.seen.clone!) || server.acked() === 1, "clone survives unless a FINAL was ACKed");
        if (failing === "held FINAL") {
          assert.equal(server.acked(), 0, "a retained FINAL never releases custody");
          await assertCustodyRetained(server, claim, work, "held FINAL retained, after redrive");
        }
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.original, "remote branch unchanged");
      });
    }

    // Guarded inventory: the guarded settlement owns cleanup. With a real RecoveryCoordinator the
    // refusal must leave the guarded hold open, send no release, and keep the trusted bare, recovery pin and archive (verified cleanup may retire the clone).
    // forge_mismatch is a secondary control: target_terminal behaves identically.
    it(`${lane}/github: guarded inventory keeps its hold and source on target_terminal`, async t => {
      const outcomes: Record<string, unknown> = {};
      for (const reason of ["forge_mismatch", "target_terminal"]) {
        api.states.length = 0;
        api.completionHoldRequests.length = 0;
        gitIn(fx.originPath, ["checkout", "main"]);
        for (const b of [BRANCH, "fixture-candidate"]) {
          try { gitIn(fx.originPath, ["branch", "-D", b]); } catch { /* absent on first pass */ }
        }
        const h = history("Fixes #3120");
        const f = forgeFor(h.original);
        sequence(h.original, [reason]);
        const claim = claimFor(lane, h.original, { inventory_guarded: true });
        // Inventory guarded, archive path only (no held-publication feature): the coordinator
        // captures and uploads, and the release FINAL cannot be sent from inside the flight.
        const server = heldServer(claim, ["recovery_inventory_v1"]);
        const push = t.mock.method(git, "pushBranch");
        const recovery = new RecoveryCoordinator({ client, git, log: nullLogger(),
          recoveryRoot: git.recoveryRoot, workerToken: JOIN_TOKEN });
        const probe = candidateExecutor(h.candidate);
        await runner(probe.executor, f.gl.gitlab, JOIN_TOKEN, { github: f.gh.github, recovery }).execute(claim);
        assert.equal(push.mock.callCount(), 0);
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.original);
        const statuses = terminal(claim.run_id).map(s => s.body.status);
        assert.deepEqual(statuses, ["failed"]);
        // Direct assertions: the guarded hold is kept, nothing is released, the source and archive remain.
        // The clone is not asserted here: a verified, self-contained archive legitimately permits its
        // physical cleanup (see the exact-stage test); the archive plus the open hold are the custody.
        const record = await assertCustodyRetained(server, claim, {}, `${reason}: guarded refusal`);
        assert.equal(server.finals.length, 0, `${reason}: no custody release was sent`);
        assert.equal(server.archiveUploads(), 1, `${reason}: the archive is retained for recovery`);
        assert.equal(record.state, "uploaded", `${reason}: the archive is on the frozen record`);
        outcomes[reason] = {
          statuses,
          holdOpen: server.holdOpen(),
          finals: server.finals.length,
          uploads: server.archiveUploads(),
          cloneRetained: fs.existsSync(probe.seen.clone!),
          recordState: record.state,
        };
        t.mock.restoreAll();
        await client.register("refresh-publication-target-terminal");
      }
      assert.deepEqual(outcomes["target_terminal"], outcomes["forge_mismatch"],
        "control: target_terminal is settled exactly like another context refusal");
    });
  }
});
