import assert from "node:assert/strict";
import fs from "node:fs";
import { it } from "node:test";
import { performance } from "node:perf_hooks";
import { setImmediate as tick } from "node:timers/promises";
import type { RunContext } from "../src/executor.js";
import type { ClaimResponse, MemoryReservationRequest, StateAck } from "../src/protocol.js";
import { WorkerMemoryRuntime } from "../src/worker-memory-runtime.js";
import { nullLogger } from "./helpers.js";
import { api, client, git, fakeGitlab, gitlabClaim, installHarness, runnerWith, worktreeDirFor, homeDir } from "./runner-harness.js";
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

for (const mode of ["retry", "uncertain", "cancel-ack"] as const) it(`public memory edge ${mode} actual monitor held lineage`,async()=>{
  // Real Git retries need headroom to finish the lineage proof within one original held budget.
  const fixtureBudgetMs=30_000;
  const f=await setup(1,{...settings,responseBudgetMs:fixtureBudgetMs});
  let releaseFeedback!:()=>void; const feedbackGate=new Promise<void>(r=>{releaseFeedback=r;});
  const o=observe();const verifications:any[]=[];
  let original:any, heldContext:any, captureSignal:AbortSignal|undefined, capturePurpose:unknown;
  let run:ReturnType<typeof runnerWith>;
  let originalHold:any, originalContext:any, effectiveHeld:any;
  const samples:any[]=[], errors:any[]=[], attempts:any[]=[], tokens:any[]=[], parents:any[]=[];
  const lineage=(phase="observation")=>{
    const hold=original?.workerMemory?.hold;
    originalHold ??= hold; originalContext ??= hold?.context;
    samples.push({phase,hold,context:hold?.context,flight:hold?.context.flight,
      held:effectiveHeld,raw:heldContext,at:Date.now(),deadline:hold?.deadline,
      rawDeadline:heldContext?.deadlineAt,effectiveDeadline:effectiveHeld?.deadlineAt,
      attemptAborted:captureSignal?.aborted,heldAborted:effectiveHeld?.signal?.aborted,
      remaining:hold ? hold.deadline-Date.now():null});
  };
  const errorMeta=(where:string,e:any)=>errors.push({where,name:e?.name,message:e?.message,stack:e?.stack,
    at:Date.now(),remaining:original?.workerMemory?.hold?.deadline-Date.now(),attemptAborted:captureSignal?.aborted});
  fs.writeFileSync(homeDir+"/session-proof","original edge session");
  const verify=git.verifyRunnerTrackingCovers.bind(git);
  git.verifyRunnerTrackingCovers=async(...args)=>{
    const actual=await verify(...args);
    const result=mode==="retry" && verifications.length<2?false:actual;
    if(heldContext) lineage();
    verifications.push({active:o.isActive(),actual,result});
    return result;
  };
  const feedback:unknown[]=[];const receipts:unknown[]=[];
  const post=client.postMessages.bind(client), report=client.reportState.bind(client);
  client.postMessages=async(...args)=>{
    if(args[1].some(m=>m.payload.event==="worker_memory_pressure")){
      await feedbackGate; await tick(); if(heldContext) lineage(); feedback.push({active:o.isActive()});
    }
    return post(...args);
  };
  client.reportState=async(...args)=>{
    let ack=await report(...args);
    if(args[1].recovery_cause==="worker_memory_pressure"){
      await tick(); if(heldContext) lineage();
      if(mode==="uncertain") ack={...ack,memoryParkAccepted:undefined};
      if(mode==="cancel-ack") {run.shutdown(); await tick();}
      receipts.push({active:o.isActive(),accepted:ack.memoryParkAccepted,aborted:heldContext?.signal.aborted});
    }
    return ack;
  };
  let delivered=false;
  const exec=new FakeCodexExecutor(o.rig.safety,async ctx=>{
    commitInTree(ctx.worktreePath,"PUBLIC.txt","memory actual bytes\n");
    const command=enroll(ctx);original=command.captured.flight;o.arm();f.pressure(950);
    const result=await command.captured.commands.watchOutcome(command.entry);
    delivered=result.code==="worker_memory_pressure";
    ctx.emit({kind:"tool_result",payload:{tool_id:result.tool_id,result}});
    throw new Error("model stopped for memory");
  },async ms=>{const requestAt=Date.now();lineage("legacy-settle-entry");try{const result=await o.rig.settle(ms);console.log("LEGACY_SETTLE_RAW",JSON.stringify({requestAt,ms,requestAtPlusMs:requestAt+ms,completedAt:Date.now(),kind:result.kind,originalDeadline:original?.workerMemory?.hold?.deadline}));releaseFeedback();return result;}catch(e){errorMeta("legacy-settle",e);throw e;}},
    async(ms,action,purpose,signal)=>{
      captureSignal=signal;capturePurpose=purpose;
      const a={requestAt:Date.now(),ms,signal,entryAt:0,rawDeadline:0};attempts.push(a);
      try{return await o.capture(ms,async held=>{
        heldContext=held;a.entryAt=Date.now();a.rawDeadline=held.deadlineAt;
        lineage("facade-entry");releaseFeedback();
        try{return await action(held);}catch(e){errorMeta("capture-action",e);throw e;}
      },purpose,signal);}catch(e){errorMeta("capture-facade",e);throw e;}
    });
  const monitoring=f.memory.run();
  try{
    run=runnerWith(()=>({executor:exec,homeDir}),fakeGitlab().gitlab,undefined,undefined,
      {memoryRuntime:f.memory,codexBoundaryDeadlineMs:fixtureBudgetMs,checkpointIntervalMs:0});
    const runner:any=run, capture=runner.captureRecoveryRestorePoint.bind(run);
    runner.captureRecoveryRestorePoint=async(...args:any[])=>{
      effectiveHeld=args[4]?.heldCapture;
      if(effectiveHeld) tokens.push(effectiveHeld);
      lineage("runner-capture-entry");
      try{return await capture(...args);}catch(e){errorMeta("runner-capture",e);throw e;}
    };
    const recovery=git.withRecoveryOperation.bind(git);
    git.withRecoveryOperation=async<T>(signal:AbortSignal,deadline:number,
      action:(signal:AbortSignal)=>Promise<T>,budget?:Parameters<typeof recovery>[3]):Promise<T>=>{
      if(original?.workerMemory?.hold) parents.push({signal,deadline,
        at:Date.now(),abortedAtEntry:signal.aborted});
      try{return await recovery(signal,deadline,action,budget);}catch(e){errorMeta("parent-operation",e);throw e;}
    };
    try { await o.execute(run.execute(f.claim)); } finally {git.withRecoveryOperation=recovery;}

    await original?.workerMemory?.operation;
    lineage("joined");
    console.log("LINEAGE_RAW",JSON.stringify({
      attempts:attempts.map(a=>({requestAt:a.requestAt,ms:a.ms,requestAtPlusMs:a.requestAt+a.ms,
        entryAt:a.entryAt,rawDeadline:a.rawDeadline,signalAborted:a.signal?.aborted})),
      samples:samples.map(({hold,context,flight,held,raw,...x})=>({...x,
        sameHold:hold===originalHold,sameContext:context===originalContext,sameFlight:flight===original,
        sameEffective:held===effectiveHeld,sameRaw:raw===heldContext})),
      parents:parents.map(({signal,...x})=>({...x,aborted:signal?.aborted,sameAttemptSignal:signal===captureSignal})),errors}));
    console.log("MEMORY_FEEDBACK",JSON.stringify(feedback),"MEMORY_ACK",JSON.stringify(receipts));
    assert.equal(delivered,true);assert.equal(f.reservations.length,1);
    assert.equal(trackedFile(2396,"PUBLIC.txt"),"memory actual bytes\n","independent actual captured bytes");
    console.log("RETRY_VERIFICATIONS",JSON.stringify(verifications));
    o.assertProof(true,true);
    assert.ok(samples.every(x=>x.hold===originalHold&&x.context===originalContext&&x.flight===original),
      "same original hold and context throughout");
    assert.ok(tokens.length>0,"real Runner capture options expose effective held token");
    assert.ok(tokens.every(h=>h===effectiveHeld&&h.context===heldContext),
      "same effective held token and raw context through retries");
    assert.ok(samples.filter(x=>x.held).every(x=>x.effectiveDeadline<=x.deadline),
      "effective held token never renews original epoch-ms deadline");
    assert.ok(parents.some(x=>x.deadline===originalHold.deadline),
      "actual parent recovery operation retains original deadline");
    if(mode==="cancel-ack") assert.ok(parents.some(x=>x.deadline===originalHold.deadline&&x.signal.aborted),
      "actual original parent operation receives cancellation");
    assert.equal(errors.filter(x=>x.name==="AssertionError").length,0,
      "no swallowed capture hook errors");
    assert.equal(capturePurpose,"cancel","memory capture uses cancellation purpose");
    assert.ok(captureSignal,"original recovery cancellation signal supplied");
    assert.equal(fs.readFileSync(homeDir+"/session-proof","utf8"),"original edge session");
    if(mode!=="retry") {
      assert.notEqual(original.parked,true,"uncertain or cancelled ACK never fabricates parked success");
      assert.equal(original.retainedLocalCustody,true);
      assert.equal(fs.existsSync(worktreeDirFor(2396)),true,"exact original clone retained");
    }
    if(mode==="cancel-ack") {
      assert.equal(captureSignal!.aborted,true,"lifecycle cancellation reaches recovery operation");
      assert.equal(heldContext.signal.aborted,true,"derived held signal propagates original cancellation");
      assert.equal(original.workerMemory.settled,true,"original operation fully settled");
    }
    assert.equal(o.observations().entries,1,"all retries share ONE outer capture");
    assert.equal(o.observations().returns,1,"one original outer scope returns");
    assert.deepEqual(verifications.map(v=>v.result),mode==="retry"?[false,false,true]:[true],"only verification retry recaptures locally");
    assert.ok(verifications.every(v=>v.actual===true&&v.active),"each real local verification runs inside original held scope");
    assert.ok(o.observations().source.every((v:any)=>v.active),"all protected source reads stay inside original capture");
    assert.ok(feedback.length>0,"actual pressure feedback sent");
    assert.ok(feedback.every((x:any)=>x.active),"capture held through awaited feedback delivery");
    assert.ok(receipts.length===1&&(receipts[0] as any).active,"matching park ACK inside capture");
    if(mode!=="uncertain") assert.deepEqual((receipts[0] as any).accepted,{
      run_id:f.claim.run_id,worker_id:client.memoryIncarnation!.worker_id,
      register_nonce:"original-memory-registration",claim_generation:7,memory_episode:3});
  }finally{await original?.workerMemory?.operation;await f.memory.stop();await monitoring;client.postMessages=post;client.reportState=report;git.verifyRunnerTrackingCovers=verify;o.restore();}
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
