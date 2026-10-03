// Fixed, credential-free launcher helper for PRD #1171 session restoration.
// The worker first materializes the current session-store generation into a
// short-lived allowlisted staging tree. launchCodexRoot invokes this entrypoint
// as uid `runner`, after the final 0710/2750 tree exists and before app-server
// spawn, so only the provider identity writes its sessions directory.
//
// Exit codes: 0 = seeded. 2 = normally this helper's own handled failure, with exactly one
// content-free reason line on stderr (never an error message, path or session content);
// the runtime itself can also exit 2 (a missing TMPDIR did, with the tsx version tested),
// in which case its own stderr is what the launcher publishes.
// 1 = a failure outside runSessionSeedCli: runtime startup or an uncaught exception at
// module load. Reproduced: (a) under the `tsx` CLI, a TMPDIR long enough that
// `${TMPDIR}/tsx-<uid>/<pid>.pipe` exceeds the 107-byte unix socket path limit gives
// `Error: listen EINVAL`, exit 1 (#2187 incident; root cause fixed separately by #2190);
// (b) an entrypoint that throws at module load exits 1 with the thrown error, under both
// `tsx <script>` and `node --import tsx <script>`. The launcher surfaces the redacted
// stderr tail in every case.

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

export interface SessionSeedCliDeps {
  readonly seed?: typeof seedCodexSessionArtifacts;
  readonly stderr?: (line: string) => void;
}

/** Error code for the stderr line: only [A-Za-z0-9_], at most 64 chars, since an error's
 * code or name can be attacker-shaped. */
function safeErrorCode(err: unknown): string {
  const raw = (err as NodeJS.ErrnoException | null | undefined)?.code
    ?? (err instanceof Error ? err.name : "unknown");
  const cleaned = String(raw).replace(/[^A-Za-z0-9_]/g, "").slice(0, 64);
  return cleaned === "" ? "unknown" : cleaned;
}

export async function runSessionSeedCli(argv: readonly string[], deps: SessionSeedCliDeps = {}): Promise<number> {
  const seed = deps.seed ?? seedCodexSessionArtifacts;
  const write = deps.stderr ?? ((line: string) => { process.stderr.write(`${line}\n`); });
  if (argv.length !== 4) {
    write("session seed: invalid arguments");
    return 2;
  }
  try {
    const result = await seed(argv[2]!, argv[3]!);
    if (result.files > 0) return 0;
    write("session seed: no artifacts copied");
    return 2;
  } catch (err) {
    write(`session seed failed: ${safeErrorCode(err)}`);
    return 2;
  }
}

const invokedPath = process.argv[1];
if (invokedPath !== undefined && import.meta.url === pathToFileURL(invokedPath).href) {
  void runSessionSeedCli(process.argv).then((code) => { process.exitCode = code; });
}
