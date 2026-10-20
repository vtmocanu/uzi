import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
const script = fileURLToPath(new URL('./recovery-record-projection.mjs', import.meta.url));
const RUN = '11111111-1111-4111-8111-111111111111', FOREIGN = '99999999-9999-4999-8999-999999999999';
const CAP = '22222222-2222-4222-8222-222222222222';
async function withRoot(fn) {
  const root = await fs.mkdtemp(path.join(os.tmpdir(), 'recovery-projection-'));
  try { await fn(root); } finally { await fs.rm(root, { recursive: true, force: true }); }
}
// Same invocation shape as the phase: script on stdin, root then run ids as arguments.
async function projectViaStdin(root, ids) {
  const src = await fs.readFile(script);
  const out = execFileSync(process.execPath, ['--input-type=module', '-', root, ...ids],
    { encoding: 'utf8', timeout: 15000, input: src, maxBuffer: 1 << 24 });
  return { out, rows: out.split('\n').filter(Boolean).map(l => JSON.parse(l)) };
}
const mk = (root, run) => fs.mkdir(path.join(root, 'recovery', run), { recursive: true });

test('projects only allowlisted fields and leaks nothing secret-shaped', () => withRoot(async root => {
  await mk(root, RUN); await mk(root, FOREIGN);
  const secrets = ['SECRET_MAC_VALUE', '/data/secret/bundle.tgz', 'c'.repeat(40), 'd'.repeat(64), 'SECRET_BODY'];
  await fs.writeFile(path.join(root, 'recovery', RUN, CAP + '.json'), JSON.stringify({
    generation: 3, state: 'bundled', reason: 'final_retry_refused', inventoryGuarded: true, finalAcknowledged: false,
    finalRequest: { body: 'SECRET_BODY' }, completionReceipt: { mac: 'SECRET_MAC_VALUE' },
    mac: 'SECRET_MAC_VALUE', bundlePath: '/data/secret/bundle.tgz', sourceSha: secrets[2], coverageDigest: secrets[3] }));
  await fs.writeFile(path.join(root, 'recovery', FOREIGN, CAP + '.json'), JSON.stringify({ generation: 7, state: 'pinned' }));
  const { out, rows } = await projectViaStdin(root, [RUN]);
  for (const s of secrets) assert.ok(!out.includes(s), s);
  assert.ok(!out.includes(FOREIGN) && !out.includes(root));
  assert.deepEqual(rows, [{ run_id: RUN, capture_id: CAP, generation: 3, state: 'bundled', reason: 'final_retry_refused',
    inventory_guarded: true, final_request_present: true, final_acknowledged: false,
    completion_receipt_present: true, authenticated: false, error: null }]);
}));

test('non-UUID ids are ignored and duplicates collapse', () => withRoot(async root => {
  await mk(root, RUN);
  await fs.writeFile(path.join(root, 'recovery', RUN, CAP + '.json'), '{}');
  const { rows } = await projectViaStdin(root, ['../etc', 'not-a-uuid', RUN, RUN, RUN.toUpperCase() + 'x']);
  assert.equal(rows.length, 1);
  assert.equal(rows[0].run_id, RUN);
}));

test('at most 16 run ids are read', () => withRoot(async root => {
  const ids = Array.from({ length: 20 }, (_, i) => `00000000-0000-4000-8000-${String(i).padStart(12, '0')}`);
  const { rows } = await projectViaStdin(root, ids);
  assert.equal(rows.length, 16);
  assert.deepEqual(rows.map(r => r.run_id), ids.slice(0, 16));
  assert.ok(rows.every(r => r.error === 'absent'));
}));

test('at most 64 files per run with a truncated marker', () => withRoot(async root => {
  await mk(root, RUN);
  for (let i = 0; i < 70; i++) await fs.writeFile(path.join(root, 'recovery', RUN, `c${String(i).padStart(3, '0')}.json`), '{"generation":1}');
  const { rows } = await projectViaStdin(root, [RUN]);
  assert.equal(rows.length, 65);
  assert.deepEqual(rows[64], { run_id: RUN, truncated: true });
  assert.equal(rows[0].capture_id, 'c000');
}));

test('oversized file reports a bounded error', () => withRoot(async root => {
  await mk(root, RUN);
  await fs.writeFile(path.join(root, 'recovery', RUN, CAP + '.json'), Buffer.alloc(4 * 1024 * 1024 + 1, 0x20));
  const { rows } = await projectViaStdin(root, [RUN]);
  assert.equal(rows[0].error, 'not_bounded_regular_file');
  assert.equal(rows[0].state, null);
}));

test('symlinked record file and symlinked run dir are refused', () => withRoot(async root => {
  await mk(root, RUN);
  const target = path.join(root, 'target.json');
  await fs.writeFile(target, JSON.stringify({ generation: 5, state: 'pinned' }));
  await fs.symlink(target, path.join(root, 'recovery', RUN, CAP + '.json'));
  const other = '33333333-3333-4333-8333-333333333333';
  await fs.mkdir(path.join(root, 'elsewhere'));
  await fs.writeFile(path.join(root, 'elsewhere', CAP + '.json'), JSON.stringify({ generation: 5, state: 'pinned' }));
  await fs.symlink(path.join(root, 'elsewhere'), path.join(root, 'recovery', other));
  const { out, rows } = await projectViaStdin(root, [RUN, other]);
  assert.equal(rows[0].error, 'unreadable_or_malformed');
  assert.equal(rows[0].generation, null);
  assert.deepEqual(rows[1], { run_id: other, error: 'unreadable_or_malformed' });
  assert.ok(!out.includes('"generation":5'));
}));

test('unrecognized reason is not echoed; absent reason is null', () => withRoot(async root => {
  await mk(root, RUN);
  await fs.writeFile(path.join(root, 'recovery', RUN, 'a.json'), JSON.stringify({ reason: 'Has Spaces /path SECRET', state: 'bogus', generation: -1 }));
  await fs.writeFile(path.join(root, 'recovery', RUN, 'b.json'), JSON.stringify({ generation: 1 }));
  const { out, rows } = await projectViaStdin(root, [RUN]);
  assert.equal(rows[0].reason, 'unrecognized');
  assert.equal(rows[0].state, null);
  assert.equal(rows[0].generation, null);
  assert.equal(rows[1].reason, null);
  assert.ok(!out.includes('SECRET'));
}));

test('absent run directory yields one absent line', () => withRoot(async root => {
  const { rows } = await projectViaStdin(root, [RUN]);
  assert.deepEqual(rows, [{ run_id: RUN, error: 'absent' }]);
}));
