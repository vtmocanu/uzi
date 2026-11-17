#!/usr/bin/env node
// Policy decisions are pure; only this entry point reads the fixed policy and UTC clock.
import { readFileSync, realpathSync } from 'node:fs';
import { dirname, isAbsolute, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const severities = ['info', 'low', 'moderate', 'high', 'critical'];
const columns = ['component', 'ghsa', 'package', 'range', 'max_severity', 'recheck_by', 'issue'];
const packageName = /^(?:@[a-z0-9][a-z0-9._-]*\/)?[a-z0-9][a-z0-9._-]*$/;
const ghsaPattern = /^GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}$/;
const object = value => value !== null && typeof value === 'object' && !Array.isArray(value);
const safeString = value => typeof value === 'string' && value.length > 0 && !/[\u0000-\u001f\u007f]/.test(value);
const severity = value => severities.indexOf(value);

function day(value) {
  if (typeof value !== 'string' || !/^\d{4}-\d{2}-\d{2}$/.test(value)) throw new Error('invalid UTC date');
  const parsed = new Date(value + 'T00:00:00.000Z');
  if (!Number.isFinite(parsed.getTime()) || parsed.toISOString().slice(0, 10) !== value) throw new Error('invalid UTC date');
  return parsed.getTime() / 86400000;
}

export function parseRows(text, today) {
  const now = day(today);
  const lines = text.split(/\r?\n/).filter(line => line.trim() && !line.startsWith('#'));
  if (lines.shift() !== columns.join('\t')) throw new Error('invalid exceptions header');
  const seen = new Set();
  return lines.map(line => {
    const fields = line.split('\t');
    if (fields.length !== columns.length) throw new Error('invalid exceptions column count');
    const row = Object.fromEntries(columns.map((key, index) => [key, fields[index]]));
    if (!['web', 'agent'].includes(row.component) || !ghsaPattern.test(row.ghsa) ||
        !packageName.test(row.package) || !safeString(row.range) || row.range.trim() !== row.range ||
        severity(row.max_severity) < 0 || !/^#[1-9]\d*$/.test(row.issue)) throw new Error('invalid exception row');
    if (day(row.recheck_by) - now > 30) throw new Error('exception recheck exceeds 30-day horizon');
    const key = [row.component, row.ghsa, row.package].join('\t');
    if (seen.has(key)) throw new Error('duplicate or conflicting exception');
    seen.add(key);
    return row;
  });
}

function rootAdvisory(value) {
  if (!object(value) || !Number.isSafeInteger(value.source) || value.source <= 0 ||
      !packageName.test(value.name ?? '') || severity(value.severity) < 0 || !safeString(value.range)) {
    throw new Error('invalid root advisory');
  }
  const match = typeof value.url === 'string' &&
    /^https:\/\/github\.com\/advisories\/(GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4})$/.exec(value.url);
  if (!match) throw new Error('root advisory has no supported GHSA identity');
  return { ghsa: match[1], package: value.name, range: value.range, severity: value.severity };
}

function occurrence(path, entry, name) {
  if (path === 'node_modules/' + name || path.endsWith('/node_modules/' + name) || entry.name === name) return true;
  if (typeof entry.resolved !== 'string') return false;
  try {
    const pathname = decodeURIComponent(new URL(entry.resolved).pathname);
    return pathname.includes('/' + name + '/-/');
  } catch {
    return false;
  }
}

function verifyNodes(nodes, packages, name, findings, errors) {
  if (!Array.isArray(nodes) || !nodes.length) { errors.push('missing lock nodes for ' + name); return; }
  for (const node of nodes) {
    if (!safeString(node) || !Object.hasOwn(packages, node) || !object(packages[node])) {
      errors.push('reported lock node is missing or invalid');
    } else if (packages[node].dev !== true) {
      findings.push('production occurrence of ' + name + ' at ' + node);
    }
  }
}

// Each traversal owns its visited set: caching a partial cyclic traversal can lose roots.
function rootsFor(start, records) {
  const roots = new Map(), visited = new Set(), pending = [start], errors = [];
  while (pending.length) {
    const name = pending.pop();
    if (visited.has(name)) continue;
    visited.add(name);
    if (!Object.hasOwn(records, name) || !object(records[name]) ||
        !Array.isArray(records[name].via) || records[name].via.length === 0) {
      errors.push('missing advisory evidence for ' + name);
      continue;
    }
    for (const via of records[name].via) {
      if (typeof via === 'string') {
        if (!Object.hasOwn(records, via)) errors.push('dangling advisory reference');
        else pending.push(via);
      } else {
        try {
          const root = rootAdvisory(via);
          roots.set(JSON.stringify(root), root);
        } catch (error) {
          errors.push(error.message);
        }
      }
    }
  }
  if (!roots.size) errors.push('rootless advisory graph');
  return { roots: [...roots.values()], errors };
}

export function evaluate({ rows, component, level, auditJson, lockJson, today }) {
  const findings = [], lifecycle = [], errors = [], used = new Set(), accepted = new Map();
  const threshold = severity(level);
  const now = day(today);
  if (threshold < 0) throw new Error('unknown audit level');
  if (!object(auditJson) || auditJson.auditReportVersion !== 2 || !object(auditJson.vulnerabilities) ||
      auditJson.error != null) throw new Error('unsupported audit report');
  if (!object(lockJson) || lockJson.lockfileVersion !== 3 || !object(lockJson.packages)) throw new Error('unsupported lockfile');
  const applicable = rows.filter(row => row.component === component);
  // Validate programmatic inputs too, not only the TSV parser's output.
  parseRows(columns.join('\t') + '\n' + rows.map(row => columns.map(key => row[key]).join('\t')).join('\n'), today);
  const records = auditJson.vulnerabilities;

  for (const [name, record] of Object.entries(records)) {
    if (!object(record) || !packageName.test(name) || !packageName.test(record.name ?? '') ||
        severity(record.severity) < 0) {
      errors.push('invalid vulnerability record');
      continue;
    }
    const closure = rootsFor(name, records);
    errors.push(...closure.errors);
    if (severity(record.severity) < threshold) continue;
    const roots = closure.roots.filter(root => severity(root.severity) >= threshold);
    if (!roots.length) errors.push('gated vulnerability has no gated root');
    for (const root of roots) {
      const row = applicable.find(candidate => candidate.ghsa === root.ghsa && candidate.package === root.package);
      if (!row) {
        findings.push('unexcepted ' + root.ghsa + ' ' + root.package);
        continue;
      }
      used.add(row);
      verifyNodes(record.nodes, lockJson.packages, name, findings, errors);
      if (root.range !== row.range) findings.push('advisory range changed for ' + root.ghsa);
      if (severity(root.severity) > severity(row.max_severity) ||
          severity(record.severity) > severity(row.max_severity)) findings.push('severity exceeds exception for ' + root.ghsa);
      if (!accepted.has(row)) accepted.set(row, new Set());
    }
  }

  for (const row of applicable) {
    if (day(row.recheck_by) < now) lifecycle.push('expired exception ' + row.ghsa);
    if (!used.has(row)) {
      lifecycle.push('unused exception ' + row.ghsa);
      continue;
    }
    const reported = records[row.package];
    if (!object(reported) || !Array.isArray(reported.nodes) || !reported.nodes.length) {
      errors.push('missing reported lock nodes for ' + row.package);
      continue;
    }
    const nodes = new Set();
    for (const node of reported.nodes) {
      if (!safeString(node)) errors.push('invalid reported lock node');
      else {
        nodes.add(node);
        const entry = lockJson.packages[node];
        if (object(entry) && !occurrence(node, entry, row.package)) errors.push('reported package identity differs from lockfile');
      }
    }
    for (const [path, entry] of Object.entries(lockJson.packages)) {
      if (!object(entry)) { errors.push('invalid lock entry'); continue; }
      if (path && occurrence(path, entry, row.package)) nodes.add(path);
    }
    if (!nodes.size) errors.push('empty lock-node evidence');
    verifyNodes([...nodes], lockJson.packages, row.package, findings, errors);
    accepted.set(row, nodes);
  }

  // Real, independently established findings stay findings even if other evidence is broken.
  const code = findings.length ? 1 : errors.length ? 2 : lifecycle.length ? 1 : 0;
  const messages = [...new Set(code === 1 ? [...findings, ...lifecycle] : errors)];
  if (code === 0) {
    for (const [row, nodes] of accepted) messages.push('npm-audit-gate: EXCEPTED ' + row.ghsa + ' ' + row.package +
      ' in ' + component + ' (dev-only, ' + nodes.size + ' nodes, range ' + row.range +
      ', recheck by ' + row.recheck_by + ', ' + row.issue + ')');
  }
  return { code, messages };
}

function readJson(path, label) {
  try { return JSON.parse(readFileSync(path, 'utf8')); }
  catch { throw new Error('cannot read supported ' + label + ' JSON'); }
}

function main(args) {
  const scriptDir = realpathSync(dirname(fileURLToPath(import.meta.url)));
  const [mode, packageDir, level, auditPath] = args;
  if (!['has-rows', 'evaluate'].includes(mode) || !packageDir ||
      args.length !== (mode === 'has-rows' ? 2 : 4)) throw new Error('invalid evaluator arguments');
  const today = new Date().toISOString().slice(0, 10);
  const rows = parseRows(readFileSync(resolve(scriptDir, 'npm-audit-exceptions.tsv'), 'utf8'), today);
  const packageRoot = realpathSync(packageDir);
  const component = relative(realpathSync(resolve(scriptDir, '..')), packageRoot);
  if (isAbsolute(component) || component === '..' || component.startsWith('../')) throw new Error('package outside repository');
  if (mode === 'has-rows') return rows.some(row => row.component === component) ? 0 : 3;
  if (!rows.some(row => row.component === component)) throw new Error('no applicable exception rows');
  const result = evaluate({ rows, component, level, today,
    auditJson: readJson(auditPath, 'audit'), lockJson: readJson(resolve(packageRoot, 'package-lock.json'), 'lockfile') });
  for (const message of result.messages) process[result.code ? 'stderr' : 'stdout'].write(message + '\n');
  return result.code;
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url))) {
  try { process.exitCode = main(process.argv.slice(2)); }
  catch (error) {
    process.stderr.write('npm-audit-gate: INSTRUMENT FAILURE: ' + error.message + '\n');
    process.exitCode = 2;
  }
}
