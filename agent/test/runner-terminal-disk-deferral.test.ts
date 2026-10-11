import { describe, it } from "node:test";
import { ProviderPolicyRefusal, admitPolicyTurn } from "../src/provider-policy-refusal.js";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { DiskParkSignal } from "../src/cache-cap.js";
import { TransientRecoveryError } from "../src/sdk-executor.js";
import { PlanRejectedError } from "../src/executor.js";
import { Outbox } from "../src/outbox.js";
import { nullLogger } from "./helpers.js";
import { api, client, fx, git, installHarness } from "./runner-harness.js";
import {
  opaqueBoundaryCases,
  trustedBoundaryCases,
} from "./fixtures/trusted-execution-refusals.js";
import {
  ROOMY,
  FULL,
  QUIESCENT,
  fixture,
  parks,
  settlementGovernor,
  waitForAbort,
  supervisedSafety,
  LINUX_CAPTURE,
  assertTrustedRefusalCase,
  terminalRefusalCases,
  splitRefusalRepresentations,
} from "./runner-terminal-disk-deferral-fixture.js";

installHarness();

// Each sibling owns one form; future forms stay in this file's complement.
const ownedRepresentations = new Set<string>(splitRefusalRepresentations);
const fullRepresentations = new Set(terminalRefusalCases.map(entry => entry.representation));
const complementCases = terminalRefusalCases.filter(entry => !ownedRepresentations.has(entry.representation));
const complementRepresentations = new Set(complementCases.map(entry => entry.representation));
assert.equal(splitRefusalRepresentations.length, 2, "one representation per refusal sibling");
assert.equal(ownedRepresentations.size, splitRefusalRepresentations.length, "sibling ownership is disjoint");
assert.deepEqual(new Set([...ownedRepresentations, ...complementRepresentations]), fullRepresentations,
  "owned forms and the complement cover the full refusal matrix");
assert.ok([...complementRepresentations].every(form => !ownedRepresentations.has(form)),
  "owned forms and the complement are disjoint");

describe("terminal execution disk deferral", () => {
  for (const tag of ["cyberPolicy", "misalignmentPolicyViolation"] as const) it(`policy refusal (#2321) ${tag} remains terminal on a full data volume`, async () => {
    const error = new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
      provider: "codex", category: "policy_refusal", origin: "root", policy_tag: tag });
    const f = fixture({ error });
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 1);
    assert.equal(f.reclaims(), 0, "policy refusal never enters the disk reclaim/retry path");
    assert.equal(parks().length, 0);
    const failed = api.states.find(s => s.body.status === "failed")!.body;
    assert.equal(failed.fail_origin, "provider_policy_refusal");
    assert.equal(failed.failure_reason, `Codex provider safety-policy refusal (${tag})`);
  });

  it("policy refusal retains its origin when terminal retirement cannot prove quiescence", async () => {
    const error = new ProviderPolicyRefusal({ ...admitPolicyTurn("implement"), event: "provider_policy_refusal",
      provider: "codex", category: "policy_refusal", origin: "root", policy_tag: "cyberPolicy" });
    let proofs = 0;
    const f = fixture({ error, runner: { quiesceRun: async req => {
      if (req.site !== "terminal_retire") return QUIESCENT;
      proofs++;
      return { ...QUIESCENT, process: { state: "unverified", processes: [], killed: [], detail: "unknown process" } };
    } } });
    await f.runner.execute(f.claim);
    assert.equal(proofs, 1);
    assert.equal(api.states.find(s => s.body.status === "failed")?.body.fail_origin, "provider_policy_refusal");
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(fs.readFileSync(path.join(f.clone(), "DIRTY.txt"), "utf8"), "dirty sentinel\n");
  });

  it("setup failure on a full volume keeps the original failure path", async () => {
    const f = fixture();
    git.createOrAttachRunnerClone = async () => { f.setFull(); throw new Error("opaque setup failure"); };
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 0);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, "opaque setup failure");
  });

  it("successful execution followed by finalize failure never takes occupancy deferral", async () => {
    const f = fixture({ result: { branch: "agent/issue-2201" } });
    git.pushBranch = async () => { throw new Error("opaque finalize failure"); };
    await f.runner.execute(f.claim);
    assert.equal(f.calls(), 1);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "failed"), true);
  });

  it("an actual awaiting-approval gate cannot become a disk park", async () => {
    const f = fixture({ afterWork: async (ctx) => {
      const verdict = await ctx.gatePlan!("# Plan\n- implement the approved work", [], async () => {
        api.setInputs(f.claim.run_id, [{ id: 1, kind: "revise_plan", body: "tighten the scope" }]);
      });
      assert.equal(verdict.kind, "revise");
      throw new Error("revision turn failed");
    } });
    await f.runner.execute({ ...f.claim, auto_approve: false });
    assert.equal(api.states.some((s) => s.body.status === "awaiting_approval"), true);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "failed"), true);
  });

  it("durable terminal write-ahead wins over coincident full occupancy", async () => {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "terminal-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    const f = fixture({ runner: { outbox }, beforeReject: async () => {
      const result = await outbox.journalTerminal(f.claim.run_id, 4, "running", 0,
        { status: "failed", failure_reason: "durable original failure", claim_generation: 4 });
      assert.equal(result.journaled, true);
    } });
    await f.runner.execute(f.claim);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.filter((s) => s.body.status === "failed").length, 0);
    assert.equal(outbox.hasPendingTerminal(f.claim.run_id, 4), true);
    assert.equal((await outbox.readTerminalJournal(f.claim.run_id, 4))?.body.failure_reason, "durable original failure");
  });

  it("a finalize journal write failure after execution success stays best effort", async () => {
    const outbox = new Outbox({ root: path.join(fx.dataDir, "finalize-outbox"), log: nullLogger(),
      runMaxBytes: 64 * 1024 * 1024, maxBytes: 512 * 1024 * 1024, retentionMs: 7 * 86_400_000 });
    await outbox.init();
    let journaled = 0;
    outbox.journalFinalize = async () => { journaled++; throw new Error("journal write refused"); };
    const f = fixture({ result: { branch: "agent/issue-2201", reportOnly: true }, runner: { outbox } });
    await f.runner.execute(f.claim);
    assert.equal(journaled, 1);
    assert.equal(f.reclaims(), 0);
    assert.equal(parks().length, 0);
    assert.equal(api.states.some((s) => s.body.status === "completed"), true);
  });

  for (const value of [new Error("same reused error"), "same primitive", undefined, NaN]) {
    for (const success of [false, true]) {
      it(`settlement replacement cannot defer: ${String(value)}, executor success=${success}`, async () => {
        const f = fixture({ error: value, result: success ? { branch: "agent/issue-2201" } : undefined,
          runner: { diskGovernor: settlementGovernor(value) } });
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 1);
        assert.equal(f.reclaims(), 0);
        assert.equal(parks().length, 0);
        assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
      });
    }
  }

  for (const mode of ["empty", "absent", "incomplete"] as const) {
    it(`Codex supervisor ${mode} must affirmatively enter a successful boundary`, async () => {
      let markers = 0;
      git.commitWipMarker = async () => { markers++; return false; };
      const f = fixture({ safety: supervisedSafety(mode),
        runner: { quiesceRun: async () => ({ ...QUIESCENT, process: undefined }) } });
      await f.runner.execute(f.claim);
      assert.equal(parks().length, mode === "empty" ? 1 : 0);
      assert.equal(markers, mode === "empty" ? 3 : 0);
      assert.equal(fs.existsSync(f.clone()), true);
    });
  }

  for (const site of ["reclaim", "capture", "coincident"] as const) {
    for (const control of ["cancel", "pause", "shutdown"] as const) {
      it(`${control} during ${site} wins over full occupancy`, { skip: site === "capture" && LINUX_CAPTURE }, async () => {
        let signal: AbortSignal;
        let fired = false;
        const interrupt = async () => {
          if (fired) return;
          fired = true;
          if (control === "shutdown") f.runner.shutdown();
          else api.setInputs(f.claim.run_id, [{ id: 1, kind: control, ...(control === "pause" ? { body: "now" } : {}) }]);
          await waitForAbort(signal);
        };
        const f = fixture({ observe: (ctx) => { signal = ctx.signal!; },
          beforeReject: site === "coincident" ? interrupt : undefined,
          reclaim: site === "reclaim" ? interrupt : undefined });
        if (control === "shutdown" && site === "coincident") git.commitWipMarker = async () => false;
        if (site === "capture") {
          const status = git.worktreeStatus.bind(git);
          git.worktreeStatus = async (...args) => { await interrupt(); return status(...args); };
        }
        await f.runner.execute(f.claim);
        assert.equal(f.calls(), 1);
        assert.equal(parks().filter((s) => s.body.recovery_cause === "data_volume_full").length, 0);
        if (control === "shutdown") {
          assert.equal(api.states.some((s) => s.body.status === "failed"), false);
          // A pre-existing shutdown uses its original checkpoint/clone retirement path.
          // Interrupts after this policy begins must retain the still-unverified clone.
          if (site !== "coincident") assert.equal(fs.existsSync(f.clone()), true);
          assert.equal(fs.existsSync(path.join(f.home, "session")), true);
        } else if (control === "pause") assert.equal(api.states.some((s) => s.body.status === "failed"), false);
        else assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, "run cancelled");
      });
    }
  }

  for (const location of ["source", "proof", "post-WIP proof", "report"] as const) {
    it(`typed plan rejection raised during ${location} uses canonical failure`, { skip: location !== "proof" && LINUX_CAPTURE }, async () => {
      const error = new PlanRejectedError("capture policy rejected");
      const f = fixture({ runner: { quiesceRun: async (req) => {
        if ((location === "proof" && req.site === "terminal_disk:after_runner_git")
          || (location === "post-WIP proof" && req.site === "recovery_capture:after_runner_git")) throw error;
        return QUIESCENT;
      } } });
      if (location === "source") git.commitWipMarker = async () => { throw error; };
      const report = client.reportState.bind(client);
      if (location === "report") client.reportState = async (id, body, signal) => {
        if (body.status === "recovery_wait") throw error;
        return report(id, body, signal);
      };
      try {
        await f.runner.execute(f.claim);
        assert.equal(parks().length, 0);
        assert.equal(api.states.find((s) => s.body.status === "failed")?.body.failure_reason, error.message);
      } finally { client.reportState = report; }
    });
  }

  for (const [name, options] of [
    ["roomy", { sample: ROOMY }], ["unknown statfs", { unknown: true }],
    ["missing files", { sample: { ...FULL, files: undefined } }],
    ["missing ffree with positive files", { sample: { ...FULL, ffree: undefined } }],
    ["HOME device mismatch", { mismatch: "home" }], ["clone device mismatch", { mismatch: "clone" }],
    ["HOME stat missing", { missingStat: "home" }], ["clone stat missing", { missingStat: "clone" }],
    ["old API", { features: ["claim_generation_fence"] }], ["no stamping", { features: ["recovery_cause_data_volume_full"] }],
    ["missing ownership generation", { generation: null }], ["awaiting approval revision", { status: "awaiting_approval" }],
  ] as const) {
    it(`keeps failure for ${name}`, async () => {
      const f = fixture(options);
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.filter((s) => s.body.status === "failed").length, 1);
    });
  }
  for (const options of [{ generation: 5 }, { status: "completed" }]) {
    it(`stops silently for confirmed ${JSON.stringify(options)}`, async () => {
      const f = fixture(options);
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(api.states.some((s) => s.body.status === "failed"), false);
    });
  }

  // Audit data travels with the reusable cases in the milestone checkpoint.
  for (const entry of complementCases) {
    it(`trusted refusal: ${entry.reason} [${entry.representation}]`, () => assertTrustedRefusalCase(entry));
  }

  for (const [name, error] of trustedBoundaryCases) {
    it(`trusted boundary: ${name}`, async () => {
      const rejection = error();
      const f = fixture({ error: rejection });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, 0);
      assert.equal(f.forge.calls.length, 0);
      const failures = api.states.filter((s) => s.body.status === "failed");
      assert.equal(failures.length, 1);
      assert.equal(failures[0]!.body.failure_reason, rejection.message);
    });
  }
  for (const [name, error] of opaqueBoundaryCases) {
    it(`opaque boundary defers: ${name}`, { skip: LINUX_CAPTURE }, async () => {
      const f = fixture({ error: error() });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 1);
      assert.equal(parks().length, 1);
      assert.equal(parks()[0]!.body.recovery_cause, "data_volume_full");
      assert.equal(api.states.some((s) => s.body.status === "failed" || s.body.status === "completed"), false);
      assert.equal(f.forge.calls.length, 0);
    });
  }

  it("five consecutive safety blocks fail retained, without a disk park", async () => {
    let proofs = 0;
    const f = fixture({ runner: { quiesceRun: async (req) => {
      if (req.site === "terminal_disk:after_runner_git") {
        proofs++;
        return { ...QUIESCENT, process: { state: "unverified", processes: [], killed: [], detail: "unknown process" } };
      }
      return QUIESCENT;
    } } });
    await f.runner.execute(f.claim);
    assert.equal(proofs, 5);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(f.clone()), true);
    assert.equal(fs.existsSync(path.join(f.home, "session")), true);
  });

  it("absent process proof cannot authorize degraded capture", async () => {
    const f = fixture({ runner: { quiesceRun: async () => ({ ...QUIESCENT, process: undefined }) } });
    await f.runner.execute(f.claim);
    assert.equal(parks().length, 0);
    assert.equal(api.states.find((s) => s.body.status === "failed")?.body.fail_origin, "worker_residue_blocked");
    assert.equal(fs.existsSync(f.clone()), true);
  });

  for (const [name, error, expectedPark] of [
    ["disk stop", new DiskParkSignal(false), true],
    ["transient recovery", new TransientRecoveryError(), true],
    ["plan rejected", new PlanRejectedError("rejected"), false],
  ] as const) {
    it(`keeps existing ${name} disposition without occupancy reclaim`, async () => {
      const f = fixture({ error });
      await f.runner.execute(f.claim);
      assert.equal(f.reclaims(), 0);
      assert.equal(parks().length, expectedPark ? 1 : 0);
      assert.equal(f.calls(), 1);
    });
  }
});
