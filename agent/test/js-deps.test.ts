import { describe, it } from "node:test";
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  depsReadyFor,
  discoverJsProjects,
  execInstall,
  installJsDeps,
  MAX_PROJECT_DIRS,
  MAX_SCAN_DEPTH,
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

/** A recording exec boundary — no real package manager, no registry. */
function recorder(outcome: { ok: boolean; detail: string } = { ok: true, detail: "exit 0" }) {
  const calls: InstallCommand[] = [];
  const exec: InstallExec = async (cmd) => {
    calls.push(cmd);
    return outcome;
  };
  return { calls, exec };
}

describe("installJsDeps: the installer is chosen by the lockfile", () => {
  const cases: { name: string; lockfile: string; manager: string; argv: string[] }[] = [
    { name: "npm", lockfile: "package-lock.json", manager: "npm", argv: ["npm", "ci", "--ignore-scripts"] },
    {
      name: "pnpm",
      lockfile: "pnpm-lock.yaml",
      manager: "pnpm",
      argv: ["pnpm", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
    {
      name: "yarn",
      lockfile: "yarn.lock",
      manager: "yarn",
      argv: ["yarn", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
    {
      name: "bun",
      lockfile: "bun.lockb",
      manager: "bun",
      argv: ["bun", "install", "--frozen-lockfile", "--ignore-scripts"],
    },
  ];

  for (const c of cases) {
    it(`${c.name}: ${c.lockfile} → ${c.argv.join(" ")}`, async () => {
      const root = mkClone({ "package.json": PKG, [c.lockfile]: "" });
      const { calls, exec } = recorder();
      const results = await installJsDeps(root, { PATH: "/base/bin" }, { exec });

      assert.equal(calls.length, 1, "exactly one install");
      assert.deepEqual([calls[0]!.command, ...calls[0]!.args], c.argv);
      assert.equal(calls[0]!.cwd, root);
      assert.deepEqual(results, [
        { dir: ".", manager: c.manager, ok: true, detail: `${c.argv.join(" ")} ok` },
      ]);
      assert.ok(depsReadyFor(results, "."));
    });
  }

  it("--ignore-scripts is on EVERY manager's install (settled decision, PRD #121)", async () => {
    for (const c of cases) {
      const root = mkClone({ "package.json": PKG, [c.lockfile]: "" });
      const { calls, exec } = recorder();
      await installJsDeps(root, {}, { exec });
      assert.ok(calls[0]!.args.includes("--ignore-scripts"), `${c.name} must suppress lifecycle scripts`);
    }
  });

  it("installs a nested project in ITS OWN dir, not the clone root", async () => {
    const root = mkClone({
      "web/package.json": PKG,
      "web/package-lock.json": "",
    });
    const { calls, exec } = recorder();
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.cwd, join(root, "web"));
    assert.equal(results[0]!.dir, "web");
    assert.ok(depsReadyFor(results, "web"));
    assert.ok(!depsReadyFor(results, "."), "the root has no package.json here");
  });

  it("prefers the non-npm lockfile when a stale package-lock.json is left behind", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "", "pnpm-lock.yaml": "" });
    const { calls, exec } = recorder();
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(results[0]!.manager, "pnpm");
    assert.equal(calls[0]!.command, "pnpm");
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
    const results = await installJsDeps(root, {}, { exec });

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
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
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
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]!.cwd, join(root, "packages/a"));
    // The root is still reported, honestly, as un-installable.
    const rootResult = results.find((r) => r.dir === ".");
    assert.ok(rootResult && !rootResult.ok && rootResult.detail.includes("no recognized lockfile"));
  });
});

describe("installJsDeps: a package.json with no lockfile is an honest skip, never a guess", () => {
  it("reports the dir, installs nothing, and never spawns", async () => {
    const root = mkClone({ "package.json": PKG });
    const { calls, exec } = recorder();
    const results = await installJsDeps(root, {}, { exec });

    assert.equal(calls.length, 0, "no lockfile ⇒ no install may be attempted");
    assert.deepEqual(results, [
      {
        dir: ".",
        manager: "none",
        ok: false,
        detail: "package.json but no recognized lockfile — not installed",
      },
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
    // web/ fails, agent/ succeeds — a failure must not abort the sweep.
    const exec: InstallExec = async (cmd) => {
      calls.push(cmd);
      return cmd.cwd.endsWith("/web") ? { ok: false, detail: "exit 1" } : { ok: true, detail: "exit 0" };
    };
    const results = await installJsDeps(root, {}, { exec });

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
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(results.length, 1);
    assert.equal(results[0]!.ok, false);
    assert.match(results[0]!.detail, /could not run: spawn EACCES/);
  });

  it("a clone with nothing installable yields no results and no spawns", async () => {
    const root = mkClone({ "README.md": "# no js here" });
    const { calls, exec } = recorder();
    assert.deepEqual(await installJsDeps(root, {}, { exec }), []);
    assert.equal(calls.length, 0);
  });
});

describe("execInstall (the real exec boundary): status only, output never", () => {
  const root = mkdtempSync(join(tmpdir(), "js-deps-exec-"));
  const base = { cwd: root, env: { PATH: process.env.PATH }, timeoutMs: 5000 };

  it("maps exit 0 to ok", async () => {
    assert.deepEqual(await execInstall({ ...base, command: "sh", args: ["-c", "exit 0"] }), {
      ok: true,
      detail: "exit 0",
    });
  });

  it("maps a non-zero exit to a reported failure", async () => {
    assert.deepEqual(await execInstall({ ...base, command: "sh", args: ["-c", "exit 1"] }), {
      ok: false,
      detail: "exit 1",
    });
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
    const r = await p;
    assert.deepEqual(r, { ok: false, detail: "cancelled" });
    // The point of aborting is NOT waiting out the wall-clock cap.
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
      // A dependency's own package.json + lockfile, at the root and nested.
      "node_modules/dep/package.json": PKG,
      "node_modules/dep/package-lock.json": "",
      "node_modules/dep/node_modules/deeper/package.json": PKG,
      "node_modules/dep/node_modules/deeper/yarn.lock": "",
      "web/node_modules/dep/package.json": PKG,
      "web/node_modules/dep/pnpm-lock.yaml": "",
      "web/package.json": PKG,
      "web/package-lock.json": "",
    });
    const projects = await discoverJsProjects(root);
    assert.deepEqual(projects.map((p) => p.dir).sort(), [".", "web"]);
    for (const p of projects) {
      assert.ok(!p.dir.includes("node_modules"), `node_modules leaked into discovery: ${p.dir}`);
    }

    // And nothing under node_modules is ever installed.
    const { calls, exec } = recorder();
    await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, 2);
    for (const c of calls) {
      assert.ok(!c.cwd.includes("node_modules"), `an install was attempted inside node_modules: ${c.cwd}`);
    }
  });
});

describe("discoverJsProjects: the search is BOUNDED (a huge repo must not melt the worker)", () => {
  it(`stops at MAX_PROJECT_DIRS (${MAX_PROJECT_DIRS}) however many projects exist`, async () => {
    const files: Record<string, string> = {};
    const total = MAX_PROJECT_DIRS + 8;
    for (let i = 0; i < total; i++) {
      const d = `p${String(i).padStart(2, "0")}`;
      files[`${d}/package.json`] = PKG;
      files[`${d}/package-lock.json`] = "";
    }
    const root = mkClone(files);

    const projects = await discoverJsProjects(root);
    assert.equal(projects.length, MAX_PROJECT_DIRS, "discovery must cap the number of project dirs");
    // Root-most / lexicographically first are the ones kept (deterministic BFS order).
    assert.equal(projects[0]!.dir, "p00");

    // The cap is what bounds provisioning wall-clock: at most MAX_PROJECT_DIRS installs.
    const { calls, exec } = recorder();
    const results = await installJsDeps(root, {}, { exec });
    assert.equal(calls.length, MAX_PROJECT_DIRS);
    assert.equal(results.length, MAX_PROJECT_DIRS);
  });

  it(`stops at MAX_SCAN_DEPTH (${MAX_SCAN_DEPTH}): a project deeper than that is not discovered`, async () => {
    const atLimit = "a/b/c/d"; // depth 4
    const tooDeep = "a/b/c/d/e"; // depth 5
    const root = mkClone({
      [`${atLimit}/package.json`]: PKG,
      [`${atLimit}/package-lock.json`]: "",
      [`${tooDeep}/package.json`]: PKG,
      [`${tooDeep}/package-lock.json`]: "",
    });
    const projects = await discoverJsProjects(root);
    assert.deepEqual(projects.map((p) => p.dir), [atLimit]);
  });
});

// PRD #121 M2 collapsed self-improve's hardcoded `["web", "agent"]` dir list into this
// discovery. A self_improve run clones THIS repo, and SELF_IMPROVE_CHECKS pre-flight on
// `web/node_modules` and `agent/node_modules` — so if discovery ever stops resolving
// those two dirs, every npm check in a self-improvement MR silently reports "skipped"
// instead of running. Asserted as a SUPERSET (never an exact roster), so adding a new JS
// dir to the repo is free; what must not change silently is these two disappearing.
describe("discoverJsProjects over uzi's own repo (the self-improve path depends on this)", () => {
  it("resolves web/ and agent/ as npm projects", async () => {
    const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..");
    const projects = await discoverJsProjects(repoRoot);
    const byDir = new Map(projects.map((p) => [p.dir, p]));
    for (const dir of ["web", "agent"]) {
      const p = byDir.get(dir);
      assert.ok(p, `${dir}/ must still be discovered — self-improve's npm checks pre-flight on its node_modules`);
      assert.equal(p.manager, "npm", `${dir}/ must resolve to npm (its lockfile is package-lock.json)`);
    }
    // A root install would NOT create web/node_modules + agent/node_modules the way the
    // per-dir installs do, so a root package.json appearing here is a real regression
    // for the self-improve checks, not a cosmetic change.
    assert.ok(!byDir.has("."), "the repo root has no package.json; a root install would not satisfy the per-dir checks");
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
      // The cap-clearing flags are the load-bearing part of the wrapper.
      for (const flag of ["--reuid", "--inh-caps", "--ambient-caps"]) {
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
      assert.deepEqual(calls[0]!.env, scrubbed);
      assert.equal(calls[0]!.env.UZI_WORKER_TOKEN, undefined, "the join token must be absent by construction");
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
    // The first dir observes the signal and aborts mid-sweep; the second must then be
    // reported cancelled without ever being spawned.
    let spawns = 0;
    const exec: InstallExec = async (cmd) => {
      spawns++;
      seen.push(cmd.signal);
      ac.abort();
      return { ok: false, detail: "cancelled" };
    };
    const results = await installJsDeps(root, {}, { exec, signal: ac.signal });

    assert.equal(spawns, 1, "the dir after the abort must not be spawned");
    assert.equal(seen[0], ac.signal, "the exec boundary must receive the caller's signal");
    assert.equal(results.length, 2, "every discovered dir is still reported");
    for (const r of results) {
      assert.equal(r.ok, false);
      assert.match(r.detail, /cancelled — node_modules absent/);
      assert.ok(!/failed \(/.test(r.detail), "a cancel must not be reported as an install failure");
    }
  });

  it("honours an overridden per-install timeout", async () => {
    const root = mkClone({ "package.json": PKG, "package-lock.json": "" });
    const { calls, exec } = recorder();
    await installJsDeps(root, {}, { exec, timeoutMs: 1234 });
    assert.equal(calls[0]!.timeoutMs, 1234);
  });
});
