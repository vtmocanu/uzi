import { it } from "node:test";
import assert from "node:assert/strict";
import { PauseNowSignal } from "../src/steering.js";
import { nullLogger } from "./helpers.js";
import { api, client, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, commitInTree, trackedFile, statuses, VAULT_FEATURE, workThenDefer, assertCaptured, receiptError, parkReports } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
installHarness();

for (const entry of ["parkForPause", "PauseNowSignal-plan", "requestParkRun"] as const) {
  for (const refusal of [false, true]) it(`public pause ${entry} initial-proof-${refusal ? "refused" : "accepted"}`, async () => {
    const {gitlab} = fakeGitlab();
    const o = observe({refusal});
    const claim = {...gitlabClaim(2852), claim_generation:1, park_run_supported:true, auto_approve:false};
    api.parkRunGeneration=1;
    api.parkRunOwnership={status:"running",claim_generation:1,hold_reason:null,claim_released:false};
    let result: unknown;
    const exec = new FakeCodexExecutor(o.rig.safety, async ctx => {
      commitInTree(ctx.worktreePath,"PUBLIC.txt","caller capture content\n");
      o.arm();
      const at={completedCount:1,total:2};
      if (entry === "PauseNowSignal-plan") throw new PauseNowSignal();
      if (entry === "requestParkRun") {
        result=await ctx.requestParkRun!(at);
        return {branch:ctx.branch, pausedAt:at};
      }
      result=await ctx.parkForPause!(at);
      return {branch:ctx.branch,pausedAt:at};
    }, o.rig.settle, o.capture);
    try {
      await o.execute(runnerWith(()=>({executor:exec}),gitlab).execute(claim));
      if (refusal) {
        o.assertRefusal();
        assert.ok(!statuses(claim.run_id).includes("paused"),"refused proof never parks");
      } else {
        assert.ok(statuses(claim.run_id).includes("paused"),"actual pause ACK landed");
        assert.equal(trackedFile(2852,"PUBLIC.txt"),"caller capture content\n");
        if (entry === "parkForPause") assert.equal(result,true);
        if (entry === "requestParkRun") assert.deepEqual(result,{kind:"paused"});
        o.assertProof(true,true);
      }
    } finally {o.restore();}
  });
}
for (const exit of ["loop-park", "loop-terminal-after-refusal", "confirm-terminal", "confirm-held", "receipt-failed"] as const) {
  it(`public credential recovery ${exit}`, async () => {
    const {gitlab} = fakeGitlab();
    client.protocolFeatures=[VAULT_FEATURE];
    const o=observe({vault:true});
    const claim=gitlabClaim(2853);
    let attempts=0;
    const firstRefusal=()=>{
      if(exit==="loop-terminal-after-refusal" && ++attempts===1) {
        api.setOwnershipStatus(claim.run_id,"failed");
        return {kind:"incomplete" as const, errors:[{category:"timeout" as const,message:"test initial proof incomplete"}]};
      }
      return undefined;
    };
    const settle: typeof o.rig.settle=async ms=>firstRefusal() ?? await o.rig.settle(ms);
    const capture: typeof o.capture=async (...args)=>firstRefusal() ?? await o.capture(...args);
    const w=workThenDefer(o.rig,settle,capture);
    if (exit==="confirm-terminal" || exit==="confirm-held")
      api.failStateWhen(claim.run_id,b=>w.deferred() && b.status==="running",
        {runStatus:exit==="confirm-terminal"?"cancelled":"paused"});
    if (exit==="receipt-failed") {
      const report=client.reportState.bind(client);
      client.reportState=async (id,body,signal)=>{
        if(w.deferred() && body.status==="running") throw receiptError();
        return report(id,body,signal);
      };
    }
    try {
      await o.execute(runnerWith(()=>({executor:w.exec}),gitlab,undefined,nullLogger(),{recoveryRetryMs:5}).execute(claim));
      assertCaptured(2853);
      if(exit==="loop-park") assert.equal(parkReports(claim.run_id).length,1);
      else assert.equal(parkReports(claim.run_id).length,0);
      o.assertProof(exit!=="confirm-terminal" && exit!=="loop-terminal-after-refusal");
    } finally {o.restore();}
  });
}
