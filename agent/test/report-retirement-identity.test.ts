import { it } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs/promises';
import path from 'node:path';
import { Outbox } from '../src/outbox.js';
import { nullLogger } from './helpers.js';
for (const kind of ['terminal', 'finalize'] as const) {
  it(`${kind}: replaced authenticated inode must refuse old lifetime identity`, async t => {
    const scratch = path.resolve('../.uzi/scratch');
    await fs.mkdir(scratch, { recursive: true });
    const root = await fs.mkdtemp(path.join(scratch, 'report-identity-'));
    t.after(() => fs.rm(root, { recursive: true, force: true }));
    const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1024 * 1024,
      maxBytes: 16 * 1024 * 1024, retentionMs: 86400000 });
    await outbox.init();
    const run = '00000000-0000-4000-8000-000000002652';
    if (kind === 'terminal') await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
    else await outbox.journalFinalize(run, 2);
    const context = kind === 'terminal'
      ? (await outbox.readTerminalJournalForRetirement(run, 2))!.context
      : { expectedIdentity: outbox.finalizeRecordIdentity(run, 2)! };
    if (kind === 'terminal') await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
    else await outbox.journalFinalize(run, 2);
    const adoptedIdentity = kind === 'terminal'
      ? (await outbox.readTerminalJournalForRetirement(run, 2))!.context.expectedIdentity
      : outbox.finalizeRecordIdentity(run, 2);
    assert.equal(adoptedIdentity, context.expectedIdentity, 'same installed winner preserves identity');
    const file = path.join(root, run, `${kind}-2.json`);
    const before = await fs.stat(file);
    await fs.writeFile(file + '.replacement', await fs.readFile(file));
    await fs.rename(file + '.replacement', file);
    assert.notEqual((await fs.stat(file)).ino, before.ino, 'installed inode changed');
    const budget = { signal: new AbortController().signal, deadline: Date.now() + 5000 };
    const retired = kind === 'terminal'
      ? await outbox.retireTerminalIfEligible(run, 2, { ...context, ...budget, eligible: () => true })
      : await outbox.retireFinalizeIfEligible(run, 2, () => true, budget.signal, context.expectedIdentity!, true, budget);
    assert.equal(retired, false, 'captured identity must not authorize replacement inode deletion');
    await fs.access(file);
    if (kind === 'terminal') await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
    else await outbox.journalFinalize(run, 2);
    const refreshed = kind === 'terminal'
      ? (await outbox.readTerminalJournalForRetirement(run, 2))!.context
      : { expectedIdentity: outbox.finalizeRecordIdentity(run, 2)! };
    assert.notEqual(refreshed.expectedIdentity, context.expectedIdentity, 'replacement requires new proof identity');
    assert.equal(kind === 'terminal'
      ? await outbox.retireTerminalIfEligible(run, 2, { ...refreshed, ...budget, eligible: () => true })
      : await outbox.retireFinalizeIfEligible(run, 2, () => true, budget.signal, refreshed.expectedIdentity!, true, budget), true);
    await assert.rejects(fs.access(file), { code: 'ENOENT' });
  });
}
for (const fault of ['cancellation', 'deadline'] as const) {
it(`terminal reauthentication stops issuing reads after ${fault}`, async t => {
  const scratch = path.resolve('../.uzi/scratch');
  await fs.mkdir(scratch, { recursive: true });
  const root = await fs.mkdtemp(path.join(scratch, 'report-cancel-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1024 * 1024,
    maxBytes: 16 * 1024 * 1024, retentionMs: 86400000 });
  await outbox.init();
  const run = '00000000-0000-4000-8000-000000002652';
  await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
  const context = (await outbox.readTerminalJournalForRetirement(run, 2))!.context;
  const target = path.join(root, run, 'terminal-2.json');
  const abort = new AbortController();
  const budget = { ...context, signal: abort.signal, deadline: Date.now() + 5000, eligible: () => true };
  const open = fs.open.bind(fs);
  let reads = 0;
  t.mock.method(fs, 'open', async (...args: Parameters<typeof open>) => {
    const file = await open(...args);
    if (String(args[0]) === target) {
      const read = file.read.bind(file);
      file.read = (async (buffer: Buffer, offset: number, length: number, position: number | null) => {
        reads++;
        const result = await read(buffer, offset, Math.min(length, 17), position);
        if (reads === 1) {
          if (fault === 'cancellation') abort.abort(new Error('cancel proof'));
          else budget.deadline = Date.now() - 1;
        }
        return result;
      }) as typeof file.read;
    }
    return file;
  });
  assert.equal(await outbox.retireTerminalIfEligible(run, 2, budget), false);
  await fs.access(target);
  assert.equal(reads, 1, 'no additional record reads after caller cancels proof');
});
}

for (const kind of ['terminal', 'finalize'] as const) {
  for (const fault of ['restart', 'replacement before unlink'] as const) {
    it(`${kind}: ${fault} refuses captured authority`, async t => {
      const scratch = path.resolve('../.uzi/scratch');
      await fs.mkdir(scratch, { recursive: true });
      const root = await fs.mkdtemp(path.join(scratch, 'report-lifetime-'));
      t.after(() => fs.rm(root, { recursive: true, force: true }));
      const options = { root, log: nullLogger(), runMaxBytes: 1024 * 1024,
        maxBytes: 16 * 1024 * 1024, retentionMs: 86400000 };
      let outbox = new Outbox(options);
      await outbox.init();
      const run = '00000000-0000-4000-8000-000000002652';
      if (kind === 'terminal') await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
      else await outbox.journalFinalize(run, 2);
      const context = kind === 'terminal'
        ? (await outbox.readTerminalJournalForRetirement(run, 2))!.context
        : { expectedIdentity: outbox.finalizeRecordIdentity(run, 2)! };
      const target = path.join(root, run, `${kind}-2.json`);
      if (fault === 'restart') {
        outbox = new Outbox(options);
        await outbox.init();
      } else {
        const original = await fs.readFile(target);
        const lstat = fs.lstat.bind(fs);
        let checks = 0;
        t.mock.method(fs, 'lstat', async (...args: Parameters<typeof lstat>) => {
          // Both report readers check the path twice; the third check precedes unlink.
          if (String(args[0]) === target && ++checks === 3) {
            await fs.writeFile(target + '.replacement', original);
            await fs.rename(target + '.replacement', target);
          }
          return lstat(...args);
        });
      }
      const budget = { signal: new AbortController().signal, deadline: Date.now() + 5000 };
      assert.equal(kind === 'terminal'
        ? await outbox.retireTerminalIfEligible(run, 2, { ...context, ...budget, eligible: () => true })
        : await outbox.retireFinalizeIfEligible(run, 2, () => true, budget.signal, context.expectedIdentity!, true, budget), false);
      await fs.access(target);
    });
  }
}

it('cancelled terminal proof holds O until its actual read and close settle', async t => {
  const scratch = path.resolve('../.uzi/scratch');
  await fs.mkdir(scratch, { recursive: true });
  const root = await fs.mkdtemp(path.join(scratch, 'report-read-settlement-'));
  t.after(() => fs.rm(root, { recursive: true, force: true }));
  const outbox = new Outbox({ root, log: nullLogger(), runMaxBytes: 1024 * 1024,
    maxBytes: 16 * 1024 * 1024, retentionMs: 86400000 });
  await outbox.init();
  const run = '00000000-0000-4000-8000-000000002652';
  await outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' });
  const context = (await outbox.readTerminalJournalForRetirement(run, 2))!.context;
  const target = path.join(root, run, 'terminal-2.json');
  let entered!: () => void, release!: () => void;
  const reading = new Promise<void>(resolve => { entered = resolve; });
  const held = new Promise<void>(resolve => { release = resolve; });
  const open = fs.open.bind(fs);
  let reads = 0, closed = false;
  t.mock.method(fs, 'open', async (...args: Parameters<typeof open>) => {
    const file = await open(...args);
    if (String(args[0]) === target) {
      const read = file.read.bind(file), close = file.close.bind(file);
      file.read = (async (buffer: Buffer, offset: number, length: number, position: number | null) => {
        if (++reads === 1) { entered(); await held; }
        return read(buffer, offset, length, position);
      }) as typeof file.read;
      file.close = async () => { await close(); closed = true; };
    }
    return file;
  });
  const abort = new AbortController();
  let settled = false, adopted = false;
  const retirement = outbox.retireTerminalIfEligible(run, 2,
    { ...context, signal: abort.signal, deadline: Date.now() + 5000, eligible: () => true })
    .then(result => { settled = true; return result; });
  await reading;
  abort.abort();
  const adoption = outbox.journalTerminal(run, 2, 'running', 0, { status: 'failed' })
    .then(result => { adopted = true; return result; });
  try {
    await new Promise<void>(resolve => setImmediate(resolve));
    assert.equal(settled, false);
    assert.equal(adopted, false);
    assert.equal(closed, false);
    assert.equal(reads, 1);
  } finally {
    release();
    const [retired, winner] = await Promise.all([retirement, adoption]);
    assert.equal(retired, false);
    assert.equal(winner.journaled, true);
  }
  assert.equal(closed, true);
  await fs.access(target);
});
