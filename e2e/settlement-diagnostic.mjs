// Read-only, bounded E2E diagnostic. Never print bodies, paths, tokens or MACs.
import fs from 'node:fs/promises';
import path from 'node:path';
import { constants } from 'node:fs';
const root = process.argv[2] ?? '/data';
let remaining = 256;
const deadline = Date.now() + 5000;
const uuid = /^[0-9a-f]{8}-(?:[0-9a-f]{4}-){3}[0-9a-f]{12}$/i;
const sha = /^[0-9a-f]{40}$/;
const digest = /^[0-9a-f]{64}$/;
const enumValue = (v, values) => values.includes(v) ? v : null;
const integer = v => Number.isSafeInteger(v) && v >= 0 ? v : null;
const safe = (v, pattern) => typeof v === 'string' && pattern.test(v) ? v : null;
const reason = v => enumValue(v, ['inventory_source_not_quiescent:execution_tail_present',
  'inventory_source_not_quiescent:non_linux_host', 'inventory_snapshot_changed',
  'inventory_quiescence_breach', 'upload_transient', 'early_pin_only_after_restart',
  'final_retry_refused', 'final_retry_unknown', 'inventory_hold_settled']);
async function names(dir) {
  if (remaining <= 0 || Date.now() >= deadline) return [];
  try { const s = await fs.lstat(dir); if (!s.isDirectory() || s.isSymbolicLink()) return []; return (await fs.readdir(dir)).sort().slice(0, 256); }
  catch { return []; }
}
async function read(file) {
  if (remaining-- <= 0 || Date.now() >= deadline) return { error: 'diagnostic_budget' };
  let handle;
  try {
    handle = await fs.open(file, constants.O_RDONLY | constants.O_NOFOLLOW | constants.O_NONBLOCK);
    const stat = await handle.stat();
    if (!stat.isFile() || stat.size > 4 * 1024 * 1024) return { error: 'not_bounded_regular_file' };
    const bytes = Buffer.alloc(stat.size + 1);
    const { bytesRead } = await handle.read(bytes, 0, bytes.length, 0);
    if (bytesRead !== stat.size) return { error: 'size_changed' };
    return { value: JSON.parse(bytes.subarray(0, bytesRead).toString()), mtime: stat.mtime.toISOString() };
  } catch { return { error: 'unreadable_or_malformed' }; }
  finally { await handle?.close(); }
}
for (const run of await names(path.join(root, 'outbox'))) {
  if (!uuid.test(run)) continue;
  const reports = (await names(path.join(root, 'outbox', run))).filter(n => /^(terminal|finalize)-[0-9]+\.json$/.test(n));
  if (!reports.length) continue;
  const pending = [];
  for (const file of reports) {
    const r = await read(path.join(root, 'outbox', run, file));
    pending.push({ kind: file.startsWith('terminal') ? 'terminal' : 'finalize',
      generation: Number(file.match(/[0-9]+/)[0]), mtime: r.mtime ?? null, error: r.error ?? null,
      outcome: enumValue(r.value?.body?.status, ['completed', 'failed', 'cancelled']) });
  }
  const records = [];
  for (const file of await names(path.join(root, 'recovery', run))) {
    if (!uuid.test(file.replace(/\.json$/, '')) || !file.endsWith('.json')) continue;
    const r = await read(path.join(root, 'recovery', run, file)), v = r.value;
    records.push({ capture_id: file.slice(0, -5), mtime: r.mtime ?? null, error: r.error ?? null,
      generation: integer(v?.generation), state: enumValue(v?.state, ['pinned', 'bundled', 'uploaded', 'needs_action']),
      final_acknowledged: typeof v?.finalAcknowledged === 'boolean' ? v.finalAcknowledged : null,
      final_kind: enumValue(v?.finalRequest?.disposition?.kind, ['archive', 'settled', 'held_publication']),
      server_capture_id: safe(v?.serverCaptureId, uuid), source_sha: safe(v?.sourceSha, sha),
      coverage_digest: safe(v?.coverageDigest, digest), retained_reason: reason(v?.reason) });
  }
  console.log(JSON.stringify({ run_id: run, observed_at: new Date().toISOString(), pending, recovery_records: records,
    assumed_code_defaults: { per_pass: 4, base_spacing_ms: 30000, max_backoff_ms: 900000 },
    limit: 256, authenticated: false }));
}
