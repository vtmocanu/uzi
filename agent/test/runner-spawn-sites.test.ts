import { describe, it } from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

// issue #1783 (R4) — every place the worker crosses into a runner/runner-cmd uid is listed here
// with its disposition toward the run-quiescence reaper. The reaper attributes runner-uid
// processes by env: an AGENT process carries the attempt marker (only the CLI spawn), and every
// worker-authored, fixed-argv spawn that can run no repo-configured code carries the worker mark
// (UZI_WORKER_SPAWN) so a reap never kills the worker's own op, while a spawn that executes repo-,
// clone-config- or agent-authored code (a runner-clone `git status`/`add`/`commit` with a planted
// filter included) is deliberately NOT marked (whatever it leaks must stay reapable). A new, unlisted `runnerCommand(` / `runnerSpawn(` /
// `commandRootCommand(` / `setprivRunnerArgs(` / `setprivArgsForUid(` site, a direct setpriv spawn
// (`spawnSync(SETPRIV`), or any string literal ending in `setpriv` ("/bin/setpriv",
// "/usr/bin/setpriv", "setpriv") fails this test until someone decides which side of that line it
// is on and records it below. A listed site that disappears fails it too.

const SRC = path.join(path.dirname(fileURLToPath(import.meta.url)), "..", "src");
const NAMES = ["runnerCommand", "runnerSpawn", "commandRootCommand", "setprivRunnerArgs", "setprivArgsForUid"] as const;
/** A DIRECT setpriv spawn (bypassing the wrappers above): `spawn(SETPRIV`, `spawnSync(SETPRIV`,
 *  an execFile of it, or a "/bin/setpriv" literal anywhere. */
const SETPRIV_SPAWN = /\b(?:spawn|spawnSync|execFile|execFileSync|execFileAsync)\(\s*SETPRIV\b/g;
/** Any string literal whose value ENDS in `setpriv`: "/bin/setpriv", "/usr/bin/setpriv", a bare
 *  "setpriv" resolved through PATH, or a template literal of either. */
const SETPRIV_LITERAL = /["'`](?:[^"'`\n]*\/)?setpriv["'`]/g;

/**
 * How a site's spawn is marked: `marked` (always carries workerSpawnEnv), `unmarked` (never: it can
 * run repo-, agent- or model-directed code, so its leaks stay reapable), `conditional` (the site
 * decides per call: runGitAsRunner marks only driver-free git subcommands, the tick spawner keeps its
 * caller's choice for a runner-uid child), or `wrapper` (a wrapper body / reference whose CALLER
 * decides the env).
 */
type Mark = "marked" | "unmarked" | "conditional" | "wrapper";

/** `<file>#<name>()` for a call, `<file>#<name>&` for a bare reference → expected count + why. */
const ALLOWLIST: Record<string, { count: number; mark: Mark; disposition: string }> = {
  "env-probe.ts#runnerSpawn&": { count: 1, mark: "unmarked", disposition: "issue #1866: spawnRunnerProbe's default spawn of the fixed run-start environment probe (constant `node -e` script, NODE_OPTIONS/NODE_PATH stripped) under the SDK env: carries the attempt marker (buildSdkEnv) like the agent CLI, never the worker mark, so a probe that outlived its own group-kill proof stays reapable" },
  "sdk-spawn.ts#runnerSpawn()": { count: 1, mark: "unmarked", disposition: "the agent CLI: carries the attempt marker (buildSdkEnv), never the worker mark" },
  "runner-uid.ts#runnerCommand()": { count: 1, mark: "wrapper", disposition: "inside runnerSpawn: the caller's env decides (see sdk-spawn.ts)" },
  "runner-uid.ts#setprivRunnerArgs()": { count: 3, mark: "marked", disposition: "runnerCommand's own body + the two kill helpers (worker-authored fixed argv: workerSpawnEnv)" },
  "runner-uid.ts#setprivArgsForUid()": { count: 3, mark: "wrapper", disposition: "setprivRunnerArgs / commandRootCommand / workerBoundaryCommand bodies (the wrappers themselves)" },
  "runner-uid.ts#setpriv-spawn": { count: 2, mark: "marked", disposition: "killRunnerGroup / killRunnerGroupOnly `kill -KILL` (worker-authored fixed argv: workerSpawnEnv)" },
  "runner-uid.ts#setpriv-literal": { count: 1, mark: "wrapper", disposition: "the SETPRIV constant the wrappers use" },
  "executor.ts#runnerCommand()": { count: 2, mark: "conditional", disposition: "stub executor git (the agent turn's stand-in `add`/`commit` in the clone, which runs clone-configured filters): NOT marked; readWorktreeHeadSha (PRD #1798, fixed driver-free `rev-parse`): marked + GIT_ALLOW_PROTOCOL pin via git.ts runnerGitSpawnEnv" },
  "git.ts#runnerCommand()": { count: 4, mark: "conditional", disposition: "runGitAsRunner: workerSpawnEnv only for runnerGitCarriesWorkerMark's driver-free subcommands (rev-parse, update-ref, config, ls-files, …; lazy fetch pinned off); status/add/commit/checkout/reset/merge/rebase/diff/log run UNMARKED (a planted filter/driver stays reapable). runRunnerUidDelete (the retention sweep and the M3 canonical-path free): MARKED (workerSpawnEnv) — a worker-authored fixed argv `/bin/rm -rf -- <target>` (absolute root-owned busybox rm, cwd `/`, PATH /usr/bin:/bin) that reads no repo, clone-config or agent-authored input and so runs no repo-configured code; the uid, not the mark, is its containment, and its own timeout kills it via killRunnerGroup. materializeRunnerClone (issue #1769, the Codex self-contained seed: `git rev-parse`/`update-ref`/`repack -a -d`/`fsck --connectivity-only`/`cat-file -e` in the clone plus `/bin/mv` of its alternates file): NOT marked — materializeEnv(gitEnv()) is a replacement env that never carries the mark, and repack/fsck read the clone's `.git/config`; it runs before the executor starts, inside the bare's withLock. spawnGitAsRunnerWithStdin outside a boundary (issue #1769, ensureRunnerCloneObjects' `git index-pack --stdin` in the clone, the finalize base-align default-tip import): NOT marked — the same replacement gitEnv() with no mark, and index-pack reads the clone's `.git/config`; inside a boundary it is boundary.spawn'd as `command` instead (no runnerCommand)" },
  "js-deps.ts#runnerCommand()": { count: 1, mark: "unmarked", disposition: "JS deps install: NOT marked (the package manager reads repo-controlled config in the clone; leaks stay reapable)" },
  "provision.ts#runnerCommand()": { count: 2, mark: "conditional", disposition: "devbox/nix run: NOT marked (untrusted nix build hooks; cwd outside the clone); PATH probe (fixed script): workerSpawnEnv" },
  "provision.ts#commandRootCommand()": { count: 1, mark: "marked", disposition: "PATH probe as runner-cmd (uid 10003, never scanned; fixed script): workerSpawnEnv" },
  "self-improve.ts#runnerCommand()": { count: 1, mark: "unmarked", disposition: "finalize-time check: NOT marked (repo-authored command, cwd in the clone; leaks stay reapable)" },
  "tick-spawner.ts#runnerCommand()": { count: 3, mark: "conditional", disposition: "kill/probe helpers (worker-authored fixed argv): workerSpawnEnv; tick child: a worker_pat child is marked, a runner-uid (`command`) child keeps its caller's mark decision (runGitAsRunner)" },
  "rmtree.ts#runnerCommand&": { count: 8, mark: "marked", disposition: "purge helper passes, and the PRD #1809 helper wrappers (agentWrappers, rmTreePinned's passes) that runHelper / the cache and HOME measure helpers / runAgentHelper (cache trim list + evict, run-procs) spawn (fixed worker-authored scripts, cwd outside the clone): workerSpawnEnv on the minimal env" },
  "rmtree.ts#commandRootCommand&": { count: 4, mark: "marked", disposition: "the same purge and PRD #1809 helper passes as runner-cmd (fixed scripts): workerSpawnEnv on the minimal env" },
  "run-procs.ts#runnerCommand&": { count: 1, mark: "wrapper", disposition: "PRD #1809 D4 attributed-process scan/kill: the wrapper handed to rmtree.ts's runAgentHelper, whose spawn carries workerSpawnEnv (a fixed worker-authored script)" },
  "run-quiescence.ts#runnerCommand()": { count: 1, mark: "marked", disposition: "the quiescence helper (worker-authored fixed argv): workerSpawnEnv, never an attempt marker" },
  "run-quiescence.ts#runnerCommand&": { count: 1, mark: "marked", disposition: "the helper's private-TMPDIR mktemp/rm wrap (fixed argv, async + time-bounded): workerSpawnEnv" },
  "codex/launcher.ts#runnerCommand()": { count: 1, mark: "unmarked", disposition: "Codex provider root supervisor: NOT marked (model-directed app-server); a recorded worker-launched root (pid + start time) instead" },
  "codex/launcher.ts#commandRootCommand()": { count: 4, mark: "conditional", disposition: "Codex command root + effect root: NOT marked (model-directed shells); standalone modes + kill (fixed argv): workerSpawnEnv" },
  "codex/launcher.ts#runnerCommand&": { count: 3, mark: "marked", disposition: "owned-tree create/remove wrap (fixed scripts) + verifyRunnerPosture wrap (fixed metadata checks): workerSpawnEnv on the inert env" },
  "codex/launcher.ts#commandRootCommand&": { count: 3, mark: "marked", disposition: "owned-tree create/remove wrap (fixed scripts) + verifyRunnerPosture wrap (fixed metadata checks): workerSpawnEnv on the inert env" },
};

function listTs(dir: string): string[] {
  return fs.readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) return listTs(p);
    return e.isFile() && e.name.endsWith(".ts") ? [p] : [];
  });
}

/** Drop imports, comment lines and the three definitions, keeping code. */
function codeOf(text: string): string {
  return text
    .replace(/^import\s[\s\S]*?\sfrom\s+"[^"]+";/gm, "")
    .split("\n")
    .filter((l) => !/^\s*(\/\/|\/\*|\*)/.test(l))
    .map((l) => l.replace(/\s\/\/\s.*$/, ""))
    .filter((l) => !/export function (runnerCommand|runnerSpawn|commandRootCommand|setprivRunnerArgs|setprivArgsForUid)\(/.test(l))
    .join("\n");
}

/**
 * The local names `fn` is callable by in a file: itself, plus every `fn as <alias>` in a named
 * import (a namespace import's `ns.fn(` already matches `\bfn\(`). Read from the RAW text, since
 * codeOf drops the import statements.
 */
function callableNames(text: string, fn: string): string[] {
  const names = [fn];
  for (const m of text.matchAll(/^import\s+(?:type\s+)?\{([\s\S]*?)\}\s+from\s+"[^"]+";/gm)) {
    for (const spec of m[1]!.split(",")) {
      const alias = spec.trim().match(new RegExp(`^${fn}\\s+as\\s+(\\w+)$`));
      if (alias) names.push(alias[1]!);
    }
  }
  return names;
}

/** True when `code` calls `fn` under any of `names`. */
function callsAny(code: string, names: readonly string[]): boolean {
  return names.some((n) => new RegExp(`\\b${n}\\(`).test(code));
}

const MARK_FNS = ["workerSpawnEnv", "unmarkedSpawnEnv"] as const;

function found(): Record<string, number> {
  const out: Record<string, number> = {};
  for (const file of listTs(SRC)) {
    const rel = path.relative(SRC, file).split(path.sep).join("/");
    const code = codeOf(fs.readFileSync(file, "utf8"));
    for (const name of NAMES) {
      for (const m of code.matchAll(new RegExp(`\\b${name}\\b(\\s*\\()?`, "g"))) {
        const key = `${rel}#${name}${m[1] ? "()" : "&"}`;
        out[key] = (out[key] ?? 0) + 1;
      }
    }
    for (const [re, tag] of [[SETPRIV_SPAWN, "setpriv-spawn"], [SETPRIV_LITERAL, "setpriv-literal"]] as const) {
      for (const _m of code.matchAll(re)) {
        const key = `${rel}#${tag}`;
        out[key] = (out[key] ?? 0) + 1;
      }
    }
  }
  return out;
}

describe("runner-uid spawn sites (issue #1783 R4)", () => {
  it("every site is listed with its disposition, and every listed site still exists", () => {
    const actual = found();
    const expected = Object.fromEntries(Object.entries(ALLOWLIST).map(([k, v]) => [k, v.count]));
    assert.deepEqual(actual, expected);
    for (const v of Object.values(ALLOWLIST)) assert.ok(v.disposition.length > 0);
  });

  // A PER-SITE check (does this very call pass workerSpawnEnv?) needs each call's enclosing
  // function, i.e. a real TypeScript parse: this repo's typescript is 7.x, whose only JS API is the
  // `typescript/unstable/*` surface, and a brace/regex heuristic misattributes sites (default
  // parameters, multi-line arrow headers). So the mark is checked per FILE, which is robust: a file
  // whose every site is `unmarked` must never call workerSpawnEnv (re-marking the stub executor's
  // git, the agent CLI, the deps install or a self-improve check reddens here), a file with a
  // `marked` site must call it, and a `conditional` file must also strip the mark (unmarkedSpawnEnv).
  it("each file's spawn-mark calls agree with its sites' dispositions", () => {
    const byFile = new Map<string, Mark[]>();
    for (const [key, v] of Object.entries(ALLOWLIST)) {
      const file = key.slice(0, key.indexOf("#"));
      byFile.set(file, [...(byFile.get(file) ?? []), v.mark]);
    }
    for (const [file, marks] of byFile) {
      const text = fs.readFileSync(path.join(SRC, file), "utf8");
      const code = codeOf(text);
      const marks_ = new Set(marks);
      // An import alias (`workerSpawnEnv as markEnv`) is resolved, so a renamed call still counts.
      const callsMark = callsAny(code, callableNames(text, "workerSpawnEnv"));
      const callsUnmark = callsAny(code, callableNames(text, "unmarkedSpawnEnv"));
      if ([...marks_].every((m) => m === "unmarked")) {
        assert.equal(callsMark, false, `${file}: every site is unmarked, yet the file calls workerSpawnEnv`);
      }
      if (marks_.has("marked")) assert.equal(callsMark, true, `${file}: a marked site, yet no workerSpawnEnv call`);
      if (file === "executor.ts") {
        // Conditional across DIFFERENT sites: the stub's agent-stand-in git is unmarked here, and
        // the pr_summary HEAD read is marked through git.ts's runnerGitSpawnEnv (its runtime env is
        // pinned in pr-summary.test.ts).
        assert.equal(callsUnmark, true, "executor.ts: the stub git must strip the mark");
        assert.equal(callsAny(code, callableNames(text, "runnerGitSpawnEnv")), true, "executor.ts: the HEAD read must use runnerGitSpawnEnv");
      } else if (marks_.has("conditional") && file !== "provision.ts" && file !== "codex/launcher.ts") {
        // provision.ts / codex/launcher.ts are conditional across DIFFERENT sites (a model-directed
        // spawn built on a replacement env, a fixed probe marked); git.ts and tick-spawner.ts decide
        // inside ONE site, so they must carry both branches.
        assert.equal(callsMark && callsUnmark, true, `${file}: a conditional site must both mark and strip the mark`);
      }
    }
  });

  it("resolves an import alias of the mark functions", () => {
    const text = 'import { a, workerSpawnEnv as markEnv, unmarkedSpawnEnv } from "./worker-spawn-mark.js";\nconst e = markEnv(x);\n';
    assert.deepEqual(callableNames(text, "workerSpawnEnv"), ["workerSpawnEnv", "markEnv"]);
    assert.equal(callsAny(codeOf(text), callableNames(text, "workerSpawnEnv")), true, "the aliased call is seen");
    assert.equal(callsAny(codeOf(text), ["workerSpawnEnv"]), false, "the bare name alone would miss it");
  });

  it("no source file re-binds a mark function by value (only direct or import-aliased calls exist)", () => {
    // A value alias (`const m = workerSpawnEnv`, passing it as a callback) would hide calls from
    // the per-file check above; forbid it outright outside the defining module.
    for (const file of listTs(SRC)) {
      const rel = path.relative(SRC, file).split(path.sep).join("/");
      if (rel === "worker-spawn-mark.ts") continue;
      const text = fs.readFileSync(file, "utf8");
      const code = codeOf(text);
      for (const fn of MARK_FNS) {
        for (const name of callableNames(text, fn)) {
          assert.equal(new RegExp(`\\b${name}\\b(?!\\s*\\()`).test(code), false, `${rel}: \`${name}\` is referenced without being called`);
        }
      }
    }
  });

  it("the source scan actually sees calls (not vacuous)", () => {
    assert.ok((found()["tick-spawner.ts#runnerCommand()"] ?? 0) >= 1);
    // The direct-setpriv detectors see the two kill helpers in runner-uid.ts.
    assert.ok((found()["runner-uid.ts#setpriv-spawn"] ?? 0) >= 1);
    assert.ok((found()["runner-uid.ts#setprivRunnerArgs()"] ?? 0) >= 1);
  });
});
