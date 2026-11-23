import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { resolve } from "node:path";
import { recordEvidence } from "./evidence.js";
import { PRIVATE_CA_TITLE } from "./private-ca-title.js";

export const PRIVATE_CA_CASES = [
  "loadConfig accepts exactly 1 MiB valid CA and captures every byte",
  ...["missing", "unreadable", "malformed", "oversized", "65certs"].map(
    kind => `loadConfig rejects ${kind} CA with sanitized error`,
  ),
  "stalled TLS has bounded typed timeout",
  "private CA completes Responses",
  "no extra CA refuses private TLS",
  "unrelated CA refuses private TLS",
];

export const PRIVATE_CA_OBSERVATIONS = [
  "owned-filesystem-observed trusted",
  "owned-filesystem-observed no-extra",
  "persisted-session-subset-observed trusted nonempty-runtime-jsonl exclusions-pass",
  "interrupted-root-settled-observed trusted active-turn cancelled owned-root-absent",
  "fresh-executor-resume-observed trusted same-runtime-session prior-history distinct-root-auth-home old-root-absent",
  ...["stalled", "trusted", "no-extra", "unrelated"].map(row => `owned-roots-absent ${row} `),
];

/** Accept only the complete TAP run, not a successful shell with partial output. */
export function requirePrivateCaProof(stdout: string): void {
  const lines = stdout.split("\n");
  const passes = lines.filter(line => /^ok \d+ - /.test(line));
  if (passes.length !== 10 || PRIVATE_CA_CASES.some(
    (title, index) => passes[index] !== `ok ${index + 1} - ${title}`,
  )) throw new Error("private CA: missing or unexpected named case");
  for (const summary of ["1..10", "# tests 10", "# pass 10", "# fail 0", "# cancelled 0", "# skipped 0", "# todo 0", "CONTAINER_EXIT=0"]) {
    if (lines.filter(line => line === summary).length !== 1) {
      throw new Error(`private CA: missing/duplicate summary ${summary}`);
    }
  }
  if (lines.some(line => /^not ok\b/.test(line))) throw new Error("private CA: failed case");
  for (const observation of PRIVATE_CA_OBSERVATIONS) {
    if (!lines.some(line => line.startsWith(`# ${observation}`))) {
      throw new Error(`private CA: missing observation ${observation}`);
    }
  }
}

export function candidateCaEnvironment(env: NodeJS.ProcessEnv): NodeJS.ProcessEnv {
  const clean = { ...env };
  delete clean.FIXTURE_IMAGE;
  delete clean.FIXTURE_EXPECTED_ID;
  return clean;
}

/** One attempt, 25-minute child deadline, 4-MiB output cap. Failure blocks completeness.
 * The shell independently bounds the build and container, and cleans its named container. */
export async function runPrivateCa(): Promise<void> {
  recordEvidence(PRIVATE_CA_TITLE, "fail");
  const { stdout, stderr } = await promisify(execFile)(
    "bash", [resolve(__dirname, "../codex-private-ca/run.sh")],
    {
      cwd: resolve(__dirname, "../.."),
      env: candidateCaEnvironment(process.env),
      timeout: 25 * 60 * 1000,
      killSignal: "SIGTERM",
      maxBuffer: 4 * 1024 * 1024,
    },
  );
  process.stdout.write(stdout);
  process.stderr.write(stderr);
  requirePrivateCaProof(stdout);
  recordEvidence(PRIVATE_CA_TITLE, "pass");
}
