// AdminShell: one header + tab bar shared by the admin surfaces (Users,
// Rate limits, Tool allowlist, Blocked repos, Instance, Branding, Health). The same
// treatment SettingsShell gives the user-scoped settings: the pages used to be separate
// top-level sidebar entries wired together only by proximity, which crowded the
// sidebar for admins and hid that they are one area — this instance's controls.
// The sidebar now carries a single "Admin" entry; the tabs carry the rest.

import { useEffect, useRef } from "react";
import { NavLink, useLocation } from "react-router-dom";
import type { ReactNode } from "react";
import { cx, PageHeader } from "./ui";
import { useAdminHealth, healthAttentionCount } from "../lib/useAdminHealth";

// Health is LAST (PRD #1484 D1), after Branding — its entry points are the sidebar pip
// (M5) and the Overview card (M5), not tab position. `pip: true` marks the one tab that
// carries the health severity pip when checks need attention.
const TABS = [
  { to: "/admin/users", label: "Users" },
  { to: "/admin/rate-limits", label: "Rate limits" },
  { to: "/admin/tool-allowlist", label: "Tool allowlist" },
  { to: "/admin/blocked-repos", label: "Blocked repos" },
  { to: "/admin/settings", label: "Instance" },
  { to: "/admin/branding", label: "Branding" },
  { to: "/admin/health", label: "Health", pip: true },
];

export function AdminShell({ description, children }: { description: ReactNode; children: ReactNode }) {
  // The tab pip must show on EVERY admin tab, not only while viewing Health, so AdminShell
  // (which wraps every admin page) owns the health read. Best-effort, last-good on failure,
  // 10 s cadence — the same shared hook the Health page uses (PRD #1484 M4). Double-fetch
  // is cheap: the endpoint caches one evaluation for 5 s.
  const health = useAdminHealth();
  const attention = health ? healthAttentionCount(health) : 0;
  // Danger dominates the pip colour, else warn (warn covers unknown, which ranks as warn).
  const pipDanger = (health?.counts.danger ?? 0) > 0;

  // On a narrow tab strip the row scrolls (overflow-x-auto); keep the ACTIVE tab in view so
  // Health — the last, rightmost tab — is reachable without a horizontal scroll the user
  // has to discover. Re-run when the route changes so switching tabs re-centres.
  const stripRef = useRef<HTMLDivElement>(null);
  const { pathname } = useLocation();
  useEffect(() => {
    const active = stripRef.current?.querySelector('[aria-current="page"]');
    if (active && typeof (active as HTMLElement).scrollIntoView === "function") {
      (active as HTMLElement).scrollIntoView({ block: "nearest", inline: "nearest" });
    }
  }, [pathname]);

  return (
    <div className="space-y-6">
      {/* The header description is CONSTANT across tabs, deliberately: the per-tab
          description used to render here, and its varying line count moved the tab
          strip up and down on every switch (round-3 feedback: "the tabs jump").
          Everything above the strip is now byte-identical whichever tab is active;
          the per-tab sentence renders below the strip, where varying height is
          ordinary content flow. */}
      <PageHeader title="Admin" description="Configuration and controls for this uzi instance." />
      {/* Same overflow contract as SettingsShell (issue #204): the tab row scrolls
          within its own container so the page body never scrolls horizontally. */}
      <div ref={stripRef} className="flex gap-1 overflow-x-auto border-b border-edge">
        {TABS.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            className={({ isActive }) =>
              cx(
                "-mb-px flex shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                isActive
                  ? "border-brand text-fg"
                  : "border-transparent text-muted hover:border-edge-strong hover:text-fg",
              )
            }
          >
            {t.label}
            {t.pip && attention > 0 && <HealthPip count={attention} danger={pipDanger} />}
          </NavLink>
        ))}
      </div>
      {description && <p className="text-sm text-muted">{description}</p>}
      {children}
    </div>
  );
}

// HealthPip is the Health tab's severity marker: a coloured dot (form: a round pip) plus
// the attention count. Severity is NOT colour-only — the count is visible text and the
// aria-label names the severity in words, so a screen reader and a colour-blind admin both
// get the signal (PRD #1484 accessibility discipline).
function HealthPip({ count, danger }: { count: number; danger: boolean }) {
  const word = danger ? "danger" : "warning";
  return (
    <span
      className={cx(
        "inline-flex items-center gap-1 rounded-full px-1.5 text-[11px] font-semibold tabular-nums",
        danger ? "bg-danger/15 text-danger" : "bg-warn/15 text-warn",
      )}
      aria-label={`${count} health ${count === 1 ? "check" : "checks"} need attention (${word})`}
    >
      <span aria-hidden="true" className={cx("h-1.5 w-1.5 rounded-full", danger ? "bg-danger" : "bg-warn")} />
      {count}
    </span>
  );
}
