// SettingsShell: one header + tab bar shared by the user-scoped settings
// surfaces (Account & tokens, Run defaults, Forge, Access, Memory). Kills the old
// discoverability problem where the /settings/* pages were URL siblings wired
// together only by hand-placed corner links — the drift a unified layout
// header exists to prevent.
//
// Workers is deliberately NOT a tab any more: it is fleet operations (live
// status, upgrade rolls, an attention badge), not a preference, and its dual
// home here + in the sidebar required AppShell's excludeSubpath active-state
// hack. It now lives at /workers as a first-class Factory page.

import { NavLink } from "react-router-dom";
import type { ReactNode } from "react";
import { PageHeader } from "./ui";
import { PinnedBlock, TabRow, tabClass } from "./TabRow";

export const TABS = [
  { to: "/settings", label: "Account & tokens", end: true },
  { to: "/settings/appearance", label: "Appearance", end: false },
  { to: "/settings/run-defaults", label: "Run defaults", end: false },
  { to: "/settings/forge", label: "Forge", end: false },
  { to: "/settings/access", label: "Access", end: false },
  { to: "/settings/memory", label: "Memory", end: false },
];

export function SettingsShell({ description, children }: { description: string; children: ReactNode }) {
  return (
    <div className="space-y-6">
      {/* Constant across tabs so the strip never jumps on a switch — the per-tab
          `description` renders BELOW the strip instead (see AdminShell, which
          documents the mechanism; both shells keep the same treatment). */}
      <PageHeader title="Settings" description="Your personal uzi configuration." />
      {/* Issue #204: the tab row scrolls WITHIN its own container so the page body never
          scrolls horizontally. Measured when this row held five tabs: at 390px the strip
          overflowed (scrollWidth 401 vs clientWidth 390). The row now holds six tabs
          (Appearance was added), so `overflow-x-auto` earns its keep — it lets the strip
          scroll rather than forcing the page body sideways. The tabs are
          `shrink-0`/`whitespace-nowrap` so they overflow-and-scroll rather than compress
          to fit (which would defeat overflow-x). */}
      <PinnedBlock>
        <TabRow>
          {TABS.map((t) => (
            <NavLink key={t.to} to={t.to} end={t.end} className={({ isActive }) => tabClass(isActive)}>
              {t.label}
            </NavLink>
          ))}
        </TabRow>
      </PinnedBlock>
      {description && <p className="text-sm text-muted">{description}</p>}
      {children}
    </div>
  );
}
