import assert from "node:assert/strict";
import fs from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { runnerCommand, commandRootCommand, RUNNER_UID } from "../src/runner-uid.js";
import type { CommandWrapper } from "../src/rmtree.js";
import { residualTestFixture } from "./residual-fixtures.js";
import { RACED_DIRS, RACED_FILES } from "./swap-racer.js";
import { AdviceTeardownDiagnostic } from "./advice-teardown-diagnostic.js";
import type { CodexTransport } from "../src/codex/transport.js";

/** Fixture commands carry only inert environment values, never the worker environment. */
export function uidScript(wrap: CommandWrapper, script: string, ...args: string[]): void {
  const command = wrap(process.execPath, ["-e", script, ...args]);
  const result = spawnSync(command.command, command.args, { env: { PATH: "/usr/bin:/bin" }, timeout: 10_000 });
  assert.equal(result.status, 0, result.stderr?.toString() || result.error?.message);
}

export async function runnerTeardownFixture(body: (root: string, victim: string, diagnostic: AdviceTeardownDiagnostic) => Promise<void>,
  diagnoseAdvice = false, output?: (line: string) => void): Promise<void> {
  const saved = process.env.UZI_UID_SPLIT;
  const diagnostic = new AdviceTeardownDiagnostic(output);
  process.env.UZI_UID_SPLIT = "1";
  try {
    await residualTestFixture(async (root, victim) => {
      await fs.chown(root, -1, RUNNER_UID);
      await fs.chmod(root, 0o3775);
      await fs.chown(victim, -1, RUNNER_UID);
      await fs.chmod(victim, 0o2770);
      try {
        if (diagnoseAdvice) await diagnostic.run(() => body(root, victim, diagnostic));
        else await body(root, victim, diagnostic);
      }
      finally {
        process.env.UZI_UID_SPLIT = "1";
        // Only after every racer/provider has settled: restore each fixture owner's dirs,
        // including deliberately inaccessible mixed-private content. Never production cleanup.
        const widen = `const fs=require('node:fs');function walk(p){let s;try{s=fs.lstatSync(p)}catch{return}if(!s.isDirectory())return;if(s.uid===process.getuid())fs.chmodSync(p,(s.mode&0o7777)|0o770);let entries;try{entries=fs.readdirSync(p)}catch{return}for(const n of entries)walk(p+'/'+n)}walk(process.argv[1]);`;
        uidScript(runnerCommand, widen, root);
        uidScript(commandRootCommand, widen, root);
        for (const wrap of [commandRootCommand, runnerCommand]) {
          uidScript(wrap, "try{require('node:fs').rmSync(process.argv[1],{recursive:true,force:true})}catch{}", root);
        }
      }
    });
  } catch (error) {
    // Fixture cleanup can fail too; diagnostic advice fixtures keep the first
    // assertion as the test result after attempting the existing cleanup.
    if (diagnoseAdvice) diagnostic.rethrow(error);
    throw error;
  } finally {
    if (saved === undefined) delete process.env.UZI_UID_SPLIT; else process.env.UZI_UID_SPLIT = saved;
  }
}

export function seedRunnerRacedTree(target: string, victim: string): void {
  uidScript(runnerCommand, `const fs=require('node:fs');const [root,victim,dirs,files]=process.argv.slice(1);
    for(let i=0;i<Number(dirs);i++){const d=root+'/d'+i+'/x';fs.mkdirSync(d,{recursive:true});fs.chmodSync(root+'/d'+i,0o2770);fs.chmodSync(d,0o2770);for(let j=0;j<Number(files);j++)fs.writeFileSync(d+'/f'+j,'')}
    for(let j=0;j<Number(files);j++)fs.writeFileSync(victim+'/f'+j,'keep\\n');`, target, victim, String(RACED_DIRS), String(RACED_FILES));
}

export function createRunnerTree(target: string, mode = "700"): void {
  uidScript(runnerCommand, "const fs=require('node:fs');fs.mkdirSync(process.argv[1],{mode:parseInt(process.argv[2],8)});fs.chmodSync(process.argv[1],parseInt(process.argv[2],8))", target, mode);
}

export async function assertGone(target: string): Promise<void> {
  await assert.rejects(fs.lstat(target), { code: "ENOENT" });
}

export function writePrivateRunnerFile(target: string): void {
  uidScript(runnerCommand, "const fs=require('node:fs');fs.mkdirSync(process.argv[1]+'/private',{mode:0o700});fs.writeFileSync(process.argv[1]+'/private/keep','keep');fs.chmodSync(process.argv[1]+'/private',0o555)", target);
}

/** The advice leaves mutate the owned root only after the app-server answered `initialize`.
 * On the real worker-UID lane this prevented the reproduced teardown failures (issue #2397);
 * that a mutation during provider startup is the exact cause remains inferred. */
export async function awaitAdviceProviderReady(transport: CodexTransport, diagnostic: AdviceTeardownDiagnostic): Promise<void> {
  try {
    await transport.request("initialize", {
      clientInfo: { name: "uzi", title: "uzi", version: "0.1.0-m4" },
      capabilities: { experimentalApi: true, requestAttestation: false },
    });
  } catch {
    diagnostic.mark("provider_not_ready");
    assert.fail("advice provider was not ready before the root mutation");
  }
  diagnostic.mark("provider_ready");
}
