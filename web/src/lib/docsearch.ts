// Client-side full-text search over the bundled `audience: user` docs. The
// docs viewer already ships every page body as a raw string (see docs.ts), so
// search needs no API, no service, and no new dependency — 11 short pages held
// in memory, scanned with plain substring loops.
//
// The pure core (`buildIndex` + `searchIndex`) is decoupled from the real corpus
// the same way docs.ts splits `resolveHref` (pure) from `rewriteHref` (bound):
// tests drive `searchIndex` with synthetic fixtures, `searchDocs` binds it to
// `listUserDocs()`.

import { listUserDocs, type Doc } from "./docs";

// A query shorter than this shows the normal index instead of searching — a
// single character matches almost everything and is never a real query. Applied
// both to the whole query and to each token.
export const MIN_QUERY_LENGTH = 2;

// Defensive upper bound: a query past this is truncated before any scanning. No
// real docs query approaches it; the cap only bounds pathological input.
const MAX_QUERY_LENGTH = 256;

// ~160-char snippet window centered on the first body match, with some context
// ahead of the match so the matched term does not sit flush against the `…`.
const SNIPPET_WINDOW = 160;
const SNIPPET_LEAD = 60;

export interface IndexedDoc {
  doc: Doc;
  /** Stripped body in original case — the source of snippet text. */
  plainBody: string;
  /** Lowercased fields for case-insensitive substring matching/counting. */
  titleLower: string;
  headingsLower: string;
  bodyLower: string;
}

export interface SearchResult {
  doc: Doc;
  /** Plain-text excerpt (body window, or the doc's summary for a title-only hit). */
  snippet: string;
  /** Snippet-relative, sorted, merged/non-overlapping match ranges for `<mark>`ing. */
  ranges: [number, number][];
}

// Reduce inline markdown to its text: links/images to their label (same regex
// as summarize()), then drop emphasis/backtick markers. Asterisks and backticks
// go wholesale, but only word-boundary underscores are treated as emphasis —
// intra-word underscores are kept so identifiers stay searchable (`_` is a
// regex word char, so `\b_` never fires inside `UZI_WORKER_TOKEN`).
function stripInline(s: string): string {
  return s
    .replace(/!?\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/[*`]/g, "")
    .replace(/\b_+|_+\b/g, "");
}

function isTableSeparatorRow(line: string): boolean {
  // A GFM alignment row: pipes, dashes, colons and spaces only, with a dash.
  return /-/.test(line) && /^\s*\|?[\s:|-]+\|[\s:|-]*$/.test(line);
}

// Strip a doc body to searchable plain text and collect its heading texts.
// Fenced code is kept verbatim as text (users search for commands like
// `docker compose --profile agent up`); fence delimiters and table alignment
// rows are dropped; heading markers are dropped but their text is retained
// (also returned separately, for ranking). A `#` inside a code fence is code,
// not a heading, so fence state is tracked in a single pass.
export function stripDocBody(body: string): { plainBody: string; headings: string[] } {
  const headings: string[] = [];
  const parts: string[] = [];
  let inFence = false;
  for (const raw of body.split("\n")) {
    if (/^\s*(```|~~~)/.test(raw)) {
      inFence = !inFence;
      continue; // drop the fence delimiter line
    }
    if (inFence) {
      parts.push(raw); // code content, kept as-is so commands stay searchable
      continue;
    }
    const heading = /^\s{0,3}(#{1,6})\s+(.*)$/.exec(raw);
    if (heading) {
      const text = stripInline(heading[2]).trim();
      if (text) {
        headings.push(text);
        parts.push(text);
      }
      continue;
    }
    if (isTableSeparatorRow(raw)) continue;
    parts.push(stripInline(raw.replace(/\|/g, " ")));
  }
  const plainBody = parts.join(" ").replace(/\s+/g, " ").trim();
  return { plainBody, headings };
}

export function buildIndex(docs: Doc[]): IndexedDoc[] {
  return docs.map((doc) => {
    const { plainBody, headings } = stripDocBody(doc.body);
    return {
      doc,
      plainBody,
      titleLower: doc.meta.title.toLowerCase(),
      headingsLower: headings.join(" \n ").toLowerCase(),
      bodyLower: plainBody.toLowerCase(),
    };
  });
}

// Count non-overlapping occurrences of `needle` in `haystack` with an indexOf
// loop — never `new RegExp(needle)`, since query tokens are user input full of
// regex metacharacters by design (`.env`, `--profile`, `c++`).
function countOccurrences(haystack: string, needle: string): number {
  if (needle === "") return 0;
  let count = 0;
  for (let i = haystack.indexOf(needle); i !== -1; i = haystack.indexOf(needle, i + needle.length)) {
    count++;
  }
  return count;
}

// Collect every occurrence of every token in `text` (case-insensitive) as
// snippet-relative ranges, then sort and merge overlapping/touching ranges so
// the UI's split-and-mark is a straight fold with no nested `<mark>`s (e.g.
// `work` + `worker` collapse into one range).
function collectRanges(text: string, tokens: string[]): [number, number][] {
  const lower = text.toLowerCase();
  const ranges: [number, number][] = [];
  for (const token of tokens) {
    for (let i = lower.indexOf(token); i !== -1; i = lower.indexOf(token, i + token.length)) {
      ranges.push([i, i + token.length]);
    }
  }
  ranges.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
  const merged: [number, number][] = [];
  for (const [start, end] of ranges) {
    const last = merged[merged.length - 1];
    if (last && start <= last[1]) last[1] = Math.max(last[1], end);
    else merged.push([start, end]);
  }
  return merged;
}

// A ~160-char window of `body` centered on `at`, trimmed to word boundaries and
// bracketed with `…` where content was cut.
function windowAround(body: string, at: number): string {
  let start = Math.max(0, at - SNIPPET_LEAD);
  let end = Math.min(body.length, start + SNIPPET_WINDOW);
  start = Math.max(0, end - SNIPPET_WINDOW); // re-extend left when the tail is short
  const cutLeft = start > 0;
  const cutRight = end < body.length;
  if (cutLeft) {
    const ws = body.indexOf(" ", start);
    if (ws !== -1 && ws < at) start = ws + 1;
  }
  if (cutRight) {
    const ws = body.lastIndexOf(" ", end);
    if (ws !== -1 && ws > at) end = ws;
  }
  let text = body.slice(start, end).trim();
  if (cutLeft) text = `…${text}`;
  if (cutRight) text = `${text}…`;
  return text;
}

// The snippet for a result: a body window around the earliest body match, or the
// doc's existing summary when no token matches in the body (title/heading-only
// hit). Ranges are computed against whatever snippet is chosen, so a token that
// matches only in the title/headings simply contributes no range.
function buildSnippet(entry: IndexedDoc, tokens: string[]): { snippet: string; ranges: [number, number][] } {
  let firstMatch = -1;
  for (const token of tokens) {
    const idx = entry.bodyLower.indexOf(token);
    if (idx !== -1 && (firstMatch === -1 || idx < firstMatch)) firstMatch = idx;
  }
  const snippet = firstMatch === -1 ? entry.doc.summary : windowAround(entry.plainBody, firstMatch);
  return { snippet, ranges: collectRanges(snippet, tokens) };
}

// Pure search core. Tokenizes on whitespace, keeps only docs where EVERY token
// appears somewhere (title, headings, or body), and ranks
// title > heading > body, then by total occurrences, then slug.
export function searchIndex(index: IndexedDoc[], query: string): SearchResult[] {
  const trimmed = query.slice(0, MAX_QUERY_LENGTH).trim();
  if (trimmed.length < MIN_QUERY_LENGTH) return [];
  // Drop sub-MIN_QUERY_LENGTH tokens too, so "a b" does not search lone
  // characters that match almost every doc.
  const tokens = trimmed.toLowerCase().split(/\s+/).filter((t) => t.length >= MIN_QUERY_LENGTH);
  if (tokens.length === 0) return [];

  const scored: { entry: IndexedDoc; tier: number; total: number }[] = [];
  for (const entry of index) {
    const matchesAll = tokens.every(
      (t) => entry.titleLower.includes(t) || entry.headingsLower.includes(t) || entry.bodyLower.includes(t),
    );
    if (!matchesAll) continue;

    let titleHits = 0;
    let headingHits = 0;
    let bodyHits = 0;
    for (const t of tokens) {
      titleHits += countOccurrences(entry.titleLower, t);
      headingHits += countOccurrences(entry.headingsLower, t);
      bodyHits += countOccurrences(entry.bodyLower, t);
    }
    const tier = titleHits > 0 ? 3 : headingHits > 0 ? 2 : 1;
    scored.push({ entry, tier, total: titleHits + headingHits + bodyHits });
  }

  scored.sort(
    (a, b) => b.tier - a.tier || b.total - a.total || a.entry.doc.slug.localeCompare(b.entry.doc.slug),
  );

  return scored.map(({ entry }) => {
    const { snippet, ranges } = buildSnippet(entry, tokens);
    return { doc: entry.doc, snippet, ranges };
  });
}

// Built once — the corpus is fixed for the life of the bundle.
let cachedIndex: IndexedDoc[] | null = null;

// Bound to the docs actually bundled in this build (the same `audience: user`
// pages the index lists).
export function searchDocs(query: string): SearchResult[] {
  if (!cachedIndex) cachedIndex = buildIndex(listUserDocs());
  return searchIndex(cachedIndex, query);
}
