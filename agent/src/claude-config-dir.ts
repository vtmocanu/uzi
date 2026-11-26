// The worker-prepared Claude CLI config directory name (PRD #4416). A leaf module with no imports of
// its own beyond node:path, so sdk-env.ts (whose import closure may load only node builtins and
// relative modules), guardrails.ts and claude-global-config.ts can all name the one path without a
// dependency cycle.
import path from "node:path";

/** The directory, directly under HOME, the CLI is pointed at with CLAUDE_CONFIG_DIR. */
export const CLAUDE_CONFIG_DIR_NAME = ".uzi-claude-config";

/** `<home>/.uzi-claude-config`: what CLAUDE_CONFIG_DIR must be for a CLI started with this HOME. */
export function claudeConfigDir(home: string): string {
  return path.join(home, CLAUDE_CONFIG_DIR_NAME);
}
