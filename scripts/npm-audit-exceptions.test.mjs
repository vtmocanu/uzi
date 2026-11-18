import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { test } from 'node:test';
import { evaluate, parseRows } from './npm-audit-exceptions.mjs';
const today = '2026-11-17', ghsa = 'GHSA-fc5q-39xc-vjpf';
const header = 'component\tghsa\tpackage\trange\tmax_severity\trecheck_by\tissue';
const policy = header + '\nweb\t' + ghsa + '\tpicomatch\t<=4.0.7\thigh\t2026-12-01\t#4435';
const rows = () => parseRows(policy, today);
const root = () => ({ source: 1247710, name: 'picomatch', severity: 'high', range: '<=4.0.7',
  url: 'https://github.com/advisories/' + ghsa });
const audit = () => ({ auditReportVersion: 2, vulnerabilities: { picomatch: {
  name: 'picomatch', severity: 'high', via: [root()], nodes: ['node_modules/picomatch'],
} } });
const manifest = () => ({ devDependencies: { knip: '6.42.1' } });
const lock = () => ({ lockfileVersion: 3, packages: {
  '': { devDependencies: { knip: '6.42.1' } },
  'node_modules/picomatch': { version: '4.0.7', dev: true },
} });
function run(change = () => {}, options = {}) {
  const input = { rows: rows(), component: 'web', level: 'high', today,
    auditJson: audit(), lockJson: lock(), manifestJson: manifest(), ...options };
  change(input);
  return evaluate(input);
}
test('real web and agent reports pass, including the rooted Vitest cycle', () => {
  for (const component of ['web', 'agent']) {
    const auditJson = JSON.parse(readFileSync(new URL('./fixtures/npm-audit/' + component + '.json', import.meta.url)));
    const lockJson = JSON.parse(readFileSync(new URL('./fixtures/npm-audit/' + component + '-lock.json', import.meta.url)));
    const manifestJson = JSON.parse(readFileSync(new URL('./fixtures/npm-audit/' + component + '-package.json', import.meta.url)));
    const result = evaluate({ rows: rows().map(row => ({ ...row, component })), component,
      level: 'high', today, auditJson, lockJson, manifestJson });
    assert.equal(result.code, 0);
    assert.match(result.messages[0], /EXCEPTED/);
  }
});
test('unexcepted and mixed roots fail; a moderate root alongside high is ignored', () => {
  for (const [mixed, level, expected] of [[false, 'high', 1], [true, 'high', 1], [true, 'moderate', 0]]) {
    assert.equal(run(({ auditJson }) => {
      const other = { ...root(), source: 2, severity: level, url: 'https://github.com/advisories/GHSA-abcd-efgh-jklm' };
      auditJson.vulnerabilities.picomatch.via = mixed ? [root(), other] : [other];
    }).code, expected);
  }
});
test('ordinary, reported aliased, name-aliased and URL-aliased production nodes re-arm', () => {
  for (const form of ['ordinary', 'reported', 'name', 'url', 'nested']) {
    const result = run(({ auditJson, lockJson }) => {
      if (form === 'ordinary') delete lockJson.packages['node_modules/picomatch'].dev;
      else {
        const node = form === 'nested' ? 'node_modules/parent/node_modules/picomatch' : 'node_modules/alias';
        lockJson.packages[node] = form === 'name' ? { name: 'picomatch' } :
          form === 'url' ? { resolved: 'https://registry.npmjs.org/picomatch/-/picomatch-4.0.7.tgz' } : {};
        if (form === 'reported') auditJson.vulnerabilities.picomatch.nodes.push(node);
      }
    });
    assert.equal(result.code, 1, form);
    assert.match(result.messages.join('\n'), /production occurrence/);
  }
});
test('empty, invalid and missing reported lock nodes fail as broken evidence', () => {
  for (const nodes of [[], ['node_modules/missing'], [null]]) {
    assert.equal(run(({ auditJson }) => { auditJson.vulnerabilities.picomatch.nodes = nodes; }).code, 2);
  }
});
test('unsupported audit/error and lock schemas cannot pass', () => {
  for (const auditJson of [{ error: { code: 'ENOTFOUND' } }, { auditReportVersion: 1, vulnerabilities: {} },
    { auditReportVersion: 2, vulnerabilities: [] }, { ...audit(), error: { code: 'EFAIL' } }]) {
    assert.throws(() => run(() => {}, { auditJson }), /unsupported audit/);
  }
  for (const lockJson of [{}, { lockfileVersion: 2, packages: {} }, { lockfileVersion: 3, packages: [] }]) {
    assert.throws(() => run(() => {}, { lockJson }), /unsupported lockfile/);
  }
});
test('rootless cycles, dangling references and disconnected rootless branches fail', () => {
  for (const via of ['picomatch', 'missing']) {
    assert.equal(run(({ auditJson }) => { auditJson.vulnerabilities.picomatch.via = [via]; }).code, 2);
  }
  assert.equal(run(({ auditJson }) => {
    auditJson.vulnerabilities.broken = { name: 'broken', severity: 'high', via: ['broken'] };
    auditJson.vulnerabilities.picomatch.via.push('broken');
  }).code, 2);
});
test('unknown severity and missing root identities fail as instrument errors', () => {
  for (const field of ['record', 'severity', 'url', 'source', 'range']) {
    assert.equal(run(({ auditJson }) => {
      const record = auditJson.vulnerabilities.picomatch;
      if (field === 'record') record.severity = 'surprise';
      else if (field === 'severity') record.via[0].severity = 'surprise';
      else delete record.via[0][field];
    }).code, 2, field);
  }
});
test('critical escalation at either level and advisory range changes re-arm', () => {
  for (const target of ['root', 'record', 'range']) {
    assert.equal(run(({ auditJson }) => {
      const record = auditJson.vulnerabilities.picomatch;
      if (target === 'root') record.via[0].severity = 'critical';
      else if (target === 'record') record.severity = 'critical';
      else record.via[0].range = '<=4.0.8';
    }).code, 1);
  }
});
test('UTC expiry is inclusive, unused rows fail on clean and downgrade snapshots', () => {
  assert.equal(run(() => {}, { today: '2026-12-01' }).code, 0);
  assert.equal(run(() => {}, { today: '2026-12-02' }).code, 1);
  assert.equal(run(({ auditJson }) => { auditJson.vulnerabilities = {}; }).code, 1);
  assert.equal(run(({ auditJson }) => {
    auditJson.vulnerabilities.picomatch.severity = 'moderate';
    auditJson.vulnerabilities.picomatch.via[0].severity = 'moderate';
  }).code, 1);
});
test('real unexcepted findings take precedence over broken evidence elsewhere', () => {
  assert.equal(run(({ auditJson }) => {
    auditJson.vulnerabilities.other = { name: 'other', severity: 'high',
      via: [{ ...root(), name: 'other', source: 3, url: 'https://github.com/advisories/GHSA-abcd-efgh-jklm' }] };
    auditJson.vulnerabilities.picomatch.via = ['missing'];
  }).code, 1);
});
test('policy rejects duplicates, malformed dates/identities, unknown levels and horizon', () => {
  const line = policy.split('\n')[1];
  for (const invalid of [policy + '\n' + line, policy.replace('2026-12-01', '2026-02-30'),
    policy.replace('2026-12-01', '2027-01-01'), policy.replace('web\t', 'other\t'),
    policy.replace('\thigh\t', '\tunknown\t'), policy.replace('<=4.0.7', ''),
    policy.replace('#4435', '#N'), policy.replace('GHSA-fc5q', 'NOPE-fc5q'),
    policy.replace('component\t', 'directory\t')]) assert.throws(() => parseRows(invalid, today));
  assert.equal(parseRows(header, today).length, 0);
});
test('suppressed dependent records also need nonempty dev-only lock evidence', () => {
  for (const dev of [true, false]) {
    assert.equal(run(({ auditJson, lockJson }) => {
      auditJson.vulnerabilities.tool = { name: 'tool', severity: 'high', via: ['picomatch'], nodes: ['node_modules/tool'] };
      lockJson.packages['node_modules/tool'] = { dev };
    }).code, dev ? 0 : 1);
  }
  assert.equal(run(({ auditJson }) => {
    auditJson.vulnerabilities.tool = { name: 'tool', severity: 'high', via: ['picomatch'], nodes: [] };
  }).code, 2);
});
test('reported aliases must agree with package identity in the lockfile', () => {
  assert.equal(run(({ auditJson, lockJson }) => {
    auditJson.vulnerabilities.picomatch.nodes = ['node_modules/other'];
    lockJson.packages['node_modules/other'] = { dev: true, name: 'other' };
  }).code, 2);
});
test('manifest reclassification and a missing lock root cannot borrow stale dev flags', () => {
  assert.throws(() => run(({ manifestJson }) => {
    manifestJson.dependencies = { knip: manifestJson.devDependencies.knip };
    delete manifestJson.devDependencies;
  }), /manifest and lockfile disagree; regenerate the lockfile/);
  assert.throws(() => run(({ lockJson }) => {
    delete lockJson.packages[''];
  }), /manifest and lockfile disagree; regenerate the lockfile/);
});
test('all four root dependency maps require identical keys and string specs', () => {
  for (const field of ['dependencies', 'devDependencies', 'optionalDependencies', 'peerDependencies']) {
    for (const change of ['added', 'spec', 'shape']) {
      assert.throws(() => run(({ manifestJson, lockJson }) => {
        manifestJson[field] = { example: '1.0.0' };
        lockJson.packages[''][field] = { example: '1.0.0' };
        if (change === 'added') manifestJson[field].extra = '2.0.0';
        else if (change === 'spec') manifestJson[field].example = '^1.0.0';
        else manifestJson[field] = [];
      }), /manifest and lockfile disagree; regenerate the lockfile/);
    }
  }
});
test('absent dependency maps equal empty maps and property order is irrelevant', () => {
  assert.equal(run(({ manifestJson, lockJson }) => {
    manifestJson.dependencies = { first: '1', second: '2' };
    lockJson.packages[''].dependencies = { second: '2', first: '1' };
    lockJson.packages[''].peerDependencies = {};
  }).code, 0);
});
test('components without rows do not acquire a manifest-root consistency requirement', () => {
  assert.equal(run(({ auditJson, lockJson }) => {
    auditJson.vulnerabilities = {};
    delete lockJson.packages[''];
  }, { rows: [], manifestJson: undefined }).code, 0);
});
