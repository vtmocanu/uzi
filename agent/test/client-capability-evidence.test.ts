import { afterEach, describe, it } from "node:test";
import assert from "node:assert/strict";
import http from "node:http";
import type { AddressInfo } from "node:net";
import { WorkerClient, type PlanCrossCheckStateRequest } from "../src/client.js";
import { CAPABILITY_INFERENCE_EVIDENCE_FEATURE } from "../src/protocol.js";
import { nullLogger } from "./helpers.js";

const servers: http.Server[] = [];
afterEach(async () => { await Promise.all(servers.splice(0).map(s => new Promise<void>((resolve,reject) => s.close(e => e ? reject(e) : resolve())))); });

describe("capability evidence negotiation", () => {
  for (const lane of ["ordinary", "checked", "semantic"] as const) {
    for (const enabled of [false, true]) it(`${lane}: advisory field negotiated=${enabled}`, async () => {
      const posts: Record<string,unknown>[] = [];
      const server = http.createServer((req,res) => {
        let raw="";req.on("data",c => raw+=String(c));req.on("end",() => {
          const body = JSON.parse(raw) as Record<string,unknown>; posts.push(body);
          const known = new Set(["status","plan_md","claim_generation","candidate_digest",...(enabled?["capability_inference_evidence"]:[])]);
          if (Object.keys(body).some(key=>!known.has(key))) {
            res.writeHead(400,{"Content-Type":"application/json"});res.end(JSON.stringify({error:"unknown field"}));return;
          }
          res.writeHead(200,{"Content-Type":"application/json"});
          res.end(JSON.stringify({run:{id:"r1",status:"running"}}));
        });
      });
      servers.push(server);
      await new Promise<void>(resolve => server.listen(0,"127.0.0.1",resolve));server.unref();
      const c = new WorkerClient(`http://127.0.0.1:${(server.address() as AddressInfo).port}`,"test-worker-token","test",nullLogger(),{sleep:async()=>{}});
      c.protocolFeatures=["claim_generation_fence","gate_revision_v1",...(enabled?[CAPABILITY_INFERENCE_EVIDENCE_FEATURE]:[])];
      const body: PlanCrossCheckStateRequest={status:"running",plan_md:"plan",claim_generation:1,candidate_digest:"a".repeat(64),capability_inference_evidence:{jvm:{triggers:[{path:"app/android/build.gradle",reason:"jvm_manifest"}],truncated:false}}};
      if(lane==="ordinary") await c.reportState("r1",body);
      else if(lane==="checked") await c.reportPlanCrossCheckGateState("r1",body);
      else await c.reportSemanticPlanGateState("r1",body);
      assert.equal(posts.length,1);
      const posted = posts[0]; assert.ok(posted);
      assert.equal("capability_inference_evidence" in posted,enabled);
      assert.equal(posted.candidate_digest,body.candidate_digest);
      assert.equal(posted.claim_generation,1);
      assert.equal(posted.plan_md,"plan");
      assert.ok(body.capability_inference_evidence,"negotiation must not mutate caller snapshot");
    });
  }
});
