// Deep secret redaction for run-message payloads.
//
// The logger's SecretRegistry (log.ts) scrubs log LINES, but run_messages go
// straight to the API through the batcher and never touch the logger. A payload
// can carry a secret: the OAuth token is in the agent subprocess env as
// CLAUDE_CODE_OAUTH_TOKEN, so a tool_result from `echo $CLAUDE_CODE_OAUTH_TOKEN`
// (or any command that surfaces it) would otherwise reach the DB and the live
// stream verbatim. This scrubs known secret substrings out of every payload
// before it is batched, the same way and with the same 8-char floor as the
// logger, so nothing sensitive is persisted or broadcast.

const REDACTED = "***REDACTED***";
// Matches log.ts SecretRegistry: don't blanket-replace short strings whose
// occurrence elsewhere would corrupt unrelated output.
const MIN_SECRET_LEN = 8;

export type PayloadRedactor = (payload: Record<string, unknown>) => Record<string, unknown>;

/**
 * Build a redactor over the given secrets. Returns an identity function when no
 * usable secret is supplied, so callers pay nothing when there is nothing to
 * scrub.
 */
export function makeRedactor(secrets: Array<string | undefined | null>): PayloadRedactor {
  const list = secrets.filter((s): s is string => typeof s === "string" && s.length >= MIN_SECRET_LEN);
  if (list.length === 0) return (payload) => payload;

  const scrubString = (s: string): string => {
    let out = s;
    for (const secret of list) out = out.split(secret).join(REDACTED);
    return out;
  };
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
