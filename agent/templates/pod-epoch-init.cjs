'use strict';

// Image-baked: no tsx, npm, persistent scratch, or third-party dependencies.
// Shell protocol (all failures abort startup):
//   identity=$(node HELPER prepare /data)
//   if [ "$identity" != unsupported ]; then
//     exec 9< /data/.uzi-epoch.lock
//   fi
//   node HELPER hold /data "$identity" 9
//   ... synchronous protected initialization ...
//   exec 9>&-
// prepare reads only; hold checks FD9 against the pre-open identity and pathname,
// locks that inherited open description, then checks again. Shell open is READ
// ONLY: it cannot create, truncate, or write a swapped target. A swapped path
// cannot authorize protected writes. Never substitute a shell output redirection.
// Keep /data root trusted (not attacker-writable); neither O_NOFOLLOW nor flock
// protects against a malicious root/ancestor-directory replacement.
// seed runs synchronously BEFORE this SH protocol. Never invoke seed under SH.
const fs = require('node:fs');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { performance } = require('node:perf_hooks');
const { randomUUID } = require('node:crypto');
const { setTimeout: delay } = require('node:timers/promises');

const HEADER = 'UZIEC01\n';
const MAX_RECORDS = 4096;
const MAX_BYTES = 151560;
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;
const FLOCK = '/usr/bin/flock';

function uuid(value) {
  if (typeof value !== 'string' || !UUID.test(value)) throw new Error('invalid pod UUID');
  return value;
}
function identity(stat) { return `${stat.dev}:${stat.ino}`; }
function aborted(signal) { signal?.throwIfAborted(); }

// Each helper has a hard deadline, including --version. Abort/timeout kills only
// this child and joins its close event before releasing the inherited descriptor.
function runHelper(args, fd, signal, deadline, spawnProcess = spawn) {
  return new Promise((resolve, reject) => {
    try { aborted(signal); } catch (error) { reject(error); return; }
    const child = spawnProcess(FLOCK, args, {
      stdio: ['ignore', 'pipe', 'pipe', ...(fd === undefined ? [] : [fd])],
      env: { PATH: '/usr/bin:/bin', LC_ALL: 'C' },
    });
    let output = '';
    let failure;
    const stop = (error) => { failure ??= error; child.kill('SIGKILL'); };
    const timer = setTimeout(() => stop(new Error('flock helper timeout')), Math.max(1, deadline - performance.now()));
    const onAbort = () => stop(signal.reason ?? new Error('aborted'));
    signal?.addEventListener('abort', onAbort, { once: true });
    const collect = (chunk) => {
      if (output.length + chunk.length > 4096) stop(new Error('flock output limit'));
      else output += chunk.toString('utf8');
    };
    child.stdout.on('data', collect);
    child.stderr.on('data', collect);
    child.once('error', (error) => { failure = error; });
    child.once('close', (code, sig) => {
      clearTimeout(timer);
      signal?.removeEventListener('abort', onAbort);
      if (failure) reject(failure);
      else if (sig || code === null) reject(new Error('flock helper terminated'));
      else resolve({ code, output });
    });
    if (signal?.aborted) onAbort();
  });
}

function createRuntime(options = {}) {
  const io = options.io ?? fs;
  const root = options.root ?? '/data';
  if (!path.isAbsolute(root)) throw new Error('absolute data root required');
  const uid = options.uid ?? 10001;
  const gid = options.gid ?? 10001;
  const now = options.now ?? (() => performance.now());
  const sleep = options.sleep ?? ((ms, signal) => delay(ms, undefined, { signal }));
  const run = options.run ?? ((args, fd, signal, deadline) =>
    runHelper(args, fd, signal, deadline, options.spawnProcess ?? spawn));
  const names = {
    epoch: path.join(root, '.uzi-epoch.lock'),
    ledger: path.join(root, '.uzi-epoch-consumed'),
    record: path.join(root, '.uzi-pod-entry'),
  };
  let verified = false;
  function supported() {
    const stat = io.lstatSync(root);
    if (!stat.isDirectory() || stat.isSymbolicLink()) throw new Error('unsafe data root');
    const type = Number(io.statfsSync(root).type);
    return type === 0xef53 || type === 0x58465342;
  }
  function metadata(stat, mode, owner = uid) {
    if (!stat.isFile() || stat.uid !== BigInt(owner) || stat.gid !== BigInt(gid) ||
        (stat.mode & 0o7777n) !== BigInt(mode) || stat.nlink !== 1n) {
      throw new Error('unsafe epoch state metadata');
    }
  }
  function verify(fd, file, mode = 0o600, expected, owner = uid) {
    // Keep 64-bit device/inode identities exact, including inode64 XFS.
    const held = io.fstatSync(fd, { bigint: true });
    const named = io.lstatSync(file, { bigint: true });
    metadata(held, mode, owner);
    metadata(named, mode, owner);
    if (identity(held) !== identity(named) || (expected && identity(held) !== expected)) {
      throw new Error('epoch state inode changed');
    }
    return held;
  }
  function open(file, mode = 0o600, owner = uid) {
    const fd = io.openSync(file, fs.constants.O_RDWR | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
    try { verify(fd, file, mode, undefined, owner); return fd; }
    catch (error) { io.closeSync(fd); throw error; }
  }
  async function utility(signal, deadline) {
    if (verified) return;
    const result = await run(['--version'], undefined, signal, deadline);
    if (result.code !== 0 || !/^flock from util-linux [^\r\n]+\n?$/.test(result.output)) {
      throw new Error('trusted util-linux flock required');
    }
    verified = true;
  }
  async function lock(fd, mode, timeout, signal, once = false) {
    const deadline = now() + timeout;
    await utility(signal, deadline);
    // Contention alone retries, at most ceil(timeout/50)+1 attempts. Every other
    // failure aborts. No sibling work is launched by this loop.
    for (let attempts = 0; attempts <= Math.ceil(timeout / 50); attempts++) {
      aborted(signal);
      if (now() >= deadline) throw new Error('epoch lock timeout');
      const result = await run([mode === 'exclusive' ? '-x' : '-s', '-n', '-E', '200', '3'], fd, signal, deadline);
      if (result.code === 0) return true;
      if (result.code !== 200) throw new Error(`flock failed: ${result.code}`);
      if (once) return false;
      if (now() >= deadline) break;
      await sleep(Math.min(50, deadline - now()), signal);
    }
    throw new Error('epoch lock timeout');
  }
  function syncDir() {
    const fd = io.openSync(root, fs.constants.O_RDONLY | fs.constants.O_DIRECTORY | fs.constants.O_NOFOLLOW);
    try { io.fsyncSync(fd); } finally { io.closeSync(fd); }
  }
  async function initialize(file, contents, signal, mutex = false) {
    let fd;
    try {
      fd = io.openSync(file, fs.constants.O_RDWR | fs.constants.O_CREAT | fs.constants.O_EXCL |
        fs.constants.O_NOFOLLOW, 0o600);
    } catch (error) {
      if (error.code !== 'EEXIST') throw error;
      return false;
    }
    try {
      // Set mode while still the creator: CHOWN does not grant FOWNER.
      io.fchmodSync(fd, 0o600);
      io.fchownSync(fd, uid, gid);
      verify(fd, file);
      if (mutex) await lock(fd, 'exclusive', 5000, signal);
      if (contents && io.writeSync(fd, contents, 0, 'ascii') !== Buffer.byteLength(contents)) {
        throw new Error('short state initialization');
      }
      io.fsyncSync(fd);
      syncDir();
      return true;
    } finally { io.closeSync(fd); }
  }
  function readBounded(fd, file, limit, mode = 0o600, owner = uid) {
    const before = verify(fd, file, mode, undefined, owner);
    if (before.size > limit) throw new Error('epoch state exceeds bound');
    const bytes = Buffer.alloc(limit + 1);
    let length = 0;
    while (length < bytes.length) {
      const count = io.readSync(fd, bytes, length, bytes.length - length, length);
      if (!count) break;
      length += count;
    }
    const after = verify(fd, file, mode, identity(before), owner);
    if (length > limit || length !== Number(before.size) || after.size !== before.size) {
      throw new Error('epoch state changed or exceeds bound');
    }
    // UTF-8 replacement and non-ASCII bytes must not normalize into valid state.
    if (bytes.subarray(0, length).some((byte) => byte > 127)) throw new Error('non-ASCII epoch state');
    return bytes.toString('ascii', 0, length);
  }
  function ledger(fd) {
    const contents = readBounded(fd, names.ledger, MAX_BYTES);
    if (!contents.startsWith(HEADER)) throw new Error('invalid consumption header');
    const body = contents.slice(HEADER.length);
    if (body.length % 37 !== 0) throw new Error('torn consumption ledger');
    const records = body ? body.slice(0, -1).split('\n') : [];
    if ((body && !body.endsWith('\n')) || records.length > MAX_RECORDS) throw new Error('invalid consumption ledger');
    records.forEach(uuid);
    const seen = new Set(records);
    if (seen.size !== records.length) throw new Error('duplicate consumption UUID');
    return { seen, size: contents.length };
  }
  // Advisory only: never acquire the ledger mutex before epoch ownership.
  // A complete read uses at most MAX_BYTES + 1 bytes; consumption still validates
  // and fsyncs under the ledger mutex after acquire, even for an existing UID.
  function inspectConsumption(podUid) {
    uuid(podUid);
    try {
      const fd = io.openSync(names.ledger,
        fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
      try {
        const before = verify(fd, names.ledger);
        const { seen } = ledger(fd);
        const after = verify(fd, names.ledger, 0o600, identity(before));
        if (after.size !== before.size || after.mtimeNs !== before.mtimeNs ||
            after.ctimeNs !== before.ctimeNs) return 'uncertain';
        return seen.has(podUid) ? 'existing' : seen.size === MAX_RECORDS ? 'full' : 'fresh';
      } finally { io.closeSync(fd); }
    } catch { return 'uncertain'; }
  }
  async function consume(podUid, signal) {
    uuid(podUid);
    const fd = open(names.ledger);
    try {
      await lock(fd, 'exclusive', 5000, signal);
      verify(fd, names.ledger);
      const { seen, size } = ledger(fd);
      let result = 'existing';
      if (!seen.has(podUid)) {
        if (seen.size === MAX_RECORDS) result = 'full';
        else {
          const record = podUid + '\n';
          if (io.writeSync(fd, record, size, 'ascii') !== 37) throw new Error('short consumption append');
          result = 'fresh';
        }
      }
      // Existing and saturated paths also settle a predecessor's complete append.
      io.fsyncSync(fd);
      verify(fd, names.ledger);
      aborted(signal);
      return result;
    } finally { io.closeSync(fd); }
  }
  async function acquire({ firstMode = 'shared', signal } = {}) {
    if (firstMode !== 'shared' && firstMode !== 'exclusive') throw new Error('invalid epoch mode');
    const participates = supported(); // Before any epoch-file open or flock.
    let fd;
    let mode = 'unsupported';
    if (participates) {
      fd = open(names.epoch);
      try {
        mode = firstMode;
        const acquired = await lock(fd, mode, 120000, signal, mode === 'exclusive');
        if (!acquired) { mode = 'shared'; await lock(fd, mode, 120000, signal); }
        verify(fd, names.epoch);
      } catch (error) { io.closeSync(fd); throw error; }
    }
    let closed = false;
    let pending = false;
    const requireIdle = () => { requireLive(); if (pending) throw new Error('epoch operation pending'); };
    const requireLive = () => { if (closed) throw new Error('epoch ownership closed'); };
    return {
      get mode() { requireLive(); return mode; },
      async consume(podUid) {
        requireIdle();
        pending = true;
        try {
          const result = await consume(podUid, signal);
          return { result, mayEvaluate: result === 'fresh' && mode === 'exclusive' };
        } finally { pending = false; }
      },
      matchesEntry(current, previous) {
        requireIdle();
        uuid(current); uuid(previous);
        if (mode !== 'exclusive') return false;
        const record = readRecord();
        return !!record && record.current === current && record.previous === previous;
      },
      async reacquireShared() {
        requireIdle();
        if (mode !== 'exclusive') return;
        // Deliberately no conversion: admission must remain closed across this gap.
        io.closeSync(fd);
        fd = undefined;
        closed = true;
        const next = open(names.epoch);
        try { await lock(next, 'shared', 120000, signal); verify(next, names.epoch); }
        catch (error) { io.closeSync(next); throw error; }
        fd = next;
        mode = 'shared';
        closed = false;
      },
      close() {
        requireIdle();
        closed = true;
        if (fd !== undefined) io.closeSync(fd);
      },
    };
  }
  function readRecord() {
    let fd;
    try { fd = open(names.record, 0o644); }
    catch (error) { if (error.code === 'ENOENT') return undefined; throw error; }
    try {
      // Malformed corroboration refuses authority, but does not disable SH
      // participation or repair history. Unsafe inode metadata remains fatal.
      if (verify(fd, names.record, 0o644).size > 128) return null;
      const contents = readBounded(fd, names.record, 128, 0o644);
      let value;
      try { value = JSON.parse(contents); } catch { return null; }
      if ((contents.match(/"(?:current|previous)"\s*:/g) ?? []).length !== 2) {
        return null;
      }
      if (!value || Array.isArray(value) || Object.keys(value).sort().join(',') !== 'current,previous') {
        return null;
      }
      if (typeof value.current !== 'string' || !UUID.test(value.current) ||
          (value.previous !== null && (typeof value.previous !== 'string' || !UUID.test(value.previous)))) return null;
      return value;
    } finally { io.closeSync(fd); }
  }
  function writeRecord(podUid) {
    const old = readRecord();
    if (old === null) return false;
    if (old?.current === podUid) {
      const fd = open(names.record, 0o644);
      try { io.fsyncSync(fd); verify(fd, names.record, 0o644); }
      finally { io.closeSync(fd); }
      syncDir();
      return;
    }
    const temporary = path.join(root, '.uzi-pod-entry.' + randomUUID());
    let fd;
    let renamed = false;
    let failure;
    try {
      fd = io.openSync(temporary, fs.constants.O_WRONLY | fs.constants.O_CREAT |
        fs.constants.O_EXCL | fs.constants.O_NOFOLLOW, 0o600);
      // Hand off ownership only after the creator has set the final mode.
      io.fchmodSync(fd, 0o644);
      io.fchownSync(fd, uid, gid);
      const contents = JSON.stringify({ current: podUid, previous: old?.current ?? null }) + '\n';
      if (io.writeSync(fd, contents, 0, 'ascii') !== contents.length) throw new Error('short pod entry write');
      io.fsyncSync(fd);
      // Revalidate the old inode/metadata before replacing: never repair unsafe state.
      const current = readRecord();
      if (JSON.stringify(current) !== JSON.stringify(old)) throw new Error('pod entry changed');
      io.renameSync(temporary, names.record);
      renamed = true;
      syncDir();
    } catch (error) { failure = error; }
    if (fd !== undefined) {
      try { io.closeSync(fd); } catch (error) { failure ??= error; }
    }
    if (!renamed) {
      try { io.unlinkSync(temporary); }
      catch (error) { if (error.code !== 'ENOENT') failure ??= error; }
    }
    if (failure) throw failure;
  }
  async function validateLedger(signal) {
    const fd = open(names.ledger);
    try {
      await lock(fd, 'exclusive', 5000, signal);
      ledger(fd);
      io.fsyncSync(fd);
      verify(fd, names.ledger);
    } finally { io.closeSync(fd); }
  }
  async function seed(podUid, signal) {
    uuid(podUid);
    if (!supported()) {
      // This mutex protects metadata only; it supplies NO epoch authority.
      await initialize(names.ledger, HEADER, signal, true);
      const fd = open(names.ledger);
      try {
        await lock(fd, 'exclusive', 5000, signal);
        ledger(fd);
        writeRecord(podUid);
        io.fsyncSync(fd);
        verify(fd, names.ledger);
      } finally { io.closeSync(fd); }
      return 'unsupported';
    }
    await initialize(names.epoch, '', signal);
    const fd = open(names.epoch);
    try {
      if (!await lock(fd, 'exclusive', 120000, signal, true)) {
        readRecord(); // Reject unsafe metadata without rewriting overlapping state.
        return 'busy';
      }
      verify(fd, names.epoch);
      await initialize(names.ledger, HEADER, signal, true);
      await validateLedger(signal);
      return writeRecord(podUid) === false ? 'invalid' : 'updated';
    } finally { io.closeSync(fd); }
  }
  function prepare() {
    if (!supported()) return 'unsupported';
    const fd = open(names.epoch);
    try { return identity(verify(fd, names.epoch)); }
    finally { io.closeSync(fd); }
  }
  async function hold(expected, fd = 9, signal) {
    if (!supported()) {
      if (expected !== 'unsupported') throw new Error('storage participation changed');
      await validateLedger(signal);
      return;
    }
    if (!/^\d+:\d+$/.test(expected)) throw new Error('invalid epoch identity');
    verify(fd, names.epoch, 0o600, expected);
    await lock(fd, 'shared', 120000, signal);
    verify(fd, names.epoch, 0o600, expected);
    await validateLedger(signal);
  }
  return { acquire, seed, prepare, hold, inspectConsumption };
}

module.exports = { createRuntime };

if (require.main === module) {
  const [command, root, argument, fd] = process.argv.slice(2);
  const controller = new AbortController();
  const onSignal = () => controller.abort(new Error('initializer interrupted'));
  process.once('SIGTERM', onSignal);
  process.once('SIGINT', onSignal);
  (async () => {
    if (!root || !path.isAbsolute(root)) throw new Error('absolute data root required');
    const runtime = createRuntime({ root });
    if (command === 'prepare') console.log(runtime.prepare());
    else if (command === 'hold') await runtime.hold(argument, fd === undefined ? 9 : Number(fd), controller.signal);
    else if (command === 'seed') console.log(await runtime.seed(argument, controller.signal));
    else throw new Error('expected prepare, hold, or seed');
  })().catch((error) => { console.error('pod-epoch-init: ' + error.message); process.exitCode = 1; });
}
