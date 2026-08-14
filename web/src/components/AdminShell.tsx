// AdminShell: one header + tab bar shared by the five admin surfaces (Users,
// Rate limits, Tool allowlist, Blocked repos, Instance). The same treatment
// SettingsShell gives the user-scoped settings: the five pages used to be five
// top-level sidebar entries wired together only by proximity, which crowded the
// sidebar for admins and hid that they are one area — this instance's controls.
// The sidebar now carries a single "Admin" entry; the tabs carry the rest.

import { NavLink } from "react-router-dom";
import type { ReactNode } from "react";
import { cx, PageHeader } from "./ui";

const TABS = [
  { to: "/admin/users", label: "Users" },
  { to: "/admin/rate-limits", label: "Rate limits" },
  { to: "/admin/tool-allowlist", label: "Tool allowlist" },
  { to: "/admin/blocked-repos", label: "Blocked repos" },
  { to: "/admin/settings", label: "Instance" },
];

export function AdminShell({ description, children }: { description: string; children: ReactNode }) {
  return (
    <div className="space-y-6">
      <PageHeader title="Admin" description={description} />
      {/* Same overflow contract as SettingsShell (issue #204): the tab row scrolls
          within its own container so the page body never scrolls horizontally. */}
      <div className="flex gap-1 overflow-x-auto border-b border-edge">
        {TABS.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            className={({ isActive }) =>
              cx(
                "-mb-px shrink-0 whitespace-nowrap border-b-2 px-3 py-2 text-sm font-medium transition-colors",
                isActive
                  ? "border-brand text-fg"
                  : "border-transparent text-muted hover:border-edge-strong hover:text-fg",
              )
            }
          >
            {t.label}
          </NavLink>
        ))}
      </div>
      {children}
    </div>
  );
}
