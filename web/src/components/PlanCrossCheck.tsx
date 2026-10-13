import { useState } from "react";
import { Link } from "react-router-dom";
import type { Run, PlanCrossCheckSummary } from "../lib/apiTypes";
import { costDisplay, costHeadline } from "../lib/costStatus";
import { formatTokens } from "../lib/formatTokens";
import { isTerminalRun } from "../lib/runStatus";
import { stripUnsafeChars } from "../lib/safeText";
import { Markdown } from "./Markdown";
import { Badge, Card, CardHeader, type BadgeTone } from "./ui";

const reasons: Record<string, string> = {
  revise: "Changes requested", block: "Blocked",
  timed_out: "Timed out (verdict deadline)", model_timeout: "Timed out (checker model timeout)",
  checker_unavailable: "Checker unavailable", interrupted: "Interrupted",
  malformed: "Malformed checker response", model_error: "Checker model error",
  confinement_failed: "Checker confinement failed", superseded: "Superseded",
  checker_failed: "Checker failed",
  approved_not_stored: "Approved plan not stored", revisions_exhausted: "Revisions exhausted",
  codex_lead_unsupported: "Not yet supported for a Codex lead",
  planning_diff_refused: "Planning diff refused", candidate_refused: "Candidate refused",
};

const diffRefusals: Record<string, string> = {
  base_unavailable: "Base unavailable", diff_failed: "Diff failed", diff_too_large: "Diff too large",
  too_many_untracked: "Too many untracked files", secret_detected: "Secret detected",
  scan_failed: "Secret scan failed", unsupported_entry: "Unsupported entry (symlink, submodule or special file)",
};

function diffRefusalLabel(code: unknown): string {
  return typeof code === "string" && Object.prototype.hasOwnProperty.call(diffRefusals, code) ? diffRefusals[code] : "Reason unavailable";
}

function reasonLabel(reason: unknown): string | undefined {
  return typeof reason === "string" && Object.prototype.hasOwnProperty.call(reasons, reason) ? reasons[reason] : undefined;
}

function planCheckOutcome(verdict: unknown, reason: unknown): string {
  if (verdict === "pending" && (reason == null || reason === "")) return "Pending";
  if (verdict === "approve" && reason === "approve") return "Passed";
  if ((verdict === "revise" || verdict === "block") && reason === verdict) return reasons[verdict];
  if (verdict === "failed" && typeof reason === "string" && reasonLabel(reason) && reason !== "revise" && reason !== "block") return reasons[reason];
  return "Outcome unavailable";
}

function record(value: unknown): Record<string, unknown> {
  return value && typeof value === "object" ? value as Record<string, unknown> : {};
}
function plain(value: unknown, limit = 200): string {
  return typeof value === "string" ? stripUnsafeChars(value.slice(0, limit)).replace(/[\n\t]/g, " ") : "";
}
function childId(value: unknown): value is string {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}

function safeRound(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 1 && value <= 5 ? value : undefined;
}

export function planCheckEventText(payload: unknown, detail?: PlanCrossCheckSummary): string {
  const p = record(payload);
  const explicitRound = safeRound(p.round);
  const eventRound = p.round === undefined ? 1 : explicitRound;
  const latestRound = safeRound(detail?.round);
  const earlier = eventRound !== undefined && latestRound !== undefined
    && (eventRound < latestRound || (eventRound === latestRound && detail?.historical === true));
  const identity = explicitRound === undefined ? "" : ` (round ${explicitRound})`;
  return `Plan cross-check${identity} of ${earlier ? "earlier-plan" : "checked"} candidate: ${planCheckOutcome(p.verdict, p.reason_class)}`;
}

// Parsing is capped at 16,384 source characters across at most 20 items.
// Omitted evidence is disclosed.
function Findings({ value }: { value: unknown }) {
  const [open, setOpen] = useState(false);
  const findings = record(value);
  let remaining = 16384;
  let truncated = false;
  const bounded = (value: unknown) => {
    if (typeof value !== "string") return "";
    const text = value.slice(0, remaining);
    remaining -= text.length;
    if (text.length < value.length) truncated = true;
    return text;
  };
  const summary = bounded(findings.summary);
  const items = Array.isArray(findings.items) ? findings.items : [];
  if (items.length > 20) truncated = true;
  const content = items.slice(0, 20).map((item) => {
    const f = record(item);
    const [file, severity, summary, rationale] = [f.file, f.severity, f.summary, f.rationale].map(bounded);
    return { file: stripUnsafeChars(file), severity: stripUnsafeChars(severity), summary, rationale };
  });
  const tones: Record<string, BadgeTone> = { info: "info", warning: "warning", error: "danger" };
  const rows = content.map((f, i) => {
    const known = Object.prototype.hasOwnProperty.call(tones, f.severity);
    return <li key={i} className="flex flex-col gap-1.5 px-3 py-2.5 sm:flex-row sm:items-start sm:gap-3">
      <span className="shrink-0 sm:w-20"><Badge wrap tone={known ? tones[f.severity] : "neutral"}>{known ? f.severity : "Severity unavailable"}</Badge></span>
      <div className="min-w-0 flex-1 space-y-1">
        {f.file && <code className="rounded bg-raised px-1.5 py-0.5 font-mono text-xs break-all">{f.file}</code>}
        <div className="judge-prose break-words"><Markdown content={f.summary} /></div>
        <div className="judge-prose break-words text-muted"><Markdown content={f.rationale} /></div>
      </div>
    </li>;
  });
  return <div className="space-y-2">
    <div className="judge-prose break-words"><Markdown content={summary} /></div>
    {rows.length > 0 && <ul className="divide-y divide-edge rounded-md border border-edge">{rows.slice(0, 3)}</ul>}
    {rows.length > 3 && <details open={open} onToggle={e => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer rounded-sm text-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60">{open ? "Hide" : "Show"} {rows.length - 3} more findings</summary>
      <ul className="divide-y divide-edge rounded-md border border-edge">{rows.slice(3)}</ul>
    </details>}
    {truncated && <p className="text-muted">Findings truncated for display.</p>}
  </div>;
}

export function PlanCrossCheckEvent({ payload, detail }: { payload: unknown; detail?: PlanCrossCheckSummary }) {
  const p = record(payload);
  return <section aria-label="Plan cross-check event" className="rounded-xl border-edge bg-surface">
    <Card className="text-sm space-y-2">
      <CardHeader title={planCheckEventText(payload, detail)} actions={
        childId(p.checker_run_id) && <Link className="text-info underline underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60" to={`/runs/${p.checker_run_id}`}>Open checker run</Link>
      } />
      <Findings value={p.findings} />
    </Card>
  </section>;
}

function sourceLabel(source: unknown): string {
  return source === "pin" || source === "worker default" ? source : "unknown";
}

export function PlanCrossCheck({ run, presentation = "standalone" }: { run: Run; presentation?: "standalone" | "inset" }) {
  const s = run.plan_cross_check_summary;
  const gate = run.plan_cross_check_gate_reason;
  if (!s && !gate && !run.plan_cross_check_required) return null;
  const outcome = s ? planCheckOutcome(s.verdict, s.reason_class)
    : run.plan_cross_check_required ? isTerminalRun(run.status) ? "Not recorded" : "Pending" : "Outcome unavailable";
  const usage = s?.usage;
  const cost = usage?.cost_status === "subscription" ? costHeadline(costDisplay("subscription", 0))
    : usage && typeof usage.cost_usd === "number" && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0
    ? costHeadline(costDisplay(usage.cost_status, usage.cost_usd)) : "Unavailable";
  const content = <>
    <CardHeader title="Plan cross-check" level={presentation === "inset" ? 3 : 2}
      badge={<Badge>{s?.historical ? "Superseded" : outcome}</Badge>}
      subtitle={s?.historical ? "Earlier-plan evidence" : undefined} actions={
      childId(s?.checker_run_id) && <Link className="text-info underline underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60" to={`/runs/${s.checker_run_id}`}>Open checker run</Link>
    } />
    {gate && <p>Current gate: {reasonLabel(gate) ?? "Reason unavailable"}</p>}
    {gate === "planning_diff_refused" && run.plan_cross_check_diff_refusal != null && <p>Refusal: {diffRefusalLabel(run.plan_cross_check_diff_refusal)}</p>}
    {s && <p>{`${s.historical ? "Earlier-plan evidence" : "Checked candidate"}: ${planCheckOutcome(s.verdict, s.reason_class)}`}</p>}
    {!s && run.plan_cross_check_required && <p>{isTerminalRun(run.status) ? "This run ended without a plan cross-check result." : "Runs when the plan is ready, before it can proceed."}</p>}
    {s && outcome === "Pending" && <p>No result recorded yet.</p>}
    {s?.historical && <p>These findings concern an earlier plan; they do not certify the current plan.</p>}
    {s && <>
      <Findings value={s.findings} />
      <p className="flex flex-wrap gap-x-1 break-all text-xs text-muted">
        <span>Model: {plain(s.checker_model) || "Unreported"} ({sourceLabel(s.checker_model_source)})</span>
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Effort: {plain(s.checker_effort) || "Unreported"} ({sourceLabel(s.checker_effort_source)})</span></span>
        {usage && <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checker tokens: {Number.isFinite(usage.input_tokens) && usage.input_tokens >= 0 ? formatTokens(usage.input_tokens) : "Unreported"} in · {Number.isFinite(usage.output_tokens) && usage.output_tokens >= 0 ? formatTokens(usage.output_tokens) : "Unreported"} out</span></span>}
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checker cost: {cost}{usage?.cost_status === "subscription" ? " · no cost estimate recorded" : cost === "Unavailable" ? " · cost unavailable" : " · API-equivalent"}</span></span>
      </p>
    </>}
  </>;
  return presentation === "inset"
    ? <section aria-label="Plan cross-check" className="rounded-lg border border-edge bg-surface p-4 text-sm space-y-2">{content}</section>
    : <section aria-label="Plan cross-check" className="rounded-xl border-edge bg-surface"><Card className="text-sm space-y-2">{content}</Card></section>;
}
