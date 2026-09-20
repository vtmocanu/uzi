// Admin → Health (PRD #1484 M4): what uzi can tell about its own health, read-only. A
// verdict with a per-severity tally, a needs-attention list whose items scroll to their
// check, grouped checks with server-authored evidence and a what-to-do line, and — at the
// end of the Workers group — the cross-user fleet table that finally shows roll health.
//
// TWO best-effort polls, each keeping its last-good value on a failed fetch (mirroring
// CustodyBoardAlert): the health document via the shared useAdminHealth hook (10 s), and the
// fleet via api.adminListWorkers (10 s). A transient blip must never blank the page.
//
// Severity is encoded in FORM as well as colour: a distinct glyph and a text label per
// severity (SeverityPill), and the tab pip's count is text with a worded aria-label — never
// colour alone. Untrusted strings (worker names, blocking reasons) are server-sanitized, but
// any that reach a `title=` attribute are re-stripped with stripUnsafeChars first.

import { useCallback, useEffect, useMemo, useState } from "react";

import { AdminShell } from "../components/AdminShell";
import { DocLink } from "../components/DocLink";
import { WorkerUpgradeBadge, likelyCause } from "../components/WorkerUpgradeBadge";
import { Card, cx } from "../components/ui";
import { api, type AdminWorker, type HealthCheck, type HealthDoc } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { formatAgo, useNow } from "../lib/rateLimits";
import { stripUnsafeChars } from "../lib/safeText";
import { useAdminHealth } from "../lib/useAdminHealth";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";

type Sev = "ok" | "warn" | "danger" | "unknown" | "na";

// Per-severity presentation: a glyph (form), a word (accessible text), and the pill classes.
// The glyph is aria-hidden; the word is the accessible name a test asserts on.
const SEV: Record<Sev, { label: string; glyph: string; pill: string }> = {
  ok: { label: "OK", glyph: "●", pill: "border-ok/30 bg-ok/10 text-ok" },
  warn: { label: "Warn", glyph: "▲", pill: "border-warn/40 bg-warn/10 text-warn" },
  danger: { label: "Danger", glyph: "◆", pill: "border-danger/45 bg-danger/10 text-danger" },
  unknown: { label: "Unknown", glyph: "?", pill: "border-dashed border-edge-strong bg-raised text-muted" },
  na: { label: "N/A", glyph: "○", pill: "border-edge bg-transparent text-faint" },
};

function sevOf(s: string): Sev {
  return (s in SEV ? s : "unknown") as Sev;
}

// The groups, in the order the mock and the server emit them.
const GROUPS: { id: string; title: string }[] = [
  { id: "workers", title: "Workers and capacity" },
  { id: "queue", title: "Queue" },
  { id: "control", title: "Control plane" },
  { id: "integrations", title: "Integrations" },
  { id: "housekeeping", title: "Housekeeping" },
];

// Attention = every non-ok, non-na check, worst first (danger, unknown, warn).
const RANK: Record<string, number> = { danger: 0, unknown: 1, warn: 2 };
function isAttention(c: HealthCheck): boolean {
  return c.severity !== "ok" && c.severity !== "na";
}

function verdictText(status: string): { title: string; sub: string } {
  if (status === "danger") {
    return {
      title: "uzi has a blocking problem",
      sub: "One or more checks are failing. Start with the flagged checks below.",
    };
  }
  if (status === "warn" || status === "unknown") {
    return {
      title: "Some checks need attention",
      sub: "Work is still flowing. These are the quiet no-ops that otherwise only surface as a log line.",
    };
  }
  return {
    title: "All systems normal",
    sub: "Every check uzi can make about itself is passing.",
  };
}

// "for 38m" from an RFC3339 `since`, or empty when the check carries none.
function sinceLabel(since: string | null, nowMs: number): string {
  if (!since) return "";
  return `for ${formatAgo(since, nowMs).replace(/ ago$/, "")}`;
}

function reducedMotion(): boolean {
  try {
    return window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  } catch {
    return false;
  }
}

export function AdminHealth() {
  const doc = useAdminHealth();
  const now = useNow(1000);

  return (
    <AdminShell description="What uzi can tell about its own health. Read-only: nothing here restarts, retries or rolls anything back.">
      {doc ? <HealthContent doc={doc} nowMs={now} /> : <LoadingCard />}
    </AdminShell>
  );
}

function LoadingCard() {
  return (
    <Card className="animate-pulse space-y-3">
      <div className="h-5 w-56 rounded bg-raised" />
      <div className="h-4 w-80 rounded bg-raised" />
    </Card>
  );
}

function HealthContent({ doc, nowMs }: { doc: HealthDoc; nowMs: number }) {
  const attention = useMemo(
    () => doc.checks.filter(isAttention).sort((a, b) => (RANK[a.severity] ?? 9) - (RANK[b.severity] ?? 9)),
    [doc.checks],
  );

  return (
    <div className="space-y-5">
      <VerdictCard doc={doc} attention={attention} nowMs={nowMs} />
      {GROUPS.map((g) => (
        <GroupCard key={g.id} group={g} checks={doc.checks.filter((c) => c.group === g.id)} nowMs={nowMs} />
      ))}
    </div>
  );
}

// jumpToCheck opens the target check and scrolls it into view — the needs-attention list's
// items are in-page anchors. The <details> is uncontrolled DOM, so this sets `open` directly.
function jumpToCheck(id: string) {
  const el = document.getElementById(`c-${id}`) as HTMLDetailsElement | null;
  if (!el) return;
  el.open = true;
  if (typeof el.scrollIntoView === "function") {
    el.scrollIntoView({ block: "center", behavior: reducedMotion() ? "auto" : "smooth" });
  }
}

function VerdictCard({ doc, attention, nowMs }: { doc: HealthDoc; attention: HealthCheck[]; nowMs: number }) {
  const v = verdictText(doc.status);
  const glyphSev = sevOf(doc.status === "danger" ? "danger" : doc.status === "ok" ? "ok" : "warn");
  const glyphColor = { ok: "text-ok", warn: "text-warn", danger: "text-danger", unknown: "text-muted", na: "text-faint" }[glyphSev];

  const [copied, setCopied] = useState(false);
  const copyDiagnostics = () => {
    // The whole document as JSON — the exact shape a bug report or a `uzi admin health --json`
    // probe carries. Best-effort: a blocked clipboard is silent, matching WorkerUpgradeDetail.
    void navigator.clipboard?.writeText(JSON.stringify(doc, null, 2)).then(
      () => {
        setCopied(true);
        window.setTimeout(() => setCopied(false), 1600);
      },
      () => {},
    );
  };

  const order: Sev[] = ["danger", "unknown", "warn", "ok", "na"];

  return (
    <Card className="space-y-4">
      <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-3">
        <div className="flex min-w-0 items-start gap-3">
          <span aria-hidden="true" className={cx("text-2xl leading-tight", glyphColor)}>
            {SEV[glyphSev].glyph}
          </span>
          <div className="min-w-0">
            <h2 className="text-lg font-semibold tracking-tight">{v.title}</h2>
            <p className="mt-0.5 max-w-prose text-sm text-muted">{v.sub}</p>
          </div>
        </div>
        <div className="flex flex-wrap gap-1.5">
          {order.map((k) =>
            doc.counts[k] > 0 ? <TallyPill key={k} sev={k} count={doc.counts[k]} /> : null,
          )}
        </div>
      </div>

      {attention.length > 0 && (
        <ul className="space-y-1.5">
          {attention.map((c) => (
            <li key={c.id} className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
              <SeverityPill severity={c.severity} />
              <span className="min-w-0 flex-1">
                <a
                  href={`#c-${c.id}`}
                  className="font-medium text-fg hover:underline"
                  onClick={(e) => {
                    e.preventDefault();
                    jumpToCheck(c.id);
                  }}
                >
                  {c.title}
                </a>{" "}
                <span className="text-muted">{c.summary}</span>
              </span>
              {c.since && <span className="shrink-0 text-xs tabular-nums text-faint">{sinceLabel(c.since, nowMs)}</span>}
            </li>
          ))}
        </ul>
      )}

      <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-edge pt-3 text-xs text-faint">
        <span>Checked {formatAgo(doc.checked_at, nowMs)}, refreshes every 10 s</span>
        <span className="tabular-nums">{doc.checks.length} checks</span>
        <button
          type="button"
          onClick={copyDiagnostics}
          aria-live="polite"
          className="rounded-lg border border-edge-strong px-2.5 py-1 text-xs text-fg transition-colors hover:bg-raised"
        >
          {copied ? "Copied" : "Copy diagnostics"}
        </button>
      </div>
    </Card>
  );
}

function SeverityPill({ severity }: { severity: string }) {
  const s = sevOf(severity);
  const m = SEV[s];
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold whitespace-nowrap",
        m.pill,
      )}
    >
      <span aria-hidden="true" className="w-2.5 text-center text-[10px]">
        {m.glyph}
      </span>
      {m.label}
    </span>
  );
}

function TallyPill({ sev, count }: { sev: Sev; count: number }) {
  const m = SEV[sev];
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-xs font-semibold whitespace-nowrap",
        m.pill,
      )}
    >
      <span aria-hidden="true" className="w-2.5 text-center text-[10px]">
        {m.glyph}
      </span>
      <span className="tabular-nums">{count}</span> {m.label}
    </span>
  );
}

function GroupCard({
  group,
  checks,
  nowMs,
}: {
  group: { id: string; title: string };
  checks: HealthCheck[];
  nowMs: number;
}) {
  const attention = checks.filter(isAttention).length;
  return (
    <Card className="overflow-hidden p-0">
      <header className="flex items-baseline justify-between gap-3 border-b border-edge px-5 py-3">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">{group.title}</h2>
        <span className="text-xs tabular-nums text-faint">
          {attention > 0 ? `${attention} need attention` : "all passing"}
        </span>
      </header>
      {checks.map((c) => (
        <CheckRow key={c.id} check={c} nowMs={nowMs} />
      ))}
      {group.id === "workers" && <FleetTable />}
    </Card>
  );
}

function CheckRow({ check, nowMs }: { check: HealthCheck; nowMs: number }) {
  const s = sevOf(check.severity);
  // danger and unknown render EXPANDED (they are what an admin needs to read); the rest are
  // collapsible. `open` stays a stable value across re-renders, so React never re-applies it
  // and a user's manual toggle (or a jump-to-check) persists through the 10 s poll.
  const expanded = s === "danger" || s === "unknown";
  const rowBg = s === "danger" ? "bg-danger/5" : s === "warn" ? "bg-warn/5" : "";

  return (
    <details id={`c-${check.id}`} open={expanded} className={cx("group border-t border-edge first:border-t-0", rowBg)}>
      <summary className="grid cursor-pointer list-none grid-cols-[auto_1fr_auto] items-baseline gap-x-3 gap-y-1 px-5 py-2.5 hover:bg-raised/60 [&::-webkit-details-marker]:hidden">
        <SeverityPill severity={check.severity} />
        <span className="min-w-0">
          <span className="font-medium text-fg">{check.title}</span>{" "}
          <span className="text-muted">{check.summary}</span>
        </span>
        <span className="shrink-0 text-xs tabular-nums text-faint">{sinceLabel(check.since, nowMs)}</span>
      </summary>
      <div className="space-y-3 px-5 pb-4 pl-5 text-sm sm:pl-[7.5rem]">
        {check.action && (
          <p className="max-w-prose">
            <span className="font-semibold">What to do.</span> {check.action}
          </p>
        )}
        {check.evidence.length > 0 && (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
            {check.evidence.map((e, i) => (
              <div key={i} className="contents">
                <dt className="text-faint">{e.label}</dt>
                <dd className="m-0 break-words font-mono text-fg">{e.value}</dd>
              </div>
            ))}
          </dl>
        )}
        {check.command && <CommandLine command={check.command} />}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-faint">
          <span className="font-mono">{check.id}</span>
          {check.doc && (
            <span>
              <DocLink slug={check.doc}>Docs: {check.doc}</DocLink>
            </span>
          )}
        </div>
      </div>
    </details>
  );
}

function CommandLine({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-stretch gap-2">
      <pre className="console min-w-0 flex-1 overflow-x-auto rounded-lg border border-edge bg-ink px-2.5 py-1.5 font-mono text-xs text-fg">
        {command}
      </pre>
      <button
        type="button"
        aria-live="polite"
        onClick={() => {
          void navigator.clipboard?.writeText(command);
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1600);
        }}
        className="shrink-0 rounded-lg border border-edge-strong px-2.5 text-xs text-fg transition-colors hover:bg-raised"
      >
        {copied ? "Copied" : "Copy"}
      </button>
    </div>
  );
}

// FleetTable is the cross-user fleet the Workers group ends with (PRD #1484). It reads
// GET /api/admin/workers, which now carries roll health, and reuses the Workers page's
// upgrade rendering (WorkerUpgradeBadge / likelyCause) so the Upgrade and Blocking cells
// match. Its own best-effort poll keeps the last-good rows on a failed fetch.
function FleetTable() {
  const [workers, setWorkers] = useState<AdminWorker[] | null>(null);
  const demo = useDemoMode();

  const load = useCallback(async () => {
    try {
      setWorkers((await api.adminListWorkers()).workers);
    } catch {
      // Best-effort: keep the last-good fleet (or stay in the loading state on first load).
    }
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  return (
    <div className="border-t border-edge">
      <header className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 px-5 pb-1 pt-3">
        <h2 className="text-xs font-semibold uppercase tracking-wider text-faint">Fleet, all users</h2>
        <span className="text-xs text-faint">Upgrade and Blocking read the roll health the admin list now carries</span>
      </header>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-sm">
          <thead>
            <tr className="border-b border-edge text-xs uppercase tracking-wide text-faint">
              <th className="px-3 py-2 pl-5 font-semibold">Owner</th>
              <th className="px-3 py-2 font-semibold">Worker</th>
              <th className="px-3 py-2 font-semibold">Kind</th>
              <th className="px-3 py-2 font-semibold">Status</th>
              <th className="px-3 py-2 font-semibold">Version</th>
              <th className="px-3 py-2 font-semibold">Upgrade</th>
              <th className="px-3 py-2 font-semibold">Blocking</th>
              <th className="px-3 py-2 pr-5 font-semibold">Since</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-edge">
            {workers === null ? (
              <tr>
                <td colSpan={8} className="px-5 py-4 text-xs text-faint">
                  Loading fleet…
                </td>
              </tr>
            ) : workers.length === 0 ? (
              <tr>
                <td colSpan={8} className="px-5 py-4 text-xs text-faint">
                  No workers across any user.
                </td>
              </tr>
            ) : (
              workers.map((w) => <FleetRow key={w.id} worker={w} demo={demo} />)
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function FleetRow({ worker: w, demo }: { worker: AdminWorker; demo: boolean }) {
  // The worker name is owner-supplied (server-sanitized at write time). Strip it again on the
  // way to the `title=` attribute — an attribute is a sink a rendered-text sweep misses, so a
  // bidi override there could reorder how the tooltip reads (issue #124).
  const name = stripUnsafeChars(w.name);
  const statusTone =
    w.upgrade_status === "upgrade_failed"
      ? "text-danger"
      : w.status === "offline"
        ? "text-faint"
        : "text-ok";

  return (
    <tr>
      <td className="px-3 py-2 pl-5">{maskEmail(w.owner_email, demo)}</td>
      <td className="px-3 py-2">
        <span className="font-mono" title={name}>
          {name}
        </span>
      </td>
      <td className="px-3 py-2">{w.kind}</td>
      <td className="px-3 py-2">
        <span className={cx("inline-flex items-center gap-1.5", statusTone)}>
          <span aria-hidden="true" className="h-1.5 w-1.5 rounded-full bg-current" />
          {w.status}
        </span>
      </td>
      <td className="px-3 py-2 font-mono">{w.version ?? "—"}</td>
      <td className="px-3 py-2">
        <WorkerUpgradeBadge worker={w} />
      </td>
      <td className="px-3 py-2">
        <BlockingCell worker={w} />
      </td>
      <td className="px-3 py-2 pr-5 text-xs tabular-nums text-faint">
        {w.last_heartbeat_at ? formatAgo(w.last_heartbeat_at) : "—"}
      </td>
    </tr>
  );
}

function BlockingCell({ worker: w }: { worker: AdminWorker }) {
  if (w.upgrade_status !== "upgrade_failed" || !w.upgrade_blocking_reason) {
    return <span className="text-faint">none</span>;
  }
  const container = stripUnsafeChars(w.upgrade_blocking_container ?? "");
  const reason = stripUnsafeChars(w.upgrade_blocking_reason);
  const text = container ? `${container}: ${reason}` : reason;
  // The human cause is web-side product copy keyed on the exact k8s reason enum (likelyCause);
  // a hostile reason matches no enum and yields null, so the title falls back to the stripped
  // container:reason — the untrusted value never reaches the attribute unstripped.
  const cause = likelyCause(w.upgrade_blocking_container ?? null, w.upgrade_blocking_reason ?? null, w.upgrade_last_exit_code);
  return (
    <span className="font-mono text-danger" title={cause ?? text}>
      {text}
    </span>
  );
}
