#!/usr/bin/env bash
# Hermetic build contract: real gen/render scripts, controlled go and freeze.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"
node --input-type=module <<'NODE'
import assert from 'node:assert/strict';
import { copyFileSync, chmodSync, mkdirSync, mkdtempSync, writeFileSync, readFileSync, readdirSync, statSync, rmSync, utimesSync } from 'node:fs';
import { spawnSync } from 'node:child_process';

const root = process.cwd();
const source = root + '/api/cmd/uzi/uxlab';
const manifest = JSON.parse(readFileSync(source + '/devbox.json', 'utf8'));
assert.deepEqual(manifest.shell.scripts.build, ['./build.sh']);
assert.deepEqual(manifest.shell.scripts.gen, ['./gen.sh']);
assert.deepEqual(manifest.shell.scripts.render, ['./render.sh']);
mkdirSync(root + '/.uzi/scratch', { recursive: true });
const tmp = mkdtempSync(root + '/.uzi/scratch/uxlab-build.');
const lab = tmp + '/api/cmd/uzi/uxlab', bin = tmp + '/bin';
mkdirSync(lab + '/frames', { recursive: true });
mkdirSync(bin);
for (const name of ['build.sh', 'gen.sh', 'render.sh']) {
  copyFileSync(source + '/' + name, lab + '/' + name);
  chmodSync(lab + '/' + name, 0o755);
}
function executable(name, text) {
  writeFileSync(bin + '/' + name, text);
  chmodSync(bin + '/' + name, 0o755);
}
executable('go', `#!/bin/bash
set -eu
echo gen >> "$LAB/order"
if [[ "$MODE" == genfail ]]; then printf partial > "$LAB/frames/partial.ansi"; exit 17; fi
if [[ "$MODE" == noframes ]]; then exit 0; fi
mkdir -p "$LAB/frames"
printf frame > "$LAB/frames/a-dark.ansi"
printf frame > "$LAB/frames/b-light.ansi"
touch -t 202001010000 "$LAB/frames/"*.ansi
`);
executable('freeze', `#!/bin/bash
set -eu
echo render >> "$LAB/order"
previous=
frame=
out=
for arg in "$@"; do
  if [[ "$previous" == -o ]]; then out="$arg"; fi
  if [[ "$arg" == *.ansi ]]; then frame="$arg"; fi
  previous="$arg"
done
if [[ "$MODE" == allmissing || ( "$MODE" == missing && "$frame" == *b-light.ansi ) ]]; then exit 0; fi
printf png > "$out"
case "$MODE" in
  stale) touch -t 201901010000 "$out" ;;
  equal) touch -r "$frame" "$out" ;;
  empty) : > "$out" ;;
  partial) exit 23 ;;
esac
`);
function run(mode, missing = false) {
  return spawnSync('/bin/bash', [lab + '/build.sh'], {
    cwd: tmp, encoding: 'utf8', timeout: 10000, maxBuffer: 64 * 1024,
    env: { ...process.env, LAB: lab, MODE: mode, PATH: missing ? tmp + '/no-tools' : bin + ':' + process.env.PATH },
  });
}
function inventory() {
  return readdirSync(lab + '/frames').sort().map(name => {
    const file = lab + '/frames/' + name;
    return [name, readFileSync(file).toString('base64'), statSync(file, { bigint: true }).mtimeNs.toString()];
  });
}
try {
  writeFileSync(lab + '/frames/old.ansi', 'existing frame');
  utimesSync(lab + '/frames/old.ansi', 1700000000.123456, 1700000000.123456);
  const before = inventory();
  const missing = run('success', true);
  assert.equal(missing.status, 1);
  assert.match(missing.stderr, /freeze.*devbox opt-in.*repo in uzi.*provisioning.*no package warnings.*devbox run build locally/);
  assert.deepEqual(inventory(), before, 'missing freeze must preserve frame names, bytes and nanosecond mtimes');
  assert.deepEqual(readdirSync(lab).sort(), ['build.sh', 'frames', 'gen.sh', 'render.sh'], 'no output mutations before preflight');
  let cases = 1;
  for (const [mode, status] of [
    ['noframes', 1], ['allmissing', 1], ['missing', 1], ['stale', 1], ['equal', 1], ['empty', 1],
    ['genfail', 17], ['partial', 23], ['success', 0],
  ]) {
    rmSync(lab + '/frames', { recursive: true, force: true });
    rmSync(lab + '/png', { recursive: true, force: true });
    rmSync(lab + '/order', { force: true });
    mkdirSync(lab + '/frames');
    const result = run(mode);
    assert.equal(result.status, status, mode + ': ' + result.stderr.slice(0, 300));
    const order = readFileSync(lab + '/order', 'utf8').trim().split('\n');
    assert.equal(order[0], 'gen', mode + ': generate first');
    if (mode === 'genfail' || mode === 'noframes') assert.deepEqual(order, ['gen']);
    else assert.equal(order[1], 'render');
    if (mode === 'success') {
      assert.match(result.stdout, /verified 2 fresh PNG/);
      assert.ok(statSync(lab + '/png/b-light.png').size > 0);
    }
    if (mode === 'partial') assert.ok(statSync(lab + '/png/a-dark.png').size > 0);
    cases++;
  }
  // A generator that writes nothing must not pass on frames left by an earlier build.
  rmSync(lab + '/png', { recursive: true, force: true });
  rmSync(lab + '/order', { force: true });
  writeFileSync(lab + '/frames/old.ansi', 'stale frame');
  const stale = run('noframes');
  assert.equal(stale.status, 1, 'stale frames with no new generation: ' + stale.stderr.slice(0, 300));
  assert.match(stale.stderr, /no ANSI frames generated/);
  cases++;
  console.log('uxlab build cases=' + cases + ' passed=' + cases);
} finally {
  rmSync(tmp, { recursive: true, force: true });
}
NODE
