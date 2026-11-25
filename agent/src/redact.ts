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
export const INVISIBLE_SRC = String.raw`\x1b\[[0-?]*[ -/]*[@-~]|[\p{Cc}\p{Cf}]`;
const INVISIBLE = new RegExp(INVISIBLE_SRC, "gu");
const INVISIBLE_TEST = new RegExp(INVISIBLE_SRC, "u");

/** Every encoding of `key` a gateway error body is known to echo it in: raw, JSON-escaped, JSON
 *  with `\/`, the HTML-safe `\u00XX` forms Go's encoding/json emits (both hex cases), and the
 *  percent-encoded forms (both hex cases, and `+` for a space). Longest first, so a raw
 *  substring never leaves the surrounding escape characters behind. */
export function providerKeyForms(key: string): string[] {
  const json = JSON.stringify(key).slice(1, -1);
  const htmlSafe = (hex: (s: string) => string): string =>
    json.replace(/[<>&\u2028\u2029]/g, (c) => `\\u${hex(c.charCodeAt(0).toString(16).padStart(4, "0"))}`);
  const pct = encodeURIComponent(key);
  const forms = [
    key, json, json.replaceAll("/", "\\/"), htmlSafe((h) => h), htmlSafe((h) => h.toUpperCase()),
    pct, pct.replace(/%[0-9A-F]{2}/g, (m) => m.toLowerCase()),
  ];
  if (key.includes(" ")) forms.push(pct.replaceAll("%20", "+"));
  return [...new Set(forms)].sort((a, b) => b.length - a.length);
}

export type TextRedactor = (s: string) => string;

/** The api bounds a non_closing reason to 1..500 code points. */
const NON_CLOSING_REASON_MAX = 500;

/** Redact a non_closing reason, then clamp it to the api's 500 code-point bound (the redaction
 *  marker can lengthen a short secret). A non-empty reason stays non-empty. */
export function redactNonClosingReason(redact: TextRedactor, reason: string): string {
  const out = redact(reason);
  const points = Array.from(out);
  return points.length > NON_CLOSING_REASON_MAX ? points.slice(0, NON_CLOSING_REASON_MAX).join("") : out;
}

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
    // Every span is found in the ORIGINAL string and the union is replaced once, so an
    // overlapping or self-overlapping occurrence never leaves part of a secret behind.
    const spans = verbatimSpans(s, list);
    if (visible.length > 0 && INVISIBLE_TEST.test(s)) collectInvisibleSpans(spans, s, visible);
    return replaceSpans(s, spans);
  };
}

/** Push [start, end) for each occurrence of `needle` in `haystack`, resuming one code unit past
 *  each match start so overlaps are found; an occurrence overlapping the previous span extends it.
 *  `map` turns a haystack range into an original-text span. */
function pushOccurrences(spans: Array<[number, number]>, haystack: string, needle: string,
  map: (at: number, end: number) => [number, number]): void {
  let open: [number, number] | undefined;
  let openEnd = -1; // haystack end of the open span
  for (let at = haystack.indexOf(needle); at !== -1; at = haystack.indexOf(needle, at + 1)) {
    const span = map(at, at + needle.length);
    if (open !== undefined && at < openEnd) {
      open[1] = span[1];
    } else {
      open = span;
      spans.push(open);
    }
    openEnd = at + needle.length;
  }
}

/** Every verbatim occurrence of each secret, overlaps included. */
function verbatimSpans(text: string, secrets: string[]): Array<[number, number]> {
  const spans: Array<[number, number]> = [];
  for (const secret of secrets) pushOccurrences(spans, text, secret, (at, end) => [at, end]);
  return spans;
}

/** Replace the union of `spans` in `text`: overlapping spans merge into one marker (spans that only
 *  touch stay separate markers); text outside every span is kept byte-for-byte. */
function replaceSpans(text: string, spans: Array<[number, number]>): string {
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
 * Push the spans of each secret wherever its characters appear in order with only invisible runs between
 * them. The text is projected onto its visible characters (remembering where each came from),
 * matched there, and every match is mapped back to a span of the ORIGINAL, invisible separators
 * included, so nothing of the secret survives to be re-joined.
 */
function collectInvisibleSpans(spans: Array<[number, number]>, text: string, secrets: string[]): void {
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

  for (const secret of secrets) {
    pushOccurrences(spans, visible, secret, (at, end) => [origin[at]!, origin[end - 1]! + 1]);
  }
}

/** Scrub string values and member names without dropping colliding data. Finite numeric sums merge,
 * plain records merge recursively, and other collisions retain the later value under a safe
 * suffix. Reserve real names first so a generated suffix cannot steal an unrelated field.
 * Bounded diagnostic callers may retain their existing container/depth boundary via descend. */
export function redactStrings<T>(value: T, scrubString: TextRedactor,
  descend?: (container: object, depth: number) => boolean): T {
  // The old walker copies enumerable members of class instances too. Preserve that output,
  // but only copies originating from plain records may participate in recursive merging.
  const plainRecords = new WeakSet<object>();
  const record = (entries: Array<[string, unknown]>, plain: boolean): Record<string, unknown> => {
    const reserved = new Set(entries.map(([name]) => name));
    const out = new Map<string, unknown>();
    for (const [name, next] of entries) {
      if (!out.has(name)) {
        out.set(name, next);
        continue;
      }
      const prior = out.get(name);
      if (typeof prior === "number" && Number.isFinite(prior) &&
          typeof next === "number" && Number.isFinite(next) && Number.isFinite(prior + next)) {
        out.set(name, prior + next);
      } else if (prior !== null && typeof prior === "object" && plainRecords.has(prior) &&
                 next !== null && typeof next === "object" && plainRecords.has(next)) {
        // These names/values are already scrubbed; do not run a redactor twice while merging.
        out.set(name, record([...Object.entries(prior), ...Object.entries(next)], true));
      } else {
        let suffix = 2;
        while (reserved.has(`${name}#${suffix}`) || out.has(`${name}#${suffix}`)) suffix++;
        out.set(`${name}#${suffix}`, next);
      }
    }
    // fromEntries keeps a literal __proto__ as own data instead of invoking a setter.
    const result = Object.fromEntries(out);
    if (plain) plainRecords.add(result);
    return result;
  };
  const walk = (value: unknown, depth = 0): unknown => {
    if (typeof value === "string") return scrubString(value);
    if (value && typeof value === "object" && descend && !descend(value, depth)) return value;
    if (Array.isArray(value)) return value.map(child => walk(child, depth + 1));
    if (value && typeof value === "object") {
      const proto = Object.getPrototypeOf(value);
      return record(Object.entries(value).map(([name, child]) => [scrubString(name), walk(child, depth + 1)]),
        proto === Object.prototype || proto === null);
    }
    return value;
  };
  return walk(value) as T;
}

/**
 * Build a payload redactor over the given secrets. Returns an identity function
 * when no usable secret is supplied, so callers pay nothing when there is nothing
 * to scrub. Member names use the same redaction and collision rules as values.
 */
export function makeRedactor(secrets: Array<string | undefined | null>): PayloadRedactor {
  if (usableSecrets(secrets).length === 0) return (payload) => payload;
  const scrubString = makeTextRedactor(secrets);
  return (payload) => redactStrings(payload, scrubString);
}

/**
 * A growable secret registry (issue #4201). An in-place credential switch publishes a NEW OAuth
 * token mid-run, after the flight's redactors were built, so the flight needs redactors that read
 * the current list. `text` and `payload` are stable function identities (safe to hand out once)
 * that always scrub every secret added so far. The same 8-char floor and invisible-run handling as
 * makeTextRedactor / makeRedactor apply; `add` is idempotent. A secret is never removed.
 */
export class SecretSet {
  private list: string[] = [];
  private currentText: TextRedactor = (s) => s;
  private currentPayload: PayloadRedactor = (payload) => payload;
  readonly text: TextRedactor = (s) => this.currentText(s);
  readonly payload: PayloadRedactor = (payload) => this.currentPayload(payload);

  constructor(initial: Array<string | null | undefined>) {
    this.list = [...new Set(usableSecrets(initial))];
    this.rebuild();
  }

  add(secret: string): void {
    if (typeof secret !== "string" || secret.length < MIN_SECRET_LEN || this.list.includes(secret)) return;
    this.list.push(secret);
    this.rebuild();
  }

  /** The usable secrets added so far (a copy). */
  values(): readonly string[] {
    return [...this.list];
  }

  private rebuild(): void {
    this.currentText = makeTextRedactor(this.list);
    this.currentPayload = makeRedactor(this.list);
  }
}
