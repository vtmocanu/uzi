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
 * The setpriv argv prefix that reuids to `runner` and drops all caps. Terminated by
 * `--` so the target command + args follow literally (no shell re-parse). Exported for
 * the boundary tests.
 */
export function setprivRunnerArgs(): string[] {
  return [
    "--reuid", RUNNER_USER,
    "--regid", RUNNER_USER,
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

/** Wrap a command so it runs as `runner` under the split, or unchanged single-uid. */
export function runnerCommand(command: string, args: readonly string[]): { command: string; args: string[] } {
  if (!uidSplitActive()) return { command, args: [...args] };
  return { command: SETPRIV, args: [...setprivRunnerArgs(), command, ...args] };
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
