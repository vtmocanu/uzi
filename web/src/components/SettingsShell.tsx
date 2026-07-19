// SettingsShell: one header + tab bar shared by the three settings surfaces
// (Account & token, Forge, Workers). Kills the old discoverability problem
// where /settings, /settings/forge and /settings/workers were URL siblings
// wired together only by hand-placed corner links — the drift multica's
// unified layout headers exist to prevent (packages/views/layout/).

import { NavLink } from "react-router-dom";
import type { ReactNode } from "react";
import { cx, PageHeader } from "./ui";

const TABS = [
  { to: "/settings", label: "Account & token", end: true },
  { to: "/settings/forge", label: "Forge", end: false },
  { to: "/settings/workers", label: "Workers", end: false },
  { to: "/settings/access", label: "Access", end: false },
  { to: "/settings/memory", label: "Memory", end: false },
];

export function SettingsShell({ description, children }: { description: string; children: ReactNode }) {
  return (
    <div className="space-y-6">
      <PageHeader title="Settings" description={description} />
      <div className="flex gap-1 border-b border-edge">
        {TABS.map((t) => (
          <NavLink
            key={t.to}
            to={t.to}
            end={t.end}
            className={({ isActive }) =>
              cx(
                "-mb-px border-b-2 px-3 py-2 text-sm font-medium transition-colors",
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
