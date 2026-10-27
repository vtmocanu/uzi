import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { CAPABILITY_INFERENCE_EVIDENCE_FEATURE } from "../src/protocol.js";
import { api, client, fakeGitlab, fx, installHarness, runner } from "./runner-harness.js";
import { GateExecutor, freshClaim, newApi, row, send } from "./gate-revision-harness.js";

installHarness();
describe("runner capability observations",()=>{
  for(const auto of [false,true]) for(const marker of [false,true])
    it(`raw detector evidence on ${auto?"autopilot":"human gate"}, marker=${marker}`,async()=>{
      newApi();client.protocolFeatures=[...client.protocolFeatures,CAPABILITY_INFERENCE_EVIDENCE_FEATURE];
      if(marker){
        fs.writeFileSync(path.join(fx.originPath,"Dockerfile"),"FROM alpine\n");
        const env={...process.env};for(const key of Object.keys(env))if(key.startsWith("GIT_"))delete env[key];
        env.GIT_CONFIG_GLOBAL="/dev/null";env.GIT_CONFIG_SYSTEM="/dev/null";
        execFileSync("git",["-C",fx.originPath,"add","Dockerfile"],{env,stdio:"pipe"});
        execFileSync("git",["-C",fx.originPath,"commit","-m","detector fixture"],{env,stdio:"pipe"});
      }
      const claim=freshClaim(1,{auto_approve:auto});
      api.onState(claim.run_id,body=>{if(body.status==="awaiting_approval")send(claim.run_id,row("approve_plan"));});
      const {gitlab}=fakeGitlab();
      await runner(new GateExecutor(["# plan"]),gitlab,undefined,{planApprovalTimeoutMs:4000}).execute(claim);
      const report=api.states.find(s=>s.runId===claim.run_id && s.body.plan_md==="# plan" && s.body.status===(auto?"running":"awaiting_approval"));
      assert.ok(report,"accepted plan report exists");
      assert.deepEqual(report.body.capability_inference_evidence,marker?{docker:{triggers:[{path:"Dockerfile",reason:"docker_marker"}],truncated:false}}:{});
    });
});
