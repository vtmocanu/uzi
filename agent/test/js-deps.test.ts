import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { chmodSync, mkdirSync, mkdtempSync, symlinkSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  DEFAULT_INSTALL_TIMEOUT_MS,
  depsReadyFor,
  discoverJsProjects,
  execInstall,
  installJsDeps,
  MAX_PROJECT_DIRS,
  MAX_SCAN_DEPTH,
  MAX_SCAN_DIRS,
  type InstallCommand,
  type InstallExec,
} from "../src/js-deps.js";
import { setprivRunnerArgs } from "../src/runner-uid.js";

/** Build a throwaway fixture clone from a {relative path → content} map. */
function mkClone(files: Record<string, string>): string {
  const root = mkdtempSync(join(tmpdir(), "js-deps-"));
  for (const [rel, content] of Object.entries(files)) {
    const abs = join(root, rel);
    mkdirSync(dirname(abs), { recursive: true });
    writeFileSync(abs, content);
  }
  return root;
}

const PKG = '{"name":"x","version":"1.0.0"}';
/** A manifest that DECLARES a dependency — which is what arms the post-install
 *  `node_modules` corroboration. */
const PKG_WITH_DEPS = '{"name":"x","version":"1.0.0","dependencies":{"left-pad":"1.3.0"}}';

/** A recording exec boundary — no real package manager, no registry. */
function recorder(outcome: { ok: boolean; detail: string } = { ok: true, detail: "exit 0" }) {
  const calls: InstallCommand[] = [];
  const exec: InstallExec = async (cmd) => {
    calls.push(cmd);
    return outcome;
  };
  return { calls, exec };
}

/** An exec that "succeeds" AND leaves a node_modules behind, like a real install. */
const execThatInstalls: InstallExec = async (cmd) => {
  mkdirSync(join(cmd.cwd, "node_modules"), { recursive: true });
  return { ok: true, detail: "exit 0" };
};

describe("installJsDeps: the installer is chosen by the lockfile", () => {
  const cases: { name: string; lockfile: string; manager: string; argv: string[] }[] = [
    { name: "npm", lockfile: "package-lock.json", manager: "npm", argv: ["npm", "ci", "--ignore-scripts"] },
    { name: "npm (shrinkwrap)", lockfile: "npm-shrinkwrap.json", manager: "npm", argv: ["npm", "ci", "--ignore-scripts"] },
    {
      name: "pnpm",
      lockfile: "pnpm-lock.yaml",
      manager: "pnpm",
      argv: [
        "pnpm",
        "install",
        "--frozen-lockfile",
        "--ignore-scripts",
        "--ignore-pnpmfile",
        "--config.manage-package-manager-versions=false",
      ],
    },
    {
      name: "yarn",
      lockfile: "yarn.lock",
      manager: "yarn",
      argv: ["yarn", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
    {
      name: "bun (bun.lockb)",
      lockfile: "bun.lockb",
      manager: "bun",
      argv: ["bun", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
    {
      // bun ≥1.2's DEFAULT lockfile, so this is the common case, not the exotic one.
      name: "bun (bun.lock)",
      lockfile: "bun.lock",
      manager: "bun",
      argv: ["bun", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
  ];

  for (const c of cases) {
    it(`${c.name}: ${c.lockfile} → ${c.argv.join(" ")}`, async () => {
      const root = mkClone({ "package.json": PKG, [c.lockfile]: "" });
      const { calls, exec } = recorder();
      const { results, truncated } = await installJsDeps(root, { PATH: "/base/bin" }, { exec });

      assert.equal(calls.length, 1, "exactly one install");
      assert.deepEqual([calls[0]!.command, ...calls[0]!.args], c.argv);
      assert.equal(calls[0]!.cwd, root);
      assert.equal(truncated, false);
      assert.deepEqual(results, [{ dir: ".", manager: c.manager, ok: true, detail: `${c.argv.join(" ")} ok` }]);
      assert.ok(depsReadyFor(results, "."));
    });
  }

  it("installs a nested project in ITS OWN dir, not the clone root", async () => {
    const root = mkClone({ "web/package.json": PKG, "web/package-lock.json": "" });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.cwd, join(root, "web"));
    assert.equal(results[0]!.dir, "web");
    assert.ok(depsReadyFor(results, "web"));
    assert.ok(!depsReadyFor(results, "."), "the root has no package.json here");
  });

  it("prefers the non-npm lockfile when a stale package-lock.json is left behind", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "", "pnpm-lock.yaml": "" });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(results[0]!.manager, "pnpm");
    assert.equal(calls[0]!.command, "pnpm");
  });

  it("prefers npm-shrinkwrap.json over package-lock.json, as npm itself does", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "", "npm-shrinkwrap.json": "" });
    const { projects } = await discoverJsProjects(root);
    assert.equal(projects[0]!.lockfile, "npm-shrinkwrap.json");
    assert.equal(projects[0]!.manager, "npm");
  });
});

// PRD #121 M1 review B1 / audit A1. `--ignore-scripts` alone does NOT stop repo-authored
// code from executing: yarn execs a repo-committed `yarnPath` release BEFORE parsing any
// flag, and pnpm runs a repo `.pnpmfile.cjs` and will fetch+run the pnpm build a repo
// names in `packageManager`. Each flag below closes a MEASURED path; dropping one is a
// security change and moves the PRD's Trust-posture line.
describe("installJsDeps: every measured repo-code-execution path is suppressed", () => {
  it("yarn carries YARN_IGNORE_PATH=1, which is what stops a repo-committed yarnPath exec", async () => {
    const root = mkClone({ "package.json": PKG, "yarn.lock": "" });
    const caller = { PATH: "/tools/bin", HOME: "/home/checks" };
    const { calls, exec } = recorder();
    await installJsDeps(root, caller, { exec });
    assert.equal(
      calls[0]!.env.YARN_IGNORE_PATH,
      "1",
      "without this a repo's .yarnrc(.yml) yarnPath makes yarn exec repo JS before it reads --ignore-scripts",
    );
    // The overlay is exactly one key on top of the caller's env — nothing else added.
    assert.deepEqual(calls[0]!.env, { ...caller, YARN_IGNORE_PATH: "1" });
  });

  it("pnpm carries --ignore-pnpmfile and disables packageManager version handoff", async () => {
    const root = mkClone({ "package.json": PKG, "pnpm-lock.yaml": "" });
    const { calls, exec } = recorder();
    await installJsDeps(root, {}, { exec });
    assert.ok(
      calls[0]!.args.includes("--ignore-pnpmfile"),
      "a repo .pnpmfile.cjs executes under --ignore-scripts; this flag is what stops it",
    );
    assert.ok(
      calls[0]!.args.includes("--config.manage-package-manager-versions=false"),
      "without this pnpm fetches and RUNS the pnpm build the repo names in packageManager",
    );
  });

  it("--ignore-scripts is on EVERY manager's install (settled decision, PRD #121)", async () => {
    for (const lockfile of ["package-lock.json", "pnpm-lock.yaml", "yarn.lock", "bun.lock"]) {
      const root = mkClone({ "package.json": PKG, [lockfile]: "" });
      const { calls, exec } = recorder();
      await installJsDeps(root, {}, { exec });
      assert.ok(calls[0]!.args.includes("--ignore-scripts"), `${lockfile} must suppress lifecycle scripts`);
    }
  });

  it("the env overlay is confined to yarn: npm, pnpm and bun get the caller's env byte-for-byte", async () => {
    // The module otherwise adds, defaults and merges NO environment key. That property
    // was audited and is what makes the single yarn key a considered exception rather
    // than drift, so it is pinned per-manager rather than assumed.
    const caller = { PATH: "/tools/bin", HOME: "/home/checks", GIT_TERMINAL_PROMPT: "0" };
    for (const lockfile of ["package-lock.json", "pnpm-lock.yaml", "bun.lock"]) {
      const root = mkClone({ "package.json": PKG, [lockfile]: "" });
      const { calls, exec } = recorder();
      await installJsDeps(root, caller, { exec });
      assert.deepEqual(calls[0]!.env, caller, `${lockfile}: the install env must be the caller's, unmodified`);
    }
  });
});

describe("installJsDeps: monorepo workspaces resolve to a SINGLE root install", () => {
  it("a root `workspaces` + root lockfile prunes the members", async () => {
    const root = mkClone({
      "package.json": '{"name":"mono","workspaces":["packages/*"]}',
      "package-lock.json": "",
      // Members carrying their own lockfile must NOT be installed separately — the root
      // install covers them, and a member install would fight it.
      "packages/a/package.json": PKG,
      "packages/a/package-lock.json": "",
      "packages/b/package.json": PKG,
      "packages/b/yarn.lock": "",
    });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });

    assert.equal(calls.length, 1, "exactly one install for the whole monorepo");
    assert.equal(calls[0]!.cwd, root);
    assert.deepEqual(results.map((r) => r.dir), ["."]);
  });

  it("a pnpm-workspace.yaml beside the lockfile prunes the members too", async () => {
    const root = mkClone({
      "package.json": PKG, // pnpm declares members in the yaml, not in package.json
      "pnpm-lock.yaml": "",
      "pnpm-workspace.yaml": "packages:\n  - 'packages/*'\n",
      "packages/a/package.json": PKG,
      "packages/a/package-lock.json": "",
    });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
    assert.deepEqual(results.map((r) => r.dir), ["."]);
    assert.equal(results[0]!.manager, "pnpm");
  });

  it("a pnpm workspace root with NO package.json is still installed (measured: pnpm accepts it)", async () => {
    const root = mkClone({
      "pnpm-workspace.yaml": "packages:\n  - 'packages/*'\n",
      "pnpm-lock.yaml": "",
      "packages/a/package.json": PKG,
    });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1, "the one install that would have worked must be attempted");
    assert.equal(calls[0]!.cwd, root);
    assert.deepEqual(results.map((r) => r.dir), ["."]);
    assert.equal(results[0]!.manager, "pnpm");
  });

  it("`workspaces` WITHOUT a root lockfile prunes nothing (there is no root install to do)", async () => {
    const root = mkClone({
      "package.json": '{"name":"mono","workspaces":["packages/*"]}',
      "packages/a/package.json": PKG,
      "packages/a/package-lock.json": "",
    });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.cwd, join(root, "packages/a"));
    const rootResult = results.find((r) => r.dir === ".");
    assert.ok(rootResult && !rootResult.ok && rootResult.detail.includes("no recognized lockfile"));
  });

  it("an EMPTY workspaces declaration prunes nothing — array and object spellings agree", async () => {
    // `[]` and `{}` both declare zero members. Reading one as "no workspaces" and the
    // other as "workspace root" would silently prune a whole subtree on the second.
    for (const ws of ["[]", "{}", '{"packages":[]}']) {
      const root = mkClone({
        "package.json": `{"name":"mono","workspaces":${ws}}`,
        "package-lock.json": "",
        "packages/a/package.json": PKG,
        "packages/a/package-lock.json": "",
      });
      const { projects } = await discoverJsProjects(root);
      assert.equal(projects[0]!.workspaceRoot, false, `workspaces: ${ws} declares nothing, so it is not a workspace root`);
      assert.deepEqual(projects.map((p) => p.dir), [".", "packages/a"], `workspaces: ${ws} must not prune the subtree`);
    }
  });
});

describe("installJsDeps: a package.json with no lockfile is an honest skip, never a guess", () => {
  it("reports the dir, installs nothing, and never spawns", async () => {
    const root = mkClone({ "package.json": PKG });
    const { calls, exec } = recorder();
    const { results } = await installJsDeps(root, {}, { exec });

    assert.equal(calls.length, 0, "no lockfile ⇒ no install may be attempted");
    assert.deepEqual(results, [
      { dir: ".", manager: "none", ok: false, detail: "package.json but no recognized lockfile — not installed" },
    ]);
    assert.ok(!depsReadyFor(results, "."));
  });
});

describe("installJsDeps: an install failure is an honest skip, never a fabricated success", () => {
  it("a non-zero exit is reported ok:false with the reason, and does not stop the other dirs", async () => {
    const root = mkClone({
      "agent/package.json": PKG,
      "agent/package-lock.json": "",
      "web/package.json": PKG,
      "web/package-lock.json": "",
    });
    const calls: InstallCommand[] = [];
    const exec: InstallExec = async (cmd) => {
      calls.push(cmd);
      return cmd.cwd.endsWith("/web") ? { ok: false, detail: "exit 1" } : { ok: true, detail: "exit 0" };
    };
    const { results } = await installJsDeps(root, {}, { exec });

    assert.equal(calls.length, 2, "the failure must not abort the remaining dirs");
    const web = results.find((r) => r.dir === "web");
    const agent = results.find((r) => r.dir === "agent");
    assert.ok(web && web.ok === false, "a failed install must never report ok");
    assert.match(web.detail, /failed \(exit 1\)/);
    assert.match(web.detail, /node_modules absent/);
    assert.ok(agent && agent.ok === true);
    assert.equal(depsReadyFor(results, "web"), false);
    assert.equal(depsReadyFor(results, "agent"), true);
  });

  it("an exec that THROWS is caught and recorded as a skip — the module never throws out", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const exec: InstallExec = async () => {
      throw new Error("spawn EACCES");
    };
    const { results } = await installJsDeps(root, {}, { exec });
    assert.equal(results.length, 1);
    assert.equal(results[0]!.ok, false);
    assert.match(results[0]!.detail, /could not run: spawn EACCES/);
  });

  it("a clone with nothing installable yields no results and no spawns", async () => {
    const root = mkClone({ "README.md": "# no js here" });
    const { calls, exec } = recorder();
    assert.deepEqual(await installJsDeps(root, {}, { exec }), { results: [], truncated: false });
    assert.equal(calls.length, 0);
  });
});

// `ok` is the signal a caller — and, later, gate honesty — reads as "the deps are there".
// An exit code alone does not establish that.
describe("installJsDeps: a claimed success is corroborated against node_modules", () => {
  it("downgrades exit 0 to a failure when the project declares deps but nothing was installed", async () => {
    const root = mkClone({ "package.json": PKG_WITH_DEPS, "package-lock.json": "" });
    const { results } = await installJsDeps(root, {}, { exec: recorder().exec }); // "succeeds", creates nothing
    assert.equal(results[0]!.ok, false, "a repo that can force exit 0 must not be able to mint a false deps-ready");
    assert.match(results[0]!.detail, /left no node_modules/);
    assert.equal(depsReadyFor(results, "."), false);
  });

  it("reports ok when the install actually produced node_modules", async () => {
    const root = mkClone({ "package.json": PKG_WITH_DEPS, "package-lock.json": "" });
    const { results } = await installJsDeps(root, {}, { exec: execThatInstalls });
    assert.equal(results[0]!.ok, true);
    assert.ok(depsReadyFor(results, "."));
  });

  it("does NOT downgrade a zero-dependency project, which legitimately gets no node_modules", async () => {
    // Measured: `npm ci` on a package.json declaring no dependencies exits 0 and creates
    // no node_modules at all. Downgrading that would fabricate a failure — the same lie
    // as a false success, pointing the other way.
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const { results } = await installJsDeps(root, {}, { exec: recorder().exec });
    assert.equal(results[0]!.ok, true, "a project with no declared dependencies needs no node_modules to be ready");
  });

  it("does NOT downgrade when the manifest is unreadable, since dependencies are then unknown", async () => {
    const root = mkClone({ "package.json": "{not json", "package-lock.json": "" });
    const { results } = await installJsDeps(root, {}, { exec: recorder().exec });
    assert.equal(results[0]!.ok, true, "unknown must not be treated as a positive in either direction");
  });
});

describe("execInstall (the real exec boundary): status only, output never", () => {
  const root = mkdtempSync(join(tmpdir(), "js-deps-exec-"));
  const base = { cwd: root, env: { PATH: process.env.PATH }, timeoutMs: 5000 };

  it("maps exit 0 to ok", async () => {
    assert.deepEqual(await execInstall({ ...base, command: "sh", args: ["-c", "exit 0"] }), { ok: true, detail: "exit 0" });
  });

  it("maps a non-zero exit to a reported failure", async () => {
    assert.deepEqual(await execInstall({ ...base, command: "sh", args: ["-c", "exit 1"] }), { ok: false, detail: "exit 1" });
  });

  it("maps a missing package manager (ENOENT) to a named failure, not a crash", async () => {
    assert.deepEqual(await execInstall({ ...base, command: "no-such-pm-xyz", args: ["install"] }), {
      ok: false,
      detail: "package manager not available in the worker",
    });
  });

  it("maps the wall-clock cap to `timed out`", async () => {
    const r = await execInstall({ ...base, command: "sh", args: ["-c", "sleep 5"], timeoutMs: 150 });
    assert.deepEqual(r, { ok: false, detail: "timed out" });
  });

  it("reports an aborted install as `cancelled`, not as an invented exit code", async () => {
    const ac = new AbortController();
    const p = execInstall({ ...base, command: "sh", args: ["-c", "sleep 30"], signal: ac.signal });
    setTimeout(() => ac.abort(), 50);
    const started = Date.now();
    assert.deepEqual(await p, { ok: false, detail: "cancelled" });
    assert.ok(Date.now() - started < 4000, "an aborted install must return promptly, not run to its timeout");
  });

  it("returns immediately when the signal is ALREADY aborted at spawn time", async () => {
    const ac = new AbortController();
    ac.abort();
    const r = await execInstall({ ...base, command: "sh", args: ["-c", "sleep 30"], signal: ac.signal });
    assert.deepEqual(r, { ok: false, detail: "cancelled" });
  });

  it("captures ONLY the exit status — install output never reaches the result", async () => {
    const secret = "sk-ant-api03-NEVER-IN-A-LOG";
    const r = await execInstall({
      ...base,
      command: "sh",
      args: ["-c", `echo ${secret}; echo ${secret} >&2; exit 3`],
    });
    assert.equal(r.ok, false);
    assert.ok(!JSON.stringify(r).includes(secret), "install output must never reach the result");
  });
});

describe("discoverJsProjects: node_modules is excluded ANYWHERE in the path", () => {
  it("a package.json + lockfile inside node_modules is never a project", async () => {
    const root = mkClone({
      "package.json": PKG,
      "package-lock.json": "",
      "node_modules/dep/package.json": PKG,
      "node_modules/dep/package-lock.json": "",
      "node_modules/dep/node_modules/deeper/package.json": PKG,
      "node_modules/dep/node_modules/deeper/yarn.lock": "",
      "web/node_modules/dep/package.json": PKG,
      "web/node_modules/dep/pnpm-lock.yaml": "",
      "web/package.json": PKG,
      "web/package-lock.json": "",
    });
    const { projects } = await discoverJsProjects(root);
    assert.deepEqual(projects.map((p) => p.dir).sort(), [".", "web"]);
    for (const p of projects) {
      assert.ok(!p.dir.includes("node_modules"), `node_modules leaked into discovery: ${p.dir}`);
    }

    const { calls, exec } = recorder();
    await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 2);
    for (const c of calls) {
      assert.ok(!c.cwd.includes("node_modules"), `an install was attempted inside node_modules: ${c.cwd}`);
    }
  });
});

// The walk must not be able to leave the clone. A repo can commit a symlink to anywhere;
// the only thing stopping the walk from following it is that `Dirent.isDirectory()` is
// lstat-based, so a symlink is never enqueued as a directory. A refactor to
// `readdir` + `stat`, or to `{ recursive: true }`, silently reopens this.
describe("discoverJsProjects: symlinks are not followed, so no install cwd escapes the clone", () => {
  it("a symlinked directory holding a real project is not discovered", async () => {
    const outside = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const root = mkClone({ "web/package.json": PKG, "web/package-lock.json": "" });
    symlinkSync(outside, join(root, "escape"));
    symlinkSync("..", join(root, "up"));
    symlinkSync(".", join(root, "loop"));

    const { projects } = await discoverJsProjects(root);
    assert.deepEqual(
      projects.map((p) => p.dir),
      ["web"],
      "a symlink must never be walked — following one would let an install cwd land outside the clone",
    );

    const { calls, exec } = recorder();
    await installJsDeps(root, {}, { exec });
    for (const c of calls) {
      assert.ok(c.cwd.startsWith(root), `install cwd escaped the clone: ${c.cwd}`);
    }
  });
});

describe("discoverJsProjects: the search is BOUNDED, and says so when it stops early", () => {
  it(`stops at MAX_PROJECT_DIRS (${MAX_PROJECT_DIRS}) and reports truncated`, async () => {
    const files: Record<string, string> = {};
    for (let i = 0; i < MAX_PROJECT_DIRS + 8; i++) {
      const d = `p${String(i).padStart(2, "0")}`;
      files[`${d}/package.json`] = PKG;
      files[`${d}/package-lock.json`] = "";
    }
    const root = mkClone(files);

    const { projects, truncated } = await discoverJsProjects(root);
    assert.equal(projects.length, MAX_PROJECT_DIRS, "discovery must cap the number of project dirs");
    assert.equal(truncated, true, "a capped scan that reads as full coverage is the lie this flag exists to stop");
    assert.equal(projects[0]!.dir, "p00");

    const install = await installJsDeps(root, {}, { exec: recorder().exec });
    assert.equal(install.results.length, MAX_PROJECT_DIRS);
    assert.equal(install.truncated, true, "truncation must survive to the caller that logs the outcome");
  });

  it(`stops at MAX_SCAN_DIRS (${MAX_SCAN_DIRS}) on a wide, flat tree with no projects at all`, async () => {
    // Neither other bound catches this shape: nothing here is a project (so
    // MAX_PROJECT_DIRS never trips) and everything is at depth 1 (so MAX_SCAN_DEPTH never
    // trips). Only the read cap ends the walk.
    const root = mkdtempSync(join(tmpdir(), "js-deps-wide-"));
    for (let i = 0; i < MAX_SCAN_DIRS + 20; i++) mkdirSync(join(root, `d${i}`));
    const { projects, truncated } = await discoverJsProjects(root);
    assert.deepEqual(projects, []);
    assert.equal(truncated, true, "the walk stopped on its read cap with directories left unexamined");
  });

  it(`stops at MAX_SCAN_DEPTH (${MAX_SCAN_DEPTH}): a project deeper than that is not discovered`, async () => {
    const atLimit = "a/b/c/d";
    const tooDeep = "a/b/c/d/e";
    const root = mkClone({
      [`${atLimit}/package.json`]: PKG,
      [`${atLimit}/package-lock.json`]: "",
      [`${tooDeep}/package.json`]: PKG,
      [`${tooDeep}/package-lock.json`]: "",
    });
    const { projects, truncated } = await discoverJsProjects(root);
    assert.deepEqual(projects.map((p) => p.dir), [atLimit]);
    // Depth is a standing policy about which trees are in scope, not a "we ran out"
    // signal — folding it into `truncated` would make the flag fire on ordinary repos.
    assert.equal(truncated, false, "a depth-bounded walk is not a truncated one");
  });

  it("an ordinary repo reports truncated:false", async () => {
    const root = mkClone({ "web/package.json": PKG, "web/package-lock.json": "" });
    assert.equal((await discoverJsProjects(root)).truncated, false);
  });

  it("an unreadable directory is skipped, never thrown", async () => {
    const root = mkClone({ "web/package.json": PKG, "web/package-lock.json": "" });
    const locked = join(root, "locked");
    mkdirSync(locked);
    mkdirSync(join(locked, "inner"));
    chmodSync(locked, 0o000);
    try {
      const { projects } = await discoverJsProjects(root);
      assert.deepEqual(projects.map((p) => p.dir), ["web"], "the readable side of the tree must still be discovered");
    } finally {
      chmodSync(locked, 0o755);
    }
  });
});

// PRD #121 M2 collapsed self-improve's hardcoded `["web", "agent"]` dir list into this
// discovery. A self_improve run clones THIS repo, and SELF_IMPROVE_CHECKS pre-flight on
// `web/node_modules` and `agent/node_modules`.
//
// Asserted as a SUPERSET, never an exact roster: adding a third JS dir breaks nothing
// (discovery would just install three), and this repo has already paid once for pinning
// its own shape — `repoagents.test.ts` pinned `.claude/agents/` by name and reddened the
// day the dev team gained a role.
describe("discoverJsProjects over uzi's own repo (the self-improve check phase depends on this)", () => {
  it("resolves web/ and agent/ as npm projects", async () => {
    const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
    const { projects } = await discoverJsProjects(repoRoot);
    const byDir = new Map(projects.map((p) => [p.dir, p]));
    for (const dir of ["web", "agent"]) {
      const p = byDir.get(dir);
      // The hazard this catches is NOT "someone renamed a directory". It is a root
      // package.json with `workspaces` appearing: discovery would then prune to a single
      // root install, that install would SUCCEED, and npm workspaces hoist — leaving no
      // web/node_modules and no agent/node_modules. All four npm checks would then
      // honest-skip, and the MR's "N of M checks were SKIPPED" banner would be read as a
      // missing toolchain. Nothing would look broken.
      assert.ok(
        p,
        `discovery no longer finds ${dir}/, where SELF_IMPROVE_CHECKS runs: the post-agent check phase will honest-skip and self-improvement MRs will silently lose their JS test evidence`,
      );
      assert.equal(p.manager, "npm", `${dir}/ must resolve to npm — its lockfile is package-lock.json`);
    }
  });
});

describe("installJsDeps: the sandbox is UNCHANGED (runner uid + the caller's scrubbed env)", () => {
  it("wraps the install in the setpriv → runner argv when the uid split is active", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const prev = process.env.UZI_UID_SPLIT;
    process.env.UZI_UID_SPLIT = "1";
    try {
      const { calls, exec } = recorder();
      await installJsDeps(root, {}, { exec });
      assert.equal(calls[0]!.command, "/bin/setpriv");
      assert.deepEqual(calls[0]!.args, [...setprivRunnerArgs(), "npm", "ci", "--ignore-scripts"]);
      // Pinned INDEPENDENTLY of setprivRunnerArgs(): the deepEqual above compares against
      // that function's own output, so it would follow a regression inside it rather than
      // catch one. Every flag of the uid/cap drop is named here.
      for (const flag of ["--reuid", "--regid", "--init-groups", "--bounding-set", "--inh-caps", "--ambient-caps"]) {
        assert.ok(calls[0]!.args.includes(flag), `${flag} must survive into the install spawn`);
      }
    } finally {
      if (prev === undefined) delete process.env.UZI_UID_SPLIT;
      else process.env.UZI_UID_SPLIT = prev;
    }
  });

  it("runs the command directly on a single-uid (#58) start", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const prev = process.env.UZI_UID_SPLIT;
    delete process.env.UZI_UID_SPLIT;
    try {
      const { calls, exec } = recorder();
      await installJsDeps(root, {}, { exec });
      assert.equal(calls[0]!.command, "npm");
      assert.deepEqual(calls[0]!.args, ["ci", "--ignore-scripts"]);
    } finally {
      if (prev !== undefined) process.env.UZI_UID_SPLIT = prev;
    }
  });

  it("passes the caller's env through UNCHANGED — it never spreads process.env", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const scrubbed: NodeJS.ProcessEnv = { PATH: "/tools/bin", HOME: "/home/checks", GIT_TERMINAL_PROMPT: "0" };
    const prevToken = process.env.UZI_WORKER_TOKEN;
    process.env.UZI_WORKER_TOKEN = "join-token-SECRET";
    try {
      const { calls, exec } = recorder();
      await installJsDeps(root, scrubbed, { exec });
      // One assertion, not two: a follow-up `env.UZI_WORKER_TOKEN === undefined` cannot
      // fail once this deepEqual passes, and a check that cannot fail is not a check.
      assert.deepEqual(calls[0]!.env, scrubbed);
    } finally {
      if (prevToken === undefined) delete process.env.UZI_WORKER_TOKEN;
      else process.env.UZI_WORKER_TOKEN = prevToken;
    }
  });

  it("threads the abort signal to the exec boundary and skips the dirs it never reached", async () => {
    const root = mkClone({
      "a/package.json": PKG,
      "a/package-lock.json": "",
      "b/package.json": PKG,
      "b/package-lock.json": "",
    });
    const ac = new AbortController();
    const seen: (AbortSignal | undefined)[] = [];
    let spawns = 0;
    // The fixture returns an ORDINARY failure, not the cancel sentinel — so dir b's
    // "cancelled" wording can only come from the module's own pre-spawn abort check.
    const exec: InstallExec = async (cmd) => {
      spawns++;
      seen.push(cmd.signal);
      ac.abort();
      return { ok: false, detail: "exit 1" };
    };
    const { results } = await installJsDeps(root, {}, { exec, signal: ac.signal });

    assert.equal(spawns, 1, "the dir after the abort must not be spawned");
    assert.equal(seen[0], ac.signal, "the exec boundary must receive the caller's signal");
    assert.equal(results.length, 2, "every discovered dir is still reported");
    const a = results.find((r) => r.dir === "a")!;
    const b = results.find((r) => r.dir === "b")!;
    assert.match(a.detail, /failed \(exit 1\)/, "the dir that actually ran reports its real failure");
    assert.match(b.detail, /cancelled — node_modules absent/, "the unreached dir is reported cancelled");
    assert.ok(!/failed \(/.test(b.detail), "a cancel must not be dressed up as an install failure");
  });

  it("defaults the per-install timeout, and honours an override", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const d = recorder();
    await installJsDeps(root, {}, { exec: d.exec });
    assert.equal(d.calls[0]!.timeoutMs, DEFAULT_INSTALL_TIMEOUT_MS);

    const o = recorder();
    await installJsDeps(root, {}, { exec: o.exec, timeoutMs: 1234 });
    assert.equal(o.calls[0]!.timeoutMs, 1234);
  });
});
