import { describe, it, mock } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { REASON_WORKER_RESIDUE_BLOCKED } from "../src/git.js";
import type { RunContext } from "../src/executor.js";
import type { HeldPublishResult } from "../src/client.js";
import { fakeGitHub, fakeGitlab, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import {
  HELD_LINUX_ONLY, JOIN_TOKEN, PUBLICATION_ID, SERVER_CAPTURE_ID, assertCustodyRetained, badRequest, bare, command, commit,
  failedStates, failingWork, frozenRecords, heldServer, patShaped, pinRef, recoveryOf, redrive, retained, skipCheckpoints,
  watchFreezes, type FinalCall, type HeldCall, type HeldServer, type Work,
} from "./runner-held-publication-fixture.js";

installHarness();

// Issue #2545 M5: every way the held publication can fall short archives instead, on the SAME frozen
// record, and the custody (open hold, pin, frozen record, clone, bare) is retained until the FINAL
// that finally settles the generation is ACKed.

interface Scenario {
  claim: ReturnType<typeof gitlabClaim>;
  server: HeldServer;
  work: Partial<Work>;
  r: ReturnType<typeof runner>;
  capture: ReturnType<typeof mock.method>;
}

let nextIid = 2600;
async function scenario(opts: {
  setup?: (s: Omit<Scenario, "capture">) => void | Promise<void>;
  extra?: (ctx: RunContext) => Promise<void> | void;
  features?: readonly string[];
  failWith?: string;
  runnerOptions?: Parameters<typeof runner>[3];
} = {}): Promise<Scenario> {
  const iid = nextIid++;
  const claim = gitlabClaim(iid, { claim_generation: 50, inventory_guarded: true });
  const { gitlab } = fakeGitlab();
  const server = heldServer(claim, opts.features);
  skipCheckpoints();
  const work: Partial<Work> = {};
  const r = runner(failingWork(work, opts.extra, opts.failWith), gitlab, JOIN_TOKEN, { checkpointIntervalMs: 1, ...opts.runnerOptions });
  const capture = mock.method(recoveryOf(r), "captureAndUpload");
  await opts.setup?.({ claim, server, work, r });
  await r.execute(claim);
  return { claim, server, work, r, capture };
}

/** The fallback outcome: archived on the same record, custody retained until the archive FINAL is ACKed. */
async function assertArchived(s: Scenario, label: string, expectedHeldCalls: number): Promise<void> {
  const { claim, server, work, r, capture } = s;
  assert.equal(failedStates(claim.run_id).length, 1, `${label}: the honest failed report is sent once`);
  assert.equal(server.heldCalls.length, expectedHeldCalls, `${label}: held upload attempts`);
  assert.ok(capture.mock.callCount() >= 1, `${label}: the archive capture ran`);
  assert.equal(server.archiveUploads(), 1, `${label}: one archive uploaded`);
  const record = await assertCustodyRetained(server, claim, work, `${label}, archived, FINAL pending`);
  assert.equal(record.state, "uploaded", `${label}: the archive is on the same frozen record`);
  assert.equal(server.finals.length, 0, `${label}: no FINAL is sent from inside the flight`);
  const sendFinal = async (call: FinalCall): Promise<void> => {
    assert.equal(call.disposition?.kind, "archive", `${label}: only the archive FINAL is sent`);
    assert.equal(server.holdOpen(), true, `${label}: the hold is open until the archive FINAL is ACKed`);
    assert.equal(server.acked(), 0);
    assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]), record.inventoryCurrentSha);
    // The FINAL names a frozen record of this generation (the live pass may also snapshot the
    // generation afresh, as it does on main; the held path is not involved here).
    const frozen = await frozenRecords(claim);
    assert.ok(frozen.some(f => f.sourceSha === call.disposition!.source_sha && f.coverageDigest === call.disposition!.coverage_digest));
  };
  server.onFinal = sendFinal;
  await redrive(r);
  assert.equal(server.finals.length, 1, `${label}: exactly one FINAL`);
  assert.equal(server.finals[0]!.disposition?.kind, "archive");
  assert.equal(server.finals[0]!.disposition?.capture_id, SERVER_CAPTURE_ID);
  assert.equal(server.acked(), 1, `${label}: the archive FINAL ACK settles the generation`);
  assert.equal(server.holdOpen(), false);
  assert.equal(server.heldCalls.length, expectedHeldCalls, `${label}: no further held upload`);
}

describe("held publication falls back to the archive", () => {
  it("a new WIP secret is never uploaded and is remembered as flagged", { skip: HELD_LINUX_ONLY }, async () => {
    let flagged = 0;
    const s = await scenario({
      extra: ctx => { fs.writeFileSync(path.join(ctx.worktreePath, "leak.txt"), `token=${patShaped()}\n`); },
      setup: ({ r }) => {
        const real = r["recordFlaggedCommits"].bind(r);
        mock.method(r as unknown as Record<string, (...a: unknown[]) => unknown>, "recordFlaggedCommits", (...a: unknown[]) => {
          flagged++;
          return (real as (...x: unknown[]) => unknown)(...a);
        });
      },
    });
    assert.ok(flagged > 0, "the finding was recorded for the flight");
    await assertArchived(s, "wip secret", 0);
  });

  it("a remembered flagged ancestor is never uploaded, even when the fresh scan sees nothing", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({
      setup: ({ server }) => {
        // The range scan is forced clean once the failure is sent (as if the flagged commit sat behind
        // a floor); the mid-run checkpoint scan stays real and remembers the commit.
        const real = git.secretScanCheckpointRange.bind(git);
        git.secretScanCheckpointRange = async (...a) => server.failedAcked() ? { trusted: true, findings: [] } : real(...a);
      },
      extra: async ctx => {
        commit(ctx.worktreePath, "old-leak.txt", `token=${patShaped()}\n`);
        await ctx.checkpoint!({ reap: false });
        commit(ctx.worktreePath, "old-leak.txt", "clean now\n");
      },
    });
    await assertArchived(s, "flagged ancestor", 0);
  });

  for (const reason of ["deadline", "range_invalid", "scan_untrusted", "too_many_merges", "count_failed"] as const) {
    it(`an untrusted scan (${reason}) archives`, { skip: HELD_LINUX_ONLY }, async () => {
      const s = await scenario({
        setup: ({ server }) => {
          const real = git.secretScanCheckpointRange.bind(git);
          git.secretScanCheckpointRange = async (...a) => server.failedAcked() ? { trusted: false, findings: [], reason } : real(...a);
        },
      });
      await assertArchived(s, `untrusted ${reason}`, 0);
    });
  }

  it("a scan that throws archives", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({
      setup: ({ server }) => {
        const real = git.secretScanCheckpointRange.bind(git);
        git.secretScanCheckpointRange = async (...a) => { if (server.failedAcked()) throw new Error("scanner crashed"); return real(...a); };
      },
    });
    await assertArchived(s, "scan throws", 0);
  });

  it("an unresolved range archives", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({
      setup: ({ server }) => {
        const real = git.resolveCheckpointRange.bind(git);
        git.resolveCheckpointRange = async (...a) => server.failedAcked() ? null : real(...a);
      },
    });
    await assertArchived(s, "range unresolved", 0);
  });

  it("a workflow change in the scanned range archives", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({ extra: ctx => { commit(ctx.worktreePath, ".github/workflows/ci.yml", "name: ci\n"); } });
    await assertArchived(s, "workflow change", 0);
  });

  it("a branch merely stale against the default branch's workflow files is still held", async () => {
    let defaultWorkflowTip = "";
    const s = await scenario({
      extra: async () => {
        // The default branch gains a workflow change after the run's clone was seeded; the bare learns
        // it, but it is not in the run's history, so it is not a change the run authors.
        defaultWorkflowTip = commit(fx.originPath, ".github/workflows/ci.yml", "name: ci\n");
        await git.ensureClone(fx.originPath);
        assert.equal(command(bare(), ["rev-parse", "refs/remotes/origin/main"]), defaultWorkflowTip);
      },
    });
    assert.equal(s.server.heldCalls.length, 1, "a stale branch does not block the held publication");
    assert.equal(s.capture.mock.callCount(), 0);
    await assertCustodyRetained(s.server, s.claim, s.work, "stale branch");
  });

  it("a pack over the cap is never sent and archives", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({ runnerOptions: { checkpointTestHooks: { heldPackMaxBytes: 16 } } });
    await assertArchived(s, "pack over cap", 0);
  });

  const answers: Array<[string, (call: HeldCall) => HeldPublishResult | Promise<HeldPublishResult>]> = [
    ["refused", c => ({ ok: true, body: { publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${c.run}/${c.generation}`, tip: c.tip, state: "refused", reason: "create_refused" } })],
    ["create_unknown", c => ({ ok: true, body: { publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${c.run}/${c.generation}`, tip: c.tip, state: "create_unknown", reason: "forge_unavailable" } })],
    ["invoked", c => ({ ok: true, body: { publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${c.run}/${c.generation}`, tip: c.tip, state: "invoked" } })],
    ["409 refusal", () => ({ ok: false, httpStatus: 409, reason: "not_failed" })],
    ["413", () => ({ ok: false, httpStatus: 413, reason: "pack_too_large" })],
    ["ci_config_guard refusal", () => ({ ok: false, httpStatus: 409, reason: "ci_config_guard" })],
    ["422", () => ({ ok: false, httpStatus: 422, reason: "pack_invalid" })],
    ["429", () => ({ ok: false, httpStatus: 429, reason: "busy" })],
    ["503", () => ({ ok: false, httpStatus: 503, reason: "forge_unavailable" })],
    ["a different tip", c => ({ ok: true, body: { publication_id: PUBLICATION_ID, ref: `refs/uzi-held/${c.run}/${c.generation}`, tip: "0".repeat(40), state: "created" } })],
    ["a foreign ref", c => ({ ok: true, body: { publication_id: PUBLICATION_ID, ref: "refs/heads/other", tip: c.tip, state: "created" } })],
    ["a timeout", () => { throw new DOMException("The operation timed out", "TimeoutError"); }],
    ["a network error", () => { throw new TypeError("fetch failed"); }],
  ];
  for (const [name, answer] of answers) {
    it(`step A ${name} archives on the same record`, { skip: HELD_LINUX_ONLY }, async () => {
      const s = await scenario({ setup: ({ server }) => { server.heldAnswer = answer; } });
      await assertArchived(s, `step A ${name}`, 1);
    });
  }

  // Without the feature nothing defers, so the flow is exactly main's: the archive is captured BEFORE the
  // failed send (and once more by the post-send settle, main's own double pass), never a held upload.
  for (const [name, options] of [
    ["an api without the held feature", { features: ["recovery_inventory_v1"] }],
    ["a feature withdrawn before the run", { setup: ({ server }: { server: HeldServer }) => { server.features.delete("recovery_held_publication_v1"); } }],
  ] as const) {
    it(`${name} archives exactly as before`, async () => {
      const freezes: boolean[] = [];
      const s = await scenario({ ...options, setup: async (ctx) => {
        await (options as { setup?: (c: typeof ctx) => void }).setup?.(ctx);
        freezes.push(...watchFreezes(ctx.server).failedAckedAtFreeze);
      } });
      const { claim, server, r } = s;
      assert.equal(failedStates(claim.run_id).length, 1);
      assert.equal(server.heldCalls.length, 0, "no held upload without the feature");
      assert.ok(s.capture.mock.callCount() >= 1, "the archive capture ran in the ordinary place");
      assert.ok(server.archiveUploads() >= 1);
      assert.equal(server.holdOpen(), true, "the hold stays open until the archive FINAL is ACKed");
      assert.equal(server.acked(), 0);
      assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]).length, 40);
      await redrive(r);
      assert.ok(server.finals.length >= 1);
      assert.ok(server.finals.every(f => f.disposition?.kind === "archive"), "only archive FINALs");
      assert.equal(server.acked(), 1);
    });
  }

  it("push_secret_blocked and worker_residue_blocked failures never defer or publish", async () => {
    const claim = gitlabClaim(2590, {
      claim_generation: 51, inventory_guarded: true,
      repo: { id: "r1", url: "https://github.com/org/repo", clone_url: fx.originPath, forge_type: "github" },
    });
    const { github } = fakeGitHub();
    const server = heldServer(claim);
    // The trusted pre-push scan finds a secret, so finalize fails typed as push_secret_blocked.
    git.secretScanRange = (async () => ({
      trusted: true, findings: [{ file: "leak.ts", startLine: 1, commit: "deadbeefcafe", ruleId: "generic-api-key" }],
    })) as typeof git.secretScanRange;
    git.pushBranch = (async () => { throw new Error("must not push"); }) as typeof git.pushBranch;
    const r = runner({ run: async ctx => {
      commit(ctx.worktreePath, "committed.txt", "work\n");
      return { branch: ctx.branch };
    } }, fakeGitlab().gitlab, JOIN_TOKEN, { github });
    const capture = mock.method(recoveryOf(r), "captureAndUpload");
    await r.execute(claim);
    assert.equal(failedStates(claim.run_id)[0]!.fail_origin, "push_secret_blocked");
    assert.equal(server.heldCalls.length, 0, "a secret-blocked run never reaches step A");
    assert.ok(capture.mock.callCount() >= 1, "it archives, as before");
    const residue = await scenario({ failWith: `${REASON_WORKER_RESIDUE_BLOCKED}: a process survived` });
    assert.equal(failedStates(residue.claim.run_id)[0]!.fail_origin, "worker_residue_blocked");
    assert.equal(residue.server.heldCalls.length, 0);
    assert.ok(residue.capture.mock.callCount() >= 1);
  });

  it("only a failed terminal can defer: cancelled, parked and completed never do", async () => {
    const { gitlab } = fakeGitlab();
    const r = runner({ run: async () => ({ branch: "x" }) }, gitlab, JOIN_TOKEN);
    heldServer(gitlabClaim(2591, { claim_generation: 52, inventory_guarded: true }));
    const eligible = (p?: { status?: string; fail_origin?: string }): boolean =>
      (r["heldEligibleTerminal"] as (p?: unknown) => boolean).call(r, p);
    assert.equal(eligible({ status: "failed" }), true);
    assert.equal(eligible({ status: "failed", fail_origin: "agent_failure" }), true);
    for (const status of ["cancelled", "completed", "paused", "limit_wait", "recovery_wait", "awaiting_approval", "credential_switch"]) {
      assert.equal(eligible({ status }), false, status);
    }
    assert.equal(eligible({ status: "failed", fail_origin: "push_secret_blocked" }), false);
    assert.equal(eligible({ status: "failed", fail_origin: "worker_residue_blocked" }), false);
    assert.equal(eligible(undefined), false);
  });

  it("the held path never receives the forge credential", async () => {
    const fetches: Array<{ failedAcked: boolean; args: unknown[] }> = [];
    const s = await scenario({
      setup: ({ server }) => {
        const real = git.fetchDefaultTip.bind(git);
        git.fetchDefaultTip = async (...a) => { fetches.push({ failedAcked: server.failedAcked(), args: a }); return real(...a); };
      },
    });
    const pat = "fixture-forge-pat-000000";
    assert.equal(s.server.heldCalls.length, 1);
    const call = s.server.heldCalls[0]!;
    assert.equal(JSON.stringify([call.run, call.generation, call.tip, call.coverage]).includes(pat), false);
    assert.equal(call.pack.includes(Buffer.from(pat)), false, "the pack carries no credential");
    assert.equal(fetches.filter(f => f.failedAcked).length, 0, "no credentialed fetch runs once the held path begins");
    assert.equal(JSON.stringify(fetches.map(f => f.args)).includes(pat), false);
  });
});

describe("a held FINAL the api refuses", () => {
  const definitive: Array<[string, (claim: { run_id: string; claim_generation?: number }) => Error | ReturnType<typeof retained>]> = [
    ["held_publication_ref_missing", c => retained(c, "held_publication_ref_missing")],
    ["held_publication_ref_mismatch", c => retained(c, "held_publication_ref_mismatch")],
    ["held_publication_not_eligible", c => retained(c, "held_publication_not_eligible")],
    ["held_publication_unsupported", c => retained(c, "held_publication_unsupported")],
    ["a downgrade 400", c => badRequest(c.run_id)],
  ];
  for (const [name, refusal] of definitive) {
    it(`definitive ${name} falls back to the archive, custody held until its FINAL is ACKed`, { skip: HELD_LINUX_ONLY }, async () => {
      const s = await scenario({
        setup: ({ claim, server }) => {
          server.finalAnswer = call => call.disposition?.kind === "held_publication"
            ? refusal(claim)
            : { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
        },
      });
      const { claim, server, work, r } = s;
      assert.equal(server.heldCalls.length, 1);
      const journaled = await assertCustodyRetained(server, claim, work, "held FINAL journaled");
      assert.equal(journaled.finalRequest?.disposition.kind, "held_publication");
      const seen: string[] = [];
      server.onFinal = async call => {
        seen.push(call.disposition!.kind);
        assert.equal(server.holdOpen(), true, "the hold is open when each FINAL is sent");
        assert.equal(server.acked(), 0, "nothing was released yet");
        assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]), journaled.inventoryCurrentSha);
        if (call.disposition!.kind === "archive") {
          const record = (await frozenRecords(claim))[0]!;
          assert.equal(record.captureId, journaled.captureId, "the archive is captured on the same record");
          assert.equal(record.finalAcknowledged, undefined);
        }
      };
      await redrive(r);
      assert.deepEqual(seen, ["held_publication", "archive"], "the refused held FINAL is replaced by the archive FINAL");
      assert.equal(server.archiveUploads(), 1);
      assert.equal(server.acked(), 1);
      assert.equal(server.holdOpen(), false);
      assert.equal(s.capture.mock.callCount(), 0, "the fallback capture ran from the replay, not the settle");
      assert.ok((await frozenRecords(claim)).length <= 1, "never a second record for the generation (cleanup may already have run)");
    });
  }

  for (const [name, answer] of [
    ["held_publication_unavailable", (c: { run_id: string; claim_generation?: number }) => retained(c, "held_publication_unavailable")],
    ["a transport error", () => new Error("socket hang up")],
    ["a 503", (c: { run_id: string }) => new Error("503 from " + c.run_id)],
  ] as const) {
    it(`a non-definitive ${name} keeps the journaled FINAL and the hold across a restart`, { skip: HELD_LINUX_ONLY }, async () => {
      const s = await scenario({
        setup: ({ claim, server }) => {
          server.finalAnswer = call => call.disposition?.kind === "held_publication"
            ? (answer as (c: typeof claim) => Error | ReturnType<typeof retained>)(claim)
            : { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
        },
      });
      const { claim, server, work, r } = s;
      const journaled = await assertCustodyRetained(server, claim, work, "held FINAL journaled");
      await redrive(r, 3);
      assert.ok(server.finals.length >= 1, "the held FINAL was tried");
      assert.ok(server.finals.every(f => f.disposition?.kind === "held_publication"), "only the held FINAL; never an archive");
      assert.ok(server.finals.every(f => JSON.stringify(f.disposition) === JSON.stringify(journaled.finalRequest!.disposition)),
        "every replay is the journaled request, verbatim");
      const kept = await assertCustodyRetained(server, claim, work, "after non-definitive refusals");
      assert.deepEqual(kept.finalRequest, journaled.finalRequest, "the journaled FINAL is kept, not cleared");
      assert.equal(server.archiveUploads(), 0, "no archive is captured for lack of fresh proof");
      assert.equal(s.capture.mock.callCount(), 0);

      // A restart replays the very same request; this time the api releases.
      server.finalAnswer = ({ disposition }) => ({ run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1, ...(disposition ? {} : {}) });
      const before = server.finals.length;
      const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
      await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
      assert.equal(server.finals.length, before + 1);
      assert.deepEqual(server.finals.at(-1)!.disposition, journaled.finalRequest!.disposition);
      assert.equal(server.acked(), 1);
      assert.equal(server.archiveUploads(), 0, "the held release needed no archive at all");
      assert.equal(server.holdOpen(), false);
    });
  }

  it("a client-side feature absence on replay is non-definitive: the FINAL is kept and the hold stays open", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario();
    const { claim, server, work, r } = s;
    const journaled = await assertCustodyRetained(server, claim, work, "held FINAL journaled");
    server.features.delete("recovery_held_publication_v1");
    await redrive(r, 3);
    assert.equal(server.finals.length, 0, "the client threw before sending anything");
    const kept = await assertCustodyRetained(server, claim, work, "feature absent on replay");
    assert.deepEqual(kept.finalRequest, journaled.finalRequest);
    assert.equal(server.archiveUploads(), 0);
    server.features.add("recovery_held_publication_v1");
    const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
    await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
    assert.equal(server.acked(), 1);
    assert.deepEqual(server.finals[0]!.disposition, journaled.finalRequest!.disposition);
  });

  it("a held FINAL kept as unavailable and definitively refused after a restart reaches the capture on the resumed pass", { skip: HELD_LINUX_ONLY }, async () => {
    const s = await scenario({
      setup: ({ claim, server }) => {
        server.finalAnswer = () => retained(claim, "held_publication_unavailable");
      },
    });
    const { claim, server, work, r } = s;
    const journaled = await assertCustodyRetained(server, claim, work, "held FINAL journaled");
    await redrive(r, 2);
    assert.equal(server.archiveUploads(), 0, "unavailable never falls back");
    assert.deepEqual((await assertCustodyRetained(server, claim, work, "kept")).finalRequest, journaled.finalRequest);

    // After the restart the api answers definitively: the held request is cleared and the SAME sweep
    // captures the archive on the same record and sends its FINAL.
    const seen: string[] = [];
    server.finalAnswer = call => call.disposition?.kind === "held_publication"
      ? retained(claim, "held_publication_ref_missing")
      : { run_id: claim.run_id, generation: claim.claim_generation!, released: true, holds_released: 1 };
    server.onFinal = async call => {
      seen.push(call.disposition!.kind);
      assert.equal(server.holdOpen(), true);
      assert.equal(server.acked(), 0);
      assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]), journaled.inventoryCurrentSha);
    };
    const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
    await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
    assert.deepEqual(seen.slice(-2), ["held_publication", "archive"]);
    assert.equal(server.archiveUploads(), 1);
    assert.equal(server.acked(), 1);
    assert.equal(server.holdOpen(), false);
  });

  it("a crash after the failed send resumes into the archive capture on the same record", async () => {
    const s = await scenario({
      setup: ({ r }) => {
        // The process dies after the failed terminal is ACKed and before the held step runs.
        mock.method(r as unknown as Record<string, (...a: unknown[]) => unknown>, "settleHeldDeferred", async () => undefined);
      },
    });
    const { claim, server } = s;
    assert.equal(server.failedAcked(), true);
    assert.equal(server.heldCalls.length, 0);
    assert.equal(server.holdOpen(), true);
    const frozen = await frozenRecords(claim);
    assert.equal(frozen.length, 1, "one frozen record survived the crash");
    assert.equal(frozen[0]!.finalRequest, undefined);
    assert.equal(command(bare(), ["rev-parse", "--verify", pinRef(claim)]), frozen[0]!.inventoryCurrentSha);
    assert.equal(server.archiveUploads(), 0);
    const seen: string[] = [];
    server.onFinal = async call => {
      seen.push(call.disposition!.kind);
      assert.equal(server.holdOpen(), true);
    };
    const r2 = runner({ run: async () => ({ branch: "x" }) }, fakeGitlab().gitlab, JOIN_TOKEN);
    await r2.resumePendingRecoveries(undefined, await r2.snapshotBootRecoveries());
    assert.equal(server.archiveUploads(), 1, "the restart captured the archive");
    assert.deepEqual(seen, ["archive"]);
    assert.equal(server.heldCalls.length, 0, "a restart never publishes held work");
    assert.equal(server.acked(), 1);
  });
});

describe("a WIP transfer that fails retains custody", () => {
  it("freezes nothing, uploads nothing and sends no FINAL; the hold and the sources stay", async () => {
    let broken = false;
    const s = await scenario({
      setup: ({ server }) => {
        const real = git.fetchAgentBranch.bind(git);
        git.fetchAgentBranch = async (...a) => { if (broken) throw new Error("injected transfer failure"); return real(...a); };
        watchFreezes(server);
      },
      extra: () => { broken = true; },
    });
    const { claim, server, work, r } = s;
    assert.equal(failedStates(claim.run_id).length, 1, "the honest failure is still reported");
    assert.equal(server.heldCalls.length, 0);
    assert.equal(s.capture.mock.callCount(), 0);
    assert.equal(server.archiveUploads(), 0);
    assert.equal(server.finals.length, 0, "no FINAL");
    assert.equal(server.holdOpen(), true, "the hold stays open");
    assert.equal(server.acked(), 0);
    assert.equal((await frozenRecords(claim)).length, 0, "nothing was frozen");
    assert.ok(fs.existsSync(work.clone!), "the runner clone is retained");
    assert.ok(fs.existsSync(bare()));
    await redrive(r);
    assert.equal(server.finals.length, 0);
    assert.equal(server.holdOpen(), true);
  });
});

describe("the held decision follows the durable first winner", () => {
  it("a completed first winner at the journaled caller never defers, whatever this caller's body", async () => {
    const { gitlab } = fakeGitlab();
    const claim = gitlabClaim(2592, { claim_generation: 53, inventory_guarded: true });
    heldServer(claim);
    const { Outbox } = await import("../src/outbox.js");
    const { nullLogger } = await import("./helpers.js");
    const outbox = new Outbox({
      root: path.join(fx.dataDir, "outbox"), log: nullLogger(), runMaxBytes: 64 * 1024 * 1024,
      maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000,
    });
    await outbox.init();
    const r = runner({ run: async () => ({ branch: "x" }) }, gitlab, JOIN_TOKEN, { outbox, outboxTerminalMaxBytes: 1 << 20, gapFillMax: 100 });
    // The competing winner is already durable: a completed terminal.
    assert.equal((await outbox.journalTerminal(claim.run_id, 53, "finalize", 0, { status: "completed", claim_generation: 53 })).journaled, true);
    const prepared: Array<unknown> = [];
    const flight = {
      runId: claim.run_id, claimGeneration: 53, batcher: { currentSeq: () => 0 },
      steering: { claimFence: () => undefined }, runLog: { info() {}, warn() {}, error() {} },
      completionSendAttempted: false, terminalResolved: false,
      prepareTerminalInventory: async (pending: unknown) => { prepared.push(pending); },
    };
    const journalAndSend = r["journalAndSendTerminal"] as (f: unknown, phase: string, body: unknown, send: unknown) => Promise<void>;
    await journalAndSend.call(r, flight, "finalize", { status: "failed", failure_reason: "late failure", fail_origin: "agent_failure" },
      async () => ({ applied: true, status: "completed" })).catch(() => undefined);
    assert.ok(prepared.length > 0, "the journaled caller prepared the terminal inventory");
    for (const pending of prepared) {
      assert.deepEqual(pending, { status: "completed" }, "the decision reads the durable winner, not the caller's failed body");
      assert.equal((r["heldEligibleTerminal"] as (p: unknown) => boolean).call(r, pending), false);
    }
  });
});
