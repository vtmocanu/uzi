import assert from "node:assert/strict";
import fs from "node:fs";
import { it } from "node:test";
import { performance } from "node:perf_hooks";
import { setImmediate as tick } from "node:timers/promises";
import type { RunContext } from "../src/executor.js";
import type { ClaimResponse, MemoryReservationRequest, StateAck } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor } from "./runner-harness.js";
import { FakeCodexExecutor, commitInTree, trackedFile } from "./runner-codex-sinks-fixture.js";
import { observe } from "./runner-codex-capture-proof-fixture.js";
installHarness();
const settings = { enabled: true as const, reserveBytes: 100, hysteresisBytes: 100,
  sampleMs: 2, responseBudgetMs: 200, rearmMs: 10, maxInterventions: 2 };

async function setup(count = 0, configured = settings) {
  api.memoryRegisterNonce = "original-memory-registration";
  api.setRegisterProtocolFeatures(["worker_memory_pressure_v1", "claim_generation_fence"]);
  await client.register("memory-fixture", "base");
  const claim: ClaimResponse = { ...gitlabClaim(2396), claim_generation: 7,
    memory_episode: 3, memory_policy: { version: 1, max_interventions: 2 },
    memory_intervention_count: count };
  claim.secrets = { ...claim.secrets, codex: {
    auth_mode: "api_key", capability: "fixture-capability", access_token: "fixture-access",
  } };
  api.setOwnershipStatus(claim.run_id, "running", 7);
  let usage = 0, sampleId = 0, used = count;
  const parkReceipts: StateAck[] = [];
  const reportState = client.reportState.bind(client);
  client.reportState = async (...args) => {
    const ack = await reportState(...args);
    if (args[1].recovery_cause === "worker_memory_pressure") parkReceipts.push(ack);
    return ack;
  };
  const reservations: MemoryReservationRequest[] = [];
  const accepted = new Map<string, number>();
  client.reserveMemoryIntervention = async request => {
    reservations.push(request);
    accepted.set(request.intervention_id, ++used);
    return { ...request, admitted: true, authorizing: true,
      allowance: { limit: 2, used, remaining: 2 - used } };
  };
  client.reportMemoryInterventionOutcome = async request => {
    const spent = accepted.get(request.intervention_id)!;
    return { ...request, policy: { version: 1, max_interventions: 2 }, admitted: true, authorizing: false,
      allowance: { limit: 2, used: spent, remaining: 2 - spent } };
  };
  const memory = new WorkerMemoryRuntime(configured, client, nullLogger(), {
    reader: { sample: () => {
      const time = performance.now();
      return { available: true, sampleId: ++sampleId, startedAtMs: time, completedAtMs: time,
        currentBytes: usage, limitBytes: 1000,
        stat: { anon: 1, shmem: 0, slab_unreclaimable: 0, unevictable: 0 },
        pressure: { some: { avg10: 0, avg60: 0, avg300: 0, total: 0 },
          full: { avg10: 0, avg60: 0, avg300: 0, total: 0 } } };
    } },
  });
  return { memory, claim, reservations, parkReceipts, pressure: (bytes: number) => { usage = bytes; } };
}

function enroll(ctx: RunContext, toolId = "original-tool") {
  const captured = ctx.capturedMemory!()!;
  let cancellations = 0;
  const entry = captured.commands.register(captured.context, {
    source: "codex-model-command", toolId, attemptId: "original-attempt",
    command: "inert fixture", root: { pid: 10, startTime: "10" },
    observe: async () => {
      const time = performance.now();
      return { available: true, complete: true, startedAtMs: time, completedAtMs: time,
        rows: [{ pid: 10, startTime: "10", rssBytes: 100 }] };
    },
    cancel: async authority => { assert.equal(authority.authorized(), true); cancellations++; },
    drain: async () => ({ confirmed: true }),
  });
  return { captured, entry, cancellations: () => cancellations };
}

it("public memory two failed local verifications then actual success in one held scope",async()=>{
  const f=await setup(1,{...settings,responseBudgetMs:5000});
  let releaseFeedback!:()=>void; const feedbackGate=new Promise<void>(r=>{releaseFeedback=r;});
  const o=observe();const verifications:any[]=[];
  const verify=git.verifyRunnerTrackingCovers.bind(git);
  git.verifyRunnerTrackingCovers=async(...args)=>{
    const actual=await verify(...args);
    const result=verifications.length<2?false:actual;
    verifications.push({active:o.isActive(),actual,result});
    return result;
  };
  const feedback:unknown[]=[];const receipts:unknown[]=[];
  const post=client.postMessages.bind(client), report=client.reportState.bind(client);
  client.postMessages=async(...args)=>{
    if(args[1].some(m=>m.payload.event==="worker_memory_pressure")){
      await feedbackGate; await tick();feedback.push({active:o.isActive()});
    }
    return post(...args);
  };
  client.reportState=async(...args)=>{
    const ack=await report(...args);
    if(args[1].recovery_cause==="worker_memory_pressure"){
      await tick();receipts.push({active:o.isActive(),accepted:ack.memoryParkAccepted});
    }
    return ack;
  };
  let delivered=false;
  const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
    commitInTree(ctx.worktreePath,"PUBLIC.txt","memory actual bytes\n");
    const command=enroll(ctx);o.arm();f.pressure(950);
    const result=await command.captured.commands.watchOutcome(command.entry);
    delivered=result.code==="worker_memory_pressure";
    ctx.emit({kind:"tool_result",payload:{tool_id:result.tool_id,result}});
    throw new Error("model stopped for memory");
  },async ms=>{const result=await o.rig.settle(ms);releaseFeedback();return result;},
    (ms,action,purpose,signal)=>o.capture(ms,async held=>{releaseFeedback();return action(held);},purpose,signal));
  const monitoring=f.memory.run();
  try{
    await o.execute(runnerWith(()=>({executor:exec}),fakeGitlab().gitlab,undefined,undefined,
      {memoryRuntime:f.memory,codexBoundaryDeadlineMs:5000,checkpointIntervalMs:0}).execute(f.claim));
    console.log("MEMORY_FEEDBACK",JSON.stringify(feedback),"MEMORY_ACK",JSON.stringify(receipts));
    assert.equal(delivered,true);assert.equal(f.reservations.length,1);
    assert.equal(trackedFile(2396,"PUBLIC.txt"),"memory actual bytes\n","independent actual captured bytes");
    console.log("RETRY_VERIFICATIONS",JSON.stringify(verifications));
    o.assertProof(true,true);
    assert.equal(o.observations().entries,1,"all retries share ONE outer capture");
    assert.equal(o.observations().returns,1,"one original outer scope returns");
    assert.deepEqual(verifications.map(v=>v.result),[false,false,true],"two failed verdicts then actual Git success");
    assert.ok(verifications.every(v=>v.actual===true&&v.active),"each real local verification runs inside original held scope");
    assert.ok(o.observations().source.every((v:any)=>v.active),"all protected source reads stay inside original capture");
    assert.ok(feedback.length>0,"actual pressure feedback sent");
    assert.ok(feedback.every((x:any)=>x.active),"capture held through awaited feedback delivery");
    assert.ok(receipts.length===1&&(receipts[0] as any).active,"matching park ACK inside capture");
    assert.deepEqual((receipts[0] as any).accepted,{
      run_id:f.claim.run_id,worker_id:client.memoryIncarnation!.worker_id,
      register_nonce:"original-memory-registration",claim_generation:7,memory_episode:3});
  }finally{await f.memory.stop();await monitoring;client.postMessages=post;client.reportState=report;git.verifyRunnerTrackingCovers=verify;o.restore();}
});


it("public memory initially missing scoped capability retains without sensitive operations",async()=>{
  const f=await setup(2,{...settings,responseBudgetMs:5000});
  const o=observe(); let reached=false, stopped=false; let legacySettles=0;
  const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
    commitInTree(ctx.worktreePath,"PUBLIC.txt","missing capability actual bytes\n");
    o.arm(); reached=true;
    try { ctx.capturedMemory!(); } catch { stopped=true; }
    throw new Error("fixture ends model after exhausted accessor");
  },async ms=>{legacySettles++;return o.rig.settle(ms);});
  try{
    await o.execute(runnerWith(()=>({executor:exec}),fakeGitlab().gitlab,undefined,undefined,
      {memoryRuntime:f.memory,codexBoundaryDeadlineMs:5000,checkpointIntervalMs:0}).execute(f.claim));
    console.log("MISSING_CAPABILITY",JSON.stringify({...o.observations(),legacySettles,parks:f.parkReceipts}));
    assert.equal(reached,true,"normal public executor ran");
    assert.equal(stopped,true,"exhausted public accessor stopped model admission");
    assert.equal(fs.existsSync(worktreeDirFor(2396)),true,"original clone retained");
    assert.equal(api.states.some(s=>["failed","completed"].includes(s.body.status)),false,"quiet retained stop");
    assert.equal(f.reservations.length,0,"spent allowance never reserves again");
    o.assertRefusal();
    assert.equal(api.states.some(s=>s.body.checkpoint_contains_latest===true),false,"no verified published success invented");
  }finally{await f.memory.stop();o.restore();}
});
