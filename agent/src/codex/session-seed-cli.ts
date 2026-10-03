// Fixed, credential-free launcher helper for PRD #1171 session restoration.
// The worker first materializes the current session-store generation into a
// short-lived allowlisted staging tree. launchCodexRoot invokes this entrypoint
// as uid `runner`, after the final 0710/2750 tree exists and before app-server
// spawn, so only the provider identity writes its sessions directory.

import { pathToFileURL } from "node:url";

import { seedCodexSessionArtifacts } from "./session-state.js";

export const SESSION_SEED_APP_ROOT = "/app";

/** The one place the seed helper's argv is built: the launcher spawns exactly this, so a
 * test can assert the production command without re-deriving it. */
export function sessionSeedInvocation(
  appRoot: string,
  sourceDir: string,
  destDir: string,
): { command: string; args: string[] } {
  // The tsx CLI opens an IPC socket under TMPDIR; an epoch's long private
  // path exceeds Unix socket limits. The resolved loader needs no CLI socket (#2190).
  return {
    command: process.execPath,
    args: ["--import", import.meta.resolve("tsx"), `${appRoot}/src/codex/session-seed-cli.ts`, sourceDir, destDir],
  };
}

export const SESSION_SEED_ENTRYPOINT = `${SESSION_SEED_APP_ROOT}/src/codex/session-seed-cli.ts`;

async function runSessionSeedCli(argv: readonly string[]): Promise<number> {
  if (argv.length !== 4) return 2;
  try {
    const result = await seedCodexSessionArtifacts(argv[2]!, argv[3]!);
    return result.files > 0 ? 0 : 2;
  } catch {
    return 2;
  }
}

const invokedPath = process.argv[1];
if (invokedPath !== undefined && import.meta.url === pathToFileURL(invokedPath).href) {
  void runSessionSeedCli(process.argv).then((code) => { process.exitCode = code; });
}
