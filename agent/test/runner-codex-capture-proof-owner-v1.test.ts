import { it } from "node:test";
import assert from "node:assert/strict";
import { observe } from "./runner-codex-capture-proof-fixture.js";
import { MaintenanceBudget } from "../src/maintenance-budget.js";
import { api, client, fx, git, gitlabClaim, installHarness, runner } from "./runner-harness.js";
import { JOIN_TOKEN, command, recoveryOf } from "./runner-held-publication-fixture.js";
installHarness();
for (const missing of [false,true]) it(`A owner V1 ${missing ? "MISSING_SCOPE" : "accepted"}`, async t => {
 const claim=gitlabClaim(30301,{claim_generation:7});
 claim.repo.default_branch="main";
 claim.secrets.codex={auth_mode:"api_key",access_token:"fixture-key",capability:"fixture-capability"};
 command(fx.originPath,["branch","agent/issue-30301","main"]);
 client.protocolFeatures=["recovery_release_exact_echo"];
 const sourceFilter: { sourcePath?: string } = {};
 const proof=observe(sourceFilter);
 const snapshot = () => { proof.recordSource(sourceFilter.sourcePath); return proof.observations().source.at(-1); };
 t.after(() => proof.restore());
 const events: Array<{name:string; state:unknown; value?:unknown}>=[];
 let budget:MaintenanceBudget|undefined;
 const clean=git.credentialFreeCancelCleanHead.bind(git);
 git.credentialFreeCancelCleanHead=async (...args) => {
   proof.recordSource(args[0]); events.push({name:"clean-entry",state:snapshot()});
   const out=await clean(...args); events.push({name:"clean-return",state:snapshot(),value:out}); return out;
 };
 const fetch=globalThis.fetch;
 t.mock.method(globalThis,"fetch",async (input:Parameters<typeof fetch>[0],init?:Parameters<typeof fetch>[1])=>{
   if (String(input).endsWith(`/runs/${claim.run_id}/archives/release`)) {
     events.push({name:"release-http",state:snapshot(),value:JSON.parse(String(init?.body))});
     return Response.json({run_id:claim.run_id,generation:7,released:true,holds_released:1,retained:false});
   }
   return fetch(input,init);
 });
 const release=client.releaseRecoveryCustody.bind(client);
 client.releaseRecoveryCustody=async (...args)=>{const out=await release(...args);events.push({name:"release-ack",state:snapshot(),value:out});return out;};
 const pin=git.deleteRecoveryPin.bind(git);
 git.deleteRecoveryPin=async (...args)=>{await pin(...args);events.push({name:"pin-delete",state:snapshot()});};
 const executor={withCredentialFreeCapture:missing?undefined:proof.capture,settleForCredentialFreeCapture:proof.rig.settle,
 async run(ctx:any){sourceFilter.sourcePath = ctx.worktreePath;proof.arm(); api.setInputs(claim.run_id,[{id:1,kind:"cancel"}]); await new Promise<void>(resolve=>ctx.signal.addEventListener("abort",()=>resolve(),{once:true}));throw new Error("run cancelled");},killAgentTree(){}};
 const r=runner(executor,fake(),JOIN_TOKEN,{cancellationBudgetFactory:()=>budget=new MaintenanceBudget(600000),quiesceRun:async()=>({process:{state:"quiescent",processes:[],killed:[],detail:"fixture"},docker:{state:"not_wired",removed:[],detail:"fixture"}})});
 const forget=recoveryOf(r).forgetGeneration.bind(recoveryOf(r));
 recoveryOf(r).forgetGeneration=async (...args)=>{await forget(...args);events.push({name:"forget",state:snapshot()});};
 await proof.execute(r.execute(claim));
 console.log("OWNER_V1",JSON.stringify({events,budgetDeadline:budget?.deadline,proof:proof.observations()}));
 if(missing){assert.equal(events.filter(x=>["clean-entry","release-http","pin-delete"].includes(x.name)).length,0,"missing scope forbids source/release/pin operations");return;}
 proof.assertProof(false);
 for(const name of ["clean-entry","clean-return","release-http","release-ack","forget","pin-delete"]){const row=events.find(x=>x.name===name);assert.ok(row,`actual ${name} occurred`);assert.equal((row.state as any).active,true,`${name} remains held`);}
 assert.ok(events.find(x=>x.name==="clean-return")?.value,"clean helper accepted real source");
});
import {fakeGitlab} from "./runner-harness.js";
function fake(){return fakeGitlab().gitlab;}
