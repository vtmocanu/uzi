// Read-only, bounded, allowlisted E2E projection of worker recovery records.
// Usage: node --input-type=module - <data-root> <run-id>... < this-file
// Never print bodies, paths, tokens, SHAs, MACs or bundle locations: every emitted
// field is an enum, a boolean, a safe integer or a string matched against a pattern.
// Observed state only; nothing here authenticates a record or its producer.
import fs from 'node:fs/promises';
import path from 'node:path';
import { constants } from 'node:fs';
const root = process.argv[2] ?? '/data';
const uuid = /^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i;
const MAX_RUNS = 16, MAX_FILES = 64, MAX_SCAN = 1024, MAX_BYTES = 4 * 1024 * 1024;
// Capture ids are randomUUID() (agent/src/recovery.ts UUID_V4); anything else, a hash-shaped name
// included, is never echoed.
const captureId = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const deadline = Date.now() + 5000;
const runs = [...new Set(process.argv.slice(3).filter(id => uuid.test(id)))].slice(0, MAX_RUNS);
const integer = v => Number.isSafeInteger(v) && v >= 0 ? v : null;
const bool = v => typeof v === 'boolean' ? v : null;
const state = v => ['pinned', 'bundled', 'uploaded', 'needs_action'].includes(v) ? v : null;
const reason = v => v === undefined || v === null ? null
  : typeof v === 'string' && /^[a-z0-9_:]{1,96}$/.test(v) && !/[0-9a-f]{16,}/.test(v) ? v : 'unrecognized';
const present = v => v !== undefined && v !== null;
async function read(file) {
  if (Date.now() >= deadline) return { error: 'deadline' };
  let handle;
  try {
    handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = await handle.stat();
    if (!stat.isFile() || stat.size > MAX_BYTES) return { error: 'not_bounded_regular_file' };
    const bytes = Buffer.alloc(stat.size + 1);
    const { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
    if (bytesRead !== stat.size) return { error: 'size_changed' };
    return { value: JSON.parse(bytes.subarray(0, bytesRead).toString()) };
  } catch { return { error: 'unreadable_or_malformed' }; }
  finally { await handle?.close(); }
}
const emit = row => console.log(JSON.stringify(row));
for (const run of runs) {
  const dir = path.join(root, 'recovery', run);
  const names = [];
  let truncated = null;
  try {
    const s = await fs.lstat(dir);
    if (!s.isDirectory() || s.isSymbolicLink()) { emit({ run_id: run, error: 'unreadable_or_malformed' }); continue; }
    // Bounded enumeration: never list the whole directory. Stop at MAX_SCAN entries, at the
    // deadline, or once one record beyond MAX_FILES is seen.
    const handle = await fs.opendir(dir, { bufferSize: 64 });
    let scanned = 0;
    try {
      for await (const entry of handle) {
        if (Date.now() >= deadline) { truncated = 'deadline'; break; }
        if (++scanned > MAX_SCAN) { truncated = 'scan_limit'; break; }
        if (!entry.name.endsWith('.json')) continue;
        if (names.length === MAX_FILES) { truncated = 'file_limit'; break; }
        names.push(entry.name);
      }
    } finally { await handle.close().catch(() => {}); }
    names.sort();
  } catch (e) { emit({ run_id: run, error: e?.code === 'ENOENT' ? 'absent' : 'unreadable_or_malformed' }); continue; }
  for (const name of names) {
    const stem = name.slice(0, -5);
    const r = await read(path.join(dir, name));
    const v = r.value && typeof r.value === 'object' ? r.value : undefined;
    emit({ run_id: run, capture_id: captureId.test(stem) ? stem : null,
      generation: integer(v?.generation), state: state(v?.state), reason: reason(v?.reason),
      inventory_guarded: bool(v?.inventoryGuarded), final_request_present: present(v?.finalRequest),
      final_acknowledged: bool(v?.finalAcknowledged), completion_receipt_present: present(v?.completionReceipt),
      authenticated: false, error: r.error ?? (v ? null : 'unreadable_or_malformed') });
  }
  if (truncated) emit({ run_id: run, truncated: true, truncated_reason: truncated });
}
