import { beforeEach, describe, it, type TestContext } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { Readable } from "node:stream";
import fs from "node:fs";
import path from "node:path";
import { GitCache } from "../src/git.js";
import { RunRunner } from "../src/runner.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import { FakeRecoveryClient } from "./codex-reap-fixture.js";
import { codexRig, FakeCodexExecutor } from "./runner-codex-sinks-fixture.js";
import { defaultGitleaksShim } from "./gitleaks-shim.js";
import { portableTeardownTestDeps } from "./teardown-fixtures.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { PublicationContextRequest } from "../src/protocol.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { followSuccessfulPush, sourcePublicationContext } from "./publication-fixture.js";
import {
  api, client, fx, git, gitlabClaim, installHarness, runner,
  fakeGitlab, fakeGitHub, fakeForgejo,
} from "./runner-harness.js";

installHarness({ httpTimeoutMs: 500 });
beforeEach(async () => {
  api.setRegisterProtocolFeatures(["claim_generation_fence", "refresh_publication_context_v1"]);
  await client.register("refresh-publication");
});

const BRANCH = "agent/issue-3120";
const SOURCE_RUN = "31200000-0000-4000-8000-000000000001";
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"];
type Lane = "mr_rework" | "ci_fix";
type Forge = "github" | "gitlab" | "forgejo";
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], {
  encoding: "utf8",
  env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
  stdio: ["ignore", "pipe", "pipe"],
}).trim();

/** Keep the adopted remote branch at O; prepare C on a separate fixture-only ref. */
function history(message: string) {
  gitIn(fx.originPath, ["checkout", "-b", BRANCH]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", "Historical source publication"]);
  const original = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "-b", "fixture-candidate"]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", message]);
  const offending = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, [...IDENT, "commit", "--allow-empty", "-m", "Candidate tail"]);
  const candidate = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
  gitIn(fx.originPath, ["checkout", "main"]);
  assert.notEqual(original, candidate);
  assert.notEqual(offending, candidate);
  assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), original);
  return { original, offending, candidate };
}

function claimFor(lane: Lane, forge: Forge, original: string) {
  return gitlabClaim(3120, {
    run_id: randomUUID(),
    kind: lane,
    issue_iid: null,
    issue_title: "Refresh existing delivery",
    issue_description: "Apply review feedback",
    claim_generation: 7,
    branch: BRANCH,
    base_branch: "main",
    repo: {
      id: "r1",
      url: `https://${forge === "github" ? "github.com" : forge === "gitlab" ? "gitlab.com" : "forgejo.example"}/org/repo`,
      clone_url: fx.originPath,
      forge_type: forge,
    },
    ...(lane === "ci_fix" ? {
      pipeline: { id: 17, ref: BRANCH, sha: original, web_url: "https://example.test/pipeline/17", failed_jobs: [] },
    } : {}),
  });
}

/** Adopt the prebuilt exact candidate, so closing/non-closing runs publish the SAME commit. */
function candidateExecutor(candidate: string) {
  const seen: { clone?: string; head?: string } = {};
  const executor = new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      assert.equal(ctx.branch, BRANCH);
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

function authority(intent: "closing" | "non_closing", original: string) {
  api.publicationContextWire = ({ runId, body }) => ({
    status: 200,
    body: sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
      runId: SOURCE_RUN, publishedHead: original, mrIid: 42, issueIid: 3120, intent,
    }),
  });
}

function forgeFor(forge: Forge, original: string) {
  // Deliberately misleading PR prose: source intent comes exclusively from the API binding.
  const opts = { head: original, existing: "Closes #3120", existingTitle: "Existing delivery" };
  const gl = fakeGitlab(opts);
  const gh = forge === "github" ? fakeGitHub(opts) : undefined;
  const fj = forge === "forgejo" ? fakeForgejo(opts) : undefined;
  const active = gh ?? fj ?? gl;
  followSuccessfulPush(git, active.pr);
  return { gl, gh, fj, active };
}

const terminal = (id: string) => api.states.filter(s =>
  s.runId === id && ["completed", "failed"].includes(s.body.status));

/** Refusals retain the actual executor clone, with no completion authority minted. */
function assertRefused(
  id: string, original: string, probe: ReturnType<typeof candidateExecutor>, reason: RegExp,
  retainedHead = probe.seen.head,
) {
  assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), original);
  assert.deepEqual(terminal(id).map(s => s.body.status), ["failed"]);
  const failure = terminal(id)[0]!.body.failure_reason!;
  assert.match(failure, reason);
  assert.ok(failure.length < 1024);
  const evidence = JSON.stringify([api.messages(id), terminal(id)]);
  assert.ok(!evidence.includes("Fixes #3120"), "commit prose stays out of refusal evidence");
  assert.ok(!evidence.includes("PRIVATE_RESPONSE"), "raw HTTP prose stays out of refusal evidence");
  assert.equal(api.completionHoldRequests.length, 0);
  assert.equal(api.completionPermitRequests.length, 0);
  assert.equal(api.completionAttemptRequests.length, 0);
  assert.ok(probe.seen.clone && fs.existsSync(probe.seen.clone), "failed run preserves local recovery source");
  assert.equal(gitIn(probe.seen.clone!, ["rev-parse", "HEAD"]), retainedHead);
  return failure;
}

type Observation = "closing" | "non_closing" | "unknown";
type Drift = "source_run_id" | "mr_iid" | "published_head";

/** Only current restriction changes; the source's closing/false stamp remains immutable. */
function observations(original: string, sequence: readonly Observation[], drift?: Drift) {
  let index = 0;
  api.publicationContextWire = ({ runId, body }) => {
    assert.ok(index < sequence.length, "unexpected additional context observation");
    const current = sequence[index++]!;
    if (current === "unknown") return { status: 200, body: {
      version: 1, status: "unknown", reason: "authority_unknown",
    } };
    const response = sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
      runId: SOURCE_RUN, publishedHead: original, mrIid: 42, issueIid: 3120, intent: "closing",
    });
    assert.equal(response.status, "resolved");
    response.binding.intent = current;
    if (index > 1 && drift) {
      if (drift === "source_run_id") response.binding.source_run_id = "31200000-0000-4000-8000-000000000002";
      if (drift === "mr_iid") {
        response.binding.mr_iid = 43;
        response.binding.source_publication!.mr_iid = 43;
      }
      if (drift === "published_head") response.binding.source_publication!.published_head = "d".repeat(40);
    }
    assert.equal(response.binding.source_publication!.intent, "closing");
    assert.equal(response.binding.source_publication!.reported_non_closing, false);
    return { status: 200, body: response };
  };
}

/** A default workflow-only advance forces real GitHub alignment after the benign early scan. */
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
  let exact: string | undefined;
  let aligned = 0;
  t.mock.method(git, "alignBranchWithDefault", async (...args: Parameters<typeof realAlign>) => {
    const result = await realAlign(...args);
    if (result === "aligned") {
      assert.equal(exact, undefined, "one alignment, before the shared network retry group");
      gitIn(args[0], [...IDENT, "commit", "--amend", "-m", "Fixes #3120"]);
      exact = gitIn(args[0], ["rev-parse", "HEAD"]);
      gitIn(args[0], ["merge-base", "--is-ancestor", h.candidate, exact]);
      aligned++;
    }
    return result;
  });
  return { h, probe, check(id: string, requests: number) {
    assert.equal(aligned, 1, "real default alignment created the exact candidate");
    assert.ok(exact);
    assert.notEqual(exact, h.candidate);
    assert.notEqual(exact, h.original);
    const observed = api.publicationContextRequests.filter(r => r.runId === id);
    assert.equal(observed.length, requests);
    assert.deepEqual(observed.map(r => r.body), [
      { claim_generation: 7, branch: BRANCH, candidate_head: h.candidate },
      ...Array.from({ length: requests - 1 }, () => ({
        claim_generation: 7, branch: BRANCH, candidate_head: exact,
      })),
    ], "early scan uses executor SHA; every exact attempt freezes the aligned SHA");
    return exact;
  } };
}

describe("M2 refresh publication — real runner, scanner and push", () => {
  for (const lane of ["mr_rework", "ci_fix"] as const) {
    for (const forge of ["github", "gitlab", "forgejo"] as const) {
      it(`${lane}/${forge}: refuses non-closing history; identical candidate pushes with closing authority`, async t => {
        const h = history("Fixes #3120");
        const f = forgeFor(forge, h.original);
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof git.pushBranch>) => forward(...args));
        const run = (probe: ReturnType<typeof candidateExecutor>) =>
          runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh?.github, forgejo: f.fj?.forgejo });

        authority("non_closing", h.original);
        const refused = claimFor(lane, forge, h.original);
        const first = candidateExecutor(h.candidate);
        await run(first).execute(refused);
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.original, "refused branch remains at the original published head");
        assert.equal(push.mock.callCount(), 0);
        assert.deepEqual(terminal(refused.run_id).map(s => s.body.status), ["failed"]);
        const reason = terminal(refused.run_id)[0]!.body.failure_reason!;
        assert.match(reason, /non-closing delivery refused/);
        assert.ok(reason.includes(h.offending), "offending SHA is visible");
        assert.ok(reason.includes(`refused candidate ${h.candidate}`), "exact candidate is visible");
        assert.ok(reason.length < 1024, "bounded terminal evidence");
        const feed = JSON.stringify(api.messages(refused.run_id));
        assert.ok(feed.includes(h.offending));
        assert.ok(feed.includes(h.candidate));
        assert.ok(!feed.includes("Fixes #3120"), "no raw commit prose in refusal feed");
        assert.ok(!reason.includes("Fixes #3120"));
        assert.equal(api.completionHoldRequests.length, 0);
        assert.equal(api.completionPermitRequests.length, 0);
        assert.equal(api.completionAttemptRequests.length, 0);
        assert.ok(first.seen.clone && fs.existsSync(first.seen.clone), "unverified legacy capture retains local recovery source");
        assert.equal(gitIn(first.seen.clone!, ["rev-parse", "HEAD"]), h.candidate);
        assert.equal(gitIn(first.seen.clone!, ["merge-base", h.original, h.candidate]), h.original,
          "original head remains reachable in retained recovery history");
        const requests = api.publicationContextRequests.filter(r => r.runId === refused.run_id);
        assert.ok(requests.length > 0);
        for (const request of requests) assert.deepEqual(request.body, {
          claim_generation: 7, branch: BRANCH, candidate_head: h.candidate,
        });

        // An independent worker cache leaves the refused worker's recovery custody intact.
        const closingGit = new GitCache(path.join(fx.dataDir, "closing-worker"), nullLogger(), undefined,
          testGitCacheOptions({ gitleaksBin: defaultGitleaksShim() }));
        followSuccessfulPush(closingGit, f.active.pr);
        const closingForward = closingGit.pushBranch.bind(closingGit);
        const closingPush = t.mock.method(closingGit, "pushBranch",
          (...args: Parameters<typeof closingGit.pushBranch>) => closingForward(...args));
        authority("closing", h.original);
        const allowed = claimFor(lane, forge, h.original);
        const second = candidateExecutor(h.candidate);
        await new RunRunner(client, closingGit, () => ({ executor: second.executor }), nullLogger(), 20, undefined, {
          pollMs: 5, planApprovalTimeoutMs: 0, prDescriptionHeadLagMs: 0,
          gitlab: f.gl.gitlab, github: f.gh?.github, forgejo: f.fj?.forgejo,
          teardownTestDeps: portableTeardownTestDeps,
        }).execute(allowed);
        assert.equal(second.seen.head, first.seen.head, "same exact commit tested under both source intents");
        assert.equal(push.mock.callCount(), 0, "refused worker never pushes");
        assert.equal(closingPush.mock.callCount(), 1, "spy forwards through forge-head follower to the real push");
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.candidate);
        assert.notEqual(h.candidate, h.original);
        assert.equal(f.active.pr.head, h.candidate);
        assert.deepEqual(terminal(allowed.run_id).map(s => s.body.status), ["completed"]);
        assert.ok(fs.existsSync(first.seen.clone!), "closing delivery leaves refused recovery evidence intact");
      });
    }

    it(`${lane}/github: another issue's closing reference succeeds on a non-closing source`, async t => {
      const h = history("Fixes #9999");
      authority("non_closing", h.original);
      const f = forgeFor("github", h.original);
      const forward = git.pushBranch.bind(git);
      const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof git.pushBranch>) => forward(...args));
      const claim = claimFor(lane, "github", h.original);
      await runner(candidateExecutor(h.candidate).executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
      assert.equal(push.mock.callCount(), 1);
      assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.candidate);
      assert.equal(f.active.pr.head, h.candidate);
      assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["completed"]);
    });
  }

  for (const lane of ["mr_rework", "ci_fix"] as const) {
    for (const fault of ["unknown", "missing_feature", "malformed", "oversize", "unavailable", "unreadable"] as const) {
      it(`${lane}/github: early ${fault} fails closed and preserves the remote and local source`, async t => {
        const h = history("Fixes #3120");
        const f = forgeFor("github", h.original);
        authority("non_closing", h.original);
        if (fault === "missing_feature") {
          api.setRegisterProtocolFeatures(["claim_generation_fence"]);
          await client.register("old-api");
        } else if (fault === "unreadable") {
          t.mock.method(git, "publishedCommitMessages", async () => null);
        } else {
          api.publicationContextWire = ({ runId, body }) => {
            if (fault === "unknown") return { status: 200, body: {
              version: 1, status: "unknown", reason: "publication_unproven",
            } };
            if (fault === "malformed") return { status: 200, rawBody: "{PRIVATE_RESPONSE" };
            // Otherwise-valid closing authority would publish if the size/status guard were bypassed.
            const allowed = sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
              runId: SOURCE_RUN, publishedHead: h.original, mrIid: 42, issueIid: 3120, intent: "closing",
            });
            if (fault === "oversize") {
              const rawBody = JSON.stringify(allowed) + " ".repeat(16 * 1024 + 1);
              assert.ok(Buffer.byteLength(rawBody) > 16 * 1024, "actual response exceeds client cap");
              return { status: 200, rawBody };
            }
            return { status: 500, body: allowed };
          };
        }
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
        const claim = claimFor(lane, "github", h.original);
        const probe = candidateExecutor(h.candidate);
        await runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
        assert.equal(push.mock.callCount(), 0);
        assertRefused(claim.run_id, h.original, probe,
          fault === "unreadable" ? /could not read the commit messages/ : /source context could not be confirmed/);
        assert.equal(api.publicationContextRequests.length, fault === "missing_feature" ? 0 : 1);
      });
    }

    // Root kind is not a wire field: these cover only the normalized prompt/self-improve shape.
    it(`${lane}/github: normalized issue-less prompt/self-improve authority permits source-issue prose`, async t => {
      const h = history("Fixes #3120");
      const f = forgeFor("github", h.original);
      api.publicationContextWire = ({ runId, body }) => {
        const response = sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
          runId: SOURCE_RUN, publishedHead: h.original, mrIid: 42, issueIid: null, intent: "not_applicable",
        });
        assert.notEqual(response.status, "unknown");
        if (response.status === "unknown") throw new Error("fixture must be not applicable");
        assert.equal(response.binding.issue_iid, null);
        assert.equal(response.binding.source_publication, null);
        return { status: 200, body: response };
      };
      const forward = git.pushBranch.bind(git);
      const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
      const claim = claimFor(lane, "github", h.original);
      await runner(candidateExecutor(h.candidate).executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
      assert.equal(push.mock.callCount(), 1);
      assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.candidate);
      assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["completed"]);
    });

    it(`${lane}/github: benign reference to the source issue publishes under non-closing authority`, async t => {
      const h = history("Refs #3120");
      authority("non_closing", h.original);
      const f = forgeFor("github", h.original);
      const forward = git.pushBranch.bind(git);
      const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
      const claim = claimFor(lane, "github", h.original);
      await runner(candidateExecutor(h.candidate).executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
      assert.equal(push.mock.callCount(), 1);
      assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.candidate);
      assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["completed"]);
    });

    it(`${lane}/github: scans closing history already on the adopted source branch`, async t => {
      const h = history("Fixes #3120");
      // Publish the closing ancestor before the model's benign tail commit.
      gitIn(fx.originPath, ["branch", "-f", BRANCH, h.offending]);
      authority("non_closing", h.offending);
      const f = forgeFor("github", h.offending);
      const forward = git.pushBranch.bind(git);
      const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
      const scan = t.mock.method(git, "publishedCommitMessages", git.publishedCommitMessages.bind(git));
      const claim = claimFor(lane, "github", h.offending);
      const probe = candidateExecutor(h.candidate);
      await runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      const reason = assertRefused(claim.run_id, h.offending, probe, /non-closing delivery refused/);
      assert.ok(reason.includes(h.offending), "already-remote closing ancestor is offending evidence");
      assert.ok(reason.includes(h.candidate), "model tail is the refused candidate");
      const scanned = await scan.mock.calls[0]!.result;
      assert.ok(scanned);
      assert.ok(scanned.some(c => c.sha === h.offending));
    });

    for (const [early, final] of [
      ["closing", "non_closing"], ["closing", "unknown"],
      ["non_closing", "closing"], ["non_closing", "unknown"],
    ] as const) {
      it(`${lane}/github: ${early} -> ${final} between benign early scan and aligned exact candidate`, async t => {
        const rig = alignedCandidate(t);
        observations(rig.h.original, [early, final]);
        const f = forgeFor("github", rig.h.original);
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
        const claim = claimFor(lane, "github", rig.h.original);
        await runner(rig.probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
        const exact = rig.check(claim.run_id, 2);
        assert.equal(push.mock.callCount(), 0);
        const reason = assertRefused(claim.run_id, rig.h.original, rig.probe,
          final === "unknown" ? /source context could not be confirmed/ : /non-closing delivery refused/, exact);
        assert.ok(reason.includes(exact), "refusal identifies the actual aligned candidate");
        if (final !== "unknown") assert.ok(reason.includes(`(${exact})`), "aligned merge is offending evidence");
      });
    }

    for (const final of ["non_closing", "unknown"] as const) {
      it(`${lane}/github: retry observes closing -> ${final} before a second network push`, async t => {
        const rig = alignedCandidate(t);
        observations(rig.h.original, ["closing", "closing", final]);
        const f = forgeFor("github", rig.h.original);
        const forward = git.pushBranch.bind(git);
        let failed = false;
        const push = t.mock.method(git, "pushBranch", async (...args: Parameters<typeof forward>) => {
          if (!failed) {
            failed = true;
            assert.equal(api.publicationContextRequests.length, 2);
            assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), rig.h.original);
            throw new Error("connection reset");
          }
          return forward(...args);
        });
        // Advance only the forge retry's one-second timer, leaving runner and HTTP timers real.
        const realTimeout = globalThis.setTimeout;
        let backoffs = 0;
        t.mock.method(globalThis, "setTimeout", ((callback: (...args: unknown[]) => void, ms?: number, ...args: unknown[]) => {
          if (ms === 1000 && new Error().stack?.includes("waitRetryDelay")) {
            backoffs++;
            return realTimeout(callback, 0, ...args);
          }
          return realTimeout(callback, ms, ...args);
        }) as typeof setTimeout);
        const claim = claimFor(lane, "github", rig.h.original);
        await runner(rig.probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
        const exact = rig.check(claim.run_id, 3);
        assert.equal(backoffs, 1, "one transient failure enters the real retry group");
        assert.equal(push.mock.callCount(), 1, "retry refuses before another push call");
        const reason = assertRefused(claim.run_id, rig.h.original, rig.probe,
          final === "unknown" ? /source context could not be confirmed/ : /non-closing delivery refused/, exact);
        assert.ok(reason.includes(exact));
        if (final === "non_closing") assert.ok(reason.includes(`(${exact})`));
      });
    }

    for (const drift of ["source_run_id", "mr_iid", "published_head"] as const) {
      it(`${lane}/github: source ${drift} drift cannot unlatch an early restriction`, async t => {
        const rig = alignedCandidate(t);
        observations(rig.h.original, ["non_closing", "closing"], drift);
        const f = forgeFor("github", rig.h.original);
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
        const claim = claimFor(lane, "github", rig.h.original);
        await runner(rig.probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
        const exact = rig.check(claim.run_id, 2);
        assert.equal(push.mock.callCount(), 0);
        const reason = assertRefused(claim.run_id, rig.h.original, rig.probe, /source context could not be confirmed/, exact);
        assert.ok(reason.includes(exact), "identity drift fails closed on the exact candidate");
      });
    }
  }

  for (const lane of ["mr_rework", "ci_fix"] as const) {
    for (const captureFailure of [false, true]) {
      it(`${lane}/github: M2 archive ${captureFailure ? "failed capture retains H" : "verified capture restores H after physical cleanup"}`, async t => {
        const aligned = alignedCandidate(t);
        observations(aligned.h.original, ["non_closing", "non_closing"]);
        const f = forgeFor("github", aligned.h.original);
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
        const archiveClient = new FakeRecoveryClient();
        const savedBundle = path.join(fx.dataDir, "refresh-uploaded.bundle");
        const upload = archiveClient.uploadRecoveryBundle.bind(archiveClient);
        t.mock.method(archiveClient, "uploadRecoveryBundle", async (...args: Parameters<typeof upload>) => {
          const [runId, captureId, manifest, stream] = args;
          const chunks: Buffer[] = [];
          for await (const chunk of stream) chunks.push(Buffer.from(chunk));
          const bytes = Buffer.concat(chunks);
          assert.equal(bytes.length, manifest.byte_size);
          assert.equal(createHash("sha256").update(bytes).digest("hex"), manifest.checksum);
          fs.writeFileSync(savedBundle, bytes);
          return upload(runId, captureId, manifest, Readable.from([bytes]));
        });
        const options = { client: archiveClient, git, log: nullLogger(),
          recoveryRoot: git.recoveryRoot, workerToken: "refresh-recovery-worker-fixture" };
        const recovery = new RecoveryCoordinator(options);
        if (captureFailure) t.mock.method(git, "produceRecoveryBundle", async () => {
          throw new Error("injected bundle production failure");
        });
        const claim = claimFor(lane, "github", aligned.h.original);
        await runner(aligned.probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github, recovery }).execute(claim);
        const exact = aligned.check(claim.run_id, 2);
        const H = aligned.probe.seen.head!;
        assert.equal(H, aligned.h.candidate);
        assert.notEqual(H, exact);
        assert.equal(push.mock.callCount(), 0);
        assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), aligned.h.original);
        assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["failed"]);
        const reason = terminal(claim.run_id)[0]!.body.failure_reason!;
        assert.match(reason, /non-closing delivery refused/);
        assert.ok(reason.includes(`(${exact})`), "X is the offending exact candidate, separately from recovery H");
        assert.ok(!JSON.stringify([api.messages(claim.run_id), terminal(claim.run_id)]).includes("Fixes #3120"));
        assert.equal(api.completionHoldRequests.length, 0);
        assert.equal(api.completionPermitRequests.length, 0);
        assert.equal(api.completionAttemptRequests.length, 0);
        const records = await new RecoveryCoordinator(options).inspect(claim.run_id);
        assert.equal(records.length, 1);
        const record = records[0]!;
        assert.equal(record.sourceSha, H, "archive identity remains original executor H after alignment to X");
        assert.equal(record.generation, 7);
        assert.equal(record.finalizationPin, true);
        if (captureFailure) {
          assert.equal(record.state, "needs_action");
          assert.equal(record.reason, "bundle_failed");
          assert.equal(archiveClient.uploadCalls.length, 0);
          assert.equal(fs.existsSync(savedBundle), false);
          assert.ok(fs.existsSync(aligned.probe.seen.clone!));
          gitIn(aligned.probe.seen.clone!, ["merge-base", "--is-ancestor", H, exact]);
          assert.equal(gitIn(aligned.probe.seen.clone!, ["cat-file", "-t", H]), "commit");
        } else {
          assert.equal(record.state, "uploaded");
          assert.equal(archiveClient.uploadCalls.length, 1);
          assert.equal(archiveClient.reserveCalls[0]!.source_sha, H);
          assert.equal(fs.existsSync(aligned.probe.seen.clone!), false, "production execute physically removes the archived clone");
          const restored = path.join(fx.dataDir, "independent-restore");
          fs.mkdirSync(restored);
          gitIn(restored, ["init"]);
          // The real archive is incremental: only its forge-default prerequisites are seeded.
          gitIn(restored, ["fetch", fx.originPath, "refs/heads/main"]);
          assert.throws(() => gitIn(restored, ["cat-file", "-t", H]), "default prerequisites do not supply H");
          gitIn(restored, ["bundle", "verify", savedBundle]);
          gitIn(restored, ["fetch", savedBundle, H]);
          assert.equal(gitIn(restored, ["rev-parse", "FETCH_HEAD"]), H);
          assert.equal(gitIn(restored, ["cat-file", "-t", H]), "commit");
        }
      });
    }

    for (const stage of ["early", "exact"] as const) {
      for (const refusal of ["unknown", "closing"] as const) {
        it(`${lane}/gitlab: M2 Codex ${stage} ${refusal} refusal crosses finalize and dispose`, async t => {
          const h = history("Fixes #3120");
          const f = forgeFor("gitlab", h.original);
          observations(h.original, stage === "exact"
            ? ["closing", refusal === "unknown" ? "unknown" : "non_closing"]
            : [refusal === "unknown" ? "unknown" : "non_closing"]);
          const probe = candidateExecutor(h.candidate);
          const rig = codexRig();
          const executor = new FakeCodexExecutor(rig.safety, ctx => probe.executor.run(ctx));
          const forward = git.pushBranch.bind(git);
          const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
          const claim = claimFor(lane, "gitlab", h.original);
          await runner(executor, f.gl.gitlab).execute(claim);
          assert.equal(push.mock.callCount(), 0);
          const reason = assertRefused(claim.run_id, h.original, probe,
            refusal === "unknown" ? /source context could not be confirmed/ : /non-closing delivery refused/);
          if (refusal === "closing") {
            assert.ok(reason.includes(h.offending), "server issue selects offending history");
            assert.ok(reason.includes(h.candidate));
          }
          assert.deepEqual(api.publicationContextRequests.map(r => ({ runId: r.runId, body: r.body })),
            Array.from({ length: stage === "early" ? 1 : 2 }, () => ({
              runId: claim.run_id, body: { claim_generation: 7, branch: BRANCH, candidate_head: h.candidate },
            })), "only the requested stages observe authority; refusal is not retried");
          assert.ok(rig.boundaries.includes("finalize"));
          assert.ok(rig.disposeBoundaries.length > 0, "post-boundary production dispose occurred");
          assert.ok(rig.disposes() > 0);
          assert.equal(gitIn(probe.seen.clone!, ["cat-file", "-t", h.candidate]), "commit");
        });
      }
    }

    // These are wire forms, not independent proofs of the server's restriction origins.
    for (const wire of ["stored_non_closing", "current_restriction", "historical_without_stamp"] as const) {
      it(`${lane}/gitlab: M2 normalized non-closing wire form ${wire} refuses server-issue history`, async t => {
        const h = history("Fixes #3120");
        const f = forgeFor("gitlab", h.original);
        api.publicationContextWire = ({ runId, body }) => {
          const response = sourcePublicationContext(runId, body as unknown as PublicationContextRequest, {
            runId: SOURCE_RUN, publishedHead: h.original, mrIid: 42, issueIid: 3120,
            intent: wire === "current_restriction" ? "closing" : "non_closing",
          });
          assert.equal(response.status, "resolved");
          response.binding.intent = "non_closing";
          if (wire === "historical_without_stamp") response.binding.source_publication = null;
          else {
            assert.equal(response.binding.source_publication!.intent,
              wire === "stored_non_closing" ? "non_closing" : "closing");
            assert.equal(response.binding.source_publication!.reported_non_closing, wire === "stored_non_closing");
          }
          return { status: 200, body: response };
        };
        const forward = git.pushBranch.bind(git);
        const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
        const probe = candidateExecutor(h.candidate);
        const claim = claimFor(lane, "gitlab", h.original);
        assert.equal(claim.issue_iid, null, "issue authority must come from the server binding");
        await runner(probe.executor, f.gl.gitlab).execute(claim);
        assert.equal(push.mock.callCount(), 0);
        const reason = assertRefused(claim.run_id, h.original, probe, /non-closing delivery refused/);
        assert.ok(reason.includes(h.offending));
        assert.ok(reason.includes(h.candidate));
        assert.equal(api.publicationContextRequests.length, 1);
      });
    }
  }

  it("mr_rework/github: workflow refusal precedes context even with closing history", async t => {
    const workflow = path.join(fx.originPath, ".github/workflows/ci.yml");
    fs.mkdirSync(path.dirname(workflow), { recursive: true });
    fs.writeFileSync(workflow, "name: ci\non: [push]\njobs: {}\n");
    gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "seed workflow"]);
    const h = history("Fixes #3120");
    gitIn(fx.originPath, ["checkout", "fixture-candidate"]);
    fs.writeFileSync(workflow, "name: changed\non: [push]\njobs: {}\n");
    gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "workflow candidate"]);
    const candidate = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    authority("non_closing", h.original);
    const f = forgeFor("github", h.original);
    const forward = git.pushBranch.bind(git);
    const push = t.mock.method(git, "pushBranch", (...args: Parameters<typeof forward>) => forward(...args));
    const scan = t.mock.method(git, "publishedCommitMessages", git.publishedCommitMessages.bind(git));
    const claim = claimFor("mr_rework", "github", h.original);
    const probe = candidateExecutor(candidate);
    await runner(probe.executor, f.gl.gitlab, undefined, { github: f.gh!.github }).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.publicationContextRequests.length, 0);
    assert.equal(scan.mock.callCount(), 0);
    assert.equal(gitIn(fx.originPath, ["rev-parse", BRANCH]), h.original);
    assert.deepEqual(terminal(claim.run_id).map(s => s.body.status), ["failed"]);
    assert.equal(terminal(claim.run_id)[0]!.body.fail_origin, "workflow_scope_missing");
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(api.completionPermitRequests.length, 0);
  });

  it("raw HTTP hook stays unavailable by default and supports malformed and sequenced replies", async () => {
    const runId = randomUUID();
    const request = { claim_generation: 7, branch: BRANCH, candidate_head: "b".repeat(40) };
    await assert.rejects(client.publicationContext(runId, request), /publication context unavailable/);
    let sequence = 0;
    api.publicationContextWire = ({ runId: id, body }) => {
      sequence++;
      if (sequence === 1) return { status: 200, rawBody: "{malformed" };
      if (sequence === 2) return { status: 200, body: { version: 1, status: "unknown", reason: "publication_unproven" } };
      return { status: 200, body: sourcePublicationContext(id, body as unknown as PublicationContextRequest, {
        runId: SOURCE_RUN, publishedHead: "a".repeat(40), mrIid: 42, issueIid: null, intent: "not_applicable",
      }) };
    };
    await assert.rejects(client.publicationContext(runId, request), /publication context unavailable/);
    assert.deepEqual(await client.publicationContext(runId, request),
      { version: 1, status: "unknown", reason: "publication_unproven" });
    const result = await client.publicationContext(runId, request);
    assert.equal(result.status, "not_applicable");
    assert.equal(result.binding.issue_iid, null);
    assert.equal(result.binding.source_publication, null);
    assert.equal(result.binding.candidate_head, request.candidate_head);
    assert.equal(Object.keys(result.binding).length, 9);
    assert.equal(api.publicationContextRequests.length, 4);
    for (const recorded of api.publicationContextRequests) assert.deepEqual(recorded, { runId, body: request });
  });
});
