import { execFile, spawn, type ChildProcess } from "node:child_process";
import { promisify } from "node:util";
import fs from "node:fs/promises";
import path from "node:path";
import type { Readable } from "node:stream";
import type { Logger } from "./log.js";
import { runnerCommand, runnerPath, runnerTmpdir } from "./runner-uid.js";

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
// write, the same posture as disableAutoMaintenance — no new trust boundary. The key is
// dot/slash-free so the dotted git-config name parses (`git config uzi-trackowner.<x>`).
function runnerTrackingOwnerKey(branch: string): string {
  return `uzi-trackowner.${branch.replace(/[^A-Za-z0-9_-]/g, "-")}`;
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
   *     run on the same issue, the self_improve fixed branch's previous cycles, or a
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
   *  head, but `runnerCloneForBranch` now `update-ref`s `refs/remotes/origin/<default>` to the
   *  fresh default head (`defaultBranchCommit`) after checkout (issue #262), so the ratchet
   *  base is fresh; the clone's local `refs/heads/main` is left untouched and stays frozen.
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
}

/**
 * Warm bare-clone cache (worker-owned) + per-run RUNNER CLONE lifecycle (PRD #51
 * M3, (b) separate-runner-clone; multica's repocache pattern ported to TS).
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

  constructor(dataDir: string, private readonly log: Logger) {
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
        await this.fetch(barePath, pat, scope, username);
      } else {
        this.log.info("repo cache: cloning bare", { url: repoUrl, bare: barePath });
        await this.cloneBare(repoUrl, barePath, pat, scope, username);
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
  async createOrAttachRunnerClone(barePath: string, issueIid: number, runId?: string): Promise<RunnerClone> {
    return this.runnerCloneForBranch(barePath, `agent/issue-${issueIid}`, `issue-${issueIid}`, runId);
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
   * checkpoint is preferred ONLY when it strictly descends that floor. On divergence
   * origin/default WINS — never a silent merge or discard — and `checkpointSetAside` is set
   * so the runner emits a loud notice.
   * The git layer is claim-agnostic: `runId` is a plain string the runner threads from
   * `claim.run_id`; nothing here knows about sessions. `runId` undefined ⇒ never owned ⇒
   * today's behaviour, which keeps the no-runId test call sites compiling.
   *
   * The seed is a LOCAL `clone --shared` from the worker bare: fast (objects are
   * referenced read-only from the bare via the clone's objects/info/alternates — the
   * runner cannot corrupt worker-bare objects through it), and the runner's own new
   * commit objects land in the clone's own objects store. The clone is a TRUSTED-source
   * local clone (worker bare → runner clone), so the local optimization here is safe;
   * the untrusted direction (worker fetching BACK from the runner clone) is the one
   * forced onto the pack transport in fetchAgentBranch (B2 invariant 3).
   */
  async runnerCloneForBranch(barePath: string, branch: string, key: string, runId?: string): Promise<RunnerClone> {
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
      const originExists = await this.refExists(barePath, originRef);
      // The tracking ref is consulted ONLY when its stamp says THIS run wrote it — the
      // run-identity anchor that gates the whole consideration (see the doc comment).
      const trackingExists = await this.refExists(barePath, trackingRef);
      const owner = trackingExists
        ? await this.tryGitStdout(barePath, ["config", "--get", runnerTrackingOwnerKey(branch)])
        : "";
      const ownedHere = trackingExists && runId !== undefined && owner === runId;
      // PRD #122 M8 cross-worker candidate: origin's checkpoint ref, mirrored into the bare
      // by fetch(). Consulted ONLY on the not-ownedHere legs (see the doc comment).
      const checkpointRef = `refs/uzi-checkpoints/${branch}`;
      const checkpointExists = await this.refExists(barePath, checkpointRef);

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
        // default. A mirrored checkpoint (PRD #122 M8) from another worker wins ONLY when
        // it strictly descends the floor; on divergence origin/default wins, LOUDLY.
        const floorRef = originExists ? originRef : await this.defaultBranchRef(barePath);
        const floorFrom: RunnerClone["seededFrom"] = originExists ? "origin" : "default";
        baseRef = floorRef;
        seededFrom = floorFrom;
        if (checkpointExists) {
          // isAncestor (merge-base --is-ancestor) is TRUE at EQUALITY, so an ancestor test
          // alone would seed a checkpoint that EQUALS the floor as "checkpoint" though nothing
          // was recovered. Require a STRICT descendant: reachable from the floor AND a
          // different commit. Compare the resolved SHAs (the isAncestor pass already proved
          // both refs resolve).
          if (await this.isAncestor(barePath, floorRef, checkpointRef)) {
            const checkpointSha = (
              await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${checkpointRef}^{commit}`])
            ).trim();
            const floorSha = (
              await this.tryGitStdout(barePath, ["rev-parse", "--verify", `${floorRef}^{commit}`])
            ).trim();
            if (checkpointSha !== "" && checkpointSha !== floorSha) {
              baseRef = checkpointRef;
              seededFrom = "checkpoint";
            }
            // else EQUAL to the floor: fall through to the floor. Equality is NOT divergence,
            // so checkpointSetAside stays false — nothing was set aside.
          } else {
            // Diverged: the checkpoint is not reachable from the floor. origin/default WINS,
            // loudly (checkpointSetAside → feed notice).
            checkpointSetAside = true;
          }
        }
      }
      const baseSha = (await this.runGit(barePath, ["rev-parse", "--verify", `${baseRef}^{commit}`])).trim();
      // How many commits the seed carries ahead of the default branch. On the origin
      // leg that is prior PUSHED work (issue #105); on the tracking leg it is the
      // interrupted attempt's RECOVERED work (PRD #218 M3). Counted in the BARE, which
      // holds every ref. Best-effort by construction — a repo with no resolvable default
      // branch, or any rev-list failure, yields 0 rather than failing a run over colour.
      const isResumeLeg = seededFrom !== "default";
      const priorCommits = isResumeLeg ? await this.commitsAheadOfDefault(barePath, baseSha) : 0;
      // On the default leg the seed already IS the default tip, so no second lookup. On a
      // resume leg they differ, and the difference is exactly what the lead cannot infer.
      const defaultBranchCommit = isResumeLeg ? await this.defaultBranchSha(barePath) : baseSha;
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
      const defaultBranch = await this.defaultBranchName(barePath);
      if (defaultBranch && defaultBranchCommit) {
        await this.runGitAsRunner(clonePath, ["update-ref", `refs/remotes/origin/${defaultBranch}`, defaultBranchCommit]);
      }
      return { path: clonePath, branch, priorCommits, baseCommit: baseSha, defaultBranchCommit, seededFrom, checkpointSetAside };
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
  ): Promise<{ tipOid: string; pack: Readable } | null> {
    const tipOid = await this.trackingTip(barePath, branch);
    if (!tipOid) return null;
    const originRef = `refs/remotes/origin/${branch}`;
    const excludeRef = (await this.refExists(barePath, originRef))
      ? originRef
      : await this.defaultBranchRef(barePath);
    const trackingRef = runnerTrackingRef(branch);
    // pack-objects reads the wanted/excluded revs on stdin (one ref per line, `^` excludes)
    // and writes the packfile to stdout.
    const { stdout } = this.spawnGit(
      barePath,
      ["pack-objects", "--revs", "--stdout"],
      `${trackingRef}\n^${excludeRef}\n`,
    );
    return { tipOid, pack: stdout };
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

  private async cloneBare(repoUrl: string, dest: string, pat?: string, scope?: string, username?: string): Promise<void> {
    try {
      await this.runGit(undefined, ["clone", "--bare", repoUrl, dest], pat, scope, username);
    } catch (err) {
      await fs.rm(dest, { recursive: true, force: true });
      throw err;
    }
    // Convert the mirror refspec to remote-tracking so future fetches write to
    // refs/remotes/origin/* (multica). Under (b) the bare's refs/heads/* stay the
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
  }

  private async fetch(barePath: string, pat?: string, scope?: string, username?: string): Promise<void> {
    await this.runGit(barePath, ["fetch", "origin"], pat, scope, username);
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

  /** True when `ancestorRef` is an ancestor of (or equal to) `descendantRef` — i.e.
   *  `descendantRef` strictly descends from it (PRD #218 M2). Exit 0 = ancestor, 1 =
   *  not; any other failure (a missing ref) answers false, so a broken candidate never
   *  wins the "strictly descends" branch. */
  private async isAncestor(barePath: string, ancestorRef: string, descendantRef: string): Promise<boolean> {
    return (await this.tryGit(barePath, ["merge-base", "--is-ancestor", ancestorRef, descendantRef])) === 0;
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

/** The git author identity planted on every runner clone (issue #234) and used by the
 *  M2 stub executor's own commit, kept in one place so the two paths cannot drift. */
export const AGENT_GIT_IDENTITY = { name: "uzi-agent", email: "uzi-agent@uzi.local" } as const;

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
