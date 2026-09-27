// PRD #1798 M7: the "Delivered" section of the run summary, the web read of the run's
// PUBLISHED plain-English PR description (Run.pr_description) and the PR's last
// description-write outcome (Run.pr_description_outcome).
//
// TRUST: every field is lead- or model-authored and UNTRUSTED. The api sanitized it FOR THE
// FORGE's markdown (api/internal/workersvc/pr_description_sanitize.go, step 3): every `<` is
// encoded as `&lt;`, block tokens, code-fence runs and every `[` / `]` are backslash-escaped, and
// a U+200B is inserted after `@` and inside closing keywords. This surface is NOT markdown:
// displayPrText undoes the two encodings (so `\[x\]` reads `[x]` and `&lt;b&gt;` reads `<b>`)
// and the result is rendered ONLY as React text nodes. Never <Markdown>, never
// dangerouslySetInnerHTML: once unescaped the text is exactly as hostile as the lead wrote it.

import { Badge, type BadgeTone } from "../../components/ui";
import type { PrDescriptionSize, PrDescriptionSizeBucket, Run, RunPrDescription } from "../../lib/api";

// The five entities an HTML-escaping pass can emit. The sanitizer itself only emits `&lt;`
// (it decodes every entity to a fixed point before encoding `<`), the rest are decoded so a
// stored value from any HTML-escaping writer still reads as its characters. One pass, so an
// `&amp;lt;` decodes to the literal text `&lt;` and never on to `<`.
const ENTITY = /&(lt|gt|amp|quot|#39|#x27|apos);/g;
const ENTITY_CHAR: Record<string, string> = {
  lt: "<",
  gt: ">",
  amp: "&",
  quot: '"',
  "#39": "'",
  "#x27": "'",
  apos: "'",
};

// A CommonMark backslash escape: a backslash before an ASCII punctuation character renders as
// that character. The sanitizer's escapes are all of this form (and a trailing odd backslash
// doubled to `\\`), so resolving them the CommonMark way reads exactly what the forge shows. A
// backslash before anything else (`C:\path`) is literal in CommonMark too and is kept.
const MD_ESCAPE = /\\([!-/:-@[-`{-~])/g;

// Cc + Cf except `\n`, `\t` (the safeText rule) AND U+200B: the zero-width space is the
// sanitizer's own breaker after `@` and inside closing keywords. It is invisible and harmless
// here, and keeping it means text copied out of this card still does not form a mention or a
// closing directive when pasted into a forge. Any other format rune (bidi overrides) goes.
const UNSAFE_EXCEPT_ZWSP = /(?![\n\t\u200B])[\p{Cc}\p{Cf}]/gu;

/** Undo the forge-markdown encodings the api sanitizer applied, for plain-text display. */
export function displayPrText(s: unknown): string {
  if (typeof s !== "string") return "";
  return s
    .replace(MD_ESCAPE, "$1")
    .replace(ENTITY, (_, name: string) => ENTITY_CHAR[name] ?? "")
    .replace(UNSAFE_EXCEPT_ZWSP, "")
    .trim();
}

const SIZE_BUCKETS = ["code", "tests", "docs", "config", "generated", "vendored"] as const;
const NUMBER_FORMAT = new Intl.NumberFormat("en-US");
const MINUS = "\u2212";
const SEPARATOR = " \u00B7 ";

function count(n: unknown): number {
  return typeof n === "number" && Number.isFinite(n) ? n : 0;
}

/**
 * PRD #1798 D3: the size line in the agent's format (agent/src/pr-size.ts renderSizeLine,
 * without its markdown bold), pinned to fixtures/pr-size-line/cases.json with the CLI's twin.
 * A bucket with no added and no deleted lines is omitted; `unavailable` reads
 * "Size: unavailable"; a size with no files renders no line (null), like the agent's empty diff.
 */
export function prSizeLine(size: PrDescriptionSize | null | undefined): string | null {
  if (!size) return null;
  if (size.unavailable) return "Size: unavailable";
  const files = count(size.files);
  if (files <= 0) return null;
  const parts: string[] = [];
  for (const name of SIZE_BUCKETS) {
    const b: PrDescriptionSizeBucket | undefined = size[name];
    const added = count(b?.added);
    const deleted = count(b?.deleted);
    if (added === 0 && deleted === 0) continue;
    parts.push(`${name} +${NUMBER_FORMAT.format(added)} ${MINUS}${NUMBER_FORMAT.format(deleted)}`);
  }
  parts.push(`${NUMBER_FORMAT.format(files)} ${files === 1 ? "file" : "files"}`);
  return `Size: ${parts.join(SEPARATOR)}`;
}

// The last write outcome, other than `published`, as one plain sentence. Mirrored by the CLI's
// prDescriptionOutcomeNote (api/cmd/uzi/run_render.go).
const OUTCOME_NOTE: Record<string, string> = {
  skipped_human_edit: "Last PR update skipped: a human edited the description.",
  skipped_no_region: "Last PR update skipped: the description no longer has a uzi section.",
  skipped_malformed: "Last PR update skipped: the uzi section of the description was damaged.",
  skipped_snapshot_moved: "Last PR update skipped: the branch moved before the update was written.",
  write_failed: "Last PR update failed: the forge did not accept the new description.",
};

/** The muted note for a non-published outcome, or null when there is nothing to say. */
export function prDescriptionOutcomeNote(outcome: string | null | undefined): string | null {
  if (typeof outcome !== "string" || outcome === "" || outcome === "published") return null;
  return OUTCOME_NOTE[outcome] ?? `Last PR update was not published (${displayPrText(outcome)}).`;
}

const SCOPE_KIND: Record<string, BadgeTone> = {
  added: "ok",
  changed: "info",
  dropped: "danger",
  deferred: "warning",
};

function strings(list: unknown): string[] {
  if (!Array.isArray(list)) return [];
  return list.map(displayPrText).filter((s) => s !== "");
}

const HEADING = "text-xs font-semibold uppercase tracking-wider text-faint";

/**
 * The "Delivered" section. Renders nothing when the run has no published description, so a
 * run without a PR (or a server predating the field) is unchanged. Lives inside RunSummary and
 * so shares its per-run collapse.
 */
export function DeliveredCard({ run }: { run: Run }) {
  const desc: RunPrDescription | null | undefined = run.pr_description;
  if (!desc || typeof desc !== "object") return null;
  const fields = desc.fields ?? ({} as Partial<RunPrDescription["fields"]>);
  const summary = displayPrText(fields.summary);
  const changes = strings(fields.changes);
  const pointers = strings(fields.review_pointers);
  const scopeNotes = (Array.isArray(fields.scope_notes) ? fields.scope_notes : [])
    .map((n) => ({ kind: displayPrText(n?.kind), text: displayPrText(n?.text) }))
    .filter((n) => n.text !== "");
  const verification = (Array.isArray(fields.verification) ? fields.verification : [])
    .map((v) => ({
      command: displayPrText(v?.command),
      result: displayPrText(v?.result),
      sha: displayPrText(v?.verified_at_sha).slice(0, 7),
    }))
    .filter((v) => v.command !== "");
  const sizeLine = prSizeLine(desc.size);
  const note = prDescriptionOutcomeNote(run.pr_description_outcome);

  return (
    <section className="space-y-3" aria-label="Delivered">
      <h3 className={HEADING}>Delivered</h3>
      {summary !== "" && <p className="whitespace-pre-wrap text-sm text-fg">{summary}</p>}
      {sizeLine && <p className="font-mono text-xs text-muted">{sizeLine}</p>}

      {changes.length > 0 && (
        <div className="space-y-1">
          <h4 className="text-xs font-medium text-muted">What changed</h4>
          <ul className="list-disc space-y-1 pl-5 text-sm text-fg">
            {changes.map((c, i) => (
              <li key={i}>{c}</li>
            ))}
          </ul>
        </div>
      )}

      {verification.length > 0 && (
        <div className="space-y-1">
          <h4 className="text-xs font-medium text-muted">Checks</h4>
          <ul className="space-y-1.5 text-sm">
            {verification.map((v, i) => (
              <li key={i} className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
                <Badge tone={v.result === "pass" ? "ok" : v.result === "fail" ? "danger" : "neutral"}>
                  {v.result || "unknown"}
                </Badge>
                <code className="break-all font-mono text-xs text-fg">{v.command}</code>
                {v.sha !== "" && <span className="text-xs text-faint">Reported by the agent at {v.sha}</span>}
              </li>
            ))}
          </ul>
        </div>
      )}

      {scopeNotes.length > 0 && (
        <div className="space-y-1">
          <h4 className="text-xs font-medium text-muted">Scope notes</h4>
          <ul className="space-y-1.5">
            {scopeNotes.map((n, i) => (
              <li key={i} className="flex items-start gap-2 text-sm">
                <Badge tone={SCOPE_KIND[n.kind] ?? "neutral"}>{n.kind || "note"}</Badge>
                <span className="min-w-0 flex-1 whitespace-pre-wrap text-muted">{n.text}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {pointers.length > 0 && (
        <div className="space-y-1">
          <h4 className="text-xs font-medium text-muted">Where to look first</h4>
          <ul className="list-disc space-y-1 pl-5 text-sm text-muted">
            {pointers.map((p, i) => (
              <li key={i}>{p}</li>
            ))}
          </ul>
        </div>
      )}

      {note && <p className="text-xs italic text-faint">{note}</p>}
    </section>
  );
}
