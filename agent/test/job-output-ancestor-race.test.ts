import { it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { uploadJobOutputs } from "../src/job-outputs.js";
import { nullLogger } from "./helpers.js";

it("never uploads outside bytes after an output ancestor is swapped between candidates", { skip: process.platform !== "linux" ? "requires Linux descriptor-relative output opens" : false }, async () => {
 const root=await fs.mkdtemp(path.join(os.tmpdir(),"uzi-job-output-race-"));
 try {
  const work=path.join(root,"work"),outside=path.join(root,"private");
  await fs.mkdir(path.join(work,"outputs","late"),{recursive:true});
  await fs.mkdir(outside);
  await fs.writeFile(path.join(work,"outputs","first.txt"),"first output");
  await fs.writeFile(path.join(work,"outputs","late","result.txt"),"intended output");
  await fs.writeFile(path.join(outside,"result.txt"),"private fixture bytes");
  const uploaded:string[]=[];
  const summary=await uploadJobOutputs({
   workDir:work,secretPaths:[],runId:"r",generation:1,
   outputFiles:["outputs/first.txt","outputs/late/result.txt"],deadlineAt:Date.now()+10000,log:nullLogger(),
   client:{async uploadJobFile(_run,meta,body){
    assert.equal(typeof body,"function");
    const chunks:Buffer[]=[]; for await(const b of await (body as () => import("node:stream").Readable)()) chunks.push(Buffer.from(b));
    uploaded.push(Buffer.concat(chunks).toString());
    if(meta.display_name==="first.txt") {
     await fs.rename(path.join(work,"outputs","late"),path.join(work,"outputs","original"));
     await fs.symlink(outside,path.join(work,"outputs","late"));
    }
    return {status:201,file:{id:"stored",display_name:meta.display_name,storage_name:"",content_type:"text/plain",byte_size:meta.size,sha256:meta.sha256,state:"attached",expires_at:null}};
   }}
  });
  assert.equal(summary.stored,1);
  assert.deepEqual(uploaded,["first output"]);
  assert.deepEqual(summary.dropped,[{display_name:"result.txt",reason:"worker_unreadable"}]);
 } finally { await fs.rm(root,{recursive:true,force:true}); }
});


it("refuses an output when descriptor anchoring is unavailable", async (t) => {
 const root=await fs.mkdtemp(path.join(os.tmpdir(),"uzi-job-output-noproc-"));
 try {
  const work=path.join(root,"work");
  await fs.mkdir(path.join(work,"outputs"),{recursive:true});
  await fs.writeFile(path.join(work,"outputs","result.txt"),"output");
  t.mock.method(fs,"statfs",async()=>({type:0}));
  let calls=0;
  const summary=await uploadJobOutputs({workDir:work,secretPaths:[],runId:"r",generation:1,outputFiles:["outputs/result.txt"],deadlineAt:Date.now()+10000,log:nullLogger(),client:{async uploadJobFile(){calls++;throw new Error("must not upload");}}});
  assert.equal(calls,0);
  assert.equal(summary.stored,0);
  assert.deepEqual(summary.dropped,[{display_name:"result.txt",reason:"worker_unreadable"}]);
 } finally { t.mock.restoreAll(); await fs.rm(root,{recursive:true,force:true}); }
});
