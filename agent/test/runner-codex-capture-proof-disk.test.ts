import { it } from "node:test";
import assert from "node:assert/strict";
import { api, client, installHarness } from "./runner-harness.js";
import { fixture, parks } from "./runner-terminal-disk-deferral-fixture.js";
import { trackedFile, receiptError } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
installHarness();
for (const exit of ["loop-park","loop-terminal-after-refusal","confirm-terminal","confirm-held","receipt-failed"] as const) {
  it(`public terminalDisk credential recovery ${exit}`, async () => {
    const o=observe({vault:true});
    let ran=false;
    let attempts=0;
    const capture: typeof o.capture=async (...args)=>{
      if(exit==="loop-terminal-after-refusal" && ++attempts===1) {
        api.setOwnershipStatus(f.claim.run_id,"failed",4);
        return {kind:"incomplete",errors:[{category:"timeout",message:"test initial proof incomplete"}]};
      }
      return o.capture(...args);
    };
    const f=fixture({
      safety:o.rig.safety,
      settleForCredentialFreeCapture:()=>o.rig.settle(2000),
      withCredentialFreeCapture:capture,
      features:["recovery_cause_data_volume_full","claim_generation_fence","recovery_cause_vault_locked"],
      beforeReject:()=>{ran=true;}
    });
    if(exit==="confirm-terminal" || exit==="confirm-held")
      api.failStateWhen(f.claim.run_id,b=>ran && b.status==="running",
        {runStatus:exit==="confirm-terminal"?"cancelled":"paused"});
    if(exit==="receipt-failed") {
      const report=client.reportState.bind(client);
      client.reportState=async(id,body,signal)=>{
        if(ran && body.status==="running") throw receiptError();
        return report(id,body,signal);
      };
    }
    try {
      await o.execute(f.runner.execute(f.claim));
      assert.equal(f.calls(),1,"opaque rejection never replays executor");
      assert.equal(trackedFile(2201,"COMMITTED.txt"),"committed sentinel\n");
      assert.equal(trackedFile(2201,"DIRTY.txt"),"dirty sentinel\n");
      assert.equal(parks().length,exit==="loop-park"?1:0);
      if(exit==="loop-terminal-after-refusal") assert.equal(attempts,2,"one refusal followed by terminal exit capture");
      o.assertProof(exit!=="confirm-terminal" && exit!=="loop-terminal-after-refusal");
    } finally {o.restore();}
  });
}
