// Admin → Health (PRD #1484 M4, reshaped into a triage page by PRD #1648 D1–D5): what uzi can
// tell about its own health, read-only. The order is FIXED (D1) and never reshuffles by state:
//
//   1. a header line: the tab's description, "Checked Xs ago" and Copy diagnostics;
//   2. the attention card (every danger/unknown/warn check, worst first, fully expanded with
//      what to do, evidence, command and docs) OR, when nothing needs attention, one quiet
//      all-clear line;
//   3. the All checks inventory: one collapsible row per group, with a chip per check (the
//      attention chips jump to their item in the attention card);
//   4. the cross-user Fleet card (id="fleet", the target of the fleet.* items' link).
//
// TWO best-effort polls, each keeping its last-good value on a failed fetch (mirroring
// CustodyBoardAlert): the health document via the shared useAdminHealth hook (10 s), and the
// fleet via api.adminListWorkers (10 s). A transient blip must never blank the page, and the
// inventory's open groups live in AllChecks state, which a new document does not remount.
//
// Severity is encoded in FORM as well as colour (PRD #1648 D6): the shared SeverityBadge
// (shape + word) and, on an inventory chip or line, InventoryMark: an aria-hidden
// SeverityShape followed by a visually hidden (sr-only) span carrying the severity word, so the
// word is in the accessible text — never colour alone. Check titles, summaries, evidence and
// commands are server-composed and render as React text only. Untrusted strings (worker
// names, upgrade details, blocking reasons) are server-sanitized, but any that reach a
// `title=` attribute are re-stripped with stripUnsafeChars first (FleetRow, BlockingCell).

import { useCallback, useEffect, useMemo, useState } from "react";

import { AdminShell } from "../components/AdminShell";
import { DocLink } from "../components/DocLink";
import { SEV, SeverityBadge, SeverityShape, sevOf, type Sev } from "../components/healthSeverity";
import { ChevronRightIcon } from "../components/icons";
import { likelyCause, upgradePresentation } from "../components/WorkerUpgradeBadge";
import { Badge, Button, Card, SectionTitle, Skeleton, cx } from "../components/ui";
import { api, type AdminWorker, type HealthCheck, type HealthDoc } from "../lib/api";
import { useDemoMode } from "../lib/demoMode";
import { maskEmail } from "../lib/demoMask";
import { attentionChecks, healthVerdict, isAttention } from "../lib/healthView";
import { formatAgo, useNow } from "../lib/rateLimits";
import { stripUnsafeChars } from "../lib/safeText";
import { useHealthStatus } from "../lib/useAdminHealth";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";

// The groups, in the order the mock and the server emit them.
const GROUPS: { id: string; title: string }[] = [
  { id: "workers", title: "Workers and capacity" },
  { id: "queue", title: "Queue" },
  { id: "control", title: "Control plane" },
  { id: "integrations", title: "Integrations" },
  { id: "housekeeping", title: "Housekeeping" },
];
const GROUP_TITLE: Record<string, string> = Object.fromEntries(GROUPS.map((g) => [g.id, g.title]));

const DESCRIPTION =
  "What uzi can tell about its own health. Read-only: nothing here restarts, retries or rolls anything back.";

// The attention severities, worst first: the band's badges and the list both read this order.
const ATTENTION_ORDER: Sev[] = ["danger", "unknown", "warn"];

// Text colour for a bare shape (the badge carries its own tone).
const SHAPE_TONE: Record<Sev, string> = {
  ok: "text-ok",
  warn: "text-warn",
  danger: "text-danger",
  unknown: "text-muted",
  na: "text-faint",
};

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

// scrollToSection is the in-page jump shared by the inventory's attention chips (to an
// attention item) and the fleet.* items' "See the affected workers" link (to the Fleet card).
// Smooth unless the user asked for reduced motion. A target that takes programmatic focus
// (tabIndex -1, the attention items) is focused too, so a keyboard user's next Tab continues
// from where they landed instead of from the chip they activated.
function scrollToSection(id: string, block: ScrollLogicalPosition) {
  const el = document.getElementById(id);
  if (!el) return;
  if (typeof el.scrollIntoView === "function") {
    el.scrollIntoView({ block, behavior: reducedMotion() ? "auto" : "smooth" });
  }
  if (el.hasAttribute("tabindex")) el.focus({ preventScroll: true });
}

export function AdminHealth() {
  const { doc } = useHealthStatus();
  const now = useNow(1000);

  // The description is rendered here, not via AdminShell's `description` prop, so the
  // "Checked" time and Copy diagnostics can share its line (D2) without changing AdminShell.
  return (
    <AdminShell description={null}>
      <HeaderLine doc={doc} nowMs={now} />
      {doc ? <HealthContent doc={doc} nowMs={now} /> : <LoadingCard />}
    </AdminShell>
  );
}

function HeaderLine({ doc, nowMs }: { doc: HealthDoc | null; nowMs: number }) {
  const [copied, setCopied] = useState(false);
  const copyDiagnostics = () => {
    if (!doc) return;
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

  return (
    <div className="flex flex-wrap items-center justify-between gap-x-6 gap-y-2">
      <p className="min-w-0 max-w-prose text-sm text-muted">{DESCRIPTION}</p>
      {doc && (
        <div className="flex shrink-0 items-center gap-3 text-xs text-faint">
          <span className="tabular-nums">Checked {formatAgo(doc.checked_at, nowMs)}</span>
          <Button variant="secondary" size="sm" aria-live="polite" onClick={copyDiagnostics}>
            {copied ? "Copied" : "Copy diagnostics"}
          </Button>
        </div>
      )}
    </div>
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
  const attention = useMemo(() => attentionChecks(doc), [doc]);

  // Three fixed slots (D1). The first swaps between the attention card and the all-clear
  // line, but it is ONE slot, so AllChecks keeps its position and its open-group state.
  return (
    <div className="space-y-5">
      {attention.length > 0 ? <AttentionCard doc={doc} attention={attention} nowMs={nowMs} /> : <AllClearLine />}
      <AllChecks doc={doc} />
      <FleetCard />
    </div>
  );
}

// ── Attention card (D3) ──────────────────────────────────────────────────────────────

function AttentionCard({ doc, attention, nowMs }: { doc: HealthDoc; attention: HealthCheck[]; nowMs: number }) {
  const danger = doc.status === "danger";
  const { title } = healthVerdict(doc.status, doc.counts);
  // The title and the band's tone come from doc.status / doc.counts (healthVerdict); the
  // severity badges and the sub line are counted from the attention list itself, so those two
  // always match the items below. The sub line is page-local on purpose: healthVerdict.sub is
  // the Overview card's.
  const count = (s: Sev) => attention.filter((c) => sevOf(c.severity) === s).length;
  const more = attention.length - count("danger");
  const sub = danger
    ? more > 0
      ? `Plus ${more} more that need attention without blocking work. Worst first, each with what to do.`
      : "Worst first, each with what to do."
    : "Work is still flowing. Worst first, each with what to do.";

  return (
    // Card always carries border-edge and cx does not merge classes; in the compiled CSS
    // .border-edge follows .border-danger/40, so the tone border needs the important modifier.
    <Card flush className={cx("overflow-hidden", danger ? "border-danger/40!" : "border-warn/40!")}>
      <header
        role={danger ? "alert" : "status"}
        className={cx(
          "flex flex-wrap items-start justify-between gap-x-4 gap-y-2 border-b px-5 py-3.5",
          danger ? "border-danger/25 bg-danger/10" : "border-warn/25 bg-warn/10",
        )}
      >
        <div className="min-w-0">
          <h2 className="flex items-center gap-2 text-sm font-semibold text-fg">
            <SeverityShape severity={danger ? "danger" : "warn"} size={10} className={danger ? "text-danger" : "text-warn"} />
            {title}
          </h2>
          <p className="mt-0.5 text-sm text-muted sm:pl-[18px]">{sub}</p>
        </div>
        <div className="flex flex-wrap gap-1.5">
          {ATTENTION_ORDER.map((s) => {
            const n = count(s);
            return n > 0 ? <SeverityBadge key={s} severity={s} count={n} /> : null;
          })}
        </div>
      </header>
      <div className="divide-y divide-edge">
        {attention.map((c) => (
          <AttentionItem key={c.id} check={c} nowMs={nowMs} />
        ))}
      </div>
    </Card>
  );
}

// AttentionItem is one check, always expanded: a triage queue item is read, not opened.
function AttentionItem({ check, nowMs }: { check: HealthCheck; nowMs: number }) {
  const group = GROUP_TITLE[check.group];
  const since = sinceLabel(check.since, nowMs);
  return (
    <div
      id={`c-${check.id}`}
      tabIndex={-1}
      className="grid scroll-mt-6 grid-cols-[auto_1fr] items-baseline gap-x-3 gap-y-2.5 px-5 py-3.5 text-sm outline-none sm:grid-cols-[4.75rem_1fr_auto]"
    >
      <span>
        <SeverityBadge severity={check.severity} />
      </span>
      <p className="min-w-0">
        <span className="font-medium text-fg">{check.title}</span> <span className="text-muted">{check.summary}</span>
        {group && <span className="text-xs text-faint"> · {group}</span>}
      </p>
      {since && <span className="col-start-2 text-xs tabular-nums text-faint sm:col-start-auto sm:text-right">{since}</span>}
      <div className="col-span-2 min-w-0 space-y-2.5 sm:col-start-2">
        {check.action && (
          <p className="max-w-prose">
            <span className="font-semibold text-fg">What to do.</span> {check.action}
          </p>
        )}
        {check.evidence.length > 0 && (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-xs">
            {check.evidence.map((e, i) => (
              <div key={i} className="contents">
                <dt className="text-faint">{e.label}</dt>
                <dd className="m-0 min-w-0 break-words font-mono text-fg">{e.value}</dd>
              </div>
            ))}
          </dl>
        )}
        {check.command && <CommandLine command={check.command} />}
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-faint">
          <span className="font-mono">{check.id}</span>
          {check.doc && <DocLink slug={check.doc}>Docs: {check.doc}</DocLink>}
          {check.id.startsWith("fleet.") && (
            <a
              href="#fleet"
              className="text-brand hover:underline"
              onClick={(e) => {
                e.preventDefault();
                scrollToSection("fleet", "start");
              }}
            >
              See the affected workers in Fleet <span aria-hidden="true">↓</span>
            </a>
          )}
        </div>
      </div>
    </div>
  );
}

function CommandLine({ command }: { command: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <div className="flex items-start gap-2">
      <pre className="console min-w-0 flex-1 overflow-x-auto rounded-lg border border-edge bg-ink px-2.5 py-1.5 font-mono text-xs text-fg">
        {command}
      </pre>
      <Button
        variant="secondary"
        size="sm"
        aria-live="polite"
        onClick={() => {
          void navigator.clipboard?.writeText(command);
          setCopied(true);
          window.setTimeout(() => setCopied(false), 1600);
        }}
      >
        {copied ? "Copied" : "Copy"}
      </Button>
    </div>
  );
}

// ── All-clear line (D4) ──────────────────────────────────────────────────────────────

function AllClearLine() {
  return (
    <Card flush className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1 px-5 py-3.5">
      <p className="flex items-center gap-2.5 text-sm">
        <span
          aria-hidden="true"
          className="inline-flex h-[18px] w-[18px] shrink-0 items-center justify-center rounded-full bg-ok/10 text-ok"
        >
          <SeverityShape severity="ok" variant="check" size={11} />
        </span>
        <span>
          <strong className="font-semibold text-fg">All systems normal.</strong>{" "}
          <span className="text-muted">Nothing needs attention.</span>
        </span>
      </p>
      <span className="text-xs text-faint">refreshes every 10 s</span>
    </Card>
  );
}

// ── All checks inventory (D5) ────────────────────────────────────────────────────────

const isApplicable = (c: HealthCheck) => sevOf(c.severity) !== "na";

function notApplicableSuffix(n: number): string {
  return n > 0 ? `, ${n} not applicable` : "";
}

// AllChecks owns the open-group state. HealthContent re-renders it with every polled
// document but never remounts it, so a group the admin opened stays open through the poll.
function AllChecks({ doc }: { doc: HealthDoc }) {
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const applicable = doc.checks.filter(isApplicable);
  const passing = applicable.filter((c) => sevOf(c.severity) === "ok").length;
  const na = doc.checks.length - applicable.length;

  return (
    <Card flush className="overflow-hidden">
      <header className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-edge px-5 py-3">
        <SectionTitle>All checks</SectionTitle>
        <span className="text-xs tabular-nums text-faint">
          {passing} of {applicable.length} passing{notApplicableSuffix(na)}
        </span>
      </header>
      <div className="divide-y divide-edge">
        {GROUPS.map((g) => (
          <GroupRow
            key={g.id}
            group={g}
            checks={doc.checks.filter((c) => c.group === g.id)}
            open={open[g.id] === true}
            onToggle={() => setOpen((o) => ({ ...o, [g.id]: !o[g.id] }))}
          />
        ))}
      </div>
    </Card>
  );
}

function GroupRow({
  group,
  checks,
  open,
  onToggle,
}: {
  group: { id: string; title: string };
  checks: HealthCheck[];
  open: boolean;
  onToggle: () => void;
}) {
  const regionId = `health-group-${group.id}`;
  return (
    <div>
      {/* The disclosure button holds only the chevron and the title; the chips (links) sit
          beside it, never inside, so the two interactions never nest. Below sm the three
          parts stack. */}
      <div className="flex flex-col gap-y-1.5 px-5 py-2.5 sm:grid sm:grid-cols-[13rem_1fr_auto] sm:items-center sm:gap-x-4">
        <button
          type="button"
          aria-expanded={open}
          aria-controls={regionId}
          onClick={onToggle}
          className="-mx-1 flex items-center gap-2 self-start rounded-md px-1 py-1 text-left text-sm font-medium text-fg transition-colors hover:bg-raised sm:self-center"
        >
          <ChevronRightIcon
            className={cx(
              "h-3.5 w-3.5 shrink-0 text-muted transition-transform motion-reduce:transition-none",
              open && "rotate-90",
            )}
          />
          {group.title}
        </button>
        <ul className="flex min-w-0 flex-wrap gap-x-4 gap-y-1 text-sm">
          {checks.map((c) => (
            <li key={c.id}>
              <CheckChip check={c} />
            </li>
          ))}
        </ul>
        <div className="sm:text-right">
          <GroupStatus checks={checks} />
        </div>
      </div>
      <div id={regionId} hidden={!open}>
        {open && (
          <ul className="px-5 pb-3 sm:pl-[2.625rem]">
            {checks.map((c) => (
              <CheckLine key={c.id} check={c} />
            ))}
          </ul>
        )}
      </div>
    </div>
  );
}

// The mark on an inventory chip or line: the quiet check for OK, the severity shape
// otherwise. The shape is decorative; the severity WORD rides along as visually hidden text
// (then a space text node), so a chip link's accessible name reads "Danger Worker image roll"
// rather than an img label glued onto the title.
function InventoryMark({ severity }: { severity: string }) {
  const s = sevOf(severity);
  return (
    <>
      <SeverityShape severity={s} variant={s === "ok" ? "check" : "shape"} className={SHAPE_TONE[s]} />
      <span className="sr-only">{SEV[s].label}</span>{" "}
    </>
  );
}

function CheckChip({ check }: { check: HealthCheck }) {
  const s = sevOf(check.severity);
  if (s === "ok" || s === "na") {
    return (
      <span className={cx("inline-flex items-center gap-1.5", s === "ok" ? "text-muted" : "text-faint")}>
        <InventoryMark severity={s} />
        {check.title}
      </span>
    );
  }
  // An attention chip jumps to its fully explained item in the attention card.
  return (
    <a
      href={`#c-${check.id}`}
      className="inline-flex items-center gap-1.5 font-medium text-fg underline decoration-edge-strong underline-offset-[3px] hover:decoration-current"
      onClick={(e) => {
        e.preventDefault();
        scrollToSection(`c-${check.id}`, "center");
      }}
    >
      <InventoryMark severity={s} />
      {check.title}
    </a>
  );
}

function GroupStatus({ checks }: { checks: HealthCheck[] }) {
  const applicable = checks.filter(isApplicable);
  const suffix = notApplicableSuffix(checks.length - applicable.length);
  if (applicable.length === 0) return <span className="text-xs text-faint">not applicable</span>;
  const attention = applicable.filter(isAttention);
  if (attention.length === 0) {
    return (
      <span className="text-xs tabular-nums text-faint">
        all {applicable.length} passing{suffix}
      </span>
    );
  }
  const anyDanger = attention.some((c) => sevOf(c.severity) === "danger");
  return (
    <Badge tone={anyDanger ? "danger" : "warning"}>
      {attention.length} of {applicable.length} need attention{suffix}
    </Badge>
  );
}

// CheckLine is one check in an opened group, in registry order: its own summary carries the
// measurements a passing check reports (DB latency, loop counts).
function CheckLine({ check }: { check: HealthCheck }) {
  return (
    <li className="grid grid-cols-[auto_1fr] items-baseline gap-x-2.5 gap-y-0.5 border-t border-dashed border-edge py-1.5 text-sm first:border-t-0 sm:grid-cols-[auto_1fr_auto]">
      <InventoryMark severity={check.severity} />
      <p className="min-w-0">
        <span className="text-fg">{check.title}</span> <span className="text-muted">{check.summary}</span>
      </p>
      <span className="col-start-2 font-mono text-xs text-faint sm:col-start-auto">{check.id}</span>
    </li>
  );
}

// FleetCard is the cross-user fleet in its own card (PRD #1484, #1648 D1/D7), the target of
// the fleet.* items' "See the affected workers" link (id="fleet"). It reads
// GET /api/admin/workers, which carries roll health, and shares the Workers page's upgrade
// words (upgradePresentation) and blocking cause (likelyCause), so the two pages cannot drift.
// Its own best-effort poll keeps the last-good rows on a failed fetch.
const FLEET_COLUMNS = ["Owner", "Worker", "Status", "Version", "Upgrade", "Blocking", "Last seen"];
const SKELETON_ROWS = 3;

function FleetCard() {
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

  const loading = workers === null;
  return (
    <Card flush id="fleet" className="scroll-mt-6 overflow-hidden">
      <header className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b border-edge px-5 py-3">
        <SectionTitle>Fleet, all users</SectionTitle>
        <span className="text-xs text-faint">Worker status and upgrade blockers across every user</span>
      </header>
      <div className="overflow-x-auto">
        <table className="w-full text-left text-sm" aria-busy={loading}>
          <thead className="border-b border-edge text-muted">
            <tr>
              {FLEET_COLUMNS.map((c) => (
                <th key={c} scope="col" className="px-4 py-3 font-medium">
                  {c}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-edge">
            {loading ? (
              Array.from({ length: SKELETON_ROWS }, (_, i) => (
                <tr key={i}>
                  {FLEET_COLUMNS.map((c) => (
                    <td key={c} className="px-4 py-3">
                      <Skeleton className="h-4 w-16" />
                    </td>
                  ))}
                </tr>
              ))
            ) : workers.length === 0 ? (
              <tr>
                <td colSpan={FLEET_COLUMNS.length} className="px-4 py-3 text-muted">
                  No workers across any user.
                </td>
              </tr>
            ) : (
              workers.map((w) => <FleetRow key={w.id} worker={w} demo={demo} />)
            )}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function FleetRow({ worker: w, demo }: { worker: AdminWorker; demo: boolean }) {
  // The worker name and the upgrade detail are server-sanitized at write time. Strip them
  // again on the way to a `title=` attribute: an attribute is a sink a rendered-text sweep
  // misses, so a bidi override there could reorder how the tooltip reads (issue #124).
  const name = stripUnsafeChars(w.name);
  const upgrade = upgradePresentation(w.upgrade_status);

  return (
    <tr>
      <td className="px-4 py-3 text-fg">{maskEmail(w.owner_email, demo)}</td>
      <td className="px-4 py-3">
        <span className="font-mono text-fg" title={name}>
          {name}
        </span>{" "}
        <span className="text-xs text-faint">{w.kind}</span>
      </td>
      <td className="px-4 py-3">
        {/* Online reads ok, anything else neutral: an upgrade failure is the Upgrade column's
            danger badge, not a second danger signal here. */}
        <Badge tone={w.status === "online" ? "ok" : "neutral"} dot>
          {w.status}
        </Badge>
      </td>
      <td className="px-4 py-3 font-mono">{w.version ?? "—"}</td>
      <td className="px-4 py-3">
        {/* `unknown` has no presentation and renders a faint dash (the Workers page badge
            renders nothing for it: an unstamped image is not a finding). */}
        {upgrade ? (
          <Badge tone={upgrade.tone} title={w.upgrade_detail ? stripUnsafeChars(w.upgrade_detail) : undefined}>
            {upgrade.label}
          </Badge>
        ) : (
          <span className="text-faint">—</span>
        )}
      </td>
      <td className="px-4 py-3">
        <BlockingCell worker={w} />
      </td>
      <td className="px-4 py-3 tabular-nums text-muted">
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
