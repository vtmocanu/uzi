import { useState } from "react";
import { Link } from "react-router-dom";
import type { Run, PlanCrossCheckSummary } from "../lib/apiTypes";
import { formatLocalTime } from "../lib/budget";
import { costDisplay, costHeadline } from "../lib/costStatus";
import { planCrossCheckGateLabel } from "../lib/crossCheckGate";
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
  worker_unsupported: "Not supported on this worker",
};

// The lead's response states (PRD #3708). None of them claims a finding was resolved.
const responseStates: Record<string, { label: string; note?: string }> = {
  reported: { label: "Reported" },
  truncated: { label: "Reported, truncated", note: "The response was longer than the stored limit; the rest is omitted." },
  not_reported: { label: "Not reported", note: "The next plan had no cross-check dispositions section." },
  unknown: { label: "Unknown", note: "The next plan was too long to look for a response." },
  replanned_after_interruption: { label: "Replanned after an interruption", note: "The lead started a new plan after an interruption, so no response was recorded." },
  reported_in_presented_plan: { label: "From the presented plan; not checked" },
  no_following_candidate: { label: "No following plan", note: "No plan followed this round." },
};
const responseWithText = new Set(["reported", "truncated", "reported_in_presented_plan"]);

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

// Parsing is capped at 8,192 source characters, the server's own response cap.
function Response({ value }: { value: unknown }) {
  const r = record(value);
  const state = typeof r.state === "string" && Object.prototype.hasOwnProperty.call(responseStates, r.state) ? r.state : undefined;
  const text = state && responseWithText.has(state) && typeof r.text === "string" ? r.text.slice(0, 8192) : "";
  const known = state ? responseStates[state] : undefined;
  return <div className="space-y-1.5 rounded-md border border-dashed border-edge p-3">
    <p className="flex flex-wrap items-center gap-2 text-xs text-muted">
      <span>Lead&rsquo;s response (unverified claims)</span>
      <Badge wrap>{known?.label ?? "Response unavailable"}</Badge>
    </p>
    {text.trim() !== "" ? <div className="judge-prose break-words"><Markdown content={text} /></div>
      : known?.note && <p className="text-muted">{known.note}</p>}
    {text.trim() !== "" && known?.note && <p className="text-xs text-muted">{known.note}</p>}
  </div>;
}

// Each round fences the checker's findings (untrusted, possibly wrong advice) apart from
// the lead's response to them (untrusted claims). Rounds are a real sequence, so they are
// numbered; the dashed fence marks text nobody verified.
function Rounds({ value, level }: { value: unknown; level: 3 | 4 }) {
  if (!Array.isArray(value) || value.length === 0) return null;
  const Heading = level === 3 ? "h3" : "h4";
  return <ol aria-label="Plan cross-check rounds" className="space-y-4 border-l border-edge pl-3 sm:pl-4">
    {value.slice(-5).map((item, i) => {
      const r = record(item);
      const n = safeRound(r.round);
      const decided = typeof r.decided_at === "string" ? formatLocalTime(r.decided_at) : null;
      const title = `${n === undefined ? "Round" : `Round ${n}`}: ${planCheckOutcome(r.verdict, r.reason_class)}`;
      return <li key={i} aria-label={title} className="relative space-y-2">
        <span aria-hidden="true" className="absolute -left-[1.15rem] top-0.5 h-3 w-3 sm:-left-[1.4rem] rounded-full border-2 border-edge bg-surface" />
        <div className="flex flex-wrap items-baseline gap-x-2">
          <Heading className="text-sm font-semibold text-fg">{title}</Heading>
          {decided && <time className="text-xs text-muted" dateTime={r.decided_at as string}>{decided}</time>}
        </div>
        <div className="space-y-1.5 rounded-md border border-edge bg-raised/40 p-3">
          <p className="text-xs text-muted">Checker findings (untrusted, may be wrong)</p>
          {r.findings == null ? <p className="text-muted">No findings recorded.</p> : <Findings value={r.findings} />}
        </div>
        {!(r.verdict === "approve" && r.findings == null && record(r.response).state === "no_following_candidate")
          && <Response value={r.response} />}
      </li>;
    })}
  </ol>;
}

// An empty-string gate reason is treated exactly like no reason, on every path here.
function reasonless(gate: unknown): boolean {
  return gate == null || gate === "";
}

// The shared "passed" rule (PRD #3708 plan §6): no gate reason, the latest check approved,
// and that check is of the current plan. No run-status term. The CLI and Slack apply the
// same rule.
function passedRounds(run: Run): number | undefined {
  const s = run.plan_cross_check_summary;
  if (!s || !reasonless(run.plan_cross_check_gate_reason) || s.historical || planCheckOutcome(s.verdict, s.reason_class) !== "Passed") return undefined;
  const rounds = Array.isArray(s.rounds) ? s.rounds : [];
  const last = rounds.length > 0 ? record(rounds[rounds.length - 1]) : undefined;
  if (last && planCheckOutcome(last.verdict, last.reason_class) !== "Passed") return undefined;
  return safeRound(s.round);
}

function gatedResult(run: Run): string {
  const s = run.plan_cross_check_summary;
  const gate = run.plan_cross_check_gate_reason;
  if (s && !s.historical && s.verdict === "pending" && reasonless(gate)) return "Pending";
  if (revisingAfter(run) !== undefined) return "Check in progress";
  const passed = passedRounds(run);
  if (passed !== undefined) return `Passed after ${passed} ${passed === 1 ? "round" : "rounds"}`;
  if (s?.historical) return "Checked an earlier version of this plan";
  if (typeof gate === "string" && gate !== "") return planCrossCheckGateLabel(gate);
  if (s) {
    // An approval the passed rule refused (rounds skew, out-of-range round) is never a pass.
    const outcome = planCheckOutcome(s.verdict, s.reason_class);
    return outcome === "Passed" ? "Outcome unavailable" : outcome;
  }
  return gatedPending(run) ? "Pending" : "Not checked";
}

// A gated run before its first gate has no plan yet; its check has not had a chance to run.
function gatedPending(run: Run): boolean {
  return !isTerminalRun(run.status) && run.status !== "awaiting_approval" && !run.plan_md;
}

// Between automatic rounds: the latest check asked for changes and the lead is writing the
// next candidate, before any plan has been presented. Returns the finished round ("" when
// the round number is unusable), else undefined.
function revisingAfter(run: Run): string | undefined {
  const s = run.plan_cross_check_summary;
  if (run.plan_cross_check_gated !== true || !s || s.historical || !reasonless(run.plan_cross_check_gate_reason)
    || !gatedPending(run) || planCheckOutcome(s.verdict, s.reason_class) !== reasons.revise) return undefined;
  const n = safeRound(s.round);
  return n === undefined ? "" : String(n);
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

// Only an effort can carry the unpinned cross-check default.
function effortSourceLabel(source: unknown): string {
  return source === "cross-check default" ? source : sourceLabel(source);
}

export function PlanCrossCheck({ run, presentation = "standalone" }: { run: Run; presentation?: "standalone" | "inset" }) {
  const s = run.plan_cross_check_summary;
  const gate = run.plan_cross_check_gate_reason;
  const gated = run.plan_cross_check_gated === true;
  if (!s && !gate && !run.plan_cross_check_required && !gated) return null;
  const outcome = s ? planCheckOutcome(s.verdict, s.reason_class)
    : run.plan_cross_check_required || gated ? isTerminalRun(run.status) ? "Not recorded" : "Pending" : "Outcome unavailable";
  const result = gated ? gatedResult(run) : s?.historical ? "Superseded" : outcome;
  const rounds = Array.isArray(s?.rounds) && s.rounds.length > 0;
  const revising = revisingAfter(run);
  const deadline = s && !s.historical && s.verdict === "pending" && typeof s.deadline_at === "string" ? formatLocalTime(s.deadline_at) : null;
  const usage = s?.usage;
  const cost = usage?.cost_status === "subscription" ? costHeadline(costDisplay("subscription", 0))
    : usage && typeof usage.cost_usd === "number" && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0
    ? costHeadline(costDisplay(usage.cost_status, usage.cost_usd)) : "Unavailable";
  const content = <>
    <CardHeader title="Plan cross-check" level={presentation === "inset" ? 3 : 2}
      badge={<Badge wrap>{result}</Badge>}
      subtitle={s?.historical ? "Earlier-plan evidence" : undefined} actions={
      childId(s?.checker_run_id) && <Link className="text-info underline underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60" to={`/runs/${s.checker_run_id}`}>Open checker run</Link>
    } />
    {gated && <p>Mode: checked before your review</p>}
    {gate && <p>Current gate: {reasonLabel(gate) ?? "Reason unavailable"}</p>}
    {gate === "planning_diff_refused" && run.plan_cross_check_diff_refusal != null && <p>Refusal: {diffRefusalLabel(run.plan_cross_check_diff_refusal)}</p>}
    {s && <p>{`${s.historical ? "Earlier-plan evidence" : "Checked candidate"}: ${planCheckOutcome(s.verdict, s.reason_class)}`}</p>}
    {!s && run.plan_cross_check_required && <p>{isTerminalRun(run.status) ? "This run ended without a plan cross-check result." : "Runs when the plan is ready, before it can proceed."}</p>}
    {!s && !run.plan_cross_check_required && gated && <p>{gatedPending(run) ? "Runs when the plan is ready, before your review." : "No check result for this plan."}</p>}
    {s && outcome === "Pending" && (deadline
      ? <p>Check in progress, deadline <time dateTime={s.deadline_at}>{deadline}</time></p>
      : <p>No result recorded yet.</p>)}
    {revising !== undefined && <p>{`Check in progress, the lead is revising after ${revising ? `round ${revising}` : "the last round"}`}</p>}
    {s?.historical && <p>These findings concern an earlier plan; they do not certify the current plan.</p>}
    {s && <>
      {rounds ? <Rounds value={s.rounds} level={presentation === "inset" ? 4 : 3} /> : <Findings value={s.findings} />}
      <p className="flex flex-wrap gap-x-1 break-all text-xs text-muted">
        <span>Model: {plain(s.checker_model) || "Unreported"} ({sourceLabel(s.checker_model_source)})</span>
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Effort: {plain(s.checker_effort) || "Unreported"} ({effortSourceLabel(s.checker_effort_source)})</span></span>
        {usage && <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checker tokens: {Number.isFinite(usage.input_tokens) && usage.input_tokens >= 0 ? formatTokens(usage.input_tokens) : "Unreported"} in · {Number.isFinite(usage.output_tokens) && usage.output_tokens >= 0 ? formatTokens(usage.output_tokens) : "Unreported"} out</span></span>}
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checker cost: {cost}{usage?.cost_status === "subscription" ? " · no cost estimate recorded" : cost === "Unavailable" ? " · cost unavailable" : " · API-equivalent"}</span></span>
      </p>
    </>}
  </>;
  return presentation === "inset"
    ? <section aria-label="Plan cross-check" className="rounded-lg border border-edge bg-surface p-4 text-sm space-y-2">{content}</section>
    : <section aria-label="Plan cross-check" className="rounded-xl border-edge bg-surface"><Card className="text-sm space-y-2">{content}</Card></section>;
}
