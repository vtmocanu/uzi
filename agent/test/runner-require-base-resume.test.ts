import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import type { Readable } from "node:stream";
import { execFileSync } from "node:child_process";
import { type RunContext, type ExecutorResult } from "../src/executor.js";
import {
  BaseAttestationMissingError,
  BaseCommitDivergedError,
  BaseProofError,
  evaluateRequiredBase,
  RunRunner,
  type ExecutorFactory,
  type RequiredBaseInput,
} from "../src/runner.js";
import { GitCache } from "../src/git.js";
import { nullLogger, testGitCacheOptions } from "./helpers.js";
import { api, client, fakeGitlab, fx, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";

installHarness();

const IDENT = ["-c", "user.name=t", "-c", "user.email=t@example.test"];
const gitIn = (dir: string, args: string[]): string =>
  execFileSync("git", ["-C", dir, ...args], { encoding: "utf8" }).trim();

// ── issue #3194: the strict --require-base decision ──────────────────────────────────────────
//
// A continuation of a seeded --require-base run adopts its own retained work, whose tip is not
// the planned commit. The decision accepts only exact identities (the planned commit, or the
// source this run attested after an accepted flight) with a matching materialized tree.

describe("evaluateRequiredBase (issue #3194)", () => {
  const P = "a".repeat(40);
  const S = "b".repeat(40);
  const T1 = "1".repeat(40);
  const T2 = "2".repeat(40);
  const base = (over: Partial<RequiredBaseInput>): RequiredBaseInput => ({
    plannedBase: P.slice(0, 12),
    identity: P,
    attestedSource: undefined,
    continuation: false,
    wipCherryPicked: false,
    planned: { kind: "resolved", sha: P },
    identityTree: T1,
    materializedTree: T1,
    ...over,
  });

  it("rule A: the adopted source is exactly the planned commit", () => {
    assert.equal(evaluateRequiredBase(base({})), "planned");
  });
  it("rule B: a continuation adopting exactly its attested source is accepted without resolving P", () => {
    assert.equal(
      evaluateRequiredBase(base({ identity: S, attestedSource: S, continuation: true, planned: { kind: "unknown" } })),
      "attested",
    );
  });
  it("a continuation with no attestation is held, not failed as divergence", () => {
    assert.throws(() => evaluateRequiredBase(base({ identity: S, continuation: true })), BaseAttestationMissingError);
  });
  it("a continuation whose attestation names other content is held", () => {
    assert.throws(
      () => evaluateRequiredBase(base({ identity: S, attestedSource: "c".repeat(40), continuation: true })),
      BaseAttestationMissingError,
    );
  });
  it("a WIP marker over P (identity is the marker, not P) is not accepted by rule A", () => {
    // The pre-fix guard compared the unwrapped parent, so a marker over P passed unchecked.
    assert.throws(() => evaluateRequiredBase(base({ identity: S, continuation: true })), BaseAttestationMissingError);
  });
  it("a fresh (non-continuation) mismatch is a genuine divergence", () => {
    assert.throws(() => evaluateRequiredBase(base({ identity: S })), BaseCommitDivergedError);
  });
  it("an ambiguous planned prefix is a proof error even when it also prefixes the identity", () => {
    assert.throws(
      () => evaluateRequiredBase(base({ identity: P, planned: { kind: "ambiguous" } })),
      (e: unknown) => e instanceof BaseProofError && !(e instanceof BaseCommitDivergedError),
    );
  });
  it("an unreadable planned commit is a proof error", () => {
    assert.throws(() => evaluateRequiredBase(base({ planned: { kind: "unknown" } })), BaseProofError);
  });
  it("a planned commit missing from the repository is a divergence", () => {
    assert.throws(() => evaluateRequiredBase(base({ planned: { kind: "missing" } })), BaseCommitDivergedError);
  });
  it("extra materialized content beyond the identity's tree is refused", () => {
    assert.throws(() => evaluateRequiredBase(base({ materializedTree: T2 })), BaseCommitDivergedError);
    assert.throws(
      () => evaluateRequiredBase(base({ identity: S, attestedSource: S, continuation: true, materializedTree: T2 })),
      BaseAttestationMissingError,
    );
  });
  it("an unreadable tree is a proof error", () => {
    assert.throws(() => evaluateRequiredBase(base({ identityTree: undefined })), BaseProofError);
  });
  it("a diverged-checkpoint WIP cherry-pick is refused even when HEAD is P", () => {
    assert.throws(
      () => evaluateRequiredBase(base({ continuation: false, wipCherryPicked: true })),
      BaseCommitDivergedError,
    );
  });
});

describe("--require-base on a same-run continuation (issue #3194)", () => {
  /** Record every publish the worker makes, with the attestation it sends, and land it. */
  function recordPublishes(): { calls: { tip: string; attested?: string }[]; restore: () => void } {
    const calls: { tip: string; attested?: string }[] = [];
    const orig = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, tipOid: string, pack: Readable, _signal?: AbortSignal, _gen?: number, attested?: string,
    ) => {
      for await (const _chunk of pack) { /* drain */ }
      calls.push({ tip: tipOid, ...(attested ? { attested } : {}) });
      return { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/x" } };
    };
    return { calls, restore: () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; } };
  }

  async function twoLegs(iid: number, carryAttestation: boolean) {
    const { gitlab } = fakeGitlab();
    const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
    const legs: RunContext[] = [];
    let leg = 0;
    const factory: ExecutorFactory = () => ({
      executor: {
        run: async (ctx: RunContext): Promise<ExecutorResult> => {
          legs.push(ctx);
          leg++;
          fs.writeFileSync(path.join(ctx.worktreePath, `leg${leg}.txt`), `leg ${leg}\n`);
          gitIn(ctx.worktreePath, ["add", "."]);
          gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", `leg ${leg}`]);
          if (leg === 1) {
            // A cooperative (reap) checkpoint publishes regardless of the time gate.
            await ctx.checkpoint?.({ reap: true });
            throw new Error("simulated budget pause");
          }
          return { branch: ctx.branch };
        },
      },
    });
    const claim = gitlabClaim(iid, {
      plan_approved: true,
      plan_source: "seeded",
      plan_md: "# Seeded plan\n- do it",
      session_id: null,
      planned_base_commit: planned,
      require_base_match: true,
      claim_generation: 1,
    });
    const pub = recordPublishes();
    try {
      const r = runnerWith(factory, gitlab);
      await r.execute(claim);
      const landed = pub.calls.at(-1);
      // What the server returns on the next claim: the persisted tip and its set-only attestation.
      const resumed = gitlabClaim(iid, {
        ...claim,
        claim_generation: 2,
        checkpoint_tip: landed?.tip ?? null,
        checkpoint_attested_source: carryAttestation ? (landed?.attested ?? null) : null,
      });
      await r.execute(resumed).catch(() => undefined);
    } finally {
      pub.restore();
    }
    const failed = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed");
    return { planned, legs, failed, publishes: pub.calls };
  }

  it("a reclaim adopting this run's own attested leg-1 commit implements (regression: the paused #3031 run)", async () => {
    const { planned, legs, failed, publishes } = await twoLegs(31031, true);
    assert.equal(legs[0]?.baseCommit, planned, "sanity: leg 1 was cut from the planned commit");
    const attested = publishes.filter((c) => c.attested);
    assert.ok(attested.length > 0, "leg 1 attested its published source after the guard accepted");
    assert.equal(legs.length, 2, "leg 2 reached the executor");
    assert.notEqual(legs[1]?.baseCommit, planned, "sanity: leg 2 adopted leg 1's work, not the planned commit");
    const refusals = failed.filter((f) => (f.body.failure_reason ?? "").includes("--require-base"));
    assert.deepEqual(refusals, [], "the continuation is not refused");
  });

  it("a reclaim adopting its own work WITHOUT an attestation does not implement", async () => {
    const { legs, failed } = await twoLegs(31032, false);
    assert.equal(legs.length, 1, "leg 2 never reached the executor");
    // M4: the refusal is an owner hold (runner-base-attestation-hold.test.ts), never a failed run.
    const reasons = failed.map((f) => f.body.failure_reason ?? "");
    assert.ok(!reasons.some((r) => r.includes("no checkpoint attests")), `held, not failed: ${reasons.join(" | ")}`);
  });
});

// ── issue #3194 acceptance: the REAL budget wall park -> extend -> fresh claim ─────────────────
//
// Leg 1 works on the planned commit and parks at its wall (PRD #1497 capture-first park). The park
// publishes the run's source with the attestation; the stub broker lands the pack in the fixture
// origin under refs/uzi-checkpoints/<branch>, as the api does. Leg 2 is the claim the server issues
// after `uzi run extend`: same run, next generation, carrying the persisted tip and its set-only
// attestation. Clean committed work and dirty WIP (a wip(park) marker) must both continue, on the
// same worker and on a fresh one.

describe("--require-base wall park -> extend -> reclaim (issue #3194)", () => {
  const GIT_ENV = { ...process.env, GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null" };
  const gitInput = (dir: string, args: string[], input: Buffer): string =>
    execFileSync("git", ["-C", dir, ...args], { env: GIT_ENV, encoding: "utf8", input }).trim();

  /** Land every publish in the fixture origin and record the server's set-only attestation. */
  function brokerStub(branch: string): { persisted: { tip?: string; attested?: string }; restore: () => void } {
    const persisted: { tip?: string; attested?: string } = {};
    const orig = client.publishCheckpoint.bind(client);
    (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = async (
      _runId: string, tipOid: string, pack: Readable, _signal?: AbortSignal, _gen?: number, attested?: string,
    ) => {
      const chunks: Buffer[] = [];
      for await (const c of pack) chunks.push(Buffer.from(c as Buffer));
      gitInput(fx.originPath, ["index-pack", "--stdin", "--fix-thin"], Buffer.concat(chunks));
      gitIn(fx.originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, tipOid]);
      persisted.tip = tipOid;
      if (attested) persisted.attested = attested; // set-only: never cleared by an unattested publish
      return { ok: true, body: { published: true, ref: `refs/uzi-checkpoints/${branch}` } };
    };
    return { persisted, restore: () => { (client as unknown as { publishCheckpoint: unknown }).publishCheckpoint = orig; } };
  }

  for (const shape of ["committed", "wip"] as const) {
    for (const where of ["same worker", "fresh worker"] as const) {
      it(`${shape} work parked at the wall continues on the ${where}`, async () => {
        const iid = 31940 + (shape === "wip" ? 2 : 0) + (where === "fresh worker" ? 1 : 0);
        const branch = `agent/issue-${iid}`;
        const { gitlab } = fakeGitlab();
        const planned = gitIn(fx.originPath, ["rev-parse", "HEAD"]);
        const legs: RunContext[] = [];
        const parked: (string | undefined)[] = [];
        const factory: ExecutorFactory = () => ({
          executor: {
            run: async (ctx: RunContext): Promise<ExecutorResult> => {
              legs.push(ctx);
              if (legs.length === 1) {
                fs.writeFileSync(path.join(ctx.worktreePath, "leg1.txt"), "leg 1\n");
                if (shape === "committed") {
                  gitIn(ctx.worktreePath, ["add", "."]);
                  gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "leg 1"]);
                }
                const outcome = await ctx.parkForWall?.({ completedCount: 0 });
                parked.push(outcome);
                return { branch: ctx.branch, walled: { reason: "run exceeded its wall-clock timeout" } };
              }
              assert.ok(fs.existsSync(path.join(ctx.worktreePath, "leg1.txt")), "leg 1's work is present");
              fs.writeFileSync(path.join(ctx.worktreePath, "leg2.txt"), "leg 2\n");
              gitIn(ctx.worktreePath, ["add", "."]);
              gitIn(ctx.worktreePath, [...IDENT, "commit", "-m", "leg 2"]);
              return { branch: ctx.branch };
            },
          },
        });
        const claim = gitlabClaim(iid, {
          plan_approved: true, plan_source: "seeded", plan_md: "# Seeded plan\n- do it", session_id: null,
          planned_base_commit: planned, require_base_match: true, claim_generation: 1,
        });
        const broker = brokerStub(branch);
        let freshData: string | undefined;
        try {
          await runnerWith(factory, gitlab).execute(claim);
          assert.equal(parked[0], "parked", "leg 1 parked at the wall");
          assert.ok(broker.persisted.attested, "the park's publish carried the attestation");
          const resumed = gitlabClaim(iid, {
            ...claim, claim_generation: 2,
            checkpoint_tip: broker.persisted.tip ?? null,
            checkpoint_attested_source: broker.persisted.attested ?? null,
          });
          // `uzi run extend` resumes the paused run; the server's ownership now names the new claim.
          api.setOwnershipStatus(claim.run_id, "running", 2);
          if (where === "fresh worker") freshData = fs.mkdtempSync(path.join(os.tmpdir(), "cdr-3194-"));
          const leg2Runner = where === "same worker"
            ? runnerWith(factory, gitlab)
            : new RunRunner(client, new GitCache(freshData!, nullLogger(),
              undefined, testGitCacheOptions()), factory, nullLogger(), 20, undefined,
              { pollMs: 5, planApprovalTimeoutMs: 0, questionTimeoutMs: 600, gitlab });
          await leg2Runner.execute(resumed);
        } finally {
          broker.restore();
          if (freshData) fs.rmSync(freshData, { recursive: true, force: true });
        }
        assert.equal(legs.length, 2, "leg 2 reached the executor");
        const refusals = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "failed")
          .map((s) => s.body.failure_reason ?? "").filter((r) => r.includes("--require-base"));
        assert.deepEqual(refusals, [], "the continuation is not refused");
        assert.ok(api.states.some((s) => s.runId === claim.run_id && s.body.status === "completed"), "leg 2 completed");
      });
    }
  }
});
