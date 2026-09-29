// issue #1932: pure, I/O-free rendering of gitleaks findings into labels that are safe to put in
// a lead prompt, a status/feed line, a run log field and the stored `failure_reason`.
//
// A finding's `file` comes from gitleaks' report of ATTACKER-authored repo content, so a committed
// filename can carry control bytes (forged rows, ANSI), bidi/format characters, prompt-shaped text,
// an unbounded length, or the secret itself (a token used as a file name). Every consumer renders
// through here so the same escaping/withholding applies everywhere. `SecretFinding.Secret` is never
// read (the report is `--redact`'d and the parser drops it).
import type { SecretFinding } from "./secret-scan-guard.js";

export interface RenderSecretFindingOpts {
  /** The run's text redactor (`makeTextRedactor`): a path it changes carries a run secret. */
  redact?: (s: string) => string;
  /** Withhold every path (a text scan of the path list was untrusted: fail closed). */
  withholdPaths?: boolean;
}

/** Escaped paths are cut to this many characters (then `…`). Applied AFTER escaping. */
const MAX_PATH_LEN = 120;

const RULE_ID_RE = /^[A-Za-z0-9._-]{1,64}$/;
const COMMIT_RE = /^[0-9a-fA-F]{40}$/;
// C0, DEL, C1, and every Unicode format (Cf: zero-width, bidi marks/overrides/isolates, BOM).
// eslint-disable-next-line no-control-regex
const UNSAFE_CHAR_RE = /[\u0000-\u001f\u007f-\u009f\p{Cf}]/gu;

/** Provider token prefixes, assembled from fragments so no complete token literal sits in source. */
const TOKEN_PREFIXES: string[] = [
  "glpat" + "-",
  "ghp" + "_",
  "gho" + "_",
  "ghu" + "_",
  "ghs" + "_",
  "ghr" + "_",
  "github" + "_pat_",
  "sk" + "-ant-",
  "xox" + "b-",
  "xox" + "p-",
  "uzp" + "_",
];
const AWS_KEY_ID_RE = new RegExp("AK" + "IA[A-Z0-9]{16}");

/**
 * Heuristic: does this path look like it embeds a secret (a token used as a file name)? Ordinary
 * paths stay visible: the score is per segment split on `/ . - _` and whitespace, so only a long
 * (24+) segment mixing upper case, lower case and digits, or a known provider prefix, matches.
 */
export function pathLooksSecretShaped(path: string): boolean {
  if (AWS_KEY_ID_RE.test(path)) return true;
  for (const p of TOKEN_PREFIXES) if (path.includes(p)) return true;
  for (const seg of path.split(/[/.\-_\s]+/u)) {
    if (seg.length >= 24 && /[A-Z]/.test(seg) && /[a-z]/.test(seg) && /[0-9]/.test(seg)) return true;
  }
  return false;
}

function escapePath(path: string): string {
  const escaped = path.replace(UNSAFE_CHAR_RE, (c) => `\\u{${c.codePointAt(0)!.toString(16)}}`);
  const chars = Array.from(escaped);
  return chars.length > MAX_PATH_LEN ? chars.slice(0, MAX_PATH_LEN).join("") + "…" : escaped;
}

function renderPath(path: unknown, opts: RenderSecretFindingOpts): string {
  if (typeof path !== "string" || opts.withholdPaths) return "[path withheld]";
  if (opts.redact && opts.redact(path) !== path) return "[path withheld]";
  if (pathLooksSecretShaped(path)) return "[path withheld]";
  return escapePath(path);
}

/** One safe label: `<commit12> <path>:<line> (rule <ruleId>)`. */
export function renderSecretFinding(f: SecretFinding, opts: RenderSecretFindingOpts = {}): string {
  const commit = typeof f.commit === "string" && COMMIT_RE.test(f.commit) ? f.commit.slice(0, 12) : "[commit withheld]";
  const rule = typeof f.ruleId === "string" && RULE_ID_RE.test(f.ruleId) ? f.ruleId : "[rule withheld]";
  const line = Number.isSafeInteger(f.startLine) && f.startLine >= 0 ? String(f.startLine) : "?";
  return `${commit} ${renderPath(f.file, opts)}:${line} (rule ${rule})`;
}

/** The first `max` labels joined with `; `, plus a `; and N more` tail. */
export function renderSecretFindings(
  findings: SecretFinding[],
  opts: RenderSecretFindingOpts = {},
  max = 5,
): string {
  const shown = findings.slice(0, max).map((f) => renderSecretFinding(f, opts));
  const more = findings.length - shown.length;
  return shown.join("; ") + (more > 0 ? `; and ${more} more` : "");
}

/**
 * The follow-up prompt for the lead when the pre-exit secret scan flagged commits not yet
 * published. Carries ONLY rendered labels, never raw file fields.
 */
export function buildSecretRemediationFollowUp(
  findings: SecretFinding[],
  opts: RenderSecretFindingOpts & { floorSha?: string; attempt: number; maxAttempts: number },
): string {
  const labels = renderSecretFindings(findings, opts);
  const floor = opts.floorSha && /^[0-9a-fA-F]{7,40}$/.test(opts.floorSha) ? opts.floorSha : undefined;
  const base = floor ?? "the parent of the earliest flagged commit";
  return [
    "The worker's pre-push secret scan (gitleaks default ruleset) flagged the following in commits that are not published yet, so the run cannot finish as is:",
    labels,
    "",
    "Rewrite the flagged literal OUT of the commit that introduced it: edit the file, `git commit --fixup=<commit>`, then `GIT_SEQUENCE_EDITOR=: git rebase -i --autosquash " +
      (floor ?? "<the flagged commit's parent>") +
      "`. A later commit that only deletes the line does NOT clear it: the original commit still ships.",
    floor
      ? `Never rewrite commits at or below ${floor}: they are already published.`
      : "Only rewrite commits that are not published yet.",
    `The finding may be a false positive (for example prose like "token: TestName"). Reword it anyway: allow directives are ignored by this scan (rebase base: ${base}).`,
    "Then re-run your checks and call signal_done again.",
    `This is attempt ${opts.attempt} of ${opts.maxAttempts}; after the last attempt the run fails.`,
  ].join("\n");
}
