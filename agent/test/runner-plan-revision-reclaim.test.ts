import assert from "node:assert/strict";
import { it } from "node:test";
import { setTimeout as delay } from "node:timers/promises";
import { api, client, fakeGitlab, installHarness, runner } from "./runner-harness.js";
import { feed, freshClaim, gates, newApi, row, send, until } from "./gate-revision-harness.js";
import { providerFixture } from "./plan-revision-provider-fixture.js";
import type { ClaimResponse } from "../src/protocol.js";
import type { RunContext } from "../src/executor.js";

installHarness();
const SOURCE = "# Source R";
const REVISED = "# Successor J";
const FRESH = "# Fresh Codex plan";
function barrier() {
  let release!: () => void;
  return { promise: new Promise<void>(resolve => { release = resolve; }), release: () => release() };
}

// These are serial worker flights. They exercise existing resume fields, not a
// durable retry checkpoint, exactly-once provider execution, or lifetime allowance.
for (const harness of ["claude", "codex"] as const) {
  for (const stage of ["expired", "J queued", "J publication uncommitted", "J committed before ACK"] as const) {
    it(`reclaim ${harness}: loss at ${stage}`, async t => {
      newApi();
      api.planRevisionRetries = true;
      client.protocolFeatures = [...client.protocolFeatures, "plan_revision_retry_v1"];
      const old = providerFixture(harness, n => n === 1 ? { plan: SOURCE }
        : n <= 9 ? { failure: true } : { plan: REVISED });
      const claim = freshClaim(1, { config: { plan_max_revisions: 5 },
        ...(harness === "codex" ? { secrets: { forge_pat: "fixture-forge-pat", codex: old.credential } } : {}) });
      const id = claim.run_id;
      const hold = barrier();
      const arrived = barrier();
      const nextRevision = barrier();
      const nextRevisionBlocked = barrier();
      let restore = () => {};
      const oldFlight = runner(old.executor, fakeGitlab().gitlab, undefined, {
        planRevisionRetryPolicy: { random: () => 0, sleep: async (ms, signal) => {
          signal.throwIfAborted();
          // Only the live expiry polling tick uses a timer; 5/10/20s
          // automatic backoffs are injected and consume no wall-clock seconds.
          if (old.starts.length >= 9) await delay(ms, undefined, { signal });
        } },
      });
      const oldDone = oldFlight.execute(claim);
      let nextFlight: ReturnType<typeof runner> | undefined;
      let nextDone: Promise<void> | undefined;
      try {
        assert.ok(await until(() => gates(id).length === 1), "source persisted");
        const source = api.gateOf(id);
        const [i] = send(id, row("revise_plan", "old I"));
        assert.ok(await until(() => feed(id).some(t => t.includes("retry allowance exhausted"))));
        assert.equal(old.starts.length, 9, "real inner retries and all three redrives exhausted");
        assert.equal(api.isApplied(id, i!.id), false);
        let j: ReturnType<typeof row> | undefined;
        if (stage === "J publication uncommitted") {
          restore = api.dropStatesWhen(id, body => {
            if (body.plan_md !== REVISED) return false;
            arrived.release();
            return true;
          }, hold.promise);
        } else if (stage === "J committed before ACK") {
          api.afterPersistState(id, body => {
            if (body.plan_md !== REVISED) return false;
            arrived.release();
            return true;
          }, hold.promise);
        }
        if (stage !== "expired") [j] = send(id, row("revise_plan", "old J"));
        if (stage === "J publication uncommitted" || stage === "J committed before ACK") {
          assert.ok(await until(() => old.starts.length === 10), "J taken by real driver");
          let reached = false;
          void arrived.promise.then(() => { reached = true; });
          assert.ok(await until(() => reached), "J report reached persistence seam");
        }
        // J queued is stopped synchronously after enqueue, before another poll
        // can take it. Held HTTP seams are released on shutdown so cleanup drains.
        oldFlight.shutdown();
        hold.release();
        await oldDone;
        if (stage === "J queued") assert.equal(old.starts.length, 9);
        const committed = stage === "J committed before ACK";
        if (harness === "codex") assert.ok(old.starts.every(s => s.threadId === "th-revision"), "all retry and replacement turns keep the returned thread identity");
        if (!committed) {
          assert.equal(gates(id).some(g => g.plan_md === REVISED), false, "unknown/uncommitted publication produces no successful plan report");
          assert.equal(api.inputReceiptCalls.some(c => c.kind === "applied" && c.ids.includes(i!.id)), false, "no false successful revision receipt before reclaim");
        }
        assert.equal(api.isApplied(id, i!.id), committed);
        if (j) assert.equal(api.isApplied(id, j.id), committed);
        assert.equal(api.isInputSuperseded(id, i!.id), committed);
        assert.equal(api.gateOf(id).revision, committed ? source.revision + 1 : source.revision);
        restore();

        const persisted = committed ? REVISED : SOURCE;
        const successor = providerFixture(harness, () => harness === "claude" ? { failure: true } : { plan: FRESH });
        if (harness === "claude" && !committed) {
          const run = successor.executor.run.bind(successor.executor);
          t.mock.method(successor.executor, "run", (ctx: RunContext) => run({
            ...ctx,
            beforePlanRevisionTurn: async () => {
              // After I exhausts its fresh allowance, queued J may immediately
              // start. Hold that next turn while inspecting I's eight starts.
              if (successor.starts.length >= 8) {
                nextRevisionBlocked.release();
                await nextRevision.promise;
              }
              await ctx.beforePlanRevisionTurn?.();
            },
          }));
        }
        api.setInputClaimGeneration(id, 2);
        api.setOwnershipStatus(id, "awaiting_approval", 2);
        const resumed: ClaimResponse = { ...claim, claim_generation: 2,
          last_seq: Math.max(0, ...api.messages(id).map(m => m.seq)),
          resume_phase: "awaiting_approval", plan_md: persisted, plan_source: "agent", plan_approved: false,
          resume_plan_at: api.resumePlanAt(id), ...api.gateResumeFields(id),
        };
        const before = gates(id).length;
        const receiptStart = api.inputReceiptCalls.length;
        const waits: number[] = [];
        nextFlight = runner(successor.executor, fakeGitlab().gitlab, undefined, {
          planRevisionRetryPolicy: { random: () => 0, sleep: async (ms, signal) => {
            signal.throwIfAborted();
            if (successor.starts.length >= 8) await delay(ms, undefined, { signal });
            else waits.push(ms);
          } },
        });
        nextDone = nextFlight.execute(resumed);
        if (harness === "claude" && !committed) {
          assert.ok(await until(() => successor.starts.length === 8), "new claim gets a fresh three-redrive allowance");
          assert.ok(await until(() => feed(id).some(t => t.includes("generation 2,") && t.includes("retry allowance exhausted"))));
          if (j) await nextRevisionBlocked.promise;
          assert.equal(successor.starts.length, 8, "inspect only I's completed allowance before J can start");
          assert.ok(successor.starts.every(s => s.prompt.includes("old I")), "FIFO replay starts I, not reconstructed J flight");
          assert.ok(successor.starts.every(s => !s.prompt.includes("old J")));
          assert.equal(waits.reduce((a, b) => a + b, 0), 35000);
          assert.equal(api.isApplied(id, i!.id), false);
          if (j) assert.equal(api.isApplied(id, j.id), false);
          assert.equal(api.isInputSuperseded(id, i!.id), false);
          assert.equal(api.gateOf(id).revision, source.revision, "replay stays on source R");
          assert.ok(gates(id).slice(before).every(g => g.claim_generation === 2 && g.expected_gate_revision === source.revision));
        } else {
          assert.ok(await until(() => gates(id).length > before), "successor presents a gate");
          const publication = gates(id).at(-1)!;
          assert.equal(publication.claim_generation, 2);
          assert.equal(publication.plan_md, harness === "codex" ? FRESH : REVISED);
          assert.equal(publication.revision_input_id, undefined);
          assert.equal(publication.superseded_revision_input_ids, undefined, "no reconstructed predecessors");
          if (harness === "codex") {
            assert.equal(successor.starts.length, 1);
            assert.equal(successor.starts[0]!.threadId, "th-revision", "turn/start uses the provider thread identity");
            if (!committed) {
              assert.ok(await until(() => feed(id).some(t => /ignored.*re-send/.test(t))), "old feedback needs resending");
              // Generic stale APPLIED disposal is not a revision persistence receipt.
              assert.equal(api.isInputSuperseded(id, i!.id), false);
              if (j) assert.equal(api.isInputSuperseded(id, j.id), false);
            }
          } else assert.equal(successor.starts.length, 0, "committed plan is re-presented without provider work");
        }
        assert.equal(api.humanPlanApproved(id), false);
        const [staleApproval] = send(id, row("approve_plan", null, { gate_binding: "bound", gate_revision: 999 }));
        assert.ok(await until(() => api.isAcked(id, staleApproval!.id)));
        assert.equal(api.isApplied(id, staleApproval!.id), false);
        assert.equal(api.humanPlanApproved(id), false);
        assert.ok(api.inputReceiptCalls.slice(receiptStart).every(c => c.generation === 2), "reclaim input receipts use current generation");
        if (committed) {
          assert.ok(api.isApplied(id, i!.id) && api.isApplied(id, j!.id));
          assert.ok(api.isInputSuperseded(id, i!.id));
          assert.equal(api.isInputSuperseded(id, j!.id), false);
        }
        send(id, row("cancel"));
        nextRevision.release();
        await nextDone;
      } finally {
        hold.release();
        oldFlight.shutdown();
        await oldDone;
        restore();
        nextFlight?.shutdown();
        nextRevision.release();
        await nextDone;
      }
    });
  }
}
