import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { api } from "../lib/api";
import { ApiError } from "../lib/apiError";
import type { CrossCheckHistoryAttemptDTO as AttemptDTO, CrossCheckHistoryGroupDTO as GroupDTO, CrossCheckHistoryParams as Params } from "../lib/apiTypes";
import { stripDisplayCell } from "../lib/safeText";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { cx } from "../components/ui";

type Attempt = AttemptDTO & { notRefreshed?: boolean };
type Group = Omit<GroupDTO, "attempts"> & { attempts: Attempt[] };

type Scope = "runs" | "cross-checks";
type Availability = "loading" | "available" | "empty" | "unavailable" | "error" | "compatibility";
const preferenceKey = (id: string) => `uzi.runs.scope.${id}`;

function savedScope(id: string): Scope {
  if (!id) return "runs";
  try {
    const value: unknown = JSON.parse(localStorage.getItem(preferenceKey(id)) ?? "null");
    return value === "cross-checks" ? value : "runs";
  } catch { return "runs"; }
}

// Availability is account-scoped and deliberately has no settings or list dependencies.
export function useCrossCheckScope(userID: string) {
  const [params, setParams] = useSearchParams();
  const saved = useMemo(() => savedScope(userID), [userID]);
  const [choice, setChoice] = useState<{ id: string; scope: Scope }>();
  const [probe, setProbe] = useState<{ id: string; state: Availability }>();
  const [retry, setRetry] = useState(0);
  useEffect(() => {
    let alive = true;
    setProbe({ id: userID, state: "loading" });
    // Older partial test consumers intentionally expose only the old Runs API.
    if (!userID || typeof api.crossCheckHistoryAvailability !== "function") {
      setProbe({ id: userID, state: "compatibility" });
      return;
    }
    void api.crossCheckHistoryAvailability().then(value => {
      if (alive) setProbe({ id: userID, state: value.available ? "available" : "empty" });
    }).catch((error: unknown) => {
      if (alive) setProbe({ id: userID, state: error instanceof ApiError && error.status === 404 ? "unavailable" : "error" });
    });
    return () => { alive = false; };
  }, [userID, retry]);
  const availability = probe?.id === userID ? probe.state : "loading";
  const urlScope = params.get("scope");
  const wanted = urlScope === "runs" || urlScope === "cross-checks" ? urlScope
    : choice?.id === userID ? choice.scope : saved;
  const scope: Scope = availability === "available" ? wanted : "runs";
  function choose(value: Scope) {
    setChoice({ id: userID, scope: value });
    try { if (userID) localStorage.setItem(preferenceKey(userID), JSON.stringify(value)); } catch { /* Storage can be blocked. */ }
    const next = new URLSearchParams(params);
    next.set("scope", value);
    next.delete("cursor");
    setParams(next);
  }
  const controls = availability === "available" ? (
    <div role="group" aria-label="What to list" className="inline-flex overflow-hidden rounded-lg border border-edge text-sm">
      {(["runs", "cross-checks"] as const).map(value => <button key={value} type="button" aria-pressed={scope === value}
        onClick={() => choose(value)} className={cx("px-3 py-1.5 transition-colors", value === "cross-checks" && "border-l border-edge", scope === value ? "bg-raised font-medium text-fg" : "text-muted hover:bg-raised/60")}>
        {value === "runs" ? "Runs" : "Cross-checks"}
      </button>)}
    </div>
  ) : null;
  const notice = availability === "error" ? <div role="status">Cross-check availability could not be loaded. <button type="button" onClick={() => setRetry(n => n + 1)}>Retry availability</button></div>
    : availability === "unavailable" ? <p role="status">Cross-check history is unavailable on this server version.</p>
    : availability === "empty" && wanted === "cross-checks" ? <p role="status">No retained cross-check evidence. Showing Runs.</p>
    : null;
  return { scope, controls, notice };
}

// Fold single-line whitespace before the existing terminal/control/format scrubber.
function text(value: unknown, fallback = "Not recorded"): string {
  return stripDisplayCell(typeof value === "string" ? value.replace(/[\n\r\t]+/g, " ") : value).replace(/\s+/g, " ").trim() || fallback;
}
function runPath(value: unknown): string | undefined {
  return typeof value === "string" && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)
    ? `/runs/${value}` : undefined;
}
const executionLabels: Record<string, string> = {
  pending: "Pending", running: "Running", finished: "Finished", didnt_finish: "Didn't finish",
  couldnt_start: "Couldn't start", replaced: "Replaced", interrupted: "Interrupted",
};
const verdictLabels: Record<string, string> = {
  approved: "Approved", changes_requested: "Changes requested", blocked: "Blocked", none: "No verdict",
};
function label(labels: Record<string, string>, value: string, fallback: string) {
  return Object.prototype.hasOwnProperty.call(labels, value) ? labels[value] : fallback;
}
const identity = (a: Attempt) => `${a.stage}:${a.round}`;
function ordered(attempts: Attempt[]): Attempt[] {
  return attempts.slice().sort((a, b) => (a.stage === "plan" ? 0 : a.stage === "code" ? 1 : 2) -
    (b.stage === "plan" ? 0 : b.stage === "code" ? 1 : 2) || a.round - b.round);
}
function mergeOlder(current: Group, older: Group): Group {
  const attempts = new Map(current.attempts.map(a => [identity(a), a]));
  // Current evidence wins duplicates; a fresh continuation can replace stale evidence.
  for (const a of older.attempts) {
    const existing = attempts.get(identity(a));
    if (!existing || (existing.notRefreshed && !a.notRefreshed)) attempts.set(identity(a), a);
  }
  return { ...current, older_cursor: older.older_cursor, omission: older.omission, attempts: ordered([...attempts.values()]) };
}

function observed(a: Attempt, value: string): string {
  return a.notRefreshed ? `Last observed: ${value} (not refreshed)` : value;
}

function CheckCard({ group: g, loadOlder, busy }: { group: Group; loadOlder: () => void; busy: boolean }) {
  const [expanded, setExpanded] = useState(false);
  const [selected, setSelected] = useState<string>();
  const detailsID = useId();
  const hadOlder = useRef(Boolean(g.older_cursor));
  if (g.older_cursor) hadOlder.current = true;
  const attempts = ordered(g.attempts);
  const shown = attempts.length;
  const activeLoaded = attempts.filter(a => !a.notRefreshed && a.active).length;
  const activeHidden = Math.max(0, g.active_attempts - activeLoaded);
  const parentPath = runPath(g.parent_id);
  const title = text(g.parent_title, "Untitled work run");
  const picked = attempts.find(a => identity(a) === selected);
  return <article className="min-w-0 overflow-hidden rounded-xl border border-edge bg-surface" aria-label={title}>
    <header className="flex flex-wrap items-start justify-between gap-3 p-4">
      <h2 className="min-w-0 flex-1 break-words font-semibold" title={title}>
        {parentPath ? <Link to={parentPath} className="hover:underline">{title}</Link> : title}
      </h2>
      <p className="rounded-full border border-edge bg-raised px-2 py-0.5 text-sm text-muted">Work run: {text(g.parent_status, "Unknown")}</p>
    </header>
    <div className="space-y-2 px-4 pb-4 text-sm text-muted">
      <p>{shown} shown · {Math.max(0, g.total_attempts - g.matching_attempts)} filtered · {Math.max(0, g.matching_attempts - shown)} remaining attempts</p>
      <p className="break-words">Loaded range: {attempts.length ? attempts.map(a => `${a.stage === "plan" ? "Plan" : a.stage === "code" ? "Code" : "Unknown stage"} ${a.round}`).join(", ") : "No attempts loaded"}</p>
      {activeHidden > 0 && <p>{activeHidden} {attempts.some(a => a.notRefreshed) ? "active attempts outside refreshed evidence or hidden by filters." : "active attempts hidden by filters or not loaded."} {text(g.active_context, "Additional active attempts are outside this loaded slice.")}</p>}
      {attempts.some(a => a.notRefreshed) && <p>Older retained evidence was not refreshed. Active counts use current evidence only; retained rows do not identify which older attempts remain active. Reload this page to reset the loaded history, then load older attempts to check their latest evidence.</p>}
      {g.older_cursor && <p>Earlier rounds omitted: load older attempts</p>}
      <div className="flex flex-wrap gap-2" role="group" aria-label="Attempt ladder">
        {attempts.map(a => {
          const name = `${a.stage === "plan" ? "Plan" : a.stage === "code" ? "Code" : "Unknown stage"} round ${a.round}`;
          const exec = observed(a, label(executionLabels, a.execution, "Unknown execution"));
          const verdict = label(verdictLabels, a.review, "Unknown verdict");
          const tone = a.notRefreshed ? "text-muted" : a.active ? "text-info" : ["didnt_finish", "couldnt_start"].includes(a.execution) ? "text-danger"
            : a.review === "approved" ? "text-ok" : ["changes_requested", "blocked"].includes(a.review) ? "text-warn" : "text-muted";
          return <button type="button" key={identity(a)} title={`${name}: ${exec}`}
            aria-pressed={selected === identity(a)} aria-controls={detailsID}
            onClick={() => { setSelected(selected === identity(a) ? undefined : identity(a)); setExpanded(true); }}
            className={cx("max-w-full rounded-lg border px-2 py-1 text-left", selected === identity(a) ? "border-brand text-fg" : "border-edge")}>
            <span aria-hidden className={cx("mr-1", tone)}>●</span>{name} · {exec}{a.review !== "none" ? ` · ${verdict}` : ""}
          </button>;
        })}
      </div>
      <button type="button" aria-expanded={expanded} aria-controls={detailsID} onClick={() => setExpanded(!expanded)}>{expanded ? "Hide" : "Show"} attempt details</button>
      {hadOlder.current && <button type="button" className="ml-3" aria-disabled={busy || !g.older_cursor} onClick={() => { if (!busy && g.older_cursor) loadOlder(); }}>{busy ? "Loading older attempts" : g.older_cursor ? "Load older attempts" : "All matching attempts loaded"}</button>}
    </div>
    {expanded && <div id={detailsID} className="border-t border-edge bg-raised/30 p-4">
      {(picked ? [picked] : attempts).map(a => <section key={identity(a)} className="mb-4 min-w-0 space-y-2 break-words last:mb-0">
        {a.notRefreshed && <p className="text-sm text-muted">Last observed evidence: this attempt was not refreshed.</p>}
        <h3 className="font-medium">{a.stage === "plan" ? "Plan" : a.stage === "code" ? "Code" : "Unknown stage"} round {a.round}</h3>
        <dl className="grid min-w-0 gap-3 text-sm sm:grid-cols-3">
          <div><dt className="text-muted">Check</dt><dd>{observed(a, label(executionLabels, a.execution, "Unknown execution"))}</dd></div>
          <div><dt className="text-muted">{a.notRefreshed ? "Last observed verdict (not refreshed)" : "Verdict"}</dt><dd>{label(verdictLabels, a.review, "Unknown verdict")}</dd></div>
          <div><dt className="text-muted">{a.notRefreshed ? "Last observed effect on run (not refreshed)" : "Effect on run"}</dt><dd>{text(a.effect, "Effect not recorded")}</dd></div>
        </dl>
        {a.historical && <p className="text-sm text-muted">{a.stage === "plan" && a.review === "approved" ? "Historical approval of an earlier plan candidate." : "Historical attempt retained as evidence."}</p>}
        <p className="text-sm">Cause: {text(a.reason, "Cause not recorded for this attempt")}</p>
        <p className="text-sm">Next action: {text(a.next_action, "No action recorded")}</p>
        <p className="text-sm text-muted">Checker: {text(a.model)} · {text(a.effort)} · {observed(a, text(a.child_status, "Status not recorded"))}</p>
        <p className="text-sm text-muted">Created: {text(a.created_at)} · Decided: {text(a.decided_at)} · Interrupted: {text(a.interrupted_at)} · Deadline: {text(a.deadline_at)}</p>
        {a.findings_count !== null && <p className="text-sm">{a.findings_count} findings recorded</p>}
        {runPath(a.child_id) ? <Link className="text-sm text-brand hover:underline" to={runPath(a.child_id)!}>Open checker</Link>
          : <p className="text-sm text-muted">{text(a.child_context, "No checker run available")}</p>}
      </section>)}
    </div>}
  </article>;
}

export function CrossChecks({ lifecycle }: { lifecycle: "active" | "past" }) {
  const [params, setParams] = useSearchParams();
  const rawStage = params.get("stage");
  const stage: Params["stage"] = rawStage === "plan" || rawStage === "code" ? rawStage : "all";
  const rawOutcome = params.get("outcome");
  const outcome: Params["outcome"] = lifecycle === "past" && ["didnt_finish", "raised_issues", "passed", "replaced"].includes(rawOutcome ?? "")
    ? rawOutcome as Params["outcome"] : "any";
  const cursor = params.get("cursor") || undefined;
  const query = useMemo(() => ({ lifecycle, stage, ...(lifecycle === "past" ? { outcome } : {}), ...(cursor ? { cursor } : {}) }), [lifecycle, stage, outcome, cursor]);
  const key = JSON.stringify(query);
  const [state, setState] = useState<{ key: string; groups: Group[]; next?: string; continued?: string[] }>({ key: "", groups: [] });
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const version = useRef(0);
  const load = useCallback(async (older?: string[]) => {
    const requestVersion = ++version.current;
    setLoading(true);
    setError("");
    try {
      const result = await api.listCrossCheckHistory(older ? { lifecycle, stage, ...(lifecycle === "past" ? { outcome } : {}), attempt_cursor: older } : query);
      if (version.current !== requestVersion) return;
      if (result.groups.some(g => g.refresh_needed)) {
        setError("Cross-check lifecycle changed. Refresh this page.");
      } else {
        setState(previous => older && previous.key === key
          ? { ...previous, continued: [...new Set([...(previous.continued ?? []), ...result.groups.map(g => g.parent_id)])], groups: previous.groups.map(g => {
            const incoming = result.groups.find(item => item.parent_id === g.parent_id);
            return incoming ? mergeOlder(g, incoming) : g;
          }) }
          : { key, next: result.next_cursor, continued: previous.key === key ? previous.continued?.filter(id => result.groups.some(g => g.parent_id === id)) : [], groups: result.groups.map(fresh => {
            const current = previous.key === key ? previous.groups.find(g => g.parent_id === fresh.parent_id) : undefined;
            if (!current) return fresh;
            // Fresh records win overlaps; explicitly loaded older windows keep their continuation.
            const merged = mergeOlder(fresh, { ...current, attempts: current.attempts.map(a => ({ ...a, notRefreshed: true })) });
            return previous.continued?.includes(fresh.parent_id) ? merged
              : { ...merged, older_cursor: fresh.older_cursor, omission: fresh.omission };
          }) });
      }
    } catch (err: unknown) {
      if (version.current === requestVersion) setError(err instanceof ApiError && err.status === 404
        ? "Cross-check history is unavailable on this server version." : "Cross-check history could not be loaded. Please retry.");
    } finally { if (version.current === requestVersion) setLoading(false); }
  }, [key, query, lifecycle, stage, outcome]);
  const invalidate = useCallback(() => { ++version.current; }, []);
  useEffect(() => {
    void load();
    return invalidate;
  }, [load, invalidate]);
  usePollWhileVisible(() => lifecycle === "active" && !loading ? load() : undefined, 10000, key);
  const groups = state.key === key ? state.groups : [];
  // The API accepts at most eight continuation cursors per request. A row action
  // includes that row first; sibling continuations share the same single request.
  const continuations = groups.flatMap(g => g.older_cursor ? [g.older_cursor] : []);
  const batchKey = useRef("");
  if (continuations.length > 1) batchKey.current = key;
  function loadOlder(first?: string) {
    const cursors = first ? [first, ...continuations.filter(cursor => cursor !== first)] : continuations;
    if (!loading && cursors.length) void load(cursors.slice(0, 8));
  }
  function filter(name: string, value: string) {
    const next = new URLSearchParams(params);
    next.delete("cursor");
    next.set(name, value);
    setParams(next);
  }
  return <section aria-label="Cross-check history" className="min-w-0 space-y-4">
    <div className="flex flex-wrap gap-2" role="group" aria-label="Stage">
      {(["all", "plan", "code"] as const).map(s => <button type="button" className={cx("rounded-full border px-3 py-1 text-sm", stage === s ? "border-brand bg-raised text-fg" : "border-edge text-muted")} key={s} aria-pressed={stage === s} onClick={() => filter("stage", s)}>{s === "all" ? "All stages" : s === "plan" ? "Plan" : "Code"}</button>)}
    </div>
    {lifecycle === "past" && <div className="flex flex-wrap gap-2" role="group" aria-label="Outcome">
      {([["any", "Any outcome"], ["didnt_finish", "Didn't finish"], ["raised_issues", "Raised issues"], ["passed", "Passed"], ["replaced", "Replaced"]] as const).map(([value, title]) => <button type="button" className={cx("rounded-full border px-3 py-1 text-sm", outcome === value ? "border-brand bg-raised text-fg" : "border-edge text-muted")} key={value} aria-pressed={outcome === value} onClick={() => filter("outcome", value)}>{title}</button>)}
    </div>}
    {error && <div role="alert">{error} <button type="button" onClick={() => void load()}>Refresh cross-checks</button></div>}
    {loading && <p role="status">Loading cross-checks...</p>}
    <p className="text-sm text-muted">{groups.length} groups on this page</p>
    {!loading && !error && groups.length === 0 && <p>{stage !== "all" || outcome !== "any" ? "No attempts match these filters." : lifecycle === "active" ? "No cross-checks running right now." : "No retained past cross-checks."}</p>}
    {groups.map(g => <CheckCard key={g.parent_id} group={g} busy={loading} loadOlder={() => loadOlder(g.older_cursor)} />)}
    {batchKey.current === key && <div className="space-y-1 text-sm"><button type="button" aria-disabled={loading || !continuations.length} onClick={() => loadOlder()}>Load older attempts for visible groups</button><p className="text-muted">{continuations.length ? "Up to 8 groups per request." : "All matching attempts loaded for visible groups."}</p></div>}
    {state.key === key && state.next && <button type="button" disabled={loading} onClick={() => filter("cursor", state.next!)}>Load more groups</button>}
    {cursor && <button type="button" onClick={() => { const next = new URLSearchParams(params); next.delete("cursor"); setParams(next); }}>Newest groups</button>}
  </section>;
}
