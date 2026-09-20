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
import { HealthPip } from "./healthSeverity";
import { useHealthStatus } from "../lib/useAdminHealth";

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
  // The tab pip must show on EVERY admin tab, not only while viewing Health. Both this pip
  // and the page read the ONE shared HealthStatusProvider mounted in AppShell (PRD #1484 M5),
  // so there is a single poll app-wide and the isAdmin gate lives in the provider alone.
  const { doc: health, attentionCount: attention } = useHealthStatus();
  // Danger dominates the pip colour, else warn (warn covers unknown, which ranks as warn).
  const pipDanger = (health?.counts.danger ?? 0) > 0;

  // On a narrow tab strip the row scrolls (overflow-x-auto); keep the ACTIVE tab in view so
  // Health — the last, rightmost tab — is reachable without a horizontal scroll the user
  // has to discover. Re-run when the route changes so switching tabs re-centres, AND when the
  // health pip appears or changes severity: the pip is fed by a best-effort fetch that resolves
  // 100-300 ms after mount, widening the (rightmost) Health tab; without it in the deps the
  // scroll runs once on the pre-pip width and leaves the just-rendered pip's count clipped at
  // the strip's right edge on a narrow viewport (PRD #1484 M4 review). `attention`/`pipDanger`
  // are the two inputs to the pip's rendered width.
  const stripRef = useRef<HTMLDivElement>(null);
  const { pathname } = useLocation();
  useEffect(() => {
    const active = stripRef.current?.querySelector('[aria-current="page"]');
    if (active && typeof (active as HTMLElement).scrollIntoView === "function") {
      (active as HTMLElement).scrollIntoView({ block: "nearest", inline: "nearest" });
    }
  }, [pathname, attention, pipDanger]);

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
