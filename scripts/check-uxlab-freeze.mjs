#!/usr/bin/env node
import { openSync, fstatSync, readSync, closeSync, constants } from 'node:fs';
import { fileURLToPath } from 'node:url';

const limit = 1024 * 1024;

// Preserve strings (including URLs and escaped quotes) while removing JSONC comments.
function uncomment(input) {
  let output = '', string = false, escaped = false;
  for (let i = 0; i < input.length; i++) {
    const c = input[i], next = input[i + 1];
    if (string) {
      output += c;
      if (escaped) escaped = false;
      else if (c === '\\') escaped = true;
      else if (c === '"') string = false;
    } else if (c === '"') {
      string = true;
      output += c;
    } else if (c === '/' && next === '/') {
      while (i < input.length && input[i] !== '\n') i++;
      output += '\n';
    } else if (c === '/' && next === '*') {
      i += 2;
      while (i < input.length && !(input[i] === '*' && input[i + 1] === '/')) i++;
      if (i === input.length) throw new Error('unterminated comment');
      i++;
      output += ' ';
    } else output += c;
  }
  return output;
}

function pin(path, label) {
  let fd;
  let manifest;
  try {
    // Nonblocking open also prevents a FIFO from hanging before the regular-file check.
    fd = openSync(path, constants.O_RDONLY | constants.O_NONBLOCK);
    const before = fstatSync(fd);
    if (!before.isFile()) throw new Error('not regular');
    if (before.size > limit) throw new Error('oversize');
    const bytes = Buffer.alloc(limit);
    let length = 0;
    while (length < bytes.length) {
      const count = readSync(fd, bytes, length, bytes.length - length, null);
      if (count === 0) break;
      length += count;
    }
    // Check growth without reading an overflow byte: actual reads stay <= 1 MiB.
    if (fstatSync(fd).size > limit) throw new Error('oversize');
    manifest = JSON.parse(uncomment(new TextDecoder('utf-8', { fatal: true }).decode(bytes.subarray(0, length))));
  } catch {
    throw new Error(`${label}: unreadable, nonregular, oversized or malformed manifest (limit 1 MiB)`);
  } finally {
    if (fd !== undefined) closeSync(fd);
  }
  if (!Array.isArray(manifest?.packages) || !manifest.packages.every(p => typeof p === 'string')) {
    throw new Error(`${label}: packages must be an array of strings`);
  }
  const candidates = manifest.packages.filter(p => p.split('@', 1)[0] === 'charm-freeze');
  if (candidates.length === 0) throw new Error(`${label}: missing charm-freeze pin`);
  if (candidates.length !== 1 || !/^charm-freeze@[0-9]+\.[0-9]+\.[0-9]+$/.test(candidates[0])) {
    throw new Error(`${label}: require one explicit, unambiguous charm-freeze version`);
  }
  return candidates[0];
}

try {
  if (process.argv.length > 4) throw new Error('usage: check-uxlab-freeze.mjs [root-manifest [nested-manifest]]');
  const root = process.argv[2] ?? fileURLToPath(new URL('../devbox.json', import.meta.url));
  const nested = process.argv[3] ?? fileURLToPath(new URL('../api/cmd/uzi/uxlab/devbox.json', import.meta.url));
  if (pin(root, 'root') !== pin(nested, 'nested')) throw new Error('root/nested charm-freeze version mismatch');
  console.log('uxlab: root/nested charm-freeze pins match');
} catch (error) {
  console.error(`uxlab: ${error.message}`);
  process.exitCode = 1;
}
