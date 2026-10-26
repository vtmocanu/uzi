import { useState } from "react";
import { Link } from "react-router-dom";
import type { CodeCrossCheckCoverage, Run } from "../lib/apiTypes";
import { costDisplay, costHeadline } from "../lib/costStatus";
import { isTerminalRun } from "../lib/runStatus";
import { stripDisplayCell, stripUnsafeChars } from "../lib/safeText";
import { Markdown } from "./Markdown";
import { Badge, Card, CardHeader, type BadgeTone } from "./ui";

function plain(value: unknown, cap = 200): string {
  return typeof value === "string" ? stripUnsafeChars(value.slice(0, cap)).replace(/[\n\t]/g, " ") : "";
}

function reasonLabel(value: string): string {
  switch (value) {
    case "worker_unsupported": return "Worker does not support code cross-check";
    case "snapshot_failed": return "Snapshot failed";
    case "interrupted": return "Interrupted";
    case "timeout":
    case "timed_out":
    case "model_timeout": return "Checker timed out";
    case "no_local_checker":
    case "checker_unavailable": return "No local checker available";
    case "model_error": return "Checker model error";
    case "malformed": return "Checker response could not be read";
    case "confinement_failed": return "Checker confinement failed";
    default: return plain(value) || "Unavailable";
  }
}

type Disposition = { finding_id: string; disposition: "addressed" | "declined" | "not_reported"; reason: string };

function dispositionEvidence(findings: unknown, value: unknown, finalized: boolean) {
  const unavailable = { state: "Dispositions unavailable", entries: new Map<string, Disposition>() };
  if (!Array.isArray(findings) || findings.length > 20 || !Array.isArray(value) || value.length > 20) return unavailable;
  const known = new Set<string>();
  for (const f of findings) {
    if (!f || typeof f.id !== "string" || !/^[A-Za-z0-9_-]{1,64}(?![\s\S])/.test(f.id) || known.has(f.id)) return unavailable;
    known.add(f.id);
  }
  const entries = new Map<string, Disposition>();
  for (const d of value) {
    if (!d || typeof d !== "object" || !known.has(d.finding_id) || entries.has(d.finding_id)
      || !["addressed", "declined", "not_reported"].includes(d.disposition)
      || typeof d.reason !== "string" || d.reason.length > 1024 || !d.reason.isWellFormed()
      || new TextEncoder().encode(d.reason).length > 1024) return unavailable;
    entries.set(d.finding_id, d);
  }
  if (finalized && entries.size !== known.size) return unavailable;
  return { state: finalized ? "Finalized" : "Pending report", entries };
}

function Findings({ value, dispositions }: { value: unknown; dispositions: Map<string, Disposition> }) {
  const [open, setOpen] = useState(false);
  const tones: Record<string, BadgeTone> = { critical: "danger", major: "warning", minor: "info" };
  const items = Array.isArray(value) ? value : [];
  let remaining = 32768;
  let truncated = items.length > 20;
  const bounded = (value: unknown, cap: number) => {
    if (typeof value !== "string") return "";
    let bytes = 0;
    let text = "";
    // At most cap UTF-16 units per field, across at most 20 findings; omissions
    // consume no sibling budget and are disclosed below.
    for (const char of value.slice(0, cap)) {
      const size = new TextEncoder().encode(char).length;
      if (bytes + size > remaining) break;
      text += char;
      bytes += size;
    }
    remaining -= bytes;
    if (text.length < value.length) truncated = true;
    return text;
  };
  const seen = new Set<string>();
  const content = items.slice(0, 20).map((item, index) => {
    if (!item || typeof item !== "object") return <article key={index} className="flex flex-col gap-1.5 px-3 py-2.5 sm:flex-row sm:items-start sm:gap-3">Finding unavailable</article>;
    const f = item as Record<string, unknown>;
    const id = typeof f.id === "string" && /^[A-Za-z0-9_-]{1,64}(?![\s\S])/.test(f.id) && !seen.has(f.id) ? f.id : "";
    if (id) seen.add(id);
    const disposition = id ? dispositions.get(id) : undefined;
    const path = plain(bounded(f.path, 512), 512);
    const title = bounded(f.title, 512);
    const detail = bounded(f.detail, 2048);
    const known = typeof f.severity === "string" && Object.prototype.hasOwnProperty.call(tones, f.severity);
    return <article key={index} className="flex flex-col gap-1.5 px-3 py-2.5 sm:flex-row sm:items-start sm:gap-3">
      <span className="shrink-0 sm:w-20"><Badge wrap tone={known ? tones[f.severity as string] : "neutral"}>{known ? f.severity as string : "Severity unavailable"}</Badge></span>
      <div className="min-w-0 flex-1 space-y-1">
        <p className="text-xs text-muted">Finding: {id ? <code className="rounded bg-raised px-1.5 py-0.5 font-mono break-all">{stripUnsafeChars(id)}</code> : "ID unavailable"}</p>
        {path && <p className="text-xs text-muted">Path: <code className="rounded bg-raised px-1.5 py-0.5 font-mono break-all">{path}</code>{Number.isSafeInteger(f.line) && (f.line as number) > 0 && (f.line as number) <= 2147483647 ? `:${f.line}` : ""}</p>}
        <div className="judge-prose break-words"><Markdown content={title} /></div>
        <div className="judge-prose break-words text-muted"><Markdown content={detail} /></div>
        {disposition && <><p>Disposition: <Badge>{disposition.disposition}</Badge></p><div className="judge-prose break-words"><Markdown content={disposition.reason} /></div></>}
      </div>
    </article>;
  });
  return <div className="space-y-3">
    {content.length > 0 && <div className="divide-y divide-edge rounded-md border border-edge">{content.slice(0, 3)}</div>}
    {content.length > 3 && <details open={open} onToggle={e => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer rounded-sm text-muted focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60">{open ? "Hide" : "Show"} {content.length - 3} more findings</summary>
      <div className="divide-y divide-edge rounded-md border border-edge">{content.slice(3)}</div>
    </details>}
    {truncated && <p>Findings truncated for display.</p>}
    {!Array.isArray(value) && <p>Findings unavailable</p>}
    {Array.isArray(value) && items.length === 0 && <p>No findings recorded.</p>}
  </div>;
}

function coverageEvidence(value: unknown): CodeCrossCheckCoverage | null {
  const object = (v: unknown): v is Record<string, unknown> => !!v && typeof v === "object" && !Array.isArray(v);
  const text = (v: unknown, cap: number): v is string => typeof v === "string" && v.length <= cap
    && new TextDecoder("utf-8", { ignoreBOM: true }).decode(new TextEncoder().encode(v)) === v
    && !v.includes("\u0000") && new TextEncoder().encode(v).length <= cap;
  if (!object(value) || value.version !== 1 || !Array.isArray(value.entries) || value.entries.length > 128
    || typeof value.inventory_incomplete !== "boolean" || typeof value.omitted_count_exact !== "boolean"
    || typeof value.activity_complete !== "boolean"
    || ![value.omitted_paths, value.unknown_judgments, value.invalid_judgments, value.omitted_judgments]
      .every(n => Number.isSafeInteger(n) && (n as number) >= 0)
    || !Array.isArray(value.reasons) || value.reasons.length > 8 || !value.reasons.every(r => text(r, 256))) return null;
  const seen = new Set<string>();
  for (const entry of value.entries) {
    if (!object(entry) || !text(entry.path, 512) || !entry.path || seen.has(entry.path)
      || !["examined", "excluded", "unassessed"].includes(entry.status as string)
      || !text(entry.reason, 256) || (entry.status === "excluded" && !entry.reason)
      || typeof entry.recorded_read !== "boolean") return null;
    seen.add(entry.path);
  }
  if ((!value.omitted_count_exact || (value.omitted_paths as number) > 0) && !value.inventory_incomplete) return null;
  try {
    if (new TextEncoder().encode(JSON.stringify(value)).length > 128 * 1024) return null;
  } catch { return null; }
  return value as unknown as CodeCrossCheckCoverage;
}

function Coverage({ value }: { value: unknown }) {
  if (value == null) return <p>coverage not reported</p>;
  const account = coverageEvidence(value);
  if (!account) return <p>Coverage unavailable</p>;
  const counts = { examined: 0, excluded: 0, unassessed: 0 };
  for (const entry of account.entries) counts[entry.status]++;
  const inventoryMissing = account.omitted_paths > 0 || !account.omitted_count_exact
    || account.reasons.some(r => r === "inventory_failed" || r === "inventory_truncated");
  const omitted = account.omitted_count_exact ? `${account.omitted_paths} paths omitted`
    : account.omitted_paths > 0 ? `at least ${account.omitted_paths} paths omitted; total unknown` : "omitted path count unknown";
  return <div className="space-y-2" aria-label="Review coverage account">
    <p>{account.entries.length} stored paths: {counts.examined} examined, {counts.excluded} excluded, {counts.unassessed} unassessed</p>
    <p>Advisory review account; not a correctness certification.</p>
    {inventoryMissing && <p>Inventory incomplete: {omitted}</p>}
    {account.inventory_incomplete && !inventoryMissing && <p>Review account incomplete; see judgment and activity metadata.</p>}
    <p>Judgments: {account.unknown_judgments} unknown, {account.invalid_judgments} invalid, {account.omitted_judgments} omitted</p>
    <p>Activity: {account.activity_complete ? "complete" : "partial"}</p>
    {account.reasons.length > 0 && <p>Account reasons: {account.reasons.map(r => stripDisplayCell(r)).join(", ")}</p>}
    <ul className="space-y-2">
      {account.entries.map((entry, index) => <li key={index} className="break-words">
        <code className="font-mono break-all">{stripDisplayCell(entry.path)}</code>
        <span> · {entry.status} · Recorded read: {entry.recorded_read ? "yes" : "no"}</span>
        <p>Reason: {stripDisplayCell(entry.reason) || "not reported"}</p>
      </li>)}
    </ul>
  </div>;
}

export function CodeCrossCheck({ run }: { run: Run }) {
  const s = run.code_cross_check_summary;
  if (!s && !run.code_cross_check_required) return null;
  const historical = s?.interrupted_at != null;
  const outcome = s?.outcome === "pending" ? "Pending" : s?.outcome === "completed" ? "Completed"
    : s?.outcome === "failed" ? "Incomplete" : !s && run.code_cross_check_required ? isTerminalRun(run.status) ? "Not recorded" : "Pending" : "Outcome unavailable";
  const evidence = dispositionEvidence(s?.findings, s?.dispositions, s?.finalized_at != null);
  const counts = { addressed: 0, declined: 0, not_reported: 0 };
  for (const d of evidence.entries.values()) counts[d.disposition]++;
  const usage = s?.usage;
  const cost = usage?.cost_status === "subscription" ? costHeadline(costDisplay("subscription", 0))
    : usage && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0
    ? costHeadline(costDisplay(usage.cost_status, usage.cost_usd)) : "Unavailable";
  return <section aria-label="Code cross-check" className="rounded-xl border-edge bg-surface">
    <Card className="text-sm space-y-2">
    <CardHeader title="Code cross-check · Advisory before publication"
      badge={<Badge>{historical ? "Superseded" : outcome}</Badge>}
      subtitle={historical ? "Earlier attempt evidence" : undefined}
      actions={typeof s?.checker_run_id === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?![\s\S])/i.test(s.checker_run_id) && <Link className="text-info underline underline-offset-2 hover:underline focus-visible:rounded-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand/60" to={`/runs/${s.checker_run_id}`}>Open checker run</Link>}
    />
    {historical && <p>These findings concern an earlier attempt; they are not current findings.</p>}
    {!s && <p>{isTerminalRun(run.status) ? "This run ended without a code cross-check result." : "Runs after implementation, before the PR is published."}</p>}
    {s?.outcome === "pending" && <p>No result recorded yet.</p>}
    {s && <>
      {s.reason_class && <p>Reason: {reasonLabel(s.reason_class)}</p>}
      <p>{evidence.state === "Pending report" ? "Dispositions pending" : evidence.state}</p>
      {!historical && s.outcome === "completed" && evidence.state === "Finalized" && <p>{counts.addressed} addressed, {counts.declined} declined, {counts.not_reported} not reported</p>}
      <Coverage value={s.coverage} />\n      <Findings value={s.findings} dispositions={evidence.entries} />
      <p className="flex flex-wrap gap-x-1 break-all text-xs text-muted">
        <span>Family: {s.checker_harness === "codex" ? "Codex" : s.checker_harness === "claude" ? "Claude" : "Unreported"}</span>
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Model: {plain(s.checker_model) || "Unreported"}</span></span>
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Effort: {plain(s.checker_effort) || "Unreported"}</span></span>
        <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checker cost: {cost}{usage?.cost_status === "subscription" ? " · subscription usage" : ""}</span></span>
        {typeof s.head_commit === "string" && /^[a-f0-9]{40}(?![\s\S])/i.test(s.head_commit) && <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Checked SHA: <code className="font-mono" title={s.head_commit}>{s.head_commit.slice(0, 12)}</code></span></span>}
        {s.custom_guidance_applied === true && <span className="inline-flex max-w-full gap-x-1"><span className="shrink-0"> · </span><span className="min-w-0">Custom guidance applied{typeof s.guidance_digest === "string" && /^[0-9a-f]{12}/i.test(s.guidance_digest)
          && <> <code className="font-mono">{s.guidance_digest.slice(0, 12).toLowerCase()}</code></>}
        </span></span>}
      </p>
    </>}
    </Card>
  </section>;
}
