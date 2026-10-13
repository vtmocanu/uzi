import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
test('settlement diagnostic exposes pending FINAL evidence without payloads or MACs', async () => {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'settlement-diagnostic-'));
  const run = '11111111-1111-4111-8111-111111111111', capture = '22222222-2222-4222-8222-222222222222';
  try {
    for (const name of ['outbox', 'recovery']) await fs.mkdir(path.join(root, name, run), { recursive: true });
    await fs.writeFile(path.join(root, 'outbox', run, 'terminal-2.json'), JSON.stringify({ body: { status: 'failed', secret: 'DO_NOT_LOG' }, mac: 'DO_NOT_LOG' }));
    await fs.writeFile(path.join(root, 'recovery', run, capture + '.json'), JSON.stringify({ generation: 2, state: 'uploaded', finalAcknowledged: true,
      finalRequest: { disposition: { kind: 'archive' }, credentials: 'DO_NOT_LOG' }, mac: 'DO_NOT_LOG', sourceSha: 'a'.repeat(40), coverageDigest: 'b'.repeat(64) }));
    await fs.symlink(path.join(root, 'outbox', run, 'terminal-2.json'), path.join(root, 'recovery', run, '33333333-3333-4333-8333-333333333333.json'));
    const output = execFileSync(process.execPath, [fileURLToPath(new URL('./settlement-diagnostic.mjs', import.meta.url)), root], { encoding: 'utf8', timeout: 10000 });
    assert.ok(!output.includes('DO_NOT_LOG'));
    const row = JSON.parse(output);
    assert.equal(row.run_id, run);
    assert.equal(row.pending[0].generation, 2);
    assert.equal(row.recovery_records[0].final_acknowledged, true);
    assert.equal(row.recovery_records[0].final_kind, 'archive');
    assert.equal(row.recovery_records[1].error, 'unreadable_or_malformed');
    assert.equal(row.authenticated, false);
  } finally { await fs.rm(root, { recursive: true, force: true }); }
});
