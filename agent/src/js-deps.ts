// PRD #121 M1 — package-manager-aware, lockfile-driven JS dependency provisioning for
// a runner clone.
//
// A worker seeds a FRESH clone per run, so the target repo's `node_modules` is absent
// when the agent starts and its first gate command (`npm test`, `vitest`, `tsc`) dies
// with `command not found`. `prepareCheckDeps` (self-improve.ts) already solves that —
// but only for `self_improve` runs, only AFTER the agent finished, and only for uzi's
// own hardcoded `["web", "agent"]` layout. This module is that routine RELOCATED and
// GENERALIZED: same sandbox, same flags, same best-effort/honest-skip contract, now
// driven by whatever lockfiles the cloned repo actually has.
//
// SANDBOX — UNCHANGED, and deliberately so. Every install subprocess goes through
// `runnerCommand` (setpriv → the cap-less `runner` uid under the PRD #51 A1 split;
// direct on a #58 single-uid start) and takes the SCRUBBED REPLACEMENT env the caller
// built with `buildCheckEnv`. Read the security comment block above `buildCheckEnv` in
// self-improve.ts before touching any of that: the worker holds the decrypted forge PAT
// + the user's Anthropic token and its own env carries the join token, none of which may
// ever reach repo-authored install code. This module widens WHICH dirs get installed; it
// widens nothing about who runs the install or what it can see.
//
// `--ignore-scripts` IS SETTLED (PRD #121 "install-flags decision"). An earlier draft
// argued for a full install on the theory that `--ignore-scripts` leaves `esbuild`
// unbuilt; that premise was tested and DISPROVEN (esbuild ≥0.16 ships its platform
// binary via `optionalDependencies`, so a `--ignore-scripts` install yields a runnable
// esbuild). Keeping the flag is also what lets provisioning run PRE-APPROVAL without a
// new opt-in gate: a frozen `--ignore-scripts` install resolves the lockfile and unpacks
// published tarballs, and runs NO repo-authored script — the same bar `repo_devbox_opt_in`
// (migration 00047) holds devbox `init_hook`s to. Dropping it breaks that reasoning and
// makes auto-install opt-in. Do not "improve" it.

import { execFile } from "node:child_process";
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { runnerCommand } from "./runner-uid.js";

/** The package managers whose lockfile we recognize. */
export type JsPackageManager = "npm" | "pnpm" | "yarn" | "bun";

// ─── Discovery bounds (a hostile/huge repo must not melt the worker) ──────────
// Discovery walks a repo the user controls. All three bounds exist so a pathological
// tree costs a fixed, small amount of work instead of an unbounded one; each is
// enforced independently because each caps a different runaway.

/** Max directory depth below the clone root that discovery descends. Real JS layouts
 *  put their projects shallow (`web/`, `packages/<name>/`, `apps/<name>/`); a
 *  package.json at depth >4 is far more likely to be a test fixture or a vendored copy
 *  than a project whose deps the agent's gates need. Caps the walk's BREADTH-per-level
 *  blowup. */
export const MAX_SCAN_DEPTH = 4;

/** Max project dirs discovery reports (and therefore max installs attempted). Each
 *  install is a subprocess with its own wall-clock cap, so this is what bounds total
 *  provisioning time: worst case MAX_PROJECT_DIRS × timeoutMs. A repo with 400 nested
 *  package.json files gets its first 12 (root-most first — see the BFS order below),
 *  not 400 subprocesses. */
export const MAX_PROJECT_DIRS = 12;

/** Max directories READ during the walk. The two bounds above do not cap this on their
 *  own: a repo with 50k directories at depth ≤4 and no package.json anywhere would still
 *  readdir() all of them. This caps the walk itself. */
export const MAX_SCAN_DIRS = 2000;

/** Per-install wall-clock cap. Matches `prepareCheckDeps`' default; overridable. */
export const DEFAULT_INSTALL_TIMEOUT_MS = 10 * 60 * 1000;

/** Directories never descended into. `node_modules` is the requirement (a dependency's
 *  own package.json + lockfile is not a project of this repo, and one `node_modules`
 *  can hold thousands); `.git` is pure walk cost with nothing installable inside. */
const SKIP_DIRS = new Set(["node_modules", ".git"]);

// LOCKFILES maps a lockfile to its package manager, IN PRECEDENCE ORDER — a dir with
// more than one takes the first match. npm's is last deliberately: a repo that migrated
// to pnpm/yarn/bun commonly leaves a stale `package-lock.json` behind, so when both are
// present the non-npm lockfile is the likelier current truth.
export const LOCKFILES: readonly { file: string; manager: JsPackageManager }[] = [
  { file: "pnpm-lock.yaml", manager: "pnpm" },
  { file: "yarn.lock", manager: "yarn" },
  // bun ≥1.2 writes the text `bun.lock` by default; `bun.lockb` is the older binary form.
  { file: "bun.lockb", manager: "bun" },
  { file: "bun.lock", manager: "bun" },
  { file: "package-lock.json", manager: "npm" },
];

// INSTALL_COMMANDS is the frozen, script-suppressed install per manager.
//
// Script suppression per manager, stated honestly rather than implied to be uniform:
//   npm  — `--ignore-scripts`: full parity, and the settled default (see the header).
//   pnpm — `--ignore-scripts`: full parity (pnpm implements the same flag).
//   bun  — `--ignore-scripts`: parity for dependency lifecycle scripts. (bun additionally
//          gates them behind a `trustedDependencies` allowlist by default, so this is
//          belt-and-braces there.)
//   yarn — `--ignore-scripts` exists in yarn CLASSIC (1.x) only, which is also the only
//          line that accepts `--frozen-lockfile`. Yarn BERRY (2+) renamed the flag to
//          `--immutable` and moved script suppression into `.yarnrc.yml`
//          (`enableScripts: false`), with NO CLI equivalent. So there is no parity on
//          Berry: this command FAILS there, which produces an honest skip (no
//          node_modules, reported as such). That is the safe direction to fail — we
//          never silently downgrade to a scripts-enabled install — but it does mean a
//          Berry repo gets no pre-provisioning today.
const INSTALL_COMMANDS: Record<JsPackageManager, { command: string; args: string[] }> = {
  npm: { command: "npm", args: ["ci", "--ignore-scripts"] },
  pnpm: { command: "pnpm", args: ["install", "--frozen-lockfile", "--ignore-scripts"] },
  yarn: { command: "yarn", args: ["install", "--frozen-lockfile", "--ignore-scripts"] },
  bun: { command: "bun", args: ["install", "--frozen-lockfile", "--ignore-scripts"] },
};

/** One JS project dir found in the clone. */
export interface JsProject {
  /** Path relative to the clone root; "." is the root itself. */
  dir: string;
  /** null when the dir has a package.json but NO recognized lockfile — such a dir is
   *  never installed (we will not guess a manager or write a lockfile). */
  manager: JsPackageManager | null;
  /** The lockfile that selected the manager, or null when there is none. */
  lockfile: string | null;
  /** True when this dir declares workspaces AND has a lockfile, so its subtree was
   *  pruned and it resolves to a single root install. */
  workspaceRoot: boolean;
}

/** The outcome of provisioning one project dir. `ok` is the only "deps are there" signal;
 *  `detail` is a short human note (an exit code or the reason it was skipped) and never
 *  carries command output. */
export interface JsDepsResult {
  dir: string;
  /** "none" when nothing was installed because no lockfile selected a manager. */
  manager: JsPackageManager | "none";
  ok: boolean;
  detail: string;
}

/** One install invocation, already wrapped for the runner uid. */
export interface InstallCommand {
  command: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  timeoutMs: number;
}

/** The exec boundary. Injectable so tests drive the composition without spawning a real
 *  package manager or touching a registry. Resolves — never rejects — in the default
 *  implementation; `installJsDeps` still guards against an injected one that throws. */
export type InstallExec = (cmd: InstallCommand) => Promise<{ ok: boolean; detail: string }>;

/**
 * Find the JS project dirs in a clone: every dir with a `package.json`, bounded by
 * MAX_SCAN_DEPTH / MAX_PROJECT_DIRS / MAX_SCAN_DIRS and never descending into
 * `node_modules`. Breadth-first, children in sorted order, so results are deterministic
 * and ROOT-MOST FIRST — which is what makes truncation at MAX_PROJECT_DIRS keep the dirs
 * most likely to matter.
 *
 * MONOREPOS: a dir that declares workspaces (a `workspaces` field in its package.json,
 * or a sibling `pnpm-workspace.yaml`) AND has a lockfile resolves to a SINGLE install at
 * that dir — its subtree is pruned, so workspace members are not installed individually
 * (that is the package manager's job, and a member install would fight the root one).
 * A workspace declaration WITHOUT a lockfile prunes nothing: there is no root install to
 * do, so any member that carries its own lockfile is still worth finding.
 *
 * Best-effort by construction: an unreadable directory is skipped, never thrown.
 */
export async function discoverJsProjects(rootPath: string): Promise<JsProject[]> {
  const found: JsProject[] = [];
  const queue: { abs: string; rel: string; depth: number }[] = [{ abs: rootPath, rel: ".", depth: 0 }];
  let scanned = 0;

  while (queue.length > 0 && found.length < MAX_PROJECT_DIRS && scanned < MAX_SCAN_DIRS) {
    const cur = queue.shift()!;
    scanned++;

    let entries;
    try {
      entries = await readdir(cur.abs, { withFileTypes: true });
    } catch {
      continue; // unreadable/vanished dir: skip it, provisioning is best-effort
    }

    const files = new Set(entries.filter((e) => !e.isDirectory()).map((e) => e.name));
    let pruned = false;
    if (files.has("package.json")) {
      const hit = LOCKFILES.find((l) => files.has(l.file));
      const workspaceRoot = hit !== undefined && (await declaresWorkspaces(cur.abs, files));
      found.push({
        dir: cur.rel,
        manager: hit?.manager ?? null,
        lockfile: hit?.file ?? null,
        workspaceRoot,
      });
      pruned = workspaceRoot;
    }

    if (pruned || cur.depth >= MAX_SCAN_DEPTH) continue;
    const subdirs = entries
      .filter((e) => e.isDirectory() && !SKIP_DIRS.has(e.name))
      .map((e) => e.name)
      .sort();
    for (const name of subdirs) {
      queue.push({
        abs: join(cur.abs, name),
        rel: cur.rel === "." ? name : `${cur.rel}/${name}`,
        depth: cur.depth + 1,
      });
    }
  }

  return found;
}

/**
 * Install dependencies for every project dir discovered under `rootPath`, sequentially
 * (concurrent installs in one tree contend on the same cache/`node_modules`, and npm has
 * no cross-process lock).
 *
 * BEST-EFFORT, ALWAYS: an install that fails — no registry egress, lockfile drift, a
 * package manager the worker does not have, a wall-clock timeout — leaves `node_modules`
 * absent and is reported as `ok: false` with the reason. This function never throws for
 * an install failure and never reports a success it did not observe: a caller (and the
 * check pre-flight) can then skip honestly instead of misreading a 127 as a test failure.
 *
 * @param rootPath the clone root.
 * @param env      the SCRUBBED replacement env for the subprocess (buildCheckEnv) — never
 *                 a `process.env` spread; the worker's join token must be absent by
 *                 construction.
 */
export async function installJsDeps(
  rootPath: string,
  env: NodeJS.ProcessEnv,
  opts: { timeoutMs?: number; exec?: InstallExec } = {},
): Promise<JsDepsResult[]> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_INSTALL_TIMEOUT_MS;
  const exec = opts.exec ?? execInstall;

  let projects: JsProject[];
  try {
    projects = await discoverJsProjects(rootPath);
  } catch (err) {
    // discoverJsProjects is already non-throwing; this is the belt to its braces, so a
    // surprise here degrades to "nothing provisioned" rather than failing the run.
    return [{ dir: ".", manager: "none", ok: false, detail: `discovery failed: ${errText(err)}` }];
  }

  const results: JsDepsResult[] = [];
  for (const project of projects) {
    if (project.manager === null) {
      results.push({
        dir: project.dir,
        manager: "none",
        ok: false,
        detail: "package.json but no recognized lockfile — not installed",
      });
      continue;
    }

    const spec = INSTALL_COMMANDS[project.manager];
    const label = `${spec.command} ${spec.args.join(" ")}`;
    // PRD #51 M4: the install runs repo-authored package.json/lockfile resolution — an
    // untrusted surface — so under the `runner` uid (setpriv wrapper); a #58 single-uid
    // start runs it directly.
    const wrapped = runnerCommand(spec.command, spec.args);
    const cwd = project.dir === "." ? rootPath : join(rootPath, project.dir);

    let outcome: { ok: boolean; detail: string };
    try {
      outcome = await exec({ command: wrapped.command, args: wrapped.args, cwd, env, timeoutMs });
    } catch (err) {
      outcome = { ok: false, detail: `could not run: ${errText(err)}` };
    }

    results.push({
      dir: project.dir,
      manager: project.manager,
      ok: outcome.ok,
      detail: outcome.ok
        ? `${label} ok`
        : `${label} failed (${outcome.detail}) — node_modules absent, gates skip honestly`,
    });
  }
  return results;
}

/** True when `dir` (relative to the clone root, "." for the root) was provisioned
 *  successfully. The honest answer to "did dir X get its deps?" — a dir that was never
 *  discovered answers false, same as one whose install failed. */
export function depsReadyFor(results: readonly JsDepsResult[], dir: string): boolean {
  const want = dir === "" ? "." : dir;
  return results.some((r) => r.dir === want && r.ok);
}

/**
 * The default exec boundary: `execFile` under the caller's env and wall-clock cap.
 * Captures ONLY the exit status, never the (potentially secret-bearing) output — the
 * run-message redactor does not cover a third-party install's stdout, so none of it is
 * allowed anywhere near a log or an MR. Same discipline as `defaultCheckRunner`.
 */
export const execInstall: InstallExec = (cmd) =>
  new Promise((resolve) => {
    execFile(
      cmd.command,
      cmd.args,
      { cwd: cmd.cwd, env: cmd.env, timeout: cmd.timeoutMs, maxBuffer: 1 << 20 },
      (error) => {
        if (!error) {
          resolve({ ok: true, detail: "exit 0" });
          return;
        }
        // execFile's error carries `code` as the ENOENT-style string on a spawn failure,
        // or the numeric exit status when the command ran and exited non-zero.
        const e = error as Error & { code?: string | number; killed?: boolean };
        if (e.code === "ENOENT") {
          resolve({ ok: false, detail: "package manager not available in the worker" });
          return;
        }
        if (e.killed) {
          resolve({ ok: false, detail: "timed out" });
          return;
        }
        resolve({ ok: false, detail: typeof e.code === "number" ? `exit ${e.code}` : "failed" });
      },
    );
  });

/** True when the dir declares a workspace layout: a `workspaces` field in package.json
 *  (npm/yarn/bun) or a `pnpm-workspace.yaml` beside it. An unreadable or malformed
 *  package.json is treated as "not a workspace root" — the conservative answer, since it
 *  only means we do not prune. */
async function declaresWorkspaces(absDir: string, files: ReadonlySet<string>): Promise<boolean> {
  if (files.has("pnpm-workspace.yaml")) return true;
  try {
    const raw = await readFile(join(absDir, "package.json"), "utf8");
    const parsed = JSON.parse(raw) as { workspaces?: unknown };
    const ws = parsed.workspaces;
    // npm/bun/yarn accept an array; yarn classic also accepts `{ packages: [...] }`.
    if (Array.isArray(ws)) return ws.length > 0;
    if (ws !== null && typeof ws === "object") return true;
    return false;
  } catch {
    return false;
  }
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
