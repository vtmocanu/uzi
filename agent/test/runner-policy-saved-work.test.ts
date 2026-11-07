import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { GitCache } from "../src/git.js";
import type { ClaimResponse, PolicyHoldRequest, StateRequest } from "../src/protocol.js";
import type { RunContext } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, input, runnerWith } from "./runner-harness.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Saved-work admission (issue #3607): a claim that is NOT preserve-only but carries policy_saved_work_head
// (an owner fresh_from_saved_work Resume while a required head remains) must prove that head is present in
// the recovered clone before ANY model work; otherwise it re-holds as saved_work_missing. The #3153
// runner-shared replacement regression below runs the same predicate over the production GitCache layout.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const MISSING_HEAD = "ab".repeat(20);
const CODEX = { forge_pat: "fixture-pat", codex: { auth_mode: "api_key" as const, access_token: "fixture-access" } };

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

function originBranch(branch: string, parent: string, file: string, content: string): string {
  const idx = path.join(fx.originPath, ".git", `tmp-index-${branch.replace(/[^a-zA-Z0-9]/g, "_")}`);
  const env = { ...GIT_ENV, GIT_INDEX_FILE: idx };
  execFileSync("git", ["-C", fx.originPath, "read-tree", parent], { env });
  const blob = execFileSync("git", ["-C", fx.originPath, "hash-object", "-w", "--stdin"],
    { env: GIT_ENV, input: content, encoding: "utf8" }).trim();
  execFileSync("git", ["-C", fx.originPath, "update-index", "--add", "--cacheinfo", `100644,${blob},${file}`], { env });
  const tree = execFileSync("git", ["-C", fx.originPath, "write-tree"], { env, encoding: "utf8" }).trim();
  const sha = execFileSync("git", ["-C", fx.originPath, ...IDENT, "commit-tree", tree, "-p", parent, "-m", `origin ${file}`],
    { env: GIT_ENV, encoding: "utf8" }).trim();
  gitIn(fx.originPath, ["update-ref", `refs/heads/${branch}`, sha]);
  fs.rmSync(idx, { force: true });
  return sha;
}

describe("saved-work admission for a non-preserve-only claim", () => {
  installHarness();
  const realInspect = CodexSessionStore.inspectSession;
  let events: string[];
  let holds: PolicyHoldRequest[];
  let verifies: { claim_generation: number; required_head: string }[];
  let states: StateRequest[];
  let contexts: RunContext[];
  let outbox: Outbox;
  let nextIid = 6000;

  beforeEach(async () => {
    events = []; holds = []; verifies = []; states = []; contexts = [];
    CodexSessionStore.inspectSession = (async () => "present") as typeof CodexSessionStore.inspectSession;
    api.setRegisterProtocolFeatures(["provider_policy_hold_v1"]);
    await client.register("policy-saved-work", "base", 1, [], ["provider_policy_hold_v1"], undefined, 0);
    outbox = new Outbox({ root: path.join(fx.dataDir, "policy-saved-work"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 128 * 1024 * 1024, retentionMs: 86400000 });
    await outbox.init();
    const report = client.reportState.bind(client);
    client.reportState = async (...args) => {
      states.push(structuredClone(args[1]));
      events.push(args[1].seeded_from_default ? "state:seeded_from_default" : `state:${args[1].status}`);
      return report(...args);
    };
    client.reportPolicyHold = async (_id, body) => {
      holds.push(structuredClone(body));
      events.push(`hold:${body.cause}`);
      return { disposition: "applied", outcome_id: body.outcome_id, source_generation: body.source_generation,
        observed_status: "paused", observed_generation: body.claim_generation, hold_reason: "codex_policy_hold" };
    };
    client.reportPolicyRecoveryVerified = async (_id, body) => {
      verifies.push(structuredClone(body));
      events.push("verify");
      return { disposition: "recorded" };
    };
    const realClone = git.createOrAttachRunnerClone.bind(git);
    git.createOrAttachRunnerClone = (async (...args: Parameters<typeof realClone>) => {
      const clone = await realClone(...args);
      events.push("clone");
      return clone;
    }) as typeof git.createOrAttachRunnerClone;
  });
  afterEach(() => { CodexSessionStore.inspectSession = realInspect; });

  function runner(extra: Parameters<typeof runnerWith>[4] = {}): RunRunner {
    return runnerWith((runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext) => {
        events.push("model");
        contexts.push(ctx);
        return Promise.reject(new Error("stop after admission"));
      } },
    }), fakeGitlab().gitlab, undefined, nullLogger(), {
      outbox, activeRuns: new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32), ...extra,
    });
  }
  function claimFor(over: Record<string, unknown> = {}, iid = nextIid++): ClaimResponse {
    return gitlabClaim(iid, { run_id: randomUUID(), claim_generation: 3, session_id: "saved-session",
      plan_approved: true, plan_md: "approved", secrets: CODEX, ...over }) as ClaimResponse;
  }
  const heldOnOrigin = (iid: number): string =>
    originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "held.txt", "held work\n");
  const rehold = (body: PolicyHoldRequest | undefined, mode = "same_conversation"): void => {
    assert.ok(body, "a re-hold was reported");
    assert.equal(body.detail, "resume_rejected");
    assert.equal(body.cause, "saved_work_missing");
    assert.equal(body.continuation_mode, mode);
    assert.equal(body.published, false);
    assert.equal(body.head, undefined, "no head is fabricated for a re-hold before model work");
    assert.equal(body.checkpoint_contains_latest, false);
  };

  it("saved work present: one verification POST, then the ordinary unconstrained turn", async () => {
    const iid = nextIid++;
    const head = heldOnOrigin(iid);
    await runner().execute(claimFor({ policy_saved_work_head: head }, iid));
    assert.deepEqual(verifies, [{ claim_generation: 3, required_head: head }]);
    assert.equal(holds.length, 0);
    assert.equal(contexts.length, 1, "the ordinary turn runs");
    assert.equal(contexts[0]?.preserveConversation, undefined, "the claim is not constrained to preserve");
  });

  it("admission order: clone result, then the verification POST, then the first provider request", async () => {
    const iid = nextIid++;
    const head = heldOnOrigin(iid);
    await runner().execute(claimFor({ policy_saved_work_head: head }, iid));
    const clone = events.indexOf("clone"), verify = events.indexOf("verify"), model = events.indexOf("model");
    assert.ok(clone >= 0 && clone < verify && verify < model, JSON.stringify(events));
    const running = events.indexOf("state:running");
    assert.ok(running >= 0 && running < verify, "the early running report precedes the verification");
    assert.equal(events.filter(e => e === "verify").length, 1);
    assert.equal(states[0]!.status, "running");
    assert.deepEqual(Object.keys(states[0]!).filter(k => /policy|recover|verif|head|saved/.test(k)), [],
      "the clone-phase running report carries no recovery acknowledgement fields");
  });

  it("saved work missing: re-holds before any model turn, report, notice or follow-up pull", async () => {
    const claim = claimFor({ policy_saved_work_head: MISSING_HEAD });
    api.setInputs(claim.run_id, [input("follow_up", "keep the public API")]);
    await runner().execute(claim);
    assert.deepEqual(events.filter(e => e !== "clone"), ["state:running", "hold:saved_work_missing"]);
    rehold(holds[0]);
    assert.equal(holds[0]!.session_id, "saved-session");
    assert.equal(verifies.length, 0);
    assert.equal(contexts.length, 0, "zero provider requests");
    assert.equal(states.some(s => s.seeded_from_default !== undefined || s.status === "failed"), false);
    const [row] = api.inputRows(claim.run_id);
    assert.ok(row);
    assert.equal(api.isInputIncluded(claim.run_id, row.id), false, "guidance never reaches a prompt");
    assert.ok(fs.existsSync(path.join(fx.dataDir, "runner")), "the retained source is not deleted");
  });

  it("a present but malformed head normalizes to missing saved work and re-holds", async () => {
    api.enqueueClaim(claimFor({ policy_saved_work_head: "NOT-A-HEAD" }));
    const claim = await client.claimRun();
    assert.ok(claim);
    assert.equal(claim.policy_saved_work_head, "");
    await runner().execute(claim);
    assert.deepEqual(events.filter(e => e !== "clone"), ["state:running", "hold:saved_work_missing"]);
    rehold(holds[0]);
    assert.equal(contexts.length, 0);
  });

  it("a git error while proving the head is uncertainty and re-holds", async () => {
    git.cloneHasAncestor = async () => { throw new Error("git exploded"); };
    await runner().execute(claimFor({ policy_saved_work_head: MISSING_HEAD }));
    assert.deepEqual(events.filter(e => e !== "clone"), ["state:running", "hold:saved_work_missing"]);
    assert.equal(verifies.length, 0);
  });

  it("an attempt-path worker re-holds with the actual fresh_from_saved_work mode", async () => {
    await runner({ dockerHost: "unix:///fixture" }).execute(claimFor({ policy_saved_work_head: MISSING_HEAD }));
    rehold(holds[0], "fresh_from_saved_work");
    assert.equal(contexts.length, 0);
  });

  it("control: an absent field is no requirement (no verification, ordinary turn)", async () => {
    await runner().execute(claimFor());
    assert.equal(verifies.length, 0);
    assert.equal(holds.length, 0);
    assert.equal(contexts.length, 1);
  });

  it("control: a preserve-only claim still uses its own policy_expected_head and ignores the saved-work field", async () => {
    await runner().execute(claimFor({ policy_preserve_only: true, policy_expected_head: MISSING_HEAD,
      policy_saved_work_head: "cd".repeat(20) }));
    assert.deepEqual(events.filter(e => e !== "clone"), ["state:running", "hold:saved_work_missing"]);
  });
});

describe("#3153 runner-shared replacement: preserve-only claim over the production GitCache layout", () => {
  installHarness();
  const realInspect = CodexSessionStore.inspectSession;
  let events: string[];
  let holds: PolicyHoldRequest[];
  let verifies: { claim_generation: number; required_head: string }[];
  let contexts: RunContext[];
  let outbox: Outbox;
  let workerA: GitCache;

  beforeEach(async () => {
    events = []; holds = []; verifies = []; contexts = [];
    CodexSessionStore.inspectSession = (async () => "present") as typeof CodexSessionStore.inspectSession;
    api.setRegisterProtocolFeatures(["provider_policy_hold_v1"]);
    await client.register("policy-3153", "base", 1, [], ["provider_policy_hold_v1"], undefined, 0);
    outbox = new Outbox({ root: path.join(fx.dataDir, "policy-3153"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 128 * 1024 * 1024, retentionMs: 86400000 });
    await outbox.init();
    client.reportPolicyHold = async (_id, body) => {
      holds.push(structuredClone(body));
      events.push(`hold:${body.cause}`);
      return { disposition: "applied", outcome_id: body.outcome_id, source_generation: body.source_generation,
        observed_status: "paused", observed_generation: body.claim_generation, hold_reason: "codex_policy_hold" };
    };
    client.reportPolicyRecoveryVerified = async (_id, body) => {
      verifies.push(structuredClone(body));
      events.push("verify");
      return { disposition: "recorded" };
    };
    // Worker A is a separate GitCache object over the SAME dataDir: the replacement (the harness `git`)
    // shares nothing with it in memory, only the runner-shared volume.
    workerA = new GitCache(fx.dataDir, nullLogger(), undefined, testGitCacheOptions());
  });
  afterEach(() => { CodexSessionStore.inspectSession = realInspect; });

  function replacement(): RunRunner {
    return runnerWith((runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext) => {
        events.push("model");
        contexts.push(ctx);
        return Promise.reject(new Error("stop after admission"));
      } },
    }), fakeGitlab().gitlab, undefined, nullLogger(), {
      outbox, activeRuns: new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32),
    });
  }

  /** Runner A's preserving run: its clone under <dataDir>/runner holds a committed R, as a real capture leaves it. */
  async function heldRun(iid: number, runId: string, track: boolean): Promise<{ r: string; clone: string }> {
    const branch = `agent/issue-${iid}`;
    const bare = await workerA.ensureClone(fx.originPath);
    const seed = await workerA.createOrAttachRunnerClone(bare, iid, noProofReseed, runId);
    assert.equal(path.relative(path.join(fx.dataDir, "runner"), seed.path).startsWith(".."), false,
      "the clone lives under <dataDir>/runner, the runner-shared subPath");
    fs.writeFileSync(path.join(seed.path, "held.txt"), "held work\n");
    gitIn(seed.path, ["add", "held.txt"]);
    gitIn(seed.path, [...IDENT, "commit", "-m", "held work"]);
    const r = gitIn(seed.path, ["rev-parse", "HEAD"]);
    if (track) await fixtureFetchTracking(workerA, bare, seed.path, branch, runId);
    return { r, clone: seed.path };
  }
  const claimFor = (iid: number, runId: string, head: string): ClaimResponse =>
    gitlabClaim(iid, { run_id: runId, claim_generation: 3, session_id: "saved-session", plan_approved: true,
      plan_md: "approved", policy_preserve_only: true, policy_expected_head: head, secrets: CODEX }) as ClaimResponse;

  it("bytes present on the shared volume: the replacement recovers R, verifies it and continues", async () => {
    const iid = 6101;
    const runId = randomUUID();
    const { r } = await heldRun(iid, runId, true);
    await replacement().execute(claimFor(iid, runId, r));
    assert.deepEqual(verifies, [{ claim_generation: 3, required_head: r }]);
    assert.equal(holds.length, 0);
    assert.equal(contexts.length, 1, "continues to the harness");
    assert.equal(contexts[0]?.preserveConversation, true);
  });

  it("bytes absent (clone removed, no checkpoint holding R): re-holds saved_work_missing, no head, source not deleted", async () => {
    const iid = 6102;
    const runId = randomUUID();
    const { r, clone } = await heldRun(iid, runId, false);
    fs.rmSync(clone, { recursive: true, force: true });
    const bare = await workerA.ensureClone(fx.originPath);
    assert.throws(() => gitIn(bare, ["cat-file", "-e", `${r}^{commit}`]), "R is in no object store of the volume");
    const refsBefore = gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]);
    await replacement().execute(claimFor(iid, runId, r));
    // Source identity and custody are retained: the shared cache's refs are untouched, and the re-hold
    // reports no terminal state (which would release custody) and no fabricated work.
    assert.equal(gitIn(bare, ["for-each-ref", "--format=%(refname) %(objectname)"]), refsBefore, "the bare cache's refs are unchanged");
    const terminal = api.stateWire.filter((s) => s.runId === runId && ["completed", "failed", "cancelled"].includes(s.body.status));
    assert.deepEqual(terminal, [], "no terminal report releases custody");
    assert.deepEqual(events, ["hold:saved_work_missing"]);
    assert.equal(holds[0]!.published, false);
    assert.equal(holds[0]!.head, undefined, "no head is fabricated");
    assert.equal(holds[0]!.detail, "resume_rejected");
    assert.equal(verifies.length, 0);
    assert.equal(contexts.length, 0, "zero provider requests");
    assert.ok(fs.existsSync(path.join(fx.dataDir, "runner")), "the runner-shared root is not deleted");
    assert.ok(fs.existsSync(bare), "the bare cache is not deleted");
  });
});
