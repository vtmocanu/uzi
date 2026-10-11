import { Link } from "react-router-dom";
import type { Run, PlanCrossCheckSummary } from "../lib/apiTypes";
import { costDisplay, costHeadline } from "../lib/costStatus";
import { formatTokens } from "../lib/formatTokens";
import { stripUnsafeChars } from "../lib/safeText";
import { Markdown } from "./Markdown";

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
    return [f.file, f.severity, f.summary, f.rationale].map(bounded).filter(Boolean).join("\n\n");
  });
  return <div className="max-h-96 overflow-auto space-y-2">
    <Markdown content={summary} />
    {content.map((text, i) => <Markdown key={i} content={text} />)}
    {truncated && <p>Findings truncated for display.</p>}
  </div>;
}

export function PlanCrossCheckEvent({ payload, detail }: { payload: unknown; detail?: PlanCrossCheckSummary }) {
  const p = record(payload);
  return <section aria-label="Plan cross-check event" className="rounded-md border border-edge p-3 text-sm">
    <p>{planCheckEventText(payload, detail)}</p>
    <Findings value={p.findings} />
    {childId(p.checker_run_id) && <Link to={`/runs/${p.checker_run_id}`}>Checker run</Link>}
  </section>;
}

function sourceLabel(source: unknown): string {
  return source === "pin" || source === "worker default" ? source : "unknown";
}

export function PlanCrossCheck({ run }: { run: Run }) {
  const s = run.plan_cross_check_summary;
  const gate = run.plan_cross_check_gate_reason;
  if (!s && !gate && !run.plan_cross_check_required) return null;
  const usage = s?.usage;
  const cost = usage?.cost_status === "subscription" ? costHeadline(costDisplay("subscription", 0))
    : usage && typeof usage.cost_usd === "number" && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0
    ? costHeadline(costDisplay(usage.cost_status, usage.cost_usd)) : "Unavailable";
  return <section aria-label="Plan cross-check" className="rounded-md border border-edge p-3 text-sm space-y-2">
    <h3 className="font-semibold">Plan cross-check</h3>
    {gate && <p>Current gate: {reasonLabel(gate) ?? "Reason unavailable"}</p>}
    {gate === "planning_diff_refused" && run.plan_cross_check_diff_refusal != null && <p>Refusal: {diffRefusalLabel(run.plan_cross_check_diff_refusal)}</p>}
    <p>{s ? `${s.historical ? "Earlier-plan evidence" : "Checked candidate"}: ${planCheckOutcome(s.verdict, s.reason_class)}` : "Outcome unavailable"}</p>
    {s?.historical && <p>These findings concern an earlier plan; they do not certify the current plan.</p>}
    {s && <>
      <Findings value={s.findings} />
      <p>Model: {plain(s.checker_model) || "Unreported"} ({sourceLabel(s.checker_model_source)}) · Effort: {plain(s.checker_effort) || "Unreported"} ({sourceLabel(s.checker_effort_source)})</p>
      {usage && <p>Checker tokens: {Number.isFinite(usage.input_tokens) && usage.input_tokens >= 0 ? formatTokens(usage.input_tokens) : "Unreported"} in · {Number.isFinite(usage.output_tokens) && usage.output_tokens >= 0 ? formatTokens(usage.output_tokens) : "Unreported"} out</p>}
      <p>Checker cost: {cost}{usage?.cost_status === "subscription" ? " · no cost estimate recorded" : cost === "Unavailable" ? " · cost unavailable" : " · API-equivalent"}</p>
      {childId(s.checker_run_id) && <Link to={`/runs/${s.checker_run_id}`}>Checker run</Link>}
    </>}
  </section>;
}
