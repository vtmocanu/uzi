import { useRef, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";

import {
  api,
  type AdminMaintenance,
  type MaintenanceCategory,
  type MaintenanceIdleWorker,
  type MaintenanceRunItem,
} from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { formatLocalTime } from "../../lib/budget";
import { useDemoMode } from "../../lib/demoMode";
import { maskEmail, maskName, maskRepoPath } from "../../lib/demoMask";
import { formatElapsed } from "../../lib/runBadge";
import { stripDisplayCell } from "../../lib/safeText";
import { useAsyncData } from "../../lib/useAsyncData";
import { useMaintenance } from "../../lib/useMaintenance";
import { Alert, Badge, Button, Card, Field, Input, SectionTitle, Skeleton, Toggle, cx } from "../../components/ui";

// Mirrors the server's reason cap (PRD #3454); the API rechecks every write.
const REASON_MAX = 280;

// The readout's blocker categories, in the order an admin works through them. Each blocks
// "clear"; queued and idle-bound workers are informational and wait for the lift.
const BLOCKERS = [
  { key: "active", label: "Running", empty: "No run is claimed or running." },
  { key: "held", label: "Held", empty: "No run is parked or waiting." },
  { key: "custody", label: "Custody holds", empty: "No recovery custody hold is open." },
] as const;

// asOfTime renders the readout instant with seconds: two refreshes a minute apart must read
// differently, or the "refresh once more" step cannot be seen to have happened.
function asOfTime(iso: string): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return iso;
  return new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

// Age is how long a blocker has been open ("34m", "2h 20m"), the clock time in its tooltip.
function Age({ since }: { since: string | null }) {
  const t = since ? Date.parse(since) : NaN;
  if (!since || !Number.isFinite(t)) return <span className="text-faint">unknown</span>;
  return <span title={`Since ${formatLocalTime(since) ?? since}`}>{formatElapsed(Date.now() - t)}</span>;
}

// maskLabel masks a person label in demo mode: the server's label is a display name or, when
// a user has none, an email, and each shape has its own mask.
function maskLabel(label: string, demo: boolean): string {
  return label.includes("@") ? maskEmail(label, demo) : maskName(label, demo);
}

// shortRunId keeps a UUID scannable in a dense table (its first 8 characters, the full id in
// the tooltip); any other id shape is shown whole.
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function shortRunId(id: string): string {
  return UUID.test(id) ? id.slice(0, 8) : id;
}

function TagBadge({ tag }: { tag: MaintenanceRunItem["tag"] }) {
  if (tag === "needs_owner") {
    return (
      <Badge tone="warning" title="Waits for its owner to act: approve, answer, resume or decide on the hold.">
        needs owner
      </Badge>
    );
  }
  if (tag === "progressing") {
    return (
      <Badge tone="info" title="Will move on by itself: a limit resets, a worker frees up or a retry runs.">
        progressing
      </Badge>
    );
  }
  return null;
}

function Capped({ category }: { category: MaintenanceCategory<unknown> }) {
  if (category.count <= category.items.length) return null;
  return (
    <p className="text-xs text-faint">
      Showing {category.items.length} of {category.count}.
    </p>
  );
}

function RunTable({ items, label }: { items: MaintenanceRunItem[]; label: string }) {
  const demo = useDemoMode();
  return (
    <div className="overflow-x-auto rounded-lg border border-edge">
      <table className="w-full min-w-[36rem] whitespace-nowrap text-left text-sm">
        <caption className="sr-only">{label}</caption>
        <thead className="bg-raised/40 text-xs text-muted">
          <tr>
            <th scope="col" className="px-3 py-1.5 font-medium">Run</th>
            <th scope="col" className="px-3 py-1.5 font-medium">Kind</th>
            <th scope="col" className="px-3 py-1.5 font-medium">Status</th>
            <th scope="col" className="px-3 py-1.5 font-medium">Owner</th>
            <th scope="col" className="px-3 py-1.5 font-medium">Repo</th>
            <th scope="col" className="px-3 py-1.5 font-medium">Open for</th>
            <th scope="col" className="px-3 py-1.5 font-medium"><span className="sr-only">Owner action</span></th>
          </tr>
        </thead>
        <tbody className="divide-y divide-edge">
          {items.map((it) => (
            <tr key={`${it.run_id}:${it.hold_id ?? ""}`} data-maintenance-run={it.run_id}>
              <td className="px-3 py-1.5">
                <Link
                  to={`/runs/${encodeURIComponent(it.run_id)}`}
                  title={it.run_id}
                  className="font-mono text-xs text-info hover:underline focus-visible:underline"
                >
                  {shortRunId(it.run_id)}
                </Link>
              </td>
              <td className="px-3 py-1.5 text-muted">{stripDisplayCell(it.kind)}</td>
              <td className="px-3 py-1.5 text-muted">{stripDisplayCell(it.status).replace(/_/g, " ")}</td>
              <td className="px-3 py-1.5">{maskLabel(stripDisplayCell(it.owner_label), demo)}</td>
              <td className="max-w-[12rem] truncate px-3 py-1.5 text-muted">
                {it.repo ? maskRepoPath(stripDisplayCell(it.repo), demo) : <span className="text-faint">none</span>}
              </td>
              <td className="whitespace-nowrap px-3 py-1.5 text-muted"><Age since={it.since} /></td>
              <td className="px-3 py-1.5 text-right"><TagBadge tag={it.tag} /></td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function WorkerTable({ items }: { items: MaintenanceIdleWorker[] }) {
  const demo = useDemoMode();
  return (
    <ul className="divide-y divide-edge rounded-lg border border-edge text-sm">
      {items.map((w) => (
        <li key={w.worker_id} className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 px-3 py-1.5">
          <span className="font-mono text-xs">{stripDisplayCell(w.worker_id)}</span>
          <span className="text-muted">{maskLabel(stripDisplayCell(w.owner_label), demo)}</span>
          <span className="text-muted">
            waits to claim{" "}
            <Link to={`/runs/${encodeURIComponent(w.run_id)}`} className="font-mono text-xs text-info hover:underline">
              {shortRunId(w.run_id)}
            </Link>
          </span>
          <span className="ml-auto text-xs text-faint"><Age since={w.since} /></span>
        </li>
      ))}
    </ul>
  );
}

// Counter is one cell of the drain strip. A blocker with work left is tinted warn; a zero
// blocker reads quiet. Informational counters never tint: they do not gate "clear".
function Counter({ label, count, blocker }: { label: string; count: number; blocker: boolean }) {
  const live = blocker && count > 0;
  return (
    <div className="min-w-[5.5rem]">
      <p className="text-xs text-muted">{label}</p>
      <p className={cx("mt-0.5 text-2xl font-semibold tabular-nums", live ? "text-warn" : blocker ? "text-fg" : "text-muted")}>
        {count}
      </p>
    </div>
  );
}

// MaintenanceCard is the admin surface for instance maintenance mode (PRD #3454): the switch,
// the plain-text reason every user sees in the banner, and the drain readout that says when
// it is safe to evict nodes. The web is the only writer (D6); PUT is admin + CSRF.
export function MaintenanceCard() {
  const { reload: reloadBanner } = useMaintenance();
  const demo = useDemoMode();
  const [data, setData] = useState<AdminMaintenance | null>(null);
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState<"toggle" | "reason" | null>(null);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  // A Refresh GET that a PUT overtakes must not overwrite the PUT's newer state when it
  // resolves late. writeGen moves when a write starts; a read keeps its outcome (data or
  // error) only if no write started since it began. A read cannot start during a write:
  // Refresh is disabled while one is busy, and the switch only renders once data loaded.
  const writeGen = useRef(0);
  // The last server reason the card adopted. Refresh compares the field against it at
  // resolve time, so text typed while its GET was in flight is never replaced.
  const savedReasonRef = useRef("");

  const adopt = (resp: AdminMaintenance) => {
    savedReasonRef.current = resp.state.reason ?? "";
    setData(resp);
    setReason(savedReasonRef.current);
  };

  const { loading, error: loadError, reload } = useAsyncData(
    async ({ isCurrent }) => {
      const resp = await api.getAdminMaintenance();
      if (!isCurrent()) return;
      adopt(resp);
    },
    [],
    { fallback: "Failed to load maintenance mode", skeleton: "always" },
  );

  const savedReason = data?.state.reason ?? "";
  const reasonDirty = reason !== savedReason;

  const write = async (active: boolean, kind: "toggle" | "reason") => {
    setError("");
    setNotice("");
    setBusy(kind);
    writeGen.current++;
    try {
      const resp = await api.setMaintenance(active, reason.trim());
      adopt(resp);
      reloadBanner();
      setNotice(
        kind === "reason"
          ? "Reason saved."
          : resp.state.active
            ? "Maintenance mode is on. New runs wait in the queue."
            : "Maintenance mode is off. Queued runs start as workers free up.",
      );
    } catch (err) {
      setError(errorMessage(err, "Failed to update maintenance mode"));
    } finally {
      setBusy(null);
    }
  };

  const refresh = async () => {
    setError("");
    setRefreshing(true);
    const gen = writeGen.current;
    try {
      const resp = await api.getAdminMaintenance();
      // A write started meanwhile: its response is newer than this read.
      if (writeGen.current !== gen) return;
      const prevSaved = savedReasonRef.current;
      const next = resp.state.reason ?? "";
      savedReasonRef.current = next;
      setData(resp);
      // Keep an in-progress reason edit, including one typed while this GET was in flight:
      // adopt the server's reason only when the field still holds the last saved one.
      setReason((cur) => (cur === prevSaved ? next : cur));
    } catch (err) {
      // A superseded read's failure is moot: the write's response is the current state.
      if (writeGen.current !== gen) return;
      setError(errorMessage(err, "Failed to refresh the readout"));
    } finally {
      setRefreshing(false);
    }
  };

  const saveReason = (e: FormEvent) => {
    e.preventDefault();
    if (!data?.state.active || !reasonDirty || busy) return;
    void write(true, "reason");
  };

  const state = data?.state;
  const readout = data?.readout;
  const since = formatLocalTime(state?.since);
  const by = state?.by_label ? maskLabel(stripDisplayCell(state.by_label), demo) : "";
  const blockersLeft = readout ? readout.active.count + readout.held.count + readout.custody.count : 0;

  return (
    <Card className="space-y-5">
      <div>
        <SectionTitle>Maintenance mode</SectionTitle>
        <p className="mt-2 text-sm text-muted">
          Drain uzi before infrastructure work. While it is on, new runs wait in the queue instead of
          starting, and runs that already started finish. Every user sees a banner with your reason.
          Turn it on, wait until the readout below is clear, then evict nodes.
        </p>
      </div>

      {(error || loadError) && <Alert message={error || loadError} />}
      {notice && <Alert tone="success" message={notice} />}

      {loading ? (
        <Skeleton className="h-32 w-full" />
      ) : !data || !state || !readout ? (
        <div className="space-y-3">
          {!error && !loadError && <Alert message="Failed to load maintenance mode." />}
          <Button variant="secondary" size="sm" onClick={() => void reload()}>
            Retry
          </Button>
        </div>
      ) : (
        <>
          {/* The switch and the reason: one form, so Enter in the reason field saves it. */}
          <form onSubmit={saveReason} className="space-y-4">
            <div className="flex items-start gap-3">
              <Toggle
                checked={state.active}
                onChange={(next) => void write(next, "toggle")}
                disabled={busy !== null}
                focusableWhenDisabled
                label="Maintenance mode"
                aria-describedby="maintenance-state"
              />
              <p id="maintenance-state" className="text-sm">
                {state.active ? (
                  <>
                    <span className="font-medium text-fg">On</span>
                    <span className="text-muted">
                      {since && ` since ${since}`}
                      {by && `, turned on by ${by}`}
                    </span>
                  </>
                ) : (
                  <>
                    <span className="font-medium text-fg">Off</span>
                    <span className="text-muted">. Runs start as soon as a worker is free.</span>
                  </>
                )}
              </p>
            </div>
            <Field label="Reason shown to every user" htmlFor="maintenance-reason">
              <Input
                id="maintenance-reason"
                value={reason}
                maxLength={REASON_MAX}
                placeholder="Node pool upgrade, back by 15:00"
                aria-describedby="maintenance-reason-count"
                onChange={(e) => {
                  setReason(e.target.value);
                  setNotice("");
                }}
              />
            </Field>
            <div className="flex flex-wrap items-center gap-3">
              {state.active && (
                <Button type="submit" size="sm" disabled={!reasonDirty || busy !== null}>
                  {busy === "reason" ? "Saving…" : "Save reason"}
                </Button>
              )}
              <span id="maintenance-reason-count" className="text-xs text-faint">
                {reason.length}/{REASON_MAX} characters.
                {!state.active && " Used when you turn maintenance mode on."}
              </span>
            </div>
          </form>

          {/* The drain readout: the clear verdict first, then the strip, then the blockers. */}
          <div className="space-y-4 border-t border-edge pt-4">
            <div className="flex flex-wrap items-start justify-between gap-3">
              <div aria-live="polite" className="min-w-0">
                {readout.clear ? (
                  <p className="flex items-center gap-2 text-sm font-medium text-ok" data-maintenance-clear="true">
                    <span aria-hidden="true" className="inline-block h-2 w-2 rounded-full bg-ok" />
                    Clear: no running or held work (as of {asOfTime(readout.as_of)})
                  </p>
                ) : (
                  <p className="flex items-center gap-2 text-sm font-medium text-warn" data-maintenance-clear="false">
                    <span aria-hidden="true" className="inline-block h-2 w-2 rounded-full bg-warn" />
                    {/* "clear" is the server's verdict; a blocker it counts that the lists do not
                        (a claim landing between the reads) must not read as "0 blockers left". */}
                    {blockersLeft > 0
                      ? `Not yet clear: ${blockersLeft} ${blockersLeft === 1 ? "blocker" : "blockers"} left (as of ${asOfTime(readout.as_of)})`
                      : `Not yet clear (as of ${asOfTime(readout.as_of)})`}
                  </p>
                )}
                <p className="mt-1 text-xs text-muted">
                  Refresh once more right before evicting nodes: a claim in flight at enable can still appear.
                </p>
              </div>
              <Button variant="secondary" size="sm" onClick={() => void refresh()} disabled={refreshing || busy !== null}>
                {refreshing ? "Refreshing…" : "Refresh"}
              </Button>
            </div>

            {/* The drain strip: what must reach zero before eviction, then what simply waits
                for the lift. The two group captions are the point: only the first gates clear. */}
            <div className="flex flex-col gap-4 rounded-xl border border-edge bg-raised/40 p-4 sm:flex-row sm:gap-6">
              <div role="group" aria-labelledby="maintenance-blockers" className="flex-[3]">
                <p id="maintenance-blockers" className="mb-2 text-xs text-faint">Must reach zero before eviction</p>
                <div className="flex flex-wrap gap-x-6 gap-y-3">
                  <Counter label="Running" count={readout.active.count} blocker />
                  <Counter label="Held" count={readout.held.count} blocker />
                  <Counter label="Custody holds" count={readout.custody.count} blocker />
                </div>
              </div>
              <div
                role="group"
                aria-labelledby="maintenance-waiting"
                className="flex-[2] border-t border-edge pt-4 sm:border-l sm:border-t-0 sm:pl-6 sm:pt-0"
              >
                <p id="maintenance-waiting" className="mb-2 text-xs text-faint">Waits until maintenance ends</p>
                <div className="flex flex-wrap gap-x-6 gap-y-3">
                  <Counter label="Queued" count={readout.queued.count} blocker={false} />
                  <Counter label="Idle bound workers" count={readout.idle_bound_workers.count} blocker={false} />
                </div>
              </div>
            </div>

            {BLOCKERS.map(({ key, label, empty }) => {
              const cat = readout[key];
              return (
                <section key={key} aria-label={label} className="space-y-2">
                  <h3 className="text-sm font-medium text-fg">
                    {label} <span className="font-normal text-muted">({cat.count})</span>
                  </h3>
                  {cat.count === 0 ? (
                    <p className="text-sm text-faint">{empty}</p>
                  ) : (
                    <>
                      <RunTable items={cat.items} label={label} />
                      <Capped category={cat} />
                    </>
                  )}
                </section>
              );
            })}

            {/* Informational: progressive disclosure, they never gate "clear". */}
            {readout.queued.count > 0 && (
              <details className="group space-y-2">
                <summary className="cursor-pointer text-sm font-medium text-fg">
                  Queued until maintenance ends <span className="font-normal text-muted">({readout.queued.count})</span>
                </summary>
                <div className="mt-2 space-y-2">
                  <RunTable items={readout.queued.items} label="Queued until maintenance ends" />
                  <Capped category={readout.queued} />
                </div>
              </details>
            )}
            {readout.idle_bound_workers.count > 0 && (
              <details className="space-y-2">
                <summary className="cursor-pointer text-sm font-medium text-fg">
                  Idle workers bound to a queued run{" "}
                  <span className="font-normal text-muted">({readout.idle_bound_workers.count})</span>
                </summary>
                <div className="mt-2 space-y-2">
                  <p className="text-xs text-muted">
                    They hold no work and are safe to evict; each claims its run after the lift.
                  </p>
                  <WorkerTable items={readout.idle_bound_workers.items} />
                  <Capped category={readout.idle_bound_workers} />
                </div>
              </details>
            )}
          </div>
        </>
      )}
    </Card>
  );
}
