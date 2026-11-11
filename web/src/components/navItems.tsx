// The app's navigation destinations, defined once (PRD #3875 M1 prefactor). The sidebar
// (AppShell's SidebarContent) renders these through NavItem/NavGroup and the command menu
// lists them under "Go to", so the two cannot drift. Dynamic parts stay with the renderer:
// a destination names which count badge it carries (`badge`) and the sidebar resolves the
// number, tone and accessible noun from its own state; the Repos entry's board children are
// injected by the sidebar right after the `repos` item.

import type { ReactNode } from "react";
import {
  ActivityIcon,
  BoardIcon,
  BookIcon,
  BotIcon,
  BugIcon,
  ChatIcon,
  ClockIcon,
  GearIcon,
  HomeIcon,
  ScaleIcon,
  ServerIcon,
  ShieldIcon,
  SkillIcon,
} from "./icons";

export type NavBadgeKey = "runs" | "findings" | "schedules" | "workers" | "judge";

export type NavDef = {
  key: string;
  label: string;
  to: string;
  icon: ReactNode;
  // Rendered (and listed in the command menu) for admins only.
  adminOnly?: boolean;
  // Active only on an exact path match, so a parent is not lit by its children.
  exactOnly?: boolean;
  // Which count badge the sidebar resolves for this item.
  badge?: NavBadgeKey;
  // The Admin entry's health severity pip (PRD #1484 M5, D1): a count + worded severity
  // when checks need attention. The sidebar feeds it from the shared health provider.
  healthPip?: true;
  // Overrides the command menu's right-hand hint, which otherwise is the section's group
  // name (empty for the unlabelled sections).
  hint?: string;
};

export type NavGroupName = "Work" | "Factory";

export type NavSection = {
  // null: no label (the leading Overview block and the bottom cluster).
  group: NavGroupName | null;
  // The leading block renders without a NavGroup rule above it.
  leading?: true;
  items: NavDef[];
};

export const NAV_SECTIONS: NavSection[] = [
  { group: null, leading: true, items: [{ key: "overview", label: "Overview", to: "/dashboard", icon: <HomeIcon /> }] },
  {
    group: "Work",
    items: [
      { key: "repos", label: "Repos", to: "/repos", icon: <BoardIcon />, exactOnly: true },
      // Runs badge (PRD #239): the caller's in-progress run count. Default "count" tone,
      // brand, "there is a queue", not the Workers alert red.
      { key: "runs", label: "Runs", to: "/runs", icon: <ActivityIcon />, badge: "runs" },
      // Findings (PRD #333 D8): off-task bugs workers flagged on YOUR code, Work, not the
      // agent-grading Factory where Judge lives. Sits directly under Runs, the lane whose
      // output it collects. Badge is the open-findings count; brand "count" tone.
      { key: "findings", label: "Findings", to: "/findings", icon: <BugIcon />, badge: "findings" },
      // Schedules (PRD #241): the time-driven run origin. Badge is the enabled-schedule
      // count unless any schedule is parked (PRD #1650 D6); the sidebar resolves which.
      { key: "schedules", label: "Schedules", to: "/schedules", icon: <ClockIcon />, badge: "schedules" },
      { key: "chat", label: "Chat", to: "/chat", icon: <ChatIcon /> },
    ],
  },
  {
    group: "Factory",
    items: [
      { key: "agents", label: "Agents", to: "/agents", icon: <BotIcon /> },
      { key: "skills", label: "Skills", to: "/skills", icon: <SkillIcon /> },
      // Workers' ONE nav home. It was also a Settings tab (as /settings/workers), which
      // needed the excludeSubpath active-state hack to keep the two entries from lighting
      // together: the fleet is operations, so it lives here alone.
      { key: "workers", label: "Workers", to: "/workers", icon: <ServerIcon />, badge: "workers" },
      // Judge (PRD #98): the cross-run recommendation workbench. Badge is the to-triage
      // backlog count, the same number the page's To-triage tab shows.
      { key: "judge", label: "Judge", to: "/judge", icon: <ScaleIcon />, badge: "judge" },
    ],
  },
  // The bottom cluster: system destinations, grouped by a rule rather than a label.
  // Settings and Docs used to each carry a one-item labeled group ("Configure", "Help"),
  // and the five admin pages each held a sidebar row; they are now one tabbed Admin
  // destination (AdminShell), the same treatment the Settings tabs give user surfaces.
  {
    group: null,
    items: [
      // Forge has no standalone entry (Decision 3): it lives only under the Settings tabs,
      // so Settings stays lit across /settings/*.
      { key: "settings", label: "Settings", to: "/settings", icon: <GearIcon /> },
      // /admin redirects to the first tab; the prefix match keeps this entry lit on every
      // /admin/* tab.
      { key: "admin", label: "Admin", to: "/admin", icon: <ShieldIcon />, adminOnly: true, healthPip: true, hint: "Admin" },
      { key: "docs", label: "Docs", to: "/docs", icon: <BookIcon /> },
    ],
  },
];

export type VisibleNavItem = NavDef & {
  // The section's group name, null for the unlabelled sections.
  group: NavGroupName | null;
  // The right-hand hint the command menu shows: the item's own hint, else the group.
  hint: string;
};

// The flat, ordered list of destinations a viewer can reach.
export function visibleNavItems(isAdmin: boolean): VisibleNavItem[] {
  return NAV_SECTIONS.flatMap((section) =>
    section.items
      .filter((item) => !item.adminOnly || isAdmin)
      .map((item) => ({
        ...item,
        group: section.group,
        hint: item.hint ?? section.group ?? "",
      })),
  );
}
