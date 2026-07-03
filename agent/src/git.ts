import { execFile } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Logger } from "./log.js";

const execFileAsync = promisify(execFile);

const GIT_TIMEOUT_MS = 10 * 60_000; // 10m — clones can be large on cold caches.

export interface WorktreeResult {
  /** Absolute path to the checked-out worktree. */
  path: string;
  /** Branch the worktree is on — always `agent/issue-{iid}`. */
  branch: string;
}

/**
 * Bare-clone cache + per-run worktree lifecycle (PRD §Worker runtime; multica's
 * repocache pattern ported to TS).
 *
 * Layout under UZI_DATA_DIR:
 *   repos/<host>+<ns>+<repo>.git   — one bare clone per repo, kept across runs
 *   worktrees/<repo>/issue-<iid>   — one worktree per run, removed on terminal
 *
 * PAT handling (primary directive): the token is passed to authenticated git
 * ops (clone/fetch) via env-scoped config (GIT_CONFIG_KEY/VALUE) only, so it
 * never lands in the process argv (visible via `ps`) and is never written to
 * the bare repo's on-disk config. It is never logged: runGit logs args only,
 * and args never carry the token.
 */
export class GitCache {
  private readonly reposRoot: string;
  private readonly worktreesRoot: string;
  /** Per-bare-path serialization: git's lockfiles can't take parallel mutations. */
  private readonly locks = new Map<string, Promise<unknown>>();

  constructor(dataDir: string, private readonly log: Logger) {
    this.reposRoot = path.join(dataDir, "repos");
    this.worktreesRoot = path.join(dataDir, "worktrees");
  }

  barePathFor(repoUrl: string): string {
    return path.join(this.reposRoot, bareDirName(repoUrl));
  }

  /** Clone the repo bare if absent, else fetch to refresh. Returns the bare path. */
  async ensureClone(repoUrl: string, pat?: string): Promise<string> {
    const barePath = this.barePathFor(repoUrl);
    return this.withLock(barePath, async () => {
      await fs.mkdir(this.reposRoot, { recursive: true });
      if (await isBareRepo(barePath)) {
        this.log.info("repo cache: fetching", { bare: barePath });
        await this.fetch(barePath, pat);
      } else {
        this.log.info("repo cache: cloning bare", { url: repoUrl, bare: barePath });
        await this.cloneBare(repoUrl, barePath, pat);
      }
      return barePath;
    });
  }

  /**
   * Create the run's worktree on branch `agent/issue-{iid}`. If that branch
   * already exists in the bare repo (a resume, or a prior run on the same
   * issue), attach to it (agent-deck's worktree-as-ledger idempotency); else
   * create it off the repo's default branch. The worktree itself is disposable:
   * a stale one at the target path is removed and recreated cleanly.
   */
  async createOrAttachWorktree(barePath: string, issueIid: number): Promise<WorktreeResult> {
    return this.withLock(barePath, async () => {
      const branch = `agent/issue-${issueIid}`;
      const repoDir = path.basename(barePath).replace(/\.git$/, "");
      const worktreePath = path.join(this.worktreesRoot, repoDir, `issue-${issueIid}`);
      await fs.mkdir(path.dirname(worktreePath), { recursive: true });

      // Clear any stale worktree at the path and prune dangling admin entries so
      // `worktree add` can't fail with a confusing "already exists".
      await this.forceRemoveWorktree(barePath, worktreePath);
      await this.tryGit(barePath, ["worktree", "prune"]);

      if (await this.branchExists(barePath, branch)) {
        this.log.info("worktree: attaching existing branch", { branch, path: worktreePath });
        await this.runGit(barePath, ["worktree", "add", worktreePath, branch]);
      } else {
        const baseRef = await this.defaultBranchRef(barePath);
        this.log.info("worktree: creating branch", { branch, base: baseRef, path: worktreePath });
        await this.runGit(barePath, ["worktree", "add", "-b", branch, worktreePath, baseRef]);
      }
      return { path: worktreePath, branch };
    });
  }

  /** Remove the run's worktree; the bare clone and branch are kept. */
  async removeWorktree(barePath: string, worktreePath: string): Promise<void> {
    await this.withLock(barePath, async () => {
      await this.forceRemoveWorktree(barePath, worktreePath);
      await this.tryGit(barePath, ["worktree", "prune"]);
    });
  }

  private async cloneBare(repoUrl: string, dest: string, pat?: string): Promise<void> {
    try {
      await this.runGit(undefined, ["clone", "--bare", repoUrl, dest], pat);
    } catch (err) {
      await fs.rm(dest, { recursive: true, force: true });
      throw err;
    }
    // Convert the mirror refspec to remote-tracking so future fetches write to
    // refs/remotes/origin/* and never collide with the refs/heads/agent/* the
    // worktrees lock (multica).
    await this.runGit(dest, ["config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"]);
    await this.fetch(dest, pat);
  }

  private async fetch(barePath: string, pat?: string): Promise<void> {
    await this.runGit(barePath, ["fetch", "origin"], pat);
    // Refresh origin/HEAD so a remote default-branch change takes effect. Best
    // effort: defaultBranchRef has fallbacks if this symref is absent.
    await this.tryGit(barePath, ["remote", "set-head", "origin", "--auto"]);
  }

  private async branchExists(barePath: string, branch: string): Promise<boolean> {
    const code = await this.tryGit(barePath, ["rev-parse", "--verify", "--quiet", `refs/heads/${branch}`]);
    return code === 0;
  }

  /** Resolve a ref usable as a `worktree add` startpoint for the default branch. */
  private async defaultBranchRef(barePath: string): Promise<string> {
    // 1) origin/HEAD, set by `remote set-head --auto`.
    const head = await this.tryGitStdout(barePath, ["symbolic-ref", "refs/remotes/origin/HEAD"]);
    if (head && (await this.refExists(barePath, head))) return head;
    // 2) common defaults, remote-tracking then mirror-layout fallback.
    for (const cand of [
      "refs/remotes/origin/main",
      "refs/remotes/origin/master",
      "refs/heads/main",
      "refs/heads/master",
    ]) {
      if (await this.refExists(barePath, cand)) return cand;
    }
    // 3) last resort: the bare repo's own HEAD.
    if (await this.refExists(barePath, "HEAD")) return "HEAD";
    throw new Error(`cannot resolve default branch for bare repo ${barePath}`);
  }

  private async refExists(barePath: string, ref: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["rev-parse", "--verify", "--quiet", `${ref}^{commit}`])) === 0;
  }

  private async forceRemoveWorktree(barePath: string, worktreePath: string): Promise<void> {
    if (await pathExists(worktreePath)) {
      // --force twice tolerates a dirty worktree; fall back to rm if git refuses.
      const code = await this.tryGit(barePath, ["worktree", "remove", "--force", worktreePath]);
      if (code !== 0) await fs.rm(worktreePath, { recursive: true, force: true });
    }
  }

  // --- git subprocess plumbing -------------------------------------------------

  private async runGit(cwd: string | undefined, args: string[], pat?: string): Promise<string> {
    const env = gitEnv(pat);
    // Log args only; the PAT lives in env (GIT_CONFIG_VALUE_n), never in args.
    this.log.debug("git", { cwd, args });
    try {
      const { stdout } = await execFileAsync("git", withDir(cwd, args), {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: 64 * 1024 * 1024,
      });
      return stdout;
    } catch (err) {
      throw new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`);
    }
  }

  /** Run git, returning the exit code (0 on success) instead of throwing. */
  private async tryGit(cwd: string | undefined, args: string[], pat?: string): Promise<number> {
    try {
      await execFileAsync("git", withDir(cwd, args), { env: gitEnv(pat), timeout: GIT_TIMEOUT_MS });
      return 0;
    } catch (err) {
      const code = (err as { code?: unknown }).code;
      return typeof code === "number" ? code : 1;
    }
  }

  private async tryGitStdout(cwd: string | undefined, args: string[]): Promise<string> {
    try {
      const { stdout } = await execFileAsync("git", withDir(cwd, args), { env: gitEnv(), timeout: GIT_TIMEOUT_MS });
      return stdout.trim();
    } catch {
      return "";
    }
  }

  /** Serialize all mutations on a given bare repo (chained promises per path). */
  private withLock<T>(key: string, fn: () => Promise<T>): Promise<T> {
    const prev = this.locks.get(key) ?? Promise.resolve();
    const next = prev.then(fn, fn);
    // Keep the chain alive but swallow the stored result's rejection so one
    // failure doesn't poison every later op on the same repo.
    this.locks.set(key, next.catch(() => undefined));
    return next;
  }
}

function withDir(cwd: string | undefined, args: string[]): string[] {
  return cwd ? ["-C", cwd, ...args] : args;
}

/**
 * git subprocess env. safe.directory=* trusts the daemon-managed dirs
 * (ownership check adds nothing here and breaks when the container UID differs
 * from the volume owner); GIT_TERMINAL_PROMPT=0 turns auth failures into clear
 * errors instead of a hang. When a PAT is supplied it is injected as an
 * http.extraHeader via env-scoped config — off the argv, never on disk.
 */
function gitEnv(pat?: string): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = { ...process.env, GIT_TERMINAL_PROMPT: "0" };
  const pairs: Array<[string, string]> = [["safe.directory", "*"]];
  if (pat) {
    // AMBIGUITY (flagged in report): GitLab accepts `Authorization: Bearer
    // <PAT>` for git-over-HTTPS; the exact header is unverified against a live
    // GitLab until M4 wires real clones. M2 tests use local fixture repos, which
    // ignore the header entirely.
    pairs.push(["http.extraHeader", `Authorization: Bearer ${pat}`]);
  }
  let count = Number(env.GIT_CONFIG_COUNT ?? "0") || 0;
  for (const [k, v] of pairs) {
    env[`GIT_CONFIG_KEY_${count}`] = k;
    env[`GIT_CONFIG_VALUE_${count}`] = v;
    count++;
  }
  env.GIT_CONFIG_COUNT = String(count);
  return env;
}

function gitErrorMessage(err: unknown): string {
  const e = err as { stderr?: unknown; message?: unknown };
  const stderr = typeof e.stderr === "string" ? e.stderr.trim() : "";
  if (stderr) return stderr;
  return typeof e.message === "string" ? e.message : String(err);
}

async function pathExists(p: string): Promise<boolean> {
  try {
    await fs.stat(p);
    return true;
  } catch {
    return false;
  }
}

async function isBareRepo(p: string): Promise<boolean> {
  return pathExists(path.join(p, "HEAD"));
}

/**
 * Filesystem-safe, collision-free bare-clone dir name for a repo URL: host plus
 * each path segment joined by '+'. '+' is illegal in GitHub/GitLab path
 * segments, so two names collide only for the same repo on the same host
 * (ported from multica's bareDirName). Examples:
 *   https://gitlab.com/org/repo.git  -> gitlab.com+org+repo.git
 *   git@gitlab.com:org/repo          -> gitlab.com+org+repo.git
 *   /tmp/fx/origin                   -> tmp+fx+origin.git    (local path, hostless)
 */
export function bareDirName(rawUrl: string): string {
  const trimmed = rawUrl.replace(/\/+$/, "");
  const [host, repoPath] = splitHostAndPath(trimmed);
  const parts: string[] = [];
  const normHost = host.trim().toLowerCase().replaceAll(":", "%3A");
  if (normHost) parts.push(normHost);
  for (const seg of repoPath.split("/")) {
    if (seg) parts.push(seg);
  }
  let name = parts.join("+");
  if (!name.endsWith(".git")) name += ".git";
  if (name === ".git") name = "repo.git";
  return name;
}

function splitHostAndPath(rawUrl: string): [host: string, path: string] {
  try {
    const u = new URL(rawUrl);
    if (u.protocol && u.host) return [u.host, u.pathname.replace(/^\//, "")];
  } catch {
    // Not a URL — fall through to scp-style / local-path handling.
  }
  let s = rawUrl;
  const at = s.indexOf("@");
  if (at >= 0) s = s.slice(at + 1);
  const colon = s.indexOf(":");
  if (colon >= 0) return [s.slice(0, colon), s.slice(colon + 1)];
  return ["", s];
}
