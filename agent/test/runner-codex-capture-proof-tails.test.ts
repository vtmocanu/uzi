import assert from "node:assert/strict";
import { it } from "node:test";
import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { setImmediate as tick } from "node:timers/promises";
import { RecoveryCoordinator } from "../src/recovery.js";
import { PlanRejectedError } from "../src/executor.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runnerWith } from "./runner-harness.js";
import { FakeCodexExecutor, commitInTree, trackedFile, statuses, workThenDefer, assertCaptured, VAULT_FEATURE } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
import { heldServer, JOIN_TOKEN } from "./runner-held-publication-fixture.js";
installHarness();
const gitEnv=Object.fromEntries(Object.entries(process.env).filter(([k])=>!k.startsWith("GIT_")));
Object.assign(gitEnv,{GIT_CONFIG_GLOBAL:"/dev/null",GIT_CONFIG_SYSTEM:"/dev/null",GIT_TERMINAL_PROMPT:"0",GIT_AUTHOR_NAME:"tail",GIT_AUTHOR_EMAIL:"tail@test",GIT_COMMITTER_NAME:"tail",GIT_COMMITTER_EMAIL:"tail@test"});
function cmd(cwd:string,args:string[],input?:string){return execFileSync("git",args,{cwd,env:gitEnv,encoding:"utf8",input}).trim();}
for(const variant of ["accepted","guarded","failed"] as const) it(`A public tail pause ${variant} ${variant==="failed"?"actual WIP undo":"actual restore head and pin completion"}`,async()=>{
 const failed=variant==="failed";
 const o=observe();const events:any[]=[];let ack=false;let pauseReturned=false;let result:unknown;let sourcePath="";
 const report=client.reportState.bind(client);
 client.reportState=async(...args)=>{const r=await report(...args);if(args[1].status==="paused")ack=true;return r;};
 const publish=client.publishCheckpoint.bind(client);
 if(failed)client.publishCheckpoint=async(...args)=>{await publish(...args);return {ok:false,httpStatus:400};};
 const tip=git.branchTip.bind(git),undo=git.undoWipMarkerOnBranch.bind(git),pin=RecoveryCoordinator.prototype.pin;
 git.branchTip=async(...args)=>{const r=await tip(...args);if(ack&&!pauseReturned){await tick();events.push({kind:"restore-head-return",active:o.isActive(),head:r});}return r;};
 RecoveryCoordinator.prototype.pin=async function(...args){const r=await pin.apply(this,args);if(ack&&!pauseReturned){await tick();events.push({kind:"pin-return",active:o.isActive(),head:args[0].sourceSha,record:!!r});}return r;};
 git.undoWipMarkerOnBranch=async(...args)=>{events.push({kind:"undo-entry",active:o.isActive()});const r=await undo(...args);await tick();events.push({kind:"undo-return",active:o.isActive(),bytes:fs.readFileSync(path.join(args[0],"PUBLIC.txt"),"utf8"),status:cmd(args[0],["status","--porcelain"])});return r;};
 const claim={...gitlabClaim(2852),claim_generation:1,park_run_supported:true,...(variant==="guarded"?{inventory_guarded:true}:{})};
 const freeze=RecoveryCoordinator.prototype.freezeInventory,upload=RecoveryCoordinator.prototype.captureAndUpload;
 let server:ReturnType<typeof heldServer>|undefined;
 if(variant==="guarded"){
  server=heldServer(claim);
  RecoveryCoordinator.prototype.freezeInventory=async function(...args){const r=await freeze.apply(this,args);await tick();if(ack&&!pauseReturned)events.push({kind:"freeze-return",active:o.isActive(),record:!!r});return r;};
  RecoveryCoordinator.prototype.captureAndUpload=async function(...args){const r=await upload.apply(this,args);await tick();if(ack&&!pauseReturned)events.push({kind:"inventory-upload-return",active:o.isActive(),outcome:r});return r;};
 }
 const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
  sourcePath=ctx.worktreePath;fs.writeFileSync(path.join(sourcePath,"PUBLIC.txt"),"tail original dirty bytes\n");o.arm();
  result=await ctx.parkForPause!({completedCount:1,total:2});pauseReturned=true;
  return {branch:ctx.branch,pausedAt:{completedCount:1,total:2}};
 },o.rig.settle,o.capture);
 try{
  await o.execute(runnerWith(()=>({executor:exec}),fakeGitlab().gitlab,JOIN_TOKEN).execute(claim));
  console.log("TAIL_PAUSE",JSON.stringify({failed,result,events}));
  if(failed){
   assert.equal(result,false);assert.ok(!statuses(claim.run_id).includes("paused"));
   assert.deepEqual(events.filter(x=>x.kind.startsWith("undo")).map(x=>[x.kind,x.active]),[["undo-entry",true],["undo-return",true]],"real WIP undo entry AND return remain within original capture");
   const e=events.find(x=>x.kind==="undo-return");assert.equal(e.bytes,"tail original dirty bytes\n");assert.match(e.status,/PUBLIC.txt/);
  }else{
   assert.equal(result,true);assert.equal(trackedFile(2852,"PUBLIC.txt"),"tail original dirty bytes\n");
   assert.ok(events.some(x=>x.kind==="restore-head-return"),"actual restore point head completed");
   assert.ok(events.some(x=>x.kind==="pin-return"&&x.record),"actual recovery pin persisted a record");
   if(variant==="guarded"){
    assert.ok(events.some(x=>x.kind==="freeze-return"&&x.record),"actual guarded inventory froze");
    assert.ok(events.some(x=>x.kind==="inventory-upload-return"),"actual guarded inventory upload returned");
    assert.ok(server!.archiveUploads()>0,"actual inventory bundle consumed");
   }
   assert.ok(events.every(x=>x.active),"accepted pause remains held through actual head and pin returns");
  }
  o.assertProof(true,!failed);
 }finally{git.branchTip=tip;git.undoWipMarkerOnBranch=undo;RecoveryCoordinator.prototype.pin=pin;RecoveryCoordinator.prototype.freezeInventory=freeze;RecoveryCoordinator.prototype.captureAndUpload=upload;o.restore();}
});
for(const branch of ["unrelated","moved"] as const)it(`A public tail actual Git gate ${branch} postcheck refusal`,async()=>{
 const o=observe();const post:any[]=[];let refusal:unknown;let verdict:unknown;let clone="";let stoppedReads=0,stoppedPubs=0;
 const claim={...gitlabClaim(2852),claim_generation:7,execution_milestones:[{id:"m1",title:"Original"}]};
 client.protocolFeatures=["milestone_reconciliation_v1","claim_generation_fence","gate_revision_v1"];
 api.gateRevisions=true;api.stampGateBindings=true;
 const ancestor=git.isAncestorRef.bind(git),head=git.worktreeHead.bind(git);
 git.isAncestorRef=async(...args)=>{
  if(!o.observations().pubs.length)return ancestor(...args);
  if(branch==="unrelated"){
   const tree=cmd(args[0],["rev-parse",args[2]+"^{tree}"]);
   const orphan=cmd(args[0],["commit-tree",tree], "independent tail root\n");
   const actual=await ancestor(args[0],args[1],orphan);await tick();
   post.push({kind:"ancestry-return",active:o.isActive(),actual,orphan});return actual;
  }
  const actual=await ancestor(...args);await tick();post.push({kind:"ancestry-return",active:o.isActive(),actual});return actual;
 };
 git.worktreeHead=async(...args)=>{
  if(o.observations().pubs.length&&branch==="moved"){
   commitInTree(clone,"POSTCHECK.txt","actual changed head\n");
   const actual=await head(...args);await tick();post.push({kind:"head-return",active:o.isActive(),actual});return actual;
  }return head(...args);
 };
 let run:ReturnType<typeof runnerWith>;
 const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
  clone=ctx.worktreePath;commitInTree(clone,"PUBLIC.txt","gate tail source bytes\n");o.arm();
  try{verdict=await ctx.gatePlan!("Plan",[{id:"m1",title:"Changed"}]);}catch(e){refusal=e;stoppedReads=o.observations().source.length;stoppedPubs=o.observations().pubs.length;run.shutdown();throw e;}
  throw new PlanRejectedError("fixture");
 },o.rig.settle,o.capture);
 try{
  run=runnerWith(()=>({executor:exec}),fakeGitlab().gitlab,undefined,undefined,{checkpointIntervalMs:0,planApprovalTimeoutMs:1000,recoveryRetryMs:1});
  await o.execute(run.execute(claim));
  console.log("TAIL_GATE",JSON.stringify({branch,post,refusal:refusal instanceof Error?refusal.name:refusal}));
  assert.equal(trackedFile(2852,"PUBLIC.txt"),"gate tail source bytes\n");
  assert.equal(fs.readFileSync(path.join(clone,"PUBLIC.txt"),"utf8"),"gate tail source bytes\n","shutdown retains the source");
  assert.equal(o.observations().source.length,stoppedReads,"shutdown starts no source recapture");
  assert.equal(o.observations().pubs.length,stoppedPubs,"shutdown starts no checkpoint replay");
  assert.equal(o.isActive(),false);
  assert.equal(verdict,undefined);assert.equal((refusal as Error)?.name,"TransientRecoveryError");
  assert.equal(api.states.some(s=>s.body.gate_checkpoint_witness!==undefined),false);
  assert.equal(api.states.some(s=>s.body.status==="awaiting_approval"),false);
  assert.deepEqual(post.map(x=>x.kind),branch==="unrelated"?["ancestry-return"]:["ancestry-return","head-return"]);
  if(branch==="unrelated")assert.equal(post[0].actual,false);else assert.equal(post[0].actual,true);
  assert.ok(post.every(x=>x.active),"negative actual Git postcheck RETURNS before capture release");
  o.assertProof(true);
 }finally{git.isAncestorRef=ancestor;git.worktreeHead=head;o.restore();}
});
it("A public tail deferred recovery actual failed fetch then actual retry",async()=>{
 const o=observe({vault:true});client.protocolFeatures=[VAULT_FEATURE];const claim=gitlabClaim(2853);
 const w=workThenDefer(o.rig,o.rig.settle,o.capture);const fetch=git.fetchAgentBranch.bind(git);const attempts:any[]=[];
 let first=true;
 git.fetchAgentBranch=async(...args)=>{
  if(!w.deferred())return fetch(...args);
  const n=attempts.length;attempts.push({entry:o.isActive()});
  try{
   // A nonexistent source path makes the actual Git fetch fail, without fabricating its outcome.
   const a=[...args] as any[];if(first){first=false;a[1]=path.join(args[1],"missing-tail-source");}
   const r=await fetch(...a as Parameters<typeof fetch>);attempts[n].ok=true;return r;
  }catch(e){attempts[n].ok=false;attempts[n].error=String(e);throw e;}
  finally{await tick();attempts[n].returned=o.isActive();}
 };
 try{
  await o.execute(runnerWith(()=>({executor:w.exec}),fakeGitlab().gitlab,undefined,undefined,{recoveryRetryMs:5}).execute(claim));
  console.log("TAIL_RETRY",JSON.stringify(attempts));assertCaptured(2853);
  assert.ok(attempts.length>=2);assert.equal(attempts[0].ok,false);assert.equal(attempts[1].ok,true);
  assert.ok(attempts.every(x=>x.entry&&x.returned),"failed and successful actual fetch stay held through return");
  assert.ok(o.observations().entries>=2,"separate actual capture attempts");
  o.assertProof(true);
 }finally{git.fetchAgentBranch=fetch;o.restore();}
});
