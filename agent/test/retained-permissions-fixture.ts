import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { createHash } from "node:crypto";
import { execFileSync, spawnSync } from "node:child_process";
import { afterEach } from "node:test";
import { launchCodexEffectRoot, SUPERVISOR_BIN } from "../src/codex/launcher.js";
import { runnerCommand } from "../src/runner-uid.js";
import { setQuiescenceViewForTests } from "../src/run-quiescence.js";
import { restoreHermeticView } from "./setup/hermetic-proc.js";
import { noProofReseed, recordingLogger } from "./helpers.js";
import { CodexExecutor } from "../src/codex/codex-executor.js";
import { selectCodexBinding } from "../src/codex/select.js";
import { api, client, fakeGitlab, fx, git, gitlabClaim, homeDir, installHarness, runnerWith } from "./runner-harness.js";

const CONTENT = "unpublished command work for retained permissions\n";
const gitEnv = () => ({
  PATH: "/usr/local/bin:/usr/bin:/bin", LANG: "C", HOME: "/nonexistent",
  GIT_CONFIG_GLOBAL: "/dev/null", GIT_CONFIG_SYSTEM: "/dev/null", GIT_TERMINAL_PROMPT: "0",
});
const gitArgs = (cwd: string, args: string[]) => ["-c", "safe.directory=*", "-C", cwd, ...args];

function workerGit(cwd: string, ...args: string[]): string {
  return execFileSync("/usr/bin/git", gitArgs(cwd, args), { env: gitEnv(), encoding: "utf8", stdio: "pipe" }).trim();
}
function runnerExec(command: string, args: string[], input?: string): string {
  const wrapped = runnerCommand(command, args);
  return execFileSync(wrapped.command, wrapped.args, {
    env: gitEnv(), encoding: "utf8", stdio: "pipe", input, timeout: 15_000,
  }).trim();
}
function runnerGit(cwd: string, ...args: string[]): string {
  return runnerExec("/usr/bin/git", gitArgs(cwd, args));
}

// No injected launcher, setpriv implementation, supervisor binary, uid resolver, or credential env.
// One primary child per call; wait and dispose each within 15s before starting a sibling.
async function commandExec(cwd: string, command: string, args: string[]) {
  const root = await launchCodexEffectRoot({
    identity: "command", command, args, cwd, env: gitEnv(), supervisorBin: SUPERVISOR_BIN,
  });
  let stdout = "", stderr = "";
  root.transport.stdout?.setEncoding("utf8").on("data", chunk => { stdout += chunk; });
  root.transport.stderr?.setEncoding("utf8").on("data", chunk => { stderr += chunk; });
  root.transport.stdin?.end();
  let code: number;
  try {
    assert.equal(root.started.uid, 10003);
    assert.equal(root.started.liveCapsZero, true);
    assert.equal(root.started.noNewPrivs, true);
    code = (await root.waitChild(15_000)).code;
  } finally {
    assert.equal((await root.dispose(15_000)).clean, true, "production command supervisor drained");
  }
  return { code, stdout: stdout.trim(), stderr };
}
async function commandNode(cwd: string, source: string, ...args: string[]) {
  const result = await commandExec(cwd, process.execPath, ["-e", source, ...args]);
  assert.equal(result.code, 0, result.stderr);
  return result.stdout;
}
async function commandGit(cwd: string, ...args: string[]) {
  const result = await commandExec(cwd, "/usr/bin/git", gitArgs(cwd, args));
  assert.equal(result.code, 0, result.stderr);
  return result.stdout;
}

type Identity = { uid: number; gid: number; mode: number; dev: number; ino: number; nlink: number; hash: string };
function identity(file: string): Identity {
  const st = fs.lstatSync(file);
  assert.equal(st.isFile(), true);
  assert.equal(st.isSymbolicLink(), false);
  return { uid: st.uid, gid: st.gid, mode: st.mode & 0o7777, dev: st.dev, ino: st.ino, nlink: st.nlink,
    hash: createHash("sha256").update(fs.readFileSync(file)).digest("hex") };
}
const SNAPSHOT = `
const fs = require("node:fs"), crypto = require("node:crypto");
const result = process.argv.slice(1).map(file => {
  const s = fs.lstatSync(file);
  if ((!s.isFile() && !s.isDirectory()) || s.isSymbolicLink()) throw Error("not an object or directory");
  return { uid:s.uid, gid:s.gid, mode:s.mode & 4095, dev:s.dev, ino:s.ino, nlink:s.nlink,
    hash:crypto.createHash("sha256").update(s.isDirectory()
      ? JSON.stringify(fs.readdirSync(file).sort()) : fs.readFileSync(file)).digest("hex") };
});
console.log(JSON.stringify(result));
`;
const RESTORE_ACCESS = `
const fs = require("node:fs"), path = require("node:path");
function visit(p) {
  const s = fs.lstatSync(p);
  if (s.uid !== 10003 || s.isSymbolicLink()) return;
  if (s.isDirectory()) {
    fs.chmodSync(p, 0o2770);
    for (const name of fs.readdirSync(p)) visit(path.join(p, name));
  } else if (s.isFile()) fs.chmodSync(p, 0o660);
}
const objects = process.argv[1];
for (const name of fs.readdirSync(objects)) visit(path.join(objects, name));
`;

let active: { clone: string } | undefined;
export function installRetainedPermissionsHarness() {
  // Registered before the harness cleanup: the worker cannot traverse command-private fanouts.
  afterEach(async () => {
    if (active) await commandNode(active.clone, RESTORE_ACCESS, path.join(active.clone, ".git/objects"));
    active = undefined;
    restoreHermeticView();
  });
  installHarness();
}

type Options = { packed?: boolean; resumed?: boolean; mixed?: "worker" | "runner"; linked?: boolean; corrupt?: boolean };
export async function retainedPermissionsCase(options: Options = {}) {
  const claim = gitlabClaim(3059, { claim_generation: 2, session_id: "retained-permissions-predecessor" });
  // makeFixture intentionally starts private. Open only this public test fixture's ancestor.
  fs.chmodSync(path.dirname(fx.originPath), 0o755);
  // The worker bare stays in its original worker group. Only the runner tree is shared:
  // making the worker config runner-group-writable would correctly fail retained admission.
  const bare = await git.ensureClone(fx.originPath);
  const runnerRoot = path.join(fx.dataDir, "runner");
  fs.mkdirSync(runnerRoot, { recursive: true });
  fs.chownSync(runnerRoot, -1, 10002);
  fs.chmodSync(runnerRoot, 0o2770);
  const clone = await git.createOrAttachRunnerClone(bare, 3059, noProofReseed, claim.run_id,
    false, undefined, undefined, { selfContained: true });
  assert.equal(fs.existsSync(path.join(clone.path, ".git/objects/info/alternates")), false,
    "the sandbox source must not read a captured tip back through bare alternates");
  active = { clone: clone.path };
  await git.markRecoveryCapture(bare, clone.path, clone.branch, claim.run_id);
  api.setOwnershipStatus(claim.run_id, "running", 2);
  const shared = path.join(path.dirname(fx.originPath), "permission-witnesses");
  fs.mkdirSync(shared);
  fs.chownSync(shared, -1, 10002);
  fs.chmodSync(shared, 0o2770);
  // The production fixed reaper requires actual procfs, with acquisition-local
  // attempt identity supplied through CodexExecutor below.
  setQuiescenceViewForTests(undefined);

  // Dummy private files prove both credential boundaries without introducing any real credential.
  const workerSecret = path.join(shared, "worker-private");
  fs.writeFileSync(workerSecret, ["worker", "dummy", "private"].join("-"), { mode: 0o600 });
  const providerSecret = path.join(shared, "provider-private");
  runnerExec(process.execPath, ["-e",
    'require("node:fs").writeFileSync(process.argv[1], ["provider","dummy","private"].join("-"), {mode:0o600})',
    providerSecret]);
  assert.equal(fs.statSync(workerSecret).uid, 10001);
  assert.equal(fs.statSync(providerSecret).uid, 10002);
  assert.ok(fs.readFileSync(workerSecret).length > 0);
  runnerExec(process.execPath, ["-e", 'require("node:fs").readFileSync(process.argv[1])', providerSecret]);
  for (const file of [workerSecret, providerSecret]) {
    const result = await commandExec(clone.path, process.execPath, ["-e", `
try { require("node:fs").readFileSync(process.argv[1]); process.exit(2); }
catch (e) { if (e.code !== "EACCES") throw e; }
`, file]);
    assert.equal(result.code, 0, "command uid must receive EACCES for private dummy credentials");
  }

  await commandNode(clone.path, `
const fs = require("node:fs");
if (process.getuid() !== 10003 || !process.getgroups().includes(10002)) throw Error("command identity");
process.umask(0o007);
fs.writeFileSync(process.argv[1], process.argv[2]);
`, path.join(clone.path, "retained.txt"), CONTENT);
  await commandGit(clone.path, "add", "retained.txt");
  await commandGit(clone.path, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test",
    "-c", "commit.gpgsign=false", "commit", "-m", "unpublished retained command objects");
  const tip = await commandGit(clone.path, "rev-parse", "HEAD");
  const tree = await commandGit(clone.path, "rev-parse", "HEAD^{tree}");
  const blob = await commandGit(clone.path, "rev-parse", "HEAD:retained.txt");
  assert.equal(await commandGit(clone.path, "cat-file", "blob", blob), CONTENT.trim());

  if (options.packed) {
    // Pack only the three newly written objects; leave existing runner-owned history alone.
    await commandNode(clone.path, `
const fs = require("node:fs"), cp = require("node:child_process"), path = require("node:path");
const cwd = process.argv[1], ids = process.argv.slice(2), objects = path.join(cwd, ".git/objects");
const pack = cp.execFileSync("/usr/bin/git",
  ["-c","safe.directory=*","-C",cwd,"pack-objects",path.join(objects,"pack/pack")],
  {input:ids.join("\\n")+"\\n",encoding:"utf8"}).trim();
for (const id of ids) fs.unlinkSync(path.join(objects,id.slice(0,2),id.slice(2)));
console.log(pack);
`, clone.path, tip, tree, blob);
  }

  const objects = path.join(clone.path, ".git/objects");
  let mixed: { names: string[]; before: Identity[] } | undefined;
  if (options.mixed) {
    // An unreferenced but valid preexisting object must survive the whole-directory recovery walk.
    // Its owner alone can fchmod it; ordinary read-only mode already permits shared-group access.
    const body = "preexisting " + options.mixed + " object\n";
    const oid = options.mixed === "worker"
      ? execFileSync("/usr/bin/git", gitArgs(clone.path, ["hash-object", "-w", "--stdin"]),
        { env: gitEnv(), input: body, encoding: "utf8" }).trim()
      : runnerExec("/usr/bin/git", gitArgs(clone.path, ["hash-object", "-w", "--stdin"]), body);
    const file = path.join(objects, oid.slice(0, 2), oid.slice(2));
    const alias = path.join(shared, "preexisting-object-alias");
    if (options.mixed === "worker") {
      fs.chmodSync(file, 0o440);
      fs.linkSync(file, alias);
    } else {
      runnerExec(process.execPath, ["-e", `
const fs=require("node:fs"); fs.chmodSync(process.argv[1],0o440); fs.linkSync(process.argv[1],process.argv[2]);
`, file, alias]);
    }
    const names = [file, alias];
    const before = names.map(identity);
    for (const item of before) {
      assert.equal(item.uid, options.mixed === "worker" ? 10001 : 10002);
      assert.equal(item.gid, 10002);
      assert.equal(item.mode, 0o440);
      assert.equal(item.nlink, 2);
    }
    assert.deepEqual(before[0], before[1]);
    assert.equal(workerGit(clone.path, "cat-file", "blob", oid), body.trim());
    assert.equal(runnerGit(clone.path, "cat-file", "blob", oid), body.trim());
    assert.equal(await commandGit(clone.path, "cat-file", "blob", oid), body.trim());
    mixed = { names, before };
  }

  const source = { runId: claim.run_id, clonePath: clone.path };
  const key = "issue-3059";
  if (options.resumed) {
    // A captured episode MUST already have the exact tip and pin in the worker bare.
    workerGit(bare, "-c", "protocol.file.allow=always", "fetch", "--no-tags", "file://" + clone.path,
      "+" + clone.branch + ":refs/uzi-runner/" + clone.branch);
    const reservation = await git.reserveRecoveryIteration(bare, clone.branch, key, source, 1, 2);
    await git.recordRecoveryCapture(bare, clone.branch, key, source, reservation.attempts, tip);
    assert.equal(await git.verifyRecoveryClosure(bare, tip), true);
    assert.equal(workerGit(bare, "rev-parse", "refs/uzi-recovery-episode/" + claim.run_id + "/" + tip), tip);
  } else {
    for (const oid of [tip, blob]) {
      const result = spawnSync("/usr/bin/git", gitArgs(bare, ["cat-file", "-e", oid]), { env: gitEnv() });
      assert.notEqual(result.status, 0, "unpublished object must be absent from the bare");
    }
  }

  const target = path.join(objects, blob.slice(0, 2), blob.slice(2));
  const candidateNames = options.linked ? [target, path.join(shared, "command-object-alias")] : [];
  if (options.linked) {
    await commandNode(clone.path,
      'require("node:fs").linkSync(process.argv[1],process.argv[2])', ...candidateNames);
  }
  if (options.corrupt) {
    // Select a clean-status scenario before the real pack transport rejects the damaged blob.
    // This fixture proves fetch-integrity refusal, not corruption detection during status.
    await commandGit(clone.path, "update-index", "--assume-unchanged", "retained.txt");
    await commandNode(clone.path, `
const fs=require("node:fs"); fs.chmodSync(process.argv[1],0o600);
fs.writeFileSync(process.argv[1],"deliberately corrupt retained loose object");
`, target);
  }

  // Apply exactly 2700/0400 only to command-owned object dirs/files.
  // The pack directory initially belongs to runner; replace its directory entry as runner first.
  if (options.packed) {
    const packDir = path.join(objects, "pack");
    const staging = path.join(objects, "command-pack-staging");
    await commandNode(clone.path, `
const fs=require("node:fs"), path=require("node:path");
fs.mkdirSync(process.argv[2],{mode:0o2770});
for(const n of fs.readdirSync(process.argv[1])) fs.renameSync(path.join(process.argv[1],n),path.join(process.argv[2],n));
`, packDir, staging);
    runnerExec(process.execPath, ["-e", 'require("node:fs").rmdirSync(process.argv[1])', packDir]);
    await commandNode(clone.path, 'require("node:fs").renameSync(process.argv[1],process.argv[2])', staging, packDir);
  }
  const privateEntries: Array<{ path: string; uid: number; gid: number; mode: number; dir: boolean }> =
    JSON.parse(await commandNode(clone.path, `
const fs=require("node:fs"), path=require("node:path"), entries=[];
function visit(p) {
  const s=fs.lstatSync(p);
  if(s.isSymbolicLink()) throw Error("unexpected link");
  if(s.isDirectory()) for(const n of fs.readdirSync(p)) visit(path.join(p,n));
  if(s.uid===10003) {
    fs.chmodSync(p,s.isDirectory()?0o2700:0o400);
    const after=fs.lstatSync(p);
    entries.push({path:p,uid:after.uid,gid:after.gid,mode:after.mode & 4095,dir:after.isDirectory()});
  }
}
for(const n of fs.readdirSync(process.argv[1])) visit(path.join(process.argv[1],n));
console.log(JSON.stringify(entries));
`, objects));
  assert.ok(privateEntries.some(e => !e.dir));
  assert.ok(privateEntries.some(e => e.dir));
  for (const entry of privateEntries) {
    assert.equal(entry.uid, 10003);
    assert.equal(entry.gid, 10002);
    assert.equal(entry.mode, entry.dir ? 0o2700 : 0o400);
    assert.throws(() => entry.dir ? fs.readdirSync(entry.path) : fs.readFileSync(entry.path),
      { code: "EACCES" }, "real worker denial");
    const wrapped = runnerCommand(process.execPath, ["-e", `
const fs=require("node:fs");
try { process.argv[2]==="dir" ? fs.readdirSync(process.argv[1]) : fs.readFileSync(process.argv[1]); process.exit(2); }
catch(e) { if(e.code!=="EACCES") throw e; }
`, entry.path, entry.dir ? "dir" : "file"]);
    const denied = spawnSync(wrapped.command, wrapped.args, { env: gitEnv() });
    assert.equal(denied.status, 0, "real runner denial: " + denied.stderr?.toString());
  }
  // Command still reads its own restricted source (including corrupted bytes in the failure case).
  const privateFiles = privateEntries.filter(e => !e.dir).map(e => e.path);
  const privateBefore: Identity[] = JSON.parse(await commandNode(clone.path, SNAPSHOT, ...privateFiles));
  assert.equal(privateBefore.length, privateFiles.length);
  const privateDirs = privateEntries.filter(e => e.dir).map(e => e.path);
  const privateDirsBefore: Identity[] = JSON.parse(await commandNode(clone.path, SNAPSHOT, ...privateDirs));
  assert.equal(privateDirsBefore.length, privateDirs.length);
  const sourceRefsBefore = await commandGit(clone.path, "for-each-ref", "--format=%(refname) %(objectname)");
  if (!options.corrupt) assert.equal(await commandGit(clone.path, "cat-file", "blob", blob), CONTENT.trim());
  else assert.notEqual((await commandExec(clone.path, "/usr/bin/git", gitArgs(clone.path, ["cat-file", "blob", blob]))).code, 0);
  const candidateBefore: Identity[] = candidateNames.length
    ? JSON.parse(await commandNode(clone.path, SNAPSHOT, ...candidateNames)) : [];
  if (options.linked) {
    assert.equal(candidateBefore[0]!.nlink, 2);
    assert.equal(candidateBefore[0]!.mode, 0o400);
    assert.deepEqual(candidateBefore[0], candidateBefore[1]);
  }

  const refsBefore = workerGit(bare, "for-each-ref", "--format=%(refname) %(objectname)").split("\n");
  let releases = 0, models = 0, settlements = 0, adoptionCalls = 0;
  let successor: Awaited<ReturnType<typeof git.prepareRecoverySuccessor>> | undefined;
  // Observe originals only. Assertions live after execute(), outside recovery callbacks whose
  // exceptions may be caught and converted into an ordinary blocked episode.
  const integrityTrace: string[] = [];
  const fetchFaults: Array<{ trace: string[]; diagnostic: string }> = [];
  if (options.corrupt) {
    const normalize = git.normalizeRetainedSourceObjects.bind(git);
    git.normalizeRetainedSourceObjects = async (...args) => {
      integrityTrace.push("readiness-start");
      const result = await normalize(...args);
      if (args[0] === bare && args[3].clonePath === clone.path) integrityTrace.push("readiness-ok");
      return result;
    };
    const status = git.worktreeStatus.bind(git);
    git.worktreeStatus = async (...args) => {
      const result = await status(...args);
      if (args[0] === clone.path) integrityTrace.push(result?.length === 0 ? "status-clean" : "status-unreadable-or-dirty");
      return result;
    };
    const fetch = git.fetchAgentBranch.bind(git);
    git.fetchAgentBranch = async (...args) => {
      const trace = [...integrityTrace];
      try {
        return await fetch(...args);
      } catch (error) {
        if (args[0] === bare && args[1] === clone.path && args[2] === clone.branch) {
          fetchFaults.push({ trace, diagnostic: error instanceof Error ? error.message : String(error) });
        }
        throw error;
      }
    };
  }
  const resumedReads: boolean[] = [];
  const verified = git.verifiedRecoveryRestorePoint.bind(git);
  git.verifiedRecoveryRestorePoint = async (...args) => {
    let readable = false;
    try { readable = runnerGit(clone.path, "cat-file", "blob", blob) === CONTENT.trim(); } catch { /* witness below */ }
    resumedReads.push(readable);
    return verified(...args);
  };
  const adopt = git.prepareRecoverySuccessor.bind(git);
  git.prepareRecoverySuccessor = async (...args) => {
    adoptionCalls++;
    successor = await adopt(...args);
    // Stop only AFTER real adoption. No provider/executor run is admitted at this seam.
    throw new Error("retained permissions fixture stops after real successor adoption");
  };
  client.releaseRecoveryCustody = async () => { releases++; throw new Error("fixture must retain custody"); };
  const log = recordingLogger();
  const selection = selectCodexBinding({ codex: {
    auth_mode: "api_key", access_token: "fixture-access", capability: "fixture-cap",
  } });
  assert.equal(selection.kind, "codex");
  if (selection.kind !== "codex") throw new Error("fixture binding unavailable");
  // Use the production pre-start capture path for immutable attempt binding and
  // supervised launch/cleanup. Provider startup remains forbidden in this fixture.
  const captureExecutor = new CodexExecutor(log.logger, path.join(homeDir, claim.run_id), {
    binding: selection.binding, client, commandSandbox: "required",
    provider: { name: "openai", baseUrl: "http://127.0.0.1:9/v1",
      envKey: "OPENAI_API_KEY", model: "gpt-6-astra" },
  }, {
    installDeps: async () => { throw new Error("must not install"); },
    provisionRunTools: async () => { throw new Error("must not provision"); },
    launchProviderRoot: async () => { models++; throw new Error("must not start provider"); },
  });
  await runnerWith(runId => ({
    homeDir: path.join(homeDir, runId),
    executor: {
      sandboxesCommands: true,
      withCredentialFreeCapture: (ms, action, purpose, signal, attempt) => {
        settlements++;
        return captureExecutor.withCredentialFreeCapture(ms, action, purpose, signal, attempt);
      },
      run: async () => { models++; throw new Error("must not start provider/model"); },
    },
  }), fakeGitlab().gitlab, undefined, log.logger, { recoveryRetryMs: 1 }).execute(claim);
  const journal = JSON.parse(workerGit(bare, "config", "uzi-recovery." + clone.branch + ".clone"));
  assert.equal(models, 0);
  assert.equal(releases, 0);
  assert.ok(settlements > 0, "retained recovery reached credential-free settlement");
  assert.ok(journal.recovery?.attempts > 0, "actual retained episode was durably reserved");
  assert.equal(fs.existsSync(clone.path), true, "source clone retained");
  assert.notEqual(journal.recovery?.blocker, "quiescence_failed",
    "the permission proof must reach source reads, not stop at test process-table setup");

  if (mixed) {
    assert.deepEqual(mixed.names.map(identity), mixed.before, "both hardlink names remain byte-for-byte and metadata identical");
    for (const name of mixed.names) {
      runnerExec(process.execPath, ["-e", 'require("node:fs").readFileSync(process.argv[1])', name]);
      await commandNode(clone.path, 'require("node:fs").readFileSync(process.argv[1])', name);
    }
  }
  if (options.linked || options.corrupt) {
    assert.equal(adoptionCalls, 0, "permission/integrity fault refuses adoption");
    assert.equal(successor, undefined);
    assert.equal(journal.clonePath, clone.path);
    assert.equal(journal.recovery.stage, "blocked");
    assert.notEqual(journal.recovery.blocker, "quiescence_failed", "the source fault, not generic process startup, blocked recovery");
    const refsAfter = workerGit(bare, "for-each-ref", "--format=%(refname) %(objectname)").split("\n");
    for (const ref of refsBefore) assert.ok(refsAfter.includes(ref), "existing source refs and journal pins survive");
    assert.equal(workerGit(clone.path, "rev-parse", "HEAD"), tip);
    assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), CONTENT);
    assert.equal(await commandGit(clone.path, "for-each-ref", "--format=%(refname) %(objectname)"), sourceRefsBefore,
      "negative recovery preserves every predecessor ref exactly");
    // A refused pass may have repaired a prefix. Every original object still has its exact
    // identity and bytes; its only permitted mode transition is the complete group-read addition.
    for (const [names, before, addition] of [
      [privateFiles, privateBefore, 0o040], [privateDirs, privateDirsBefore, 0o050],
    ] as const) {
      const after: Identity[] = JSON.parse(await commandNode(clone.path, SNAPSHOT, ...names));
      assert.equal(after.length, before.length);
      for (const [index, original] of before.entries()) {
        const observed = after[index]!;
        assert.ok(observed.mode === original.mode || observed.mode === (original.mode | addition),
          "negative recovery permits only unchanged mode or exact readiness addition: " + names[index]);
        assert.deepEqual(observed, { ...original, mode: observed.mode },
          "negative recovery preserves original object bytes, directory entries and identity: " + names[index]);
      }
    }
    if (options.linked) {
      assert.match(JSON.stringify(log.lines), /hardlinked_mutation/,
        "the command-owned hardlink mutation guard itself was reached; generic capture failure is not proof");
      assert.deepEqual(JSON.parse(await commandNode(clone.path, SNAPSHOT, ...candidateNames)), candidateBefore,
        "multiply linked command mutation candidate refused before fchmod on either name");
    } else {
      assert.ok(fetchFaults.length > 0,
        "corrupt source must reach real fetch integrity refusal; " + JSON.stringify({ integrityTrace, logs: log.lines }));
      for (const fault of fetchFaults) {
        assert.deepEqual(fault.trace.slice(-3), ["readiness-start", "readiness-ok", "status-clean"],
          "real readiness and clean runner status precede each actual corrupt-source fetch");
        assert.match(fault.diagnostic, new RegExp(`(?:loose object ${blob} [^\\n]*corrupt|unable to unpack ${blob} header)`),
          "real fetch rejected the deliberately corrupt blob, not a generic capture or permission fault");
        assert.doesNotMatch(fault.diagnostic, /Permission denied|EACCES|timed out|deadline|quiescen/i);
      }
      assert.equal(await commandNode(clone.path, 'console.log(require("node:fs").readFileSync(process.argv[1],"utf8"))', target),
        "deliberately corrupt retained loose object");
    }
    assert.ok(api.states.some(s => s.body.status === "failed"), "retained failure reported with custody kept");
    return;
  }

  assert.ok(successor, "actual retained recovery must adopt a successor; " + JSON.stringify(log.lines));
  assert.equal(adoptionCalls, 1);
  assert.equal(journal.clonePath, successor.path, "verified successor attribution was handed off durably");
  assert.equal(journal.recovery.stage, "ready-for-model");
  assert.ok(journal.retainedSources.some((entry: { clonePath: string }) => entry.clonePath === clone.path),
    "the predecessor remains in the adopted journal source set");
  assert.notEqual(successor.path, clone.path);
  assert.match(successor.path, /\.attempt-.*-g2-/);
  assert.equal(runnerGit(successor.path, "rev-parse", "HEAD"), tip);
  assert.equal(runnerGit(successor.path, "rev-parse", "HEAD^{tree}"), tree);
  assert.equal(runnerGit(successor.path, "rev-parse", "HEAD:retained.txt"), blob);
  assert.equal(fs.readFileSync(path.join(successor.path, "retained.txt"), "utf8"), CONTENT);
  assert.equal(workerGit(bare, "cat-file", "blob", blob), CONTENT.trim());
  assert.equal(await git.verifyRecoveryClosure(bare, tip), true);
  if (options.resumed) {
    assert.ok(resumedReads.length > 0, "captured restore tip was actually verified");
    assert.equal(resumedReads.every(Boolean), true, "source normalized BEFORE resumed-tip verification");
  }
  assert.deepEqual(JSON.parse(await commandNode(clone.path, SNAPSHOT, ...privateFiles)),
    privateBefore.map(entry => ({ ...entry, mode: entry.mode | 0o040 })),
    "predecessor objects retain bytes and identity with only group-read added");
  for (const entry of privateEntries.filter(entry => entry.dir)) {
    assert.equal(fs.lstatSync(entry.path).mode & 0o7777, entry.mode | 0o050,
      "predecessor directories gain only group read/traverse");
  }
  assert.equal(await commandGit(clone.path, "for-each-ref", "--format=%(refname) %(objectname)"), sourceRefsBefore,
    "predecessor refs remain unchanged after successor adoption");
  assert.equal(await commandGit(clone.path, "rev-parse", "HEAD"), tip);
  assert.equal(fs.readFileSync(path.join(clone.path, "retained.txt"), "utf8"), CONTENT);
  const refsAfter = workerGit(bare, "for-each-ref", "--format=%(refname) %(objectname)").split("\n");
  for (const ref of refsBefore.filter(ref => ref.startsWith("refs/uzi-recovery-episode/"))) {
    assert.ok(refsAfter.includes(ref), "pre-existing recovery pins remain unchanged after adoption");
  }
  assert.equal(workerGit(bare, "rev-parse", "refs/uzi-recovery-episode/" + claim.run_id + "/" + tip), tip,
    "the exact captured recovery pin survives successor adoption");
}
