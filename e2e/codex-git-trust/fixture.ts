// Opt-in Issue #1716 worker-image fixture: a Codex command (runner-cmd 10003, supervisor +
// command sandbox) runs git in the runner-owned (10002) checkout. Every check runs and prints
// PASS/FAIL, so a base-code run (no withCommandGitTrust, labelled UNFIXED) shows which fail.
import assert from "node:assert/strict";
import { execFile, spawnSync } from "node:child_process";
import fs from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { pathToFileURL } from "node:url";

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

  if (failures.length > 0) {
    console.log(`RESULT: ${label} FAIL (${failures.length} check(s)): ${failures.join("; ")}`);
    process.exitCode = 1;
  } else {
    console.log(`RESULT: ${label} PASS`);
  }
}
main().catch((error: unknown) => { console.error(error); process.exitCode = 1; });
