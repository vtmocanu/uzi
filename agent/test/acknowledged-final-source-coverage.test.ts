import { it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { RecoveryCoordinator } from '../src/recovery.js';
import { nullLogger } from './helpers.js';

for (const [covered, earlierSynthetic, name] of [
  [false, false, 'refuses an uncovered competing FINAL source SHA'],
  [true, false, 'retires with a covered competing FINAL source SHA'],
  [true, true, 'retires despite an earlier aggregate whose synthetic tip is not an archive ancestor'],
] as const) it(`acknowledged-FINAL report retirement ${name} (#2883)`, async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'ack-final-source-'));
  try {
    const bare = path.join(root, 'repo.git');
    execFileSync('git', ['init', '--bare', bare]);
    const env = { ...process.env, GIT_AUTHOR_NAME:'test', GIT_AUTHOR_EMAIL:'test@example.test', GIT_COMMITTER_NAME:'test', GIT_COMMITTER_EMAIL:'test@example.test' };
    const cmd = (args: string[], input?: string) => execFileSync('git', ['--git-dir', bare, ...args], { env, input, encoding:'utf8' }).trim();
    const tree = cmd(['mktree'], '');
    const base = cmd(['commit-tree', tree], 'published base\n');
    const uncovered = cmd(['commit-tree', tree, '-p', base], 'uncovered source\n');
    const runId = '11111111-1111-4111-8111-111111111111', generation=1;
    const ctx = { barePath:bare, runId, generation, kind:'issue', branch:'agent/issue-2938', defaultIdentity:{ref:'refs/remotes/origin/main',sha:base} };
    const checksum = '0'.repeat(64), digest='e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
    const archiveId = '22222222-2222-4222-8222-222222222222';
    const client:any = {
      getRunOwnership: async () => ({status:'completed',claim_generation:generation,inventory_guarded:true}),
      getRecoveryCaptureStatus: async () => ({capture_id:archiveId,state:'available',manifest_bound:true,checksum,byte_size:1,expires_at:'2099-01-01T00:00:00Z'}),
      withAcknowledgedFinalReportCustody: async (_r:any,_g:any,_b:any,action:any) => action(() => true),
    };
    const git:any = {
      withReportProofBudget: async (_b:any, action:any) => action(),
      withBareLock: async (_b:any, action:any) => action(),
      resolveRecoveryBareDir: async () => bare,
      readInventoryCloneHeads: async () => ({kind:'verified',heads:[],clones:[]}),
      enumerateOwedUnderLock: async () => [],
      ancestry: async (_b:any,a:string,b:string) => {
        try {cmd(['merge-base','--is-ancestor',a,b]);return 'ancestor';} catch {return 'divergent';}
      },
    };
    const recovery = new RecoveryCoordinator({ client, git, log:nullLogger(), recoveryRoot:path.join(root,'recovery'), workerToken:'buddy-test-key' });
    const common:any = {version:1,runId,generation,inventoryGuarded:true,kind:'issue',branch:ctx.branch,createdAt:Date.now(),bareDir:'repo.git',defaultBranch:'main',coverageContext:ctx,coverageDigest:digest,originalSourceSha:base,inventoryCurrentSha:base,originalRoots:[]};
    const archive = {...common,captureId:archiveId,sourceSha:base,state:'uploaded',serverCaptureId:archiveId,checksum,byteSize:1,finalAcknowledged:true,finalRequest:{disposition:{kind:'archive',capture_id:archiveId,source_sha:base,coverage_digest:digest}}};
    const pending = {...common,captureId:'33333333-3333-4333-8333-333333333333',sourceSha:covered ? base : uncovered,state:'needs_action',reason:'inventory_source_not_quiescent:execution_tail_present',finalRequest:{disposition:{kind:'settled',coverage_digest:digest},evidence:'publication'}};
    await (recovery as any).writeRecordUnlocked(archive);
    await (recovery as any).writeRecordUnlocked(pending);
    if (earlierSynthetic) {
      const synthetic = cmd(['commit-tree', tree, '-p', base], 'earlier recovery aggregate\n');
      await (recovery as any).writeRecordUnlocked({...common,captureId:'44444444-4444-4444-8444-444444444444',sourceSha:synthetic,state:'uploaded',serverCaptureId:'44444444-4444-4444-8444-444444444444',checksum,byteSize:1});
    }
    assert.equal((await recovery.inspect(runId)).length, earlierSynthetic ? 3 : 2,'every journal must authenticate and parse');
    let removed=0;
    await recovery.retireAcknowledgedFinalReport(runId,generation,{signal:new AbortController().signal,deadline:Date.now()+10_000},action=>action(()=>true),async()=>true,async()=>{removed++;});
    assert.equal(removed, covered ? 1 : 0, covered ? 'covered competing source retires the report' : 'uncovered competing sourceSha must refuse report retirement');
  } finally {await fs.rm(root,{recursive:true,force:true});}
});
