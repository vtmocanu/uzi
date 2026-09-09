// PRD #51 M4 — spawn and signal the untrusted execution surfaces under the `runner`
// uid. This is the single place the worker→runner uid boundary is crossed at runtime.
//
// The credential-holding worker (uid `worker`, holding only ambient CAP_SETUID/SETGID
// after the M2 entrypoint drop) launches every untrusted surface — the SDK agent CLI,
// the self-improve check runner + `npm ci`, the provision/nix hooks, and the runner
// clone's seed `git clone`/`checkout` — as the distinct, cap-less uid `runner` via a
// `setpriv` wrapper. The wrapper reuids/regids to `runner`, picks up its supplementary
// groups (`--init-groups`), and **clears the inheritable + ambient capability sets**
// (`--inh-caps -all --ambient-caps -all`). The cap-clear is load-bearing, not cosmetic:
// a plain reuid from a non-root uid to another non-root uid does NOT drop the worker's
// ambient CAP_SETUID (verified on the image), so without the clear the runner child
// would inherit CAP_SETUID and could setuid back to the worker or 0 — defeating the
// split. With the clear the runner child ends CapEff=CapPrm=CapAmb=0 (the only residue,
// CapBnd, is inert: dropping it needs CAP_SETPCAP which the worker deliberately does not
// hold, and the image ships no file-capability/setuid binary to raise from it, plus
// `no-new-privileges` blocks any such raise on execve — getcap evidence recorded).
//
// SIGNALLING a runner process needs the SAME wrapper: the worker (different uid, no
// CAP_KILL) cannot `kill(2)` a runner process directly (EPERM), so it forks a
// `setpriv`-to-runner `kill` that signals the group as the same uid (verified).
//
// PRD #58 single-uid (non-root) start: when the entrypoint did NOT establish the split
// (a k8s runAsUser:10001 start — no root window, no `runner` uid to drop to), there is
// only one uid, so these primitives run the command / signal directly. The entrypoint
// exports `UZI_UID_SPLIT=1` ONLY on the A1 root-started path; its absence means
// single-uid, and the #51 containment does not apply there (the #58 accepted posture).

import { spawn, spawnSync, type ChildProcess, type SpawnOptions } from "node:child_process";

/** The OS user the untrusted execution surfaces run as under the split. */
const RUNNER_USER = "runner";
/** The absolute, image-baked setpriv (util-linux) — the same binary the entrypoint
 *  drop uses. Absolute so the worker never resolves it from a runner-writable PATH. */
const SETPRIV = "/bin/setpriv";

// PRD #1171 (M3) — the Codex production adapter needs THREE OS identities where the
// #51 split previously needed one. The numbers are the image-baked uids; see the
// worker/runner/runner-cmd accounts in agent/templates/base/Dockerfile (`addgroup`/
// `adduser` for worker 10001, runner 10002, runner-cmd 10003). These consts are
// additive; the existing `runner` (provider) identity and its argv are preserved
// byte-for-byte below.

/** uid 10002 — `runner`, the EXISTING cap-less identity. Provider roots (the app-server
 *  processes that hold the selected Codex credential) and every pre-#1171 untrusted
 *  surface run as this uid. Image account: the `runner` account in
 *  agent/templates/base/Dockerfile. */
export const RUNNER_UID = 10002;
/** uid 10003 — `runner-cmd`, a NEW distinct cap-less identity for command roots (the
 *  model-authorized shell + fileop effect surface). Credential-free, primary group
 *  `runner-cmd` (gid 10003) and a supplementary member of group `runner`, so its
 *  worktree writes are group-`runner` and group-writable under the enforced
 *  setgid+umask discipline. Distinct from RUNNER_UID so a command root
 *  cannot read a provider root's auth/session state at the OS level (plan §2.8). The
 *  `runner-cmd` account now EXISTS in the images — group gid 10003 plus a `runner-cmd`
 *  passwd entry that is a supplementary member of group `runner` — numbered above the
 *  existing pair; see the worker/runner/runner-cmd accounts in
 *  agent/templates/base/Dockerfile. */
export const COMMAND_UID = 10003;
/** uid 10001 — `worker`, the PAT-holding worker process itself. Credentialed
 *  boundary-action roots ARE this process, so becoming `worker` needs no setpriv (see
 *  {@link workerBoundaryCommand}). Image account: the `worker` account in
 *  agent/templates/base/Dockerfile. */
export const WORKER_UID = 10001;

/** True when the entrypoint established the worker/runner uid split (A1 root start). */
export function uidSplitActive(env: NodeJS.ProcessEnv = process.env): boolean {
  return env.UZI_UID_SPLIT === "1";
}

/**
 * The PATH the runner env builders (buildSdkEnv/buildProvisionEnv/buildCheckEnv) put
 * on a runner child. Under the split the entrypoint exports `UZI_RUNNER_PATH` = the
 * full image PATH INCLUDING `/nix` (which the runner needs to realize devbox/nix
 * packages), while the WORKER's own PATH is stripped to root-owned image dirs only (no
 * `/nix`) — so a runner-writable `/nix` can never plant a binary the PAT-holding worker
 * resolves (M2-audit MEDIUM).
 *
 * Single-uid (#58, hosted k8s): the entrypoint exports the SAME untouched image PATH on
 * that branch too (issue #120). That export is NOT redundant with the `env.PATH` fallback
 * below: the CMD is `npm run start`, so by the time the worker reads its own PATH npm's
 * run-script has PREPENDED `/app/node_modules/.bin` (+ `/node_modules/.bin`, +
 * `@npmcli/run-script/.../node-gyp-bin`) to it — and the real `agent-browser` npm CLI
 * there SHADOWED the `/usr/local/bin` shim that injects `--no-sandbox`, so browser
 * launches hit the setuid sandbox the PRD #51 hardening makes impossible. The fallback is
 * kept for a non-entrypoint start (unit tests, a bare `npm start`), not as the container
 * path. Do not "simplify" the second export away. */
export function runnerPath(env: NodeJS.ProcessEnv = process.env): string | undefined {
  return env.UZI_RUNNER_PATH || env.PATH;
}

/** The runner's private 0700 TMPDIR under the split (entrypoint `UZI_RUNNER_TMPDIR`),
 *  else the ambient TMPDIR (single-uid). 5-bis per-uid scratch isolation. */
export function runnerTmpdir(env: NodeJS.ProcessEnv = process.env): string | undefined {
  return env.UZI_RUNNER_TMPDIR || env.TMPDIR;
}

/**
 * The setpriv argv prefix that reuids/regids to `uid` and drops all caps. Terminated
 * by `--` so the target command + args follow literally (no shell re-parse). This is
 * the ONE source of truth for the cap-clear flag discipline; {@link setprivRunnerArgs}
 * and {@link commandRootCommand} both route through it so the two identities can never
 * drift in how they drop capabilities.
 *
 * The reuid/regid TOKEN is the image account NAME for RUNNER_UID and the numeric uid
 * otherwise. setpriv treats a name and its numeric uid identically, but emitting the
 * `runner` name for RUNNER_UID keeps the argv the established #51 Claude/launcher paths
 * produce BYTE-FOR-BYTE identical to the pre-#1171 literal (a drift guard pins it).
 * COMMAND_UID is emitted numerically (setpriv resolves a name or its numeric uid the
 * same way). Its `runner-cmd` account now EXISTS in the images — group gid 10003 plus a
 * `runner-cmd` passwd entry that is a supplementary member of group `runner` — so
 * `--regid 10003 --init-groups` resolves the account and grants the `runner` group.
 */
export function setprivArgsForUid(uid: number): string[] {
  const identity = uid === RUNNER_UID ? RUNNER_USER : String(uid);
  return [
    "--reuid", identity,
    "--regid", identity,
    "--init-groups",
    // NOTE (audit M4): `--bounding-set -all` is effectively a NO-OP here — the worker
    // lacks CAP_SETPCAP, so it cannot shrink the child's bounding set (it stays 0xc0).
    // The containment does NOT rely on it: it rests on the `--inh-caps -all` +
    // `--ambient-caps -all` clears (which zero the runner's Eff/Prm/Amb — verified: a
    // plain reuid leaks CAP_SETUID and the runner can climb to uid 0), `no-new-privileges`
    // (blocks any fcap/suid raise on execve), and the image shipping NO file-capability /
    // setuid binary (getcap-confirmed). It is kept for intent/defense-in-depth. If a
    // future node:22-alpine base bump changes util-linux `setpriv` to ABORT on an
    // un-droppable bounding cap (rather than best-effort), this flag would break the spawn
    // functionally — make a base bump a conscious setpriv re-check.
    "--bounding-set", "-all",
    "--inh-caps", "-all",
    "--ambient-caps", "-all",
    "--",
  ];
}

/**
 * The setpriv argv prefix that reuids to `runner` and drops all caps. Preserved as a
 * named export (the #51 boundary tests and the M3a launcher import it); reimplemented
 * as a thin call to {@link setprivArgsForUid} so its output stays byte-for-byte what it
 * produced before while the flag discipline has a single owner.
 */
export function setprivRunnerArgs(): string[] {
  return setprivArgsForUid(RUNNER_UID);
}

/** Wrap a command so it runs as `runner` under the split, or unchanged single-uid. */
export function runnerCommand(command: string, args: readonly string[]): { command: string; args: string[] } {
  if (!uidSplitActive()) return { command, args: [...args] };
  return { command: SETPRIV, args: [...setprivRunnerArgs(), command, ...args] };
}

/**
 * Wrap a command so it runs as the command-root uid `runner-cmd` (COMMAND_UID) under
 * the split, or unchanged single-uid. This is the credential-free shell + fileop effect
 * surface the Codex adapter admits while a credentialed provider root is live; running
 * it as a DISTINCT uid from the provider `runner` is what makes "same uid + mode 0700 is
 * not evidence" (plan §2.8) into a real OS boundary. Mirrors {@link runnerCommand}'s
 * shape exactly, differing only in the target uid.
 */
export function commandRootCommand(command: string, args: readonly string[]): { command: string; args: string[] } {
  if (!uidSplitActive()) return { command, args: [...args] };
  return { command: SETPRIV, args: [...setprivArgsForUid(COMMAND_UID), command, ...args] };
}

/**
 * The credentialed boundary-action identity: the PAT-holding `worker` (WORKER_UID).
 * The worker process already is uid 10001, but on the split profile it retains
 * controller-only SETUID/SETGID capabilities. A boundary subprocess needs its PAT
 * environment, not those caps, so it re-enters uid/gid 10001 through setpriv,
 * clears inheritable/ambient caps there, and asks the trusted supervisor to capset
 * the retained effective/permitted pair to zero before profile validation. Single-uid
 * remains verbatim. This seam is symmetric with
 * {@link runnerCommand}/{@link commandRootCommand}, so the Codex safety lane never
 * reaches for a bare spawn. Reaping every command root BEFORE a credentialed boundary
 * action runs is enforced by the Codex safety lane, not here.
 */
export function workerBoundaryCommand(command: string, args: readonly string[]): { command: string; args: string[] } {
  if (!uidSplitActive()) return { command, args: [...args] };
  // The root-start entrypoint deliberately leaves SETUID/SETGID effective on the
  // worker controller so it can launch the two untrusted identities. A worker-PAT
  // durability child needs the worker uid and credential env, not those caps. Clear
  // them before the supervisor posture check, while retaining uid/gid 10001.
  return { command: SETPRIV, args: [...setprivArgsForUid(WORKER_UID), command, ...args] };
}

/**
 * Spawn `command` under the runner uid (split) or directly (single-uid). The setpriv
 * wrapper, when present, is the process group leader after it execs the target, so a
 * `detached` spawn's pid is still the group id `killRunnerGroup` targets. The caller
 * supplies the (scrubbed, runner-PATH/TMPDIR) env explicitly — setpriv passes it
 * through unchanged (no --reset-env), so the run's Anthropic OAuth still reaches the
 * agent while the worker credentials stay absent by construction.
 */
export function runnerSpawn(
  command: string,
  args: readonly string[],
  opts: { cwd?: string; env?: NodeJS.ProcessEnv; signal?: AbortSignal; detached?: boolean; stdio?: SpawnOptions["stdio"] },
): ChildProcess {
  const wrapped = runnerCommand(command, args);
  return spawn(wrapped.command, wrapped.args, {
    cwd: opts.cwd,
    env: opts.env,
    signal: opts.signal,
    detached: opts.detached ?? false,
    stdio: opts.stdio ?? ["pipe", "pipe", "pipe"],
  });
}

/**
 * SIGKILL the process GROUP led by `pid` (the runner subprocess tree). Under the split
 * the worker cannot signal a runner process directly (EPERM), so it forks a
 * setpriv-to-runner `kill` that signals the group as the same uid. Synchronous
 * (spawnSync) so the B1 pre-push reap completes before the worker's PAT touches a git
 * child — a surviving agent during the credentialed window is exactly the threat. Safe
 * with an undefined/dead pid. @returns true if a kill was dispatched.
 */
export function killRunnerGroup(pid: number | undefined): boolean {
  if (pid === undefined || pid <= 0) return false;
  if (!uidSplitActive()) {
    // Single-uid: signal the group directly (same uid, permitted).
    try {
      process.kill(-pid, "SIGKILL");
      return true;
    } catch {
      try {
        process.kill(pid, "SIGKILL");
        return true;
      } catch {
        return false;
      }
    }
  }
  // Split: reap as `runner` via setpriv. `kill -KILL -<pid>` targets the process group.
  const r = spawnSync(SETPRIV, [...setprivRunnerArgs(), "kill", "-KILL", `-${pid}`], { stdio: "ignore" });
  if (r.status === 0) return true;
  // Fall back to the single pid (a non-group-leader child) as `runner`.
  const r2 = spawnSync(SETPRIV, [...setprivRunnerArgs(), "kill", "-KILL", `${pid}`], { stdio: "ignore" });
  return r2.status === 0;
}
