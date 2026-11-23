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
// never opened, so a symlink or hardlink to an outside file is not written through. The target is
// never unlinked first, so a failed rename leaves the previous state, not a missing file.
//
// Mode. 0660 with the runner group, inherited from the setgid HOME: the CLI saves its config in
// place (open O_WRONLY, truncate) after its rename into the sticky shared agent-home root is
// refused with EPERM for a file the worker owns, and that in-place write needs group write. The
// CLI's follow-up chmod to 0600 fails for a non-owner and is ignored by the CLI.
//
// Fail closed. A directory (or anything else rename cannot replace) at that path makes the start
// throw a TrustedExecutionRefusal before any process is spawned.
//
// Residuals (pending owner decisions, ADR-4416): a live runner-uid writer racing the startup read
// (a background process of an earlier turn can rewrite the file after this reset and before the
// CLI reads it), and a possible mid-process re-read (the CLI watches the file for other
// processes). The reset narrows the window to the spawn; it does not close those two.
import { randomBytes } from "node:crypto";
import fs from "node:fs";
import path from "node:path";
import { TrustedExecutionRefusal } from "./trusted-execution-refusal.js";

/** The bytes written to `<HOME>/.claude.json`: truthy `hasCompletedOnboarding` and `numStartups`,
 *  so the CLI treats it as a populated config and uses it directly (never falling back to a
 *  backup, which it does only for an empty or whitespace file), and no `env` key. */
export const CLAUDE_GLOBAL_CONFIG_BYTES = '{"hasCompletedOnboarding":true,"numStartups":1}\n';

const REFUSAL = "Claude CLI global config could not be replaced by the worker";

/** Replace `<env.HOME>/.claude.json` with {@link CLAUDE_GLOBAL_CONFIG_BYTES}. No-op without a HOME. */
export function resetClaudeGlobalConfig(env: Record<string, string | undefined> | undefined): void {
  const home = env?.HOME;
  if (home === undefined || home === "") return;
  if (!path.isAbsolute(home) || path.resolve(home) !== home) {
    throw new TrustedExecutionRefusal("Claude CLI global config refused: HOME is not a canonical absolute path");
  }
  const target = path.join(home, ".claude.json");
  const tmp = `${target}.uzi-${randomBytes(8).toString("hex")}`;
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
