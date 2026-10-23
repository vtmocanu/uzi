// Read the current test declarations independently of the execution report.
import { createRequire } from 'node:module';
import { fileURLToPath, pathToFileURL } from 'node:url';
import path from 'node:path';

const repo = fileURLToPath(new URL('../../', import.meta.url));
const require = createRequire(path.join(repo, 'agent/package.json'));
const { API } = await import(pathToFileURL(require.resolve('typescript/unstable/sync')));
const { SyntaxKind } = await import(pathToFileURL(require.resolve('typescript/unstable/ast')));
const files = ['codex-launcher', 'codex-executor', 'codex-shared-dir', 'entrypoint-migration', 'git-planning-diff', 'codex-capture-reaper-real', 'git-retained-permissions', 'claude-command-launcher', 'claude-command-real'];
const singleFile = 'single-uid-preflight';
const sharedFile = 'runner-shared-real';
const e2eFiles = new Set(['memory-command-real', 'claude-command-real', singleFile, sharedFile]);
const captureSuite = 'Unit2 runner source capture';
const captureLeaves = new Set([
  'cancels during worktree comparisons and leaves no temp tree',
  'cancels while many tiny worktree entries are walked and leaves no temp tree',
  "never reads a symlink target's content",
  'reads each symlink only through its still-open pinned parent descriptor',
  'refuses a leaf swapped to a regular file before readlink without resolving the pathname',
  'refuses leaf and parent symlink escapes, nonregular ignores and an unavailable base',
  'captures an unchanged tracked symlink tree as an empty patch',
  'captures edits elsewhere next to unchanged tracked symlinks',
  'treats a symlinked .gitignore as absent while the path stays a candidate',
  'reads loose trusted bare objects as the real runner uid without write authority',
  'reads packed trusted bare objects as the real runner uid without write authority',
  'captures under the enforced required command sandbox',
]);

const claudeRealSuite = 'real Claude guarded command lane';
const claudeRealLeaves = new Set([
  'stops a detached command and its descendants through a guard-style cancel and confirms the drain',
  'expires a short explicit deadline on a command with a detached descendant and drains the whole tree',
  "cancelling the selected run's command leaves a sibling run's command alive until it completes",
  "lets another attempt's capture scan skip the live command's recorded root while the owner's own reap still kills it",
  'rejects a candidate observation whose root start time differs from the enrolled one and cancels nothing',
  'never selects a larger unattributed process tree',
  "returns the exact worker_memory_pressure result as the selected command's tool error",
  'refuses a registration whose source is not one of the two model-command literals',
  'never enrolls unminted, cloned or foreign-context metadata, yet the command still runs',
  'gives a command that declares its own worker marks no protection and no special enrollment',
  'poisons the lane and kills the exact supervisor when a stopped supervisor cannot confirm disposal',
  'keeps native Bash when the guard is off',
]);
const singleLeaf = 'single-uid launcher preflight matches the container posture';

// Issue #3153: the runner-shared layout under real uids. One root-started container, nine literal
// top-level leaves; none may be skipped, renamed or made nonliteral.
const sharedLeaves = [
  'runner-init refuses a missing, wrong or symlinked layout marker before any write',
  'runner-init as root builds worker:runner 3775 with a worker:worker 0755 .tmp and leaves old and sibling data untouched',
  'runner-init refuses a hostile runner-shared or .tmp symlink or file without following or re-owning anything',
  'runner-init as the worker uid requires ownership by the current uid and fails closed on anything else',
  'the runner, runner-cmd and dind uids cannot replace .tmp or runner-shared entries, and their groups exclude 10001',
  'a fresh .tmp is created by the entrypoint and by ensureRunnerTmpdir in both uid modes, and a planted symlink or file is refused',
  'per-uid scratch under a live writer: a mismatched owner is refused untouched, an expected owner is adopted with writer exclusion not established',
  "old-layout and new-layout entrypoints overlap on one data directory without re-owning, wiping or following into each other's tree",
  'foreign-owned run-directory content is retained, counted and logged by the production retention delete in split and single-uid modes',
];

const retainedLeaves = new Set([
  "recovers retained command-owned loose objects",
  "recovers retained command-owned packed objects",
  "normalizes before verifying an already captured restore tip",
  "preserves worker-owned hardlinks while recovering command-owned loose objects",
  "preserves worker-owned hardlinks while recovering command-owned packed objects",
  "preserves runner-owned hardlinks while recovering command-owned loose objects",
  "preserves runner-owned hardlinks while recovering command-owned packed objects",
  "refuses command-owned multiply linked mutation candidates and keeps custody",
  "keeps source and custody when retained object recovery fails"
]);

// The source AST seam lets regressions share one small parser project.
export function inventoryForSource(source, file) {
  const planning = file === 'git-planning-diff';
  const command = file === 'memory-command-real' || file === singleFile || file === sharedFile;
  const claudeReal = file === 'claude-command-real';
  const retained = file === 'git-retained-permissions';
  const expected = [];
  const identities = new Set();
  let gates = 0;
  let suites = 0;
  const visit = (node, ancestors = [], targeted = false) => {
    if (node.kind === SyntaxKind.CallExpression && ['it', 'test', 'describe'].includes(node.expression.text)) {
      const name = node.arguments[0];
      const options = node.arguments.length === 3 ? node.arguments[1] : undefined;
      const gate = !planning && options?.properties?.some(p => p.name?.text === 'skip' && (
        ['INIT_SKIP', 'GROUP_B_SKIP', 'GROUP2_SKIP'].includes(p.initializer?.text) ||
        (file === 'codex-launcher' && source.text.slice(p.pos, p.end).includes('WORKER_UID'))
      ));
      if (gate) gates++;
      const active = targeted || gate || command;
      if (name?.kind !== SyntaxKind.StringLiteral) {
        if (active) throw new Error(`nonliteral targeted test title: ${file}`);
        // Unknown planning ancestry cannot establish an exact capture identity.
        if (planning) return;
      } else {
        const names = [...ancestors, name.text];
        if (planning && node.expression.text === 'describe' && ancestors.length === 0 && name.text === captureSuite) suites++;
        const capture = planning && ancestors.length === 1 && ancestors[0] === captureSuite && captureLeaves.has(name.text);
        if (['it', 'test'].includes(node.expression.text) && (active || capture)) {
          const identity = JSON.stringify(names);
          if (identities.has(identity)) throw new Error(`duplicate source leaf identity: ${file}: ${identity}`);
          identities.add(identity);
          expected.push({ file: command || claudeReal ? `e2e/worker-uid/${file}.test.mts` : `agent/test/${file}.test.ts`, names });
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
  } else if (retained) {
    if (gates !== 1) throw new Error(`${file}: expected one UID gate, found ${gates}`);
    for (const leaf of retainedLeaves) {
      if (!expected.some(test => test.names.length === 2 && test.names[1] === leaf)) {
        throw new Error(`${file}: missing required literal retained recovery leaf: ${leaf}`);
      }
    }
  } else if (claudeReal) {
    if (gates !== 1) throw new Error(`${file}: expected one UID gate, found ${gates}`);
    for (const leaf of claudeRealLeaves) {
      if (!expected.some(test => test.names.length === 2 && test.names[0] === claudeRealSuite && test.names[1] === leaf)) {
        throw new Error(`${file}: missing required literal Claude command proof: ${leaf}`);
      }
    }
  } else if (file === singleFile) {
    if (!expected.some(test => test.names.length === 1 && test.names[0] === singleLeaf)) {
      throw new Error(`${file}: missing required literal single-uid proof: ${singleLeaf}`);
    }
  } else if (file === sharedFile) {
    for (const leaf of sharedLeaves) {
      if (!expected.some(test => test.names.length === 1 && test.names[0] === leaf)) {
        throw new Error(`${file}: missing required literal runner-shared proof: ${leaf}`);
      }
    }
  } else if (command) {
    const titles = [
      'real adopted detached tree: eligibility, identity refusal, concurrent cleanup and exact pressure failure',
      'real supervisor observation fixtures execute under the command UID',
    ];
    for (const title of titles) {
      if (!expected.some(test => test.names.length === 1 && test.names[0] === title)) {
        throw new Error(`${file}: missing required literal command proof: ${title}`);
      }
    }
  } else if (file === 'codex-capture-reaper-real') {
    const required = [
      'fixed capture reaper: real runner procfs and signal compatibility',
      'fixed capture reaper: descendant settlement precedes verdict',
      'fixed capture reaper: own mode kills owned residue before capture proceeds',
      'fixed capture reaper: genuine report preserves the same owned writer',
    ];
    if (gates !== 1 || expected.length !== required.length || required.some(title =>
      !expected.some(test => test.names.length === 2 && test.names[0] === 'fixed capture reaper: real worker profile' && test.names[1] === title))) {
      throw new Error(`${file}: expected one UID gate and all required literal fixed reaper leaves`);
    }
  } else if (gates !== 1 || !expected.length) {
    throw new Error(`${file}: expected one UID gate with nonempty leaf inventory, found ${gates}`);
  }
  return expected;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const api = new API({ cwd: path.join(repo, 'agent') });
  try {
    const snapshot = api.updateSnapshot({ openProjects: [path.join(repo, 'e2e/worker-uid/tsconfig.json')] });
    const program = snapshot.getProjects()[0].program;
    const lane = process.argv[2] ?? 'worker';
    if (!['worker', 'command', 'single', 'shared'].includes(lane)) throw new Error(`unknown lane: ${lane}`);
    const selected = lane === 'command' ? ['memory-command-real'] : lane === 'single' ? [singleFile] : lane === 'shared' ? [sharedFile] : files;
    const expected = selected.flatMap(file => {
      const filename = path.join(repo, e2eFiles.has(file)
        ? `e2e/worker-uid/${file}.test.mts` : `agent/test/${file}.test.ts`);
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
