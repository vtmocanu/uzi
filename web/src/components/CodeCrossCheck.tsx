import { Link } from "react-router-dom";
import type { Run } from "../lib/apiTypes";
import { costDisplay, costHeadline } from "../lib/costStatus";
import { stripUnsafeChars } from "../lib/safeText";
import { Markdown } from "./Markdown";

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

function Findings({ value }: { value: unknown }) {
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
    if (!item || typeof item !== "object") return <p key={index}>Finding unavailable</p>;
    const f = item as Record<string, unknown>;
    const id = typeof f.id === "string" && /^[A-Za-z0-9_-]{1,64}(?![\s\S])/.test(f.id) && !seen.has(f.id) ? f.id : "";
    if (id) seen.add(id);
    const path = plain(bounded(f.path, 512), 512);
    const title = bounded(f.title, 512);
    const detail = bounded(f.detail, 2048);
    return <article key={index} className="space-y-2">
      <p>Finding: {id || "ID unavailable"} · {["critical", "major", "minor"].includes(String(f.severity)) ? String(f.severity) : "Severity unavailable"}</p>
      {path && <p>Path: {path}{Number.isSafeInteger(f.line) && (f.line as number) > 0 && (f.line as number) <= 2147483647 ? `:${f.line}` : ""}</p>}
      <Markdown content={title} />
      <Markdown content={detail} />
    </article>;
  });
  return <div className="max-h-96 overflow-auto space-y-3">
    {content}
    {truncated && <p>Findings truncated for display.</p>}
    {!Array.isArray(value) && <p>Findings unavailable</p>}
    {Array.isArray(value) && items.length === 0 && <p>No findings recorded.</p>}
  </div>;
}

export function CodeCrossCheck({ run }: { run: Run }) {
  const s = run.code_cross_check_summary;
  if (!s && !run.code_cross_check_required) return null;
  const historical = s?.interrupted_at != null;
  const outcome = s?.outcome === "pending" ? "Pending" : s?.outcome === "completed" ? "Completed"
    : s?.outcome === "failed" ? "Incomplete" : "Outcome unavailable";
  const usage = s?.usage;
  const cost = usage?.cost_status === "subscription" ? costHeadline(costDisplay("subscription", 0))
    : usage && Number.isFinite(usage.cost_usd) && usage.cost_usd >= 0
    ? costHeadline(costDisplay(usage.cost_status, usage.cost_usd)) : "Unavailable";
  return <section aria-label="Code cross-check" className="rounded-md border border-edge p-3 text-sm space-y-2">
    <h3 className="font-semibold">Code cross-check · Advisory before publication</h3>
    <p>{historical ? "Superseded · Earlier attempt evidence" : outcome}</p>
    {historical && <p>These findings concern an earlier attempt; they are not current findings.</p>}
    {s && <>
      {s.reason_class && <p>Reason: {reasonLabel(s.reason_class)}</p>}
      {typeof s.head_commit === "string" && /^[a-f0-9]{40}(?![\s\S])/i.test(s.head_commit) && <p>Checked SHA: {s.head_commit}</p>}
      <Findings value={s.findings} />
      <p>Family: {s.checker_harness === "codex" ? "Codex" : s.checker_harness === "claude" ? "Claude" : "Unreported"} · Model: {plain(s.checker_model) || "Unreported"} · Effort: {plain(s.checker_effort) || "Unreported"}</p>
      <p>Checker cost: {cost}{usage?.cost_status === "subscription" ? " · subscription usage" : ""}</p>
      {typeof s.checker_run_id === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}(?![\s\S])/i.test(s.checker_run_id) && <Link to={`/runs/${s.checker_run_id}`}>Checker run</Link>}
    </>}
  </section>;
}
