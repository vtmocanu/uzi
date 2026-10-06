import { followSuccessfulPush } from "./publication-fixture.js";
import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import { nullLogger, makeClaim } from "./helpers.js";
import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import { computeSize } from "../src/pr-size.js";
import {
  COMPLETION_END,
  COMPLETION_START,
  REGION_END,
  REGION_START,
  closingDirectiveFor,
  parseOwnedBlocks,
} from "../src/pr-description.js";
import type { ClaimResponse } from "../src/protocol.js";
import { FakePrDescApi } from "./fake-pr-desc-api.js";
import { api, fakeGitlab as rawFakeGitlab, fx, git, gitlabClaim, installHarness, runner, runnerWith, type FakeForgeOpts } from "./runner-harness.js";

installHarness();

// PRD #1798 M6 (M6b): the description publisher and the completion interlock, end to end through the
// REAL RunRunner.execute() (StubExecutor commits real work, phasePublish pushes it), against a fake
// forge whose single PR keeps its description across reads and writes.

function fakeGitlab(opts: Parameters<typeof rawFakeGitlab>[0] = {}) {
  const forge = rawFakeGitlab(opts);
  followSuccessfulPush(git, forge.pr, opts.head === undefined || opts.head === H);
  return forge;
}

const H = "1111111111111111111111111111111111111111";
const OTHER = "2222222222222222222222222222222222222222";

function interlockedClaim(iid: number, scope?: { deferred: { milestone_id: string; title: string; reason: string }[] }) {
  return gitlabClaim(iid, {
    config: { completion_contract_version: 1, contract_revision: 1, ...(scope ? { completion_scope: scope } : {}) },
  });
}

function statuses(runId: string): string[] {
  return api.states.filter((s) => s.runId === runId).map((s) => s.body.status);
}

const SIZE_REGION = [REGION_START, "**Size:** code +1 −0 · 1 file", REGION_END].join("\n");

/** An adopted PR's body: uzi's (older) blocks with human text around them. */
function adoptedBody(iid: number, humanText: string): string {
  const completion = [COMPLETION_START, `Related to #${iid}.`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
  return `${humanText}\n\n${SIZE_REGION}\n\n${completion}\n\nA review bot's summary.`;
}

describe("RunRunner — completion interlock over a preserved body (PRD #1798 M6, amended D10)", () => {
  it("hold path: a human-typed `Closes #N` outside uzi's blocks forces a whole-body non-closing rewrite, then holds", async () => {
    const { gitlab, pr } = fakeGitlab({ head: OTHER, existing: adoptedBody(1820, "Closes #1820 when merged, please.") });
    const claim = interlockedClaim(1820);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 1, "the run holds");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the rewrite landed, so it holds rather than fails");
    assert.equal(closingDirectiveFor(pr.description, 1820, "org/repo"), false, "no closing directive anywhere in the body");
    assert.match(pr.description, /Completion unverified/);
    assert.ok(!pr.description.includes("A review bot's summary."), "preservation yielded to the interlock");
  });

  it("hold path: when that non-closing rewrite cannot be written, the run fails CLOSED", async () => {
    const { gitlab } = fakeGitlab({ head: OTHER, putStatus: 403, existing: adoptedBody(1821, "Fixes #1821") });
    const claim = interlockedClaim(1821);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 0, "never holds a possibly-closing MR");
    assert.ok(statuses(claim.run_id).includes("failed"), "fails closed");
    assert.ok(!statuses(claim.run_id).includes("completed"));
  });

  it("hold path: human text without a directive is preserved while the completion block turns non-closing", async () => {
    const existing = adoptedBody(1822, "Notes from the maintainer.");
    const { gitlab, pr } = fakeGitlab({ head: OTHER, existing });
    const claim = interlockedClaim(1822);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.equal(api.completionHoldRequests.length, 1);
    assert.ok(pr.description.startsWith("Notes from the maintainer.\n\n"), "outside text is kept byte for byte");
    assert.ok(pr.description.endsWith("\n\nA review bot's summary."));
    assert.match(pr.description, /Completion unverified/);
  });

  it("owner partial on a verified head: a human `Closes #N` forces a non-closing rewrite, and the run completes non-closing", async () => {
    const { gitlab, pr } = fakeGitlab({ head: H, existing: adoptedBody(1823, "Resolves #1823") });
    const claim = interlockedClaim(1823, { deferred: [{ milestone_id: "m2", title: "Second", reason: "later" }] });
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "a verified partial completes");
    assert.equal(closingDirectiveFor(pr.description, 1823, "org/repo"), false, "the partial PR never closes");
    assert.match(pr.description, /owner scope decision/);
  });

  it("verified head: malformed completion markers are rewritten whole, and the completion carries Closes", async () => {
    const broken = `${SIZE_REGION}\n\n${COMPLETION_START}\nRelated to #1824.\n${COMPLETION_START}\n${COMPLETION_END}`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing: broken });
    const claim = interlockedClaim(1824);
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const parsed = parseOwnedBlocks(pr.description);
    assert.equal(parsed.kind, "ok");
    assert.match(parsed.kind === "ok" ? (parsed.completion ?? "") : "", /Closes #1824/, "a completed run's PR carries Closes");
  });

  it("verified head: the publisher's first read fails, and no blind rewrite destroys the human text (the interlock owns it)", async () => {
    const existing = adoptedBody(1826, "Notes from the maintainer.");
    let gets = 0;
    const { gitlab, pr, calls } = fakeGitlab({
      head: H,
      existing,
      // ONLY the publisher's read 1 is refused; every later read (the interlock's) succeeds.
      intercept: (req) => (req.method === "GET" && ++gets === 1 ? { status: 403, text: async () => "{}" } : undefined),
    });
    const claim = interlockedClaim(1826);
    api.setCompletionPermitResponse(true);


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.ok(pr.description.startsWith("Notes from the maintainer.\n\n"), "the human text survives");
    assert.ok(pr.description.endsWith("\n\nA review bot's summary."));
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "one write: the interlock's Closes add, no blind rewrite");
    assert.match(pr.description, /Closes #1826/);
  });

  it("verified head: when the Closes write cannot be confirmed (nor the strip), the run fails closed and never reports completed", async () => {
    const { gitlab } = fakeGitlab({ head: H, putStatus: 403, existing: "Legacy body with no markers." });
    const claim = interlockedClaim(1825);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    // ADR 1225: a Closes add that may have landed is stripped before any hold; the strip is refused
    // too (403), so the MR cannot be proven non-closing and the run fails closed.
    assert.equal(api.completionHoldRequests.length, 0, "never holds a possibly-closing MR");
    assert.ok(statuses(claim.run_id).includes("failed"));
  });
});

describe("RunRunner — the description publisher (PRD #1798 M6)", () => {
  it("stages, binds and acks the new PR's description through the api, and the body carries uzi's region", async () => {
    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, pr } = fakeGitlab();
    const claim = gitlabClaim(1830);
    // The fake answers the landed head, so read 1 matches the staged snapshot.


    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const ops = prApi.calls.map((c) => c.op);
    assert.deepEqual(ops.filter((o) => o === "stage").length, 1);
    assert.equal(prApi.calls.find((c) => c.op === "stage")!.body.source, "deterministic_only", "no editor pass, no lead claims");
    assert.deepEqual(prApi.acks(), ["published"]);
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok" && parsed.region?.startsWith(REGION_START));
    assert.match(pr.description, /Closes #1830/, "a legacy issue run keeps Closes at creation");
  });

  it("a publisher failure is advisory: an api without the routes still completes the run", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(1831);
    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });
});

describe("RunRunner — refresh runs (PRD #1798 D17)", () => {
  function mrReworkClaim(): ClaimResponse {
    return makeClaim({
      run_id: randomUUID(),
      kind: "mr_rework",
      issue_iid: null,
      issue_title: "Rework: address MR review",
      issue_description: "Apply the reviewer's feedback.",
      branch: "agent/issue-42",
      base_branch: "main",
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath },
      last_seq: 0,
      secrets: { forge_pat: "fixture-forge-pat-000000", anthropic_oauth_token: "dummy-oauth-do-not-scan" },
    });
  }

  it("mr_rework leaves a legacy PR without uzi markers untouched", async () => {
    const legacy = "A human-written PR body.\n\nCloses #42";
    const { gitlab, calls, pr } = fakeGitlab({ head: H, existing: legacy });

    await runner(new StubExecutor(nullLogger()), gitlab).execute(mrReworkClaim());
    assert.ok(!calls.some((c) => c.method === "PUT"), "no body write");
    assert.equal(pr.description, legacy);
  });

  it("mr_rework on a marked PR refreshes the region and keeps the completion block's Closes", async () => {
    const completion = [COMPLETION_START, "Related to #42.", "", "Closes #42", "", "---", "Opened by uzi.", COMPLETION_END].join("\n");
    const existing = `Intro.\n\n${SIZE_REGION}\n\n${completion}\n\nBot text.`;
    const { gitlab, pr } = fakeGitlab({ head: H, existing });

    await runner(new StubExecutor(nullLogger()), gitlab).execute(mrReworkClaim());
    const parsed = parseOwnedBlocks(pr.description);
    assert.ok(parsed.kind === "ok");
    assert.equal(parsed.kind === "ok" && parsed.completion, completion, "the completion block is untouched");
    assert.notEqual(parsed.kind === "ok" && parsed.region, SIZE_REGION, "the region was refreshed");
    assert.ok(parsed.kind === "ok" && parsed.region?.includes(`Describes \`${pr.head.slice(0, 7)}\` against \`main\`.`));
    assert.ok(pr.description.startsWith("Intro.\n\n") && pr.description.endsWith("\n\nBot text."));
  });
});

describe("RunRunner — the verified-head Closes add is bound to what was written (ADR 1225, PRD #1798 M6)", () => {
  /** The fake PR answers the REAL landed head, so the publisher stages, binds and acks a version and
   *  the completion block carries a described SHA: the production configuration. */
  function followLandedHead(pr: { head: string }): void {
    followSuccessfulPush(git, pr);
  }

  it("PROBE-A: the head moves when `Closes #1901` is written → Closes is stripped and the run holds", async () => {
    api.prDescription = new FakePrDescApi();
    const { gitlab, pr } = fakeGitlab({
      onWrite: (p, description) => {
        if (/Closes #1901/.test(description)) p.head = OTHER;
      },
    });
    followLandedHead(pr);
    const claim = interlockedClaim(1901);
    api.setCompletionPermitResponse(true);

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "never completes on a moved head");
    assert.doesNotMatch(pr.description, /Closes #1901/, "no Closes is left on the MR");
    assert.equal(closingDirectiveFor(pr.description, 1901, "org/repo"), false);
    // The strip is readable and confirmed, so the run holds (exactly once) and does not fail.
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"), "the strip landed, so it holds rather than fails");
    assert.match(pr.description, /Completion unverified/);
  });

  it("PROBE-B: the MR becomes unreadable (404) after the Closes write → Closes is stripped and the run holds", async () => {
    api.prDescription = new FakePrDescApi();
    const opts: FakeForgeOpts = {};
    opts.onWrite = (_p, description) => {
      if (/Closes #1902/.test(description)) opts.headStatus = 404;
    };
    const { gitlab, pr } = fakeGitlab(opts);
    followLandedHead(pr);
    const claim = interlockedClaim(1902);
    api.setCompletionPermitResponse(true);

    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "never completes on an unreadable MR");
    assert.doesNotMatch(pr.description, /Closes #1902/, "no Closes is left on the MR");
    assert.equal(closingDirectiveFor(pr.description, 1902, "org/repo"), false);
    // The strip's read fails, so it falls back to the blind non-closing rewrite (which lands): the
    // run holds exactly once and does not fail.
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.match(pr.description, /Completion unverified/);
  });
});

/** The fake PR follows the REAL landed head (the publisher then stages real snapshots). Returns the
 *  landed head once the run read it. */
function followHead(pr: { head: string }): () => string {
  return followSuccessfulPush(git, pr);
}

/** A StubExecutor run stopped by an operator scope directive after committing work (PRD #634). */
class ScopeCappedStub extends StubExecutor {
  override async run(ctx: RunContext): Promise<ExecutorResult> {
    return { ...(await super.run(ctx)), scopeCapped: { completedCount: 1, total: 2 } };
  }
}

describe("RunRunner — a legacy non-closing run whose PR still closes fails closed (PRD #1798 M6, amended D10, M1)", () => {
  const closingCompletion = (iid: number) =>
    [COMPLETION_START, `Related to #${iid}.`, "", `Closes #${iid}`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");

  it("an adopted body with `Closes #N` and a failing forge write: the scope-capped legacy run fails, never completes", async () => {
    const { gitlab, pr } = fakeGitlab({ putStatus: 403, existing: `${SIZE_REGION}\n\n${closingCompletion(1840)}` });
    followHead(pr);
    const claim = gitlabClaim(1840); // no config: a legacy (non-interlocked) run

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "a partial never completes with a closing PR");
    assert.ok(statuses(claim.run_id).includes("failed"), "it fails closed");
    const failed = api.states.find((st) => st.runId === claim.run_id && st.body.status === "failed")!.body;
    assert.equal(failed.failure_reason, "completion interlock: a closing directive could not be removed from a non-closing merge request");
    assert.match(pr.description, /Closes #1840/, "the write really failed (the directive is still there)");
  });

  it("control: the same adopted body with a working forge is rewritten non-closing and the run completes", async () => {
    const { gitlab, pr } = fakeGitlab({ existing: `Closes #1841 too\n\n${SIZE_REGION}\n\n${closingCompletion(1841)}` });
    followHead(pr);
    const claim = gitlabClaim(1841);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.description, 1841, "org/repo"), false);
  });
});

describe("RunRunner — the publication and the interlock are independent (PRD #1798 M6, L1)", () => {
  it("a stale_claim at staging stops the publisher's writes; the interlock still adds Closes on the verified head", async () => {
    const prApi = new FakePrDescApi();
    prApi.failNext("stage", 409, "stale_claim");
    api.prDescription = prApi;
    const { gitlab, pr, all } = fakeGitlab();
    followHead(pr);
    const claim = interlockedClaim(1850);
    api.setCompletionPermitResponse(true);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.deepEqual(prApi.calls.map((c) => c.op), ["stage"], "no api call after the 409");
    const puts = all.filter((c) => c.method === "PUT");
    assert.equal(puts.length, 1, "the only body write is the interlock's");
    assert.match(String(JSON.parse(puts[0]!.body ?? "{}").description), /Closes #1850/);
    assert.ok(statuses(claim.run_id).includes("completed"));
  });
});

describe("RunRunner — an over-cap MR read (PRD #1798 M6, H1)", () => {
  it("the publisher skips the region, the read is not retried, and a legacy run still completes", async () => {
    const { gitlab, pr, reads, calls } = fakeGitlab({ existing: "x".repeat(7 * 1_048_576) });
    followHead(pr);
    const claim = gitlabClaim(1860);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(reads.length, 1, "a response-too-large read is permanent: one GET, no retry");
    assert.ok(!calls.some((c) => c.method === "PUT"), "nothing is written over an unreadable PR");
  });
});

describe("RunRunner — the size line counts the PR's actual target, not the repo default (Greptile #1813)", () => {
  it("an adopted PR targeting `release` gets the size of the branch against `release`", async () => {
    // origin: `release` at the first commit; main gains a commit the agent branch inherits. Against
    // main the branch changes the stub's file only; against release it also carries main's commit.
    const env = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
    const og = (args: string[]) => execFileSync("git", ["-C", fx.originPath, ...args], { env, stdio: "pipe" });
    og(["branch", "release"]);
    execFileSync("sh", ["-c", 'printf "one\\ntwo\\nthree\\n" > docs-extra.md'], { cwd: fx.originPath });
    og(["add", "docs-extra.md"]);
    og(["commit", "-m", "main moves on"]);

    const prApi = new FakePrDescApi();
    api.prDescription = prApi;
    const { gitlab, pr } = fakeGitlab({ target: "release" });
    const landed = followHead(pr);
    const claim = gitlabClaim(1870, {
      repo: { id: "r1", url: "https://gitlab.example.test/org/repo", clone_url: fx.originPath, default_branch: "main" },
    });

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"));
    const bare = git.barePathFor(fx.originPath);
    const vsRelease = (await computeSize(git, bare, "release", landed())).line;
    const vsMain = (await computeSize(git, bare, "main", landed())).line;
    assert.ok(vsRelease && vsMain && vsRelease !== vsMain, `${vsRelease} / ${vsMain}`);
    const parsed = parseOwnedBlocks(pr.description);
    const region = parsed.kind === "ok" ? (parsed.region ?? "") : "";
    assert.ok(region.includes(vsRelease!), `the size line is against the PR's target:\n${region}`);
    assert.ok(!region.includes(vsMain!), region);
    assert.match(region, /against `release`\./);
    const stages = prApi.calls.filter((c) => c.op === "stage").map((c) => c.body.target_branch);
    assert.equal(stages.at(-1), "release", "the version the PR carries was staged for its real target");
    assert.deepEqual(prApi.acks().at(-1), "published");
  });
});

// PRD #1798 M6: the verified-head path of an INTERLOCKED non-closing delivery (an owner partial, a
// scope cap). The head is verified, so the reconcile writes the partial's non-closing block; its two
// failure shapes take different arms. A failed MR read falls back to the blind non-closing rewrite
// (`blind`): nothing confirmed the completion, so the run holds and never completes. A refused write
// leaves the MR unconfirmed (it may still carry an adopted closing block), so the strip runs first,
// and when the strip is refused too the run fails closed with no hold.
describe("RunRunner — an interlocked non-closing delivery on a verified head (PRD #1798 M6)", () => {
  const OWNER_PARTIAL = { deferred: [{ milestone_id: "m2", title: "Second", reason: "later" }] };
  const arms: Array<[string, (iid: number) => ClaimResponse, () => StubExecutor]> = [
    ["owner partial", (iid) => interlockedClaim(iid, OWNER_PARTIAL), () => new StubExecutor(nullLogger())],
    ["scope cap", (iid) => interlockedClaim(iid), () => new ScopeCappedStub(nullLogger())],
  ];
  let iid = 1910;
  for (const [name, makeClaim_, makeExecutor] of arms) {
    it(`${name}: the reconcile's MR read fails (403) → the run HOLDS and never reports completed`, async () => {
      const n = iid++;
      const opts: FakeForgeOpts = { head: H, existing: adoptedBody(n, "Notes from the maintainer.") };
      const { gitlab, pr, all } = fakeGitlab(opts);
      // The head reads keep verifying H; every MR DETAIL read after the first head read (the
      // reconcile's) is answered 403 by the transport. The head reads are left working so a run that
      // skipped the `blind` arm would re-verify the head and complete (the mutation this gates).
      let verified = false;
      let refused = 0;
      const readHead = gitlab.getMergeRequestHead.bind(gitlab);
      gitlab.getMergeRequestHead = async (...args: Parameters<typeof readHead>) => {
        const head = await readHead(...args);
        verified = true;
        return head;
      };
      const readDetail = gitlab.getMergeRequest.bind(gitlab);
      gitlab.getMergeRequest = async (...args: Parameters<typeof readDetail>) => {
        if (!verified) return readDetail(...args);
        refused++;
        opts.headStatus = 403;
        try {
          return await readDetail(...args);
        } finally {
          opts.headStatus = undefined;
        }
      };
      const claim = makeClaim_(n);
      api.setCompletionPermitResponse(true);


      await runnerWith(() => ({ executor: makeExecutor() }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      assert.ok(refused >= 1, "the reconcile's read was refused after the head was verified");
      assert.ok(!statuses(claim.run_id).includes("completed"), "an unconfirmed partial never completes");
      assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
      assert.ok(!statuses(claim.run_id).includes("failed"), "the blind rewrite landed, so it holds rather than fails");
      assert.equal(closingDirectiveFor(pr.description, n, "org/repo"), false);
      assert.ok(!pr.description.includes("Notes from the maintainer."), "the blind rewrite replaced the body whole");
      assert.equal(all.at(-1)?.method, "PUT", "the last forge call is the blind rewrite");
    });

    it(`${name}: body writes are refused (403) → the run FAILS closed with no hold`, async () => {
      const n = iid++;
      const { gitlab, pr } = fakeGitlab({ head: H, putStatus: 403, existing: adoptedBody(n, "Notes from the maintainer.") });
      const claim = makeClaim_(n);
      api.setCompletionPermitResponse(true);


      await runnerWith(() => ({ executor: makeExecutor() }), gitlab, undefined, undefined, { recoveryRetryMs: 1 }).execute(claim);

      assert.ok(!statuses(claim.run_id).includes("completed"));
      assert.equal(api.completionHoldRequests.length, 0, "never holds an MR the strip could not confirm");
      assert.ok(statuses(claim.run_id).includes("failed"), "fails closed");
      assert.equal(pr.description, adoptedBody(n, "Notes from the maintainer."), "every write really was refused");
    });
  }
});

/** A real streamed Response of `total` filler bytes (a body over a byte cap). */
function streamedResponse(status: number, total: number): Response {
  const MiB = 1_048_576;
  let sent = 0;
  const chunk = new Uint8Array(MiB).fill(0x61);
  return new Response(
    new ReadableStream<Uint8Array>({
      pull(ctl) {
        if (sent >= total) return ctl.close();
        sent += chunk.byteLength;
        ctl.enqueue(chunk);
      },
    }),
    { status },
  );
}

describe("RunRunner — over-cap forge answers are permanent and treated as unreadable (PRD #1798 M6, H1)", () => {
  it("an over-cap create (201) answer fails the run after ONE POST: never retried", async () => {
    const { gitlab, calls } = fakeGitlab({
      intercept: (req) => (req.method === "POST" ? streamedResponse(201, 16 * 1_048_576) : undefined),
    });
    const claim = gitlabClaim(1920);

    await runner(new StubExecutor(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    assert.ok(statuses(claim.run_id).includes("failed"));
    assert.equal(calls.filter((c) => c.method === "POST").length, 1, "a response-too-large create is not retried");
  });

  it("an interlocked run whose head read is over the cap takes the unreadable-head path: strip, then hold", async () => {
    const { gitlab, pr } = fakeGitlab({
      head: H,
      existing: adoptedBody(1921, "Closes #1921 please"),
      intercept: (req) => (req.method === "GET" ? streamedResponse(200, 16 * 1_048_576) : undefined),
    });
    const claim = interlockedClaim(1921);
    api.setCompletionPermitResponse(true);


    await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, undefined, {
      recoveryRetryMs: 1,
    }).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"), "an unreadable head never completes");
    assert.equal(api.completionHoldRequests.length, 1, "exactly one hold");
    assert.ok(!statuses(claim.run_id).includes("failed"));
    assert.equal(closingDirectiveFor(pr.description, 1921, "org/repo"), false, "the adopted directive was stripped (blind)");
    assert.match(pr.description, /Completion unverified/);
  });
});

// PRD #1798 M6 (amended D10, M1): a legacy non-closing run whose publisher NEVER read the PR must not
// complete on the strength of having observed nothing. It falls back to the blind whole-body
// non-closing rewrite; only when that is refused too does the run fail closed.
describe("RunRunner — a legacy non-closing run whose PR is never readable (PRD #1798 M6, amended D10)", () => {
  const closingCompletion = (iid: number) =>
    [COMPLETION_START, `Related to #${iid}.`, "", `Closes #${iid}`, "", "---", "Opened by uzi.", COMPLETION_END].join("\n");

  it("every GET is refused (403): the blind non-closing rewrite lands and the scope-capped run completes non-closing", async () => {
    const existing = `${SIZE_REGION}\n\n${closingCompletion(1930)}`;
    const { gitlab, pr, calls } = fakeGitlab({ headStatus: 403, existing });
    const claim = gitlabClaim(1930);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(statuses(claim.run_id).includes("completed"), "the blind rewrite removed the directive");
    assert.equal(closingDirectiveFor(pr.description, 1930, "org/repo"), false);
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "one blind write");
  });

  it("every GET and every PUT is refused (403): the run fails closed and never completes", async () => {
    const existing = `${SIZE_REGION}\n\n${closingCompletion(1931)}`;
    const { gitlab, pr } = fakeGitlab({ headStatus: 403, putStatus: 403, existing });
    const claim = gitlabClaim(1931);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.ok(!statuses(claim.run_id).includes("completed"));
    const failed = api.states.find((st) => st.runId === claim.run_id && st.body.status === "failed")?.body;
    assert.equal(failed?.failure_reason, "completion interlock: a closing directive could not be removed from a non-closing merge request");
    assert.equal(pr.description, existing, "the write really was refused");
  });

  it("an over-cap body: the read is refused, and the blind rewrite path runs (the run completes non-closing)", async () => {
    const existing = `Closes #1932\n\n${"x".repeat(7 * 1_048_576)}\n\n${SIZE_REGION}\n\n${closingCompletion(1932)}`;
    const { gitlab, pr, reads, calls } = fakeGitlab({ existing });
    followHead(pr);
    const claim = gitlabClaim(1932);

    await runner(new ScopeCappedStub(nullLogger()), gitlab).execute(claim);

    assert.equal(reads.length, 1, "the over-cap read is permanent: not retried");
    assert.equal(calls.filter((c) => c.method === "PUT").length, 1, "the blind rewrite");
    assert.ok(statuses(claim.run_id).includes("completed"));
    assert.equal(closingDirectiveFor(pr.description, 1932, "org/repo"), false);
    assert.ok(pr.description.length < 1_048_576, "the body was rewritten whole");
  });
});
