import { it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { api, client, installHarness, worktreeDirFor } from "./runner-harness.js";
import { fixture, parks } from "./runner-terminal-disk-deferral-fixture.js";
import { trackedFile, receiptError, spyCustodySettle } from "./runner-codex-sinks-fixture.js";
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
    const custody = spyCustodySettle(f.runner);
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
      assert.equal(o.rig.refreshCalls(), 1, "only the original admission attempts credentials");
      assert.equal(o.rig.releaseCalls(), 0);
      assert.equal(custody(), 0);
      assert.equal(f.forge.calls.length, 0);
      assert.equal(trackedFile(2201,"COMMITTED.txt"),"committed sentinel\n");
      assert.equal(trackedFile(2201,"DIRTY.txt"),"dirty sentinel\n");
      assert.equal(parks().length,exit==="loop-park"?1:0);
      if(exit==="loop-terminal-after-refusal") assert.equal(attempts,2,"one refusal followed by terminal exit capture");
      o.assertProof(exit!=="confirm-terminal" && exit!=="loop-terminal-after-refusal");
    } finally {o.restore();}
  });
}

for (const exhausted of [false, true]) {
  it(`terminalDisk typed handoff ${exhausted ? "exhausts" : "retains"} the original deadline across A retries`, async t => {
    const o = observe({ vault: true });
    const now = Date.now.bind(Date);
    let elapsed = 0;
    t.mock.method(Date, "now", () => now() + elapsed);
    const budgets: number[] = [];
    const capture: typeof o.capture = async (ms, ...args) => {
      budgets.push(ms);
      if (budgets.length === 1) {
        elapsed += exhausted ? 180_001 : 1_000;
        return { kind: "incomplete", errors: [{ category: "timeout", message: "initial proof incomplete" }] };
      }
      return o.capture(ms, ...args);
    };
    const f = fixture({
      safety: o.rig.safety,
      withCredentialFreeCapture: capture,
      features: ["recovery_cause_data_volume_full", "claim_generation_fence", "recovery_cause_vault_locked"],
    });
    const custody = spyCustodySettle(f.runner);
    try {
      await o.execute(f.runner.execute(f.claim));
      assert.equal(f.calls(), 1);
      assert.equal(o.rig.refreshCalls(), 1, "only the original admission attempts credentials");
      assert.equal(o.rig.releaseCalls(), 0);
      assert.equal(custody(), 0);
      assert.equal(f.forge.calls.length, 0);
      assert.ok(budgets[0]! > 120_000 && budgets[0]! <= 180_000, "handoff inherits B's remaining total budget");
      if (exhausted) {
        assert.equal(budgets.length, 1, "expiry cannot acquire a renewed A capture budget");
        assert.equal(parks().length, 0);
        assert.equal(trackedFile(2201, "COMMITTED.txt"), null);
        assert.equal(trackedFile(2201, "DIRTY.txt"), null);
        assert.equal(fs.readFileSync(path.join(worktreeDirFor(2201), "COMMITTED.txt"), "utf8"), "committed sentinel\n");
        assert.equal(fs.readFileSync(path.join(worktreeDirFor(2201), "DIRTY.txt"), "utf8"), "dirty sentinel\n");
        assert.equal(o.observations().source.length, 0);
        assert.equal(o.observations().pubs.length, 0);
      } else {
        assert.equal(budgets.length, 2);
        assert.ok(budgets[1]! <= budgets[0]! - 1_000, "retry consumes the original deadline");
        assert.equal(trackedFile(2201, "COMMITTED.txt"), "committed sentinel\n");
        assert.equal(trackedFile(2201, "DIRTY.txt"), "dirty sentinel\n");
        assert.equal(parks().length, 1);
        o.assertProof(true);
      }
    } finally { o.restore(); }
  });
}
