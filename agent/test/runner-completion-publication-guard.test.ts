import { beforeEach, describe, it } from "node:test";
import type { SteeringChannel } from "../src/steering.js";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { COMPLETION_END, closingDirectiveFor } from "../src/pr-description.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { nullLogger } from "./helpers.js";
import { followSuccessfulPush } from "./publication-fixture.js";
import { codexRig, FakeCodexExecutor, manualDeadline } from "./runner-codex-sinks-fixture.js";
import {
  api, client, fx, git, gitlabClaim, installHarness, runner, runnerWith, homeDir, worktreeDirFor,
  fakeGitlab, fakeGitHub, fakeForgejo,
} from "./runner-harness.js";

installHarness({ httpTimeoutMs: 500 });
beforeEach(async () => {
  api.setRegisterProtocolFeatures(["claim_generation_fence", "completion_non_closing_v1"]);
  await client.register("m3");
});

const IID = 3090;
const DEFERRED = [{ milestone_id: "m2", title: "Follow up", reason: "Owner deferred" }];
const statuses = (id: string) => api.states.filter(s => s.runId === id).map(s => s.body.status);
const terminal = (id: string) => api.states.filter(s => s.runId === id && ["completed", "failed"].includes(s.body.status));
const gitIn = (dir: string, args: string[]) => execFileSync("git", ["-C", dir, ...args], {
  encoding: "utf8", env: { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" },
}).trim();
const IDENT = ["-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"];

function claimFor(forge: "github" | "gitlab" | "forgejo" = "gitlab", scope = DEFERRED) {
  const claim = gitlabClaim(IID, {
    claim_generation: 7,
    repo: { id: "r1", url: `https://${forge === "github" ? "github.com" : forge === "gitlab" ? "gitlab.com" : "forgejo.example"}/org/repo`, clone_url: fx.originPath, forge_type: forge },
    config: { completion_contract_version: 1, contract_revision: 1, completion_scope: { deferred: scope } },
  });
  api.setPublicationState(claim.run_id, scope);
  api.requireCompletionAttempt(claim.run_id);
  return claim;
}

function committing(message: string, extra: Partial<ExecutorResult> = {}, configure?: (ctx: RunContext) => void) {
  const seen: { head?: string; clone?: string } = {};
  const executor = new class extends StubExecutor {
    override async run(ctx: RunContext): Promise<ExecutorResult> {
      const result = await super.run(ctx);
      fs.writeFileSync(path.join(ctx.worktreePath, "m3.txt"), "preserved work\n");
      gitIn(ctx.worktreePath, ["add", "m3.txt"]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", message]);
      seen.head = gitIn(ctx.worktreePath, ["rev-parse", "HEAD"]);
      seen.clone = ctx.worktreePath;
      configure?.(ctx);
      return { ...result, ...extra };
    }
  }(nullLogger());
  return { executor, seen };
}

function guardOnly(body: Record<string, unknown>): boolean {
  return body.status === "running" && Object.keys(body).every(key => ["status", "claim_generation", "messages_through_seq"].includes(key));
}

function asyncStack(): string {
  const previous = Error.stackTraceLimit;
  try {
    Error.stackTraceLimit = 100;
    return new Error("publication boundary probe").stack!;
  } finally {
    Error.stackTraceLimit = previous;
  }
}

function noPublish(id: string) {
  assert.equal(terminal(id).length, 0);
  assert.equal(api.completionPermitRequests.length, 0);
}

describe("M3 publication-start completion guard", () => {
  for (const forge of ["github", "gitlab", "forgejo"] as const) {
    it(`${forge}: owner partial closing history records one empty attempt then holds without push`, async t => {
      const claim = claimFor(forge);
      let checkpoint = -1;
      const probe = committing("Fixes #3090", {}, () => { checkpoint = api.states.length; });
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      const gl = fakeGitlab();
      const gh = forge === "github" ? fakeGitHub() : undefined;
      const fj = forge === "forgejo" ? fakeForgejo() : undefined;
      await runner(probe.executor, gl.gitlab, undefined, { github: gh?.github, forgejo: fj?.forgejo }).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionAttemptRequests.length, 1);
      assert.deepEqual(api.completionAttemptRequests[0], {
        milestones_completed: [], head: probe.seen.head, worktree_fingerprint: null, claim_generation: 7,
      });
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(api.completionHoldRequests[0]!.body.claim_generation, 7);
      const holdCopy = JSON.stringify(api.messages(claim.run_id));
      assert.match(holdCopy, /choose continue.*lead to reword/);
      assert.match(holdCopy, /published history or a Stop, cancel/);
      noPublish(claim.run_id);
      assert.ok(fs.existsSync(worktreeDirFor(IID)));
      const guards = api.states.slice(checkpoint).filter(s => s.runId === claim.run_id && guardOnly(s.body as unknown as Record<string, unknown>));
      assert.equal(guards.length, 1, "exactly one publication report after the executor checkpoint");
      assert.deepEqual(guards.at(-1)!.body, { status: "running", claim_generation: 7 });
    });
  }

  for (const [label, extra, stored] of [
    ["Stop cap", { scopeCapped: { completedCount: 1, total: 2 } }, false],
    ["declared", { nonClosing: { reason: "Acceptance remains" } }, false],
    ["stored declaration", {}, true],
  ] as const) {
    it(`${label}: zero attempts become an admitted prepush hold`, async t => {
      const claim = claimFor("gitlab", []);
      if (stored) claim.config!.non_closing = { reason: "Stored intent" };
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      await runner(committing("Closes #3090", extra).executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionAttemptRequests.length, 1);
      assert.equal(api.completionHoldRequests.length, 1);
      if (label !== "Stop cap") assert.ok(api.completionAttemptRequests[0]!.non_closing);
      noPublish(claim.run_id);
    });
  }

  for (const [name, mutate] of [
    ["missing revision", (run: Record<string, unknown>) => { delete run.completion_revision; }],
    ["null revision", (run: Record<string, unknown>) => { run.completion_revision = null; }],
    ["string revision", (run: Record<string, unknown>) => { run.completion_revision = "1"; }],
    ["unsafe revision", (run: Record<string, unknown>) => { run.completion_revision = Number.MAX_SAFE_INTEGER + 1; }],
    ["missing deferred", (run: Record<string, unknown>) => { delete run.completion_deferred; }],
    ["null deferred", (run: Record<string, unknown>) => { run.completion_deferred = null; }],
    ["nonarray deferred", (run: Record<string, unknown>) => { run.completion_deferred = {}; }],
    ["primitive entry", (run: Record<string, unknown>) => { run.completion_deferred = [1]; }],
    ["null entry", (run: Record<string, unknown>) => { run.completion_deferred = [null]; }],
    ["missing id", (run: Record<string, unknown>) => { run.completion_deferred = [{}]; }],
    ["wrong id type", (run: Record<string, unknown>) => { run.completion_deferred = [{ milestone_id: 1 }]; }],
    ["empty id", (run: Record<string, unknown>) => { run.completion_deferred = [{ milestone_id: "" }]; }],
    ["overlong id", (run: Record<string, unknown>) => { run.completion_deferred = [{ milestone_id: "m".repeat(65) }]; }],
    ["invalid id", (run: Record<string, unknown>) => { run.completion_deferred = [{ milestone_id: "-m" }]; }],
    ["mixed array", (run: Record<string, unknown>) => { run.completion_deferred = [DEFERRED[0], null]; }],
    ["wrong run", (run: Record<string, unknown>) => { run.id = "other-run"; }],
    ["wrong worker", (run: Record<string, unknown>) => { run.worker_id = "other-worker"; }],
  ] as const) {
    it(`${name}: unknown projection/identity holds before pushing`, async t => {
      const claim = claimFor();
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      const probe = committing("Refs #3090", {}, () => {
        api.stateAckWire = (body, run) => {
          if (body.status !== "running") return;
          mutate(run);
          return { status: 200, body: { run } };
        };
      });
      await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionAttemptRequests.length, 1);
      assert.equal(api.completionHoldRequests.length, 1);
      noPublish(claim.run_id);
      const feed = api.messages(claim.run_id).filter(m => JSON.stringify(m.payload).includes("Completion state could not be confirmed before push"));
      assert.equal(feed.length, 1);
      assert.ok(!JSON.stringify(feed).includes(probe.seen.head!));
    });
  }

  for (const status of ["queued", "claimed", "completed", "failed", "paused", "cancelled", undefined, null, 1, false, {}, []]) {
    it(`nonrunning ${status} stops without push, hold or terminal`, async t => {
      const claim = claimFor();
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      const probe = committing("Fixes #3090", {}, () => {
        api.stateAckWire = (body, run) => body.status === "running" ? { status: 200, body: { run: { ...run, status } } } : undefined;
      });
      await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionHoldRequests.length, 0);
      assert.equal(api.completionAttemptRequests.length, 0);
      assert.ok(fs.existsSync(worktreeDirFor(IID)), "quiet stop retains this flight's work through the stopped-source path");
      noPublish(claim.run_id);
    });
  }

  for (const stale of [false, true]) {
    it(`running409 stale=${stale}: ${stale ? "quiet stop" : "holds"}`, async t => {
      const claim = claimFor();
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      const probe = committing("Refs #3090", {}, () => {
        api.stateAckWire = (body, run) => body.status === "running"
          ? { status: 409, body: { run, ...(stale ? { disposition: "stale_claim" } : {}) } } : undefined;
      });
      await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionHoldRequests.length, stale ? 0 : 1);
      noPublish(claim.run_id);
    });
  }

  it("a pre-freeze live claim learns partial scope from its own ACK and refuses closing history", async t => {
    const claim = claimFor("gitlab", []);
    delete claim.config!.contract_revision;
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    const probe = committing("Fixes #3090", {}, () => api.setPublicationState(claim.run_id, DEFERRED, 2));
    await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionHoldRequests.length, 1);
    noPublish(claim.run_id);
  });

  for (const mode of ["full", "accept", "partial", "reordered"] as const) {
    it(`${mode}: valid projection permits appropriate title/body and no synthetic attempt`, async () => {
      const second = { milestone_id: "m3", title: "Second follow up", reason: "Later" };
      const scope = mode === "reordered" ? [...DEFERRED, second] : mode === "partial" ? DEFERRED : [];
      const claim = claimFor("gitlab", scope);
      if (mode === "accept") claim.config!.completion_scope = { accepted: [{ id: "m1.c1", milestone_id: "m1", text: "Criterion", reason: "Accepted" }] };
      if (mode === "reordered") api.setPublicationState(claim.run_id, [second, DEFERRED[0], second, DEFERRED[0]]);
      api.setCompletionPermitResponse(true);
      api.completionPermitScope = claim.config!.completion_scope ?? {};
      const forge = fakeGitlab();
      followSuccessfulPush(git, forge.pr, true);
      let checkpoint = -1;
      await runner(committing(scope.length ? "Refs #3090" : "Fixes #3090", {}, () => { checkpoint = api.states.length; }).executor, forge.gitlab).execute(claim);
      assert.equal(api.states.slice(checkpoint).filter(s => guardOnly(s.body as unknown as Record<string, unknown>)).length, 1);
      if (mode === "reordered") assert.match(forge.pr.description, /Second follow up/);
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.equal(api.completionAttemptRequests.length, 0);
      assert.equal(api.completionHoldRequests.length, 0);
      if (scope.length) {
        assert.doesNotMatch(forge.pr.description, /Closes #3090/);
        assert.match(forge.pr.description, /Follow up/);
        assert.match(forge.pr.title, /partial/i);
      } else assert.match(forge.pr.description, /Closes #3090/);
    });
  }

  for (const hold of ["confirmed", "ambiguous", "refused"] as const) {
    it(`capture exhaustion + ${hold} hold uses empty head and actual hold disposition`, async () => {
      const claim = claimFor();
      const probe = committing("Fixes #3090", {}, () => {
        git.verifyRunnerTrackingCovers = async () => false;
        if (hold === "ambiguous") api.completionHoldWire.set(claim.run_id, res => { res.writeHead(200); res.end(""); });
        if (hold === "refused") api.setCompletionHoldResponse("running", 409);
      });
      const sentinel = path.join(homeDir, "session");
      fs.writeFileSync(sentinel, "session");
      await runnerWith(() => ({ executor: probe.executor, homeDir }), fakeGitlab().gitlab, undefined, undefined, {
        recoveryRetryMs: 1, completionHoldReconcileTimeoutMs: 500,
      }).execute(claim);
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(api.completionHoldRequests[0]!.body.head, "");
      assert.equal(fs.existsSync(worktreeDirFor(IID)), hold !== "refused");
      assert.equal(fs.existsSync(sentinel), hold !== "refused");
      assert.equal(terminal(claim.run_id).length, hold === "refused" ? 1 : 0);
      if (hold === "refused") assert.match(terminal(claim.run_id)[0]!.body.failure_reason!, new RegExp(probe.seen.head!));
    });
  }

  it("existing attempts skip the synthetic call", async () => {
    const claim = claimFor();
    api.setPublicationState(claim.run_id, DEFERRED, 1, 2);
    api.requireCompletionAttempt(claim.run_id, 2);
    await runner(committing("Fixes #3090").executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(api.completionHoldRequests.length, 1);
    noPublish(claim.run_id);
  });

  it("attempt 409: stale stops immediately", async () => {
    const claim = claimFor();
    api.completionAttemptHttpStatus = 409;
    await runner(committing("Fixes #3090").executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.equal(api.completionHoldRequests.length, 0);
    noPublish(claim.run_id);
  });

  for (const count of ["absent", "null"] as const) {
    for (const hold of ["confirmed", "ambiguous", "refused"] as const) {
      it(`unknown attempt count ${count}: exhausted 503 with prior one and ${hold} hold`, async t => {
        const claim = claimFor();
        api.requireCompletionAttempt(claim.run_id, 1);
        api.setPublicationState(claim.run_id, DEFERRED, 1, 1);
        api.completionAttemptHttpStatus = 503;
        let publicationAcks = 0;
        const probe = committing("Fixes #3090", {}, () => {
          api.stateAckWire = (body, run) => {
            if (!guardOnly(body as unknown as Record<string, unknown>)) return;
            assert.equal(run.completion_attempts, 1, "server really has one successful prior attempt");
            publicationAcks++;
            if (count === "absent") delete run.completion_attempts;
            else run.completion_attempts = null;
            return { status: 200, body: { run } };
          };
        });
        if (hold === "ambiguous") api.completionHoldWire.set(claim.run_id, res => { res.writeHead(200); res.end(""); });
        if (hold === "refused") api.setCompletionHoldResponse("running", 409);
        const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
        await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
        assert.equal(publicationAcks, 1);
        assert.equal(api.completionAttemptRequests.length, 3);
        assert.equal(api.completionHoldRequests.length, 1);
        assert.equal(push.mock.callCount(), 0);
        assert.equal(api.completionPermitRequests.length, 0);
        assert.equal(terminal(claim.run_id).length, hold === "refused" ? 1 : 0);
        if (hold === "refused") {
          assert.equal(terminal(claim.run_id)[0]!.body.status, "failed");
          assert.equal(terminal(claim.run_id)[0]!.body.failure_reason,
            `non-closing delivery refused: commit message(s) close issue #3090 (${probe.seen.head}); publication stopped; correcting them needs a history rewrite, left to the owner`);
        } else assert.ok(fs.existsSync(worktreeDirFor(IID)), "held/ambiguous work remains local");
      });
    }
  }

  it("unknown projection with definitive hold refusal reports one fixed unknown line and no SHA", async t => {
    const claim = claimFor();
    api.setCompletionHoldResponse("running", 409);
    const probe = committing("Fixes #3090", {}, () => {
      api.stateAckWire = (body, run) => {
        if (!guardOnly(body as unknown as Record<string, unknown>)) return;
        delete run.completion_deferred;
        return { status: 200, body: { run } };
      };
    });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(terminal(claim.run_id).length, 1);
    assert.equal(terminal(claim.run_id)[0]!.body.status, "failed");
    assert.equal(terminal(claim.run_id)[0]!.body.failure_reason,
      "completion interlock: could not verify the final head or park the run for recovery");
    assert.ok(!terminal(claim.run_id)[0]!.body.failure_reason!.includes(probe.seen.head!));
    const lines = api.messages(claim.run_id).filter(m => JSON.stringify(m.payload).includes("Completion state could not be confirmed before push"));
    assert.equal(lines.length, 1);
    assert.equal((lines[0]!.payload as { text: string }).text,
      "Completion state could not be confirmed before push; the run is held for the owner.");
    assert.ok(!JSON.stringify(lines).includes(probe.seen.head!));
  });

  for (const evidence of ["unreadable", "invalid", "many"] as const) {
    it(`closing ${evidence} evidence is content-free and preserves work`, async t => {
      const claim = claimFor();
      const originals = Array.from({ length: 7 }, (_, i) => String(i + 1).repeat(40));
      t.mock.method(git, "publishedCommitMessages", async () => evidence === "unreadable" ? null : evidence === "invalid"
        ? [{ sha: "malformed-evidence", message: "Fixes #3090" }]
        : originals.map(sha => ({ sha, message: "Fixes #3090" })));
      await runner(committing("Refs #3090").executor, fakeGitlab().gitlab).execute(claim);
      const feed = JSON.stringify(api.messages(claim.run_id));
      if (evidence === "many") {
        assert.ok(feed.includes(originals[0]!));
        assert.ok(feed.includes(originals[4]!));
        assert.ok(!feed.includes(originals[5]!));
        assert.ok(feed.includes("2 more"));
      } else assert.match(feed, /could not read/);
      assert.ok(!feed.includes("malformed-evidence"));
      assert.equal(api.completionHoldRequests.length, 1);
      noPublish(claim.run_id);
    });
  }

  for (const transport of ["transient", "lost ACK"] as const) {
    it(`synthetic attempt ${transport} retries then holds once`, async () => {
      const claim = claimFor();
      if (transport === "transient") {
        api.completionAttemptHttpStatus = 503;
        api.completionAttemptFailTimes = 1;
      } else api.completionAttemptDropReplies = 1;
      await runner(committing("Fixes #3090").executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(api.completionAttemptRequests.length, 2);
      assert.equal(api.completionHoldRequests.length, 1);
      noPublish(claim.run_id);
    });
  }

  for (const kind of ["closing", "unknown"] as const) {
    it(`declared intent without feature: ${kind} refusal stays terminal`, async () => {
      api.setRegisterProtocolFeatures(["claim_generation_fence"]);
      await client.register("old-api");
      const claim = claimFor("gitlab", []);
      claim.config!.non_closing = { reason: "Still requires acceptance" };
      const probe = committing("Fixes #3090", {}, () => {
        if (kind === "unknown") api.setPublicationState(claim.run_id, null);
      });
      await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
      assert.equal(api.completionAttemptRequests.length, 0);
      assert.equal(api.completionHoldRequests.length, 0);
      assert.equal(terminal(claim.run_id).length, 1);
      assert.doesNotMatch(JSON.stringify(api.messages(claim.run_id)), /the run is held/,
        "terminal compatibility refusal never claims an accepted hold");
      assert.match(terminal(claim.run_id)[0]!.body.failure_reason!, kind === "closing" ? new RegExp(probe.seen.head!) : /completion interlock: could not verify/);
    });
  }

  it("an earlier iteration ACK cannot replace the publication ACK or its permit revision", async t => {
    const claim = claimFor("gitlab", []);
    delete claim.config!.contract_revision;
    const other = [{ milestone_id: "other", title: "Earlier title", reason: "Earlier reason" }];
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    api.completionPermitScope = { deferred: DEFERRED };
    let earlier!: Promise<unknown>;
    let release!: () => void;
    let received!: () => void;
    const releaseEarlier = new Promise<void>(resolve => { release = resolve; });
    const earlierReceived = new Promise<void>(resolve => { received = resolve; });
    const report = client.reportState.bind(client);
    let checkpoint = -1;
    let earlierRevision: number | undefined;
    const probe = committing("Refs #3090", {}, ctx => {
      checkpoint = api.states.length;
      api.stateAckWire = (body, run) => body.status === "running" ? {
        status: 200, body: { run: { ...run,
          completion_revision: body.iteration_count === 23 ? 9 : 2,
          completion_deferred: body.iteration_count === 23 ? other : DEFERRED,
        } },
      } : undefined;
      t.mock.method(client, "reportState", async (id: string, body: Parameters<typeof client.reportState>[1], signal?: AbortSignal) => {
        if (body.iteration_count === 23) {
          const ack = await report(id, body, signal);
          earlierRevision = ack.completionProjection?.revision;
          received();
          await releaseEarlier;
          return ack;
        }
        if (guardOnly(body as unknown as Record<string, unknown>)) {
          // Start an earlier HTTP report through the public iteration seam after the
          // executor report-chain drain. Delay only its client return, not the server.
          assert.ok(ctx.reportIteration);
          earlier = Promise.resolve(ctx.reportIteration(23));
          await earlierReceived;
          try {
            const own = await report(id, body, signal);
            assert.equal(own.completionProjection?.revision, 2);
            release();
            await earlier; // revision 9 reaches flight.reportState before the permit
            return own;
          } finally { release(); }
        }
        return report(id, body, signal);
      });
    });
    try {
      await runner(probe.executor, forge.gitlab).execute(claim);
    } finally {
      release();
      await earlier;
    }
    assert.equal(earlierRevision, 9, "the competing ACK really carried the higher revision");
    assert.equal(api.states.slice(checkpoint).filter(s => guardOnly(s.body as unknown as Record<string, unknown>)).length, 1);
    assert.equal(api.completionPermitRequests.length, 1);
    assert.equal(api.completionPermitRequests[0]!.body.contract_revision, 2);
    assert.match(forge.pr.description, /Follow up.*Owner deferred/);
    assert.doesNotMatch(forge.pr.description, /Earlier title|Earlier reason|Closes #3090/);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });

  for (const sent of [6, 7]) {
    it(`local outbound generation ${sent} alone determines publication admission`, async t => {
      const claim = claimFor("gitlab", []);
      const forge = fakeGitlab();
      followSuccessfulPush(git, forge.pr, true);
      api.completionPermitScope = {};
      const push = t.mock.method(git, "pushBranch", git.pushBranch.bind(git));
      let checkpoint = -1;
      const report = client.reportState.bind(client);
      const probe = committing("Refs #3090", {}, () => {
        checkpoint = api.states.length;
        api.stateAckWire = (body, run) => body.status === "running"
          ? { status: 200, body: { run: { ...run, claim_generation: 7 } } } : undefined;
        t.mock.method(client, "reportState", (id: string, body: Parameters<typeof client.reportState>[1], signal?: AbortSignal) =>
          report(id, guardOnly(body as unknown as Record<string, unknown>) ? { ...body, claim_generation: sent } : body, signal));
      });
      await runner(probe.executor, forge.gitlab).execute(claim);
      const reports = api.states.slice(checkpoint).filter(s => guardOnly(s.body as unknown as Record<string, unknown>));
      assert.equal(reports.length, 1);
      assert.equal(reports[0]!.body.claim_generation, sent);
      assert.equal(push.mock.callCount(), sent === 7 ? 1 : 0);
      assert.equal(api.completionPermitRequests.length, sent === 7 ? 1 : 0);
      assert.equal(forge.calls.length > 0, sent === 7);
      assert.equal(api.completionHoldRequests.length, sent === 7 ? 0 : 1);
      if (sent === 7) assert.ok(statuses(claim.run_id).includes("completed"));
      else {
        noPublish(claim.run_id);
        assert.equal(gitIn(fx.originPath, ["branch", "--list", "agent/issue-3090"]), "");
      }
    });
  }

  for (const mode of ["legacy", "report_only", "not_code", "capped empty"] as const) {
    it(`${mode} skips the publication-start report after executor return`, async t => {
      const claim = claimFor("gitlab", []);
      if (mode === "legacy") delete claim.config!.completion_contract_version;
      if (mode === "not_code") claim.kind = "ci_fix";
      const forge = fakeGitlab();
      let pushes = 0;
      if (mode === "legacy") followSuccessfulPush(git, forge.pr, true);
      else t.mock.method(git, "pushBranch", async () => { pushes++; throw new Error("unexpected push"); });
      api.completionPermitScope = {};
      let checkpoint = -1;
      const events: string[] = [];
      const permit = client.requestCompletionPermit.bind(client);
      t.mock.method(client, "requestCompletionPermit", async (...args: Parameters<typeof client.requestCompletionPermit>) => {
        events.push("permit");
        assert.equal(terminal(claim.run_id).length, 0, "empty-cap permit precedes terminal completion");
        return permit(...args);
      });
      const mark = () => { checkpoint = api.states.length; };
      const executor = mode === "legacy" ? committing("Refs #3090", {}, mark).executor : {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          mark();
          return { branch: ctx.branch,
            ...(mode === "report_only" ? { reportOnly: true, summary: "Evidence only" } :
              mode === "not_code" ? { fixVerdict: "not_code" as const } :
                { scopeCapped: { completedCount: 0, total: 2 }, summary: "Nothing landed" }),
          };
        },
      };
      await runner(executor, forge.gitlab).execute(claim);
      assert.ok(checkpoint >= 0);
      assert.equal(api.states.slice(checkpoint).filter(s => guardOnly(s.body as unknown as Record<string, unknown>)).length, 0);
      assert.ok(statuses(claim.run_id).includes("completed"));
      assert.equal(api.completionPermitRequests.length, mode === "capped empty" ? 1 : 0);
      if (mode === "capped empty") {
        assert.deepEqual(events, ["permit"]);
        assert.equal(terminal(claim.run_id)[0]!.body.report_only, true);
        assert.equal(terminal(claim.run_id)[0]!.body.scope_capped, true);
      }
      if (mode !== "legacy") {
        assert.equal(pushes, 0);
        assert.equal(forge.calls.length, 0);
      }
    });
  }

  it("a revision-one empty ACK does not invent a frozen NULL contract", async t => {
    const claim = claimFor("gitlab", []);
    delete claim.config!.contract_revision;
    api.setPublicationState(claim.run_id, [], 1, 1);
    api.requireCompletionAttempt(claim.run_id, 1);
    api.setCompletionPermitResponse(false, { denyReason: "contract_not_frozen" });
    const permit = client.requestCompletionPermit.bind(client);
    let deniedReason: string | undefined;
    t.mock.method(client, "requestCompletionPermit", async (...args: Parameters<typeof client.requestCompletionPermit>) => {
      const response = await permit(...args);
      deniedReason = response.denyReason;
      return response;
    });
    const forge = fakeGitlab();
    await runner(committing("Refs #3090").executor, forge.gitlab).execute(claim);
    assert.equal(api.completionPermitRequests[0]!.body.contract_revision, 1);
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(forge.calls.length, 0);
    assert.equal(terminal(claim.run_id).length, 0);
    assert.equal(deniedReason, "contract_not_frozen");
  });

  for (const mismatch of [false, true]) {
    it(`zero-attempt capped postpush ${mismatch ? "mismatch" : "denial"} fails after strict hold refusal without inventing an attempt`, async () => {
      const claim = claimFor("gitlab", []);
      api.completionPermitWire = body => ({
        granted: mismatch, deny_reason: "scope_cap_not_reached",
        ...(mismatch ? { permit: {
          id: "m3-permit", contract_revision: 2, branch: body.branch, head: body.head,
          issued_at: "2026-10-17T00:00:00Z", finding_ids: [], completion_scope: {},
        } } : {}),
      });
      const forge = fakeGitlab();
      await runner(committing("Refs #3090", { scopeCapped: { completedCount: 1, total: 2 } }).executor, forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests.length, 1, "postpush permit was reached");
      assert.equal(api.completionPermitRequests[0]!.body.scope_capped, true);
      assert.equal(api.completionAttemptRequests.length, 0);
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(forge.calls.length, 0);
      assert.equal(terminal(claim.run_id).length, 1);
      assert.equal(terminal(claim.run_id)[0]!.body.status, "failed");
      assert.match(terminal(claim.run_id)[0]!.body.failure_reason!, /completion interlock/i);
    });
  }

  it("a nonrunning ACK quietly exits the real Codex finalize boundary", async t => {
    const claim = claimFor();
    const rig = codexRig();
    const commit = committing("Fixes #3090", {}, () => {
      api.stateAckWire = (body, run) => body.status === "running"
        ? { status: 200, body: { run: { ...run, status: "paused" } } } : undefined;
    });
    const executor = new FakeCodexExecutor(rig.safety, ctx => commit.executor.run(ctx));
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor }), forge.gitlab).execute(claim);
    assert.ok(rig.boundaries.includes("finalize"), "the real safety facade entered finalize");
    assert.ok(rig.disposeBoundaries.length > 0, "post-boundary disposal executed");
    assert.ok(fs.existsSync(worktreeDirFor(IID)), "Codex quiet stop retains this flight's local source");
    assert.equal(push.mock.callCount(), 0);
    assert.equal(forge.calls.length, 0);
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(api.completionHoldRequests.length, 0);
    noPublish(claim.run_id);
  });

  for (const codex of [false, true]) {
    it(`prepush hold capture cancellation keeps the owner-cancel outcome (Codex=${codex})`, async () => {
      const claim = claimFor();
      const rig = codexRig();
      const probe = committing("Fixes #3090");
      const executor = codex ? new FakeCodexExecutor(rig.safety, ctx => probe.executor.run(ctx)) : probe.executor;
      const forge = fakeGitlab();
      const run = runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, { recoveryRetryMs: 1 });
      const capture = run["captureHoldContext"].bind(run);
      let captures = 0;
      run["captureHoldContext"] = async (...args) => {
        captures++;
        const result = await capture(...args);
        const active = (run as unknown as { activeRuns: Map<string, { steering: SteeringChannel }> }).activeRuns.get(claim.run_id)!;
        active.steering.markCancelled();
        return result;
      };
      await run.execute(claim);
      assert.equal(captures, 1, "cancel arrives during the phasePublish prepush hold capture");
      assert.equal(api.completionHoldRequests.length, 0, "known cancel never posts a hold");
      const reports = terminal(claim.run_id);
      assert.equal(reports.length, 1);
      assert.equal(reports[0]!.body.failure_reason, "run cancelled", "the API uses the owner's stored cancellation stop kind");
      assert.equal(forge.calls.length, 0);
    });
  }

  for (const codex of [false, true]) for (const abortWait of [false, true]) {
    it(`prepush hold retry cancellation releases provisional clone and HOME retention (Codex=${codex}, abortWait=${abortWait})`, async () => {
      const claim = claimFor();
      const runHome = path.join(homeDir, claim.run_id);
      fs.mkdirSync(runHome, { recursive: true });
      const sentinel = path.join(runHome, "SESSION_TRANSCRIPT");
      fs.writeFileSync(sentinel, "session bytes\n");
      const rig = codexRig();
      const probe = committing("Fixes #3090");
      const executor = codex ? new FakeCodexExecutor(rig.safety, ctx => probe.executor.run(ctx)) : probe.executor;
      const forge = fakeGitlab();
      const run = runnerWith(() => ({ executor, homeDir: runHome }), forge.gitlab);
      let captures = 0, retries = 0;
      run["captureHoldContext"] = async () => {
        captures++;
        return { verified: false, published: false, mode: "same_worker_only", head: null };
      };
      const wait = run["waitRecoveryRetry"].bind(run);
      run["waitRecoveryRetry"] = async flight => {
        retries++;
        const waiting = abortWait ? wait(flight, true, 10_000) : undefined;
        run["observeCancellation"](flight);
        assert.equal(flight.cancel.signal.aborted, true, "the hold's shared signal is aborted between capture attempts");
        await waiting;
      };
      await run.execute(claim);
      assert.equal(captures, 1);
      assert.equal(retries, 1);
      assert.equal(api.completionHoldRequests.length, 0);
      assert.equal(fs.existsSync(worktreeDirFor(IID)), false, "cancel releases this hold's provisional clone retention");
      assert.equal(fs.existsSync(sentinel), false, "cancel releases this hold's provisional HOME retention");
      assert.equal(terminal(claim.run_id).at(-1)!.body.failure_reason, "run cancelled");
      assert.equal(forge.calls.length, 0);
    });
  }

  for (const disposition of ["confirmed", "ambiguous"] as const) {
    it(`Codex prepush ${disposition} hold remains nonterminal after boundary disposal`, async () => {
      const claim = claimFor();
      if (disposition === "ambiguous")
        api.completionHoldWire.set(claim.run_id, res => { res.writeHead(200); res.end(""); });
      const rig = codexRig();
      const probe = committing("Fixes #3090");
      const executor = new FakeCodexExecutor(rig.safety, ctx => probe.executor.run(ctx));
      const forge = fakeGitlab();
      await runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, {
        recoveryRetryMs: 1, completionHoldReconcileTimeoutMs: 500,
      }).execute(claim);
      assert.ok(rig.boundaries.includes("finalize"));
      assert.ok(rig.disposeBoundaries.length > 0);
      assert.equal(api.completionAttemptRequests.length, 1);
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(forge.calls.length, 0);
      assert.ok(fs.existsSync(worktreeDirFor(IID)));
      noPublish(claim.run_id);
    });
  }

  it("generation-stripped state fallback cannot authorize publication", async t => {
    const claim = claimFor("gitlab", []);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(committing("Refs #3090", {}, () => api.failStateStrictDecodeNext(1)).executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionHoldRequests.length, 1);
    noPublish(claim.run_id);
  });

  for (const mismatch of ["different IDs", "missing scope", "different revision", "denied"] as const) {
    it(`postpush permit ${mismatch} holds before PR without a synthetic attempt`, async () => {
      const claim = claimFor();
      api.setPublicationState(claim.run_id, DEFERRED, 2, 1);
      api.requireCompletionAttempt(claim.run_id, 1);
      api.completionPermitWire = body => ({
        granted: mismatch !== "denied", deny_reason: "contract_drift",
        ...(mismatch === "denied" ? {} : { permit: {
          id: "m3-permit", contract_revision: mismatch === "different revision" ? 3 : 2,
          branch: body.branch, head: body.head, issued_at: "2026-10-17T00:00:00Z", finding_ids: [],
          ...(mismatch === "missing scope" ? {} : { completion_scope: { deferred: mismatch === "different IDs" ? [{ milestone_id: "other" }] : DEFERRED } }),
        } }),
      });
      const forge = fakeGitlab();
      await runner(committing("Refs #3090").executor, forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests[0]!.body.contract_revision, 2);
      assert.equal(api.completionAttemptRequests.length, 0);
      assert.equal(api.completionHoldRequests.length, 1);
      assert.equal(forge.calls.length, 0);
      assert.equal(terminal(claim.run_id).length, 0);
    });
  }

  for (const explicitScope of [false, true]) {
    it(`claim revision 3 with guard/grant revision 2 ${explicitScope ? "uses explicit permit scope" : "holds when permit scope is omitted"}`, async () => {
      const claim = claimFor("gitlab", [{ milestone_id: "m2", title: "Claim title must not render", reason: "Claim reason must not render" }]);
      claim.config!.contract_revision = 3;
      api.setPublicationState(claim.run_id, [{ milestone_id: "m2", title: "ACK title must not render", reason: "ACK reason must not render" }], 2, 1);
      api.requireCompletionAttempt(claim.run_id, 1);
      api.completionPermitWire = body => ({
        granted: true,
        permit: {
          id: "m3-permit", contract_revision: 2, branch: body.branch, head: body.head,
          issued_at: "2026-10-17T00:00:00Z", finding_ids: [],
          ...(explicitScope ? { completion_scope: { deferred: DEFERRED } } : {}),
        },
      });
      const forge = fakeGitlab();
      followSuccessfulPush(git, forge.pr, true);
      await runner(committing("Refs #3090").executor, forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests[0]!.body.contract_revision, 2);
      assert.equal(api.completionAttemptRequests.length, 0);
      if (explicitScope) {
        assert.ok(statuses(claim.run_id).includes("completed"));
        assert.equal(api.completionHoldRequests.length, 0);
        assert.match(forge.pr.description, /Follow up.*Owner deferred/);
        assert.doesNotMatch(forge.pr.description, /Claim title|Claim reason|ACK title|ACK reason|Closes #3090/);
      } else {
        assert.equal(api.completionHoldRequests.length, 1);
        assert.equal(forge.calls.length, 0);
        assert.equal(terminal(claim.run_id).length, 0);
      }
    });
  }

  it("live pre-freeze clean publication resolves deferred titles/reasons from the permit only", async () => {
    const claim = claimFor("gitlab", []);
    delete claim.config!.contract_revision;
    claim.issue_title = "Fixes #3090";
    const probe = committing("Refs #3090", {}, () => api.setPublicationState(claim.run_id, [{ milestone_id: "m2", title: "ACK title must not render", reason: "ACK reason must not render" }], 2));
    api.completionPermitScope = { deferred: DEFERRED };
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(forge.pr.description, /Follow up.*Owner deferred/);
    assert.doesNotMatch(forge.pr.description, /ACK title|ACK reason|Closes #3090/);
    assert.doesNotMatch(forge.pr.title, /Fixes #3090/);
  });

  it("finalize abort during synthetic attempt retry stops further attempts, push and hold", async t => {
    const claim = claimFor();
    const deadline = manualDeadline("finalize");
    const rig = codexRig({ armDeadline: deadline.armDeadline });
    api.completionAttemptHttpStatus = 503;
    const commit = committing("Fixes #3090");
    const executor = new FakeCodexExecutor(rig.safety, ctx => commit.executor.run(ctx));
    const realFetch = globalThis.fetch;
    let sends = 0;
    let aborted = false;
    t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
      if (!String(url).endsWith("/completion/attempt")) return realFetch(url, init);
      sends++;
      if (sends === 1) return realFetch(url, init);
      assert.equal(sends, 2, "no further retries after the aborted request");
      const signal = init?.signal;
      assert.ok(signal && !signal.aborted, "retry starts with a live combined signal");
      await new Promise<void>((_, reject) => {
        signal.addEventListener("abort", () => {
          aborted = true;
          reject(signal.reason);
        }, { once: true });
        deadline.fire();
      });
      throw new Error("unreachable");
    });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    const forge = fakeGitlab();
    await runnerWith(() => ({ executor }), forge.gitlab, undefined, undefined, {
      codexBoundaryDeadlineMs: 60_000,
    }).execute(claim);
    assert.equal(deadline.armed(), 1);
    assert.equal(deadline.fired(), 1);
    assert.equal(aborted, true, "the retry observed the real finalize-boundary abort");
    assert.equal(sends, 2);
    assert.equal(api.completionAttemptRequests.length, 1, "only the initial 503 reached the server");
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(forge.calls.length, 0);
    assert.ok(rig.disposeBoundaries.length > 0, "existing boundary cleanup settled");
  });

  it("interlocked GitHub partial workflow refusal precedes publication report and closing scan", async t => {
    const claim = claimFor("github");
    let checkpoint = -1;
    const probe = committing("Fixes #3090", {}, ctx => {
      fs.mkdirSync(path.join(ctx.worktreePath, ".github/workflows"), { recursive: true });
      fs.writeFileSync(path.join(ctx.worktreePath, ".github/workflows/ci.yml"), "name: ci\\non: [push]\\njobs: {}\\n");
      gitIn(ctx.worktreePath, ["add", ".github/workflows/ci.yml"]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "workflow work"]);
      checkpoint = api.states.length;
    });
    const scan = t.mock.method(git, "publishedCommitMessages", git.publishedCommitMessages.bind(git));
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    const forge = fakeGitHub();
    await runner(probe.executor, fakeGitlab().gitlab, undefined, { github: forge.github }).execute(claim);
    assert.ok(checkpoint >= 0);
    assert.equal(api.states.slice(checkpoint).filter(s => guardOnly(s.body as unknown as Record<string, unknown>)).length, 0);
    assert.equal(scan.mock.callCount(), 0, "workflow refusal precedes even the early closing helper");
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(api.completionPermitRequests.length, 0);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(forge.all.length, 0);
    assert.equal(terminal(claim.run_id).length, 1);
    assert.equal(terminal(claim.run_id)[0]!.body.fail_origin, "workflow_scope_missing");
  });

  it("ALIGN exact candidate changed by real default merge holds outside the forge retry callback", async t => {
    const claim = claimFor("github");
    const workflow = path.join(fx.originPath, ".github/workflows/ci.yml");
    fs.mkdirSync(path.dirname(workflow), { recursive: true });
    fs.writeFileSync(workflow, "name: ci\\non: [push]\\njobs: {}\\n");
    gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "seed workflow"]);
    const probe = committing("Refs #3090", {}, () => {
      fs.writeFileSync(workflow, "name: ci\\non: [pull_request]\\njobs: {}\\n");
      gitIn(fx.originPath, ["add", ".github/workflows/ci.yml"]);
      gitIn(fx.originPath, [...IDENT, "commit", "-m", "default workflow advances"]);
    });
    const realAlign = git.alignBranchWithDefault.bind(git);
    let alignedHead: string | undefined;
    const align = t.mock.method(git, "alignBranchWithDefault", async (...args: Parameters<typeof git.alignBranchWithDefault>) => {
      const result = await realAlign(...args);
      if (result === "aligned") {
        // The real alignment creates the changed candidate; model a closing message
        // introduced there, after the early scan of the executor's clean tip.
        gitIn(args[0], [...IDENT, "commit", "--amend", "-m", "Fixes #3090"]);
        alignedHead = gitIn(args[0], ["rev-parse", "HEAD"]);
      }
      return result;
    });
    const scan = git.publishedCommitMessages.bind(git);
    const scanStacks: string[] = [];
    t.mock.method(git, "publishedCommitMessages", async (...args: Parameters<typeof git.publishedCommitMessages>) => {
      scanStacks.push(asyncStack());
      return scan(...args);
    });
    const hold = client.requestCompletionHold.bind(client);
    const holdStacks: string[] = [];
    t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof client.requestCompletionHold>) => {
      holdStacks.push(asyncStack());
      return hold(...args);
    });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(probe.executor, fakeGitlab().gitlab, undefined, { github: fakeGitHub().github }).execute(claim);
    assert.ok(align.mock.callCount() > 0, "real GitHub alignment ran");
    assert.ok(scanStacks.some(stack => stack.includes("withForgeRetry")), "positive stack control sees the exact-candidate retry callback");
    assert.equal(holdStacks.length, 1);
    assert.doesNotMatch(holdStacks[0]!, /withForgeRetry/, "hold RPC runs after the retry callback unwinds");
    assert.ok(alignedHead);
    assert.equal(api.completionAttemptRequests[0]!.head, alignedHead, "hold attempt names the actual aligned candidate");
    assert.ok(JSON.stringify(api.messages(claim.run_id)).includes(alignedHead));
    assert.notEqual(alignedHead, probe.seen.head, "the aligned exact candidate changed");
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(push.mock.callCount(), 0);
    noPublish(claim.run_id);
  });

  it("exact candidate refusal unwinds push retry before making one hold", async t => {
    const claim = claimFor();
    let scans = 0;
    let retryStack = "";
    const holdStacks: string[] = [];
    const hold = client.requestCompletionHold.bind(client);
    t.mock.method(client, "requestCompletionHold", async (...args: Parameters<typeof client.requestCompletionHold>) => {
      holdStacks.push(asyncStack());
      return hold(...args);
    });
    const probe = committing("Refs #3090");
    t.mock.method(git, "publishedCommitMessages", async (_bare: string, head: string) => {
      assert.equal(api.completionHoldRequests.length, 0, "no hold inside the push closure");
      scans++;
      if (scans === 2) retryStack = asyncStack();
      return scans === 1 ? [] : [{ sha: head, message: "Fixes #3090" }];
    });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(scans, 2, "early and authoritative exact-candidate scans");
    assert.match(retryStack, /withForgeRetry/, "positive stack control detects the retry callback");
    assert.equal(holdStacks.length, 1);
    assert.doesNotMatch(holdStacks[0]!, /withForgeRetry/, "hold RPC is outside the retry callback");
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.equal(api.completionHoldRequests.length, 1);
    noPublish(claim.run_id);
  });

  it("rescans closing history already pushed on an earlier claim", async t => {
    const claim = claimFor();
    gitIn(fx.originPath, ["checkout", "-b", "agent/issue-3090"]);
    fs.writeFileSync(path.join(fx.originPath, "earlier.txt"), "earlier work");
    gitIn(fx.originPath, ["add", "earlier.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "Fixes #3090"]);
    const earlier = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(committing("Refs #3090").executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(JSON.stringify(api.messages(claim.run_id)).includes(earlier));
    assert.equal(gitIn(fx.originPath, ["rev-parse", "agent/issue-3090"]), earlier);
    noPublish(claim.run_id);
  });

  it("bridge candidate restores a closing published floor and holds outside the retry closure", async t => {
    const claim = claimFor();
    gitIn(fx.originPath, ["checkout", "-b", "agent/issue-3090"]);
    fs.writeFileSync(path.join(fx.originPath, "floor.txt"), "published work");
    gitIn(fx.originPath, ["add", "floor.txt"]);
    gitIn(fx.originPath, [...IDENT, "commit", "-m", "Fixes #3090"]);
    const floor = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    gitIn(fx.originPath, ["checkout", "main"]);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    const executor = { run: async (ctx: RunContext): Promise<ExecutorResult> => {
      fs.writeFileSync(path.join(ctx.worktreePath, "reworded.txt"), "reworded work");
      gitIn(ctx.worktreePath, ["add", "reworded.txt"]);
      gitIn(ctx.worktreePath, [...IDENT, "commit", "--amend", "-m", "Refs #3090"]);
      return { branch: ctx.branch };
    } };
    await runner(executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.notEqual(api.completionAttemptRequests[0]!.head, floor);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(JSON.stringify(api.messages(claim.run_id)).includes(floor));
    noPublish(claim.run_id);
  });

  it("oversized publication ACK never pushes or holds and reaches the bounded finalize failure", async t => {
    const claim = claimFor("gitlab", []);
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    let reads = 0;
    const realFetch = globalThis.fetch;
    const probe = committing("Refs #3090", {}, () => {
      t.mock.method(globalThis, "fetch", async (url: string | URL | Request, init?: RequestInit) => {
        if (String(url).endsWith("/state") && guardOnly(JSON.parse(String(init?.body)))) {
          reads++;
          return new Response(new ReadableStream({ start(controller) {
            controller.enqueue(Buffer.alloc(6 * 1024 * 1024 + 1, " "));
            controller.close();
          } }), { headers: { "Content-Length": "1" } });
        }
        return realFetch(url, init);
      });
    });
    await runner(probe.executor, fakeGitlab().gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.ok(reads >= 1 && reads <= 3);
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(api.completionAttemptRequests.length, 0);
    assert.equal(terminal(claim.run_id).length, 1);
    assert.equal(terminal(claim.run_id)[0]!.body.status, "failed");
  });

  it("attempt counter survives scope updates and rejected requests", async () => {
    const claim = claimFor();
    const args = { milestonesCompleted: [], head: null, worktreeFingerprint: null, claimGeneration: 7 };
    assert.equal((await client.recordCompletionAttempt(claim.run_id, args)).attemptCount, 1);
    api.setPublicationState(claim.run_id, DEFERRED, 2);
    assert.equal((await client.recordCompletionAttempt(claim.run_id, args)).attemptCount, 2);
    api.completionAttemptHttpStatus = 503;
    await assert.rejects(client.recordCompletionAttempt(claim.run_id, args));
    const ack = await client.reportState(claim.run_id, { status: "running", claim_generation: 7 });
    assert.equal(ack.completionAttempts, 2, "only successful attempts count across a scope update");
    assert.equal(api.completionAttemptRequests.length, 3, "a rejected arrival is not a recorded attempt");
    assert.equal((await client.requestCompletionHold(claim.run_id, { head: "", claimGeneration: 7 })).status, "paused");
  });

  it("attempt-aware fake refuses a hold without a successful attempt", async () => {
    const claim = claimFor();
    const ack = await client.requestCompletionHold(claim.run_id, { head: "", claimGeneration: 7 });
    assert.equal(ack.status, "running");
    api.completionAttemptHttpStatus = 503;
    await assert.rejects(client.recordCompletionAttempt(claim.run_id, { milestonesCompleted: [], head: null, worktreeFingerprint: null, claimGeneration: 7 }));
    assert.equal((await client.requestCompletionHold(claim.run_id, { head: "", claimGeneration: 7 })).status, "running");
  });
});

// PRD #1230 M4: frozen, human-authorized maintainer-pending criteria are a SEPARATE non-closing source.
// They widen the closing guard and the non-closing delivery decision, are never merged into the legacy
// deferred-milestone ids, and the permit must project exactly what the pre-push guard acted on.
describe("M4 maintainer-pending publication", () => {
  const CRIT = "00000000-0000-4000-8000-0000000000a1";
  const CRIT2 = "00000000-0000-4000-8000-0000000000a2";
  const criterion = (over: Record<string, unknown> = {}) => ({
    criterion_id: CRIT, milestone_id: "m2", kind: "maintainer", text: "Observe the hosted rollout",
    reason: "Only the owner can reach the cluster", conditions: ["The rollout reached the hosted cluster"],
    verification_steps: ["Open the dashboard and confirm the new revision"], authorization_revision: 1,
    status: "unverified", ...over,
  });
  const pending = (over: Record<string, unknown> = {}, criteria: unknown[] = [criterion()]) =>
    ({ version: 1, contract_revision: 1, criteria, ...over });
  const FEATURES = ["claim_generation_fence", "completion_non_closing_v1", "completion_semantic_publication_v1"];

  async function arrange(opts: { ack?: unknown; permit?: unknown; features?: string[]; message?: string; deferred?: typeof DEFERRED }) {
    api.setRegisterProtocolFeatures(opts.features ?? FEATURES);
    await client.register("m4-pending");
    const claim = claimFor("gitlab", opts.deferred ?? []);
    api.setCompletionPermitResponse(true);
    api.completionPermitScope = opts.deferred ? { deferred: opts.deferred } : {};
    const forge = fakeGitlab();
    followSuccessfulPush(git, forge.pr, true);
    const probe = committing(opts.message ?? "Refs #3090", {}, () => {
      api.stateAckWire = (body, run) => guardOnly(body as unknown as Record<string, unknown>) && opts.ack !== undefined
        ? { status: 200, body: { run: { ...run, completion_pending: opts.ack } } } : undefined;
      api.completionPermitWire = json => ({ granted: true, permit: {
        id: "permit-m4", contract_revision: json.contract_revision, branch: json.branch, head: json.head,
        issued_at: "2026-10-20T00:00:00Z", finding_ids: [], completion_scope: opts.deferred ? { deferred: opts.deferred } : {},
        ...(opts.permit !== undefined ? { completion_pending: opts.permit } : {}),
      } });
    });
    return { claim, forge, probe };
  }

  it("pending-only delivery is non-closing, unchecked and never touches the legacy deferred ids", async () => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending() });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify(statuses(claim.run_id)));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.equal(api.completionAttemptRequests.length, 0, "no synthetic attempt for a valid projection");
    assert.doesNotMatch(forge.pr.description, /Closes #3090/);
    assert.doesNotMatch(forge.pr.title, /\b(closes|fixes|resolves)\b/i);
    assert.match(forge.pr.description, new RegExp(`- \\[ \\] \`${CRIT}\``));
    assert.match(forge.pr.description, /Checking a box grants no completion or closure authority/);
    assert.doesNotMatch(forge.pr.description, /Implements part of|Partial delivery — owner scope decision/,
      "a pending-only run is not an owner partial: m2 is not in the legacy deferred set");
  });

  it("pending-only delivery refuses a closing commit through the early guard and holds without a push", async t => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending(), message: "Fixes #3090" });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionAttemptRequests.length, 1);
    assert.equal(api.completionHoldRequests.length, 1);
    noPublish(claim.run_id);
  });

  it("the exact-tip guard also refuses a closing commit that appears after the early scan", async t => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending() });
    const real = git.publishedCommitMessages.bind(git);
    let scans = 0;
    t.mock.method(git, "publishedCommitMessages", async (bare: string, tip: string) => {
      const messages = await real(bare, tip);
      return ++scans < 2 || messages === null ? messages : [...messages, { sha: "e".repeat(40), message: "Fixes #3090" }];
    });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(scans >= 2, "both guards scanned");
    assert.doesNotMatch(forge.pr.description, /Closes #3090/);
    assert.equal(terminal(claim.run_id).filter(s => s.body.status === "completed").length, 0);
  });

  it("an api that sent pending criteria without advertising the semantic publication feature holds before push", async t => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending(),
      features: ["claim_generation_fence", "completion_non_closing_v1"] });
    const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.equal(push.mock.callCount(), 0);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(terminal(claim.run_id).length, 0, "a hold, never a failure");
    noPublish(claim.run_id);
  });

  for (const [name, bad] of [
    ["unknown field", pending({ extra: 1 })],
    ["wrong version", pending({ version: 2 })],
    ["null", null],
    ["criteria not an array", pending({ criteria: {} })],
    ["non-maintainer kind", pending({}, [criterion({ kind: "code" })])],
    ["status satisfied", pending({}, [criterion({ status: "verified" })])],
    ["authorization revision zero", pending({}, [criterion({ authorization_revision: 0 })])],
    ["bad criterion id", pending({}, [criterion({ criterion_id: "not-a-uuid" })])],
    ["empty conditions", pending({}, [criterion({ conditions: [] })])],
    ["oversized text", pending({}, [criterion({ text: "x".repeat(2049) })])],
    ["duplicate ids", pending({}, [criterion(), criterion()])],
  ] as const) {
    it(`malformed ACK projection (${name}) holds before push instead of degrading to legacy`, async t => {
      const { claim, forge, probe } = await arrange({ ack: bad, permit: pending() });
      const push = t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      await runner(probe.executor, forge.gitlab).execute(claim);
      assert.equal(push.mock.callCount(), 0);
      assert.equal(api.completionHoldRequests.length, 1);
      noPublish(claim.run_id);
    });
  }

  for (const [name, permit] of [
    ["absent on the permit", undefined],
    ["a different authorization revision only", pending({}, [criterion({ authorization_revision: 2 })])],
    ["a different contract revision", pending({ contract_revision: 2 })],
    ["an extra criterion", pending({}, [criterion(), criterion({ criterion_id: CRIT2 })])],
    ["a different criterion id", pending({}, [criterion({ criterion_id: CRIT2 })])],
    ["a malformed permit projection", { version: 1 }],
  ] as const) {
    it(`permit with ${name} holds instead of delivering`, async () => {
      const { claim, forge, probe } = await arrange({ ack: pending(), permit });
      await runner(probe.executor, forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests.length, 1);
      assert.equal(api.completionHoldRequests.length, 1, JSON.stringify(statuses(claim.run_id)));
      assert.equal(statuses(claim.run_id).includes("completed"), false);
      assert.equal(forge.pr.description.includes("Observe the hosted rollout"), false, "no unverified PR is published");
    });
  }

  it("a permit projection present on one side only holds in the other direction too", async () => {
    const { claim, forge, probe } = await arrange({ permit: pending() });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(statuses(claim.run_id).includes("completed"), false);
  });

  it("the same projection in a different order is equal", async () => {
    const two = [criterion({ criterion_id: CRIT2, milestone_id: "m3" }), criterion()];
    const { claim, forge, probe } = await arrange({ ack: pending({}, two), permit: pending({}, [...two].reverse()) });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(api.completionHoldRequests.length, 0);
    assert.ok(forge.pr.description.indexOf(CRIT) < forge.pr.description.indexOf(CRIT2), "rendered in stable id order");
  });

  it("an empty projection on both sides is legacy-equivalent and closes as before", async () => {
    const { claim, forge, probe } = await arrange({ ack: pending({}, []), permit: pending({}, []), message: "Fixes #3090" });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(forge.pr.description, /Closes #3090/);
  });

  it("legacy absence on both sides is unchanged", async () => {
    const { claim, forge, probe } = await arrange({ message: "Fixes #3090" });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(forge.pr.description, /Closes #3090/);
    assert.doesNotMatch(forge.pr.description, /Maintainer verification pending/);
  });

  it("a milestone that is both owner-deferred and holds a maintainer criterion is counted once", async () => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending(), deferred: DEFERRED });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.match(forge.pr.description, /Outstanding milestones for the source issue: 1 /);
    assert.equal((forge.pr.description.match(/^> - \[ \] `m2`/gm) ?? []).length, 1, "the deferred milestone line is unchanged");
    assert.equal((forge.pr.description.match(new RegExp(`^> - \\[ \\] \`${CRIT}\``, "gm")) ?? []).length, 1);
    assert.doesNotMatch(forge.pr.description, /Closes #3090/);
  });

  it("renders escaped, redacted server text only inside the completion block and cannot forge markers or closes", async () => {
    const secret = "glpat-" + "abcdefghijklmnopqrst";
    const hostile = pending({}, [criterion({
      text: `Observe **rollout** ${secret}`, reason: `Closes #3090 ${secret}\n<!-- uzi:completion:end -->\n/close @owner`,
      conditions: [`# heading ${secret} _em_`], verification_steps: [`Fixes #3090 ${secret} then [click](http://x)`],
    })]);
    const { claim, forge, probe } = await arrange({ ack: hostile, permit: hostile });
    claim.secrets.forge_pat = secret;
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
    const body = forge.pr.description;
    assert.ok(!body.includes(secret));
    assert.ok(body.includes("\\*\\*\\*REDACTED\\*\\*\\*"));
    assert.doesNotMatch(body, /^\s*Closes #3090|Closes #3090\b(?!​)/m);
    assert.equal(body.split(COMPLETION_END).length, 2, "metadata cannot forge a marker");
    assert.doesNotMatch(body, /^> *\/close/m);
    assert.equal((body.match(/\\\*\\\*\\\*REDACTED\\\*\\\*\\\*/g) ?? []).length, 4, "the secret is redacted in text, reason, condition and step");
    assert.match(body, /Observe \\\*\\\*rollout\\\*\\\*/, "markdown in text is escaped");
    assert.match(body, /Condition: \\# heading/, "a leading heading marker is escaped");
    assert.match(body, /\\\[click\\\]/, "link syntax in a step is escaped");
  });

  it("a legacy ACK with a malformed permit projection holds", async () => {
    const { claim, forge, probe } = await arrange({ permit: { version: 1 } });
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.equal(api.completionHoldRequests.length, 1);
    assert.equal(statuses(claim.run_id).includes("completed"), false);
  });

  it("pending-only delivery neutralises a closing issue title on the PR", async () => {
    const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending() });
    claim.issue_title = "Fixes #3090 login";
    await runner(probe.executor, forge.gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify(statuses(claim.run_id)));
    assert.doesNotMatch(forge.pr.title, /\b(closes|fixes|resolves)\b/i, forge.pr.title);
  });

  for (const refuse of [false, true]) {
    it(`pending-only delivery ${refuse ? "fails closed when" : "strips a model-authored closing directive even if"} the body write ${refuse ? "is refused" : "succeeds"}`, async () => {
      const { claim, forge, probe } = await arrange({ ack: pending(), permit: pending() });
      const adopted = fakeGitlab({ head: "1111111111111111111111111111111111111111", existing: "Model notes. closes #3090",
        ...(refuse ? { putStatus: 403 } : {}) });
      followSuccessfulPush(git, adopted.pr, true);
      void forge;
      await runner(probe.executor, adopted.gitlab).execute(claim);
      if (!refuse) assert.equal(closingDirectiveFor(adopted.pr.description, IID, "org/repo"), false, adopted.pr.description);
      if (refuse) assert.equal(statuses(claim.run_id).includes("completed"), false, "never completes while the PR still closes");
      else assert.ok(statuses(claim.run_id).includes("completed"), JSON.stringify(statuses(claim.run_id)));
    });
  }

  for (const carried of [true, false]) {
    it(`capped zero-diff delivery ${carried ? "holds fail-closed when its permit carries pending criteria" : "completes when the permit projection is empty"}`, async t => {
      api.setRegisterProtocolFeatures(FEATURES);
      await client.register("m4-capped");
      const claim = claimFor("gitlab", []);
      api.setCompletionPermitResponse(true);
      api.completionPermitScope = {};
      api.completionPermitWire = json => ({ granted: true, permit: {
        id: "permit-capped", contract_revision: json.contract_revision, branch: json.branch, head: json.head,
        issued_at: "2026-10-20T00:00:00Z", finding_ids: [], completion_scope: {},
        completion_pending: pending({}, carried ? [criterion()] : []),
      } });
      // A scope-capped zero-diff run requests its permit before any publication ACK exists.
      const executor = { run: async (ctx: RunContext): Promise<ExecutorResult> => ({
        branch: ctx.branch, scopeCapped: { completedCount: 0, total: 2 }, summary: "Nothing landed" }) };
      const forge = fakeGitlab();
      t.mock.method(git, "pushBranch", async () => { throw new Error("unexpected push"); });
      await runner(executor, forge.gitlab).execute(claim);
      assert.equal(api.completionPermitRequests.length, 1);
      assert.equal(api.completionPermitRequests[0]!.body.scope_capped, true, "the capped-empty permit request");
      assert.equal(api.completionHoldRequests.length, carried ? 1 : 0, JSON.stringify(statuses(claim.run_id)));
      assert.equal(statuses(claim.run_id).includes("completed"), !carried);
    });
  }
});
