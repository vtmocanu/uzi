// Devbox tool provisioning (PRD #18 M3, Technical Design §2, Decisions 2/3).
//
// Before the SDK starts, the worker installs the run's tier-1 tool packages so
// they land on the agent's PATH. This is security-sensitive: the install runs
// nix build hooks (arbitrary code) in a process the WORKER controls — the same
// process that holds the decrypted forge PAT, the Anthropic token, and can read
// the join-token file. Two invariants make it safe (Decision 3):
//
//   1. The install subprocess runs with a REPLACEMENT env built from an explicit
//      allowlist — never `process.env` spread — so the join-token ENV VAR (and
//      anything else) can't leak into it. The PAT + Anthropic token are held in
//      worker memory (never in process.env), so a replacement env excludes them by
//      construction; this also drops UZI_WORKER_TOKEN[_FILE].
//   2. Only a synthesized packages-only devbox.json is used, written OUTSIDE the
//      clone. A repo's own devbox.json (init_hook/scripts) is never executed here.
//
// CLOSED for the local path (PRD #51 M4): the provision/nix build hooks now run under
// the cap-less `runner` uid (via runnerCommand below), and the join-token FILE at
// /run/secrets/worker_token is 0400 worker-owned, so a build hook can no longer read it
// — the same-uid residual this used to name is gone on the A1 (root-started) path. The
// admin allowlist bounds WHICH packages install ONLY on the tier-1 path (their build hooks
// run in this scrubbed env). CORRECTION (2026-07-25, PRD #123 §6): this line used to claim
// the allowlist bounds them full stop. It does not. Under `repo_devbox_opt_in` the tier-2
// list comes from the CLONED REPO's devbox.json and is filtered by SHAPE ONLY — no
// allowlist, and no `toolprofile.Denied()`, so the 20 credential-bearing CLIs that check
// exists to bar (toolprofile.go:66-80) install unimpeded. The worker cannot do better today:
// ClaimConfig ships it no rule set (workersvc/claim.go:160-183). PRD #123 M1b closes it.
// Stated plainly because this sentence is the recorded mitigation for the residual above.
// On a #58 single-uid (non-root) start there is no split and the hook runs
// as the sole uid (that PRD's accepted posture); the cross-container k8s form is mapped in
// docs/proc-hardening.md.
//
// PATH assumption: nix/devbox tooling needs /sbin on PATH (e.g. Alpine's addgroup
// lives there). The image PATH passed through by buildProvisionEnv includes it
// today; a future PATH trim must keep /sbin or provisioning breaks.
//
// The env we export back INTO the SDK is likewise an explicit allowlist
// (PROVISION_ENV_ALLOWLIST) — never a blind merge of devbox's shellenv, which
// would widen the deliberately-minimal agent env.

import fs from "node:fs/promises";
import path from "node:path";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import type { Logger } from "./log.js";
import { errMessage } from "./util.js";
import { runnerCommand, runnerPath, runnerTmpdir } from "./runner-uid.js";

const execFileAsync = promisify(execFile);

/**
 * The ONLY env vars allowed out of devbox's shellenv into the agent SDK env.
 * PATH carries the provisioned tool bins; the two nix vars keep TLS trust and
 * locale working for tools nix builds. Extend deliberately, per package need —
 * never blanket-merge, and never a var that could carry a worker secret.
 */
export const PROVISION_ENV_ALLOWLIST: ReadonlySet<string> = new Set([
  "PATH",
  "NIX_SSL_CERT_FILE",
  "LOCALE_ARCHIVE",
]);

export interface ProvisionInput {
  /** The resolved tier-1 package list (already allowlist-validated server-side). */
  packages: string[];
  /** A per-run dir OUTSIDE the clone where the synthesized devbox.json lives. */
  runDir: string;
  /** HOME for the install subprocess (nix single-user profile + devbox state);
   *  a dir on the persistent data volume so the nix store warm-starts. */
  homeDir: string;
}

/** Result of running a provisioning command. */
export interface RunResult {
  stdout: string;
  stderr: string;
}

export interface ProvisionDeps {
  log: Logger;
  /** Runs a provisioning command. Injected so unit tests never touch real
   *  devbox/nix (no substituter egress). Default shells out to devbox. */
  run?: (cmd: string, args: string[], opts: { cwd: string; env: NodeJS.ProcessEnv }) => Promise<RunResult>;
  /** Source env to derive the SCRUBBED subprocess env from (default process.env). */
  processEnv?: NodeJS.ProcessEnv;
}

export interface ProvisionResult {
  /** Allowlisted vars to merge into the SDK env (PATH + the nix TLS/locale vars). */
  toolEnv: Record<string, string>;
}

const defaultRun = async (
  cmd: string,
  args: string[],
  opts: { cwd: string; env: NodeJS.ProcessEnv },
): Promise<RunResult> => {
  // PRD #51 M4: nix build hooks are arbitrary untrusted code, so run devbox/nix under
  // the `runner` uid (setpriv wrapper), not the credential-holding worker. Single-uid
  // (#58) runs it directly. The scrubbed provision env (buildProvisionEnv) is passed
  // through unchanged.
  const wrapped = runnerCommand(cmd, args);
  const { stdout, stderr } = await execFileAsync(wrapped.command, wrapped.args, {
    cwd: opts.cwd,
    env: opts.env,
    // Provisioning can be slow on a cold nix store; bounded so a hung fetch fails
    // the run rather than wedging the worker.
    timeout: 10 * 60_000,
    maxBuffer: 8 * 1024 * 1024,
  });
  return { stdout, stderr };
};

/**
 * Build the SCRUBBED replacement env for the provisioning subprocess (Decision 3).
 * A replacement env (not a spread of `source`) is the load-bearing property: the
 * join token (UZI_WORKER_TOKEN / UZI_WORKER_TOKEN_FILE) and any other worker var
 * are absent by construction; the PAT + Anthropic token are never in process.env
 * to begin with. Only what nix/devbox demonstrably needs is added.
 */
export function buildProvisionEnv(source: NodeJS.ProcessEnv, homeDir: string): NodeJS.ProcessEnv {
  const env: NodeJS.ProcessEnv = {
    // So `devbox`, `nix`, and `sh` resolve. The RUNNER PATH (PRD #51 M4): the
    // /nix-bearing image PATH under the split (provisioning runs as `runner`), NOT the
    // worker's stripped PATH. Single-uid (#58): ALSO the image PATH since PRD #120 — the
    // entrypoint pins UZI_RUNNER_PATH on both branches. Not a secret. NOTE this value is
    // what devbox's `$PATH` back-ref expands against in filterShellenv below, so the
    // toolEnv.PATH handed to the agent inherits the image PATH as its tail.
    PATH: runnerPath(source),
    // nix single-user profile + devbox state live under this (data volume) HOME.
    HOME: homeDir,
  };
  // 5-bis: nix/devbox scratch on the runner's private 0700 TMPDIR under the split.
  const tmp = runnerTmpdir(source);
  if (tmp) env.TMPDIR = tmp;
  // Pass through TLS trust only if the base image set it (needed to fetch from
  // substituters over HTTPS). Never invent it; never carry anything else.
  if (source.NIX_SSL_CERT_FILE) env.NIX_SSL_CERT_FILE = source.NIX_SSL_CERT_FILE;
  if (source.SSL_CERT_FILE) env.SSL_CERT_FILE = source.SSL_CERT_FILE;
  return env;
}

/** Strip a single layer of surrounding single/double quotes. */
function unquote(v: string): string {
  const t = v.trim();
  if (t.length >= 2 && ((t[0] === '"' && t.at(-1) === '"') || (t[0] === "'" && t.at(-1) === "'"))) {
    return t.slice(1, -1);
  }
  return t;
}

/**
 * Filter `devbox shellenv` output down to the allowlist. Parses `export KEY=VALUE`
 * lines, keeps only PROVISION_ENV_ALLOWLIST keys, and resolves a `$PATH` /
 * `${PATH}` back-reference against the scrubbed base PATH (devbox prepends its tool
 * bins to the existing PATH). Everything not on the allowlist is dropped.
 */
export function filterShellenv(output: string, basePath: string): Record<string, string> {
  const out: Record<string, string> = {};
  const re = /^\s*export\s+([A-Za-z_][A-Za-z0-9_]*)=(.*)$/;
  for (const line of output.split("\n")) {
    const m = re.exec(line);
    if (!m) continue;
    const key = m[1];
    if (!key || !PROVISION_ENV_ALLOWLIST.has(key)) continue;
    // Function replacer so a `$&`/`$1` sequence in basePath is inserted literally,
    // not interpreted as a replacement pattern.
    const value = unquote(m[2] ?? "").replace(/\$\{?PATH\}?/g, () => basePath);
    out[key] = value;
  }
  return out;
}

/**
 * Provision the run's tool packages and return the allowlisted env to fold into
 * the SDK env. Throws on any failure (missing package, devbox error) so the run
 * fails with a clear message rather than degrading silently.
 */
export async function provisionTools(input: ProvisionInput, deps: ProvisionDeps): Promise<ProvisionResult> {
  const run = deps.run ?? defaultRun;
  const source = deps.processEnv ?? process.env;

  await fs.mkdir(input.runDir, { recursive: true });
  // Packages-only manifest, synthesized OUTSIDE the clone (Decision 3): no
  // init_hook/scripts, no repo input — just the resolved, validated package list.
  const manifest = JSON.stringify({ packages: input.packages }, null, 2) + "\n";
  await fs.writeFile(path.join(input.runDir, "devbox.json"), manifest, "utf8");

  const env = buildProvisionEnv(source, input.homeDir);

  try {
    await run("devbox", ["install"], { cwd: input.runDir, env });
  } catch (err) {
    throw new Error(`tool provisioning failed (devbox install): ${errMessage(err)}`);
  }

  let shellenv: RunResult;
  try {
    // --no-refresh-alias: don't touch shell aliases; we only want the env export.
    shellenv = await run("devbox", ["shellenv", "--no-refresh-alias"], { cwd: input.runDir, env });
  } catch (err) {
    throw new Error(`tool provisioning failed (devbox shellenv): ${errMessage(err)}`);
  }

  const toolEnv = filterShellenv(shellenv.stdout, env.PATH ?? "");
  deps.log.info("tools provisioned", { packages: input.packages, exported: Object.keys(toolEnv).sort() });
  return { toolEnv };
}
