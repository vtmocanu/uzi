// Read the current test declarations independently of the execution report.
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const repo = fileURLToPath(new URL('../../', import.meta.url));
const require = createRequire(path.join(repo, 'agent/package.json'));
const { API } = await import(pathToFileURL(require.resolve('typescript/unstable/sync')));
const { SyntaxKind } = await import(pathToFileURL(require.resolve('typescript/unstable/ast')));
const files = ['codex-launcher', 'codex-executor', 'codex-shared-dir', 'entrypoint-migration', 'git-planning-diff'];
const captureSuite = 'Unit2 runner source capture';
const captureLeaves = new Set([
  'cancels while the object-store snapshot streams and leaves no temp tree',
  'cancels while many tiny object files are copied and leaves no temp tree',
  "never reads a symlink target's content",
  'reads each symlink only through its still-open pinned parent descriptor',
  'refuses a leaf swapped to a regular file before readlink without resolving the pathname',
  'refuses leaf and parent symlink escapes, nonregular ignores and an unavailable base',
  'captures an unchanged tracked symlink tree as an empty patch',
  'captures edits elsewhere next to unchanged tracked symlinks',
  'treats a symlinked .gitignore as absent while the path stays a candidate',
]);

// The source AST seam lets regressions share one small parser project.
export function inventoryForSource(source, file) {
  const planning = file === 'git-planning-diff';
  const expected = [];
  const identities = new Set();
  let gates = 0;
  let suites = 0;
  const visit = (node, ancestors = [], targeted = false) => {
    if (node.kind === SyntaxKind.CallExpression && ['it', 'describe'].includes(node.expression.text)) {
      const name = node.arguments[0];
      const options = node.arguments.length === 3 ? node.arguments[1] : undefined;
      const gate = !planning && options?.properties?.some(p => p.name?.text === 'skip' && (
        ['INIT_SKIP', 'GROUP_B_SKIP', 'GROUP2_SKIP'].includes(p.initializer?.text) ||
        (file === 'codex-launcher' && source.text.slice(p.pos, p.end).includes('WORKER_UID'))
      ));
      if (gate) gates++;
      const active = targeted || gate;
      if (name?.kind !== SyntaxKind.StringLiteral) {
        if (active) throw new Error(`nonliteral targeted test title: ${file}`);
        // Unknown planning ancestry cannot establish an exact capture identity.
        if (planning) return;
      } else {
        const names = [...ancestors, name.text];
        if (planning && node.expression.text === 'describe' && ancestors.length === 0 && name.text === captureSuite) suites++;
        const capture = planning && ancestors.length === 1 && ancestors[0] === captureSuite && captureLeaves.has(name.text);
        if (node.expression.text === 'it' && (active || capture)) {
          const identity = JSON.stringify(names);
          if (identities.has(identity)) throw new Error(`duplicate source leaf identity: ${file}: ${identity}`);
          identities.add(identity);
          expected.push({ file: `agent/test/${file}.test.ts`, names });
          return;
        }
        const callback = node.arguments[node.arguments.length - 1];
        callback?.forEachChild(child => { visit(child, names, active); });
        return;
      }
    }
    node.forEachChild(child => { visit(child, ancestors, targeted); });
  };
  visit(source);
  if (planning) {
    if (suites !== 1) throw new Error(`${file}: expected exactly one literal ${captureSuite} suite, found ${suites}`);
    for (const leaf of captureLeaves) {
      if (!expected.some(test => test.names[1] === leaf)) throw new Error(`${file}: missing required literal capture leaf: ${leaf}`);
    }
  } else if (gates !== 1 || !expected.length) {
    throw new Error(`${file}: expected one UID gate with nonempty leaf inventory, found ${gates}`);
  }
  return expected;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const api = new API({ cwd: path.join(repo, 'agent') });
  try {
    const snapshot = api.updateSnapshot({ openProjects: [path.join(repo, 'agent/tsconfig.json')] });
    const program = snapshot.getProjects()[0].program;
    const expected = files.flatMap(file => {
      const filename = path.join(repo, `agent/test/${file}.test.ts`);
      if (program.getSyntacticDiagnostics(filename).length) throw new Error(`invalid test syntax: ${file}`);
      const source = program.getSourceFile(filename);
      if (!source) throw new Error(`missing source: ${file}`);
      return inventoryForSource(source, file);
    });
    process.stdout.write(JSON.stringify(expected, null, 2) + '\n');
  } finally {
    api.close();
  }
}
