// Read the current test declarations independently of the execution report.
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const repo = fileURLToPath(new URL('../../', import.meta.url));
const require = createRequire(path.join(repo, 'agent/package.json'));
const { API } = await import(pathToFileURL(require.resolve('typescript/unstable/sync')));
const { SyntaxKind } = await import(pathToFileURL(require.resolve('typescript/unstable/ast')));
const files = ['codex-launcher', 'codex-executor', 'codex-shared-dir', 'entrypoint-migration'];
const api = new API({ cwd: path.join(repo, 'agent') });
const expected = [];
try {
  const snapshot = api.updateSnapshot({ openProjects: [path.join(repo, 'agent/tsconfig.json')] });
  const program = snapshot.getProjects()[0].program;
  for (const file of files) {
    const filename = path.join(repo, `agent/test/${file}.test.ts`);
    if (program.getSyntacticDiagnostics(filename).length) throw new Error(`invalid test syntax: ${file}`);
    const source = program.getSourceFile(filename);
    if (!source) throw new Error(`missing source: ${file}`);
    let gates = 0;
    const visit = (node, ancestors = [], targeted = false) => {
      if (node.kind === SyntaxKind.CallExpression && ['it', 'describe'].includes(node.expression.text)) {
        const name = node.arguments[0];
        const options = node.arguments.length === 3 ? node.arguments[1] : undefined;
        const gate = options?.properties?.some(p => p.name?.text === 'skip' && (
          ['INIT_SKIP', 'GROUP_B_SKIP', 'GROUP2_SKIP'].includes(p.initializer?.text) ||
          (file === 'codex-launcher' && source.text.slice(p.pos, p.end).includes('WORKER_UID'))
        ));
        if (gate) gates++;
        const active = targeted || gate;
        if (name.kind !== SyntaxKind.StringLiteral) {
          if (active) throw new Error(`nonliteral targeted test title: ${file}`);
        } else {
          const names = [...ancestors, name.text];
          if (node.expression.text === 'it' && active) {
            expected.push({ file: `agent/test/${file}.test.ts`, names });
            return;
          }
          const callback = node.arguments[node.arguments.length - 1];
          callback.forEachChild(child => { visit(child, names, active); });
          return;
        }
      }
      node.forEachChild(child => { visit(child, ancestors, targeted); });
    };
    visit(source);
    if (gates !== 1 || !expected.some(test => test.file.endsWith(`/${file}.test.ts`))) {
      throw new Error(`${file}: expected one UID gate with nonempty leaf inventory, found ${gates}`);
    }
  }
  process.stdout.write(JSON.stringify(expected, null, 2) + '\n');
} finally {
  api.close();
}
