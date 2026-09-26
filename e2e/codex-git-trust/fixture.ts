// Opt-in Issue #1716 worker-image fixture: a Codex command (runner-cmd 10003, supervisor +
// command sandbox) runs git in the runner-owned (10002) checkout. Every check runs and prints
// PASS/FAIL, so a base-code run (no withCommandGitTrust, labelled UNFIXED) shows which fail.
import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";
import type { Readable } from "node:stream";

const src = process.env.CODEX_GIT_TRUST_SRC ?? "/app/src";
const load = async (name: string) => import(pathToFileURL(path.join(src, name)).href);
const exec = promisify(execFile);
const root = `/data/runner/codex-git-trust-${process.pid}`;
const checkoutA = path.join(root, "worktree");
const nestedB = path.join(checkoutA, "nested-b");
const DUBIOUS = "detected dubious ownership";

type Spawn = (argv: string[], opts: { cwd?: string; env?: NodeJS.ProcessEnv }) =>
  Promise<{ code: number; stdout: string; stderr: string }>;
type Wrap = (command: string, args: readonly string[]) => { command: string; args: string[] };

const failures: string[] = [];

function check(label: string, ok: boolean, detail: string): void {
  if (ok) {
    console.log(`PASS: ${label}`);
  } else {
    const lines = detail.split("\n");
    console.log(`FAIL: ${label}: ${lines.slice(0, 3).join(" | ")}${lines.length > 3 ? " | ..." : ""}`);
    failures.push(label);
  }
}

/** Run `argv` as the worker-owned runner identity (10002), the checkout's owner. */
async function asRunner(wrap: Wrap, argv: string[]): Promise<string> {
  const [cmd, ...rest] = argv;
  const w = wrap(cmd!, rest);
  return (await exec(w.command, w.args, { cwd: root, env: { PATH: "/usr/bin:/bin" } })).stdout;
}

/** The first line of `text` naming the dubious-ownership rejection, for the log. */
function dubiousLine(text: string): string {
  return text.split("\n").find((line) => line.includes(DUBIOUS)) ?? "(none)";
}

/**
 * issue #1769 m3 part 2 TODO — the finalize import (`GitCache.ensureRunnerCloneObjects`, the
 * m2 agent's boundary-machinery API for pulling in whatever the run's finalize/push path still
 * needs from the bare) under the real command-sandbox boundary. Stubbed here so part 1 has a
 * clearly marked hook: part 2 calls the real API and asserts it succeeds with the bare NOT
 * granted to the sandboxed command, exactly like the seed-time materialization above it.
 */
// eslint-disable-next-line @typescript-eslint/no-unused-vars
async function sharedCloneFinalizeImport(_git: unknown, _bare: string, _clonePath: string, _branch: string): Promise<void> {
  // TODO(#1769 m3 part 2): call the finalize-import API here, under the command sandbox, and
  // assert it completes without the bare mounted into the sandboxed command's granted roots.
}

/** Drains a packfile stream (checkpointPack's `pack`) to a Buffer for `git index-pack --stdin`. */
async function drain(r: Readable): Promise<Buffer> {
  const chunks: Buffer[] = [];
  for await (const c of r) chunks.push(c as Buffer);
  return Buffer.concat(chunks);
}

/** Plain git helper for the fixture's own stand-in "forge" repo: not part of the boundary under
 *  test, so it runs as whatever uid the fixture process already is (worker, 10001). */
async function plainGit(dir: string, args: string[]): Promise<string> {
  const identity = ["-c", "user.email=fixture@example.com", "-c", "user.name=fixture", "-c", "commit.gpgsign=false"];
  const { stdout } = await exec("git", ["-C", dir, ...identity, ...args], { env: { PATH: "/usr/bin:/bin" } });
  return stdout.trim();
}

/**
 * issue #1769 m3 part 1 — the shared-clone section, with its OWN FIXED/UNFIXED label (whether
 * the mounted src's git.js exports `RunnerCloneMaterializationError`; base commit 3989078e does
 * not). Builds a REALISTIC owner-matched DIVERGED wip(park) checkpoint through the REAL
 * `GitCache` — the exact shape `agent/test/git-materialize.test.ts`'s "owner-matched diverged
 * checkpoint leg" test drives — seeds the tested runner clone with `{ selfContained: true }`,
 * then proves under the real Codex command sandbox that the clone works standalone (FIXED) or
 * fails reading the bare's borrowed objects (UNFIXED, the pre-m1 `--shared` clone).
 */
async function runSharedCloneSection(
  codex: Record<string, any>,
  launcher: Record<string, any>,
  registry: Record<string, any>,
  runner: Record<string, any>,
  probe: { status: number | null },
): Promise<void> {
  const git = await load("git.js");
  const logMod = await load("log.js");
  const logger = logMod.createLogger("error");

  // Replicate the entrypoint's runner-writable carve-out (agent/templates/entrypoint.sh, "PRD
  // #51 M4: runner-owned /data subtree carve-out"): worker:runner, setgid + sticky, so the
  // runner-uid clone/checkout GitCache performs below can create its own per-run dir under it.
  const runnerRoot = "/data/runner";
  await fs.mkdir(runnerRoot, { recursive: true });
  await fs.chown(runnerRoot, 10001, 10002);
  await fs.chmod(runnerRoot, 0o3775);

  const gitCache = new git.GitCache("/data", logger);
  const scFixed = typeof git.RunnerCloneMaterializationError === "function";
  const scLabel = scFixed ? "FIXED" : "UNFIXED";
  console.log(`SHARED-CLONE MODE: ${scLabel} (src=${src})`);

  // 1. The stand-in "forge": a default branch with a couple of commits.
  const originPath = path.join(root, "shared-origin");
  await fs.mkdir(originPath, { recursive: true });
  await plainGit(originPath, ["init", "-q", "-b", "main"]);
  await fs.writeFile(path.join(originPath, "README.md"), "one\n");
  await plainGit(originPath, ["add", "README.md"]);
  await plainGit(originPath, ["commit", "-q", "-m", "commit 1"]);
  await fs.writeFile(path.join(originPath, "second.txt"), "two\n");
  await plainGit(originPath, ["add", "second.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "commit 2"]);
  const floor = await plainGit(originPath, ["rev-parse", "HEAD"]);

  // 2. The worker's bare, exactly like production (GitCache.reposRoot = <dataDir>/repos).
  const bare = await gitCache.ensureClone(originPath);

  // 3. A wip(park) marker over the pre-advance floor, published as THIS run's own checkpoint
  //    (refs/uzi-checkpoints/<branch>) — the git-materialize.test.ts "owner-matched diverged
  //    checkpoint leg" template, driven through the real GitCache.
  const branch = "agent/issue-shared-clone";
  const seedRunId = "run-shared-a";
  const seed = await gitCache.runnerCloneForBranch(bare, branch, "seed", seedRunId, false);
  await fs.writeFile(path.join(seed.path, "WIP.txt"), "diverged wip\n");
  const marked = await gitCache.commitWipMarker(seed.path);
  assert.equal(marked, true, "the wip(park) marker committed");
  // `seed.path` is RUNNER-owned (GitCache clones/checks out as runner uid), so reading it as the
  // worker-uid fixture process trips git's dubious-ownership check; read it as the runner.
  const markerWrapped = runner.runnerCommand("git", ["-C", seed.path, "rev-parse", "HEAD"]);
  const marker = (await exec(markerWrapped.command, markerWrapped.args, { env: { PATH: "/usr/bin:/bin" } })).stdout.trim();
  await gitCache.fetchAgentBranch(bare, seed.path, branch, seedRunId);
  const packed = await gitCache.checkpointPack(bare, branch);
  if (!packed) throw new Error("checkpointPack returned null — nothing to publish");
  const packBuf = await drain(packed.pack);
  const indexPack = spawnSync("git", ["-C", originPath, "index-pack", "--stdin", "--fix-thin"], {
    input: packBuf,
    env: { PATH: "/usr/bin:/bin" },
  });
  if (indexPack.status !== 0) throw new Error(`index-pack failed: ${indexPack.stderr?.toString()}`);
  await plainGit(originPath, ["update-ref", `refs/uzi-checkpoints/${branch}`, marker]);

  // 4. Advance the origin default branch PAST the checkpoint's parent: the checkpoint marker
  //    now DIVERGES from the new tip (its own parent, `floor`, stays an ancestor of it).
  await fs.writeFile(path.join(originPath, "advance.txt"), "advance\n");
  await plainGit(originPath, ["add", "advance.txt"]);
  await plainGit(originPath, ["commit", "-q", "-m", "advance default"]);
  const advancedTip = await plainGit(originPath, ["rev-parse", "HEAD"]);
  assert.notEqual(advancedTip, floor, "the default branch actually advanced");

  // Refresh the bare: pulls the advanced default AND mirrors refs/uzi-checkpoints/* (fetch()).
  await gitCache.ensureClone(originPath);

  // 5. THE SEED UNDER TEST — a fresh run (a DIFFERENT runId, so NOT ownedHere), owner-matched
  //    via `expectedCheckpointTip`, `resume: false`. `selfContained: true` is always passed: the
  //    base src (no such param) silently ignores it (old `--shared` clone, UNFIXED), the mounted
  //    src (m1) materializes it (FIXED).
  const finalRunId = "run-shared-b";
  const rc = await gitCache.runnerCloneForBranch(bare, branch, "final", finalRunId, false, marker, { selfContained: true });
  check(`SHARED-CLONE ${scLabel}: checkpoint was diverged + owner-matched (wipRecovered)`, rc.wipRecovered === true,
    `wipRecovered=${rc.wipRecovered} seededFrom=${rc.seededFrom} checkpointSetAside=${rc.checkpointSetAside}`);
  check(`SHARED-CLONE ${scLabel}: base is the advanced default tip (checkpoint set aside, not adopted)`, rc.baseCommit === advancedTip,
    `baseCommit=${rc.baseCommit} advancedTip=${advancedTip}`);

  // TODO(#1769 m3 part 2): await sharedCloneFinalizeImport(git, bare, rc.path, branch); once the
  // m2 agent's finalize-import API lands, asserting it works with the bare NOT sandbox-granted.

  // 6. Calibration: DAC allows reading the bare and a SIBLING runner clone from OUTSIDE the
  //    sandbox (runner-cmd, plain setpriv — no Codex supervisor/command sandbox), so a denial
  //    inside the sandbox below is Landlock, never a permissions mistake.
  const asCommandRoot = async (argv: string[], cwd: string): Promise<{ code: number; stdout: string; stderr: string }> => {
    const [cmd, ...rest] = argv;
    const w = runner.commandRootCommand(cmd!, rest);
    try {
      const { stdout } = await exec(w.command, w.args, { cwd, env: { PATH: "/usr/bin:/bin" } });
      return { code: 0, stdout, stderr: "" };
    } catch (err) {
      const e = err as { code?: number; stdout?: string; stderr?: string };
      return { code: e.code ?? 1, stdout: e.stdout ?? "", stderr: e.stderr ?? "" };
    }
  };
  // rc.path is RUNNER-owned (uid 10002); runner-cmd (uid 10003) needs an explicit
  // safe.directory exception to avoid a dubious-ownership false negative unrelated to Landlock.
  const calibStatus = await asCommandRoot(["git", "-c", `safe.directory=${rc.path}`, "status", "--porcelain"], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration git status in the tested clone (outside sandbox)`, calibStatus.code === 0,
    `code=${calibStatus.code} stderr=${calibStatus.stderr}`);
  const calibBare = await asCommandRoot(["ls", path.join(bare, "objects", "pack")], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration ls bare objects/pack (outside sandbox)`, calibBare.code === 0,
    `code=${calibBare.code} stderr=${calibBare.stderr}`);
  const calibSibling = await asCommandRoot(["ls", path.join(seed.path, ".git")], rc.path);
  check(`SHARED-CLONE ${scLabel}: calibration ls sibling clone .git (outside sandbox)`, calibSibling.code === 0,
    `code=${calibSibling.code} stderr=${calibSibling.stderr}`);

  // 7. Under the real Codex command sandbox. Grant git trust (issue #1716, landed on both srcs)
  //    so a "dubious ownership" rejection can never be mistaken for the Landlock denial (#1769)
  //    this section actually tests.
  const sc1716Fixed = typeof codex.withCommandGitTrust === "function" && typeof codex.canonicalCheckoutPath === "function";
  const scCommandEnv: NodeJS.ProcessEnv = sc1716Fixed
    ? codex.withCommandGitTrust(codex.buildCommandEnv("/tmp", {}), await codex.canonicalCheckoutPath(rc.path))
    : codex.buildCommandEnv("/tmp", {});

  const runMode = async (mode: "best-effort" | "required") => {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "required" ? 173 : 172));
    const spawn: Spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, rc.path, scCommandEnv, mode);
    const sh = (script: string) => spawn(["/bin/sh", "-c", script], { cwd: rc.path });
    const tag = `SHARED-CLONE ${scLabel} ${mode}`;

    for (const args of ["status", "diff HEAD", "log --oneline"]) {
      const r = await sh(`git ${args}`);
      check(`${tag}: git ${args}`, r.code === 0, `code=${r.code} stderr=${r.stderr.trim()}`);
    }
    for (const sha of [rc.baseCommit, marker, advancedTip]) {
      const r = await sh(`git cat-file -e ${sha}`);
      check(`${tag}: cat-file -e ${sha.slice(0, 12)}`, r.code === 0, `code=${r.code} stderr=${r.stderr.trim()}`);
    }
    // Checked BEFORE the write+commit below: that commit stages+commits WHATEVER is in the
    // index (including this cherry-pick), so the WIP file is only OBSERVABLY staged pre-commit.
    // On the SECOND mode invocation (required, reusing the same clone) it is already committed
    // by best-effort's own write+commit — check its committed presence there instead.
    if (mode === "best-effort") {
      const staged = await sh("git diff --cached --name-only");
      check(`${tag}: cherry-picked WIP.txt is staged`, staged.code === 0 && staged.stdout.includes("WIP.txt"),
        `code=${staged.code} stdout=${JSON.stringify(staged.stdout)}`);
    } else {
      const committed = await sh("git log --name-only --format= -1 HEAD");
      check(`${tag}: cherry-picked WIP.txt was committed (by the prior best-effort mode)`,
        committed.code === 0 && committed.stdout.includes("WIP.txt"),
        `code=${committed.code} stdout=${JSON.stringify(committed.stdout)}`);
    }

    const commitMsg = `sc-${mode}-${Date.now()}`;
    const commit = await sh(
      `printf '%s\\n' "${commitMsg}" > "sc-${mode}.txt" && git add "sc-${mode}.txt" && git -c user.name=x -c user.email=x@example.com commit -q -m "${commitMsg}"`);
    check(`${tag}: write + git add + git commit`, commit.code === 0, `code=${commit.code} stderr=${commit.stderr.trim()}`);

    // 8. Negative controls, unconditional on FIXED/UNFIXED: the bare and a sibling runner clone
    //    are OUTSIDE the granted root either way — Landlock confines to what the command sandbox
    //    grants, which is only `rc.path` — so a denial here is proof of confinement, not of m1.
    const bareLs = await sh(`ls "${path.join(bare, "objects", "pack")}"`);
    check(`${tag}: ls bare objects/pack is denied`, bareLs.code !== 0, `code=${bareLs.code} stderr=${bareLs.stderr.trim()}`);
    const siblingLs = await sh(`ls "${path.join(seed.path, ".git")}"`);
    check(`${tag}: ls sibling clone .git is denied`, siblingLs.code !== 0, `code=${siblingLs.code} stderr=${siblingLs.stderr.trim()}`);

    check(`${tag}: no live command root`, roots.hasLiveCommandRoot() === false, "a command root is still live");
  };
  await runMode("best-effort");
  if (probe.status === 0) await runMode("required");
  else console.log("SKIP: SHARED-CLONE Codex required mode; sandbox --probe returned 10");
}

async function main(): Promise<void> {
  if (process.env.UZI_UID_SPLIT !== "1" || process.getuid?.() !== 10001) {
    console.log("SKIP: root entrypoint did not establish worker/runner uid split");
    process.exitCode = 77;
    return;
  }
  // Production posture (main.ts): the worker runs with umask 002 under the uid split, and the
  // runner children inherit it, so checkout files are group-`runner` writable for runner-cmd.
  process.umask(0o002);
  const codex = await load("codex/codex-executor.js");
  const launcher = await load("codex/launcher.js");
  const registry = await load("codex/registry.js");
  const runner = await load("runner-uid.js");

  const parent = await fs.stat(path.dirname(root));
  console.log(`OBSERVATION: ${path.dirname(root)} uid=${parent.uid} gid=${parent.gid} mode=${(parent.mode & 0o7777).toString(8)}`);
  await fs.mkdir(root, { recursive: true, mode: 0o2770 });
  // Group `runner` (gid 10002) explicitly, which the worker is a member of: the production clone
  // parent gets it by setgid inheritance from /data/runner, which an older entrypoint may not set.
  await fs.chown(root, 10001, 10002);
  await fs.chmod(root, 0o2770);
  // Checkout A and the nested repository B are both created and owned by the runner (10002),
  // like the production runner clone; B lives inside A's Landlock-permitted subtree, so a
  // rejection there is git's ownership check, not a confinement failure.
  await asRunner(runner.runnerCommand, ["/bin/sh", "-ceu", [
    'git init -q "$1"',
    'chmod 2770 "$1"',
    'printf "one\\n" > "$1/file.txt"',
    'git -C "$1" add file.txt',
    'git -C "$1" -c user.name=runner -c user.email=runner@example.com commit -q -m seed',
    'printf "two\\n" > "$1/file.txt"',
    'git init -q "$2"',
    'printf "b\\n" > "$2/b.txt"',
    'git -C "$2" add b.txt',
    'git -C "$2" -c user.name=runner -c user.email=runner@example.com commit -q -m b',
  ].join("\n"), "sh", checkoutA, nestedB]);
  assert.equal((await fs.stat(checkoutA)).uid, 10002, "checkout A is owned by the runner");
  assert.equal((await fs.stat(nestedB)).uid, 10002, "nested B is owned by the runner");

  const fixed = typeof codex.withCommandGitTrust === "function" && typeof codex.canonicalCheckoutPath === "function";
  const label = fixed ? "FIXED" : "UNFIXED";
  // The same composition run() uses (issue #1716); a base-code src has no trust helper.
  const commandEnv: NodeJS.ProcessEnv = fixed
    ? codex.withCommandGitTrust(codex.buildCommandEnv("/tmp", {}), await codex.canonicalCheckoutPath(checkoutA))
    : codex.buildCommandEnv("/tmp", {});
  console.log(`MODE: ${label} (src=${src}); inline git config: ${JSON.stringify(
    Object.fromEntries(Object.entries(commandEnv).filter(([k]) => k.startsWith("GIT_CONFIG"))))}`);

  const probe = spawnSync("/usr/local/bin/uzi-codex-command-sandbox", ["--probe"], { encoding: "utf8" });
  if (probe.error) throw probe.error;
  if (probe.status !== 0 && probe.status !== 10) throw new Error(`sandbox --probe: ${probe.status}: ${probe.stderr}`);
  console.log(`LANDLOCK: probe=${probe.status}`);
  // Acceptance guard (issue #1769 m3): when the caller demands Landlock (a CI lane that must
  // never silently degrade to unconfined "required" skips), a missing ABI is a hard FAIL, never
  // the exit-77 SKIP a missing Docker/image/uid-split gets.
  if (process.env.CODEX_GIT_TRUST_REQUIRE_LANDLOCK === "1" && probe.status !== 0) {
    console.log(`RESULT: FAIL — CODEX_GIT_TRUST_REQUIRE_LANDLOCK=1 but sandbox --probe returned ${probe.status} (no Landlock ABI)`);
    process.exitCode = 1;
    return;
  }

  let commits = 0;
  const execute = async (mode: "best-effort" | "required") => {
    const roots = new registry.ExecutionRegistry(registry.newLocalExecutionEpoch(mode === "required" ? 171 : 170));
    const spawn: Spawn = codex.makeDefaultSpawnCommand(roots, launcher.launchCodexEffectRoot, 5000, checkoutA, commandEnv, mode);
    const sh = (script: string, cwd = checkoutA) => spawn(["/bin/sh", "-c", script], { cwd });
    const tag = `${label} ${mode}`;

    const id = await sh("id -u");
    check(`${tag}: command identity is runner-cmd`, id.code === 0 && id.stdout.trim() === "10003",
      `code=${id.code} stdout=${JSON.stringify(id.stdout)} stderr=${id.stderr}`);

    for (const args of ["status", "log --oneline", "diff HEAD"]) {
      const r = await sh(`git ${args}`);
      check(`${tag}: git ${args} in A`, r.code === 0, `code=${r.code} ${dubiousLine(r.stderr)} stderr=${r.stderr.trim()}`);
    }

    commits += 1;
    const message = `cmd-${mode}-${commits}`;
    const commit = await sh(
      `printf '%s\\n' "${message}" > "cmd-${mode}.txt" && git add "cmd-${mode}.txt" && git -c user.name=x -c user.email=x@example.com commit -q -m "${message}"`);
    check(`${tag}: write + git add + git commit in A`, commit.code === 0,
      `code=${commit.code} ${dubiousLine(commit.stderr)} stderr=${commit.stderr.trim()}`);
    if (commit.code === 0) {
      let head = "";
      try {
        head = await asRunner(runner.runnerCommand, ["git", "-C", checkoutA, "log", "--oneline", "-1"]);
      } catch (error) {
        head = `ERROR ${(error as Error).message}`;
      }
      check(`${tag}: the owner (runner) sees the command's commit`, head.includes(message), `log -1: ${head.trim()}`);
    }

    const nodeGit = await spawn([process.execPath, "-e",
      'const r = require("node:child_process").spawnSync("git", ["status", "--porcelain"], { encoding: "utf8" }); process.stderr.write(r.stderr ?? ""); process.exit(r.error ? 99 : r.status ?? 98);'],
    { cwd: checkoutA });
    check(`${tag}: node child spawning git status in A`, nodeGit.code === 0,
      `code=${nodeGit.code} ${dubiousLine(nodeGit.stderr)} stderr=${nodeGit.stderr.trim()}`);

    const readB = await sh(`cat "${nestedB}/.git/HEAD" && ls "${nestedB}/.git"`);
    check(`${tag}: B's .git is readable under the sandbox`, readB.code === 0 && readB.stdout.includes("ref:"),
      `code=${readB.code} stderr=${readB.stderr.trim()}`);

    const statusB = await sh(`git -C "${nestedB}" status`);
    check(`${tag}: git -C nested-b status is rejected as dubious ownership`,
      statusB.code !== 0 && statusB.stderr.includes(DUBIOUS), `code=${statusB.code} stderr=${statusB.stderr.trim()}`);

    const control = await sh("git -c safe.directory= status");
    check(`${tag}: negative control git -c safe.directory= status in A is rejected`,
      control.code !== 0 && control.stderr.includes(DUBIOUS), `code=${control.code} stderr=${control.stderr.trim()}`);
    if (control.stderr.includes(DUBIOUS)) console.log(`OBSERVATION: ${tag} control: ${dubiousLine(control.stderr)}`);

    check(`${tag}: no live command root`, roots.hasLiveCommandRoot() === false, "a command root is still live");
  };
  await execute("best-effort");
  if (probe.status === 0) await execute("required");
  else console.log("SKIP: Codex required mode; sandbox --probe returned 10");

  // issue #1769 m3 part 1 — the shared-clone section, its own FIXED/UNFIXED label (whether the
  // mounted src exports RunnerCloneMaterializationError), folded into the same `failures`/exit
  // code so a red before-run is visibly red overall.
  await runSharedCloneSection(codex, launcher, registry, runner, probe);

  if (failures.length > 0) {
    console.log(`RESULT: ${label} FAIL (${failures.length} check(s)): ${failures.join("; ")}`);
    process.exitCode = 1;
  } else {
    console.log(`RESULT: ${label} PASS`);
  }
}
main().catch((error: unknown) => { console.error(error); process.exitCode = 1; });
