// PRD #1287 — the git-I/O half of the D8 O-receipt merge gate, split OUT of run-receipts.ts so the
// tail computation is UNIT-TESTABLE without triggering the gate. These three helpers do all the
// impure work `task check:codex-m4-receipts` needs — resolve the candidate commit, load the trusted
// (out-of-band, gitignored) candidate manifest, and measure the provenBaseCommit..candidate tail via
// git — and this module runs NOTHING at import time (no `main()`, no top-level statement with a side
// effect). That lets receipts.test.ts import `computeTail` and point it at a scratch repo without the
// gate running or calling process.exit. run-receipts.ts stays a thin entrypoint that wires these into
// the pure `checkReceipts` and prints diagnostics.
//
// Inputs (both out-of-band, supplied by the lead):
//   CODEX_M4_RECEIPT_MANIFEST  — path to the trusted candidate manifest JSON (the packaged proof's
//                                proven base/jvm digests + one discharging record per committed O
//                                clause, all bound to a provenBaseCommit). Absent/unreadable → fail
//                                closed as "manifest absent"; present-but-malformed → fail closed +
//                                exit 1. It is gitignored (receipt-manifest.json), so recording it
//                                makes NO commit and cannot invalidate the candidate it certifies.
//   CODEX_M4_CANDIDATE_COMMIT  — the candidate commit; when unset, derived from `git rev-parse HEAD`.

import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import path from "node:path";

import { parseManifest, type CandidateManifest, type TailInput } from "./receipts.js";

/** The candidate commit being certified: the explicit env override, else `git rev-parse HEAD`. */
export function resolveCandidateCommit(): string {
  const env = process.env.CODEX_M4_CANDIDATE_COMMIT;
  if (env !== undefined && env.trim().length > 0) return env.trim();
  try {
    return execFileSync("git", ["rev-parse", "HEAD"], { encoding: "utf8" }).trim();
  } catch (err) {
    console.error(
      "run-receipts: cannot determine the candidate commit — set CODEX_M4_CANDIDATE_COMMIT or run "
      + `inside a git checkout (${(err as Error).message})`,
    );
    process.exit(1);
  }
}

/** Load the trusted candidate manifest, failing closed. Returns the parsed manifest, or null when
 *  it is absent/unreadable/malformed; `malformed` is true only for a present-but-broken file (a
 *  distinct, always-exit-1 case from a simply-absent one). */
export function loadManifest(): { manifest: CandidateManifest | null; malformed: boolean } {
  const envPath = process.env.CODEX_M4_RECEIPT_MANIFEST;
  if (envPath === undefined || envPath.trim().length === 0) {
    return { manifest: null, malformed: false }; // unset → absent (checkReceipts reports it)
  }
  const resolved = path.resolve(process.cwd(), envPath.trim());

  let raw: string;
  try {
    raw = readFileSync(resolved, "utf8");
  } catch (err) {
    if ((err as NodeJS.ErrnoException).code === "ENOENT") {
      return { manifest: null, malformed: false }; // file does not exist → absent
    }
    console.error(`run-receipts: candidate manifest is malformed: cannot read ${resolved}: ${(err as Error).message}`);
    return { manifest: null, malformed: true };
  }

  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch (err) {
    console.error(`run-receipts: candidate manifest is malformed: ${(err as Error).message}`);
    return { manifest: null, malformed: true };
  }

  const result = parseManifest(parsed);
  if ("error" in result) {
    console.error(`run-receipts: candidate manifest is malformed: ${result.error}`);
    return { manifest: null, malformed: true };
  }
  return { manifest: result.manifest, malformed: false };
}

/** Compute the `provenBase..candidate` tail via git. `git merge-base --is-ancestor` (exit 0 →
 *  ancestor, non-zero/throw → not) decides reachability; when it is an ancestor, `git diff
 *  --no-renames --name-only provenBase..candidate` yields the changed paths. A total git failure
 *  returns null so checkReceipts fails closed (base-not-ancestor) rather than certifying on missing
 *  evidence.
 *
 *  `--no-renames` is LOAD-BEARING for the evidence-only-tail invariant: with git's default rename
 *  detection ON, a RUNTIME file relocated to an EVIDENCE path (e.g. `git mv agent/src/foo.ts
 *  docs/foo.ts`) shows ONLY the evidence destination, so the runtime source's drift vanishes and the
 *  gate would wrongly certify a candidate whose guardrail runtime moved. With `--no-renames` the
 *  rename surfaces BOTH the deleted runtime SOURCE and the added evidence destination, so the runtime
 *  path is always present and drifts. `-c core.quotepath=false` keeps non-ASCII paths raw (unquoted,
 *  unescaped) so classifyPath sees the real path; it is applied to the merge-base call too for
 *  consistency (harmless there).
 *
 *  `cwd` (default `process.cwd()`) is passed to BOTH git invocations so a unit test can point the
 *  helper at a throwaway scratch repo. */
export function computeTail(provenBase: string, candidate: string, cwd: string = process.cwd()): TailInput | null {
  try {
    let baseIsAncestor: boolean;
    try {
      execFileSync(
        "git",
        ["-c", "core.quotepath=false", "merge-base", "--is-ancestor", provenBase, candidate],
        { stdio: "ignore", cwd },
      );
      baseIsAncestor = true;
    } catch {
      baseIsAncestor = false; // exit 1 (not an ancestor) or a bad-object error → treat as not reachable
    }
    if (!baseIsAncestor) return { paths: [], baseIsAncestor: false };

    const out = execFileSync(
      "git",
      ["-c", "core.quotepath=false", "diff", "--no-renames", "--name-only", `${provenBase}..${candidate}`],
      { encoding: "utf8", cwd },
    );
    const paths = out.split("\n").map((line) => line.trim()).filter((line) => line.length > 0);
    return { paths, baseIsAncestor: true };
  } catch (err) {
    console.error(`run-receipts: cannot compute the provenBase..candidate tail (fail closed): ${(err as Error).message}`);
    return null;
  }
}
