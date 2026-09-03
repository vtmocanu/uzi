// PRD #974 M2 (load-bearing security): the pure, I/O-free helpers behind the finalize
// pre-push secret scan. Shaped like ci-config-guard.ts — this module parses/decides only;
// the caller (git.ts secretScanRange) runs gitleaks and does every read/write.
//
// WHY a liveness gate at all: gitleaks prints "no leaks found" and exits 0 on an
// UNRESOLVED or EMPTY commit range (a bad `--log-opts`, a base that does not resolve), so a
// clean verdict is MEANINGLESS unless we also prove the scan actually walked the commits the
// push carries. scanIsTrustworthy is that proof; the caller fails OPEN (relies on the GH013
// remote backstop) when it is false, and only acts on findings when it is true.

/** One gitleaks finding, narrowed to the fields the finalize path needs. Parsed from
 *  gitleaks' JSON report objects (`File`, `StartLine`, `Commit`, `RuleID`); extra report
 *  fields (Secret, Entropy, Fingerprint, …) are ignored. */
export interface SecretFinding {
  file: string;
  startLine: number;
  commit: string;
  ruleId: string;
}

/** Cap on the findings parseGitleaksReport materialises from one report. The pre-push decision
 *  only needs "≥1 secret" and a few names for the failure_reason, so an adversarial report with
 *  a huge finding count is bounded here (paired with secretScanRange's byte cap on the read). */
const MAX_PARSED_FINDINGS = 500;

/**
 * Parse gitleaks' JSON report (an array of finding objects) into SecretFinding[].
 *
 * Tolerant by design: returns [] — never throws — for an empty string, the literal
 * `"null"` (gitleaks writes `null` to the report when it finds nothing), a non-array
 * top-level value, or malformed JSON. Missing/extra fields on an element are tolerated:
 * File/Commit/RuleID default to "" and StartLine to 0.
 *
 * NOTE for the CALLER: a parse THROW cannot happen here (this function swallows it to []),
 * but the caller still treats a failed report READ (fs error) or any thrown state as
 * UNTRUSTED — it returns trusted:false rather than a clean verdict. Parsing to [] here is
 * "the report said no findings"; it is NOT "the scan was trustworthy" — that is
 * scanIsTrustworthy's separate job.
 */
export function parseGitleaksReport(json: string): SecretFinding[] {
  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) return [];
  const out: SecretFinding[] = [];
  for (const el of parsed) {
    if (el === null || typeof el !== "object") continue;
    const o = el as Record<string, unknown>;
    const startLine = typeof o.StartLine === "number" ? o.StartLine : 0;
    out.push({
      file: typeof o.File === "string" ? o.File : "",
      startLine,
      commit: typeof o.Commit === "string" ? o.Commit : "",
      ruleId: typeof o.RuleID === "string" ? o.RuleID : "",
    });
    // Cap the materialised findings: the caller only needs to know secrets are present and to
    // name a few in the failure_reason, so an adversarial report with millions of findings must
    // not be walked in full. A cap here means "there is at least one secret", which is all the
    // pre-push decision requires.
    if (out.length >= MAX_PARSED_FINDINGS) break;
  }
  return out;
}

/**
 * Extract N from gitleaks' `INF N commits scanned` line on stderr (it prints e.g.
 * `8:15AM INF 1 commits scanned.`). Case-insensitive and tolerant of surrounding text /
 * ANSI color codes; returns the first match's N, or null when the line is absent.
 *
 * This is the count the liveness gate compares against the range length — the single signal
 * that the scan walked commits at all rather than no-opping on an unresolved range.
 */
export function commitsScannedFromStderr(stderr: string): number | null {
  const m = stderr.match(/(\d+)\s+commits?\s+scanned/i);
  if (!m) return null;
  const n = Number.parseInt(m[1]!, 10);
  return Number.isNaN(n) ? null : n;
}

/**
 * The liveness gate. TRUE only when EVERY clause holds — otherwise the caller fails OPEN to
 * the GH013 remote backstop rather than acting on a possibly-vacuous clean verdict:
 *
 *  - execOk: the gitleaks process ran to completion without an exec error. Under
 *    `--exit-code 0` a real finding is still exit 0, so a NON-zero exit is an instrument
 *    failure, not a finding — the caller sets execOk = !error accordingly.
 *  - no ERR / fatal / unknown revision token on stderr (case-insensitive): gitleaks logs a
 *    scan error (bad range, unreadable object) as `ERR`/`fatal`, and git resolves a bad ref
 *    with `unknown revision`. Any of these means the walk was incomplete, so a "no leaks"
 *    verdict cannot be trusted.
 *  - scannedCommits !== null: we could actually READ the "N commits scanned" line. Absent →
 *    we cannot prove the scan walked anything.
 *  - scannedCommits === expectedCommits: the scan walked EXACTLY the commits the push
 *    carries (the `base..head` range length). A mismatch (0, or fewer than expected) is the
 *    unresolved/empty-range trap — gitleaks reports clean but scanned the wrong thing.
 */
export function scanIsTrustworthy(opts: {
  stderr: string;
  scannedCommits: number | null;
  expectedCommits: number;
  execOk: boolean;
}): boolean {
  if (!opts.execOk) return false;
  // WORD-BOUNDARY match, not a bare substring: gitleaks/git surface a scan error as the
  // zerolog level `ERR` or the words `error`/`fatal` (git also emits `unknown revision`), all
  // standalone tokens. A substring `includes("err")` would also fire on a benign path or word
  // that merely CONTAINS "err" (e.g. a filename), silently flipping every scan to untrusted and
  // degrading the block to backstop-only. The tokens below are the real error signals only.
  if (/\b(err|error|fatal)\b/i.test(opts.stderr) || /unknown revision/i.test(opts.stderr)) {
    return false;
  }
  if (opts.scannedCommits === null) return false;
  return opts.scannedCommits === opts.expectedCommits;
}

/**
 * The exact argv AFTER the binary name, so a unit test can pin it and the caller cannot drift
 * from the invocation proven (issue #974 step 5) to disable all three silencers GitHub Push
 * Protection ignores:
 *   - `-c <configPath>` — an EXPLICIT `[extend] useDefault=true` config, which forces the
 *     embedded default ruleset AND skips auto-discovery of the target repo's `.gitleaks.toml`.
 *   - scanning the worker BARE (no working tree) from a neutral cwd — the repo's
 *     `.gitleaks.toml` / `.gitleaksignore` are never on disk to be read.
 *   - `--ignore-gitleaks-allow` — disables inline `//gitleaks:allow` comments.
 * Plus `--exit-code 0` (a finding is NOT a nonzero exit — nonzero means an instrument
 * failure), `--no-banner`, `--redact` (never write raw secrets to the report or logs), and a
 * JSON report at `reportPath`. `--log-opts` takes the `base..head` range as its value.
 */
export function gitleaksArgs(opts: {
  sourcePath: string;
  logRange: string;
  configPath: string;
  reportPath: string;
}): string[] {
  return [
    "git",
    opts.sourcePath,
    "--log-opts",
    opts.logRange,
    "-c",
    opts.configPath,
    "--ignore-gitleaks-allow",
    "--exit-code",
    "0",
    "--no-banner",
    "--redact",
    "--report-format",
    "json",
    "--report-path",
    opts.reportPath,
  ];
}
