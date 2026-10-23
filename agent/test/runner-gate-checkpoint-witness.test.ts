import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { Readable } from "node:stream";
import { PlanRejectedError, type Executor } from "../src/executor.js";
import fs from "node:fs";
import path from "node:path";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { mockCommittedTracking } from "./runner-tracking-fixture.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, input, installHarness, runnerWith } from "./runner-harness.js";

installHarness();
const H = "a".repeat(40);
const P = "b".repeat(40);
const ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";

describe("M1 gate checkpoint witness", () => {
  for (const mode of ["published", "claude-false", "claude-throw", "failed", "unrelated", "moved", "legacy", "initial", "unchanged",
    "unchanged-empty", "absent-candidate", "removed", "frozen-empty", "reordered", "resumed", "resumed-empty", "held-approve", "revise", "revise-unconfirmed"] as const) {
    it(`${mode}: only a confirmed recoverable publish presents a witness`, async () => {
      const order: string[] = [];
      const old = [{ id: "m1", title: "Original" }, { id: "m2", title: "Second" }];
      const candidate = mode === "absent-candidate" ? undefined :
        mode === "removed" || mode === "unchanged-empty" || mode === "resumed-empty" ? [] :
        mode === "unchanged" ? old.map((m) => ({ title: m.title, id: m.id })) :
        mode === "reordered" ? [...old].reverse() : [{ id: "m1", title: "Replacement" }];
      const ordinary = ["legacy", "initial", "unchanged", "unchanged-empty", "absent-candidate"].includes(mode);
      const resumed = mode === "resumed" || mode === "resumed-empty";
      const claim = gitlabClaim(2944, {
        claim_generation: 7,
        ...(mode === "initial" ? {} : { execution_milestones: mode === "frozen-empty" || mode === "unchanged-empty" ? [] : old }),
        ...(resumed ? {
          ...(mode === "resumed-empty" ? { milestones: [] } : {}),
          plan_md: "Plan", resume_phase: "awaiting_approval",
          resume_plan_at: "2000-01-01T00:00:00.000000Z",
          resume_gate_revision: 2, resume_gate_presentation_id: ID,
        } : {}),
      });
      client.protocolFeatures = mode === "legacy" ? [] : ["milestone_reconciliation_v1", "claim_generation_fence", "gate_revision_v1"];
      api.gateRevisions = mode !== "legacy";
      api.stampGateBindings = mode !== "legacy";
      const outbox = new Outbox({ root: path.join(fx.dataDir, "outbox"), log: nullLogger(),
        runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 86_400_000 });
      await outbox.init();
      const terminalWrites: unknown[] = [];
      const journalTerminal = outbox.journalTerminal.bind(outbox);
      outbox.journalTerminal = async (...args) => { terminalWrites.push(args); return journalTerminal(...args); };
      let sourcePath = "";
      let refusal: unknown;
      let returned = false;
      const { gitlab } = fakeGitlab();
      const executor: Executor = {
        resumesAtGate: resumed,
        killAgentTree() { order.push("quiesce"); },
        async settleWorkerCommands() {
          if (!sourcePath || !mode.startsWith("claude-")) return { confirmed: true };
          order.push("settle");
          if (mode === "claude-throw") throw new Error("poisoned Claude command settlement");
          return { confirmed: false };
        },
        async run(ctx) {
          sourcePath = ctx.worktreePath;
          git.worktreeStatus = async () => [];
          git.worktreeHead = async () => mode === "moved" && order.includes("ancestry") ? "c".repeat(40) : H;
          git.planChangedFiles = async () => [];
          mockCommittedTracking(git, H);
          git.verifyRunnerTrackingCovers = async () => true;
          git.isAncestorRef = async (_bare, source, published) => {
            assert.equal(source, H);
            assert.equal(published, P);
            order.push("ancestry");
            return mode !== "unrelated";
          };
          git.checkpointPack = async () => {
            order.push("pack");
            assert.ok(order.includes("quiesce"), "source is quiesced before packing");
            const open = async () => ({ pack: Readable.from(["PACK"]), exited: Promise.resolve(0), cancel() {} });
            return { tipOid: P, ...await open(), replay: open };
          };
          client.publishCheckpoint = async (_run, tip, pack, _signal, generation) => {
            assert.equal(generation, 7, "the original flight generation authorizes the pack");
            assert.equal(tip, P);
            for await (const _chunk of pack) { /* settle the pack producer */ }
            order.push("publish");
            return mode === "failed" ? { ok: false, httpStatus: 400 } :
              { ok: true, body: { published: true, ref: "refs/uzi-checkpoints/agent/issue-2944" } };
          };
          try {
            const verdict = await ctx.gatePlan!("Plan", mode === "resumed-empty" ? ctx.frozenMilestones ?? undefined : candidate);
            returned = true;
            assert.equal(verdict.kind, mode === "revise" ? "revise" : "reject");
            if (mode === "revise") assert.ok(order.includes("running"), "revision settles running before another provider turn");
          } catch (error) {
            refusal = error;
            if (mode !== "held-approve" && mode !== "revise-unconfirmed") run.shutdown();
            throw error;
          }
          throw new PlanRejectedError("fixture rejection");
        },
      };
      const run = runnerWith(() => ({ executor, homeDir }), gitlab, undefined, undefined, { outbox, checkpointIntervalMs: 0, planApprovalTimeoutMs: 2_000, recoveryRetryMs: 1 });
      api.onState(claim.run_id, (body) => {
        if (body.status === "running" && order.includes("gate")) order.push("running");
        if (body.status !== "awaiting_approval") return;
        order.push("gate");
        if (mode === "revise-unconfirmed") {
          const report = client.reportState.bind(client);
          client.reportState = async (id, b, ...rest) => b.status === "running"
            ? { applied: false, status: "awaiting_approval" } : report(id, b, ...rest);
        }
        api.setInputs(claim.run_id, [input(mode === "held-approve" ? "approve_plan" : mode.startsWith("revise") ? "revise_plan" : "reject_plan", mode.startsWith("revise") ? "Revise the replacement" : undefined)]);
      });
      let recoveredClaim = claim;
      if (mode === "resumed-empty") {
        // Exercise the real HTTP JSON decoder, rather than passing the candidate
        // directly to gatePlan. Go's claim-wire regression pins the server half.
        api.enqueueClaim(JSON.parse(JSON.stringify(claim)));
        const decoded = await client.claimRun();
        assert.ok(decoded);
        assert.deepEqual(decoded.milestones, []);
        recoveredClaim = decoded;
      }
      await run.execute(recoveredClaim);
      const gates = api.states.filter((s) => s.runId === claim.run_id && s.body.status === "awaiting_approval");
      if (!ordinary && mode !== "failed" && mode !== "unrelated" && mode !== "moved") {
        assert.equal(gates.length, 1);
        if (mode.startsWith("claude-")) {
          const beforePack = order.slice(0, order.indexOf("pack"));
          assert.ok(beforePack.filter(step => step === "settle").length >= 2,
            "poisoned settlement reaches the outer entry and nested restore capture");
          assert.ok(beforePack.filter(step => step === "quiesce").length >= 2,
            "nested restore capture still proves quiescence and re-proves after runner git");
        }
        assert.deepEqual(gates[0]!.body.gate_checkpoint_witness, { source_head: H, published_checkpoint: P });
        assert.equal(gates[0]!.body.claim_generation, 7, "presentation retains the frame generation");
        assert.equal(order.slice(0, order.indexOf("gate")).filter((step) => step === "pack").length, 1, "capture packs once");
        assert.equal(order.slice(0, order.indexOf("gate")).filter((step) => step === "publish").length, 1, "gate reuses the capture's confirmed publication");
        assert.ok(order.indexOf("publish") < order.indexOf("ancestry"));
        assert.ok(order.indexOf("ancestry") < order.indexOf("gate"));
        assert.deepEqual(gates[0]!.body.milestones, candidate);
        if (resumed) {
          assert.ok(gates[0]!.body.presentation_id);
          assert.notEqual(gates[0]!.body.presentation_id, ID, "new capture cannot reuse the previous immutable presentation");
          assert.equal(gates[0]!.body.adopt_gate_revision, undefined);
        }
        if (mode === "held-approve" || mode === "revise-unconfirmed") {
          assert.ok(refusal instanceof Error);
          assert.deepEqual(terminalWrites, [], "stopped predecessor journals no terminal outcome");
          assert.deepEqual(api.states.filter(s => s.runId === claim.run_id && ["completed", "failed"].includes(s.body.status)), [],
            "stopped predecessor reports no terminal outcome");
          assert.ok(fs.existsSync(sourcePath), "stopped source clone remains in custody");
          assert.equal(order.slice(order.indexOf("gate") + 1).includes("publish"), false, "no publish after the gate");
          assert.equal(returned, false, "held approval cannot restart the predecessor");
        } else {
          assert.equal(refusal, undefined);
          assert.equal(returned, true);
        }
      } else if (ordinary) {
        assert.equal(gates.length, 1);
        assert.equal(gates[0]!.body.gate_checkpoint_witness, undefined);
        assert.equal(order.includes("pack"), false);
        assert.equal(order.includes("publish"), false);
        assert.equal(order.indexOf("quiesce") < order.indexOf("gate") && order.includes("quiesce"), false,
          "ordinary gates do not quiesce execution before presenting");
        assert.deepEqual(gates[0]!.body.milestones, candidate);
        assert.equal(Object.hasOwn(gates[0]!.body, "milestones"), candidate !== undefined);
        assert.equal(returned, true);
      } else {
        assert.equal(gates.length, 0, "no unready presentation or local-only witness");
        assert.ok(refusal instanceof TransientRecoveryError);
        assert.equal(returned, false, "executor cannot resume from the refused gate");
      }
    });
  }
});
