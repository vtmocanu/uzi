import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router-dom";
import { listUserDocs } from "../lib/docs";
import { searchDocs, MIN_QUERY_LENGTH, type SearchResult } from "../lib/docsearch";
import { Card, Input } from "../components/ui";

const SEARCH_INPUT_ID = "docs-search";

// Split a snippet into text and <mark> nodes from snippet-relative, sorted,
// non-overlapping ranges — a straight fold, no nested marks. React elements
// only; never dangerouslySetInnerHTML. `mark` is explicitly styled (bg-warn/25
// text-fg) because the UA default is opaque yellow, unusable on the dark theme.
function Highlighted({ text, ranges }: { text: string; ranges: [number, number][] }) {
  if (ranges.length === 0) return <>{text}</>;
  const parts: ReactNode[] = [];
  let pos = 0;
  ranges.forEach(([start, end], i) => {
    if (start > pos) parts.push(text.slice(pos, start));
    parts.push(
      <mark key={i} className="rounded bg-warn/25 text-fg">
        {text.slice(start, end)}
      </mark>,
    );
    pos = end;
  });
  if (pos < text.length) parts.push(text.slice(pos));
  return <>{parts}</>;
}

function DocCard({ title, slug, children }: { title: string; slug: string; children?: ReactNode }) {
  return (
    <Link to={`/docs/${slug}`} className="block">
      <Card className="transition-colors hover:border-edge-strong">
        <h2 className="font-medium text-fg">{title}</h2>
        {children}
      </Card>
    </Link>
  );
}

// Public docs index: the `audience: user` howtos, ordered by frontmatter
// `order`, with a client-side full-text search box on top. Adding a page never
// touches this file — both the list and the search corpus are driven by the
// bundled docs' frontmatter.
export function Docs() {
  const docs = listUserDocs();
  const [query, setQuery] = useState("");
  const searching = query.trim().length >= MIN_QUERY_LENGTH;
  const results = useMemo<SearchResult[]>(() => (searching ? searchDocs(query) : []), [query, searching]);

  // `/` focuses the box from anywhere on the index (unless already typing in a
  // field); `Escape` clears the query.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") {
        setQuery("");
        return;
      }
      if (e.key === "/") {
        const el = document.activeElement as HTMLElement | null;
        const tag = el?.tagName;
        if (tag === "INPUT" || tag === "TEXTAREA" || el?.isContentEditable) return;
        e.preventDefault();
        document.getElementById(SEARCH_INPUT_ID)?.focus();
      }
    }
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Docs</h1>
        <p className="mt-1 text-muted">
          Howtos for getting from nothing to a working board.
        </p>
      </div>

      <Input
        id={SEARCH_INPUT_ID}
        type="search"
        aria-label="Search docs"
        placeholder="Search docs…"
        value={query}
        onChange={(e) => setQuery(e.target.value)}
      />

      {searching ? (
        <div aria-live="polite" className="space-y-3">
          {results.length === 0 ? (
            <Card>
              <p className="text-sm text-faint">No docs match “{query.trim()}”.</p>
            </Card>
          ) : (
            <>
              <p className="text-sm text-faint">
                {results.length === 1 ? "1 doc matches" : `${results.length} docs match`}
              </p>
              <ul className="space-y-3">
                {results.map(({ doc, snippet, ranges }) => (
                  <li key={doc.slug}>
                    <DocCard title={doc.meta.title || doc.slug} slug={doc.slug}>
                      {snippet && (
                        <p className="mt-1 text-sm text-muted">
                          <Highlighted text={snippet} ranges={ranges} />
                        </p>
                      )}
                    </DocCard>
                  </li>
                ))}
              </ul>
            </>
          )}
        </div>
      ) : docs.length === 0 ? (
        <Card>
          <p className="text-sm text-faint">No howtos published yet.</p>
        </Card>
      ) : (
        <ul className="space-y-3">
          {docs.map((doc) => (
            <li key={doc.slug}>
              <DocCard title={doc.meta.title || doc.slug} slug={doc.slug}>
                {doc.summary && <p className="mt-1 text-sm text-muted">{doc.summary}</p>}
              </DocCard>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
