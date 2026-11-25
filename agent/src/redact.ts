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
export const MIN_SECRET_LEN = 8;

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

/** Charged units one redacted string, or one message payload walked by a payload redactor, may spend (PRD 4416,
 *  owner decision B3). A unit is one consulted text position or one UTF-16 unit a literal comparison may touch:
 *  every comparison is charged before it runs, and the uncharged work left (the 8-unit prefix search, the skipped
 *  positions) is linear in the text, so the guard bounds the periodic worst case (a long literal against long periodic output) whatever the host's
 *  speed. A wall deadline cannot interrupt synchronous matching, hence a counter. */
export const REDACTION_WORK_BUDGET = 2 ** 26;

/** What a redactor returns for a WHOLE input whose matching exceeded the budget: never the raw text and never a
 *  partly redacted one. */
export const REDACTION_WITHHELD = "[withheld: redaction work budget exceeded]";

/** The charged work of one redaction. The overflowing charge is included, and nothing is added once the budget is
 *  passed, so a finished count never exceeds REDACTION_WORK_BUDGET + 1 plus the longest single literal. */
export interface RedactionWork {
  charged: number;
}

/** Thrown inside the matcher when a redaction passes its budget; every top-level entry point catches it. */
class RedactionBudgetExceeded extends Error {
  constructor() {
    super("redaction work budget exceeded");
  }
}

function charge(work: RedactionWork, units: number): void {
  if (work.charged > REDACTION_WORK_BUDGET) throw new RedactionBudgetExceeded();
  work.charged += units;
  if (work.charged > REDACTION_WORK_BUDGET) throw new RedactionBudgetExceeded();
}

/** Redacts every string (and member name) of a payload against ONE shared work counter, so a payload's total
 *  charged work is bounded, not each string's. Once the budget is spent, each further string that needs a charged
 *  comparison is withheld whole; strings needing none pass through. */
function redactPayloadWithin(payload: Record<string, unknown>,
  within: (s: string, work: RedactionWork) => string): Record<string, unknown> {
  const work: RedactionWork = { charged: 0 };
  try {
    return redactStrings(payload, (s) => {
      try {
        return within(s, work);
      } catch (err) {
        if (err instanceof RedactionBudgetExceeded) return REDACTION_WITHHELD;
        throw err;
      }
    });
  } finally {
    workObserver?.(work);
  }
}

let workObserver: ((work: RedactionWork) => void) | undefined;

/** Test hook: called with the charged work of every budgeted redaction once it finishes (also on overflow).
 *  Pass undefined to stop observing. */
export function observeRedactionWork(observer: ((work: RedactionWork) => void) | undefined): void {
  workObserver = observer;
}

/** Runs `fn` with a fresh work counter; undefined when the budget was exceeded (fail closed: the caller
 *  withholds). `fn` must not itself return undefined. */
export function runWithinBudget<T>(fn: (work: RedactionWork) => T): T | undefined {
  const work: RedactionWork = { charged: 0 };
  try {
    return fn(work);
  } catch (err) {
    if (err instanceof RedactionBudgetExceeded) return undefined;
    throw err;
  } finally {
    workObserver?.(work);
  }
}

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
  const within = textRedactorWithin(secrets);
  if (within === undefined) return (s) => s;
  return (s) => runWithinBudget((work) => within(s, work)) ?? REDACTION_WITHHELD;
}

/** The budgeted core of makeTextRedactor (throws once `work` passes the budget); undefined when no secret is usable. */
function textRedactorWithin(secrets: Array<string | undefined | null>): ((s: string, work: RedactionWork) => string) | undefined {
  const list = usableSecrets(secrets);
  if (list.length === 0) return undefined;
  // With no invisible character in any secret the visible forms are the plain list itself.
  const stripped = list.some((secret) => INVISIBLE_TEST.test(secret));
  const visible = stripped ? list.map((secret) => secret.replace(INVISIBLE, "")).filter((v) => v.length >= MIN_SECRET_LEN) : list;
  if (list.length > PER_SECRET_MAX) {
    const find = literalSpanFinder(list);
    const findVisible = !stripped ? find : visible.length > 0 ? literalSpanFinder(visible) : undefined;
    return (s, work) => redactUnion(s, work, find, findVisible);
  }
  const findEach = spansOfEach(list);
  const findVisibleEach = visible.length === 0 ? undefined : !stripped ? findEach : spansOfEach(visible);
  return (s, work) => redactUnion(s, work, findEach, findVisibleEach);
}

/** Every span is found in the ORIGINAL string (verbatim, and across invisible separators) and the union is
 *  replaced once, so an overlapping or self-overlapping occurrence, or a verbatim one overlapping an
 *  invisible-separated one, never leaves part of a secret behind (#4688). */
function redactUnion(s: string, work: RedactionWork, find: SpanFinder, findVisible: SpanFinder | undefined): string {
  const spans = find(s, work);
  if (findVisible !== undefined && INVISIBLE_TEST.test(s)) collectInvisibleSpans(spans, s, findVisible, work);
  spans.sort((a, b) => a[0] - b[0]);
  return replaceSpans(s, spans);
}

/** The most secrets redacted one pass each; a longer list is matched in a single indexed pass. */
export const PER_SECRET_MAX = 64;

/** Finds the [start, end) spans of registered literals in a text. */
export type SpanFinder = (text: string, work: RedactionWork) => Array<[number, number]>;

/** A node of a bucket's compressed radix trie: the literal ending here (if any) and the edges below, keyed by the
 *  first UTF-16 unit of each edge's label. */
interface TrieNode {
  terminal: string | undefined;
  children: Map<number, TrieEdge> | undefined;
}
interface TrieEdge {
  label: string;
  node: TrieNode;
}

function newNode(terminal?: string): TrieNode {
  return { terminal, children: undefined };
}

/**
 * A mutable, reference-counted index of literals for single-pass matching. Every literal is at least
 * MIN_SECRET_LEN long, so the first MIN_SECRET_LEN UTF-16 units of a literal key a table: at each text position
 * the table is consulted once. The literals sharing a key form a compressed radix trie over their units after the
 * key, so one walk compares each edge label natively and the cost per position is bounded by the longest
 * literal, not by the number of literals sharing the key. Adding a literal twice and removing it once keeps it;
 * add and remove touch only that literal's key bucket, and the trie shape depends only on the live set.
 */
export class LiteralIndex {
  private readonly counts = new Map<string, number>();
  private readonly buckets = new Map<string, TrieNode>();
  private readonly firstUnit = new Uint32Array(0x10000);

  /** The distinct literals held. */
  get size(): number {
    return this.counts.size;
  }

  add(literal: string): void {
    if (literal.length < MIN_SECRET_LEN) return;
    const held = this.counts.get(literal);
    this.counts.set(literal, (held ?? 0) + 1);
    if (held !== undefined) return;
    const key = literal.slice(0, MIN_SECRET_LEN);
    let node: TrieNode = this.buckets.get(key) ?? newNode();
    this.buckets.set(key, node);
    let pos = MIN_SECRET_LEN;
    for (;;) {
      if (pos === literal.length) {
        node.terminal = literal;
        break;
      }
      const unit = literal.charCodeAt(pos);
      const edge: TrieEdge | undefined = node.children?.get(unit);
      if (edge === undefined) {
        (node.children ??= new Map()).set(unit, { label: literal.slice(pos), node: newNode(literal) });
        break;
      }
      const label = edge.label;
      let k = 1;
      const max = Math.min(label.length, literal.length - pos);
      while (k < max && label.charCodeAt(k) === literal.charCodeAt(pos + k)) k++;
      if (k < label.length) {
        const mid = newNode();
        mid.children = new Map([[label.charCodeAt(k), { label: label.slice(k), node: edge.node }]]);
        edge.label = label.slice(0, k);
        edge.node = mid;
      }
      node = edge.node;
      pos += k;
    }
    const first = literal.charCodeAt(0);
    this.firstUnit[first] = (this.firstUnit[first] ?? 0) + 1;
  }

  remove(literal: string): void {
    const held = this.counts.get(literal);
    if (held === undefined) return;
    if (held > 1) {
      this.counts.set(literal, held - 1);
      return;
    }
    this.counts.delete(literal);
    const key = literal.slice(0, MIN_SECRET_LEN);
    const root = this.buckets.get(key);
    if (root !== undefined) {
      const path: Array<{ parent: TrieNode; unit: number; edge: TrieEdge }> = [];
      let node = root;
      let pos = MIN_SECRET_LEN;
      // The walk trusts edge lengths without comparing labels: `counts` held this literal, so its exact path exists.
      while (pos < literal.length) {
        const unit = literal.charCodeAt(pos);
        const edge = node.children?.get(unit);
        if (edge === undefined) break;
        path.push({ parent: node, unit, edge });
        pos += edge.label.length;
        node = edge.node;
      }
      if (pos === literal.length && node.terminal === literal) {
        node.terminal = undefined;
        // Prune empty leaves, then merge a non-terminal single-child node into its child.
        while (path.length > 0 && node.terminal === undefined && node.children === undefined) {
          const { parent, unit } = path.pop()!;
          parent.children!.delete(unit);
          if (parent.children!.size === 0) parent.children = undefined;
          node = parent;
        }
        const last = path[path.length - 1];
        if (last !== undefined && node.terminal === undefined && node.children !== undefined && node.children.size === 1) {
          const [child] = node.children.values();
          last.edge.label += child!.label;
          last.edge.node = child!.node;
        }
        if (root.terminal === undefined && root.children === undefined) this.buckets.delete(key);
      }
    }
    const first = literal.charCodeAt(0);
    this.firstUnit[first] = (this.firstUnit[first] ?? 1) - 1;
  }

  /** The [start, end) span of the longest literal at each position where one occurs: sorted by start, and
   *  possibly overlapping (replaceSpans merges them). Throws once `work` passes the budget. */
  find(text: string, work: RedactionWork): Array<[number, number]> {
    const spans: Array<[number, number]> = [];
    this.scan(text, work, true, (literal, at) => spans.push([at, at + literal.length]));
    return spans;
  }

  /** Every literal occurring in the text, not only the longest at a position. Throws once `work` passes the budget. */
  present(text: string, work: RedactionWork): Set<string> {
    const found = new Set<string>();
    this.scan(text, work, false, (literal) => found.add(literal));
    return found;
  }

  private scan(text: string, work: RedactionWork, longestOnly: boolean, hit: (literal: string, at: number) => void): void {
    const last = text.length - MIN_SECRET_LEN;
    for (let i = 0; i <= last; i++) {
      if (this.firstUnit[text.charCodeAt(i)] === 0) continue;
      charge(work, 1);
      const start = this.buckets.get(text.slice(i, i + MIN_SECRET_LEN));
      if (start === undefined) continue;
      let node: TrieNode = start;
      let best = node.terminal;
      if (!longestOnly && best !== undefined) hit(best, i);
      let j = i + MIN_SECRET_LEN;
      while (node.children !== undefined) {
        const edge = node.children.get(text.charCodeAt(j));
        if (edge === undefined) break;
        // The comparison may touch every unit of the label: charge it before it runs.
        charge(work, edge.label.length);
        if (!text.startsWith(edge.label, j)) break;
        j += edge.label.length;
        node = edge.node;
        if (node.terminal !== undefined) {
          best = node.terminal;
          if (!longestOnly) hit(best, i);
        }
      }
      if (longestOnly && best !== undefined) hit(best, i);
    }
  }
}

function indexOf(literals: Iterable<string>): LiteralIndex {
  const index = new LiteralIndex();
  for (const literal of literals) index.add(literal);
  return index;
}

/** A single-pass finder for many literals: the longest literal at each position (see {@link LiteralIndex}). */
function literalSpanFinder(literals: readonly string[]): SpanFinder {
  const index = indexOf(new Set(literals));
  return (text, work) => index.find(text, work);
}

/** One pass over a text that returns every one of `literals` occurring in it (not only the longest at a
 *  position), so a literal that is a prefix of another is still reported. Over the work budget it fails closed
 *  and reports EVERY literal as present, so each scan verdict built on it withholds. */
export function literalsPresent(literals: readonly string[]): (text: string) => Set<string> {
  const all = new Set(literals);
  const index = indexOf(all);
  return (text) => runWithinBudget((work) => index.present(text, work)) ?? new Set(all);
}

/** `text` with every span (sorted by start, possibly overlapping) replaced by the redaction marker. */
export function replaceSpans(text: string, spans: Array<[number, number]>): string {
  if (spans.length === 0) return text;
  let out = "";
  let cursor = 0;
  for (const [start, end] of spans) {
    if (start >= cursor) out += text.slice(cursor, start) + REDACTED;
    cursor = Math.max(cursor, end);
  }
  return out + text.slice(cursor);
}

/** The prefix function of `s`: fail[i] is the length of the longest proper prefix of s[0..i] that is also its
 *  suffix. A proper prefix of `s` that is also its suffix (a border) exists only if fail[s.length - 1] > 0, and only
 *  then can two occurrences of `s` overlap. */
function prefixFunction(s: string): Int32Array {
  const fail = new Int32Array(s.length);
  let k = 0;
  for (let i = 1; i < s.length; i++) {
    while (k > 0 && s.charCodeAt(i) !== s.charCodeAt(k)) k = fail[k - 1]!;
    if (s.charCodeAt(i) === s.charCodeAt(k)) k++;
    fail[i] = k;
  }
  return fail;
}

/** The finder that looks for each secret (at least MIN_SECRET_LEN long): `indexOf` of its first MIN_SECRET_LEN units
 *  finds a candidate, and the full comparison is charged, so a long secret against periodic text cannot spend more
 *  than the budget. Overlapping occurrences of one secret are reported too (replaceSpans merges them): an occurrence
 *  starting inside a hit must start at a shift that leaves a border of the secret, so after a hit only those border
 *  shifts are tried (each charged) and the search resumes at the hit's end. A miss resumes one unit later. */
export function spansOfEach(secrets: readonly string[]): SpanFinder {
  return (text, work) => {
    const spans: Array<[number, number]> = [];
    for (const secret of secrets) {
      const prefix = secret.slice(0, MIN_SECRET_LEN);
      let fail: Int32Array | undefined;
      let at = text.indexOf(prefix);
      while (at !== -1) {
        charge(work, 1 + secret.length);
        let next = at + 1;
        if (text.startsWith(secret, at)) {
          const end = at + secret.length;
          spans.push([at, end]);
          if (fail === undefined) {
            charge(work, secret.length);
            fail = prefixFunction(secret);
          }
          // Another occurrence starting inside this one must start at a shift that leaves a border of the
          // secret, so only those shifts are tried (nearest first), and only while the occurrence still fits.
          // text[at + shift ..< end) is already known to match, so one unit at `end` gates the full comparison.
          for (let b = fail[secret.length - 1]!; b > 0; b = fail[b - 1]!) {
            const start = end - b;
            if (start + secret.length > text.length) break;
            charge(work, 1);
            if (text.charCodeAt(end) !== secret.charCodeAt(b)) continue;
            charge(work, secret.length);
            if (text.startsWith(secret, start)) spans.push([start, start + secret.length]);
          }
          next = end;
        }
        at = text.indexOf(prefix, next);
      }
    }
    return spans.sort((a, b) => a[0] - b[0]);
  };
}

/**
 * Push the spans of each secret wherever its characters appear in order with only invisible runs between
 * them. The text is projected onto its visible characters (remembering where each came from),
 * matched there, and every match is mapped back to a span of the ORIGINAL, invisible separators
 * included, so nothing of the secret survives to be re-joined.
 */
function collectInvisibleSpans(spans: Array<[number, number]>, text: string, find: SpanFinder, work: RedactionWork): void {
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

  // Pushed one by one: a spread of a very large span list overflows the call-argument limit.
  for (const [from, to] of find(visible, work)) spans.push([origin[from]!, origin[to - 1]! + 1]);
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
  const within = textRedactorWithin(secrets);
  if (within === undefined) return (payload) => payload;
  return (payload) => redactPayloadWithin(payload, within);
}

/**
 * A growable secret registry (issue #4201). An in-place credential switch publishes a NEW OAuth
 * token mid-run, after the flight's redactors were built, so the flight needs redactors that read
 * the current list. `text` and `payload` are stable function identities (safe to hand out once)
 * that always scrub every secret added so far. The same 8-char floor and invisible-run handling as
 * makeTextRedactor / makeRedactor apply; `add` is idempotent. A secret is never removed.
 */
export class SecretSet {
  private readonly list: string[] = [];
  private readonly held = new Set<string>();
  /** Built when the list first passes PER_SECRET_MAX, then kept current by every add. */
  private plain: LiteralIndex | undefined;
  /** The secrets with their invisible characters removed; absent while no secret holds one (the plain list
   *  and index then serve for the visible match too). */
  private visible: { list: string[]; index: LiteralIndex | undefined } | undefined;
  readonly text: TextRedactor = (s) => this.redactText(s);
  readonly payload: PayloadRedactor = (payload) =>
    this.list.length === 0 ? payload : redactPayloadWithin(payload, (s, work) => this.redactWithin(s, work));

  constructor(initial: Array<string | null | undefined>) {
    for (const secret of usableSecrets(initial)) this.append(secret);
  }

  add(secret: string): void {
    if (typeof secret !== "string") return;
    this.append(secret);
  }

  /** The usable secrets added so far (a copy). */
  values(): readonly string[] {
    return [...this.list];
  }

  private append(secret: string): void {
    if (secret.length < MIN_SECRET_LEN || this.held.has(secret)) return;
    this.held.add(secret);
    this.list.push(secret);
    this.plain?.add(secret);
    if (this.visible) {
      this.addVisible(this.visible, secret);
    } else if (INVISIBLE_TEST.test(secret)) {
      this.visible = { list: [], index: undefined };
      for (const s of this.list) this.addVisible(this.visible, s);
    }
  }

  private addVisible(visible: { list: string[]; index: LiteralIndex | undefined }, secret: string): void {
    const v = secret.replace(INVISIBLE, "");
    if (v.length < MIN_SECRET_LEN) return;
    visible.list.push(v);
    visible.index?.add(v);
  }

  private redactText(s: string): string {
    if (this.list.length === 0) return s;
    return runWithinBudget((work) => this.redactWithin(s, work)) ?? REDACTION_WITHHELD;
  }

  private redactWithin(s: string, work: RedactionWork): string {
    const list = this.list;
    const indexed = list.length > PER_SECRET_MAX;
    const plain = indexed ? (this.plain ??= indexOf(list)) : undefined;
    const find: SpanFinder = plain ? (t, w) => plain.find(t, w) : spansOfEach(list);
    const visible = this.visible;
    let findVisible: SpanFinder | undefined;
    if (visible === undefined) {
      findVisible = find;
    } else if (visible.list.length === 0) {
      findVisible = undefined;
    } else if (indexed) {
      const index = (visible.index ??= indexOf(visible.list));
      findVisible = (t, w) => index.find(t, w);
    } else {
      findVisible = spansOfEach(visible.list);
    }
    return redactUnion(s, work, find, findVisible);
  }
}
