import { it } from "node:test";
import assert from "node:assert/strict";
import { promises as fs } from "node:fs";
import os from "node:os";
import path from "node:path";
import { openJobOutputFile } from "../src/job-workspace.js";

it("pins a directory across a swap during the descriptor walk", { skip: process.platform !== "linux" ? "requires Linux procfs" : false }, async () => {
 const root=await fs.mkdtemp(path.join(os.tmpdir(),"uzi-job-output-pin-"));
 try {
  const work=path.join(root,"work"),outside=path.join(root,"private");
  await fs.mkdir(path.join(work,"outputs","nested"),{recursive:true});
  await fs.mkdir(path.join(outside,"nested"),{recursive:true});
  await fs.writeFile(path.join(work,"outputs","nested","result.txt"),"intended output");
  await fs.writeFile(path.join(outside,"nested","result.txt"),"private fixture bytes");
  const handle=await openJobOutputFile(work,"outputs/nested/result.txt",{afterPin:async (dir)=>{
   if(dir!=="outputs")return;
   await fs.rename(path.join(work,"outputs"),path.join(work,"original"));
   await fs.symlink(outside,path.join(work,"outputs"));
  }});
  try { assert.equal(await handle.readFile("utf8"),"intended output"); } finally { await handle.close(); }
 } finally { await fs.rm(root,{recursive:true,force:true}); }
});


it("refuses a symlink at a workspace ancestor", { skip: process.platform !== "linux" ? "requires Linux procfs" : false }, async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-job-root-link-"));
  try {
    const outside = path.join(root, "private");
    await fs.mkdir(path.join(outside, "work", "outputs"), { recursive: true });
    await fs.writeFile(path.join(outside, "work", "outputs", "result.txt"), "private fixture bytes");
    await fs.symlink(outside, path.join(root, "job"));
    await assert.rejects(openJobOutputFile(path.join(root, "job", "work"), "outputs/result.txt"));
  } finally {
    await fs.rm(root, { recursive: true, force: true });
  }
});

it("refuses a workspace not owned by the worker", { skip: process.platform !== "linux" ? "requires Linux procfs" : false }, async (t) => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), "uzi-job-root-owner-"));
  try {
    const work = path.join(root, "work");
    await fs.mkdir(path.join(work, "outputs"), { recursive: true });
    await fs.writeFile(path.join(work, "outputs", "result.txt"), "output");
    const uid = process.getuid!();
    t.mock.method(process as typeof process & { getuid: () => number }, "getuid", () => uid + 1);
    await assert.rejects(openJobOutputFile(work, "outputs/result.txt"), /not owned by this worker/);
  } finally {
    t.mock.restoreAll();
    await fs.rm(root, { recursive: true, force: true });
  }
});
