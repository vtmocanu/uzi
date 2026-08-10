// PRD #121 M1 — package-manager-aware, lockfile-driven JS dependency provisioning for
// a runner clone.
//
// A worker seeds a FRESH clone per run, so the target repo's `node_modules` is absent
// when the agent starts and its first gate command (`npm test`, `vitest`, `tsc`) dies
// with `command not found`. A routine called `prepareCheckDeps` (self-improve.ts) used to
// solve that — but only for `self_improve` runs, only AFTER the agent finished, and only
// for uzi's own hardcoded `["web", "agent"]` layout. This module is that routine
// RELOCATED and GENERALIZED: same sandbox, same best-effort/honest-skip contract, now
// driven by whatever lockfiles the cloned repo actually has. PRD #121 M2 deleted the
// original once both call sites moved here, so nothing by that name exists any more.
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
// ─── Repo-authored code execution: a REDUCTION, not a close ───────────────────
//
// `--ignore-scripts` IS SETTLED (PRD #121 "install-flags decision") and stays: the esbuild
// premise that argued against it was tested and disproven, and the flag is what lets
// provisioning run PRE-APPROVAL without a new opt-in gate.
//
// But `--ignore-scripts` ALONE DOES NOT MEAN "no repo-authored code runs". An earlier
// version of this header claimed exactly that, in absolute terms, and it was FALSE for
// yarn and pnpm. Both were measured (PRD #121 M1 review, 2026-07-26; probes run offline
// with `--network none`, yarn inside the pinned `node:22-alpine` base image):
//
//   yarn   `.yarnrc.yml` `yarnPath:` — and the classic `.yarnrc` `yarn-path` — make yarn
//          EXEC a repo-committed .cjs as the package manager BEFORE it parses any flag.
//          Measured: repo JS executed, exit 0, both spellings. This is the layout
//          `yarn set version berry` produces, i.e. the standard one, not a contrived one.
//          CLOSED by `YARN_IGNORE_PATH=1` (yarn's own mechanism) — measured: repo JS not
//          executed, both spellings.
//   pnpm   a repo-local `.pnpmfile.cjs` executes under `--ignore-scripts`; pnpm's own docs
//          say `ignorePnpmfile` is what you need "together with --ignore-scripts when you
//          want to make sure that no script gets executed during install". Measured with
//          pnpm 10.34.5: executed without the flag, not executed with `--ignore-pnpmfile`.
//          SECOND vector, same class as yarnPath: `manage-package-manager-versions`
//          (default ON since 9.7) FETCHES AND RUNS the pnpm version a repo declares in
//          `packageManager`. Measured offline: without the flag pnpm tried
//          `GET https://registry.npmjs.org/pnpm` and failed; with
//          `--config.manage-package-manager-versions=false` it ran the installed pnpm and
//          exited 0. CLOSED by that flag.
//   npm    no equivalent found. Measured: a `packageManager: "npm@9.0.0"` field caused no
//          handoff (offline, exit 0), and the auditor separately measured that a repo
//          `.npmrc` with `ignore-scripts=false` does not override the CLI flag.
//   bun    no handoff observed (a `packageManager: "bun@1.0.0"` field caused none,
//          offline, exit 0).
//
// So the honest claim, in the register `self-improve.ts` already uses for the same
// mechanism ("a defense-in-depth REDUCTION of the lifecycle-script code-exec path", "a
// reduction, not a close"): the flags AND ENV KEY below suppress every repo-authored
// execution path we found and measured, per manager. (Not all of it is flags — yarn's
// half is an environment variable, because yarn reads `yarnPath` before it parses argv.) That is stronger than `--ignore-scripts` alone and
// weaker than a proof. It is NOT an exhaustive audit of any package manager — a manager
// that grows a new repo-file exec path would reopen this silently, which is why each flag
// is documented with what it closes rather than left as folklore. Anything that survives
// still lands inside the SAME sandbox that already runs the repo's tests (runner uid +
// scrubbed env), which is what bounds the blast radius.
//
// If you add a manager, or drop one of these flags, you are moving the PRD's *Trust
// posture* line — the one that says auto-install "needs no new opt-in gate" BECAUSE no
// repo-authored script runs. Say so out loud; do not cross it silently.
//
// ─── What the shipped worker image can actually run ───────────────────────────
// Probed on the pinned base (`agent/templates/base/Dockerfile` → `node:22-alpine`):
// npm ✓ and yarn 1.22.22 ✓ are present; **pnpm and bun are NOT**, and neither the
// Dockerfile's `apk add` nor `devbox-global/devbox.json` supplies them. So on a stock
// worker a pnpm or bun repo ENOENTs into an honest skip, and this module's headline
// generalization delivers nothing there today. They become reachable when a run's
// toolchain provisions them (tier-1 `tool_packages`, or the repo's own devbox under
// `repo_devbox_opt_in`), since `buildCheckEnv` puts `toolEnv.PATH` first — which is
// exactly why their flags are wired now rather than later.

import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { killRunnerGroup, runnerCommand } from "./runner-uid.js";

/** The package managers whose lockfile we recognize. */
export type JsPackageManager = "npm" | "pnpm" | "yarn" | "bun";

// ─── Discovery bounds (a hostile/huge repo must not melt the worker) ──────────
// Discovery walks a repo the user controls. All three bounds exist so a pathological
// tree costs a fixed, small amount of work instead of an unbounded one; each is
// enforced independently because each caps a different runaway.

/** Max directory depth below the clone root that discovery descends. Real JS layouts
 *  put their projects shallow (`web/`, `packages/<name>/`, `apps/<name>/`); a
 *  package.json at depth >4 is far more likely to be a test fixture or a vendored copy
 *  than a project whose deps the agent's gates need. It bounds how DEEP the walk goes,
 *  nothing else — a single level can still be arbitrarily wide, which is what
 *  MAX_SCAN_DIRS is for. */
export const MAX_SCAN_DEPTH = 4;

/** Max project dirs discovery reports (and therefore max installs attempted). Each
 *  install is a subprocess with its own wall-clock cap, so this is what bounds total
 *  provisioning time: worst case MAX_PROJECT_DIRS × timeoutMs. A repo with 400 nested
 *  package.json files gets its first 12 (root-most first — see the BFS order below),
 *  not 400 subprocesses. Hitting it sets `truncated`. */
export const MAX_PROJECT_DIRS = 12;

/** Max directories READ during the walk. The two bounds above do not cap this: a repo
 *  with 50k directories at depth ≤4 and no package.json anywhere would still readdir()
 *  all of them. This is the only bound on the walk's own cost. Hitting it sets
 *  `truncated`. */
export const MAX_SCAN_DIRS = 2000;

/** Per-install wall-clock cap. Inherited from the routine this module replaced (which
 *  used the same 10 minutes); overridable per call. */
export const DEFAULT_INSTALL_TIMEOUT_MS = 10 * 60 * 1000;

/** Directories never descended into. `node_modules` is the requirement (a dependency's
 *  own package.json + lockfile is not a project of this repo, and one `node_modules`
 *  can hold thousands); `.git` is pure walk cost with nothing installable inside. */
const SKIP_DIRS = new Set(["node_modules", ".git"]);

/** The pnpm workspace manifest. Doubles as a workspace-root marker AND, uniquely among
 *  the managers, as evidence of an installable dir with no `package.json` of its own —
 *  measured: `pnpm install --frozen-lockfile` at such a root exits 0. */
const PNPM_WORKSPACE = "pnpm-workspace.yaml";

// LOCKFILES maps a lockfile to its package manager, IN PRECEDENCE ORDER — a dir with
// more than one takes the first match. npm's are last deliberately: a repo that migrated
// to pnpm/yarn/bun commonly leaves a stale `package-lock.json` behind, so when both are
// present the non-npm lockfile is the likelier current truth.
// Not exported: nothing outside this module has a use for it, and the observable
// behaviour (which lockfile selects which manager, and in what precedence) is pinned
// through `discoverJsProjects` where a consumer would actually meet it.
const LOCKFILES: readonly { file: string; manager: JsPackageManager }[] = [
  { file: "pnpm-lock.yaml", manager: "pnpm" },
  { file: "yarn.lock", manager: "yarn" },
  // bun ≥1.2 writes the text `bun.lock` by default; `bun.lockb` is the older binary form.
  { file: "bun.lockb", manager: "bun" },
  { file: "bun.lock", manager: "bun" },
  // `npm ci` accepts a shrinkwrap as a lockfile, and prefers it over package-lock.json
  // when both exist — so it is listed first of the two for the same reason.
  { file: "npm-shrinkwrap.json", manager: "npm" },
  { file: "package-lock.json", manager: "npm" },
];

/** How one manager is invoked. `env` is a per-manager OVERLAY on the caller's scrubbed
 *  env — see the note on INSTALL_COMMANDS for why it exists at all. */
interface InstallSpec {
  command: string;
  args: string[];
  env?: Record<string, string>;
}

// INSTALL_COMMANDS is the frozen, execution-suppressed install per manager. Every flag
// here closes a MEASURED repo-authored execution path (the header records each probe);
// none is decoration, and removing one is a security change, not a cleanup.
//
//   npm  — `--ignore-scripts` suppresses lifecycle scripts. Nothing further was found.
//   pnpm — `--ignore-scripts` (lifecycle) + `--ignore-pnpmfile` (a repo `.pnpmfile.cjs`,
//          which runs DESPITE --ignore-scripts) + `--config.manage-package-manager-
//          versions=false` (stops pnpm fetching and running the pnpm build a repo names
//          in `packageManager`).
//   yarn — `--ignore-scripts` covers lifecycle scripts on yarn CLASSIC (1.x), which is
//          also the only line that accepts `--frozen-lockfile`. It does NOT cover
//          `yarnPath`, so the env overlay carries `YARN_IGNORE_PATH=1`.
//   bun  — `--ignore-scripts` here suppresses the scripts in the PROJECT'S OWN
//          package.json. That is the load-bearing case and the reason the flag matters
//          MORE on bun than elsewhere, not less: bun already declines to run a
//          *dependency's* scripts unless it is in `trustedDependencies`, so the untrusted
//          thing this flag stops is the cloned repo's own `postinstall`. Measured with bun
//          1.3.14: without the flag the repo's own postinstall ran; with it, it did not.
//
// THE ENV OVERLAY IS AN EXCEPTION, AND A NARROW ONE. The module otherwise adds, defaults
// and merges NO environment key — it passes the caller's scrubbed env through verbatim,
// which is a property an audit checked and which the tests pin. `YARN_IGNORE_PATH` is a
// deliberate hardening key in the same register as `buildCheckEnv`'s own
// `GIT_TERMINAL_PROMPT=0`, it carries no secret, and it applies ONLY to the yarn arm — the
// other three managers still receive the caller's env unchanged, byte for byte.
const INSTALL_COMMANDS: Record<JsPackageManager, InstallSpec> = {
  npm: { command: "npm", args: ["ci", "--ignore-scripts"] },
  pnpm: {
    command: "pnpm",
    args: [
      "install",
      "--frozen-lockfile",
      "--ignore-scripts",
      "--ignore-pnpmfile",
      "--config.manage-package-manager-versions=false",
    ],
  },
  // THE SPLIT BELOW IS DELIBERATE: `--ignore-scripts` is ARGV, `YARN_IGNORE_PATH` is the
  // env overlay, and the two are NOT interchangeable despite reading as siblings. Folding
  // `--ignore-scripts` into the overlay beside its neighbour looks like tidying and would
  // be a security regression on Berry. Both halves verified against Berry's source at
  // `master`, 2026-07-27:
  //
  //   - Berry keeps an `IGNORED_ENV_VARIABLES` blocklist (`yarnpkg-core/sources/
  //     Configuration.ts`), skipped for any setting whose source is `<environment>`.
  //     `ignoreScripts` IS in it, carrying Berry's own comment that `YARN_IGNORE_SCRIPTS`
  //     "should not shadow Yarn Modern's enableScripts setting when inherited from shared
  //     CI environments". So as an env var it is silently discarded.
  //   - `ignorePath` is NOT in that blocklist, which is why the env half of this
  //     mitigation reaches Berry rather than being a yarn-1-only mechanism.
  //
  // The regression is worse than losing suppression, because of what the argv form buys
  // us today: Berry's `install` command declares no `--ignore-scripts` option at all (its
  // full `Option.*` list is json/immutable/immutable-cache/refresh-lockfile/check-cache/
  // check-resolutions/inline-builds/mode plus the hidden yarn-1 aliases — `--frozen-lockfile`
  // among them, so THAT one is accepted). A Berry install therefore fails on the
  // unsupported flag and honest-skips, which is safe. Move the flag to the env and the
  // command becomes one Berry ACCEPTS while ignoring the env var — turning a failing
  // honest skip into a SUCCEEDING install that runs the repo's scripts.
  yarn: {
    command: "yarn",
    args: ["install", "--frozen-lockfile", "--ignore-scripts"],
    env: { YARN_IGNORE_PATH: "1" },
  },
  bun: { command: "bun", args: ["install", "--frozen-lockfile", "--ignore-scripts"] },
};

/** One JS project dir found in the clone. */
export interface JsProject {
  /** Path relative to the clone root; "." is the root itself. */
  dir: string;
  /** null when the dir has no recognized lockfile — such a dir is never installed (we
   *  will not guess a manager or write a lockfile). */
  manager: JsPackageManager | null;
  /** The lockfile that selected the manager, or null when there is none. */
  lockfile: string | null;
  /** True when this dir declares workspaces AND has a lockfile, so its subtree was
   *  pruned and it resolves to a single root install. */
  workspaceRoot: boolean;
  /** True when the dir's package.json declares at least one dependency of any kind.
   *  Load-bearing for the post-install corroboration: a project that declares NONE
   *  legitimately ends up with no `node_modules` (measured: `npm ci` on a zero-dependency
   *  project exits 0 and creates nothing), so absence there must not be read as failure. */
  declaresDependencies: boolean;
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

/** What discovery found, and whether it saw the whole tree. */
export interface JsProjectScan {
  projects: JsProject[];
  /** True when the walk stopped at MAX_PROJECT_DIRS or MAX_SCAN_DIRS with directories
   *  left unexamined — i.e. the project list is a PREFIX, not the full set. Reported
   *  because a silent cap reads exactly like full coverage, which is the class of lie
   *  this PRD exists to remove. MAX_SCAN_DEPTH does NOT set it: depth is a standing
   *  policy about which trees are in scope at all, so folding it in here would make the
   *  flag fire on ordinary repos and mean nothing. */
  truncated: boolean;
}

/** The result of a provisioning sweep. */
export interface JsDepsInstall {
  results: JsDepsResult[];
  /** Carried up from discovery: `results` covers only the dirs discovery reached. A
   *  caller logging "installed everything" must consult this first. */
  truncated: boolean;
}

/** One install invocation, already wrapped for the runner uid. */
export interface InstallCommand {
  command: string;
  args: string[];
  cwd: string;
  env: NodeJS.ProcessEnv;
  timeoutMs: number;
  /** Aborts this install (PRD #121 M2). Provisioning is kicked off concurrently with
   *  the plan turn, so a run that ends without ever reaching the implement phase — a
   *  rejected plan, a cancel — must be able to reclaim the subprocess instead of
   *  blocking teardown on it for the full timeout. */
  signal?: AbortSignal;
}

/** The `detail` an install reports when it was aborted rather than run to a verdict.
 *  Distinguished from a genuine failure so a log never reads a cancel as npm erroring.
 *  EXPORTED because it is part of the `InstallExec` contract: `installJsDeps` renders
 *  this value differently from a failure, so an alternative exec boundary must use it
 *  for cancellation and must not return it for an ordinary error. */
export const DETAIL_CANCELLED = "cancelled";

/** The `detail` of the ONE deliberate non-install: a dir with a package.json but no
 *  recognized lockfile, which uzi never installs (it refuses to guess a manager). Gate
 *  honesty (sdk-executor) keys its "unverified" exclusion on THIS exact reason, so it is
 *  a named export shared with that consumer rather than a bare literal re-typed there —
 *  otherwise a genuine install/discovery FAILURE that also carries `manager:"none"` (the
 *  `discovery failed` record below) gets silently folded in with the deliberate skip and
 *  the false-green it should annotate disappears. */
export const DETAIL_NO_LOCKFILE = "package.json but no recognized lockfile — not installed";

/**
 * The exec boundary. Injectable so tests drive the composition without spawning a real
 * package manager or touching a registry. Resolves — never rejects — in the default
 * implementation; `installJsDeps` still guards against an injected one that throws.
 *
 * CONTRACT for an alternative implementation: `detail` is a SHORT status note that ends
 * up on the run's activity feed. It must never carry the subprocess's output. The
 * shipped `execInstall` cannot leak any (`stdio: "ignore"`), but an `execFile`-style
 * boundary embeds `Command failed: <cmd>\n<stderr>` in its Error, which would put
 * third-party install stderr in front of a user.
 */
export type InstallExec = (cmd: InstallCommand) => Promise<{ ok: boolean; detail: string }>;

/**
 * Find the JS project dirs in a clone, bounded by MAX_SCAN_DEPTH / MAX_PROJECT_DIRS /
 * MAX_SCAN_DIRS and never descending into `node_modules`. Breadth-first, children in
 * sorted order, so results are deterministic and ROOT-MOST FIRST — which is what makes
 * truncation at MAX_PROJECT_DIRS keep the dirs most likely to matter.
 *
 * A dir is a project when it has a `package.json`, OR when it is a pnpm workspace root
 * (`pnpm-workspace.yaml`) that has a lockfile — pnpm installs such a root fine with no
 * root `package.json` of its own (measured), and skipping it would mean the one install
 * that would have worked is never attempted.
 *
 * MONOREPOS: a dir that declares workspaces (a non-empty `workspaces` field in its
 * package.json, or a `pnpm-workspace.yaml`) AND has a lockfile resolves to a SINGLE
 * install at that dir — its subtree is pruned, so workspace members are not installed
 * individually (that is the package manager's job, and a member install would fight the
 * root one). A workspace declaration WITHOUT a lockfile prunes nothing: there is no root
 * install to do, so any member that carries its own lockfile is still worth finding.
 *
 * SYMLINKS ARE NOT FOLLOWED, and that is load-bearing rather than incidental: it is what
 * keeps the walk — and therefore every install `cwd` — inside the clone. A repo can
 * commit a symlink to `/` or to `..`; `Dirent.isDirectory()` is lstat-based, so such an
 * entry is not a directory to this walk and is never enqueued. A refactor to
 * `readdir` + `stat`, or to `readdir(..., { recursive: true })`, would silently reopen
 * it — there is a test pinning this, keep it.
 *
 * Best-effort by construction: an unreadable directory is skipped, never thrown.
 */
export async function discoverJsProjects(rootPath: string): Promise<JsProjectScan> {
  const projects: JsProject[] = [];
  const queue: { abs: string; rel: string; depth: number }[] = [{ abs: rootPath, rel: ".", depth: 0 }];
  let scanned = 0;
  let truncated = false;

  while (queue.length > 0) {
    if (projects.length >= MAX_PROJECT_DIRS || scanned >= MAX_SCAN_DIRS) {
      // Stopped with dirs still queued: the list is a prefix, and the caller must know.
      truncated = true;
      break;
    }
    const cur = queue.shift()!;
    scanned++;

    let entries;
    try {
      entries = await readdir(cur.abs, { withFileTypes: true });
    } catch {
      continue; // unreadable/vanished dir: skip it, provisioning is best-effort
    }

    // isDirectory() is lstat-based, so a symlink lands here rather than in `subdirs`.
    const files = new Set(entries.filter((e) => !e.isDirectory()).map((e) => e.name));
    const hit = LOCKFILES.find((l) => files.has(l.file));
    const hasPkg = files.has("package.json");
    const pnpmWorkspace = files.has(PNPM_WORKSPACE);

    let pruned = false;
    if (hasPkg || (pnpmWorkspace && hit !== undefined)) {
      const manifest = hasPkg ? await readManifest(cur.abs) : null;
      const workspaceRoot = hit !== undefined && (pnpmWorkspace || (manifest?.declaresWorkspaces ?? false));
      projects.push({
        dir: cur.rel,
        manager: hit?.manager ?? null,
        lockfile: hit?.file ?? null,
        workspaceRoot,
        declaresDependencies: manifest?.declaresDependencies ?? false,
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

  return { projects, truncated };
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
 *                 construction. Passed through verbatim except for the documented
 *                 per-manager hardening overlay (INSTALL_COMMANDS).
 * @param opts.signal aborts the sweep: the in-flight install is killed and every dir not
 *                 yet reached is reported cancelled. Cancelling is still a normal return,
 *                 never a throw — the caller gets the partial results.
 */
export async function installJsDeps(
  rootPath: string,
  env: NodeJS.ProcessEnv,
  opts: { timeoutMs?: number; exec?: InstallExec; signal?: AbortSignal } = {},
): Promise<JsDepsInstall> {
  const timeoutMs = opts.timeoutMs ?? DEFAULT_INSTALL_TIMEOUT_MS;
  const exec = opts.exec ?? execInstall;

  let scan: JsProjectScan;
  try {
    scan = await discoverJsProjects(rootPath);
  } catch (err) {
    // discoverJsProjects is already non-throwing; this is the belt to its braces, so a
    // surprise here degrades to "nothing provisioned" rather than failing the run.
    return {
      results: [{ dir: ".", manager: "none", ok: false, detail: `discovery failed: ${errText(err)}` }],
      truncated: false,
    };
  }

  const results: JsDepsResult[] = [];
  for (const project of scan.projects) {
    if (project.manager === null) {
      results.push({
        dir: project.dir,
        manager: "none",
        ok: false,
        detail: DETAIL_NO_LOCKFILE,
      });
      continue;
    }

    const spec = INSTALL_COMMANDS[project.manager];
    // Rendered shell-style, WITH any env overlay, because this string is what a human
    // reads on the run feed and in the logs. Showing only the argv would display a yarn
    // install as `yarn install --frozen-lockfile --ignore-scripts` — which, as displayed,
    // is the command that executes a repo-committed yarnPath. The half that closes it
    // would be invisible to exactly the person debugging whether it was applied.
    const label = [...Object.entries(spec.env ?? {}).map(([k, v]) => `${k}=${v}`), spec.command, ...spec.args].join(" ");
    // PRD #51 M4: the install runs repo-authored package.json/lockfile resolution — an
    // untrusted surface — so under the `runner` uid (setpriv wrapper); a #58 single-uid
    // start runs it directly.
    const wrapped = runnerCommand(spec.command, spec.args);
    const cwd = project.dir === "." ? rootPath : join(rootPath, project.dir);
    // The caller's env, untouched, unless this manager needs a hardening key.
    const installEnv = spec.env ? { ...env, ...spec.env } : env;

    let outcome: { ok: boolean; detail: string };
    if (opts.signal?.aborted) {
      // Already cancelled: report the remaining dirs honestly instead of spawning
      // installs whose result nobody will wait for.
      outcome = { ok: false, detail: DETAIL_CANCELLED };
    } else {
      try {
        outcome = await exec({
          command: wrapped.command,
          args: wrapped.args,
          cwd,
          env: installEnv,
          timeoutMs,
          signal: opts.signal,
        });
      } catch (err) {
        outcome = { ok: false, detail: `could not run: ${errText(err)}` };
      }
    }

    // CORROBORATE a claimed success against the filesystem. `ok` is the signal a caller
    // (and, later, gate honesty) reads as "the deps are there", so an exit 0 that left no
    // `node_modules` must not set it — a repo that can force exit 0 would otherwise mint
    // a false "deps ready".
    //
    // The condition is not caution, it is measured, and it cuts BOTH ways:
    //
    //  - A project that declares NO dependencies legitimately ends up with no
    //    `node_modules`: `npm ci` there exits 0 and creates nothing (yarn and pnpm do
    //    create one). Corroborating unconditionally would turn that genuine success into
    //    a reported failure — the same class of lie in the other direction.
    //  - A WORKSPACE ROOT is corroborated even when its own package.json declares no
    //    dependencies, because the deps live in the members and `workspaceRoot` PRUNES the
    //    whole subtree — so one false `ok` here would cover every member of the monorepo,
    //    which is the common shape rather than an exotic one. This does not over-fire:
    //    measured, `npm ci` at a workspace root creates a root `node_modules` even with
    //    zero dependencies declared anywhere in the workspace (it writes the member
    //    symlinks), while the members get none of their own.
    //
    // When we genuinely cannot tell — no readable package.json and not a workspace root —
    // we do not downgrade. Unknown is not a positive in either direction.
    //
    // WORTH RE-DECIDING WHEN M4 LANDS. This leans on an asymmetry that is true today: a
    // false "not ready" costs an unnecessary honest skip, while a false "ready" is exactly
    // the lie the corroboration exists to catch. Once gate honesty consumes this signal, a
    // false "not ready" instead produces an "unverified" banner on a delivery that was
    // fine — and banners that cry wolf train reviewers to ignore them, which is the
    // failure mode M4's own design warns about. The asymmetry flips; revisit it there.
    if (outcome.ok && (project.declaresDependencies || project.workspaceRoot) && !existsSync(join(cwd, "node_modules"))) {
      outcome = { ok: false, detail: "reported success but left no node_modules" };
    }

    results.push({
      dir: project.dir,
      manager: project.manager,
      ok: outcome.ok,
      detail: outcome.ok
        ? `${label} ok`
        : outcome.detail === DETAIL_CANCELLED
          ? `${label} cancelled — node_modules absent`
          : `${label} failed (${outcome.detail}) — node_modules absent, gates skip honestly`,
    });
  }
  return { results, truncated: scan.truncated };
}

/** True when `dir` (relative to the clone root, "." for the root) was provisioned
 *  successfully. The honest answer to "did dir X get its deps?" — a dir that was never
 *  discovered answers false, same as one whose install failed. */
export function depsReadyFor(results: readonly JsDepsResult[], dir: string): boolean {
  const want = dir === "" ? "." : dir;
  return results.some((r) => r.dir === want && r.ok);
}

/**
 * The default exec boundary: `spawn` under the caller's env, with a wall-clock cap and
 * cancellation. Reports ONLY how the process ended — never its output, which is not
 * captured at all (`stdio: "ignore"`). That is deliberate and structural rather than
 * disciplinary: the run-message redactor does not cover a third-party install's stdout,
 * so the safest place for it is nowhere.
 *
 * THE `spawn` + `detached: true` + `killRunnerGroup` TRIO IS LOAD-BEARING. A revert to
 * `execFile` — which looks like a simplification and reads like one — silently
 * reintroduces a measured defect: `execFile`'s own `timeout` (and its `signal` option)
 * kill from the WORKER uid, which is EPERM against a process running as `runner`, so
 * under the PRD #51 split NEITHER the cap NOR a cancel could kill anything. Measured
 * in-container before this shape: `kill EPERM`, the runner's process still alive 6s after
 * the timeout fired, up to MAX_PROJECT_DIRS orphans able to accumulate.
 *
 *  - `detached: true` makes the child a process-GROUP leader, which is the shape
 *    `killRunnerGroup` documents and what lets a kill reach any grandchild the package
 *    manager spawned. `execFile` does not even forward `detached` (it copies a fixed
 *    subset of options to spawn), so the flag would be dropped without a word.
 *  - `killRunnerGroup` reuids via setpriv under the split and signals directly on a #58
 *    single-uid start, so both modes can actually reap.
 *
 * WHAT `detached` COSTS, since it is a real behaviour change: the install is its own
 * process group and session leader, so it NO LONGER receives a signal sent to the
 * worker's process group. Reaping it depends entirely on the abort path, the timeout, or
 * container teardown — which is why the executor aborts on every path that does not join
 * (sdk-executor.ts), rather than trusting process-group semantics it no longer has.
 */
export const execInstall: InstallExec = (cmd) =>
  new Promise((resolve) => {
    let cancelled = false;
    let timedOut = false;
    let settled = false;
    const child = spawn(cmd.command, cmd.args, {
      cwd: cmd.cwd,
      env: cmd.env,
      detached: true,
      stdio: "ignore",
    });
    const onAbort = (): void => {
      cancelled = true;
      killRunnerGroup(child.pid);
    };
    const timer = setTimeout(() => {
      timedOut = true;
      killRunnerGroup(child.pid);
    }, cmd.timeoutMs);
    // The live child keeps the loop alive on its own; an un-unref'd timer would keep it
    // alive for the FULL cap even after the install finished early.
    timer.unref();
    const done = (r: { ok: boolean; detail: string }): void => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      cmd.signal?.removeEventListener("abort", onAbort);
      resolve(r);
    };

    child.on("error", (err) => {
      // A spawn failure: the package manager is not on the runner's PATH.
      const code = (err as NodeJS.ErrnoException).code;
      done({ ok: false, detail: code === "ENOENT" ? "package manager not available in the worker" : "failed" });
    });
    child.on("close", (code, signalName) => {
      // A cancelled or timed-out install has no verdict — say so, never invent an exit
      // code from the signal that killed it.
      if (cancelled) return done({ ok: false, detail: DETAIL_CANCELLED });
      if (timedOut) return done({ ok: false, detail: "timed out" });
      if (code === 0) return done({ ok: true, detail: "exit 0" });
      if (code === null) return done({ ok: false, detail: `killed (${signalName})` });
      done({ ok: false, detail: `exit ${code}` });
    });

    if (cmd.signal) {
      if (cmd.signal.aborted) onAbort();
      else cmd.signal.addEventListener("abort", onAbort, { once: true });
    }
  });

/** The two facts we need out of a dir's package.json. An unreadable or malformed file
 *  yields `null`, which every caller treats as "cannot tell" — never as a positive: not
 *  a workspace root (so we do not prune), and dependencies unknown (so we do not
 *  downgrade a successful install). */
async function readManifest(absDir: string): Promise<{ declaresWorkspaces: boolean; declaresDependencies: boolean } | null> {
  try {
    const raw = await readFile(join(absDir, "package.json"), "utf8");
    const parsed = JSON.parse(raw) as {
      workspaces?: unknown;
      dependencies?: unknown;
      devDependencies?: unknown;
      optionalDependencies?: unknown;
      peerDependencies?: unknown;
    };
    return {
      declaresWorkspaces: workspacePatternCount(parsed.workspaces) > 0,
      declaresDependencies: [
        parsed.dependencies,
        parsed.devDependencies,
        parsed.optionalDependencies,
        parsed.peerDependencies,
      ].some((d) => d !== null && typeof d === "object" && Object.keys(d as object).length > 0),
    };
  } catch {
    return null;
  }
}

/** How many workspace patterns a `workspaces` field declares. npm/bun/yarn accept a bare
 *  array; yarn classic also accepts `{ packages: [...] }`. Counting rather than
 *  truthiness is what keeps the two spellings SYMMETRIC — `workspaces: []` and
 *  `workspaces: {}` must both mean "declares nothing", where an `Array.isArray` test
 *  followed by a bare `typeof === "object"` made the empty object prune a whole subtree. */
function workspacePatternCount(ws: unknown): number {
  if (Array.isArray(ws)) return ws.length;
  if (ws !== null && typeof ws === "object") {
    const packages = (ws as { packages?: unknown }).packages;
    return Array.isArray(packages) ? packages.length : 0;
  }
  return 0;
}

function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
