// Deep secret redaction for run-message payloads.
//
// The logger's SecretRegistry (log.ts) scrubs log LINES, but run_messages go
// straight to the API through the batcher and never touch the logger. A payload
// can carry a secret: the OAuth token is in the agent subprocess env as
// CLAUDE_CODE_OAUTH_TOKEN, so a tool_result from `echo $CLAUDE_CODE_OAUTH_TOKEN`
// (or any command that surfaces it) would otherwise reach the DB and the live
// stream verbatim. The worker's join token, though absent from the sparse agent
// env, lives in the WORKER process env and so is reachable via a /proc read of
// the parent (denied by the guardrails, redacted here as defense-in-depth).
// This scrubs the run's forge PAT, OAuth token, and join token out of every
// payload before it is batched, the same way and with the same 8-char floor as
// the logger, so nothing sensitive is persisted or broadcast.
//
// Matching is exact, plus one normalisation: invisible separators are skipped. The
// CLI/TUI render payloads through termsafe.SanitizeTTY, which deletes control (Cc)
// and format (Cf) characters, so a secret split by a zero-width space, soft
// hyphen, \r or \b would otherwise be stored split and re-join on screen. A full
// ANSI CSI sequence is skipped too: SanitizeTTY deletes only its ESC, but any
// surface that interprets the sequence shows the halves adjacent. \t and \n
// (Cc, kept by SanitizeTTY) are skipped as well, conservatively. Only the worker
// knows these exact values, so it is the one place such a split can be
// recognised (fixtures/split-secret-redaction pins this against SanitizeTTY). Anything else (base64, whitespace-injected, reordered)
// still slips through: this is a safety net, not a boundary — the boundaries are
// the sparse agent env (the PAT and join token never enter the agent) and the
// tool-boundary guardrails.

const REDACTED = "***REDACTED***";
// Matches log.ts SecretRegistry: don't blanket-replace short strings whose
// occurrence elsewhere would corrupt unrelated output.
const MIN_SECRET_LEN = 8;

// An ANSI CSI escape sequence, or one control (Cc) / format (Cf) character: what a terminal
// renderer deletes or interprets without printing, so text either side of it reads as adjacent.
const INVISIBLE_SRC = String.raw`\x1b\[[0-?]*[ -/]*[@-~]|[\p{Cc}\p{Cf}]`;
const INVISIBLE = new RegExp(INVISIBLE_SRC, "gu");
const INVISIBLE_TEST = new RegExp(INVISIBLE_SRC, "u");

export type TextRedactor = (s: string) => string;
export type PayloadRedactor = (payload: Record<string, unknown>) => Record<string, unknown>;

// usableSecrets keeps only strings at/above the length floor; below it an
// exact-substring replace would corrupt unrelated output.
function usableSecrets(secrets: Array<string | undefined | null>): string[] {
  return secrets.filter((s): s is string => typeof s === "string" && s.length >= MIN_SECRET_LEN);
}

/**
 * Build a string redactor over the given secrets (exact-substring, same 8-char
 * floor as the logger). Identity when no usable secret is supplied. Use this for
 * strings that reach the API OUTSIDE a run_message payload — e.g. a run's
 * failure_reason, which goes straight to reportState and never passes through the
 * batcher's PayloadRedactor.
 */
export function makeTextRedactor(secrets: Array<string | undefined | null>): TextRedactor {
  const list = usableSecrets(secrets);
  if (list.length === 0) return (s) => s;
  const visible = list.map((secret) => secret.replace(INVISIBLE, "")).filter((v) => v.length >= MIN_SECRET_LEN);
  return (s) => {
    let out = s;
    for (const secret of list) out = out.split(secret).join(REDACTED);
    if (visible.length > 0 && INVISIBLE_TEST.test(out)) out = redactAcrossInvisible(out, visible);
    return out;
  };
}

/**
 * Redact each secret wherever its characters appear in order with only invisible runs between
 * them. The text is projected onto its visible characters (remembering where each came from),
 * matched there, and every matched span is replaced in the ORIGINAL, invisible separators
 * included, so nothing of the secret survives to be re-joined.
 */
function redactAcrossInvisible(text: string, secrets: string[]): string {
  const chunks: string[] = [];
  const origin = new Int32Array(text.length); // visible UTF-16 unit i came from text[origin[i]]
  let n = 0;
  let last = 0;
  const keep = (from: number, to: number) => {
    chunks.push(text.slice(from, to));
    for (let i = from; i < to; i++) origin[n++] = i;
  };
  for (const m of text.matchAll(INVISIBLE)) {
    keep(last, m.index);
    last = m.index + m[0].length;
  }
  keep(last, text.length);
  const visible = chunks.join("");

  const spans: Array<[number, number]> = [];
  for (const secret of secrets) {
    for (let at = visible.indexOf(secret); at !== -1; at = visible.indexOf(secret, at + secret.length)) {
      spans.push([origin[at]!, origin[at + secret.length - 1]! + 1]);
    }
  }
  if (spans.length === 0) return text;
  spans.sort((a, b) => a[0] - b[0]);
  let out = "";
  let cursor = 0;
  for (const [start, end] of spans) {
    if (start >= cursor) out += text.slice(cursor, start) + REDACTED;
    cursor = Math.max(cursor, end);
  }
  return out + text.slice(cursor);
}

/**
 * Build a payload redactor over the given secrets. Returns an identity function
 * when no usable secret is supplied, so callers pay nothing when there is nothing
 * to scrub.
 */
export function makeRedactor(secrets: Array<string | undefined | null>): PayloadRedactor {
  if (usableSecrets(secrets).length === 0) return (payload) => payload;

  const scrubString = makeTextRedactor(secrets);
  const walk = (value: unknown): unknown => {
    if (typeof value === "string") return scrubString(value);
    if (Array.isArray(value)) return value.map(walk);
    if (value && typeof value === "object") {
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(value)) out[k] = walk(v);
      return out;
    }
    return value;
  };
  return (payload) => walk(payload) as Record<string, unknown>;
}
