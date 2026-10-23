import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtempSync, writeFileSync, mkdirSync, rmSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const root = fileURLToPath(new URL('../', import.meta.url));
const checker = fileURLToPath(new URL('./check-uxlab-freeze.mjs', import.meta.url));
mkdirSync(root + '.uzi/scratch', { recursive: true });

test('freeze manifest parity rejects unsafe inputs and accepts JSONC strings', () => {
  const dir = mkdtempSync(root + '.uzi/scratch/freeze-check.');
  const a = dir + '/root.json', b = dir + '/nested.json';
  const manifest = packages => JSON.stringify({ packages });
  const valid = manifest(['charm-freeze@1.2.3']);
  let cases = 0;
  function check(left, right, expected, diagnostic) {
    writeFileSync(a, left);
    writeFileSync(b, right);
    const result = spawnSync(process.execPath, [checker, a, b], { encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
    assert.equal(result.status, expected, 'fixture exit status: ' + result.stderr.slice(0, 300));
    if (diagnostic) assert.match(result.stderr, diagnostic);
    assert.ok(result.stderr.length < 1000, 'bounded diagnostics');
    cases++;
  }
  try {
    check(valid, valid, 0);
    check(manifest(['charm-freeze@1.2.3', 'charm-freeze-extra@1.2.3']), valid, 0);
    check(valid, manifest(['charm-freeze-extra@1.2.3', 'charm-freeze@1.2.3']), 0);
    // No third fixed version: any matching explicit pins work.
    const strings = JSON.stringify({ url: 'https://example.test/a/*b*/', quote: '"//', packages: ['charm-freeze@1.2.3'] });
    check('/* header */' + strings + '// tail\n', valid, 0);
    check(manifest(['ruby@4.0.6']), valid, 1, /root: missing charm-freeze/);
    check(valid, manifest(['charm-freeze@1.2.4']), 1, /mismatch/);
    for (const packages of [
      [], ['charm-freeze'], ['charm-freeze@'], ['charm-freezer@1.2.3'],
      ['charm-freeze@latest'], ['charm-freeze@1.2'], ['charm-freeze@1.2.3@4'],
      ['charm-freeze@1.2.3', 'charm-freeze@1.2.3'],
      { 'charm-freeze': '1.2.3' }, [42], [null],
    ]) {
      check(manifest(packages), valid, 1);
      check(valid, manifest(packages), 1);
    }
    for (const malformed of [
      '{}', '{', 'null', '{"packages":[]} // charm-freeze@1.2.3',
      '{"packages":[]} /* charm-freeze@1.2.3 */', valid + '/* unterminated',
      '{"packages":["charm-freeze@1.2.3"],}', Buffer.from([0xff]),
      ' '.repeat(1024 * 1024 + 1), ' '.repeat(1024 * 1024) + valid,
    ]) check(malformed, valid, 1);
    // Observe actual bytes read from an oversized manifest through the public CLI.
    writeFileSync(a, ' '.repeat(2 * 1024 * 1024));
    const observeReads = `
      import fs from 'node:fs';
      import { syncBuiltinESMExports } from 'node:module';
      import { pathToFileURL } from 'node:url';
      const [checker, left, right, mode] = process.argv.slice(1);
      let grow = mode === 'grow';
      const open = fs.openSync, read = fs.readSync;
      const watched = new Set();
      let bytes = 0;
      fs.openSync = (file, ...args) => {
        const fd = open(file, ...args);
        if (file === left) watched.add(fd);
        return fd;
      };
      fs.readSync = (fd, ...args) => {
        const count = read(fd, ...args);
        if (watched.has(fd)) {
          bytes += count;
          if (grow && count > 0) {
            grow = false;
            fs.appendFileSync(left, ' ');
          }
        }
        return count;
      };
      syncBuiltinESMExports();
      process.argv = [process.execPath, checker, left, right];
      await import(pathToFileURL(checker).href);
      console.log('MANIFEST_READ_BYTES=' + bytes);
    `;
    const bounded = spawnSync(process.execPath, ['--input-type=module', '-e', observeReads, checker, a, b],
      { encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
    assert.equal(bounded.status, 1, 'oversized manifest must fail');
    const bytesRead = /^MANIFEST_READ_BYTES=([0-9]+)$/m.exec(bounded.stdout);
    assert.ok(bytesRead, 'read-count observation completed');
    assert.ok(Number(bytesRead[1]) <= 1024 * 1024, 'actual manifest reads stay within 1 MiB');

    // Grow only after the first read, so this covers the read loop rather than
    // the early size rejection. A cap-plus-one buffer would read the new byte.
    writeFileSync(a, valid + ' '.repeat(1024 * 1024 - Buffer.byteLength(valid)));
    const growth = spawnSync(process.execPath, ['--input-type=module', '-e', observeReads, checker, a, b, 'grow'],
      { encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
    assert.equal(growth.status, 1, 'growth past the cap must fail');
    const growthReads = /^MANIFEST_READ_BYTES=([0-9]+)$/m.exec(growth.stdout);
    assert.ok(growthReads, 'growth read-count observation completed');
    assert.ok(Number(growthReads[1]) > 0, 'growth probe exercises the read loop');
    assert.ok(Number(growthReads[1]) <= 1024 * 1024, 'growth never causes an overflow-byte read');

    rmSync(a);
    let result = spawnSync(process.execPath, [checker, a, b], { encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
    assert.notEqual(result.status, 0, 'missing manifest');
    mkdirSync(a);
    result = spawnSync(process.execPath, [checker, a, b], { encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
    assert.notEqual(result.status, 0, 'nonregular manifest');
    // The byte cap itself is inclusive.
    rmSync(a, { recursive: true });
    check(valid + ' '.repeat(1024 * 1024 - Buffer.byteLength(valid)), valid, 0);
    check(valid + ' '.repeat(1024 * 1024 - Buffer.byteLength(valid) - 1) + 'é', valid, 1);
    console.log('manifest fixture cases=' + cases + ' plus missing/nonregular');
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test('default repository manifests match', () => {
  const result = spawnSync(process.execPath, [checker], { cwd: root + '.uzi/scratch', encoding: 'utf8', timeout: 5000, maxBuffer: 64 * 1024 });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /pins match/);
});
