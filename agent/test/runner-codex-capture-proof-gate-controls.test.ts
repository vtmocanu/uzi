import assert from "node:assert/strict";
import { it } from "node:test";
import { setImmediate as tick } from "node:timers/promises";
import { PlanRejectedError } from "../src/executor.js";
import { api, client, git, fakeGitlab, gitlabClaim, input, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, commitInTree, trackedFile } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
installHarness();
for (const failed of [false,true]) it(`public gate extra actual Git ${failed?"publication failure":"approve"}`,async()=>{
  const o=observe(); const post:any[]=[]; let gates=0;
  let prefix:any[]=[]; let verdict:unknown; let refusal:unknown;
  const publish=client.publishCheckpoint.bind(client);
  client.publishCheckpoint=async(...args)=>{
    const result=await publish(...args);
    return failed?{ok:false,httpStatus:400}:result;
  };
  const claim={...gitlabClaim(2852),claim_generation:7,
    execution_milestones:[{id:"m1",title:"Original"}]};
  client.protocolFeatures=["milestone_reconciliation_v1","claim_generation_fence","gate_revision_v1"];
  api.gateRevisions=true; api.stampGateBindings=true;
  const ancestor=git.isAncestorRef.bind(git), head=git.worktreeHead.bind(git);
  git.isAncestorRef=async(...args)=>{
    if(o.observations().pubs.length){await tick();post.push({kind:"ancestry",active:o.isActive()});}
    return ancestor(...args);
  };
  git.worktreeHead=async(...args)=>{
    if(o.observations().pubs.length){await tick();post.push({kind:"head",active:o.isActive()});}
    return head(...args);
  };
  api.onState(claim.run_id,body=>{
    if(body.status==="awaiting_approval"){prefix=post.slice();gates++;api.setInputs(claim.run_id,[input("approve_plan")]);}
  });
  let run: ReturnType<typeof runnerWith>;
  const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
    commitInTree(ctx.worktreePath,"PUBLIC.txt","gate actual bytes\n");o.arm();
    try { verdict=await ctx.gatePlan!("Plan",[{id:"m1",title:"Changed"}]); } catch(error) {refusal=error;run.shutdown(); throw error;}
    throw new PlanRejectedError("fixture rejection");
  },o.rig.settle,o.capture);
  try{
    run=runnerWith(()=>({executor:exec}),fakeGitlab().gitlab,undefined,undefined,
      {checkpointIntervalMs:0,planApprovalTimeoutMs:2000,recoveryRetryMs:1});
    await o.execute(run.execute(claim));
    console.log("GATE_POST",JSON.stringify(post));
    console.log("GATE_EXTRA",JSON.stringify({failed,prefix,post,gates,verdict,refusal:refusal instanceof Error?refusal.name:refusal}));
    assert.equal(trackedFile(2852,"PUBLIC.txt"),"gate actual bytes\n","actual durable worker bare has captured source bytes");
    o.assertProof(true);
    if(failed){
      assert.equal(gates,0,"failed optional publication never presents approval");
      assert.equal(verdict,undefined,"no gate result fabricated");
      assert.equal((refusal as Error)?.name,"TransientRecoveryError","existing gate witness policy refuses local-only capture");
      assert.equal(api.states.some(s=>s.body.gate_checkpoint_witness!==undefined),false,"no fabricated published witness");
    }else{
      assert.equal(gates,1);
      const gate=api.states.find(s=>s.body.status==="awaiting_approval")!;
      assert.equal(gate.body.claim_generation,7);
      assert.ok(gate.body.gate_checkpoint_witness);
      assert.ok(prefix.length>0&&prefix.every(x=>x.active),"EVERY postcheck in presentation prefix is held");
      assert.ok(prefix.some(x=>x.kind==="ancestry")&&prefix.some(x=>x.kind==="head"),"presentation prefix contains ancestry AND unchanged HEAD");
      assert.equal(verdict,undefined,"held predecessor does not resume after approve");
      assert.ok(refusal instanceof Error,"approval follows existing held-predecessor policy");
    }
  }finally{git.isAncestorRef=ancestor;git.worktreeHead=head;o.restore();}
});
