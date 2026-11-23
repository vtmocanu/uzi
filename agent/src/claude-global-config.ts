// Reset the Claude CLI's global config (`<HOME>/.claude.json`) before every CLI start (PRD #4416).
//
// Threat. The pinned Claude CLI honours an `env` block in `<HOME>/.claude.json` (for example
// OTEL_LOG_RAW_API_BODIES, NODE_OPTIONS or ANTHROPIC_BASE_URL), and does so regardless of
// `settingSources: []`. The CLI runs as the runner uid and so does the agent's Bash tool, so the
// agent can write that file during one turn and the next CLI start reads it, with the provider
// credential already in its environment. This module replaces the file with fixed bytes that carry
// no `env` key immediately before each spawn (sdk-spawn.ts spawnDetached), and never reads or
// parses what was there.
//
// Form. A temp file is created exclusively (O_CREAT|O_EXCL|O_NOFOLLOW) next to the target and
// renamed over it: rename replaces a planted symlink (it does not follow it), and the old inode is
// never opened by the worker, so the worker never writes through a planted path. The target is
// never unlinked first, so a failed rename leaves the previous state, not a missing file. In a
// HOME that is not sticky a live runner-uid process can still swap the temp entry before the
// rename and so publish a file of its own; that is part of the live-writer residual below.
//
// Alternate file. The pinned CLI (2.1.319) resolves its global config from
// `<CLAUDE_CONFIG_DIR or HOME/.claude>/.config.json` first: a non-empty regular file there wins
// over `<HOME>/.claude.json`, so an agent can plant an env block there with no process alive.
// Before the replace, that file is removed so the resolver returns `<HOME>/.claude.json`. Under
// the uid split the `.claude` directory is normally created by the CLI as the runner uid, which
// the worker cannot unlink inside, so the removal runs as the runner uid (`rm -f`); without the
// split it is a plain unlink. Failure (non-zero exit, timeout, signal, a directory at the path)
// is a refusal. Without the split, ENOTDIR (`.claude` is a file) is accepted as absent: the CLI's
// stat of the file then fails and its resolver falls back to `.claude.json`. Under the split the
// image's busybox `rm -f` exits non-zero on ENOTDIR, so that state refuses the start (fail closed).
// The `.claude<oauth suffix>.json` variants are chosen only with CLAUDE_CODE_CUSTOM_OAUTH_URL,
// and the run-env `CLAUDE_` prefix is denied (run-env-names.ts), so they are out of scope.
//
// Mode. 0660 with the group the HOME hands down (runner for run and chat HOMEs; job HOMEs
// differ; there is no setgid on single-uid). The CLI saves its config in place (open O_WRONLY,
// truncate) after its rename is refused with EPERM; that applies to the sticky shared chat root
// only, and the in-place write needs group write. The CLI's follow-up chmod to 0600 fails for a
// non-owner and is ignored by the CLI.
//
// Fail closed. A directory (or anything else rename cannot replace) at that path makes the start
// throw a TrustedExecutionRefusal before any process is spawned.
//
// Residuals: ADR-4416, section "Global CLI config input", lists them as pending owner decisions
// (among them a live runner-uid writer racing the startup read or swapping the temp entry, and a
// write while the CLI runs, since the CLI watches the file). The reset narrows the window to the
// spawn; it does not close those.
import { randomBytes } from "node:crypto";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import path from "node:path";
import { runnerCommand, uidSplitActive } from "./runner-uid.js";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";

const RM_BIN = "/bin/rm";
const RM_TIMEOUT_MS = 5000;

/** Injectable seam; production callers pass nothing. */
export interface ResetDeps {
  /** Remove `file` as the runner uid (`rm -f`); must throw on any failure. */
  removeAsRunner?: (file: string) => void;
}

function removeAsRunnerDefault(file: string): void {
  const cmd = runnerCommand(RM_BIN, ["-f", "--", file], true);
  const r = spawnSync(cmd.command, cmd.args, {
    env: { PATH: "/usr/bin:/bin" },
    shell: false,
    stdio: "ignore",
    timeout: RM_TIMEOUT_MS,
    killSignal: "SIGKILL",
  });
  if (r.error) throw r.error;
  if (r.signal) throw new Error(`rm ended by signal ${r.signal}`);
  if (r.status !== 0) throw new Error(`rm exited ${r.status}`);
}

function removeAlternateConfig(home: string, deps: ResetDeps): void {
  const alt = path.join(home, ".claude", ".config.json");
  if (uidSplitActive()) {
    (deps.removeAsRunner ?? removeAsRunnerDefault)(alt);
    return;
  }
  try {
    fs.unlinkSync(alt);
  } catch (err) {
    const code = (err as NodeJS.ErrnoException).code;
    if (code !== "ENOENT" && code !== "ENOTDIR") throw err;
  }
}

/** The bytes written to `<HOME>/.claude.json`: truthy `hasCompletedOnboarding` and `numStartups`,
 *  so the CLI treats it as a populated config and uses it directly (never falling back to a
 *  backup, which it does only for an empty or whitespace file), and no `env` key. */
export const CLAUDE_GLOBAL_CONFIG_BYTES = '{"hasCompletedOnboarding":true,"numStartups":1}\n';

const REFUSAL = "Claude CLI global config could not be replaced by the worker";

/** Replace `<env.HOME>/.claude.json` with {@link CLAUDE_GLOBAL_CONFIG_BYTES}. No-op without a HOME. */
export function resetClaudeGlobalConfig(
  env: Record<string, string | undefined> | undefined,
  deps: ResetDeps = {},
): void {
  const home = env?.HOME;
  if (home === undefined || home === "") return;
  if (!path.isAbsolute(home) || path.resolve(home) !== home) {
    throw new TrustedExecutionRefusal("Claude CLI global config refused: HOME is not a canonical absolute path");
  }
  const target = path.join(home, ".claude.json");
  const tmp = `${target}.uzi-${randomBytes(8).toString("hex")}`;
  try {
    removeAlternateConfig(home, deps);
  } catch (err) {
    throw new TrustedExecutionRefusal(REFUSAL, { cause: err });
  }
  try {
    const fd = fs.openSync(
      tmp,
      fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_WRONLY | fs.constants.O_NOFOLLOW,
      0o660,
    );
    try {
      fs.writeSync(fd, CLAUDE_GLOBAL_CONFIG_BYTES);
      fs.fchmodSync(fd, 0o660);
    } finally {
      fs.closeSync(fd);
    }
    fs.renameSync(tmp, target);
  } catch (err) {
    try {
      fs.unlinkSync(tmp);
    } catch {
      /* best effort: the temp may never have been created */
    }
    throw new TrustedExecutionRefusal(REFUSAL, { cause: err });
  }
}
