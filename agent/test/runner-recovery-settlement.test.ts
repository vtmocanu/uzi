import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

import { StubExecutor, type ExecutorResult, type RunContext } from "../src/executor.js";
import type { GitCache, RunnerClone } from "../src/git.js";
import { Outbox, canonicalizeTerminalBody } from "../src/outbox.js";
import { RecoveryCoordinator } from "../src/recovery.js";
import type { ExecutorFactory } from "../src/runner.js";
import {
  PUSHED_HEAD_UNRECORDED,
  SettlementJournal,
  type RecoverySettleClient,
  type SettlementRecord,
} from "../src/recovery-settlement.js";
import type { RecoverySettleRequest, RecoverySettleResponse } from "../src/protocol.js";
import { FakeRecoveryClient, FakeRecoveryGit, commitInTree, fixture as codexFixture } from "./codex-reap-fixture.js";
import { nullLogger, recordingLogger } from "./helpers.js";
import { api, fakeGitlab, fx, git, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

// issue #1582 M2 — the RUNNER wiring of ancestry settlement for an older-generation custody hold:
// adoption evidence after clone (tracking/checkpoint only, authenticated predecessor source only),
// the pushed head persisted BEFORE the completed report, and the settle sent only AFTER the
// completion ACK. The clone's seed leg is forced by wrapping the REAL createOrAttachRunnerClone
// (the real-git adoption regression lives in runner-recovery-settlement-realgit.test.ts).

const WORKER_TOKEN = "settle-runner-worker-join-token-0123456789";
const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: "/dev/null",
  GIT_CONFIG_SYSTEM: "/dev/null",
  GIT_TERMINAL_PROMPT: "0",
};
const IDENT = ["-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"];
const HOLD_A = "aaaaaaaa-0000-4000-8000-000000000001";
const HOLD_B = "bbbbbbbb-0000-4000-8000-000000000002";

function gitOut(cwd: string, ...args: string[]): string {
  return execFileSync("git", ["-C", cwd, ...args], { env: GIT_ENV, encoding: "utf8", stdio: "pipe" }).trim();
}

function refOrNull(bare: string, ref: string): string | null {
  try {
    return gitOut(bare, "rev-parse", "--verify", ref);
  } catch {
    return null;
  }
}

/** A real commit on a side branch of the origin fixture (fetched into the worker bare at clone),
 *  standing in for a predecessor generation's journaled source. */
function originCommit(branch: string, file: string): string {
  const o = fx.originPath;
  gitOut(o, "checkout", "-q", "-b", branch, "main");
  fs.writeFileSync(path.join(o, file), `${branch}\n`);
  gitOut(o, "add", file);
  gitOut(o, ...IDENT, "commit", "-q", "-m", `prior ${branch}`);
  const sha = gitOut(o, "rev-parse", "HEAD");
  gitOut(o, "checkout", "-q", "main");
  return sha;
}

class FakeSettleClient implements RecoverySettleClient {
  calls: Array<{ runId: string; holdId: string; req: RecoverySettleRequest }> = [];
  answer: (holdId: string) => RecoverySettleResponse | Error = () => new Error("no answer configured");
  constructor(private readonly events?: string[]) {}
  async settleRecoveryHold(runId: string, holdId: string, req: RecoverySettleRequest): Promise<RecoverySettleResponse> {
    this.events?.push(`settle:${holdId}`);
    this.calls.push({ runId, holdId, req });
    const a = this.answer(holdId);
    if (a instanceof Error) throw a;
    return a;
  }
}

interface Rig {
  coord: RecoveryCoordinator;
  recoveryClient: FakeRecoveryClient;
  settlement: SettlementJournal;
  settleClient: FakeSettleClient;
  recoveryRoot: string;
  settlementRoot: string;
  seeded: RunnerClone[];
  cleanup: () => void;
}

/** A token-keyed coordinator + settlement journal over temp roots, and (optionally) the real
 *  clone seeding re-labelled as `seededFrom` so the adoption gate can be exercised per leg. */
function rig(seededFrom?: RunnerClone["seededFrom"], events?: string[], opts: { wipRecovered?: boolean } = {}): Rig {
  const recoveryRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-rec-"));
  const settlementRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-set-"));
  const recoveryClient = new FakeRecoveryClient();
  const coord = new RecoveryCoordinator({
    client: recoveryClient,
    git: new FakeRecoveryGit(),
    log: nullLogger(),
    recoveryRoot,
    workerToken: WORKER_TOKEN,
    now: () => 1_700_000_000_000,
  });
  const settlement = new SettlementJournal({ root: settlementRoot, workerToken: WORKER_TOKEN, log: nullLogger() });
  const seeded: RunnerClone[] = [];
  const g = git as GitCache & { createOrAttachRunnerClone: GitCache["createOrAttachRunnerClone"] };
  const orig = g.createOrAttachRunnerClone.bind(g);
  g.createOrAttachRunnerClone = async (...args: Parameters<GitCache["createOrAttachRunnerClone"]>) => {
    const clone = await orig(...args);
    let out = seededFrom ? { ...clone, seededFrom } : clone;
    if (opts.wipRecovered !== undefined) out = { ...out, wipRecovered: opts.wipRecovered };
    seeded.push(out);
    return out;
  };
  return {
    coord,
    recoveryClient,
    settlement,
    settleClient: new FakeSettleClient(events),
    recoveryRoot,
    settlementRoot,
    seeded,
    cleanup: () => {
      fs.rmSync(recoveryRoot, { recursive: true, force: true });
      fs.rmSync(settlementRoot, { recursive: true, force: true });
    },
  };
}

async function runCompleting(r: Rig, claim: ReturnType<typeof gitlabClaim>): Promise<void> {
  const { gitlab } = fakeGitlab();
  await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, nullLogger(), {
    recovery: r.coord,
    settlement: r.settlement,
    settleClient: r.settleClient,
  }).execute(claim);
}

const released = (runId: string, holdId: string): RecoverySettleResponse => ({
  run_id: runId,
  hold_id: holdId,
  outcome: "released",
});
const retained = (runId: string, holdId: string, reason: string): RecoverySettleResponse => ({
  run_id: runId,
  hold_id: holdId,
  outcome: "retained",
  reason,
});
const hasStatus = (runId: string, status: string): boolean =>
  api.states.some((s) => s.runId === runId && s.body.status === status);
const bare = (): string => git.barePathFor(fx.originPath);
const settleRef = (runId: string, holdId: string, kind: string) => `refs/uzi-settle/${runId}/${holdId}/${kind}`;

describe("RunRunner — adoption evidence + settle of an older-generation hold (issue #1582 M2)", () => {
  for (const leg of ["tracking", "checkpoint"] as const) {
    it(`${leg} adoption: records the exact evidence, persists the pushed head, settles after the ACK, cleans up on released`, async () => {
      const r = rig(leg);
      try {
        const src = originCommit("prior-gen1", "PRIOR.txt");
        const claim = gitlabClaim(5101, { claim_generation: 2 });
        const pred = await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "agent/issue-5101", generation: 1 });
        assert.ok(pred, "precondition: the predecessor's authenticated journal record exists");
        r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
        r.settleClient.answer = (h) => released(claim.run_id, h);
        await runCompleting(r, claim);

        assert.ok(hasStatus(claim.run_id, "completed"));
        const adopted = r.seeded[0]!.baseCommit;
        const branch = r.seeded[0]!.branch;
        const pushed = gitOut(bare(), "rev-parse", `refs/uzi-runner/${branch}`);
        assert.deepEqual(r.settleClient.calls, [
          {
            runId: claim.run_id,
            holdId: HOLD_A,
            req: {
              predecessor_generation: 1,
              successor_generation: 2,
              pushed_sha: pushed,
              source_sha: src,
              adopted_sha: adopted,
            },
          },
        ]);
        assert.equal(gitOut(fx.originPath, "rev-parse", branch), pushed, "pushed_sha is the successor's landed head");
        // released → exactly this hold cleaned up: settlement record, pins, predecessor journal.
        assert.deepEqual(await r.settlement.listRun(claim.run_id), []);
        for (const kind of ["source", "adopted", "pushed"]) {
          assert.equal(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, kind)), null, `${kind} pin deleted`);
        }
        assert.deepEqual(await r.coord.inspect(claim.run_id), [], "the predecessor's journal record is removed");
        assert.deepEqual(r.recoveryClient.releasedGenerations(), [2], "only the successor's own hold was released directly");
      } finally {
        r.cleanup();
      }
    });
  }

  it("the pushed head is persisted BEFORE the completed report is sent, and the settle is sent only AFTER its ACK", async () => {
    const events: string[] = [];
    const r = rig("tracking", events);
    try {
      const src = originCommit("prior-order", "PRIOR.txt");
      const claim = gitlabClaim(5102, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      r.settleClient.answer = (h) => retained(claim.run_id, h, "ancestry_unknown");
      let atReport: SettlementRecord | undefined;
      let fileAtReport = "";
      api.onState(claim.run_id, (body) => {
        if (body.status !== "completed") return;
        events.push("state:completed");
        const file = path.join(r.settlementRoot, claim.run_id, `${HOLD_A}.json`);
        fileAtReport = fs.readFileSync(file, "utf8");
        atReport = JSON.parse(fileAtReport) as SettlementRecord;
      });
      await runCompleting(r, claim);
      assert.ok(atReport, "the settlement record existed when the completed report landed");
      assert.equal(atReport!.state, "pushed", "write-ahead state: persisted, but never sendable before the ACK");
      assert.match(atReport!.pushedSha ?? "", /^[0-9a-f]{40}$/, "pushedSha persisted before the report");
      assert.equal(atReport!.disposition, "publication");
      assert.deepEqual(events, ["state:completed", `settle:${HOLD_A}`], "settle only after the completion ACK");
      assert.ok(!fileAtReport.includes("fixture-forge-pat-000000"), "no forge PAT in the settlement record");
      assert.doesNotMatch(fileAtReport, /forge_pat|token/i);
      // ancestry_unknown → retry later; everything retained.
      const [rec] = await r.settlement.listRun(claim.run_id);
      assert.equal(rec!.state, "pending_settle");
      assert.equal(rec!.attempts, 1);
      assert.ok(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, "pushed")));
      assert.equal((await r.coord.inspect(claim.run_id)).length, 1, "predecessor journal retained");
    } finally {
      r.cleanup();
    }
  });

  it("a sweep fired right after the write-ahead persist (before the completion ACK) sends NOTHING; the post-ACK path settles", async () => {
    const r = rig("tracking");
    try {
      const src = originCommit("prior-sweep", "PRIOR.txt");
      const claim = gitlabClaim(5106, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      r.settleClient.answer = (h) => released(claim.run_id, h);
      const { gitlab } = fakeGitlab();
      const runner = runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), gitlab, undefined, nullLogger(), {
        recovery: r.coord,
        settlement: r.settlement,
        settleClient: r.settleClient,
      });
      // The timer sweep fires at the worst instant: right after the pushed head is persisted, before
      // the completed report is even sent (no outbox is wired, so no pending-terminal entry exists).
      let sweptAfterPersist = false;
      let callsAtSweep = -1;
      const put = r.settlement.put.bind(r.settlement);
      r.settlement.put = async (rec: SettlementRecord) => {
        const ok = await put(rec);
        if (rec.pushedSha && !sweptAfterPersist) {
          sweptAfterPersist = true;
          await runner.settlePendingPredecessors();
          callsAtSweep = r.settleClient.calls.length;
        }
        return ok;
      };
      await runner.execute(claim);
      assert.ok(sweptAfterPersist, "precondition: the sweep ran right after the persist");
      assert.equal(callsAtSweep, 0, "no settle before the api applied the completion");
      assert.ok(hasStatus(claim.run_id, "completed"));
      assert.equal(r.settleClient.calls.length, 1, "the post-ACK path settled exactly once");
      assert.deepEqual(await r.settlement.listRun(claim.run_id), [], "released and cleaned up");
    } finally {
      r.cleanup();
    }
  });

  it("an ACK that reads the run as NOT completed (the server failed it) sends no settle", async () => {
    const r = rig("tracking");
    try {
      const src = originCommit("prior-noack", "PRIOR.txt");
      const claim = gitlabClaim(5103, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      r.settleClient.answer = (h) => released(claim.run_id, h);
      api.overrideStateStatus(claim.run_id, "failed");
      await runCompleting(r, claim);
      assert.equal(r.settleClient.calls.length, 0, "no settle without a completed ACK");
      const [rec] = await r.settlement.listRun(claim.run_id);
      assert.equal(rec!.state, "terminal", "never left sendable: the successor did not complete");
      assert.equal(rec!.lastReason, "successor_not_completed");
      assert.ok(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, "pushed")), "pins kept for the owner path");
      assert.equal((await r.coord.inspect(claim.run_id)).length, 1, "predecessor journal kept");
      await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), fakeGitlab().gitlab, undefined, nullLogger(), {
        recovery: r.coord,
        settlement: r.settlement,
        settleClient: r.settleClient,
      }).settlePendingPredecessors();
      assert.equal(r.settleClient.calls.length, 0, "the sweep never sends it either");
    } finally {
      r.cleanup();
    }
  });

  for (const [reason, want] of [
    ["not_ancestor", "terminal"],
    ["candidate_mismatch", "terminal"],
    ["state_changed", "pending_settle"],
  ] as const) {
    it(`retained/${reason} → ${want}; predecessor journal + settlement pins intact`, async () => {
      const r = rig("tracking");
      try {
        const src = originCommit(`prior-${reason}`, "PRIOR.txt");
        const claim = gitlabClaim(5104, { claim_generation: 2 });
        await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
        r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
        r.settleClient.answer = (h) => retained(claim.run_id, h, reason);
        await runCompleting(r, claim);
        assert.equal(r.settleClient.calls.length, 1);
        const [rec] = await r.settlement.listRun(claim.run_id);
        assert.equal(rec!.state, want);
        assert.equal(rec!.lastReason, reason);
        const preds = await r.coord.inspect(claim.run_id);
        assert.deepEqual(preds.map((p) => p.generation), [1], "the predecessor journal record is kept");
        for (const kind of ["source", "adopted", "pushed"]) {
          assert.ok(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, kind)), `${kind} pin kept`);
        }
        assert.equal(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, "source")), src);
      } finally {
        r.cleanup();
      }
    });
  }

  it("released cleans up EXACTLY that hold; a sibling predecessor retained on the same run is untouched", async () => {
    const r = rig("tracking");
    try {
      const srcA = originCommit("prior-a", "A.txt");
      const srcB = originCommit("prior-b", "B.txt");
      const claim = gitlabClaim(5105, { claim_generation: 3 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: srcA, kind: "issue", branch: "b", generation: 1 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: srcB, kind: "issue", branch: "b", generation: 2 });
      r.recoveryClient.holds = [
        { hold_id: HOLD_A, generation: 1, has_available_capture: true },
        { hold_id: HOLD_B, generation: 2, has_available_capture: true },
      ];
      r.settleClient.answer = (h) =>
        h === HOLD_A ? released(claim.run_id, h) : retained(claim.run_id, h, "ancestry_unknown");
      await runCompleting(r, claim);
      assert.deepEqual(r.settleClient.calls.map((c) => [c.holdId, c.req.predecessor_generation, c.req.source_sha]).sort(), [
        [HOLD_A, 1, srcA],
        [HOLD_B, 2, srcB],
      ]);
      assert.deepEqual((await r.settlement.listRun(claim.run_id)).map((x) => x.holdId), [HOLD_B]);
      assert.deepEqual((await r.coord.inspect(claim.run_id)).map((p) => p.generation), [2]);
      assert.equal(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, "source")), null);
      assert.equal(refOrNull(bare(), settleRef(claim.run_id, HOLD_B, "source")), srcB);
    } finally {
      r.cleanup();
    }
  });
});

describe("RunRunner — NO adoption evidence (issue #1582 M2)", () => {
  async function expectNothing(
    r: Rig,
    claim: ReturnType<typeof gitlabClaim>,
  ): Promise<void> {
    r.settleClient.answer = (h) => released(claim.run_id, h);
    await runCompleting(r, claim);
    assert.ok(hasStatus(claim.run_id, "completed"));
    assert.deepEqual(await r.settlement.listRun(claim.run_id), [], "no settlement record");
    assert.equal(r.settleClient.calls.length, 0, "no settle sent");
    assert.deepEqual(r.recoveryClient.releasedGenerations(), [claim.claim_generation], "only the current generation released");
  }

  it("a default reseed records nothing (the predecessor stays retained)", async () => {
    const r = rig(); // real seeding: a fresh run seeds off the default tip
    try {
      const src = originCommit("prior-default", "PRIOR.txt");
      const claim = gitlabClaim(5201, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
      assert.equal(r.seeded[0]!.seededFrom, "default");
      assert.equal((await r.coord.inspect(claim.run_id)).length, 1, "predecessor journal untouched");
    } finally {
      r.cleanup();
    }
  });

  it("a tracking adoption that recovered a wip(park) marker records nothing (the marker is out of history)", async () => {
    const r = rig("tracking", undefined, { wipRecovered: true });
    try {
      const src = originCommit("prior-wip", "PRIOR.txt");
      const claim = gitlabClaim(5205, { claim_generation: 2 });
      const pred = await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "agent/issue-5205", generation: 1 });
      assert.ok(pred, "precondition: the predecessor's authenticated journal record exists");
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
      assert.equal(r.seeded[0]!.wipRecovered, true);
      const pins = gitOut(bare(), "for-each-ref", "--format=%(refname)", `refs/uzi-settle/${claim.run_id}/`);
      assert.equal(pins, "", "no settlement pin");
      assert.equal((await r.coord.inspect(claim.run_id)).length, 1, "predecessor journal untouched");
    } finally {
      r.cleanup();
    }
  });

  it("a missing predecessor journal record records nothing", async () => {
    const r = rig("tracking");
    try {
      const claim = gitlabClaim(5202, { claim_generation: 2 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
    } finally {
      r.cleanup();
    }
  });

  it("a TAMPERED predecessor journal record records nothing", async () => {
    const r = rig("tracking");
    try {
      const src = originCommit("prior-tamper", "PRIOR.txt");
      const claim = gitlabClaim(5203, { claim_generation: 2 });
      const pred = await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      const file = path.join(r.recoveryRoot, claim.run_id, `${pred!.captureId}.json`);
      const obj = JSON.parse(fs.readFileSync(file, "utf8"));
      obj.sourceSha = gitOut(fx.originPath, "rev-parse", "main"); // redirect the source
      fs.writeFileSync(file, JSON.stringify(obj));
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
    } finally {
      r.cleanup();
    }
  });

  it("a hold id the journal would refuse pins NOTHING (checked before refs/uzi-settle is written)", async () => {
    const r = rig("tracking");
    try {
      const src = originCommit("prior-unsafe", "PRIOR.txt");
      const claim = gitlabClaim(5204, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      // "bad.hold" sanitizes to the ref component "bad-hold" but is refused by the journal.
      r.recoveryClient.holds = [{ hold_id: "bad.hold", generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
      const pins = gitOut(bare(), "for-each-ref", "--format=%(refname)", `refs/uzi-settle/${claim.run_id}/`);
      assert.equal(pins, "", "no orphan settlement pin");
    } finally {
      r.cleanup();
    }
  });

  it("a newer generation marks an older successor's still-adopted/pushed records terminal/superseded (pins kept)", async () => {
    const r = rig(); // default reseed: this generation adopts nothing itself
    try {
      const claim = gitlabClaim(5207, { claim_generation: 3 });
      const old: SettlementRecord = {
        version: 1,
        runId: claim.run_id,
        holdId: HOLD_A,
        predecessorGeneration: 1,
        successorGeneration: 2,
        sourceSha: "1".repeat(40),
        sourceCaptureId: "cap-old",
        adoptedSha: "2".repeat(40),
        seededFrom: "tracking",
        branch: "agent/issue-5207",
        barePath: "/nonexistent",
        createdAt: 1,
        state: "pushed",
        pushedSha: "3".repeat(40),
        disposition: "publication",
        attempts: 0,
      };
      await r.settlement.put(old);
      await r.settlement.put({ ...old, holdId: HOLD_B, state: "adopted", pushedSha: undefined, disposition: undefined });
      r.settleClient.answer = (h) => released(claim.run_id, h);
      await runCompleting(r, claim);
      const recs = await r.settlement.listRun(claim.run_id);
      assert.deepEqual(
        recs.map((x) => [x.holdId, x.state, x.lastReason]),
        [
          [HOLD_A, "terminal", "superseded"],
          [HOLD_B, "terminal", "superseded"],
        ],
      );
      assert.equal(r.settleClient.calls.length, 0);
    } finally {
      r.cleanup();
    }
  });

  it("a hold at (or above) the current generation is never treated as a predecessor", async () => {
    const r = rig("tracking");
    try {
      const src = originCommit("prior-same", "PRIOR.txt");
      const claim = gitlabClaim(5205, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 3 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 3, has_available_capture: true }];
      await expectNothing(r, claim);
    } finally {
      r.cleanup();
    }
  });

  it("a predecessor source ABSENT from the bare pins nothing and records nothing", async () => {
    const r = rig("tracking");
    try {
      const claim = gitlabClaim(5206, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: "9".repeat(40), kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      await expectNothing(r, claim);
    } finally {
      r.cleanup();
    }
  });
});

describe("RunRunner — settlement promotion on every terminal path (issue #1582 M2 follow-up)", () => {
  function pushedRecord(runId: string, gen: number): SettlementRecord {
    return {
      version: 1,
      runId,
      holdId: HOLD_A,
      predecessorGeneration: gen - 1,
      successorGeneration: gen,
      sourceSha: "1".repeat(40),
      sourceCaptureId: "cap-qd",
      adoptedSha: "2".repeat(40),
      seededFrom: "tracking",
      branch: "agent/issue-5301",
      barePath: "/nonexistent",
      createdAt: 1,
      state: "pushed",
      pushedSha: "3".repeat(40),
      disposition: "publication",
      attempts: 0,
    };
  }

  it("the queued-duplicate gate's pending-terminal replay promotes pushed → pending_settle BEFORE retiring the outbox entry", async () => {
    const r = rig();
    const outboxRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-qd-outbox-"));
    try {
      const claim = gitlabClaim(5301, { claim_generation: 2 });
      const outbox = new Outbox({
        root: outboxRoot,
        log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024,
        maxBytes: 512 * 1024 * 1024,
        retentionMs: 7 * 86_400_000,
      });
      await outbox.init();
      // The previous same-run attempt journaled its completed terminal write-ahead and persisted the
      // pushed head, then died before the send: the queued duplicate's gate replays it.
      const journaled = await outbox.journalTerminal(
        claim.run_id,
        2,
        "running",
        0,
        canonicalizeTerminalBody({ status: "completed", branch: "agent/issue-5301" }, 1 << 20),
      );
      assert.equal(journaled.journaled, true, "precondition: the completed terminal is journaled");
      await r.settlement.put(pushedRecord(claim.run_id, 2));
      const pendingAtPromotion: boolean[] = [];
      const put = r.settlement.put.bind(r.settlement);
      r.settlement.put = async (rec: SettlementRecord) => {
        if (rec.state === "pending_settle") pendingAtPromotion.push(outbox.hasPendingTerminal(claim.run_id, 2));
        return put(rec);
      };
      api.setOwnershipStatus(claim.run_id, "completed", 2);
      r.settleClient.answer = (h) => released(claim.run_id, h);
      const runner = runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), fakeGitlab().gitlab, undefined, nullLogger(), {
        recovery: r.coord,
        settlement: r.settlement,
        settleClient: r.settleClient,
        outbox,
      });
      const proceed = await (runner as unknown as { gateQueuedDuplicate(c: typeof claim): Promise<boolean> })
        .gateQueuedDuplicate(claim);
      assert.equal(proceed, false, "the run is terminal: the duplicate does not execute");
      assert.ok(hasStatus(claim.run_id, "completed"), "the pending completed terminal was replayed");
      assert.equal(outbox.hasPendingTerminal(claim.run_id, 2), false, "the outbox entry was retired");
      assert.deepEqual(pendingAtPromotion, [true], "promoted while the outbox still held the pending terminal");
      const [rec] = await r.settlement.listRun(claim.run_id);
      assert.equal(rec!.state, "pending_settle", "not stranded in `pushed`");
      await runner.settlePendingPredecessors();
      assert.deepEqual(r.settleClient.calls.map((c) => c.holdId), [HOLD_A], "the sweep then sends the settle");
    } finally {
      fs.rmSync(outboxRoot, { recursive: true, force: true });
      r.cleanup();
    }
  });

  it("a deferred (Codex) committed completion persists the pushed head, promotes it on the ACK, and settles", async () => {
    const r = rig("tracking");
    const events: string[] = [];
    const codex = codexFixture(events, () => false);
    try {
      const src = originCommit("prior-codex", "PRIOR.txt");
      const claim = gitlabClaim(5302, { claim_generation: 2 });
      await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
      r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
      r.settleClient.answer = (h) => released(claim.run_id, h);
      const states: string[] = [];
      const put = r.settlement.put.bind(r.settlement);
      r.settlement.put = async (rec: SettlementRecord) => {
        states.push(rec.state);
        return put(rec);
      };
      const homeRoot = fs.mkdtempSync(path.join(os.tmpdir(), "uzi-settle-codex-"));
      const factory: ExecutorFactory = (runId) => ({
        homeDir: path.join(homeRoot, runId),
        executor: {
          safety: codex.safety,
          run: async (ctx: RunContext): Promise<ExecutorResult> => {
            fs.mkdirSync(path.join(homeRoot, runId), { recursive: true });
            commitInTree(ctx.worktreePath, "CODEX.txt", "codex work\n");
            return { branch: ctx.branch };
          },
        },
      });
      try {
        await runnerWith(factory, fakeGitlab().gitlab, undefined, nullLogger(), {
          recovery: r.coord,
          settlement: r.settlement,
          settleClient: r.settleClient,
        }).execute(claim);
      } finally {
        fs.rmSync(homeRoot, { recursive: true, force: true });
      }
      assert.ok(hasStatus(claim.run_id, "completed"));
      assert.deepEqual(states, ["adopted", "pushed", "pending_settle"], "write-ahead pushed, then promoted on the ACK");
      const branch = r.seeded[0]!.branch;
      assert.deepEqual(r.settleClient.calls.map((c) => [c.holdId, c.req.pushed_sha]), [
        [HOLD_A, gitOut(bare(), "rev-parse", `refs/uzi-runner/${branch}`)],
      ]);
      assert.deepEqual(await r.settlement.listRun(claim.run_id), [], "released and cleaned up");
    } finally {
      fs.rmSync(codex.root, { recursive: true, force: true });
      r.cleanup();
    }
  });

  for (const [mode, label] of [
    ["pin-false", "a pushed pin that fails"],
    ["pin-throws", "a pushed pin that throws"],
    ["tip-null", "an unreadable tracking tip"],
  ] as const) {
    it(`${label} marks the record terminal/pushed_head_unrecorded (logged), never sent`, async () => {
      const r = rig("tracking");
      try {
        const src = originCommit(`prior-unrec-${mode}`, "PRIOR.txt");
        const claim = gitlabClaim(5303, { claim_generation: 2 });
        await r.coord.pin({ runId: claim.run_id, sourceSha: src, kind: "issue", branch: "b", generation: 1 });
        r.recoveryClient.holds = [{ hold_id: HOLD_A, generation: 1, has_available_capture: true }];
        r.settleClient.answer = (h) => released(claim.run_id, h);
        const pin = git.pinSettlementRefs.bind(git);
        let pushedPins = 0;
        git.pinSettlementRefs = async (...args: Parameters<GitCache["pinSettlementRefs"]>) => {
          if (args[3].pushed !== undefined) {
            pushedPins += 1;
            if (mode === "pin-throws") throw new Error("pin exploded");
            if (mode === "pin-false") return false;
          }
          return pin(...args);
        };
        if (mode === "tip-null") git.trackingTip = async () => null;
        const { logger, lines } = recordingLogger();
        await runnerWith(() => ({ executor: new StubExecutor(nullLogger()) }), fakeGitlab().gitlab, undefined, logger, {
          recovery: r.coord,
          settlement: r.settlement,
          settleClient: r.settleClient,
        }).execute(claim);
        assert.ok(hasStatus(claim.run_id, "completed"));
        const [rec] = await r.settlement.listRun(claim.run_id);
        assert.equal(rec!.state, "terminal");
        assert.equal(rec!.lastReason, PUSHED_HEAD_UNRECORDED, "the real cause, not successor_not_published");
        assert.equal(rec!.pushedSha, undefined);
        assert.equal(r.settleClient.calls.length, 0, "never sent");
        assert.equal(pushedPins, mode === "tip-null" ? 0 : 1, "no pushed pin without a head");
        assert.ok(
          lines.some((l) => (l as { reason?: string; hold_id?: string }).reason === PUSHED_HEAD_UNRECORDED
            && (l as { hold_id?: string }).hold_id === HOLD_A),
          "the unrecorded pushed head is logged with its reason",
        );
        assert.ok(refOrNull(bare(), settleRef(claim.run_id, HOLD_A, "source")), "the source pin is kept");
        assert.equal((await r.coord.inspect(claim.run_id)).length, 1, "the predecessor journal is kept");
      } finally {
        r.cleanup();
      }
    });
  }
});
