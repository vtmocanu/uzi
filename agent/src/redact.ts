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
// Best-effort by design: this is exact-substring replacement, so an obfuscated
// secret (base64, char-split, whitespace-injected) slips through. It is a
// safety net, not a boundary — the boundaries are the sparse agent env (the PAT
// and join token never enter the agent) and the tool-boundary guardrails.

const REDACTED = "***REDACTED***";
// Matches log.ts SecretRegistry: don't blanket-replace short strings whose
// occurrence elsewhere would corrupt unrelated output.
const MIN_SECRET_LEN = 8;

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
  return (s) => {
    let out = s;
    for (const secret of list) out = out.split(secret).join(REDACTED);
    return out;
  };
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
