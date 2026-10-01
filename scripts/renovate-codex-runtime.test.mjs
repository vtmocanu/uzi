#!/usr/bin/env node
// Extraction and partial-update regression for the native Codex runtime pin.
import assert from 'node:assert/strict';
import { readFileSync, mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath, pathToFileURL } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const packageFile = 'agent/codex/codex-package.lock';
const lock = readFileSync(path.join(root, packageFile), 'utf8');
const config = JSON.parse(readFileSync(path.join(root, 'renovate.json'), 'utf8'));
const managers = config.customManagers.filter(m => m.packageNameTemplate === 'openai/codex');
assert.equal(managers.length, 2, 'Codex identity and archive managers must be present');
const deps = managers.flatMap(manager => manager.matchStrings.flatMap(pattern =>
  [...lock.matchAll(new RegExp(pattern, 'g'))].map(match => ({
    manager, match, ...match.groups,
    depName: manager.depNameTemplate.replace('{{{arch}}}', match.groups.arch ?? ''),
  }))));
assert.deepEqual(deps.map(d => d.depName).sort(), ['codex-package-amd64', 'codex-package-arm64', 'codex-source']);
assert.equal(new Set(deps.map(d => d.currentValue)).size, 1, 'all three pins must share a release');
assert.notEqual(deps[1].currentDigest, deps[2].currentDigest, 'archives need distinct digests');
for (const [index, dep] of deps.entries()) {
  assert.deepEqual(dep.manager.managerFilePatterns, ['/^agent/codex/codex-package\\.lock$/']);
  const version = new RegExp(dep.manager.versioningTemplate.slice('regex:'.length));
  assert(version.test(dep.currentValue));
  assert(!version.test('rust-v0.160.0-alpha.6.2'), 'prereleases must not be eligible');
  assert(!version.test('v0.159.3'), 'only native rust-v releases are eligible');
  assert.equal(dep.manager.datasourceTemplate, dep.depName === 'codex-source' ? 'github-releases' : 'github-release-attachments');
  for (const other of deps.slice(index + 1)) {
    assert(dep.match.index + dep.match[0].length <= other.match.index || other.match.index + other.match[0].length <= dep.match.index,
      `${dep.depName} overlaps ${other.depName}: a sequential replacement can silently skip a pin`);
  }
}
const rule = config.packageRules.find(r => r.matchPackageNames?.includes('openai/codex'));
assert(rule, 'all pins need one group');
assert.equal(rule.groupName, 'Codex runtime');
assert.equal(rule.separateMajorMinor, false);
assert.equal(rule.automerge, false);
assert.equal(rule.dependencyDashboardApproval ?? false, false);
assert.equal(config.minimumReleaseAge, '7 days');
assert.equal(config.internalChecksFilter, 'strict');

const nextTag = 'rust-v99.1.2';
const nextDigests = { 'codex-source': 'a'.repeat(40), 'codex-package-amd64': 'b'.repeat(64), 'codex-package-arm64': 'c'.repeat(64) };
const permutations = values => values.length ? values.flatMap((value, index) =>
  permutations(values.filter((_, i) => i !== index)).map(rest => [value, ...rest])) : [[]];
// Optional maintainer check runs the exact Renovate extractor, template engine and
// replacement validation. CI's dependency-free contract below also checks every order.
const dist = process.argv[2];
let actual;
const scratch = mkdtempSync(path.join(os.tmpdir(), 'codex-renovate-test-'));
try {
  if (dist) {
    const load = relative => import(pathToFileURL(path.join(dist, relative)).href);
    const [{ regex_exports }, { doAutoReplace }, { GlobalConfig }] = await Promise.all([
      load('modules/manager/custom/regex/index.js'),
      load('workers/repository/update/branch/auto-replace.js'),
      load('config/global.js'),
    ]);
    GlobalConfig.set({ localDir: scratch });
    actual = { extract: regex_exports.extractPackageFile, replace: doAutoReplace };
  }
  for (const order of permutations(deps)) {
    let updated = lock;
    for (const dep of order) {
      const newDigest = nextDigests[dep.depName];
      if (actual) {
        const extracted = actual.extract(lock, packageFile, dep.manager);
        const depIndex = extracted.deps.findIndex(d => d.depName === dep.depName);
        assert(depIndex >= 0);
        updated = await actual.replace({ ...dep.manager, ...extracted, ...extracted.deps[depIndex],
          manager: 'regex', packageFile, depIndex, newValue: nextTag, newDigest,
          autoReplaceGlobalMatch: true }, updated, false);
      } else {
        // Pin the only custom template, then exercise its concrete output. Archive
        // pairs use Renovate's standard literal version/digest replacements.
        let replacement;
        if (dep.depName === 'codex-source') {
          assert.equal(dep.manager.autoReplaceStringTemplate,
            "CODEX_VERSION={{{replace '^rust-v' '' newValue}}}\nCODEX_MANIFEST_VERSION={{{replace '^rust-v' '' newValue}}}\nCODEX_TAG={{{newValue}}}\nCODEX_SOURCE_COMMIT={{{newDigest}}}");
          replacement = `CODEX_VERSION=99.1.2\nCODEX_MANIFEST_VERSION=99.1.2\nCODEX_TAG=${nextTag}\nCODEX_SOURCE_COMMIT=${newDigest}`;
        } else replacement = dep.match[0].replace(dep.currentValue, nextTag).replace(dep.currentDigest, newDigest);
        assert(updated.includes(dep.match[0]), 'each original region must survive previous replacements');
        updated = updated.replace(dep.match[0], replacement);
      }
    }
    for (const key of ['CODEX_TAG', 'CODEX_TAG_amd64', 'CODEX_TAG_arm64']) assert(updated.includes(`${key}=${nextTag}\n`));
    for (const key of ['CODEX_VERSION', 'CODEX_MANIFEST_VERSION']) assert(updated.includes(`${key}=99.1.2\n`));
    assert(updated.includes(`CODEX_SOURCE_COMMIT=${nextDigests['codex-source']}`));
    for (const arch of ['amd64', 'arm64']) assert(updated.includes(`CODEX_SHA256_${arch}=${nextDigests[`codex-package-${arch}`]}`));
  }
  // Run the real installer with no artifact: each malformed lock must fail BEFORE
  // any download or installation, on both architectures. No Docker or network.
  for (const key of ['CODEX_VERSION', 'CODEX_MANIFEST_VERSION', 'CODEX_TAG_amd64', 'CODEX_TAG_arm64']) {
    const bad = lock.replace(new RegExp(`^${key}=.+$`, 'm'), `${key}=${key.startsWith('CODEX_TAG') ? nextTag : '99.1.2'}`);
    const badLock = path.join(scratch, 'bad.lock');
    writeFileSync(badLock, bad);
    for (const arch of ['amd64', 'arm64']) {
      const result = spawnSync('bash', [path.join(root, 'agent/codex/install-codex.sh'), arch], {
        encoding: 'utf8', timeout: 5000,
        env: { ...process.env, TARGETARCH: arch, UZI_CODEX_LOCK: badLock,
          UZI_CODEX_PREFIX: path.join(scratch, 'install'), UZI_CODEX_ARTIFACT: path.join(scratch, 'absent.tar.gz') },
      });
      assert.equal(result.error, undefined);
      assert.equal(result.status, 1);
      assert.match(result.stderr, /disagrees/, `${key} must be rejected before artifact access`);
      assert(!result.stderr.includes('downloading'));
    }
  }
} finally { rmSync(scratch, { recursive: true, force: true }); }
console.log(`Codex Renovate contract: PASS (six update orders${actual ? ', real Renovate' : ''}, eight partial-pin rejections)`);
