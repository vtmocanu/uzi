import { useEffect, useRef, useState } from "react";
import { api, type Run } from "../../lib/api";
import { stripUnsafeChars } from "../../lib/safeText";
import { Alert, Button } from "../../components/ui";

function safeValue(value: unknown, depth = 0): string {
  if (depth > 8) return "Further detail omitted";
  if (typeof value === "string") return stripUnsafeChars(value);
  if (value === null || typeof value === "number" || typeof value === "boolean") return String(value);
  if (Array.isArray(value)) return value.length === 0 ? "[]" : value.slice(0, 100).map(v => safeValue(v, depth + 1)).join(", ");
  if (value && typeof value === "object") {
    return Object.entries(value).slice(0, 100).map(([key, v]) =>
      stripUnsafeChars(key) + ": " + safeValue(v, depth + 1)).join("; ");
  }
  return "Unavailable";
}

function StateDetails({ value, label }: { value: unknown; label: string }) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return <p>{label}: details unavailable</p>;
  }
  const state = value as Record<string, unknown>;
  return <div className="space-y-1">
    <h4 className="font-semibold">{label}</h4>
    {[
      ["milestones_frozen", "Milestones"], ["milestones_candidate", "Proposed milestones"],
      ["milestones_completed", "Completed"], ["milestones_in_progress", "In progress"],
      ["milestones_agents", "Agents"], ["scope_ceiling", "Scope ceiling"],
      ["stop_kind", "Stop decision"], ["stop_reason", "Stop reason"], ["completion_contract", "Completion criteria and decisions"],
    ].map(([key, label]) => <p key={key}>{label}: {safeValue(state[key])}</p>)}
  </div>;
}

function cursor(value: unknown): number | undefined {
  return typeof value === "number" && Number.isSafeInteger(value) && value > 0 ? value : undefined;
}

// One bounded server page at a time. No automatic paging and no accumulated history.
export function MilestoneResetHistory({ run }: { run: Run }) {
  const [page, setPage] = useState<Run | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState("");
  const inFlight = useRef(false);
  const generation = useRef(0);
  const activeRun = useRef(run.id);
  activeRun.current = run.id;
  useEffect(() => {
    generation.current += 1;
    setPage(null); setLoading(false); setError(""); inFlight.current = false;
    return () => { generation.current += 1; };
  }, [run.id]);
  const shown = page?.id === run.id ? page : run;
  const records = Array.isArray(shown.milestone_reconciliations) ? shown.milestone_reconciliations.slice(0, 50) : [];
  const before = cursor(shown.milestone_reconciliations_next_before_gate_revision);
  const loadOlder = async () => {
    if (!before || inFlight.current) return;
    const id = run.id;
    const gen = generation.current;
    inFlight.current = true; setLoading(true); setError("");
    try {
      const result = await api.getRun(id, before);
      if (activeRun.current !== id || generation.current !== gen) return;
      if (result.run.id !== id) throw new Error("Wrong run");
      const next = cursor(result.run.milestone_reconciliations_next_before_gate_revision);
      if (next !== undefined && next >= before) throw new Error("Invalid history cursor");
      setPage(result.run);
    } catch {
      if (activeRun.current === id && generation.current === gen) setError("Could not load older reset history.");
    } finally {
      if (activeRun.current === id && generation.current === gen) {
        inFlight.current = false; setLoading(false);
      }
    }
  };
  if (!records.length && !before && !page) return null;
  return <section aria-label="Milestone reset history" className="space-y-3 rounded-xl border border-edge p-4">
    <h2 className="font-semibold">Milestone reset history</h2>
    {records.map((value, i) => {
      const record = value && typeof value === "object" && !Array.isArray(value) ? value : {};
      return <article key={i} className="space-y-2 break-words rounded border border-edge p-3 text-sm">
        <p>Gate: {safeValue(record.gate_revision)} · Time: {safeValue(record.created_at)}</p>
        <p>Reason: {safeValue(record.reason)}</p>
        {record.reason_truncated === true && <p>Reason truncated.</p>}
        {record.details_omitted === true && <p>Some audit details were omitted from this readout.</p>}
        <StateDetails label="Before reset" value={record.before_state} />
        <StateDetails label="After reset" value={record.after_state} />
      </article>;
    })}
    {error && <Alert message={error} />}
    {before && <Button variant="secondary" disabled={loading} onClick={() => void loadOlder()}>Load older</Button>}
  </section>;
}
