// PRD #1908 M7 (D-D): the job run's result panel. A job has no repo, branch or merge
// request; its deliverable is this panel: who asked for it, what it was given, and the
// report plus structured findings it returned.
//
// TRUST: every free-text field here is UNTRUSTED.
//  - origin.requested_by_label is an end-user label an external PRODUCT supplied. uzi never
//    verified it, so it is shown only inside an attribution sentence that names the product
//    as its source ("reported by <product>"), never as a bare statement uzi makes.
//  - result.report_md is agent-authored over caller-supplied inputs. It renders through the
//    hardened <Markdown> (no raw HTML, dangerous URL schemes neutralized, Cf/bidi stripped,
//    links open in a new tab with rel="noopener noreferrer").
//  - findings are rendered INERT: message_md is plain escaped text (never markdown), a URL
//    location becomes a link only for http/https, and file:line is plain text.
//  - input names, the result status and the product name go through stripUnsafeChars.

import { useId } from "react";
import { Markdown } from "../../components/Markdown";
import { Badge, Card, type BadgeTone } from "../../components/ui";
import type { Run, RunJob, RunJobFinding } from "../../lib/api";
import { jobTypeLabel } from "../../lib/jobTypes";
import { isTerminalRun } from "../../lib/runStatus";
import { stripUnsafeChars } from "../../lib/safeText";

const SEVERITY: Record<string, { tone: BadgeTone; label: string; one: string; many: string }> = {
  error: { tone: "danger", label: "Error", one: "error", many: "errors" },
  warning: { tone: "warning", label: "Warning", one: "warning", many: "warnings" },
  info: { tone: "info", label: "Info", one: "note", many: "notes" },
};

// httpUrl returns the parsed href for an http(s) URL and null for anything else (a
// javascript:/data:/file: scheme, a relative path, or an unparseable string), which the
// caller then renders as plain text.
function httpUrl(raw: string): string | null {
  let u: URL;
  try {
    u = new URL(raw);
  } catch {
    return null;
  }
  return u.protocol === "http:" || u.protocol === "https:" ? u.href : null;
}

function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(n < 10 * 1024 ? 1 : 0)} KiB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`;
}

function Origin({ origin }: { origin: RunJob["origin"] }) {
  const product = origin.product_name ? stripUnsafeChars(origin.product_name) : null;
  const label = origin.requested_by_label ? stripUnsafeChars(origin.requested_by_label).trim() : "";
  if (!label) {
    return (
      <p className="text-sm text-muted">
        {product ? (
          <>
            Created through <span className="font-medium text-fg">{product}</span>.
          </>
        ) : (
          "Created with a CLI token."
        )}
      </p>
    );
  }
  // The label is quoted and attributed in the same sentence, and the "not verified" note sits
  // beside it, so an external string can never read as uzi's own claim about who asked.
  return (
    <p className="border-l-2 border-info/50 pl-3 text-sm text-muted" data-testid="job-origin">
      Requested by <q className="font-medium text-fg">{label}</q>, as reported by{" "}
      {product ? <span className="font-medium text-fg">{product}</span> : "the caller"}.{" "}
      <span className="text-faint">uzi has not verified this.</span>
    </p>
  );
}

function FindingLocation({ finding }: { finding: RunJobFinding }) {
  if (finding.url) {
    const href = httpUrl(finding.url);
    const text = stripUnsafeChars(finding.url);
    return href ? (
      <a
        href={href}
        target="_blank"
        rel="noopener noreferrer"
        className="break-all text-info underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60"
      >
        {text}
      </a>
    ) : (
      <span className="break-all font-mono">{text}</span>
    );
  }
  if (finding.file) {
    return (
      <code className="break-all font-mono">
        {stripUnsafeChars(finding.file)}
        {finding.line != null ? `:${finding.line}` : ""}
      </code>
    );
  }
  return null;
}

function severityCounts(findings: RunJobFinding[]): string {
  const counts = new Map<string, number>();
  for (const f of findings) counts.set(f.severity, (counts.get(f.severity) ?? 0) + 1);
  return ["error", "warning", "info"]
    .filter((s) => counts.has(s))
    .map((s) => {
      const n = counts.get(s)!;
      return `${n} ${n === 1 ? SEVERITY[s].one : SEVERITY[s].many}`;
    })
    .join(", ");
}

export function JobResultPanel({ run }: { run: Run }) {
  const headingId = useId();
  const inputsId = useId();
  const job = run.job;
  if (run.kind !== "job" || !job) return null;
  const result = job.result;
  const terminal = isTerminalRun(run.status);
  const counts = result ? severityCounts(result.findings) : "";

  return (
    <section aria-labelledby={headingId}>
      <Card className="space-y-4 p-4">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 id={headingId} className="text-sm font-semibold text-fg">
            {jobTypeLabel(job.type)} job
          </h2>
          {result && (
            <Badge tone="neutral" title="The status the job reported with its result">
              {stripUnsafeChars(result.status)}
            </Badge>
          )}
        </div>

        <Origin origin={job.origin} />

        <div className="space-y-1.5">
          <h3 id={inputsId} className="text-xs font-semibold uppercase tracking-wider text-faint">
            Inputs
          </h3>
          {job.inputs.length === 0 ? (
            <p className="text-sm text-faint">No inputs; the job worked from its prompt alone.</p>
          ) : (
            <ul className="flex flex-wrap gap-1.5" aria-labelledby={inputsId}>
              {job.inputs.map((input, i) => (
                <li
                  key={`${i}-${input.name}`}
                  className="inline-flex items-center gap-2 rounded-md border border-edge bg-raised/50 px-2 py-1 text-xs"
                >
                  <span className="font-mono text-fg">{stripUnsafeChars(input.name)}</span>
                  <span className="tabular-nums text-faint">{formatBytes(input.size_bytes)}</span>
                </li>
              ))}
            </ul>
          )}
        </div>

        {!result ? (
          <p className="text-sm text-muted">
            {terminal
              ? "This job ended without returning a result."
              : "The report and findings appear here when the job finishes."}
          </p>
        ) : (
          <>
            <div className="space-y-1.5">
              <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">Report</h3>
              {result.report_md.trim() === "" ? (
                <p className="text-sm text-faint">The job returned no report text.</p>
              ) : (
                // Capped and scrollable (like the report-only findings card) so the activity
                // feed below stays reachable; tabIndex lets a keyboard user scroll it.
                <div
                  className="max-h-[32rem] overflow-auto rounded-md border border-edge bg-raised/30 px-3 py-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60"
                  tabIndex={0}
                  role="region"
                  aria-label="Job report"
                  data-testid="job-report"
                >
                  <Markdown content={result.report_md} />
                </div>
              )}
            </div>

            <div className="space-y-1.5">
              {/* The counts sit beside the heading, not inside it: nested, a screen reader read
                  the heading as one run-on word ("Findings1 error, …"). */}
              <div className="flex flex-wrap items-baseline gap-x-2">
                <h3 className="text-xs font-semibold uppercase tracking-wider text-faint">Findings</h3>
                {counts && (
                  <p className="text-xs text-faint" data-testid="job-findings-counts">
                    {counts}
                  </p>
                )}
              </div>
              {result.findings.length === 0 ? (
                <p className="text-sm text-faint">No findings.</p>
              ) : (
                <ul className="divide-y divide-edge rounded-md border border-edge" data-testid="job-findings">
                  {result.findings.map((f, i) => {
                    const sev = SEVERITY[f.severity];
                    return (
                      <li key={i} className="flex flex-col gap-1.5 px-3 py-2.5 sm:flex-row sm:items-start sm:gap-3">
                        <span className="shrink-0 sm:w-20">
                          <Badge tone={sev?.tone ?? "neutral"}>{sev?.label ?? stripUnsafeChars(f.severity)}</Badge>
                        </span>
                        <div className="min-w-0 flex-1 space-y-1">
                          {/* Inert: message_md is shown as its literal text, never parsed. */}
                          <p className="whitespace-pre-wrap break-words text-sm text-fg">
                            {stripUnsafeChars(f.message_md)}
                          </p>
                          {(f.url || f.file) && (
                            <p className="text-xs text-muted">
                              <FindingLocation finding={f} />
                            </p>
                          )}
                        </div>
                      </li>
                    );
                  })}
                </ul>
              )}
            </div>
          </>
        )}
      </Card>
    </section>
  );
}
