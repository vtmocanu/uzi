import fs from 'node:fs';
const expected = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'));
if (!expected.length) throw new Error('empty worker UID inventory');
const escape = text => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
// Node's full test name joins suite ancestry and the leaf title with spaces.
process.stdout.write(expected.map(test => `^${escape(test.names.join(' '))}$`).join('|'));
