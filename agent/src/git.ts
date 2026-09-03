import { execFile, spawn, type ChildProcess } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { randomUUID } from "node:crypto";
import type { Readable } from "node:stream";
import type { Logger } from "./log.js";
import { runnerCommand, runnerPath, runnerTmpdir } from "./runner-uid.js";
import { withForgeRetry } from "./forge-retry.js";
import { flagCIConfigPaths } from "./ci-config-guard.js";
import {
  commitsScannedFromStderr,
  gitleaksArgs,
  parseGitleaksReport,
  scanIsTrustworthy,
  type SecretFinding,
} from "./secret-scan-guard.js";

const execFileAsync = promisify(execFile);

// A ROOT-OWNED, non-writable (0555) empty dir BAKED into the worker image (see
// agent/templates/base/Dockerfile, created as root; the image has no `USER` line —
// the entrypoint drops to the non-root worker at runtime). Every worker git
// invocation sets core.hooksPath here (via gitEnv's own GIT_CONFIG pairs, which
// override any config file), so a hook that ANY non-root process tries to plant
// CANNOT be created — a non-root process cannot create a child inside a root-owned
// 0555 dir. That covers every planter regardless of uid: the agent's own Bash and the
// self_improve check-phase test code (both the `runner` uid under the PRD #51 split;
// out-of-worktree writes are the accepted PRD #42 residual) AND the worker itself. So
// no pre-push (or any) hook can fire (M10 audit).
//
// It is NOT created at runtime: a runtime mkdir would land under the SHARED uid and
// be agent-writable — exactly the vector this closes (relocating, not fixing). A
// nonexistent path is harmless (git simply runs no hooks), so this is safe on a
// host/image without the baked dir (e.g. a unit test), where no hooks run anyway.
const EMPTY_GIT_HOOKS_DIR = "/usr/share/uzi-git-nohooks";

// PRD #51 M0 — shared-git write→worker-execute hardening. git has several config keys
// whose value is run as a COMMAND during ordinary (even non-credentialed) operations. A
// process that can write a config source a WORKER-side git reads could plant one and get
// code-exec AS THE WORKER (the PAT/token holder) — no PAT required. The M10
// `core.hooksPath` pin closed hooks only; these close the other FIXED-name keys. Landed
// at M0 (pre-split); the threat surface NARROWED under the M3 (b) topology, and both
// states are described so the pins' role is unambiguous.
//
// Under (b) — the SHIPPED design (see the GitCache layout note below) — the worker is
// BARE-ONLY: it clones/fetches with the PAT, fetches the agent branch BACK over
// file://+pack, tree-diffs (`git diff --name-only origin/main...ref`, no working tree),
// and pushes. It NEVER runs `worktree add`/checkout, `status`, or a content diff, and it
// reads ONLY its own WORKER-OWNED bare `<bare>/config`, which the runner CANNOT write
// (config-source ownership). So a runner cannot plant a key the worker reads, and the
// checkout-/working-tree-only keys never fire worker-side at all. (The runner checks out
// its OWN clone as the runner uid via runGitAsRunner — the untrusted uid exec'ing in its
// own tree is not a boundary crossing; runGitAsRunner still uses this gitEnv base, so the
// pins ride it too.) In M0 (no split, shared agentdata, the WORKER itself did the
// checkout) these vectors WERE live worker-side; the pins were the M0 close and stay the
// close on a #58 single-uid start, where there is no split.
//
// We pin the FIXED-name keys via gitEnv's inline GIT_CONFIG_KEY/VALUE pairs, which are
// HIGHEST precedence (last-value-wins over every config file), so our value OVERRIDES a
// plant regardless of which config source it lives in — verified experimentally. Applied
// UNCONDITIONALLY on every gitEnv git (worker AND runner), so they also neutralize a key
// in the runner's own clone config.
//
// The values are the empirically-verified inert overrides (git 2.55). Two groups:
//
// Command-valued keys — the value is a program git runs, so there is NO boolean
// "disabled" form; the pin is a harmless no-op command that wins by precedence:
//   - core.fsmonitor=false — boolean-false disables the monitor hook (it fires on a
//     working-tree scan — `status`/`add`/checkout — which the bare-only worker never does;
//     pinned defensively + for the runner's checkout).
//   - diff.external=true — git runs the value as an external-diff command via the shell,
//     so there is NO "disabled" value; `""`/`false`/`cat` all get exec'd (and error).
//     `true` is a shell builtin no-op → neutralizes the plant, exits 0, and emits no
//     external diff. The worker's only diff (changedFiles) is `--name-only`, which never
//     invokes diff.external anyway, so this is defensive (and covers the runner's git).
//   - core.pager=cat — disables paging (the git-documented no-op pager). Only ever
//     reached with a tty or `--paginate`, neither of which the piped worker git hits, but
//     pinned defensively.
//   - core.sshCommand=ssh — reverts any plant to the default ssh program. The worker's
//     forge transport is https + local-file only (never ssh), so this is pure
//     belt-and-suspenders; it cannot exec an attacker-chosen program.
//
// Auth / ref keys — reachable when a WORKER-side credentialed op hits an auth challenge
// (401/407) and git runs `git credential fill`, or when it enumerates alternate refs. For
// these an EMPTY value is the inert override (verified: plant fires, empty pin neutralizes)
// — added by the M0 audit (MEDIUM/LOW):
//   - credential.helper="" — an empty value RESETS git's accumulated helper list, so a
//     planted `[credential] helper = !evil` is dropped (append-and-reset, not last-wins).
//     The worker authenticates via the http.extraHeader Basic pair, so it needs no helper
//     — dropping the list cannot break its auth.
//   - core.askpass="" — empty overrides a planted askpass so git skips it (it would
//     otherwise run on a password challenge, even with GIT_TERMINAL_PROMPT=0).
//   - core.alternateRefsCommand="" — empty falls back to git's built-in alternate-ref
//     enumeration (no external command). The worker's bare clones carry no
//     `objects/info/alternates`, so it never fires today; pinned defensively.
//
// NOT pinnable / excluded, with the reason:
//   - core.gitProxy — a MULTIVAR: an appended inline entry does NOT override a planted one
//     (verified: planted proxy still fires under an inline `none`/``), and `git://` is the
//     only transport that consults it — which the worker never uses (https + local-file).
//     Excluded: not inline-pinnable AND not reachable.
//   - core.editor / sequence.editor — no worker fire point: every git this env runs is
//     non-interactive (clone/fetch/push/diff/config), and the agent's OWN `git commit` uses
//     the SDK Bash env, NOT gitEnv. Excluded; pin if an interactive git op is ever routed
//     through gitEnv (reviewer nit — my call).
//   - The ARBITRARY-name keys `filter.<name>.*` (smudge/clean/process) / `diff.<name>.*`
//     (command/textconv) / `merge.<name>.driver` CANNOT be blanket-pinned (driver names are
//     attacker-chosen). They only fire on a checkout/content-diff — which the bare-only
//     worker NEVER performs under (b), so they never reach the worker at all; the close is
//     config-source OWNERSHIP (the worker reads only its worker-owned bare config, which the
//     runner cannot write, and the worker keeps NO worktrees, so there is no
//     `config.worktree` to reach). Where the runner checks out its own clone, such a key is
//     the untrusted uid exec'ing in its OWN tree — not a boundary crossing. In M0 (no split,
//     the worker did the checkout) they WERE reachable worker-side; (b) removed that path.
const GIT_CODE_EXEC_KEY_PINS: ReadonlyArray<readonly [key: string, value: string]> = [
  ["core.fsmonitor", "false"],
  ["diff.external", "true"],
  ["core.pager", "cat"],
  ["core.sshCommand", "ssh"],
  ["credential.helper", ""],
  ["core.askpass", ""],
  ["core.alternateRefsCommand", ""],
];

const GIT_TIMEOUT_MS = 10 * 60_000; // 10m — clones can be large on cold caches.

// PRD #400 M4b — byte cap on the review diff a ReviewRunner feeds the reviewer model.
// A huge diff must not blow the model's context window or the worker's memory, so the
// diff is truncated at this size with a marker. 512 KiB is generous for a task-run diff
// while staying an order of magnitude under runGit's 64 MiB maxBuffer.
const REVIEW_DIFF_MAX_BYTES = 512 * 1024;

// PRD #974 M2 — cap the gitleaks JSON report read in secretScanRange. The report grows with
// the finding COUNT over attacker-authored commits, so an adversarial repo could inflate it
// past memory; an over-cap report is treated as an untrusted scan (fail open to the GH013
// backstop) rather than read into memory. 16 MiB holds far more findings than any honest push.
const SECRET_SCAN_REPORT_MAX_BYTES = 16 * 1024 * 1024;

// PRD #51 M3 — (b) separate-runner-clone: the worker-side tracking-ref namespace the
// worker's fetch-back writes the agent branch into. Deliberately NOT refs/heads/* (B2
// invariant 2: the runner's branch is admitted only into a demarcated worker-side
// tracking namespace, never commingled with the bare's heads); pushBranch then pushes
// FROM this ref to origin's refs/heads/<branch>. The worker owns its bare, so this ref
// is worker-controlled and read only by worker-side git on gitEnv.
const RUNNER_TRACKING_PREFIX = "refs/uzi-runner/";
function runnerTrackingRef(branch: string): string {
  return `${RUNNER_TRACKING_PREFIX}${branch}`;
}

// PRD #218 — the run-identity ANCHOR for a tracking ref. `refs/uzi-runner/<branch>` is
// per-BRANCH and nothing ever deletes it, so on its own it cannot tell "the run that
// wrote this parked its own work" from "a DIFFERENT, permanently-dead run left an orphan
// on the same issue" — the auditor's counterexample (run A parks and dies; a fresh run B
// on the same issue is later killed mid-turn and requeued, and would seed off A's commits
// while claiming to have recovered its own). So alongside the ref we stamp the writing
// run's id into the worker bare's own config, and the reseed reads the tracking ref back
// ONLY when the stamp matches the claiming run. This is a worker-owned `config --local`
// write, the same posture as disableAutoMaintenance — no new trust boundary.
//
// issue #887 — the branch is carried in a git-config SUBSECTION, not flattened into the
// variable name. git parses `section.subsection.variable` by the FIRST and LAST dot, so
// the middle keeps the branch VERBATIM (slashes and dots included) and the variable is the
// fixed `owner`. The earlier form flattened `/`->`-` into the variable name, which collided:
// `uzi/self-improve` and a literal `uzi-self-improve` mapped to the same key, so clearing
// one branch's owner stamp (clearConflictingAncestorTrackingRefs) could wipe the other's
// and cost a later resume its unpushed recovery state. The subsection encoding is reversible
// and collision-free — distinct branches always land in distinct `[uzi-trackowner "<branch>"]`
// blocks. (Legacy two-part stamps on a persistent bare become dead config no read touches.)
function runnerTrackingOwnerKey(branch: string): string {
  return `uzi-trackowner.${branch}.owner`;
}

// issue #909 — the PRE-#887 flattened owner-key form. Kept ONLY so a resume can still READ a
// stamp a persistent bare wrote under old code during the rollout window. flatten() is lossy
// (`/`, `.` -> `-`), so this key is NOT branch-injective: never WRITE under it, and read it only
// through readTrackingOwner()'s collision guard plus the caller's runId-equality gate.
function legacyFlatTrackingOwnerKey(branch: string): string {
  return `uzi-trackowner.${branch.replace(/[^A-Za-z0-9_-]/g, "-")}`;
}

/**
 * PRD #1062 M2 (#1036) — the optional context `checkpointPack` needs to build the
 * `.github/workflows` overlay wrapper commit. Supplied by the runner ONLY on GitHub and ONLY
 * on a path where the agent tree is already reaped (a PAT git op — `fetchDefaultTip` — runs
 * here). Undefined ⇒ `checkpointPack` ships the raw tracking tip, byte-for-byte as before.
 */
export interface CheckpointOverlayContext {
  /** The default branch name (already resolved the way the finalize align resolves it). */
  defaultBranch: string;
  /** The forge PAT for the authenticated default-tip fetch. */
  pat?: string;
  /** The repo clone URL, used to scope the PAT's HTTP header. */
  cloneUrl?: string;
  /** The forge bot username for HTTP Basic auth. */
  username?: string;
  /** The current tip of `refs/uzi-checkpoints/<branch>` as the run last knows it. When a
   *  40-hex value, it becomes the overlay's FIRST parent (base-first) so a second sequential
   *  overlay strictly descends the prior one and the broker accepts it as a fast-forward. */
  prevCheckpointTip?: string;
}

export interface RunnerClone {
  /** Absolute path to the runner clone's working tree (the ONLY working tree under
   *  (b) — the worker is bare-only). The agent checks out + commits here. */
  path: string;
  /** Branch the runner clone is on — `agent/issue-{iid}`, `ci-fix/…`, or `uzi/…`. */
  branch: string;
  /** How many commits the seeded branch already carries that the default branch does
   *  not. 0 when seeded off the default branch, and 0 when the count could not be
   *  taken (best-effort: this is prompt/feed colour, never load-bearing).
   *
   *  WHAT IT COUNTS DEPENDS ON `seededFrom`, and PRD #218 made that split load-bearing:
   *   - `"origin"`  — prior PUSHED work the clone was based off: an earlier completed
   *     run on the same issue, a RESUMED self_improve cycle's own previously-pushed
   *     commits (its branch is fresh-per-cycle, keyed on runId — #686 M8), or a
   *     human's commits. This is the original contract (issue #105): a run pushes
   *     exactly once, after the executor returns, and a fresh seed `fs.rm`s and
   *     re-clones, so a fresh claim never sees THIS attempt's commits here.
   *   - `"tracking"` — the commits RECOVERED from the interrupted attempt's own work,
   *     read back from `refs/uzi-runner/<branch>` (PRD #218 M2). This INVERTS the
   *     original "NOT the commits the interrupted attempt made" wording on purpose:
   *     on the tracking-ref leg the interrupted attempt's commits are exactly what the
   *     fetch-back preserved and the reseed is recovering, and M3 turns this number
   *     into a lead-facing "recovered N commit(s)" status.
   *   - `"default"` — 0 (the seed IS the default tip). */
  priorCommits: number;
  /** The commit the branch was checked out AT, resolved off the fresh remote-tracking ref
   *  in the bare (`refs/remotes/origin/*`, which every fetch updates) WHERE ONE EXISTS: the
   *  default branch's current tip for a new branch, the branch's own current origin tip on
   *  a resume. Full 40-char SHA.
   *
   *  "Where one exists" is load-bearing, not hedging. `defaultBranchRef` is a fallback chain
   *  and its last three rungs — `refs/heads/main`, `refs/heads/master`, then the bare's own
   *  `HEAD` — are the MIRROR-LAYOUT fallback, i.e. exactly the frozen refs this comment warns
   *  about. On those rungs `baseCommit` IS the frozen mirror, and so is `defaultBranchCommit`,
   *  since defaultBranchSha resolves through the same function.
   *
   *  How reachable that is, stated as what was and was not established: `cloneBare` rewrites
   *  the refspec and fetches before returning, so the remote-tracking refs exist by the time
   *  any normal run resolves a base. But its post-clone `fetch` is NOT covered by the
   *  `fs.rm` that cleans up a failed `clone`, so a bare whose first fetch died can persist
   *  on disk carrying `refs/heads/*` only. Whether a run then reaches `defaultBranchRef` in
   *  that state was NOT determined — it needs a warm bare in that condition. Treat this as a
   *  narrow window rather than a routine path, and do not assume the base is fresh when the
   *  bare has no `refs/remotes/origin/*`.
   *
   *  Load-bearing for the lead, which otherwise has to GUESS the branch's parent from the
   *  clone's local default branch. That ref is NOT a substitute, and not for the reason an
   *  earlier version of this comment gave: the clone's LOCAL `refs/heads/main` is a FROZEN
   *  MIRROR. `cloneBare` rewrites the refspec to `+refs/heads/*:refs/remotes/origin/*`, so the
   *  bare's own `refs/heads/*` never move after the first clone, and the runner clone inherits
   *  that frozen head as its local `main`. Its `origin/main` used to inherit the same frozen
   *  head, but `runnerCloneForBranch` now `update-ref`s `refs/remotes/origin/<default>` after
   *  checkout (issue #262) so the ratchet base is fresh; on a fresh run or ordinary resume that
   *  write is the fresh default head (`defaultBranchCommit`). Issue #313 CLAMPS it: on a resume
   *  leg where `defaultBranchCommit` resolves through the frozen `refs/heads/main` rung to a
   *  stale ancestor of `baseSha`, the ref is set to `baseSha` instead, so the ratchet base is
   *  never a strict ancestor of the branch base; a divergent-forward resume (main moved ahead)
   *  keeps `defaultBranchCommit`. The clone's local `refs/heads/main` is left untouched and
   *  stays frozen.
   *
   *  The drift therefore has no predictable direction — the mirror can sit at, behind, or
   *  ahead of this commit, one per topology — so neither `main..HEAD` nor `main...HEAD` is
   *  reliable, and prompt.ts's note forbids the ref NAME in both dot forms while predicting
   *  no symptom. (This comment used to assert `main..HEAD` reports the default branch's
   *  commits as a DELETION. Measured on a drifted bare, they are ADDITIONS, and the two
   *  dot forms return the same diff; see the block above baseCommitNote in prompt.ts.)
   *
   *  NOT the branch's fork point on a resume — see defaultBranchCommit, which is the
   *  distinction that makes the note correct on the runs that carry prior work. */
  baseCommit: string;
  /** The DEFAULT branch's current tip in the bare. Equal to `baseCommit` on a fresh
   *  branch (the seed IS the default tip); on a resume it is the other commit the lead
   *  may want, because `baseCommit` is then the branch's own previously-pushed tip and
   *  `baseCommit..HEAD` shows only what THIS run added.
   *
   *  Best-effort, exactly like priorCommits: a repo with no resolvable default branch
   *  yields undefined and the prompt says less rather than saying something false. */
  defaultBranchCommit?: string;
  /** Which leg of the reseed resolved the base (PRD #218 M2). `"origin"` when the
   *  branch already existed at origin, `"tracking"` when the base came from the
   *  worker-side tracking ref `refs/uzi-runner/<branch>` (the interrupted attempt's
   *  recovered work), `"default"` when neither applied and the seed is the default
   *  tip. Read by the runner to decide what M3's feed status says: `"tracking"` names
   *  the recovered commit count, `"default"` on a RESUME admits the tree was lost.
   *
   *  PRD #122 M8 adds `"checkpoint"`: on a NOT-ownedHere leg (cross-worker / fresh)
   *  the base came from origin's mirrored `refs/uzi-checkpoints/<branch>` — a DIFFERENT
   *  worker's brokered checkpoint that strictly descends the floor (origin branch, else
   *  default). It is a resume leg like `"tracking"`, so priorCommits counts the recovered
   *  commits. */
  seededFrom: "origin" | "tracking" | "default" | "checkpoint";
  /** PRD #122 M8 — true when a mirrored checkpoint ref existed but LOST to origin/default
   *  because it had diverged (not a strict descendant of the floor). Origin/default wins on
   *  divergence — never a silent merge or discard — and the runner emits a LOUD worker
   *  notice so the set-aside work is not lost silently. Absent/false on every other leg. */
  checkpointSetAside?: boolean;
  /** PRD #759 M2 — true when a `wip(park):` marker (WIP_PARK_COMMIT_PREFIX) was adopted as
   *  the base tip and `git reset --soft`'d back to uncommitted at adopt time, so the
   *  recovered content is present in the working tree as UNCOMMITTED changes and the marker
   *  is NOT in the history the agent builds on. Set on two legs: the same-worker /
   *  cross-worker-clean tracking/checkpoint leg (the adopted tip IS the marker, reset --soft
   *  onto its parent), and the cross-worker DIVERGED leg (the checkpoint marker's delta was
   *  cherry-pick --no-commit'd onto the new floor). Consumed by M5 (feed event distinguishing
   *  WIP-snapshot recovery from committed-milestone recovery) and M4 (recovery-success signal
   *  for the re-gate decision). Absent/false on every other leg. */
  wipRecovered?: boolean;
}

/**
 * Warm bare-clone cache (worker-owned) + per-run RUNNER CLONE lifecycle (PRD #51
 * M3, (b) separate-runner-clone).
 *
 * Layout under UZI_DATA_DIR:
 *   repos/<host>+<ns>+<repo>.git   — WORKER-ONLY warm bare clone per repo, kept
 *                                    across runs (config/hooks/refs/objects). The
 *                                    worker is BARE-ONLY: it clones/fetches with the
 *                                    PAT, fetches the agent branch BACK from the
 *                                    runner clone, tree-diffs, and pushes — it never
 *                                    runs worktree add / checkout.
 *   runner/<repo>/<key>            — RUNNER-OWNED clone per run (the ONLY working
 *                                    tree). Seeded from the worker bare via a local
 *                                    `clone --shared` (objects referenced read-only
 *                                    from the bare — the runner cannot corrupt the
 *                                    bare's objects through the alternate); the agent
 *                                    checks out + commits here. Removed on terminal.
 *
 * The (b) split closes the shared-git cross-uid channels by construction (B2): no
 * worker-side git ever reads a runner-owned config source (no worktree add checkout,
 * no shared commondir), and the worker fetches the agent branch back over the
 * pack-protocol `file://` transport (never the local-copy optimization that would
 * traverse a runner-planted objects/info/alternates — CVE-2022-39253 class).
 *
 * PAT handling (primary directive): the token is passed to authenticated git ops
 * (clone/fetch/push to origin) via env-scoped config (GIT_CONFIG_KEY/VALUE) only, so
 * it never lands in the process argv (visible via `ps`) and is never written to the
 * bare repo's on-disk config. It is never logged: runGit logs args only, and args
 * never carry the token. The runner-clone seed and the worker's fetch-BACK are LOCAL
 * (no credential).
 */
export class GitCache {
  private readonly reposRoot: string;
  /** Runner-owned clone store (the working trees). A distinct /data subtree from the
   *  worker-only `repos/` bare cache, so the M3 ownership carve-out is a clean
   *  boundary: `runner/` is runner-writable, `repos/` stays worker-only. */
  private readonly runnerRoot: string;
  /** Per-bare-path serialization: git's lockfiles can't take parallel mutations. */
  private readonly locks = new Map<string, Promise<unknown>>();

  constructor(
    dataDir: string,
    private readonly log: Logger,
    /** Test-only seam for the ensureClone network-op retry. Undefined in production,
     *  so withForgeRetry falls back to its own FORGE_RETRY_SCHEDULE + real sleep. */
    private readonly retry?: { schedule?: number[]; sleep?: (ms: number) => Promise<void> },
  ) {
    this.reposRoot = path.join(dataDir, "repos");
    this.runnerRoot = path.join(dataDir, "runner");
  }

  barePathFor(repoUrl: string): string {
    return path.join(this.reposRoot, bareDirName(repoUrl));
  }

  /** Clone the repo bare if absent, else fetch to refresh. Returns the bare path. */
  async ensureClone(repoUrl: string, pat?: string, username?: string): Promise<string> {
    const barePath = this.barePathFor(repoUrl);
    const scope = httpScopeForUrl(repoUrl);
    return this.withLock(barePath, async () => {
      await fs.mkdir(this.reposRoot, { recursive: true });
      if (await isBareRepo(barePath)) {
        this.log.info("repo cache: fetching", { bare: barePath });
        // Issue #134: reassert IDEMPOTENTLY on the warm path, BEFORE the fetch (the first
        // object-writing command, hence the first spawner). cloneBare only runs on the very
        // first clone, and `/data` is persistent — a per-worker PVC in k8s, the `agentdata`
        // volume under compose — so every bare on an already-deployed worker would otherwise
        // never receive these keys at all. That is the exact case the bare-repo reasoning
        // below is about, so writing it only in cloneBare left it applied to none of them.
        await this.disableAutoMaintenance(barePath);
        await withForgeRetry(() => this.fetch(barePath, pat, scope, username), {
          schedule: this.retry?.schedule,
          sleep: this.retry?.sleep,
          log: this.log,
        });
      } else {
        this.log.info("repo cache: cloning bare", { url: repoUrl, bare: barePath });
        await withForgeRetry(() => this.cloneBare(repoUrl, barePath, pat, scope, username), {
          schedule: this.retry?.schedule,
          sleep: this.retry?.sleep,
          log: this.log,
        });
      }
      return barePath;
    });
  }

  /**
   * Push the run's branch to origin using the PAT. This is a WORKER-owned
   * authenticated op — the agent never has a push credential — so it runs here,
   * not through the SDK's guardrailed Bash. The PAT rides the host-scoped
   * extraHeader in the env (off argv, off disk), and the push is never forced.
   * Idempotent on resume: a branch already at origin pushes as up-to-date.
   *
   * Under (b) the agent's commit lives in the RUNNER clone, so the source is the
   * worker-side tracking ref `fetchAgentBranch` wrote (refs/uzi-runner/<branch>),
   * NOT refs/heads/<branch> — the caller MUST fetchAgentBranch first. Pushing from
   * the tracking ref keeps the runner's branch out of the bare's heads namespace
   * (B2 invariant 2) while landing the agent's commits at origin's refs/heads.
   */
  async pushBranch(barePath: string, branch: string, pat: string, repoUrl: string, username?: string): Promise<void> {
    const scope = httpScopeForUrl(repoUrl);
    const src = runnerTrackingRef(branch);
    await this.withLock(barePath, async () => {
      if (!(await this.refExists(barePath, src))) {
        // The tracking ref is written by fetchAgentBranch; its absence means the
        // fetch-back was skipped — refuse rather than silently pushing nothing.
        throw new Error(`cannot push ${branch}: ${src} not present (fetchAgentBranch must run first)`);
      }
      await this.runGit(barePath, ["push", "origin", `${src}:refs/heads/${branch}`], pat, scope, username);
    });
  }

  /** The default branch's short name (e.g. `main`), for an MR target. */
  async defaultBranchName(barePath: string): Promise<string | undefined> {
    const ref = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!ref) return undefined;
    return ref.replace(/^refs\/remotes\/origin\//, "").replace(/^refs\/heads\//, "") || undefined;
  }

  /**
   * Seed the run's RUNNER CLONE on branch `agent/issue-{iid}` (PRD #51 M3). The
   * working tree lives ONLY here (the worker is bare-only). If the branch already
   * exists at origin (a resume, or a prior run on the same issue — fetched into the
   * bare's refs/remotes/origin/<branch>), the clone's branch is based off that fresh
   * tip so successive runs build on prior work; else off the repo's default branch.
   */
  async createOrAttachRunnerClone(barePath: string, issueIid: number, runId?: string, resume = false, expectedCheckpointTip?: string): Promise<RunnerClone> {
    return this.runnerCloneForBranch(barePath, `agent/issue-${issueIid}`, `issue-${issueIid}`, runId, resume, expectedCheckpointTip);
  }

  /**
   * Seed a RUNNER CLONE for an EXPLICIT branch — the PRD #6 ci_fix targets (a fresh
   * `ci-fix/pipeline-{id}` off the default branch, or an existing `agent/issue-{iid}`
   * run branch, updating its MR) and the PRD #46 self_improve branch.
   * `key` names the on-disk clone dir (branch names carry `/`, so callers pass a
   * filesystem-safe key). The cross-kind same-branch exclusion (server-side) plus the
   * per-run clone dir means a stale dir is simply removed and recloned.
   *
   * Base resolution (PRD #218 M2). Three candidates: `refs/remotes/origin/<branch>`
   * (pushed work), the worker-side tracking ref `refs/uzi-runner/<branch>` (an
   * interrupted attempt's fetched-back work), and the default branch. The tracking ref
   * is consulted ONLY when it is OWNED BY THIS RUN — its stamp (see
   * runnerTrackingOwnerKey) equals `runId`. That anchor is what stops a fresh run, or a
   * different run on the same issue, from inheriting a dead run's orphan ref (it would
   * reintroduce issue #105's silent redo through this very fix). `ownedHere` gates the
   * WHOLE tracking-ref consideration:
   *   - NOT owned here (a fresh run, a stale ref from another run, or `runId` undefined):
   *     the tracking ref is IGNORED entirely — origin/<branch> if it exists, else the
   *     default. This is today's pre-#218 behaviour.
   *   - owned here, origin AND tracking both exist: the tracking ref ONLY when
   *     `merge-base --is-ancestor origin <tracking>` holds (it strictly descends from
   *     origin); on divergence, origin — another worker pushed and silently preferring
   *     local work would drop a published commit.
   *   - owned here, tracking exists but no origin branch (the first-park case): the
   *     tracking ref, with NO ancestry test — there is no competing published work to
   *     protect and the current default tip is not a meaningful reference (it may have
   *     moved far past the fork point, which is exactly the case a uniform ancestor
   *     test discards the recovered work on).
   *
   * PRD #122 M8 adds a FOURTH candidate, `refs/uzi-checkpoints/<branch>`, the CROSS-WORKER
   * signal path: a DIFFERENT worker brokered its checkpoint to origin (M8 publish), and
   * `fetch()` mirrored origin's checkpoint refs into this bare. It matters ONLY on the
   * NOT-ownedHere legs — when THIS run owns the local tracking ref, that ref is
   * equal-or-ahead of any checkpoint (the checkpoint was pushed FROM the tracking state),
   * so the checkpoint adds nothing and the ownedHere legs above are unchanged. On a
   * not-ownedHere leg the floor is the origin branch if pushed, else the default; the
   * checkpoint is preferred ONLY when it is THIS run's own (the owner anchor below) AND
   * strictly descends that floor. On divergence of an owned checkpoint origin/default WINS —
   * never a silent merge or discard — and `checkpointSetAside` is set so the runner emits a
   * loud notice.
   * The git layer is claim-agnostic: `runId` is a plain string the runner threads from
   * `claim.run_id`; nothing here knows about sessions. `runId` undefined ⇒ never owned ⇒
   * today's behaviour, which keeps the no-runId test call sites compiling.
   *
   * PRD #1030 M3 — `resume` is a SEPARATE additional signal the runner threads from
   * `claim.session_id != null` (a resume, not a fresh first attempt). It is NOT the runId
   * ownership anchor and does not affect the ownedHere legs. It relaxes ONLY the
   * cross-worker checkpoint-adoption rule on the not-ownedHere leg: on a resume with NO
   * `origin/<branch>` (an unpushed branch), a mirrored checkpoint that shares history with
   * the default is adopted with no strict-descendant test against the current default, which
   * would wrongly discard a valid checkpoint when `main` advanced during the park. The strict
   * test is KEPT when `origin/<branch>` exists (genuinely competing published work) and for
   * every fresh (non-resume) run. `resume` defaults false ⇒ today's behaviour, which keeps
   * the existing test call sites compiling.
   *
   * issue #1042 M4 / #1059 M1 — checkpoint adoption is gated on an OWNER ANCHOR:
   * `expectedCheckpointTip` (threaded from `claim.checkpoint_tip`, the tip THIS run last
   * published to its own checkpoint ref, persisted server-side on every publish — M2). A
   * checkpoint ref is per-BRANCH, so sharing history with the default is not enough — the
   * mirrored checkpoint might be a PRIOR (possibly plan-rejected) or foreign run's work.
   * #1042 M4 anchored the resume-adopt leg only; #1059 M1 extends the SAME anchor to EVERY
   * adoption leg on the not-ownedHere path — both the resume-relaxed leg and the
   * strict-descendant leg — via a single owner-gated predicate. Adopt (or set aside for the
   * #759 cherry-pick) ONLY when `expectedCheckpointTip` is a non-empty string AND equals the
   * mirrored checkpoint's current SHA; a NULL/empty tip (a run that never published) or a
   * mismatch (a foreign/prior checkpoint) falls through to the origin/default floor, LOUDLY,
   * and is NEVER set aside (so the #759 cherry-pick cannot re-import that foreign work). A
   * same-run legitimate resume still adopts, because its persisted tip advanced with its own
   * checkpoint and still matches. `expectedCheckpointTip` defaults undefined ⇒ no adoption ⇒
   * keeps existing test call sites conservative.
   *
   * The seed is a LOCAL `clone --shared` from the worker bare: fast (objects are
   * referenced read-only from the bare via the clone's objects/info/alternates — the
   * runner cannot corrupt worker-bare objects through it), and the runner's own new
   * commit objects land in the clone's own objects store. The clone is a TRUSTED-source
   * local clone (worker bare → runner clone), so the local optimization here is safe;
   * the untrusted direction (worker fetching BACK from the runner clone) is the one
   * forced onto the pack transport in fetchAgentBranch (B2 invariant 3).
   */
  async runnerCloneForBranch(barePath: string, branch: string, key: string, runId?: string, resume = false, expectedCheckpointTip?: string): Promise<RunnerClone> {
    return this.withLock(barePath, async () => {
      const repoDir = path.basename(barePath).replace(/\.git$/, "");
      const clonePath = path.join(this.runnerRoot, repoDir, key);
      await fs.rm(clonePath, { recursive: true, force: true });
      // The clone's parent dir. Under the M4 split it must be group-`runner`-writable so
      // the runner-uid `git clone` can create <key> inside it: /data/runner is
      // worker:runner 2775 (setgid) from the entrypoint, and the worker runs with umask
      // 002 (main.ts), so this mkdir is 2775 group `runner` — the runner (a `runner`-group
      // member) can then create its clone here. Single-uid (#58): plain worker dir.
      await fs.mkdir(path.dirname(clonePath), { recursive: true });

      // Resolve the base commit in the BARE (authoritative), per the PRD #218 M2 table
      // documented above. All three candidate refs live in the bare (the clone does not
      // necessarily carry the default branch), so the resolution happens here.
      const originRef = `refs/remotes/origin/${branch}`;
      const trackingRef = runnerTrackingRef(branch);
      // issue #781 — disjoint-history guard. Resolve the default ref ONCE up front, then
      // qualify EACH candidate base ref: it counts as existing only if it also shares
      // history with the default branch. A candidate that exists but is disjoint from
      // default (a stale ref whose remote counterpart was rebuilt from an orphan root, or
      // a leftover from an unrelated branch reused) is treated as ABSENT, so the seed falls
      // back cleanly to the default tip instead of seeding off unrelated history. The guard
      // qualifies up front (not a post-override): each candidate's downstream logic below is
      // unchanged for a candidate that DOES share history.
      // Best-effort: if the default cannot be resolved we cannot prove disjointness, so
      // every candidate is kept (byte-identical to pre-#781 behaviour on this leg). The
      // not-ownedHere/no-origin else leg still resolves the default authoritatively for its
      // floor below, and throws there if truly unresolvable — unchanged.
      let defaultRef: string | undefined;
      try {
        defaultRef = await this.defaultBranchRef(barePath);
      } catch {
        defaultRef = undefined;
      }
      const originExistsRaw = await this.refExists(barePath, originRef);
      const originDisjoint = originExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, originRef, defaultRef));
      if (originDisjoint) {
        this.log.warn(
          "runner clone: origin branch ref is disjoint from default — ignoring, seeding off default tip",
          { branch, originRef },
        );
      }
      const originExists = originExistsRaw && !originDisjoint;
      // The tracking ref is consulted ONLY when its stamp says THIS run wrote it — the
      // run-identity anchor that gates the whole consideration (see the doc comment). A
      // disjoint tracking ref is treated as absent so it cannot make `ownedHere` true.
      const trackingExistsRaw = await this.refExists(barePath, trackingRef);
      const trackingDisjoint = trackingExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, trackingRef, defaultRef));
      if (trackingDisjoint) {
        this.log.warn(
          "runner clone: tracking ref is disjoint from default — ignoring, seeding off default tip",
          { branch, trackingRef },
        );
      }
      const trackingExists = trackingExistsRaw && !trackingDisjoint;
      const owner = trackingExists
        ? await this.readTrackingOwner(barePath, branch)
        : "";
      const ownedHere = trackingExists && runId !== undefined && owner === runId;
      // PRD #122 M8 cross-worker candidate: origin's checkpoint ref, mirrored into the bare
      // by fetch(). Consulted ONLY on the not-ownedHere legs (see the doc comment). A
      // disjoint checkpoint is treated as absent (so no checkpointSetAside is emitted for it).
      const checkpointRef = `refs/uzi-checkpoints/${branch}`;
      const checkpointExistsRaw = await this.refExists(barePath, checkpointRef);
      const checkpointDisjoint = checkpointExistsRaw && defaultRef !== undefined
        && !(await this.sharesHistory(barePath, checkpointRef, defaultRef));
      if (checkpointDisjoint) {
        this.log.warn(
          "runner clone: checkpoint ref is disjoint from default — ignoring, seeding off default tip",
          { branch, checkpointRef },
        );
      }
      const checkpointExists = checkpointExistsRaw && !checkpointDisjoint;

      let baseRef: string;
      let seededFrom: RunnerClone["seededFrom"];
      let checkpointSetAside = false;
      if (ownedHere && originExists) {
        // Both present: prefer the recovered local work ONLY when it strictly descends
        // from origin; on divergence origin wins so a published commit is never dropped.
        const descends = await this.isAncestor(barePath, originRef, trackingRef);
        baseRef = descends ? trackingRef : originRef;
        seededFrom = descends ? "tracking" : "origin";
      } else if (ownedHere) {
        // First park: recovered work with no competing published branch — no ancestry
        // test, because the current default tip is not a meaningful reference here.
        baseRef = trackingRef;
        seededFrom = "tracking";
      } else {
        // NOT owned here — cross-worker / fresh. Floor = origin branch if pushed, else
        // default. A mirrored checkpoint (PRD #122 M8) is adopted ONLY when it is THIS run's
        // OWN checkpoint (the owner anchor, issue #1059 M1); a foreign/prior or never-published
        // checkpoint is set aside off the floor, LOUDLY, and never re-imported.
        const floorRef = originExists ? originRef : await this.defaultBranchRef(barePath);
        const floorFrom: RunnerClone["seededFrom"] = originExists ? "origin" : "default";
        baseRef = floorRef;
        seededFrom = floorFrom;
        if (checkpointExists) {
          // issue #1059 M1 — a SINGLE owner-gated adopt predicate over BOTH adoption legs.
          // Before this, the resume-adopt leg (Path A below) was owner-anchored (#1042 M4)
          // while the strict-descendant leg (Path B) was NOT: a fresh cross-worker run would
          // adopt ANY mirrored checkpoint that strictly descended the floor, and on divergence
          // set it aside (→ the #759 cherry-pick), regardless of whether the checkpoint was
          // THIS run's own work or a PRIOR/FOREIGN run's. That is the #1059 bug. The owner
          // anchor now gates the whole `checkpointExists` block: `expectedCheckpointTip`
          // (threaded from claim.checkpoint_tip, the tip THIS run last published to its own
          // per-branch checkpoint ref, persisted server-side on every publish — #1042 M2) must
          // be a non-empty string AND equal the mirrored checkpoint's current SHA for ANY
          // adoption or set-aside to happen.
          const checkpointSha = (
            await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointRef}^{commit}`])
          ).trim();
          const ownerMatch =
            expectedCheckpointTip !== undefined &&
            expectedCheckpointTip !== null &&
            expectedCheckpointTip !== "" &&
            expectedCheckpointTip === checkpointSha;
          if (ownerMatch) {
            if (resume && !originExists) {
              // PATH A — PRD #1030 M3 resume-adopt, unchanged. RESUME with an UNPUSHED branch:
              // `main` advancing during a rate-limit park diverges an otherwise-valid mirrored
              // checkpoint from the moved default, and Path B's strict-descendant test would
              // wrongly set it aside and cold-start the run, losing the committed milestones
              // (the #1009 incident). On a resume with no competing published `origin/<branch>`,
              // adopt using the DISJOINT-HISTORY guard ONLY — `checkpointExists` already encodes
              // `sharesHistory(checkpointRef, default)` (a disjoint checkpoint was treated as
              // absent up front), so reaching here means it shares history. No ancestry test
              // against the current default, mirroring the ownedHere first-park leg. The
              // adopt-time wip(park) marker unwrap (adoptedMarker / willRecoverMarker below)
              // still runs on this adopted base because seededFrom is "checkpoint".
              baseRef = checkpointRef;
              seededFrom = "checkpoint";
            } else {
              // PATH B — strict-descendant, NOW owner-gated (#1059 M1 extends #1042 M4's owner
              // anchor to this leg). isAncestor (merge-base --is-ancestor) is TRUE at EQUALITY,
              // so an ancestor test alone would seed a checkpoint that EQUALS the floor as
              // "checkpoint" though nothing was recovered. Require a STRICT descendant:
              // reachable from the floor AND a different commit.
              if (await this.isAncestor(barePath, floorRef, checkpointRef)) {
                const floorSha = (
                  await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${floorRef}^{commit}`])
                ).trim();
                if (checkpointSha !== "" && checkpointSha !== floorSha) {
                  baseRef = checkpointRef;
                  seededFrom = "checkpoint";
                }
                // else EQUAL to the floor: fall through to the floor. Equality is NOT
                // divergence, so checkpointSetAside stays false — nothing was set aside.
              } else {
                // Diverged and OWNED (ownerMatch): the checkpoint is not reachable from the
                // floor but it IS this run's own work, so origin/default WINS loudly and the
                // set-aside drives the #759 wip(park) cherry-pick recovery (leg #4 below).
                checkpointSetAside = true;
              }
            }
          } else {
            // !ownerMatch — a FOREIGN or FRESH (NULL/empty tip) checkpoint. issue #1059: NEVER
            // adopt and NEVER set checkpointSetAside. A checkpoint ref is a per-BRANCH ref, so a
            // mirrored checkpoint that shares history with the default (or even strictly
            // descends it) might be a PRIOR (possibly plan-rejected) or foreign run's work; and
            // checkpointSetAside drives the #759 cherry-pick, which would re-import the very
            // prior/foreign work this guard keeps out. Seed off the origin/default floor set
            // just above and log LOUDLY (structured warn, not a run-feed status) so an operator
            // can see why a present checkpoint was set aside. This folds the two prior warns
            // (the #1042 resume-mismatch warn and the implicit fresh-adopt path) into one.
            this.log.warn(
              "runner clone: checkpoint set aside LOUDLY — mirrored checkpoint tip does not match this run's own persisted checkpoint (owner-anchor guard, issue #1059 M1 extends #1042 M4 to the strict-descendant leg); seeding off the origin/default floor, NOT a prior/foreign run's checkpoint",
              {
                branch,
                checkpointRef,
                checkpoint_sha: checkpointSha,
                expected_checkpoint_tip: expectedCheckpointTip ?? null,
                seeded_from: seededFrom,
              },
            );
          }
        }
      }
      let baseSha = (await this.runGit(barePath, ["rev-parse", "--verify", `${baseRef}^{commit}`])).trim();
      // PRD #1062 M2 (#1036) — peel a `.github/workflows` overlay transport wrapper. When the
      // adopted base is a `ckpt(overlay):` commit (built by checkpointPack for a branch behind
      // `main` on workflows, pushed UNCHANGED by the broker), its swapped `.github/workflows`
      // tree is NOT branch content — it exists only to satisfy GitHub's tip-vs-default
      // workflow-scope check on the push. DISCARD it by re-pointing `baseSha` to the overlay's
      // LAST parent (= realTip, by construction: checkpointPack builds base-FIRST/realTip-LAST).
      // This runs BEFORE the wip-park adoptedMarker logic below, so a `wip(park)` marker stacked
      // under an overlay is still soft-reset on the peeled tip. Order: overlay = discard-by-
      // reparent (outer) → wip-park = keep-by-soft-reset (inner). Best-effort: if the peel can't
      // resolve the parent, leave baseSha as-is and warn (never crash a reseed over the wrapper).
      //
      // PROVENANCE GATE (issue #1036 review): peel ONLY when seededFrom === "checkpoint" — the
      // worker-built, api-brokered ref the agent CANNOT push to. An overlay is only ever
      // synthesised by checkpointPack for that ref; a `ckpt(overlay):` subject on any other base
      // (a tracking/origin/default tip the agent controls) is a FORGERY, and peeling it would
      // DISCARD the agent's real commit (this peel throws the tip's tree away — unlike the
      // wip-park soft-reset, which keeps it). A subject prefix is not provenance across the agent
      // trust boundary; seededFrom is.
      if (seededFrom === "checkpoint" && (await this.isOverlayMarker(barePath, baseSha))) {
        const overlaySha = baseSha;
        // The real tip is the LAST parent. Count parents (`rev-list --parents -n 1` prints
        // "<commit> <p1> [<p2> …]"), then read `<sha>^<n>` where n = parent count.
        const parentLine = (
          await this.tryGitStdout(barePath, ["rev-list", "--parents", "-n", "1", `${overlaySha}^{commit}`])
        ).trim();
        const nParents = parentLine === "" ? 0 : parentLine.split(/\s+/).length - 1;
        const realTip =
          nParents > 0
            ? (
                await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${overlaySha}^${nParents}`])
              ).trim()
            : "";
        if (/^[0-9a-f]{40}$/.test(realTip)) {
          baseSha = realTip;
          this.log.info("runner clone: peeled .github/workflows overlay wrapper to its realTip", {
            branch,
            overlay_sha: overlaySha,
            real_tip: realTip,
            seeded_from: seededFrom,
          });
        } else {
          this.log.warn("runner clone: overlay marker present but its realTip parent did not resolve — not peeling", {
            branch,
            overlay_sha: overlaySha,
            parents: nParents,
            seeded_from: seededFrom,
          });
        }
      }
      // PRD #759 M2 — same-worker + cross-worker-clean recovery. On the tracking leg
      // (same-worker) and the checkpoint leg (cross-worker strict-descendant), `baseSha`
      // itself is the adopted tip, and that tip may be a `wip(park):` marker M1 planted:
      // the throwaway commit auto-saving the pre-park uncommitted tree. When it is, the
      // REAL branch base is the marker's PARENT (the last real commit), so the counts and
      // the ratchet clamp below must sit on the parent, not on the marker (M5 requires the
      // recovered-commit count exclude the marker — it is not committed work). We detect
      // the marker HERE, before those counts are computed, and `reset --soft` the checkout
      // back to the parent after `checkout -b` below, so the marker's tree lands as
      // uncommitted changes and the marker never enters the history the agent builds on.
      const adoptedMarker = (seededFrom === "tracking" || seededFrom === "checkpoint")
        && await this.isWipParkMarker(barePath, baseSha);
      const markerParent = adoptedMarker
        ? (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${baseSha}^^{commit}`])).trim()
        : "";
      // A root-commit marker (no parent) or a failed parent read leaves markerParent empty:
      // there is nothing to reset onto, so treat it as NOT-recovered — keep the marker as
      // the base (byte-identical to today's behaviour), do not reset, and log. Vanishingly
      // rare (the clone base is never empty), but handled rather than crashed.
      if (adoptedMarker && markerParent === "") {
        this.log.warn("runner clone: adopted wip(park) marker has no parent — not recovering (root-commit marker)", {
          branch,
          baseSha,
          seeded_from: seededFrom,
        });
      }
      const willRecoverMarker = adoptedMarker && markerParent !== "";
      // The effective base the branch will actually sit on after the `reset --soft` below.
      // When there is no marker (every non-park leg), effectiveBase === baseSha and every
      // downstream computation is byte-identical to today.
      const effectiveBase = willRecoverMarker ? markerParent : baseSha;
      // How many commits the seed carries ahead of the default branch. On the origin
      // leg that is prior PUSHED work (issue #105); on the tracking leg it is the
      // interrupted attempt's RECOVERED work (PRD #218 M3). Counted in the BARE, which
      // holds every ref. Best-effort by construction — a repo with no resolvable default
      // branch, or any rev-list failure, yields 0 rather than failing a run over colour.
      // Counted from effectiveBase so a recovered wip(park) marker (which reset --soft
      // strips out of history) is never counted (PRD #759 M2/M5).
      const isResumeLeg = seededFrom !== "default";
      const priorCommits = isResumeLeg ? await this.commitsAheadOfDefault(barePath, effectiveBase) : 0;
      // On the default leg the seed already IS the default tip, so no second lookup. On a
      // resume leg they differ, and the difference is exactly what the lead cannot infer.
      const defaultBranchCommit = isResumeLeg ? await this.defaultBranchSha(barePath) : effectiveBase;
      this.log.info("runner clone: seeding", { branch, base: baseRef, seeded_from: seededFrom, prior_commits: priorCommits, path: clonePath });

      // The seed clone + checkout run as the RUNNER uid (PRD #51 M4), so the clone +
      // working tree are runner-owned (the agent commits there; the worker never writes
      // it). `--shared` references the bare's objects read-only (alternate); `--no-checkout`
      // skips populating the stale default so we check the agent branch out at the
      // resolved base SHA (reachable via the alternate) in one step. No PAT (local op).
      await this.runGitAsRunner(undefined, ["clone", "--shared", "--no-checkout", barePath, clonePath]);
      // Issue #134 (production half of #127). ONE detached `git maintenance run --auto
      // --detach` per object-writing command (fetch/commit/push) outlives the git we awaited
      // and keeps writing inside `.git`; it spawns a repack/pack-objects subtree only once
      // `gc.auto`'s threshold is met, which a per-run `--shared` clone never reaches.
      // removeRunnerClone (runner.ts:454) `fs.rm`s this tree moments after the agent's last
      // commit and our push, and `force: true` suppresses ENOENT, not ENOTEMPTY.
      //
      // As RUNNER, matching the clone: `<clone>/.git/config` is runner-owned, so this
      // rewrites it in place as the same uid. Doing it as WORKER would plant a worker-owned
      // config inside a directory the untrusted runner owns and can replace anyway — no gain,
      // and it breaks the ownership invariant the plant-a-key analysis above rests on. Note
      // the image puts `worker` in the `runner` group, so a worker-uid write here would
      // likely SUCCEED QUIETLY rather than fail loudly.
      await this.disableAutoMaintenance(clonePath, /* asRunner */ true);
      // Issue #234 — plant the SDK agent's commit identity so its very FIRST `git commit`
      // does not fail exit-128 (`unable to auto-detect email address`) on the passwd-less
      // runner uid, which has no /etc/passwd GECOS for git to fall back to. Written
      // repo-local AS THE RUNNER uid, for the same ownership reason disableAutoMaintenance
      // is (the clone is runner-owned; a worker-uid write would break the ownership
      // invariant the plant-a-key analysis above rests on) — and, being repo-local, it
      // additionally covers the AGENT's own git (the SDK Bash tool), which does NOT go
      // through gitEnv. `commit.gpgsign=false` mirrors the stub executor so a signing
      // config reachable from the agent's HOME `.gitconfig` cannot block the commit.
      // Kept as explicit inline calls (NOT folded into disableAutoMaintenance, whose name
      // would hide the identity write).
      await this.runGitAsRunner(clonePath, ["config", "user.name", AGENT_GIT_IDENTITY.name]);
      await this.runGitAsRunner(clonePath, ["config", "user.email", AGENT_GIT_IDENTITY.email]);
      await this.runGitAsRunner(clonePath, ["config", "commit.gpgsign", "false"]);
      await this.runGitAsRunner(clonePath, ["checkout", "-b", branch, baseSha]);
      // PRD #759 M2 — restore a recovered WIP tree to UNCOMMITTED at adopt time. Exactly
      // ONE of the two branches below can run: #3 fires on the tracking/checkpoint legs
      // where `baseSha` IS the marker (adoptedMarker); #4 fires on the not-ownedHere floor
      // leg where the checkpoint DIVERGED and was set aside (checkpointSetAside). They are
      // mutually exclusive by construction — adoptedMarker requires seededFrom
      // tracking|checkpoint, while checkpointSetAside is set only inside the owner-matched
      // Path-B diverged arm (issue #1059 M1), which reassigns neither baseRef nor seededFrom
      // and so leaves seededFrom ∈ {origin, default} — so the structure below can pick at most
      // one.
      let wipRecovered = false;
      if (willRecoverMarker) {
        // #3 — SAME-WORKER + CROSS-WORKER-CLEAN. The checkout materialized the marker's
        // tree at HEAD; `reset --soft` moves HEAD back to the marker's parent while leaving
        // the index + working tree at the marker's tree, so the WIP content is present as
        // staged/uncommitted changes and the marker commit is no longer in history — it
        // never enters what the agent builds on, never reaches finalize, never lands in the
        // MR (PRD #759 D3, without the finalize-time rewrite that would collide with ADR
        // #456). runGitAsRunner: a working-tree write in the runner-owned clone.
        await this.runGitAsRunner(clonePath, ["reset", "--soft", markerParent]);
        wipRecovered = true;
        this.log.info("runner clone: recovered wip(park) marker to uncommitted (reset --soft)", {
          branch,
          marker: baseSha,
          parent: markerParent,
          seeded_from: seededFrom,
        });
      } else if (checkpointSetAside && checkpointExists && await this.isWipParkMarker(barePath, checkpointRef)) {
        // #4 — CROSS-WORKER DIVERGED. `main` advanced during the park, so the mirrored
        // checkpoint is not a strict descendant of the floor and the strict-descendant guard
        // set it aside (baseSha = floor). When the checkpoint tip is a `wip(park):` marker we
        // MAY recover just the WIP tree onto the new floor — but only when doing so drops NO
        // committed work. Two things the naive `cherry-pick --no-commit <ref>` got wrong,
        // both fixed here (PRD #759 M2):
        //
        //  (1) DOA — the ref never resolves in the clone. The runner clone is created above
        //      with `git clone --shared --no-checkout`, which copies NONE of the bare's
        //      custom refs, so `refs/uzi-checkpoints/*` is absent from the clone and a
        //      cherry-pick BY REF NAME always `fatal: bad revision`d — this leg could never
        //      recover. The OBJECTS are reachable via the `--shared` alternate; only the ref
        //      NAME is missing. So resolve the marker to a 40-char SHA against the BARE
        //      (mirroring the strict-descendant guard's rev-parse ~:483) and cherry-pick the
        //      SHA, which the clone can name through its alternate.
        //  (2) Silent milestone drop. `cherry-pick --no-commit <marker>` applies ONLY the
        //      marker's diff against ITS OWN parent (the WIP delta). If the diverged
        //      checkpoint carries committed-but-unpushed milestones between the fork point
        //      and the marker (`fork → m1 → wip-marker`, the exact #628 shape), picking only
        //      the tip DROPS m1's content — silently, when m1 touches files disjoint from the
        //      WIP so the pick still applies clean — AND flips checkpointSetAside=false,
        //      suppressing the loud set-aside notice. So gate the recovery on there being NO
        //      committed work below the marker: the marker's PARENT must be an ancestor of the
        //      floor (isAncestor is true at equality — the #685 zero-commit shape where the
        //      marker sits directly on floor_at_park, itself an ancestor of the advanced
        //      floor). If the parent is NOT an ancestor of the floor, committed divergence
        //      lives below the marker: leave it set aside for a human, do NOT cherry-pick.
        const checkpointMarkerSha = (
          await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointRef}^{commit}`])
        ).trim();
        const checkpointMarkerParentSha = checkpointMarkerSha === ""
          ? ""
          : (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointMarkerSha}^^{commit}`])).trim();
        if (checkpointMarkerParentSha === "") {
          // A root-commit marker (no parent), or a failed tip/parent read: no fork point to
          // test against the floor, so we cannot prove that no committed work would be
          // dropped. Keep it set aside (the loud notice is preserved), do not cherry-pick.
          this.log.warn("runner clone: diverged wip(park) checkpoint has no readable parent — left set aside (no recovery)", {
            branch,
            checkpoint: checkpointRef,
            floor: baseSha,
          });
        } else if (await this.isAncestor(barePath, checkpointMarkerParentSha, baseSha)) {
          // The marker sits directly on a floor-ancestor: no committed milestones live below
          // it, so recovering just the WIP delta drops nothing. Cherry-pick the resolved SHA
          // (NOT the ref name — see (1)) `--no-commit` so the WIP tree lands staged/uncommitted
          // on the floor.
          try {
            await this.runGitAsRunner(clonePath, ["cherry-pick", "--no-commit", checkpointMarkerSha]);
            // SUCCESS — the WIP is recovered as uncommitted on the floor. It rides on top of
            // the floor base (priorCommits stays 0 for this leg: no committed work recovered),
            // so seededFrom stays the floor leg. It was recovered, not set aside.
            wipRecovered = true;
            checkpointSetAside = false;
            this.log.info("runner clone: recovered diverged wip(park) checkpoint onto new floor (cherry-pick --no-commit)", {
              branch,
              checkpoint: checkpointRef,
              marker: checkpointMarkerSha,
              floor: baseSha,
              seeded_from: seededFrom,
            });
          } catch (err) {
            // FAILURE (merge conflict) — a WIP that touches files main also moved conflicts
            // even with a floor-ancestor parent; that is the genuine unclean case. Guarantee a
            // pristine floor tree (abort the half-applied pick, hard-reset to the floor, clean
            // untracked) and report failure: checkpointSetAside stays true, wipRecovered false,
            // seededFrom unchanged. Required SAFE FAILURE (SC#1(b)): reports failure rather than
            // silently dropping OR force-applying work.
            await this.runGitAsRunner(clonePath, ["cherry-pick", "--abort"]).catch(() => undefined);
            await this.runGitAsRunner(clonePath, ["reset", "--hard", baseSha]).catch(() => undefined);
            await this.runGitAsRunner(clonePath, ["clean", "-fd"]).catch(() => undefined);
            this.log.warn("runner clone: diverged wip(park) checkpoint did NOT apply cleanly onto new floor — recovery failed (set aside)", {
              branch,
              checkpoint: checkpointRef,
              floor: baseSha,
              error: gitErrorMessage(err),
            });
          }
        } else {
          // The marker's parent is NOT an ancestor of the floor: committed-but-unpushed work
          // (m1…mN) lives between the fork point and the marker. Cherry-picking only the tip
          // would silently drop it AND flip off the set-aside notice, reporting a partial
          // recovery as full success. Leave it set aside (checkpointSetAside stays true,
          // wipRecovered false, seededFrom unchanged) so the loud notice tells the human that
          // committed work was left behind.
          this.log.warn("runner clone: diverged wip(park) checkpoint carries committed divergence below the marker — left set aside for a human (no cherry-pick)", {
            branch,
            checkpoint: checkpointRef,
            marker: checkpointMarkerSha,
            markerParent: checkpointMarkerParentSha,
            floor: baseSha,
          });
        }
      }
      // Issue #262 — refresh the clone's default remote-tracking ref to the FRESH default
      // head. The clone's `refs/remotes/origin/<default>` is copied from the bare's
      // `refs/heads/*` (cloneBare rewrites the fetch refspec to
      // `+refs/heads/*:refs/remotes/origin/*`, leaving the bare's own `refs/heads/*` a
      // FROZEN mirror fixed at first clone), so it inherits a stale head even though the
      // branch is checked out at the fresh default tip. golangci-lint's ratchet
      // `issues: {new-from-merge-base: origin/main, whole-files: true}` then computes
      // `merge-base(origin/main[frozen], HEAD[fresh])` = an ancient commit and false-reds
      // the entire pre-existing backlog as branch-introduced. Point the ratchet base at the
      // fresh default head (already resolved in `defaultBranchCommit`, reachable via the
      // `--shared` alternate) so it gates only branch-introduced findings. Local + offline;
      // as RUNNER, matching the surrounding runner-owned writes. `defaultBranchCommit` may be
      // undefined (a repo with no resolvable default branch); guard and skip so we never mask
      // the existing merge-base pre-flight in the Taskfile.
      // Issue #313 — never let the ratchet base be a STRICT ANCESTOR of the branch base. On a
      // resume leg `defaultBranchCommit` can resolve (via defaultBranchSha → defaultBranchRef's
      // fallback chain) to the FROZEN refs/heads/main mirror, a stale ancestor of baseSha; #262
      // then advances origin/main to that stale commit, so merge-base(origin/main, HEAD) regresses
      // below the fork point and false-reds other people's backlog. Clamp to baseSha — the exact
      // base the lead computes by hand (--new-from-merge-base=<baseSha> returns 0 issues).
      // isAncestor is TRUE at equality, so a fresh run (defaultBranchCommit === baseSha) and an
      // ordinary resume both clamp to baseSha with NO behaviour change from #262 on the fresh run
      // (the same value is written); only the frozen-mirror case is corrected. When
      // defaultBranchCommit is NOT an ancestor of baseSha (a resume where main moved forward on a
      // divergent line) keep defaultBranchCommit so vs-main merge-base semantics are preserved.
      // Read ancestry against the worker-owned BARE with the existing worker-uid isAncestor helper
      // (both commits are bare-reachable); the WRITE stays runGitAsRunner into the runner-owned
      // clone, exactly as #262. The no-resolvable-default-branch edge keeps today's skip (origin/main
      // keeps whatever the plain clone copied); it is governed by the Taskfile merge-base pre-flight,
      // out of scope here.
      // Issue #363 — the clamp below is made DURABLE by removing the clone's `remote.origin.fetch`
      // refspec right after it, so a later agent-initiated `git fetch origin main` / `git fetch
      // origin` / `git pull` updates only FETCH_HEAD and cannot move `refs/remotes/origin/<default>`
      // back to the frozen bare mirror head, which would undo the clamp and re-corrupt the ratchet base.
      // PRD #759 M2: clamp against effectiveBase — the real fork point after a wip(park)
      // reset --soft (== baseSha on every non-marker leg, so byte-identical there).
      const defaultBranch = await this.defaultBranchName(barePath);
      let ratchetBase = defaultBranchCommit;
      if (ratchetBase && (await this.isAncestor(barePath, ratchetBase, effectiveBase))) {
        ratchetBase = effectiveBase;
      }
      if (defaultBranch && ratchetBase) {
        await this.runGitAsRunner(clonePath, ["update-ref", `refs/remotes/origin/${defaultBranch}`, ratchetBase]);
        if (ratchetBase !== defaultBranchCommit) {
          this.log.info("runner clone: clamped ratchet base to branch base (stale default ref)", {
            branch,
            baseSha,
            defaultBranchCommit,
            clamped_to: ratchetBase,
          });
        }
      }
      // Issue #363 — make the clamp above durable. `git clone` writes a
      // `remote.origin.fetch` refspec (`+refs/heads/*:refs/remotes/origin/*`); a later
      // agent-initiated `git fetch origin main` / `git fetch origin` / `git pull` would
      // re-apply it and force `refs/remotes/origin/<default>` backward to the frozen bare
      // mirror head, undoing the clamp and re-corrupting the ratchet base. With no
      // configured refspec, a fetch updates only FETCH_HEAD and touches no tracking ref, so
      // the clamp holds for the run's lifetime. `git config --unset-all` exits 5 when the key
      // is absent (a plain clone always has it, so exit 5 is only a defensive edge); treat
      // ONLY exit 5 as non-fatal and rethrow anything else.
      await this.runGitAsRunner(clonePath, ["config", "--unset-all", "remote.origin.fetch"]).catch((err: unknown) => {
        if ((err as { code?: unknown }).code === 5) return;
        throw err;
      });
      // PRD #759 M2: baseCommit is the REAL fork point — effectiveBase, which is the
      // marker's parent when a wip(park) marker was reset --soft'd back to uncommitted, and
      // baseSha (byte-identical) on every other leg. wipRecovered surfaces the recovery to
      // M4/M5.
      return { path: clonePath, branch, priorCommits, baseCommit: effectiveBase, defaultBranchCommit, seededFrom, checkpointSetAside, wipRecovered };
    });
  }

  /**
   * Turn git's detached auto-maintenance off in a repo we own (issue #134).
   *
   * Idempotent, so it is safe on the warm path — which is where it matters: `cloneBare`
   * runs only on the very first clone, and `/data` is persistent (a per-worker PVC in k8s,
   * the `agentdata` volume under compose), so a bare on an already-deployed worker is only
   * ever reached through `ensureClone`'s fetch branch.
   *
   * Both keys, deliberately — neither subsumes the other across the version range. See the
   * note on `gitEnv`'s inline pins, which close the same hole for every git that goes
   * through this module; these repo-local writes additionally cover the AGENT's own git
   * (the SDK Bash tool), which does not use `gitEnv`.
   *
   * `tryGit` rather than `runGit`: this prevents a warning-level directory leak, so it must
   * never be the reason a run fails to seed.
   */
  private async disableAutoMaintenance(repoPath: string, asRunner = false): Promise<void> {
    // No tryGitAsRunner exists; swallow explicitly rather than adding a near-duplicate of
    // tryGit whose only caller would be this one.
    const run = asRunner
      ? (args: string[]) => this.runGitAsRunner(repoPath, args).catch(() => undefined)
      : (args: string[]) => this.tryGit(repoPath, args).then(() => undefined);
    await run(["config", "maintenance.auto", "false"]);
    await run(["config", "gc.auto", "0"]);
    // `core.fsmonitor=false` closes a SECOND detached daemon that the two keys above do
    // not touch. `git fsmonitor--daemon run --detach` is spawned by any git command in a
    // repo where core.fsmonitor is true, reparents to init, and then WATCHES THE
    // DIRECTORY for as long as it lives — so unlike the maintenance child, which holds
    // its lock for milliseconds, this one holds handles indefinitely. Measured
    // 2026-08-03 on this host: a fixture created 2026-07-13 still had its daemon alive
    // 21 DAYS later, holding `data/worktrees/.../issue-55`, which is why `fs.rmSync`
    // left a file-free directory skeleton behind rather than removing the tree.
    //
    // It reaches these repos through `gitEnv()`'s deliberate GIT_CONFIG_GLOBAL
    // passthrough: with the var unset (an ordinary dev shell) git falls back to
    // ~/.gitconfig, and `core.fsmonitor = true` there is a common and reasonable
    // setting. The fixture ORIGIN is already shielded because makeFixture pins
    // GIT_CONFIG_GLOBAL=/dev/null; the repos THIS module creates are not, which is
    // exactly the residue issue #127's retry was left to absorb.
    //
    // Control, same host, same git: a repo carrying only the two keys above spawned 1
    // daemon that held the directory; adding this key spawned 0. Safe by construction —
    // fsmonitor is a `git status` optimisation for large trees, never a correctness
    // input, and these clones are short-lived and small. In a worker container the
    // daemon would die with the container anyway, so this is a no-op there and a leak
    // fix locally.
    await run(["config", "core.fsmonitor", "false"]);
  }

  /** Commits reachable from `sha` but not from the repo's default branch. Best-effort:
   *  any failure (no resolvable default, an unexpected rev-list error) answers 0, so a
   *  caller can treat a non-zero count as "there is prior work here" and nothing else. */
  /** The default branch's tip as a full OID, or undefined when it cannot be resolved.
   *  Best-effort by construction (same posture as commitsAheadOfDefault): this feeds a
   *  prompt note, and a run must never fail because a repo has no default branch.
   *
   *  Inherits defaultBranchRef's fallback chain, so on its mirror-layout rungs this returns
   *  the FROZEN ref rather than a fresh tip — see RunnerClone.baseCommit for the window. */
  private async defaultBranchSha(barePath: string): Promise<string | undefined> {
    const ref = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!ref) return undefined;
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${ref}^{commit}`])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : undefined;
  }

  private async commitsAheadOfDefault(barePath: string, sha: string): Promise<number> {
    const defaultRef = await this.defaultBranchRef(barePath).catch(() => undefined);
    if (!defaultRef) return 0;
    const n = Number.parseInt(await this.tryGitStdout(barePath, ["rev-list", "--count", `${defaultRef}..${sha}`]), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  /**
   * Worker fetches the agent branch BACK from the runner clone into the worker bare
   * (PRD #51 M3, B2). The worker is bare-only and the runner clone is runner-owned, so
   * this is the ONE point the worker reads a runner-controlled store — hardened by the
   * six B2 invariants:
   *   - single-branch refspec (`+refs/heads/<branch>:refs/uzi-runner/<branch>`), never
   *     `refs/heads/*` — the runner's whole ref namespace is never admitted (inv. 2);
   *   - `file://` transport forces the PACK protocol (upload-pack over a pipe), so the
   *     local-copy optimization that would traverse a runner-planted
   *     objects/info/alternates is NOT used — this is the specific job of the file://
   *     transport (CVE-2022-39253 class, inv. 3);
   *   - `protocol.file.allow=user` pinned deliberately for that `file://` fetch (the
   *     minimal-privilege value: this is a top-level, user-initiated fetch, which `user`
   *     — git's compiled default since 2.38.1 — allows, while NOT enabling file:// in
   *     the submodule/non-user contexts `always` would);
   *   - the worker's FETCH side (fetch-pack, ref update, object write) runs on gitEnv
   *     (GIT_CONFIG_NOSYSTEM + GIT_CONFIG_GLOBAL=/dev/null + the M0 code-exec-key pins),
   *     so the worker's OWN config governs the process that writes into its bare (inv. 4).
   *     git DOES spawn upload-pack in the runner clone, which reads the runner clone's
   *     repo-local config — but that is safe by construction: `uploadpack.packObjectsHook`
   *     is respected ONLY from PROTECTED config (documented, transport-independent,
   *     stable), so a runner repo-local plant is ignored; and upload-pack performs no
   *     checkout/diff, so the command-valued core.* keys never fire there (and the
   *     worker's inline GIT_CONFIG_* pins are inherited by it regardless);
   *   - `--no-tags`: only the branch, no runner-controlled tag namespace.
   * The agent's new objects transfer into the WORKER bare's own object store, so the
   * subsequent push does not depend on the (torn-down, possibly compromised) runner
   * clone (objects-integrity win). Returns the worker-side tracking ref pushBranch/
   * changedFiles then read.
   */
  async fetchAgentBranch(barePath: string, clonePath: string, branch: string, runId: string): Promise<string> {
    const dst = runnerTrackingRef(branch);
    await this.withLock(barePath, async () => {
      // issue #887: clear any legacy FLAT tracking ref that is a strict path-prefix
      // (directory ancestor) of `dst`, or the fetch below aborts. See the helper's own
      // doc for the full mechanism. Runs FIRST, under the same lock as the fetch/stamp,
      // so the namespace is clear before the ref-store tries to create the dst directory.
      await this.clearConflictingAncestorTrackingRefs(barePath, dst);
      await this.runGit(barePath, [
        "-c", "protocol.file.allow=user",
        "fetch", "--no-tags", `file://${clonePath}`,
        `+refs/heads/${branch}:${dst}`,
      ]);
      // PRD #218: stamp the run that owns this ref, under the SAME lock as the ref write
      // so the two are always consistent. The reseed (runnerCloneForBranch) reads it back
      // and takes the tracking ref only when this run wrote it — the anchor that stops a
      // different run on the same issue from inheriting orphaned work.
      await this.runGit(barePath, ["config", "--local", runnerTrackingOwnerKey(branch), runId]);
    });
    return dst;
  }

  /**
   * issue #887 — clear a legacy FLAT tracking ref that path-blocks the fetch's dst ref.
   *
   * git's ref store is a directory tree: a ref FILE at `refs/uzi-runner/uzi/self-improve`
   * and a ref DIRECTORY at `refs/uzi-runner/uzi/self-improve/<runId>` cannot coexist,
   * because the leaf file occupies the very path the directory needs (a D/F — directory/
   * file — conflict). Before PRD #774 / ADR 0686 D9, a self_improve run's tracking ref was
   * the flat leaf `refs/uzi-runner/uzi/self-improve`; #774 moved it to the per-run
   * namespace `refs/uzi-runner/uzi/self-improve/<runId>`. On a worker whose persistent bare
   * still carries the pre-#774 flat leaf, that leaf is a strict path-prefix (ancestor) of
   * the new dst, so `fetch … :refs/uzi-runner/uzi/self-improve/<runId>` fails the whole
   * update with "some local refs could not be updated" and the run dies. self_improve is
   * the ONLY run kind whose ref shape changed leaf→namespace, so it is the only observed
   * failure — we clear exactly the ancestor refs that can block dst.
   *
   * The mirror case — a legacy per-run DIRECTORY blocking a new FLAT leaf (a descendant path
   * blocking its own ancestor) — is deliberately OUT OF SCOPE: no run kind moved
   * namespace→leaf, so it does not arise here, and handling it would mean deleting a whole
   * live subtree on a guess.
   *
   * Any conflicting ancestor found is ARCHIVED (its tip may carry unmerged commits) under
   * refs/uzi-archive/<sanitized>/<sha> before it is deleted. Its dangling PRD #218 owner stamp
   * is cleared under the #887 subsection key, and — when no colliding live sibling shares it —
   * under the pre-#887 flattened key too (issue #909). Deepest-first so a partially-migrated
   * bare with several stacked ancestors is cleaned bottom-up.
   */
  private async clearConflictingAncestorTrackingRefs(barePath: string, dst: string): Promise<void> {
    // Only refs inside the tracking namespace can D/F-conflict with a tracking-ref dst.
    if (!dst.startsWith(RUNNER_TRACKING_PREFIX)) return;
    const suffix = dst.slice(RUNNER_TRACKING_PREFIX.length);
    const parts = suffix.split("/");
    // Cumulative prefixes STRICTLY shorter than the full branch: every part except the last.
    // For suffix "uzi/self-improve/<runId>" this yields the branches "uzi" and
    // "uzi/self-improve", i.e. the candidate refs refs/uzi-runner/uzi and
    // refs/uzi-runner/uzi/self-improve — never the namespace root and never dst itself.
    const candidates: string[] = [];
    let acc = "refs/uzi-runner";
    for (const part of parts.slice(0, -1)) {
      acc = `${acc}/${part}`;
      candidates.push(acc);
    }
    // Deepest-first: delete the most specific blocking leaf before its shorter ancestors.
    for (const candidate of candidates.reverse()) {
      if (!(await this.refExists(barePath, candidate))) continue;
      const sha = (await this.runGit(barePath, ["rev-parse", candidate])).trim();
      // Archive first so a possibly-unmerged tip is never lost by the delete. The
      // <sanitized>/<sha> shape is D/F-safe within refs/uzi-archive (the sha leaf never
      // collides with a sibling branch's subtree) and idempotent (re-archiving the same
      // tip writes the same ref to the same sha).
      const ancestorBranch = candidate.slice(RUNNER_TRACKING_PREFIX.length);
      const sanitized = ancestorBranch.replace(/[^A-Za-z0-9_-]/g, "-");
      await this.runGit(barePath, ["update-ref", `refs/uzi-archive/${sanitized}/${sha}`, sha]);
      await this.runGit(barePath, ["update-ref", "-d", candidate]);
      // Clear the now-dangling PRD #218 owner stamp for the deleted ref. tryGit swallows
      // exit 5 (key absent), which runGit would instead throw on — see the helper notes.
      await this.tryGit(barePath, ["config", "--local", "--unset", runnerTrackingOwnerKey(ancestorBranch)]);
      // issue #909 — a pre-#887 bare may hold the stamp under the FLATTENED key instead. Clear it
      // too, but only when unattributable-to-a-sibling: the flat key is lossy, so unsetting it
      // while a DISTINCT live tracking ref flattens to the same token would wipe THAT branch's
      // stamp (the #887 collision). The ancestor ref was deleted just above, so it is already out
      // of the live set and cannot flag itself; a colliding live sibling still would.
      if (!(await this.flatOwnerKeyAmbiguous(barePath, ancestorBranch))) {
        await this.tryGit(barePath, ["config", "--local", "--unset", legacyFlatTrackingOwnerKey(ancestorBranch)]);
      }
    }
  }

  /** PRD #122 M6: the tip of the worker-side tracking ref `fetchAgentBranch` wrote
   *  (refs/uzi-runner/<branch>), or null when it does not exist yet. Used by the
   *  checkpoint no-op check to tell "the branch advanced since the last checkpoint"
   *  from "nothing new to fetch". Best-effort (tryGitStdout): a broken/absent ref
   *  answers null rather than throwing. */
  async trackingTip(barePath: string, branch: string): Promise<string | null> {
    const sha = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${runnerTrackingRef(branch)}^{commit}`])).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /** PRD #122 M6: the tip of the runner clone's own `refs/heads/<branch>` (the agent's
   *  committed work), or null when unresolvable. Read as the RUNNER uid — the clone is
   *  runner-owned, so a worker-uid read would hit the B2 dubious-ownership boundary
   *  (git.ts B2 invariants). Best-effort. */
  async branchTip(clonePath: string, branch: string): Promise<string | null> {
    const sha = (await this.runGitAsRunner(clonePath, ["rev-parse", "--verify", `refs/heads/${branch}^{commit}`]).catch(() => "")).trim();
    return /^[0-9a-f]{40}$/.test(sha) ? sha : null;
  }

  /**
   * PRD #122 M8 — the delta packfile of `<exclude>..refs/uzi-runner/<branch>`, for a
   * brokered origin publish at a checkpoint. Returns `{ tipOid, pack }` where `tipOid`
   * is the tracking-ref tip (the same the checkpoint fetched back) and `pack` STREAMS the
   * packfile bytes; null when there is no tracking ref yet (nothing to publish).
   *
   * The exclude boundary mirrors the reseed's floor: `refs/remotes/origin/<branch>` when
   * origin carries the branch, else the default branch — so the pack carries only what the
   * checkpoint added beyond what origin already has.
   *
   * STREAMS, never buffers: a pack can exceed the 64 MiB `maxBuffer` cap runGit uses, so
   * this spawns `git pack-objects --revs --stdout` and hands back its stdout as a Readable
   * for the client to upload directly. Credential-free: this is a LOCAL read of the
   * WORKER-OWNED bare by the worker uid (base gitEnv, no PAT, no runner-uid switch — the
   * runner does not own the bare). A shared lock is not taken: pack-objects reads a
   * consistent object snapshot, and this is a read, not a mutation.
   */
  async checkpointPack(
    barePath: string,
    branch: string,
    overlay?: CheckpointOverlayContext,
  ): Promise<{ tipOid: string; pack: Readable } | null> {
    const realTip = await this.trackingTip(barePath, branch);
    if (!realTip) return null;
    const originRef = `refs/remotes/origin/${branch}`;
    const excludeRef = (await this.refExists(barePath, originRef))
      ? originRef
      : await this.defaultBranchRef(barePath);
    const trackingRef = runnerTrackingRef(branch);

    // PRD #1062 M2 (#1036) — the `.github/workflows` overlay. When an overlay context is
    // supplied (GitHub, agent already reaped — see runner.ts), attempt to build a genuine
    // fast-forward wrapper commit `O_ov` whose `.github/workflows` tree equals the default's,
    // and pack THAT instead of the raw tracking tip, so a branch behind `main` on those files
    // is no longer rejected `workflow_scope` and checkpoints durably. FAIL-SOFT: on ANY error
    // or gate-miss `buildWorkflowOverlay` returns null and we ship `realTip` — today's exact
    // behaviour (→ the clean workflow-scope skip; #377 owns a workflow-MODIFYING branch at
    // finalize). This method never throws for an overlay reason. When `overlay` is undefined
    // the block is skipped and `realTip` ships, byte-for-byte as before.
    let wantRev = realTip;
    if (overlay) {
      const ov = await this.buildWorkflowOverlay(barePath, branch, realTip, overlay);
      if (ov) wantRev = ov;
    }

    // pack-objects reads the wanted/excluded revs on stdin (one ref per line, `^` excludes)
    // and writes the packfile to stdout. The wanted rev is `realTip` on the no-overlay path
    // (via `trackingRef`'s tip) and `O_ov` when an overlay was built; `^excludeRef` is the
    // same floor either way, so the default's workflow blobs (reachable from the floor on the
    // not-pushed leg) are not re-shipped.
    const wanted = wantRev === realTip ? trackingRef : wantRev;
    const { stdout } = this.spawnGit(
      barePath,
      ["pack-objects", "--revs", "--stdout"],
      `${wanted}\n^${excludeRef}\n`,
    );
    return { tipOid: wantRev, pack: stdout };
  }

  /**
   * PRD #1062 M2 (#1036) — build the `.github/workflows` overlay wrapper commit `O_ov` for a
   * checkpoint, or return null to ship the raw `realTip` (today's behaviour). ALL work is
   * FAIL-SOFT: every error resolves to null so `checkpointPack` never throws for an overlay
   * reason, and every gate-miss ships `realTip`.
   *
   * The overlay is a transport wrapper the broker pushes UNCHANGED and adoption peels
   * (`runnerCloneForBranch`, `isOverlayMarker`): its `.github/workflows` subtree is swapped to
   * the default's so GitHub's tip-vs-default workflow-scope check passes, while the real branch
   * content lives on its LAST parent (`realTip`). The base (a prior overlay tip) goes FIRST and
   * `realTip` LAST so the api broker's parent[0]-first strict-descendant DFS over its depth-1
   * store accepts it (see the PRD's broker note — the broker stays byte-unchanged).
   *
   * Synthesis is pure object-DB work via a TEMP INDEX (no live worktree, no filter drivers, no
   * runner-clone perturbation): `read-tree` the real tip, `rm --cached` the workflow set,
   * `read-tree --prefix` the default's subtree back (only when the default HAS one), `write-tree`,
   * then `commit-tree` with a DETERMINISTIC identity + committer date so a no-new-work rebuild
   * yields the same OID. Runs worker-uid on the base git env with NO PAT (the local reads), the
   * PAT is used only by `fetchDefaultTip`.
   */
  private async buildWorkflowOverlay(
    barePath: string,
    branch: string,
    realTip: string,
    overlay: CheckpointOverlayContext,
  ): Promise<string | null> {
    const trackingRef = runnerTrackingRef(branch);
    // GATE 1 — the default tip must resolve (a fresh authenticated fetch). A failure ships
    // realTip: overlay durability is best-effort and never blocks the checkpoint.
    let defaultTip: string;
    try {
      defaultTip = await this.fetchDefaultTip(
        barePath,
        overlay.defaultBranch,
        overlay.pat,
        overlay.cloneUrl,
        overlay.username,
      );
    } catch (e) {
      this.log.warn("checkpoint overlay: could not resolve the default tip — shipping realTip", {
        branch,
        error: gitErrorMessage(e),
      });
      return null;
    }
    // GATE 2 — only a branch actually behind on `.github/workflows` needs an overlay (reuse
    // #627's exact trigger). Not behind ⇒ ship realTip (the broker accepts the raw tip).
    if (!(await this.workflowTreeDiffers(barePath, trackingRef, defaultTip))) return null;
    // GATE 3 — the branch must NOT itself have modified a workflow file. `changedFiles` returns
    // null when the diff can't be computed: FAIL-SAFE (cannot verify ⇒ ship realTip, never
    // synthesise a tree that might hide a branch's own workflow edit). A non-empty workflow hit
    // set means #377 owns this branch at finalize; ship realTip → the clean workflow-scope skip.
    const changed = await this.changedFiles(barePath, trackingRef);
    if (changed === null) return null;
    if (flagCIConfigPaths(changed, [".github/workflows/**"]).length > 0) return null;

    // Temp index + empty throwaway work-tree, both cleaned up in the finally. The work-tree is
    // required ONLY by `read-tree --prefix` (it refuses in a bare repo); WITHOUT `-u` nothing is
    // checked out into it, so no `.gitattributes` filter/smudge driver ever fires — a security
    // property (do NOT add `-u`).
    const tmpIdx = path.join(os.tmpdir(), `uzi-ckpt-idx-${randomUUID()}`);
    const tmpWork = path.join(os.tmpdir(), `uzi-ckpt-work-${randomUUID()}`);
    try {
      await fs.mkdir(tmpWork, { recursive: true });
      const idxEnv: NodeJS.ProcessEnv = { GIT_INDEX_FILE: tmpIdx };
      await this.runGitWithEnv(barePath, ["read-tree", realTip], idxEnv);
      // `--ignore-unmatch` is load-bearing: against a temp index a `git rm --cached` with no
      // match exits non-zero, so this flag makes the no-`.github`/already-deleted edges no-ops
      // (the same flag `alignBranchWithDefault` carries).
      await this.runGitWithEnv(
        barePath,
        ["rm", "--cached", "-r", "--ignore-unmatch", ".github/workflows"],
        idxEnv,
      );
      // Restore the default's workflow subtree ONLY when the default actually has one; an empty
      // ls-tree means the default deleted its whole `.github/workflows/`, and the rm above
      // already equalised to "none" (the #627 deleted-workflows edge).
      const defaultWfTree = (
        await this.runGit(barePath, ["ls-tree", defaultTip, "--", ".github/workflows"])
      ).trim();
      if (defaultWfTree.length > 0) {
        await this.runGitWithEnv(
          barePath,
          ["read-tree", "--prefix=.github/workflows", `${defaultTip}:.github/workflows`],
          { ...idxEnv, GIT_WORK_TREE: tmpWork },
        );
      }
      const newRoot = (await this.runGitWithEnv(barePath, ["write-tree"], idxEnv)).trim();
      if (!/^[0-9a-f]{40}$/.test(newRoot)) return null;
      // If the synthesised root equals realTip's own tree the branch is not actually behind
      // (e.g. realTip has no `.github` and the default has none) ⇒ ship realTip.
      const realTipTree = (await this.runGit(barePath, ["rev-parse", `${realTip}^{tree}`])).trim();
      if (newRoot === realTipTree) return null;
      // Deterministic committer date = realTip's own, so a no-new-work rebuild is byte-identical.
      const committerDate = (
        await this.runGit(barePath, ["show", "-s", "--format=%cI", realTip])
      ).trim();
      // Parent order: base (a prior overlay tip) FIRST, realTip LAST — see the method doc and
      // the PRD's broker note. A missing / non-40-hex prevCheckpointTip ⇒ single parent realTip.
      const parents: string[] = [];
      const prev = overlay.prevCheckpointTip;
      if (prev && /^[0-9a-f]{40}$/.test(prev)) parents.push(prev);
      parents.push(realTip);
      const commitArgs = ["-c", "commit.gpgsign=false", "commit-tree", newRoot];
      for (const p of parents) commitArgs.push("-p", p);
      commitArgs.push(
        "-m",
        `${OVERLAY_COMMIT_PREFIX} align .github/workflows with ${defaultTip} (realTip ${realTip.slice(0, 12)})`,
      );
      const ovSha = (
        await this.runGitWithEnv(barePath, commitArgs, {
          ...idxEnv,
          GIT_AUTHOR_NAME: AGENT_GIT_IDENTITY.name,
          GIT_AUTHOR_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_COMMITTER_NAME: AGENT_GIT_IDENTITY.name,
          GIT_COMMITTER_EMAIL: AGENT_GIT_IDENTITY.email,
          GIT_AUTHOR_DATE: committerDate,
          GIT_COMMITTER_DATE: committerDate,
        })
      ).trim();
      if (!/^[0-9a-f]{40}$/.test(ovSha)) return null;
      this.log.info("checkpoint overlay built (.github/workflows aligned to default)", {
        branch,
        real_tip: realTip,
        default_tip: defaultTip,
        overlay_sha: ovSha,
        parents: parents.length,
      });
      return ovSha;
    } catch (e) {
      this.log.warn("checkpoint overlay synthesis failed — shipping realTip", {
        branch,
        error: gitErrorMessage(e),
      });
      return null;
    } finally {
      await fs.rm(tmpIdx, { force: true }).catch(() => undefined);
      await fs.rm(tmpWork, { recursive: true, force: true }).catch(() => undefined);
    }
  }

  /** Remove the run's runner clone (a standalone clone, not a linked worktree — no
   *  bare interaction). The warm bare and the fetched refs/objects are kept. */
  async removeRunnerClone(clonePath: string): Promise<void> {
    await fs.rm(clonePath, { recursive: true, force: true });
  }

  /**
   * Files changed on the agent branch since it diverged from the default branch
   * (three-dot diff against the merge base) — used by a self_improve run to flag
   * guard-critical paths in its MR (PRD #46). Under (b) this is a WORKER-BARE
   * tree-to-tree diff (no working tree, no runner-owned config source read): the
   * caller passes the worker-side tracking ref that fetchAgentBranch wrote, and
   * `--name-only` fires no diff drivers. Returns null (NOT []) when the diff cannot be
   * computed, so the caller fails CLOSED — a loud "guard-path check unavailable" note
   * rather than silently raising no flag on a possibly guard-touching MR (M5 audit).
   * An empty list means "computed, nothing changed".
   */
  async changedFiles(barePath: string, trackingRef: string): Promise<string[] | null> {
    try {
      const baseRef = await this.defaultBranchRef(barePath);
      const out = await this.runGit(barePath, ["diff", "--name-only", `${baseRef}...${trackingRef}`]);
      return out.split("\n").map((l) => l.trim()).filter((l) => l !== "");
    } catch {
      return null;
    }
  }

  /**
   * PRD #212: the plan-turn changed-file list for the approval gate. Runs
   * `git status --porcelain` in the runner clone AS THE RUNNER UID (runGitAsRunner),
   * NEVER a worker-uid helper — `git status` touches the working tree and can fire
   * attacker-chosen .gitattributes filter.<name>.clean drivers that exec as the running
   * uid, so a worker-uid status would re-open code-exec as the PAT holder (PRD #51 M0).
   * A runner-uid status in the runner-owned clone is the untrusted uid exec'ing in its
   * own tree — not a boundary crossing (git.ts topology comment ~:39-44).
   *
   * BEST-EFFORT: returns [] on ANY error (a benign git failure, or a pathological/hostile
   * tree that hangs to GIT_TIMEOUT_MS). This is a VISIBILITY feature — it must never throw
   * into the awaiting_approval report, whose reportState await has no .catch and would
   * abort the run instead of parking it at the gate. Mirrors changedFiles → [] on error.
   *
   * Honors .gitignore (no --ignored): the list is bounded to non-ignored
   * tracked-modifications + untracked files — the plan-turn writes worth surfacing.
   * Each returned element is a raw porcelain line ("XY <path>"); the LEADING space of the
   * XY status code is meaningful, so DO NOT trim lines (only split + drop empties). The
   * server (api) re-sanitizes and caps each line; this is not the last line of defense.
   */
  async planChangedFiles(cwd: string): Promise<string[]> {
    try {
      const out = await this.runGitAsRunner(cwd, ["status", "--porcelain"]);
      return out.split("\n").filter((l) => l.length > 0);
    } catch (err) {
      this.log.debug("plan-turn git status failed (best-effort → no changes)", {
        cwd,
        error: gitErrorMessage(err),
      });
      return [];
    }
  }

  /**
   * Issue #281 / CodeRabbit #655: the porcelain read for the no-progress detector's
   * worktree fingerprint. Identical git invocation and runner-uid rationale as
   * planChangedFiles, but returns `null` on a failed read instead of `[]`, so an
   * UNREADABLE status is never mistaken for a genuinely clean tree. planChangedFiles
   * swallows errors to `[]` on purpose (a plan-gate visibility feature that must never
   * throw); the fingerprint needs the opposite — a failed read must NOT look like "no
   * changes", or a run whose status read keeps failing while the lead repeats itself
   * could trip the detector without the tree ever having been verified. The caller
   * treats `null` as "cannot assert unchanged" (no trip).
   */
  async worktreeStatus(cwd: string): Promise<string[] | null> {
    try {
      const out = await this.runGitAsRunner(cwd, ["status", "--porcelain"]);
      return out.split("\n").filter((l) => l.length > 0);
    } catch (err) {
      this.log.debug("fingerprint git status failed (→ null, cannot assert unchanged)", {
        cwd,
        error: gitErrorMessage(err),
      });
      return null;
    }
  }

  /**
   * PRD #759 M1 — commit the runner clone's uncommitted work to a clearly-marked
   * THROWAWAY commit on the park path, so the existing fetch-back + #628 checkpoint
   * broker carry that work off the tree before the reseed's `fs.rm` wipes it. This is
   * the one thing run #685 lacked: every durability layer captures committed commits
   * only, so ~4h of mid-milestone work that had never been committed was lost on park.
   *
   * Runs AS THE RUNNER UID (runGitAsRunner) in the runner-owned clone — never a
   * worker-uid git op. `git status`/`add`/`commit` touch the working tree and can fire
   * attacker-chosen .gitattributes filter drivers that exec as the running uid; a
   * worker-uid write would re-open code-exec as the PAT holder (PRD #51 M0). A runner-uid
   * write in the runner-owned clone is the untrusted uid exec'ing in its own tree — not a
   * boundary crossing (git.ts topology comment ~:39-48). The AGENT_GIT_IDENTITY and
   * `commit.gpgsign=false` planted at seed (~:536-538) mean the commit needs no identity
   * flags and cannot be blocked by a signing config.
   *
   * The subject is prefixed WIP_PARK_COMMIT_PREFIX so it is a recognizable throwaway:
   * M2 detects it on the adopted tip and `git reset --soft <parent>` restores the content
   * to UNCOMMITTED at adopt time, so the marker never enters the history the agent builds
   * on and never reaches the MR. This deliberately reverses PRD #218 D6's "no auto-commit
   * on park" — a decision the maintainer explicitly asked to revisit (PRD #759 M1 / D3):
   * a marked throwaway stripped back to uncommitted at adopt time never masquerades as
   * reviewed work, so #218 D6's "a half-applied edit that survives is worse than one that
   * does not" no longer applies.
   *
   * BEST-EFFORT: every error is caught, logged, and returns `false` rather than thrown —
   * a commit failure must NEVER propagate, because parking is the state that preserves the
   * tree and a failed park loses MORE than a missing WIP commit (D4). Returns `true` only
   * when a marker commit was actually created (an already-clean tree returns `false` —
   * nothing to commit).
   */
  async commitWipMarker(clonePath: string): Promise<boolean> {
    try {
      const status = await this.runGitAsRunner(clonePath, ["status", "--porcelain"]);
      if (status.split("\n").filter((l) => l.length > 0).length === 0) {
        return false; // clean tree — nothing to save
      }
      await this.runGitAsRunner(clonePath, ["add", "-A"]);
      await this.runGitAsRunner(clonePath, [
        "commit",
        "-m",
        `${WIP_PARK_COMMIT_PREFIX} interrupted work auto-saved on usage-limit park (throwaway; restored uncommitted on resume)`,
      ]);
      return true;
    } catch (err) {
      this.log.warn("WIP park auto-commit failed (best-effort → not committed)", {
        cwd: clonePath,
        error: gitErrorMessage(err),
      });
      return false;
    }
  }

  /**
   * The unified diff of the reviewed `branch` against `base` (three-dot: the changes on
   * `branch` since it diverged from `base`), for a PRD #400 M4b diff-review run. `branch` is
   * resolved as the bare's remote-tracking ref (`refs/remotes/origin/<name>`, which every
   * fetch updates — the same namespace `changedFiles` reads), so the caller must have
   * fetched the bare (ensureClone) first; the reviewed task branch is pushed to origin (M2),
   * so it exists there. `base` may be EITHER a branch name (resolved the same way, under
   * `refs/remotes/origin/`) OR a commit-ish — the seed commit sha a handoff records as its
   * base when created without --base (issue #403 F3), which does not exist under
   * `refs/remotes/origin/` but is present in the mirror as an ancestor of the reviewed
   * branch.
   *
   * The result is CAPPED at REVIEW_DIFF_MAX_BYTES with a truncation marker: a pathological
   * diff must not blow the reviewer model's context or the worker's memory. `--no-color`
   * keeps the text plain for the model, and `--no-ext-diff` is LOAD-BEARING: gitEnv pins
   * `diff.external=true` (a code-exec-key neutralization), so a plain `git diff` would run
   * that no-op external driver and emit NOTHING — `changedFiles` sidesteps it with
   * `--name-only`; a real patch must disable it explicitly.
   */
  async reviewDiff(barePath: string, base: string, branch: string): Promise<string> {
    // issue #403 F3: `base` is usually a branch name (resolved under refs/remotes/origin/), but a
    // handoff created without --base records the SEED COMMIT sha as its base so the review diffs
    // only the worker's commits, not the user's seeded HEAD. A raw sha does not resolve under
    // refs/remotes/origin/, so fall back to using `base` verbatim — the seed commit is present in
    // the mirror as an ancestor of the reviewed branch.
    const remoteBaseRef = `refs/remotes/origin/${base}`;
    const baseRef = (await this.refExists(barePath, remoteBaseRef)) ? remoteBaseRef : base;
    const branchRef = `refs/remotes/origin/${branch}`;
    const out = await this.runGit(barePath, ["diff", "--no-color", "--no-ext-diff", `${baseRef}...${branchRef}`]);
    const buf = Buffer.from(out, "utf8");
    if (buf.byteLength <= REVIEW_DIFF_MAX_BYTES) return out;
    const marker = `\n… diff truncated at ${REVIEW_DIFF_MAX_BYTES} bytes (${buf.byteLength} total) …\n`;
    // Slice on a byte boundary so a giant diff cannot balloon the string we keep. A cut
    // through a multi-byte rune yields at most one U+FFFD; harmless for review text.
    return buf.subarray(0, REVIEW_DIFF_MAX_BYTES).toString("utf8") + marker;
  }

  /**
   * PRD #377 M1 — the full unified diff of the agent branch (`trackingRef`) against the
   * default-branch base, preserved on a `failed` report when a GitHub run's branch touches
   * `.github/workflows/**` and the bot's repo-only PAT cannot push it. Mirrors
   * `changedFiles`'s base resolution (`defaultBranchRef`, three-dot) but emits a REAL patch
   * instead of a name list.
   *
   * `--no-ext-diff` is LOAD-BEARING for the same reason it is in `reviewDiff`: gitEnv pins
   * `diff.external=true` (a code-exec-key neutralization — see GIT_CODE_EXEC_KEY_PINS), so a
   * plain `git diff` runs that no-op external driver and emits NOTHING; a real patch must
   * disable it explicitly. `changedFiles` sidesteps it via `--name-only`. `--no-color` keeps
   * the text plain.
   *
   * The result is CAPPED at REVIEW_DIFF_MAX_BYTES with a truncation marker, sliced on a byte
   * boundary (a cut through a multi-byte rune yields at most one U+FFFD). It is secret-scrubbed
   * by the CALLER (redactText) before it leaves the worker — this method does not scrub. Returns
   * `null` on ANY failure (try/catch), so the caller can still report the failed outcome without
   * a patch rather than turning a diff-computation error into a second failure.
   */
  async workflowScopeDiff(barePath: string, trackingRef: string): Promise<string | null> {
    try {
      const baseRef = await this.defaultBranchRef(barePath);
      const out = await this.runGit(barePath, [
        "diff",
        "--no-color",
        "--no-ext-diff",
        `${baseRef}...${trackingRef}`,
      ]);
      const buf = Buffer.from(out, "utf8");
      if (buf.byteLength <= REVIEW_DIFF_MAX_BYTES) return out;
      const marker = `\n… diff truncated at ${REVIEW_DIFF_MAX_BYTES} bytes (${buf.byteLength} total) …\n`;
      return buf.subarray(0, REVIEW_DIFF_MAX_BYTES).toString("utf8") + marker;
    } catch {
      return null;
    }
  }

  /**
   * PRD #974 M2 (load-bearing security) — scan the commit range the finalize push would carry
   * for secrets with the repo's pinned gitleaks, GitLeaks' three silencers DISABLED, and
   * return whether the scan is TRUSTWORTHY plus any findings.
   *
   * Range: `base..head` (TWO-dot — the commits ON the branch and NOT on the default branch,
   * exactly what a push carries), base = defaultBranchRef, head = trackingRef. An empty range
   * (0 commits) is the nothing-to-scan case and returns trusted with no findings so the normal
   * push proceeds.
   *
   * WHY the BARE, and why the silencers are disabled: GitHub Push Protection (GH013) ignores
   * `.gitleaks.toml`, `.gitleaksignore` and inline `//gitleaks:allow`, so a scan that honored
   * any of them would go green on a secret GitHub still rejects (a vacuous fix). We disable all
   * three: (1) scanning the worker BARE (no working tree) means a repo-shipped `.gitleaks.toml`
   * / `.gitleaksignore` cannot be auto-discovered from disk; (2) an EXPLICIT `-c` config forces
   * gitleaks' embedded default ruleset and skips `.gitleaks.toml` auto-discovery; (3)
   * `--ignore-gitleaks-allow` disables inline allow comments. Proven against all three silencers
   * (issue #974 step 5). The bare's git config is WORKER-authored (not attacker-controlled), so
   * gitleaks' internal `git log -p` cannot fire an attacker-chosen diff driver.
   *
   * TRUST: gitleaks prints "no leaks found" rc 0 on an unresolved/empty range, so a clean
   * verdict is meaningless unless the scan actually walked the range. scanIsTrustworthy is that
   * liveness gate (exec ok, no error token on stderr, the "N commits scanned" line present AND
   * equal to the range length). The caller acts on findings ONLY when trusted; when untrusted it
   * fails OPEN and relies on the GH013 remote backstop.
   *
   * The temp config + JSON report live under os.tmpdir() (worker-writable; the runner's 0700 tmp
   * is not) and are removed in a finally. gitleaks runs under `--exit-code 0`, so a finding is
   * still exit 0 and any nonzero exit is an INSTRUMENT failure (execOk = !error).
   */
  async secretScanRange(
    barePath: string,
    trackingRef: string,
  ): Promise<{ trusted: boolean; findings: SecretFinding[] }> {
    // Resolve the range base INSIDE the fail-open envelope: defaultBranchRef throws when no
    // default ref resolves, and an uncaught throw here would escape to the generic catch and
    // report `failed` with NO preserved_patch — the exact work-loss this feature prevents. So
    // it fails open like every other setup step below (the sibling changedFiles guards the
    // identical call the same way).
    let base: string;
    try {
      base = await this.defaultBranchRef(barePath);
    } catch {
      this.log.warn("finalize secret scan: could not resolve the default branch; failing open", {
        barePath,
      });
      return { trusted: false, findings: [] };
    }
    const logRange = `${base}..${trackingRef}`;
    let expectedCommits = 0;
    try {
      const out = await this.runGit(barePath, ["rev-list", "--count", logRange]);
      expectedCommits = Number.parseInt(out.trim(), 10);
      if (Number.isNaN(expectedCommits)) expectedCommits = 0;
    } catch {
      // A failed count is an untrusted scan setup — do NOT block; fail open to the backstop.
      this.log.warn("finalize secret scan: could not count the push range; failing open", {
        barePath,
      });
      return { trusted: false, findings: [] };
    }
    if (expectedCommits === 0) {
      // Nothing to scan (the push carries no new commit); the normal push proceeds.
      return { trusted: true, findings: [] };
    }

    const scratch = os.tmpdir();
    const configPath = path.join(scratch, `uzi-gl-config-${randomUUID()}.toml`);
    const reportPath = path.join(scratch, `uzi-gl-report-${randomUUID()}.json`);
    try {
      // `[extend] useDefault=true` forces gitleaks' embedded default ruleset and, being an
      // explicit `-c`, skips auto-discovery of the target repo's `.gitleaks.toml`.
      await fs.writeFile(configPath, "[extend]\nuseDefault = true\n", "utf8");
      const args = gitleaksArgs({ sourcePath: barePath, logRange, configPath, reportPath });
      // gitleaks (baked on PATH by the worker image, see agent/templates/*/Dockerfile). Do NOT
      // throw on a nonzero exit: under --exit-code 0 a finding is exit 0, so any nonzero is an
      // instrument failure captured as `error` and folded into the trust gate.
      let execOk = true;
      let stderr = "";
      try {
        const res = await execFileAsync("gitleaks", args, {
          // gitEnv(): the hardened REPLACEMENT env (no join token / API URL, plus the
          // core.hooksPath / GIT_CONFIG_NOSYSTEM / global=/dev/null pins), so gitleaks'
          // internal `git -C … log -p` runs WITHOUT worker credentials in its environment and
          // cannot fire a planted hook or read a system/global config as the worker uid. gitEnv
          // carries PATH+HOME (+TMPDIR), so gitleaks itself still runs; it needs no secret env.
          env: gitEnv(),
          maxBuffer: 64 * 1024 * 1024,
          timeout: GIT_TIMEOUT_MS,
        });
        stderr = res.stderr ?? "";
      } catch (err) {
        execOk = false;
        stderr = typeof (err as { stderr?: unknown }).stderr === "string"
          ? (err as { stderr: string }).stderr
          : gitErrorMessage(err);
      }

      // Read + parse the report; a failed read/parse (fs error, truncated file) is UNTRUSTED —
      // return trusted:false rather than a clean verdict on an unreadable report. SIZE-CAP the
      // read first: the report is O(findings) over ATTACKER-authored commits, so a committed
      // file of millions of secret-shaped lines could balloon it to multiple GB and OOM the
      // worker. An over-cap report is an untrusted scan (fail open to the GH013 backstop), never
      // an OOM. parseGitleaksReport additionally caps the number of findings it materialises.
      let findings: SecretFinding[];
      try {
        const st = await fs.stat(reportPath);
        if (st.size > SECRET_SCAN_REPORT_MAX_BYTES) {
          this.log.warn(
            "finalize secret scan: gitleaks report exceeds the size cap; failing open",
            { barePath, bytes: st.size, cap: SECRET_SCAN_REPORT_MAX_BYTES },
          );
          return { trusted: false, findings: [] };
        }
        const raw = await fs.readFile(reportPath, "utf8");
        findings = parseGitleaksReport(raw);
      } catch {
        this.log.warn("finalize secret scan: could not read the gitleaks report; failing open", {
          barePath,
        });
        return { trusted: false, findings: [] };
      }

      const scannedCommits = commitsScannedFromStderr(stderr);
      const trusted = scanIsTrustworthy({ stderr, scannedCommits, expectedCommits, execOk });
      this.log.debug("finalize secret scan complete", {
        barePath,
        expectedCommits,
        scannedCommits,
        execOk,
        trusted,
        findings: findings.length,
      });
      return { trusted, findings };
    } finally {
      await fs.rm(configPath, { force: true }).catch(() => undefined);
      await fs.rm(reportPath, { force: true }).catch(() => undefined);
    }
  }

  /**
   * PRD #456 M1 — fetch the CURRENT default-branch tip from origin into the worker bare
   * and return its SHA. This is a WORKER-uid AUTHENTICATED op (mirrors `fetch` at the top
   * of this class): the worker owns the bare and holds the PAT, and the agent never has a
   * push/fetch credential, so this cannot run runner-uid.
   *
   * Why a fresh fetch at all: the finalize `fetchAgentBranch` is a LOCAL `file://` fetch,
   * and the last origin contact was at claim time (`ensureClone`), so
   * `refs/remotes/origin/<default>` in the bare still holds the CLAIM-TIME tip. The align
   * target must be main's tip AS IT IS NOW (main may have advanced its `.github/workflows/**`
   * since the clone base), so we re-fetch it here, immediately before the align.
   *
   * The returned SHA is the align TARGET and is the FRESHLY-FETCHED tip — NEVER
   * `defaultBranchRef`'s frozen-mirror fallback rungs (N2): those can resolve to a stale
   * `refs/heads/main` mirror fixed at first clone, which is exactly what this method exists
   * to bypass. We read `refs/remotes/origin/<default>` (which this fetch just updated),
   * falling back to `FETCH_HEAD`, and validate a 40-hex OID before returning.
   */
  async fetchDefaultTip(
    barePath: string,
    defaultBranch: string,
    pat?: string,
    cloneUrl?: string,
    username?: string,
  ): Promise<string> {
    const scope = cloneUrl ? httpScopeForUrl(cloneUrl) : undefined;
    return this.withLock(barePath, async () => {
      await this.runGit(barePath, ["fetch", "origin", defaultBranch], pat, scope, username);
      // The configured refspec (+refs/heads/*:refs/remotes/origin/*) means a named-branch
      // fetch updates the remote-tracking ref; read the fresh tip off it.
      let sha = (
        await this.runGit(barePath, [
          "rev-parse",
          "--verify",
          `refs/remotes/origin/${defaultBranch}^{commit}`,
        ]).catch(() => "")
      ).trim();
      if (!/^[0-9a-f]{40}$/.test(sha)) {
        // Fallback: some server/refspec shapes leave only FETCH_HEAD current.
        sha = (
          await this.runGit(barePath, ["rev-parse", "--verify", "FETCH_HEAD^{commit}"]).catch(
            () => "",
          )
        ).trim();
      }
      if (!/^[0-9a-f]{40}$/.test(sha)) {
        throw new Error(
          `fetchDefaultTip: could not resolve a 40-hex tip for origin/${defaultBranch}`,
        );
      }
      return sha;
    });
  }

  /**
   * PRD #456 M1 (D1) — true iff the branch tip's `.github/workflows/` tree DIFFERS from the
   * freshly-fetched default tip. This is the precise trigger for the finalize align: only a
   * behind-on-workflows branch (main moved those files after the clone base, or vice versa)
   * needs realigning, and this keeps the align/conflict surface minimal.
   *
   * WORKER-uid, bare-only. A TWO-dot direct tree compare (`trackingRef` vs `defaultTip`), NOT
   * three-dot — we want "do these two trees' workflow files differ right now", not "what did
   * the branch change since a merge base". `--name-only` correctly sidesteps the pinned
   * `diff.external` code-exec neutralizer (see GIT_CODE_EXEC_KEY_PINS / `changedFiles`), so no
   * `--no-ext-diff` is needed. On ANY error this returns false (FAIL-OPEN to the normal push):
   * a run must never be blocked on an inability to compute this.
   */
  async workflowTreeDiffers(
    barePath: string,
    trackingRef: string,
    defaultTip: string,
  ): Promise<boolean> {
    try {
      const out = await this.runGit(barePath, [
        "diff",
        "--name-only",
        trackingRef,
        defaultTip,
        "--",
        ".github/workflows/",
      ]);
      return out.trim() !== "";
    } catch {
      return false;
    }
  }

  /**
   * PRD #456 M1 (B3) — align the run's branch with the fresh default IN THE RUNNER CLONE,
   * never the worker bare. The clone is the ONLY working tree at finalize, and it is
   * RUNNER-owned, so every git op here runs runner-uid (`runGitAsRunner`) — a worker-uid op
   * in the runner clone would hit the B2 dubious-ownership boundary.
   *
   * `baseTip` is the agent's own committed tip BEFORE any align (captured by the caller from
   * `branchTip`). We reset the clone's branch to it before each strategy so (a) leftover
   * UNCOMMITTED artifacts cannot block the op, and (b) a rebase FALLBACK after a clean merge
   * replays the ORIGINAL agent commits rather than the merge commit the merge left on
   * `refs/heads/<branch>`. The committed work is never lost — it lives in the commit object;
   * we only move the branch ref back to it. (This `baseTip` parameter is a small, deliberate
   * deviation from the spec's 4-arg signature; without it a merge→rebase fallback would rebase
   * the merge commit and the S3 commit-count assertion would spuriously fire.)
   *
   * `defaultTip`'s objects are reachable in the clone via its `--shared` alternate (the worker
   * bare received them in `fetchDefaultTip`), so we anchor them under a fixed LOCAL ref via
   * `update-ref` (no `file://` fetch, no PAT, no protocol.file.allow concern) and merge/rebase
   * against that ref, deleting it in a `finally`.
   *
   * `git clean -fd`, NOT `-fdx`: `-x` also deletes IGNORED files (node_modules, build outputs,
   * a local .env), which are not part of the committed work and may be needed / expensive to
   * recreate; removing untracked-but-tracked-eligible files (`-fd`) is enough to unblock a
   * merge/rebase, which only refuses when an UNtracked file would be overwritten.
   *
   * Returns `"aligned"` on a clean result. On a genuine CONFLICT it runs the matching abort
   * and returns `"conflict"`; an unrelated (non-conflict) git failure is RETHROWN. For a
   * rebase it uses `--empty=keep --no-autosquash` and asserts the branch's own commit count is
   * preserved across the replay (S3) — a silent drop throws rather than pushing truncated work.
   *
   * `"workflow-subtree"` (issue #627) is the narrow PRIMARY strategy: it overlays ONLY the
   * default tip's `.github/workflows/` subtree onto the agent tip (making that subtree equal
   * the default's, deletions included) and never conflicts, so it has NO `"conflict"` return —
   * a genuine git error propagates so the caller can fall back to merge/rebase. See its arm.
   */
  async alignBranchWithDefault(
    clonePath: string,
    branch: string,
    baseTip: string,
    defaultTip: string,
    strategy: "merge" | "rebase" | "workflow-subtree",
  ): Promise<"aligned" | "conflict"> {
    const targetRef = "refs/uzi-align/target";
    try {
      // Anchor the fresh default under a local ref (objects reachable via the --shared
      // alternate). Not under refs/heads/* so it never pollutes the branch namespace.
      await this.runGitAsRunner(clonePath, ["update-ref", targetRef, defaultTip]);
      // Rewind to the pre-align committed agent tip: clears uncommitted scratch AND undoes a
      // prior merge's commit so each strategy starts from the original agent work.
      await this.runGitAsRunner(clonePath, ["checkout", "--force", branch]);
      await this.runGitAsRunner(clonePath, ["reset", "--hard", baseTip]);
      await this.runGitAsRunner(clonePath, ["clean", "-fd"]);

      if (strategy === "workflow-subtree") {
        // PRD #456 (issue #627) — the PRIMARY, narrow strategy. Overlay ONLY the default
        // tip's `.github/workflows/` subtree onto the agent tip so the branch's workflow tree
        // becomes BYTE-FOR-BYTE equal to the default's (all GitHub's tip-vs-default check
        // needs) WITHOUT dragging in any unrelated change on main. It cannot conflict — the
        // caller gates on the branch never having modified a workflow file (Part B) — so there
        // is no abort/`"conflict"` return here. A genuine git error PROPAGATES (the caller
        // falls back to merge/rebase); only "there are staged changes" is treated as non-error.
        //
        // 1. Remove the branch's entire workflow set from index+worktree (--ignore-unmatch so
        //    it is a no-op when the branch has no such path).
        await this.runGitAsRunner(clonePath, [
          "rm",
          "-r",
          "--ignore-unmatch",
          ".github/workflows",
        ]);
        // 2. Restore the default's files ONLY when the default actually has a workflows tree.
        //    An empty ls-tree means the default deleted its whole `.github/workflows/`; a bare
        //    `checkout <ref> -- .github/workflows` would ERROR there, so skip it — the rm above
        //    already achieved the equalization ("remove them all"). rm-then-restore makes the
        //    index at that path equal the default's tree exactly (extras gone, missing added).
        const defaultWfTree = (
          await this.runGitAsRunner(clonePath, ["ls-tree", targetRef, "--", ".github/workflows"])
        ).trim();
        if (defaultWfTree.length > 0) {
          await this.runGitAsRunner(clonePath, ["checkout", targetRef, "--", ".github/workflows"]);
        }
        // 3. If nothing is staged the branch was already equal — make NO commit and return
        //    aligned (the normal push then proceeds). `diff --cached --name-only` exits 0
        //    whether or not there are changes (unlike `--quiet`), so its stdout is unambiguous.
        const staged = (
          await this.runGitAsRunner(clonePath, ["diff", "--cached", "--name-only"])
        ).trim();
        if (staged.length === 0) {
          return "aligned";
        }
        // 4. A single overlay commit on top of baseTip — a fast-forward, so the original agent
        //    SHAs are preserved and nothing is rebased. Pass an ident explicitly so the commit
        //    succeeds even if the clone lacks user.name/email (matches how the tests commit).
        await this.runGitAsRunner(clonePath, [
          "-c",
          `user.name=${AGENT_GIT_IDENTITY.name}`,
          "-c",
          `user.email=${AGENT_GIT_IDENTITY.email}`,
          "-c",
          "commit.gpgsign=false",
          "commit",
          "-m",
          `chore: align .github/workflows with ${defaultTip}`,
        ]);
        return "aligned";
      }

      if (strategy === "merge") {
        try {
          await this.runGitAsRunner(clonePath, ["merge", "--no-edit", targetRef]);
          return "aligned";
        } catch (err) {
          if (await this.inMerge(clonePath)) {
            await this.runGitAsRunner(clonePath, ["merge", "--abort"]).catch(() => undefined);
            return "conflict";
          }
          throw err; // a non-conflict git failure — not ours to swallow.
        }
      }

      // rebase: count the branch's own commits (ahead of the target) before and after so a
      // silently dropped commit is caught (S3). `--reapply-cherry-picks` re-applies commits
      // whose change already landed on the target (git drops these by default), so a branch
      // whose work overlaps main's new commits keeps ITS commits and the count stays honest —
      // otherwise the S3 count guard would false-trip on safe, landable work.
      const before = await this.countAhead(clonePath, defaultTip, branch);
      try {
        await this.runGitAsRunner(clonePath, [
          "rebase",
          "--empty=keep",
          "--no-autosquash",
          "--reapply-cherry-picks",
          targetRef,
        ]);
      } catch (err) {
        if (await this.inRebase(clonePath)) {
          await this.runGitAsRunner(clonePath, ["rebase", "--abort"]).catch(() => undefined);
          return "conflict";
        }
        throw err; // a non-conflict git failure — rethrow.
      }
      const after = await this.countAhead(clonePath, defaultTip, branch);
      if (after < before) {
        throw new Error(
          `alignBranchWithDefault: rebase dropped commits (${before} → ${after}) — refusing to push truncated work`,
        );
      }
      return "aligned";
    } finally {
      await this.runGitAsRunner(clonePath, ["update-ref", "-d", targetRef]).catch(
        () => undefined,
      );
    }
  }

  /** True while a merge is in progress in `clonePath` (a conflict left it mid-merge).
   *  Checked via the on-disk `.git/MERGE_HEAD` marker rather than a git command so it is
   *  uid-independent (the clone is runner-owned) and cannot be confused by a non-conflict
   *  git failure. The runner clone's `.git` is a real directory (a `clone --shared`). */
  private async inMerge(clonePath: string): Promise<boolean> {
    return pathExists(path.join(clonePath, ".git", "MERGE_HEAD"));
  }

  /** True while a rebase is in progress in `clonePath` (a conflict paused the replay).
   *  Either backend leaves its state dir behind: `rebase-merge` (the default merge backend)
   *  or `rebase-apply` (the am backend). Marker-file check for the same reasons as inMerge. */
  private async inRebase(clonePath: string): Promise<boolean> {
    return (
      (await pathExists(path.join(clonePath, ".git", "rebase-merge"))) ||
      (await pathExists(path.join(clonePath, ".git", "rebase-apply")))
    );
  }

  /** Count of commits reachable from `tip` but not from `base`, runner-uid in the clone.
   *  Any failure answers 0 — the caller only compares before-vs-after, and a symmetric
   *  failure (0 == 0) simply skips the assertion rather than false-failing a good rebase. */
  private async countAhead(clonePath: string, base: string, tip: string): Promise<number> {
    const out = await this.runGitAsRunner(clonePath, [
      "rev-list",
      "--count",
      `${base}..${tip}`,
    ]).catch(() => "0");
    const n = Number.parseInt(out.trim(), 10);
    return Number.isFinite(n) && n > 0 ? n : 0;
  }

  private async cloneBare(repoUrl: string, dest: string, pat?: string, scope?: string, username?: string): Promise<void> {
    // Widen the cleanup over the WHOLE body: a transient blip on the post-clone
    // authenticated fetch (or the config/disableAutoMaintenance steps) must also rm
    // `dest` before rethrowing. Otherwise a retry would run `git clone --bare` into a
    // non-empty dir → a deterministic "already exists" failure (permanent), converting a
    // transient into a permanent one. This guarantees each retry starts from a clean bare.
    try {
      await this.runGit(undefined, ["clone", "--bare", repoUrl, dest], pat, scope, username);
      // Convert the mirror refspec to remote-tracking so future fetches write to
      // refs/remotes/origin/*. Under (b) the bare's refs/heads/* stay the
      // stale mirror; the agent branch is resolved via refs/remotes/origin/* (resume)
      // and lands back in refs/uzi-runner/* (fetchAgentBranch), never the bare's heads.
      await this.runGit(dest, ["config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"]);
      // Issue #134 — belt for a FRESH bare; the warm path in ensureClone is what covers the
      // deployed fleet. The bare is LONG-LIVED and shared across runs, so auto-maintenance is
      // arguably wanted here. Disabled anyway because a detached gc can run CONCURRENTLY with
      // a claim's fetch — a race nobody chose. The cost is real and tracked: an unmaintained
      // bare never repacks and never prunes, so `gc.autoPackLimit` (default 50) stops guarding
      // pack growth against a fixed-size PVC. The clean fix is a DELIBERATE
      // `git maintenance run --task=incremental-repack` taken inside `withLock(barePath)`,
      // which already serializes every bare operation and so is race-free by construction.
      await this.disableAutoMaintenance(dest);
      await this.fetch(dest, pat, scope, username);
    } catch (err) {
      await fs.rm(dest, { recursive: true, force: true });
      throw err;
    }
  }

  private async fetch(barePath: string, pat?: string, scope?: string, username?: string): Promise<void> {
    // issue #781 — the `--prune` FLAG (NOT the config form `fetch.prune` /
    // `remote.origin.prune`) prunes only the ref namespace of this fetch's configured
    // refspec `+refs/heads/*:refs/remotes/origin/*` — i.e. `refs/remotes/origin/*` — so a
    // remote branch deleted upstream stops seeding stale disjoint bases while
    // `refs/uzi-runner/*` and the separate checkpoint mirror fetch's `refs/uzi-checkpoints/*`
    // stay intact. The config form would additionally prune the locally-mirrored checkpoint
    // refs on the separate mirror fetch just below (whose origin has no `refs/uzi-checkpoints/*`
    // to match), degrading PRD #122 M8 cross-worker recovery.
    await this.runGit(barePath, ["fetch", "--prune", "origin"], pat, scope, username);
    // Refresh origin/HEAD so a remote default-branch change takes effect. Best
    // effort: defaultBranchRef has fallbacks if this symref is absent.
    await this.tryGit(barePath, ["remote", "set-head", "origin", "--auto"]);
    // PRD #122 M8 — mirror origin's brokered checkpoint refs into the bare, BEST-EFFORT.
    // This is the CROSS-WORKER signal path: a worker with no local refs/uzi-runner/<branch>
    // can still pull ANOTHER worker's published refs/uzi-checkpoints/<branch> and seed off
    // it (runnerCloneForBranch's checkpoint candidate). Rides the authenticated fetch (the
    // PAT is available here). Tolerates absence — origin may carry no such refs (fetch
    // succeeds fetching nothing), and the `.catch` covers an older server or a permission
    // edge that would otherwise surface as a fetch error.
    await this.runGit(
      barePath,
      ["fetch", "origin", "+refs/uzi-checkpoints/*:refs/uzi-checkpoints/*"],
      pat,
      scope,
      username,
    ).catch(() => undefined);
  }

  /** Resolve a ref for the repo's default branch (the runner-clone base + the
   *  changedFiles diff base). */
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

  /** issue #299 / PRD #759 — true when origin's brokered checkpoint ref for `branch`
   *  holds COMMITTED work that a report-only completion would orphan. `fetch()` mirrors
   *  `refs/uzi-checkpoints/<branch>` into the bare best-effort on every fetch, so this
   *  catches a checkpoint any PRIOR/cross-worker attempt landed; the runner pairs it with
   *  its own `lastPublishedTip` to also catch a checkpoint THIS worker published mid-run
   *  (not yet mirrored locally). The report-only completion guard uses the union to refuse
   *  orphaning a published checkpoint.
   *
   *  The marker-only exception (PRD #759): a usage-limit park publishes a throwaway
   *  `wip(park):` marker commit (WIP_PARK_COMMIT_PREFIX) to the checkpoint ref, and nothing
   *  deletes it on resume. A checkpoint whose tip is ONLY such a marker — with no committed
   *  milestone below it — is NOT committed work: it is an abandoned WIP marker, so this
   *  returns false for it and a legitimate report-only completion is no longer failed on
   *  its mere existence. The discriminator is ancestry, not descent: a marker-only checkpoint
   *  is one whose parent is an ancestor-or-equal of the recovery floor; anything else (a
   *  non-marker tip, or a marker whose parent STRICTLY DESCENDS the floor OR has DIVERGED from
   *  it — the "main advanced during a park" shape) is a genuine committed milestone and still
   *  returns true.
   *
   *  Only a SINGLE leading marker is ever stripped, and that is sufficient: the reseed's
   *  `reset --soft` (see runnerClone, git.ts ~654-669) removes an adopted marker from the
   *  branch history, so a resumed branch never carries a marker into the work the agent
   *  builds on. Any subsequent park therefore plants its marker on a parent that is a real
   *  commit or the base — never on another marker — so a one-marker strip cannot miss a
   *  buried committed milestone. */
  async hasCommittedCheckpoint(barePath: string, branch: string): Promise<boolean> {
    const ref = `refs/uzi-checkpoints/${branch}`;
    // 1) No checkpoint ref → nothing to orphan.
    if (!(await this.refExists(barePath, ref))) return false;
    // 2) Resolve the tip. A non-marker tip is a genuine committed checkpoint — return true,
    //    byte-identical to the pre-PRD-759 existence check for every real checkpoint.
    const tip = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${ref}^{commit}`])).trim();
    if (!(await this.isWipParkMarker(barePath, tip))) return true;
    // 3) The tip IS a `wip(park):` marker. The committed work (if any) is its parent and
    //    below. A root-commit marker (no readable parent) has nothing committed below to
    //    orphan → false (mirrors the root-marker handling in runnerClone, git.ts ~585-591).
    const parent = (await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${tip}^^{commit}`])).trim();
    if (parent === "") return false;
    // Compute the recovery floor similar to the way the reseed does (git.ts ~535): origin's
    // branch if pushed, else the default branch. (Only "similar to": the reseed excludes a
    // DISJOINT origin ref via originExists = raw existence AND non-disjoint, git.ts:475/484,
    // whereas this helper uses raw refExists. That is acceptable here because a disjoint
    // origin floor only pushes toward BLOCK — the safe direction: a disjoint parent is not an
    // ancestor of it, so the committed-work test below returns true.)
    //
    // The parent is a marker-only checkpoint iff it is an ancestor-or-equal of the floor;
    // anything else — the parent STRICTLY descends the floor, OR it DIVERGED from the floor
    // (shared ancestor, neither contains the other; the "main advanced during a park" shape) —
    // is committed work that a report-only completion would orphan, so it still blocks. This
    // mirrors the reseed's diverged-WIP leg (git.ts:713), which uses the same
    // isAncestor(markerParent, floor) discriminator to decide "no committed milestones live
    // below the marker". isAncestor is TRUE at equality, so the parent == floor case is a
    // marker-only checkpoint (allow); it also returns false on a missing/broken ref, which
    // fails safe toward BLOCK here.
    try {
      const floorRef = (await this.refExists(barePath, `refs/remotes/origin/${branch}`))
        ? `refs/remotes/origin/${branch}`
        : await this.defaultBranchRef(barePath);
      // Committed work exists below the marker iff the marker's parent is NOT an
      // ancestor-or-equal of the recovery floor:
      //   parent strictly descends floor → isAncestor(parent,floor) false → block ✓
      //   parent == floor                → isAncestor true (true at equality) → allow ✓
      //   parent is an ancestor of floor → isAncestor true                    → allow ✓
      //   parent and floor DIVERGED      → isAncestor(parent,floor) false → block ✓
      return !(await this.isAncestor(barePath, parent, floorRef));
    } catch (err) {
      // Fail-safe: the floor could not be resolved. Preserve the guard rather than risk
      // orphaning committed work below the marker.
      this.log.warn("hasCommittedCheckpoint: could not resolve recovery floor — preserving guard", {
        branch,
        tip,
        parent,
        error: err instanceof Error ? err.message : String(err),
      });
      return true;
    }
  }

  /** True when `ancestorRef` is an ancestor of (or equal to) `descendantRef` — i.e.
   *  `descendantRef` strictly descends from it (PRD #218 M2). Exit 0 = ancestor, 1 =
   *  not; any other failure (a missing ref) answers false, so a broken candidate never
   *  wins the "strictly descends" branch. */
  private async isAncestor(barePath: string, ancestorRef: string, descendantRef: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["merge-base", "--is-ancestor", ancestorRef, descendantRef])) === 0;
  }

  /** issue #781 — true when `ref` shares any history with the default branch, i.e.
   *  plain `git merge-base <ref> <default>` prints a commit (exit 0). Distinct from
   *  isAncestor (merge-base --is-ancestor), which returns non-zero for BOTH a disjoint
   *  AND a merely-diverged base — so it cannot be reused here without false-rejecting a
   *  legitimately far-ahead resume/first-park base. Only plain merge-base discriminates
   *  disjoint (empty, exit 1) from diverged (a common ancestor exists, exit 0). */
  private async sharesHistory(barePath: string, ref: string, defaultRef: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["merge-base", ref, defaultRef])) === 0;
  }

  /** PRD #759 M2 — is the commit `sha` a `wip(park):` marker (WIP_PARK_COMMIT_PREFIX)?
   *  Reads the commit SUBJECT from the worker-owned bare with a read-only object read
   *  (`log -1 --format=%s`, worker-uid — no working-tree touch, so no filter-driver
   *  fire, safe). Best-effort: any failure (a missing/broken commit) answers false, so a
   *  bad read never routes the reseed onto the reset-soft / cherry-pick recovery path. */
  private async isWipParkMarker(barePath: string, sha: string): Promise<boolean> {
    const subject = await this.tryGitStdout(barePath, ["log", "-1", "--format=%s", `${sha}^{commit}`]);
    return subject.startsWith(WIP_PARK_COMMIT_PREFIX);
  }

  /** PRD #1062 M2 (#1036) — is the commit `sha` a `ckpt(overlay):` transport wrapper
   *  (OVERLAY_COMMIT_PREFIX)? Mirrors isWipParkMarker: a read-only worker-uid subject read,
   *  best-effort (any failure ⇒ false, so a bad read never routes adoption onto the overlay
   *  peel). Adoption peels an overlay by DISCARDING its swapped `.github/workflows` tree and
   *  re-pointing the base to its LAST parent (the real tip) — see runnerCloneForBranch. */
  private async isOverlayMarker(barePath: string, sha: string): Promise<boolean> {
    const subject = await this.tryGitStdout(barePath, ["log", "-1", "--format=%s", `${sha}^{commit}`]);
    return subject.startsWith(OVERLAY_COMMIT_PREFIX);
  }

  // --- git subprocess plumbing -------------------------------------------------

  private async runGit(cwd: string | undefined, args: string[], pat?: string, scope?: string, username?: string): Promise<string> {
    const env = gitEnv(pat, scope, username);
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

  /**
   * PRD #1062 M2 (#1036) — a worker-uid git that carries EXTRA environment on top of the base
   * `gitEnv()` (no PAT): `GIT_INDEX_FILE` for a temp-index synthesis, `GIT_WORK_TREE` for the
   * one op (`read-tree --prefix`) that refuses in a bare repo, and the deterministic
   * `GIT_AUTHOR_*`/`GIT_COMMITTER_*`/`*_DATE` identity for the overlay `commit-tree`. The extra
   * pairs are merged AFTER `gitEnv()` so the security config pins (safe.directory / hooksPath /
   * code-exec-key pins) are never displaced. Same 64 MiB cap + timeout as `runGit`.
   */
  private async runGitWithEnv(
    cwd: string | undefined,
    args: string[],
    extraEnv: NodeJS.ProcessEnv,
  ): Promise<string> {
    const env = { ...gitEnv(), ...extraEnv };
    this.log.debug("git (env)", { cwd, args });
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

  /**
   * Spawn a git subprocess and STREAM its stdout (PRD #122 M8), for an output that can
   * exceed runGit's 64 MiB `maxBuffer` cap — a checkpoint packfile. Plain `spawn` as the
   * WORKER uid on the credential-free base gitEnv: the worker owns the bare it reads, so
   * there is no runner-uid switch and no PAT. Writes the optional `stdin` and ends it, then
   * returns the child and its stdout so the caller can pipe/drain it.
   *
   * On a nonzero exit (or a spawn error) the stdout stream is DESTROYED with an Error, so a
   * consumer streaming it (the publish upload) sees the failure and the caller's best-effort
   * `.catch` fires rather than a truncated pack landing silently.
   */
  private spawnGit(
    cwd: string,
    args: string[],
    stdin?: string,
  ): { child: ChildProcess; stdout: Readable } {
    const env = gitEnv();
    this.log.debug("git (spawn)", { cwd, args });
    const child = spawn("git", withDir(cwd, args), { env });
    const stderrChunks: Buffer[] = [];
    child.stderr?.on("data", (c: Buffer) => stderrChunks.push(c));
    child.on("error", (err) => child.stdout?.destroy(err));
    child.on("close", (code) => {
      if (code !== 0) {
        const detail = Buffer.concat(stderrChunks).toString().trim();
        child.stdout?.destroy(
          new Error(`git ${args.join(" ")} exited ${code ?? "signal"}${detail ? `: ${detail}` : ""}`),
        );
      }
    });
    if (child.stdin) child.stdin.end(stdin ?? "");
    return { child, stdout: child.stdout as Readable };
  }

  /**
   * Run a git op as the RUNNER uid (PRD #51 M4) — the runner-clone seed + checkout,
   * which must be runner-owned. NEVER carries a PAT (a local, non-credentialed op), and
   * runs on gitEnv's config pins (safe.directory / hooksPath / M0 code-exec-key pins)
   * but with the RUNNER PATH + the runner's private TMPDIR so `git` resolves and its
   * scratch lands on the runner's 0700 tmp (not the worker's, which the runner cannot
   * write). Single-uid (#58): `runnerCommand` is a passthrough, so this is a plain git.
   */
  private async runGitAsRunner(cwd: string | undefined, args: string[]): Promise<string> {
    const base = gitEnv();
    const env: NodeJS.ProcessEnv = { ...base, PATH: runnerPath() };
    const tmp = runnerTmpdir();
    if (tmp) env.TMPDIR = tmp;
    const wrapped = runnerCommand("git", withDir(cwd, args));
    this.log.debug("git (runner uid)", { cwd, args });
    try {
      const { stdout } = await execFileAsync(wrapped.command, wrapped.args, {
        env,
        timeout: GIT_TIMEOUT_MS,
        maxBuffer: 64 * 1024 * 1024,
      });
      return stdout;
    } catch (err) {
      // Preserve git's numeric exit code on the wrapped error so a caller can discriminate
      // an expected non-zero status (e.g. `config --unset-all` exit 5 = key absent) from a
      // real failure — the same `.code` idiom `tryGit` reads off the raw execFile error.
      const failure = new Error(`git ${args.join(" ")} failed: ${gitErrorMessage(err)}`);
      const code = (err as { code?: unknown }).code;
      if (typeof code === "number") (failure as { code?: number }).code = code;
      throw failure;
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

  /** issue #909 — read the tracking-ref owner stamp. Prefer the #887 subsection form; fall back
   *  to the pre-#887 flattened form for a bare stamped under old code during the rollout window.
   *  The fallback is COLLISION-AWARE: the flat key is not branch-injective, so it is consulted
   *  only when no OTHER live tracking ref flattens to the same key. The decisive second guard is
   *  the caller's runId-equality test (a foreign branch's stamp carries a different, globally
   *  unique runId). Returns "" when neither form is present. Best-effort throughout. */
  private async readTrackingOwner(barePath: string, branch: string): Promise<string> {
    const current = await this.tryGitStdout(barePath, ["config", "--get", runnerTrackingOwnerKey(branch)]);
    if (current) return current;
    const legacy = await this.tryGitStdout(barePath, ["config", "--get", legacyFlatTrackingOwnerKey(branch)]);
    if (!legacy) return "";
    if (await this.flatOwnerKeyAmbiguous(barePath, branch)) return "";
    return legacy;
  }

  /** issue #909 — true when a DISTINCT live tracking ref (refs/uzi-runner/<branch>) flattens to
   *  the same pre-#887 flat owner key as `branch`, making a legacy flat stamp unattributable.
   *  Enumerates the tracking namespace; the ref suffix is the branch. Best-effort. */
  private async flatOwnerKeyAmbiguous(barePath: string, branch: string): Promise<boolean> {
    const token = branch.replace(/[^A-Za-z0-9_-]/g, "-");
    const out = await this.tryGitStdout(barePath, ["for-each-ref", "--format=%(refname)", RUNNER_TRACKING_PREFIX]);
    if (!out) return false;
    for (const ref of out.split("\n")) {
      if (!ref.startsWith(RUNNER_TRACKING_PREFIX)) continue;
      const other = ref.slice(RUNNER_TRACKING_PREFIX.length);
      if (other === branch) continue;
      if (other.replace(/[^A-Za-z0-9_-]/g, "-") === token) return true;
    }
    return false;
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

/** The git author identity planted on every runner clone (issue #234) and used by the
 *  M2 stub executor's own commit, kept in one place so the two paths cannot drift. */
export const AGENT_GIT_IDENTITY = { name: "uzi-agent", email: "uzi-agent@uzi.local" } as const;

/**
 * PRD #759 M1 — subject prefix of the throwaway "work-in-progress" commit that
 * `commitWipMarker` plants on the park path so uncommitted work survives the reseed.
 * Kept in one place because the recognition side reads it too: M2 detects this prefix
 * on the adopted tip to `git reset --soft <parent>` the content back to uncommitted (so
 * the marker never enters the history the agent builds on, never reaches finalize, and
 * never lands in the MR), and M5 uses it to distinguish a recovered WIP snapshot from a
 * recovered committed milestone. Do NOT inline the literal string anywhere else.
 *
 * @public — the recognition side (M2 reset-at-adopt, M5 feed event) lands in later
 * milestones, so there is no cross-file static consumer YET; exported here in M1 so
 * those milestones import the one definition rather than re-inlining the literal
 * (issue #597 convention for a deliberately-exported symbol knip cannot yet see a use
 * for, mirroring protocol.ts's @public wire DTOs).
 */
export const WIP_PARK_COMMIT_PREFIX = "wip(park):" as const;

/**
 * PRD #1062 M2 (#1036) — the commit-subject prefix that marks a `.github/workflows` overlay
 * transport wrapper on `refs/uzi-checkpoints/<branch>`. `checkpointPack` prefixes the synthetic
 * wrapper's subject with it; adoption (`runnerCloneForBranch` via `isOverlayMarker`) recognises
 * it and peels the wrapper (discard its swapped `.github` tree, re-point to its last parent =
 * realTip) BEFORE the wip-park soft-reset. Exported so the agent Contract-B tests reference the
 * one definition rather than re-inlining the literal (knip's zero-unused-export gate).
 */
export const OVERLAY_COMMIT_PREFIX = "ckpt(overlay):" as const;

/**
 * git subprocess env. safe.directory=* trusts the daemon-managed dirs
 * (ownership check adds nothing here and breaks when the container UID differs
 * from the volume owner); GIT_TERMINAL_PROMPT=0 turns auth failures into clear
 * errors instead of a hang.
 *
 * When a PAT is supplied it is injected as an http.extraHeader via env-scoped
 * config (GIT_CONFIG_KEY/VALUE), NOT via `git -c`. This is deliberate and
 * load-bearing: `git -c value` lands on git's argv, where the PAT is readable in
 * the container's process table (`ps`, /proc/<pid>/cmdline) during every network
 * op — and in M3 an agent subprocess may be alive during the worker's push. The
 * env path keeps it off argv (env is 0600 per /proc/<pid>/environ), off on-disk
 * config, and out of logs (runGit logs args only). Exported for the secret-flow
 * test. Supported since git 2.31.
 *
 * The header is HTTP **Basic** auth — `Authorization: Basic base64(user:pat)` —
 * NOT `PRIVATE-TOKEN`. GitLab honors PRIVATE-TOKEN only on its REST API; git-over-
 * HTTPS (clone/fetch/push) speaks HTTP Basic, so a PRIVATE-TOKEN header carries no
 * credential and git falls back to a (disabled) terminal prompt and fails. The PAT
 * is the Basic *password*; the username is the bot login when known, else the
 * conventional `oauth2` (GitLab accepts any non-empty username with a PAT).
 *
 * `httpScope` (M4 audit item 9) host-scopes the header so the credential is only
 * sent to the repo's own host: `http.<scope>.extraHeader` where <scope> is e.g.
 * `https://gitlab.example.com/`. Without scoping (`http.extraHeader`), a cross-
 * host redirect would replay it to the redirect target. `followRedirects` is also
 * pinned off so no redirect can carry the credential elsewhere. A hostless URL
 * (local fixture path / scp form) has no scope, so the header falls back to
 * unscoped — harmless because local/file transport ignores http.* config entirely.
 */
export function gitEnv(pat?: string, httpScope?: string, username?: string): NodeJS.ProcessEnv {
  // REPLACEMENT env (M10 audit), NOT a process.env spread. A git subprocess can spawn
  // agent-controlled code (a hook at the default path) as the worker uid, outside the
  // SDK hook system — and gitEnv also injects the forge PAT (GIT_CONFIG_VALUE_n). So
  // the join token (UZI_WORKER_TOKEN[_FILE]) + API URL must be ABSENT BY CONSTRUCTION,
  // or that hook could read them (join token → claim → PAT + Anthropic token). Only
  // what git + git-over-HTTPS demonstrably need is carried, all non-secret:
  const env: NodeJS.ProcessEnv = {
    PATH: process.env.PATH,
    HOME: process.env.HOME,
    GIT_TERMINAL_PROMPT: "0",
    // Issue #284: pin git's locale to C so push stderr is language-stable for the
    // transient-vs-permanent classifier (forge-retry.ts) — otherwise a localized
    // "Connection reset"/"[rejected]" would slip past the pattern match.
    LANG: "C",
    LC_ALL: "C",
    // Neutralize /etc/gitconfig: a system config source is another place a code-exec
    // key could be planted, and it is outside the inline-pin override guarantee for
    // any key we don't pin. The worker needs nothing from it (PRD #51 M0).
    GIT_CONFIG_NOSYSTEM: "1",
  };
  // PRD #51 M3 / 5-bis: keep git's scratch (packs, lockfiles) on the worker's private
  // 0700 TMPDIR (set by the entrypoint) rather than a shared sticky /tmp. Carry it only
  // if the image set it; git falls back to /tmp otherwise (a host/test without the
  // entrypoint). The M4 runner-spawned git ops get the RUNNER's TMPDIR via the spawn.
  if (process.env.TMPDIR) env.TMPDIR = process.env.TMPDIR;
  // The "global" config PATH. In the e2e overlay this points at an insteadOf-rewrite
  // file (a config-file path, not a secret) — pass it through untouched. In production
  // it is unset, and we must NOT let git fall back to $HOME/.gitconfig or
  // $XDG_CONFIG_HOME/git/config (either could carry a planted code-exec key outside the
  // pin set), so default it to /dev/null — an empty config that replaces both global
  // lookups. The inline GIT_CONFIG pairs below still OVERRIDE whatever a passed-through
  // file contains (higher precedence than any config file). (PRD #51 M0.)
  env.GIT_CONFIG_GLOBAL = process.env.GIT_CONFIG_GLOBAL || "/dev/null";
  // TLS trust for git-over-HTTPS pushes to the real forge. Carry only if the image set
  // them; never invent, never carry a secret.
  for (const k of ["NIX_SSL_CERT_FILE", "SSL_CERT_FILE", "GIT_SSL_CAINFO"] as const) {
    if (process.env[k]) env[k] = process.env[k];
  }

  // core.hooksPath → the empty dir on EVERY invocation: structurally neutralizes a
  // planted hook regardless of what the agent wrote. safe.directory=* as before.
  // The code-exec key pins (fsmonitor/diff.external/pager/sshCommand) are added
  // UNCONDITIONALLY — the no-PAT paths (changedFiles' `git diff`, `worktree add`)
  // must be covered too, so they cannot sit inside the `if (pat)` block below.
  const pairs: Array<[string, string]> = [
    ["safe.directory", "*"],
    ["core.hooksPath", EMPTY_GIT_HOOKS_DIR],
    // Issue #134. Every object-WRITING git (fetch/commit/push) ends by spawning a detached
    // `git maintenance run --auto --detach` that outlives the process we awaited and keeps
    // writing inside `.git` — so an `fs.rm` of that tree can hit ENOTEMPTY (`force: true`
    // suppresses ENOENT, not ENOTEMPTY). Pinned HERE, inline, rather than only written into
    // each repo's config: these pins are highest precedence, they apply warm or cold, they
    // need no ordering care, and a planted config file cannot override them. The repo-local
    // writes are kept as well — the AGENT's own git (SDK Bash tool) does not go through
    // gitEnv, and that git is the one doing the commits.
    //
    // BOTH keys, and neither subsumes the other across the version range: on git 2.54 (the
    // shipped worker image) `prepare_auto_maintenance` reads ONLY `maintenance.auto`, so
    // `gc.auto=0` alone leaves the spawn intact; on 2.55 it gained a `gc.auto` fallback, so
    // `gc.auto=0` alone suffices there — but an explicit `maintenance.auto=true` re-enables
    // the spawn regardless of `gc.auto`. Setting both is correct on either.
    ["maintenance.auto", "false"],
    ["gc.auto", "0"],
    ...GIT_CODE_EXEC_KEY_PINS.map(([k, v]) => [k, v] as [string, string]),
  ];
  if (pat) {
    // HTTP Basic (base64(user:pat)) — git-over-HTTPS auth, unlike GitLab's
    // REST-only PRIVATE-TOKEN. Scope the header + pin followRedirects to the repo
    // host so neither the credential nor a redirect can reach another host.
    const headerKey = httpScope ? `http.${httpScope}.extraHeader` : "http.extraHeader";
    const redirKey = httpScope ? `http.${httpScope}.followRedirects` : "http.followRedirects";
    pairs.push([headerKey, `Authorization: Basic ${gitBasicCredential(pat, username)}`]);
    pairs.push([redirKey, "false"]);
  }
  // count starts at 0: the replacement env carries no inherited GIT_CONFIG_COUNT.
  let count = 0;
  for (const [k, v] of pairs) {
    env[`GIT_CONFIG_KEY_${count}`] = k;
    env[`GIT_CONFIG_VALUE_${count}`] = v;
    count++;
  }
  env.GIT_CONFIG_COUNT = String(count);
  return env;
}

/**
 * The base64 credential for git-over-HTTPS Basic auth: base64(`user:pat`), with
 * `user` the bot login when known else the conventional `oauth2`. Exported so the
 * runner can register this exact blob with the redactor / secret registry —
 * defense in depth: it only ever lives in GIT_CONFIG_VALUE_n (never argv/logs),
 * but if future code ever logged the git env, the scrubber would catch it. Keep
 * the fallback in lockstep with gitEnv's, so the two never drift.
 */
export function gitBasicCredential(pat: string, username?: string): string {
  const user = username?.trim() || "oauth2";
  return Buffer.from(`${user}:${pat}`).toString("base64");
}

/**
 * The `http.<scope>.*` prefix that host-scopes credential config to a repo's own
 * host, e.g. `https://gitlab.example.com/` for any https URL on that host.
 * Returns undefined for a hostless URL (local path / scp-style), where http.*
 * config does not apply.
 */
/**
 * PRD #456 M1 — true when a push error is GitHub's workflow-scope rejection: the bot's
 * repo-only PAT is refused when the pushed tip's `.github/workflows/**` tree differs from
 * the default branch. Matches the stable phrase `workflow scope` (case-insensitive), which
 * appears in both `without workflow scope` and the full `refusing to allow a Personal Access
 * Token to create or update workflow .github/workflows/<f> without workflow scope`. Used to
 * decide whether a merge-aligned push that STILL failed warrants the rebase fallback (a
 * genuine workflow-scope reject) versus an unrelated push failure (which rethrows).
 */
export function isWorkflowScopeRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return msg.includes("workflow scope");
}

/**
 * PRD #974 M2 — true when a push error is GitHub Push Protection's secret rejection: the
 * remote refuses the push because a commit in it carries a secret Push Protection detected.
 * GitHub's rejection reads `remote: … GH013 … Push cannot contain secrets` (with a per-secret
 * detail block), so this matches any of the stable tokens `gh013`, `push cannot contain
 * secrets`, `push protection`, or `secret detected` (case-insensitive). Mirrors
 * isWorkflowScopeRejection's shape.
 *
 * This is the remote BACKSTOP for the case the pre-push gitleaks scan (default ruleset) misses
 * a secret GitHub's own scanner catches — GitHub's pattern set is broader than gitleaks' and
 * the two are not identical, so a clean pre-push scan does not guarantee GitHub accepts the
 * push. Routing this rejection to the same typed `push_secret_blocked` fail_origin preserves
 * the diff and gives an actionable failure instead of GitHub's opaque remote reject discarding
 * the committed work.
 */
export function isPushProtectionRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return (
    msg.includes("gh013") ||
    msg.includes("push cannot contain secrets") ||
    msg.includes("push protection") ||
    msg.includes("secret detected")
  );
}

/**
 * PRD #456 NB2 — true when a push error is a non-fast-forward rejection: the remote
 * refused a non-forced push because the pushed tip is not a descendant of the branch's
 * already-published tip. This is the resumed / rewritten-history case: when the finalize
 * base-align rebase fallback rewinds to the original agent tip and replays the commits,
 * it rewrites SHAs that were already published at origin (a resume — including a resumed
 * `self_improve` cycle reattaching to its own fresh-per-cycle branch), so the subsequent
 * non-forced push cannot fast-forward. Force-push is
 * denied by the guardrails by design, so the correct outcome is the typed
 * base-align-conflict preserve path (preserved_patch + `finalize_base_align_conflict`),
 * not a push retry — routing here keeps it off the generic catch (raw error, defaulted
 * `fail_origin`, no preserved diff). Matches the stable git phrases `non-fast-forward` and
 * `fetch first` (case-insensitive), the same phrases forge-retry treats as permanent
 * push-failure patterns; deliberately does NOT match the bare `[rejected]` token, which is
 * too broad.
 */
export function isNonFastForwardRejection(err: unknown): boolean {
  const msg = (err instanceof Error ? err.message : String(err)).toLowerCase();
  return msg.includes("non-fast-forward") || msg.includes("fetch first");
}

export function httpScopeForUrl(rawUrl: string): string | undefined {
  try {
    const u = new URL(rawUrl);
    if (u.protocol === "https:" || u.protocol === "http:") {
      return `${u.protocol}//${u.host}/`;
    }
  } catch {
    // Not a URL (scp-style or local path) — no http scope.
  }
  return undefined;
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
 * segments, so two names collide only for the same repo on the same host.
 * Examples:
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
