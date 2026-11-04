import { afterEach, beforeEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { randomUUID } from "node:crypto";
import fs from "node:fs";
import fsp from "node:fs/promises";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { ActiveRunRegistry } from "../src/active-run-registry.js";
import { CodexSessionStore } from "../src/codex/session-state.js";
import { GitCache, WIP_PARK_COMMIT_PREFIX } from "../src/git.js";
import { PolicyContinuationRejected, ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import type { ClaimResponse, PolicyHoldRequest, StateRequest } from "../src/protocol.js";
import type { RunContext } from "../src/executor.js";
import type { RunRunner } from "../src/runner.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, input, runnerWith } from "./runner-harness.js";
import { fixtureFetchTracking } from "./runner-tracking-fixture.js";
import { noProofReseed, nullLogger, testGitCacheOptions } from "./helpers.js";

// Preserve-only admission: a claim the server marked policy_preserve_only may only continue the held
// conversation. Every re-hold is decided in phaseResume, before any notice, report, follow-up pull or
// model turn, and never deletes the retained source.

const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0" };
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const MISSING_HEAD = "ab".repeat(20);
const CODEX = { forge_pat: "fixture-pat", codex: { auth_mode: "api_key" as const, access_token: "fixture-access" } };

function gitIn(dir: string, args: string[]): string {
  return execFileSync("git", ["-C", dir, ...args], { encoding: "utf8", env: GIT_ENV }).trim();
}

/** A commit on refs/heads/<branch> of the fixture origin, built with plumbing so its checkout is untouched. */
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

describe("preserve-only claim admission in phaseResume", () => {
  installHarness();
  const realInspect = CodexSessionStore.inspectSession;
  let transcript: "present" | "absent" | "unknown" = "present";
  let events: string[];
  let holds: PolicyHoldRequest[];
  let verifies: { claim_generation: number; required_head: string }[];
  let states: StateRequest[];
  let contexts: RunContext[];
  let outbox: Outbox;
  let nextIid = 5000;

  beforeEach(async () => {
    transcript = "present";
    events = []; holds = []; verifies = []; states = []; contexts = [];
    CodexSessionStore.inspectSession = (async () => transcript) as typeof CodexSessionStore.inspectSession;
    api.setRegisterProtocolFeatures(["provider_policy_hold_v1"]);
    await client.register("policy-preserve", "base", 1, [], ["provider_policy_hold_v1"], undefined, 0);
    outbox = new Outbox({ root: path.join(fx.dataDir, "policy-preserve"), log: nullLogger(),
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
  });
  afterEach(() => { CodexSessionStore.inspectSession = realInspect; });

  function runner(extra: Parameters<typeof runnerWith>[4] = {}, body?: (ctx: RunContext) => Promise<never>): RunRunner {
    return runnerWith((runId) => ({
      homeDir: path.join(homeDir, runId),
      executor: { run: async (ctx: RunContext) => {
        events.push("model");
        contexts.push(ctx);
        return body ? body(ctx) : Promise.reject(new Error("stop after admission"));
      } },
    }), fakeGitlab().gitlab, undefined, nullLogger(), {
      outbox, activeRuns: new ActiveRunRegistry(() => outbox.listPendingTerminals(), () => 32), ...extra,
    });
  }
  function claimFor(over: Record<string, unknown> = {}, iid = nextIid++): ClaimResponse {
    return gitlabClaim(iid, { run_id: randomUUID(), claim_generation: 3, session_id: "saved-session",
      plan_approved: true, plan_md: "approved", policy_preserve_only: true, secrets: CODEX, ...over }) as ClaimResponse;
  }
  const noLineageNotice = (runId: string): void => {
    const notices = api.messages(runId).filter(m => ["resume_lineage_break", "resume_continued"]
      .includes(String((m.payload as { event?: string })?.event)));
    assert.deepEqual(notices, [], "no lineage or resume notice before admission settles");
  };
  /** The proof is recorded right after the plain clone-phase running report and before any model turn. */
  const verifiedBeforeModel = (): void => {
    assert.equal(events[0], "state:running");
    assert.equal(events[1], "verify", "the recovery record is the next observable step");
    const model = events.indexOf("model");
    assert.ok(model > 1, "a model turn follows the record: " + JSON.stringify(events));
    assert.equal(holds.length, 0);
  };
  const reholdShape = (body: PolicyHoldRequest | undefined, cause: PolicyHoldRequest["cause"], mode = "same_conversation"): void => {
    assert.ok(body, "a re-hold was reported");
    assert.equal(body.detail, "resume_rejected");
    assert.equal(body.cause, cause);
    assert.equal(body.continuation_mode, mode);
    assert.equal(body.published, false);
    assert.equal(body.head, undefined, "a re-hold before model work reports no head");
    for (const key of ["tag", "phase", "correlation_id"] as const)
      assert.equal(body[key], undefined, `a re-hold carries no refusal metadata (${key})`);
    assert.equal(body.source_generation, body.claim_generation);
  };

  describe("ordering", () => {
    it("a missing held head re-holds right after the plain running report: no notice, seeded report, follow-up pull or model", async () => {
      const claim = claimFor({ policy_expected_head: MISSING_HEAD });
      api.setInputs(claim.run_id, [input("follow_up", "keep the public API")]);
      await runner().execute(claim);
      assert.deepEqual(events, ["state:running", "hold:saved_work_missing"]);
      assert.equal(states.length, 1);
      assert.equal(states[0]!.status, "running");
      assert.equal(states[0]!.seeded_from_default, undefined, "the clone-phase report carries no policy or loss data");
      assert.deepEqual(Object.keys(states[0]!).filter(k => /policy|seeded|head/.test(k)), []);
      reholdShape(holds[0], "saved_work_missing");
      assert.equal(holds[0]!.session_id, "saved-session", "the saved session identity is kept");
      assert.equal(verifies.length, 0, "no verification record for work that is not there");
      noLineageNotice(claim.run_id);
      assert.equal(contexts.length, 0, "no model turn");
      assert.ok(fs.existsSync(path.join(fx.dataDir, "runner")), "the retained runner source is not deleted");
    });

    it("guidance survives a re-hold-only claim: the owner follow-up is never pulled or consumed", async () => {
      const claim = claimFor({ policy_expected_head: MISSING_HEAD });
      api.setInputs(claim.run_id, [input("follow_up", "keep the public API")]);
      await runner().execute(claim);
      assert.equal(contexts.length, 0, "the executor, the only caller of pullFollowUps, never ran");
      const [row] = api.inputRows(claim.run_id);
      assert.ok(row, "the guidance row is still on the server");
      // The steering poll's receipts predate admission and are unchanged. What a re-hold-only claim must
      // not do is put the guidance into a prompt (the inclusion stamp) or hand it to an executor.
      assert.equal(api.isInputIncluded(claim.run_id, row.id), false, "never stamped as included in a prompt");
      assert.deepEqual(api.inclusionCalls.filter(c => c.runId === claim.run_id), []);
    });

    it("a verified head is recorded before any model turn, and the guard reaches the executor", async () => {
      const iid = nextIid++;
      const head = originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "held.txt", "held work\n");
      const claim = claimFor({ policy_expected_head: head }, iid);
      await runner().execute(claim);
      assert.equal(events[0], "state:running");
      verifiedBeforeModel();
      assert.deepEqual(verifies, [{ claim_generation: 3, required_head: head }]);
      assert.equal(contexts[0]?.preserveConversation, true, "every root lead turn of the claim is guarded");
      assert.equal(contexts[0]?.sessionId, "saved-session");
      assert.equal(holds.length, 0);
    });

    it("a lost or failed verification record is tolerated and the claim continues", async () => {
      const iid = nextIid++;
      const head = originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "held.txt", "held work\n");
      let attempts = 0;
      client.reportPolicyRecoveryVerified = async () => { attempts++; events.push("verify"); throw new Error("reply lost"); };
      await runner().execute(claimFor({ policy_expected_head: head }, iid));
      assert.equal(attempts, 3, "bounded retry, then the claim carries on");
      assert.equal(events.filter(e => e === "model").length, 1);
      assert.equal(holds.length, 0);
    });
  });

  describe("recovery verification on the real clone path", () => {
    it("positive: the held head is an ancestor of the recovered branch head", async () => {
      const iid = nextIid++;
      const head = originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "a.txt", "a\n");
      const tip = originBranch(`agent/issue-${iid}`, head, "b.txt", "b\n");
      assert.notEqual(tip, head);
      await runner().execute(claimFor({ policy_expected_head: head }, iid));
      verifiedBeforeModel();
    });

    async function seedWipMarker(iid: number, runId: string, content: string, worker: GitCache = git, dataBare?: string):
      Promise<{ marker: string; parent: string; branch: string; bare: string }> {
      const branch = `agent/issue-${iid}`;
      const bare = dataBare ?? await worker.ensureClone(fx.originPath);
      const seed = await worker.createOrAttachRunnerClone(bare, iid, noProofReseed, runId);
      const parent = gitIn(seed.path, ["rev-parse", "HEAD"]);
      fs.writeFileSync(path.join(seed.path, "WIP.txt"), content);
      assert.equal(await worker.commitWipMarker(seed.path), true);
      const marker = gitIn(seed.path, ["rev-parse", "HEAD"]);
      assert.ok(gitIn(seed.path, ["log", "-1", "--format=%s"]).startsWith(WIP_PARK_COMMIT_PREFIX));
      await fixtureFetchTracking(worker, bare, seed.path, branch, runId);
      return { marker, parent, branch, bare };
    }

    it("positive: the recovered wip(park) marker (reset --soft leg) itself is the held head", async () => {
      const runId = randomUUID();
      const iid = nextIid++;
      const { marker } = await seedWipMarker(iid, runId, "in-progress work\n");
      let recovered: unknown;
      const real = git.createOrAttachRunnerClone.bind(git);
      git.createOrAttachRunnerClone = (async (...args: Parameters<typeof real>) => {
        const clone = await real(...args);
        recovered = clone;
        return clone;
      }) as typeof git.createOrAttachRunnerClone;
      await runner().execute(claimFor({ run_id: runId, policy_expected_head: marker }, iid));
      const clone = recovered as { wipRecovered?: boolean; recoveredMarkerSha?: string };
      assert.equal(clone.wipRecovered, true, "the seed took the reset --soft leg");
      assert.equal(clone.recoveredMarkerSha, marker, "the clone result exposes the recovered marker");
      verifiedBeforeModel();
    });

    it("positive: a head older than the recovered marker is contained in the recovered branch", async () => {
      const runId = randomUUID();
      const iid = nextIid++;
      const older = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
      const { parent } = await seedWipMarker(iid, runId, "newer in-progress work\n");
      assert.equal(parent, older);
      await runner().execute(claimFor({ run_id: runId, policy_expected_head: older }, iid));
      verifiedBeforeModel();
    });

    it("positive: a head beneath the marker is accepted through the marker when the branch head does not contain it", async () => {
      const runId = randomUUID();
      const iid = nextIid++;
      const { marker, parent } = await seedWipMarker(iid, runId, "marker work\n");
      // Only the branch-head ancestry check answers "no": the marker's own ancestry must carry the proof.
      const real = git.cloneHasAncestor.bind(git);
      git.cloneHasAncestor = (async (clone: string, ancestor: string, descendant: string) =>
        descendant === "HEAD" ? false : real(clone, ancestor, descendant)) as typeof git.cloneHasAncestor;
      await runner().execute(claimFor({ run_id: runId, policy_expected_head: parent }, iid));
      assert.notEqual(marker, parent);
      verifiedBeforeModel();
    });

    it("positive: the cherry-picked diverged wip(park) marker (cross-worker leg) is the held head", async () => {
      const runId = randomUUID();
      const iid = nextIid++;
      const branch = `agent/issue-${iid}`;
      const dirA = path.join(fx.dataDir, "workerA");
      fs.mkdirSync(dirA, { recursive: true });
      const workerA = new GitCache(dirA, nullLogger(), undefined, testGitCacheOptions());
      const floor0 = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
      const { marker } = await seedWipMarker(iid, runId, "diverged in-progress work\n", workerA);
      const packed = await workerA.checkpointPack(await workerA.ensureClone(fx.originPath), branch);
      assert.ok(packed);
      const chunks: Buffer[] = [];
      for await (const chunk of packed.pack) chunks.push(chunk as Buffer);
      execFileSync("git", ["-C", fx.originPath, "index-pack", "--stdin", "--fix-thin"],
        { env: GIT_ENV, input: Buffer.concat(chunks), stdio: ["pipe", "pipe", "pipe"] });
      gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);
      originBranch(branch, floor0, "ORIGIN_WORK.txt", "published divergent origin work\n");
      let recovered: unknown;
      const real = git.createOrAttachRunnerClone.bind(git);
      git.createOrAttachRunnerClone = (async (...args: Parameters<typeof real>) => {
        const clone = await real(...args);
        recovered = clone;
        return clone;
      }) as typeof git.createOrAttachRunnerClone;
      await runner().execute(claimFor({ run_id: runId, policy_expected_head: marker, checkpoint_tip: marker }, iid));
      const clone = recovered as { wipRecovered?: boolean; wipCherryPicked?: boolean; recoveredMarkerSha?: string };
      assert.equal(clone.wipCherryPicked, true, "the seed took the diverged cherry-pick leg");
      assert.equal(clone.recoveredMarkerSha, marker, "the clone result exposes the cherry-picked marker");
      verifiedBeforeModel();
    });

    it("negative: missing bytes re-hold as saved work missing and record nothing", async () => {
      const claim = claimFor({ policy_expected_head: MISSING_HEAD });
      await runner().execute(claim);
      assert.deepEqual(events, ["state:running", "hold:saved_work_missing"]);
      assert.equal(verifies.length, 0);
    });

    it("negative: a head that exists in the repository but not in the recovered work is not recovered", async () => {
      const iid = nextIid++;
      // A commit the origin default branch never contains and the agent branch never descends from.
      const stray = originBranch("unrelated/stray", gitIn(fx.originPath, ["rev-parse", "HEAD"]), "stray.txt", "stray\n");
      originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "held.txt", "held\n");
      await runner().execute(claimFor({ policy_expected_head: stray }, iid));
      assert.deepEqual(events, ["state:running", "hold:saved_work_missing"]);
    });

    it("negative: any git error is uncertainty and re-holds", async () => {
      git.cloneHasAncestor = async () => { throw new Error("git exploded"); };
      await runner().execute(claimFor({ policy_expected_head: MISSING_HEAD }));
      assert.deepEqual(events, ["state:running", "hold:saved_work_missing"]);
      assert.equal(verifies.length, 0);
    });

    it("negative: a malformed expected head reads as missing saved work, not as a degraded capture", async () => {
      api.enqueueClaim(claimFor({ policy_expected_head: "NOT-A-HEAD" }));
      const claim = await client.claimRun();
      assert.ok(claim);
      assert.equal(claim.policy_expected_head, "");
      await runner().execute(claim);
      assert.deepEqual(events, ["state:running", "hold:saved_work_missing"]);
    });

    it("control: a degraded capture (no expected head) skips verification and continues preserving", async () => {
      const claim = claimFor();
      await runner().execute(claim);
      assert.equal(events[0], "state:running");
      assert.ok(events.includes("model"), "the claim continues preserving");
      assert.equal(verifies.length, 0);
      assert.equal(holds.length, 0);
      assert.equal(contexts[0]?.preserveConversation, true);
    });

    it("the retained source stays on disk after a missing-head re-hold", async () => {
      const iid = nextIid++;
      const claim = claimFor({ policy_expected_head: MISSING_HEAD }, iid);
      await runner().execute(claim);
      const repo = path.basename(git.barePathFor(fx.originPath)).replace(/\.git$/, "");
      const entries = fs.readdirSync(path.join(fx.dataDir, "runner", repo)).filter(n => n.startsWith(`issue-${iid}`));
      assert.ok(entries.length > 0, "the clone directory is kept for the owner");
    });
  });

  describe("every cause comes from its real path", () => {
    it("attempt_path: a Docker-wired worker re-holds as fresh_from_saved_work before verification", async () => {
      const claim = claimFor({ policy_expected_head: MISSING_HEAD });
      await runner({ dockerHost: "unix:///fixture" }).execute(claim);
      assert.deepEqual(events, ["state:running", "hold:attempt_path"], "rule 1 precedes the verification rules");
      reholdShape(holds[0], "attempt_path", "fresh_from_saved_work");
      assert.equal(holds[0]!.session_id, "saved-session");
      noLineageNotice(claim.run_id);
    });

    it("attempt_path: an unwired worker whose recovery mode enables attempt paths re-holds the same way", async t => {
      // The persisted recovery mode is the same effectiveAttemptPaths input a retained recovery sets.
      t.mock.method(git, "recoveryAttemptMode", async () => true);
      const claim = claimFor({ policy_expected_head: MISSING_HEAD });
      await runner().execute(claim);
      assert.deepEqual(events, ["state:running", "hold:attempt_path"]);
      reholdShape(holds[0], "attempt_path", "fresh_from_saved_work");
      assert.equal(contexts.length, 0);
      noLineageNotice(claim.run_id);
    });

    it("saved_work_missing: the held head is not in the recovered clone", async () => {
      await runner().execute(claimFor({ policy_expected_head: MISSING_HEAD }));
      reholdShape(holds[0], "saved_work_missing");
    });

    it("session_missing: a verified head with no saved session id", async () => {
      const iid = nextIid++;
      const head = originBranch(`agent/issue-${iid}`, gitIn(fx.originPath, ["rev-parse", "HEAD"]), "held.txt", "held\n");
      const claim = claimFor({ policy_expected_head: head, session_id: null }, iid);
      await runner().execute(claim);
      assert.deepEqual(events, ["state:running", "verify", "hold:session_missing"], "the proof is recorded, then the rule re-holds");
      reholdShape(holds[0], "session_missing");
      assert.equal(holds[0]!.session_id, undefined);
      noLineageNotice(claim.run_id);
    });

    for (const state of ["absent", "unknown"] as const) {
      it(`transcript_unknown: the saved Codex transcript is ${state} on this worker`, async () => {
        transcript = state;
        const claim = claimFor();
        await runner().execute(claim);
        assert.deepEqual(events, ["state:running", "hold:transcript_unknown"]);
        reholdShape(holds[0], "transcript_unknown");
        assert.equal(holds[0]!.session_id, "saved-session");
        noLineageNotice(claim.run_id);
      });
    }

    it("provider_resume_rejected: a continuation rejected after the claim started re-holds with a capture, not a failure", async () => {
      const claim = claimFor();
      await runner({}, async () => { throw new PolicyContinuationRejected("provider_resume_rejected"); }).execute(claim);
      assert.deepEqual(events.filter(e => !e.startsWith("state:")), ["model", "hold:provider_resume_rejected"]);
      reholdShape(holds[0], "provider_resume_rejected");
      assert.equal(states.some(s => s.status === "failed"), false, "never an ordinary terminal failure");
    });

    it("session_missing from the harness guard re-holds the same way", async () => {
      await runner({}, async () => { throw new PolicyContinuationRejected("session_missing"); }).execute(claimFor());
      reholdShape(holds[0], "session_missing");
      assert.equal(states.some(s => s.status === "failed"), false);
    });

    it("refused: a real provider refusal keeps its metadata and the refused cause", async () => {
      const claim = claimFor({ session_id: null, policy_preserve_only: undefined });
      await runner({}, async () => {
        throw new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
          provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });
      }).execute(claim);
      const body = holds[0];
      assert.ok(body);
      assert.equal(body.detail, "refused");
      assert.equal(body.cause, "refused");
      assert.equal(body.tag, "cyberPolicy");
      assert.equal(body.phase, "implementation");
      assert.match(String(body.correlation_id), /^pr-[0-9a-f]{32}-[1-9][0-9]*$/);
    });

    it("an ineligible worker never turns a rejected continuation into a terminal failure", async () => {
      // No capability negotiated: nothing can be reported, so the run stays non-terminal.
      api.setRegisterProtocolFeatures([]);
      await client.register("policy-preserve", "base", 1, [], [], undefined, 0);
      await runner({}, async () => { throw new PolicyContinuationRejected("provider_resume_rejected"); }).execute(claimFor());
      assert.equal(holds.length, 0);
      assert.equal(states.some(s => s.status === "failed" || s.status === "completed"), false);
    });

    it("a pending owner pause keeps the hold for a rejected continuation", async () => {
      const claim = claimFor({ pause_pending: true, pause_mode: "now" });
      await runner({}, async () => { throw new PolicyContinuationRejected("provider_resume_rejected"); }).execute(claim);
      assert.equal(holds.length, 1, "the pause did not downgrade the re-hold into a failure");
      assert.equal(states.some(s => s.status === "failed"), false);
    });
  });

  describe("repeated claims and the lost-reply chain", () => {
    it("repeated Preserve claims while the held head is missing never reach a model turn", async () => {
      // The first claim re-holds. The held source is then retained, so a reclaim of the same run goes
      // through retained recovery (which the shared harness, with no authority runtime, stops), and a
      // restarted worker with a fresh runner behaves the same. Either way no model turn is ever reached.
      const run_id = randomUUID();
      const iid = nextIid++;
      for (const generation of [3, 4, 5]) {
        await runner().execute(claimFor({ run_id, claim_generation: generation, policy_expected_head: MISSING_HEAD }, iid));
      }
      assert.equal(contexts.length, 0, "no model turn on any claim");
      assert.equal(events.includes("model"), false);
      assert.equal(holds[0]?.cause, "saved_work_missing");
      assert.equal(states.some(s => s.status === "failed" || s.status === "completed"), false, "never terminal");
    });

    it("independent claims with a missing held head each re-hold under their own outcome", async () => {
      for (const generation of [3, 4]) {
        await runner().execute(claimFor({ claim_generation: generation, policy_expected_head: MISSING_HEAD }));
      }
      assert.equal(contexts.length, 0);
      assert.deepEqual(holds.map(h => h.cause), ["saved_work_missing", "saved_work_missing"]);
      assert.notEqual(holds[0]!.outcome_id, holds[1]!.outcome_id);
    });

    it("lost reply to the first hold: the reclaim converts the pending record, then re-holds before any notice, report or thread", async () => {
      const run_id = randomUUID();
      const iid = nextIid++;
      const original: PolicyHoldRequest = {
        outcome_id: "22222222-2222-4222-8222-222222222222", source_generation: 1, claim_generation: 1,
        detail: "refused", cause: "refused", continuation_mode: "same_conversation", published: false,
        tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "b".repeat(32) + "-1", session_id: "saved-session",
      };
      await outbox.journalPolicyHold(run_id, original);
      client.getRunOwnership = async () => ({ status: "running", claim_generation: 3, claim_released: false }) as never;
      client.reportPolicyHold = async (_id, body) => {
        holds.push(structuredClone(body));
        events.push(`hold:${body.cause}`);
        // The first send was applied server-side and its reply lost, so the worker sees only a pending
        // answer; the owner has since resumed, so the current-claim conversion finds it already applied.
        if (body.claim_generation === 1) return { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 };
        return body.cause === "pending_conversion"
          ? { disposition: "already_applied", outcome_id: body.outcome_id, source_generation: 1,
              observed_status: "running", observed_generation: 3 }
          : { disposition: "applied", outcome_id: body.outcome_id, source_generation: body.source_generation,
              observed_status: "paused", observed_generation: 3, hold_reason: "codex_policy_hold" };
      };
      await runner().execute(claimFor({ run_id, claim_generation: 3, policy_expected_head: MISSING_HEAD }, iid));
      assert.deepEqual(events.filter(e => e !== "state:running"),
        ["hold:refused", "hold:pending_conversion", "hold:saved_work_missing"]);
      assert.equal(states.every(s => s.status === "running" && s.seeded_from_default === undefined), true,
        "nothing but the plain running report precedes the re-hold");
      assert.ok(events.indexOf("hold:saved_work_missing") > events.indexOf("hold:pending_conversion"));
      reholdShape(holds[2], "saved_work_missing");
      assert.equal(holds[2]!.source_generation, 3, "the re-hold is its own outcome at the current claim");
      assert.notEqual(holds[2]!.outcome_id, original.outcome_id);
      assert.equal(holds[1]!.outcome_id, original.outcome_id, "the conversion keeps the original identity");
      noLineageNotice(run_id);
      assert.equal(contexts.length, 0);
    });

    it("absent-past-lease residual: an unconstrained claim after the record is gone starts a fresh conversation", async () => {
      // Documented residual: once the hold record is out of reach the claim carries no policy fields and
      // is the ordinary baseline. Nothing here can re-create the constraint.
      transcript = "absent";
      const claim = claimFor({ policy_preserve_only: undefined });
      await runner().execute(claim);
      assert.equal(holds.length, 0);
      assert.equal(contexts.length, 1);
      assert.equal(contexts[0]?.sessionId, undefined, "the lost transcript starts a fresh session");
      assert.equal(contexts[0]?.preserveConversation, undefined);
      assert.ok(api.messages(claim.run_id).some(m => (m.payload as { event?: string })?.event === "resume_lineage_break"),
        "the unconstrained loss notice is unchanged");
    });
  });

  describe("retention reasons", () => {
    type Flags = { clone: boolean; session: boolean; inventory: boolean };
    const flagsOf = (flight: Record<string, unknown>): Flags => ({
      clone: flight.preserveRecoveryClone === true, session: flight.preserveSession === true,
      inventory: flight.keepGuardedInventoryOpen === true,
    });
    const original: PolicyHoldRequest = {
      outcome_id: "22222222-2222-4222-8222-222222222222", source_generation: 1, claim_generation: 1,
      detail: "refused", cause: "refused", continuation_mode: "same_conversation", published: false,
      tag: "cyberPolicy", phase: "implementation", correlation_id: "pr-" + "b".repeat(32) + "-1", session_id: "saved-session",
    };
    async function admissionClaim(retained: boolean): Promise<{ before: Flags; after: Flags; attemptPaths: boolean | undefined }> {
      const iid = nextIid++;
      const claim = claimFor({ policy_preserve_only: undefined }, iid);
      if (retained) {
        const bare = await git.ensureClone(fx.originPath);
        const clone = await git.createOrAttachRunnerClone(bare, iid, noProofReseed, claim.run_id);
        await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
        await fsp.writeFile(path.join(clone.path, "retained.txt"), "keep this work");
        await git.reserveRecoveryIteration(bare, clone.branch, `issue-${iid}`, { runId: claim.run_id, clonePath: clone.path }, 1);
        await git.blockRecoveryEpisode(bare, clone.branch, `issue-${iid}`, { runId: claim.run_id, clonePath: clone.path }, "decoded_history_limit");
      }
      await outbox.journalPolicyHold(claim.run_id, original);
      client.getRunOwnership = async () => ({ status: "running", claim_generation: 3, claim_released: false }) as never;
      client.reportPolicyHold = async (_id, body) => body.claim_generation === 1
        ? { disposition: "pending", outcome_id: body.outcome_id, source_generation: 1 }
        : { disposition: "already_applied", outcome_id: body.outcome_id, source_generation: 1,
            observed_status: "running", observed_generation: 3 };
      const r = runner();
      const seam = r as unknown as { reconcilePolicyAdmission: (c: ClaimResponse, f: Record<string, unknown>) => Promise<boolean> };
      const real = seam.reconcilePolicyAdmission.bind(r);
      let before: Flags | undefined, after: Flags | undefined, attemptPaths: boolean | undefined;
      seam.reconcilePolicyAdmission = async (c, f) => {
        before = flagsOf(f);
        attemptPaths = f.effectiveAttemptPaths as boolean | undefined;
        const ok = await real(c, f);
        after = flagsOf(f);
        assert.equal(ok, true, "the admission reconciled");
        return ok;
      };
      await r.execute(claim);
      assert.ok(before && after, "the reconciliation ran");
      return { before, after, attemptPaths };
    }

    it("reconciling an admission releases only what the admission itself retained", async () => {
      const { before, after, attemptPaths } = await admissionClaim(false);
      assert.notEqual(attemptPaths, true);
      assert.deepEqual(before, { clone: true, session: true, inventory: true }, "the admission retains while unresolved");
      assert.deepEqual(after, { clone: false, session: false, inventory: false }, "nothing else holds the flags, so they clear");
    });

    it("retained recovery keeps its own retention when the admission is reconciled", async () => {
      const { before, after, attemptPaths } = await admissionClaim(true);
      assert.equal(attemptPaths, true, "retained recovery on this unwired worker established the attempt path");
      assert.deepEqual(before, { clone: true, session: true, inventory: true });
      assert.deepEqual(after, { clone: true, session: true, inventory: true },
        "the retained-recovery writer is independent of the admission reason");
    });
  });
});
