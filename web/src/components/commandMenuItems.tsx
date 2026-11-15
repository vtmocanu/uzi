// Pure item builders for the command menu's search-only groups (#4373): settings and admin
// sections, agents, schedules, workers, skills, recent runs and docs. Each takes already
// loaded data and returns CommandItems; labels keep the raw (API-supplied) text, and the menu
// strips unsafe characters from label and hint at render time. Every target is built from a
// loaded id or slug, never from label text.

import type { ReactNode } from "react";
import type { AgentTemplate, RunSummaryItem, Schedule, Skill, Worker } from "../lib/api";
import { maskRepoPath } from "../lib/demoMask";
import type { DocHeading } from "../lib/docHeadingsPipeline";
import type { Doc } from "../lib/docs";
import { scheduleDisplayName } from "../lib/scheduleList";
import { ActivityIcon, BookIcon, BotIcon, ClockIcon, GearIcon, ServerIcon, ShieldIcon, SkillIcon } from "./icons";
import { visibleSectionItems } from "./navSections";

export type CommandGroup =
  | "needs"
  | "actions"
  | "goto"
  | "sections"
  | "agents"
  | "schedules"
  | "workers"
  | "skills"
  | "runs"
  | "docs";

export type CommandItem = {
  id: string;
  group: CommandGroup;
  label: string;
  hint: string;
  // An in-app path; choosing the item navigates here.
  to: string;
  icon: ReactNode;
  title?: string;
  // Extra words the item also matches on (never displayed).
  keywords?: string[];
};

export function sectionCommandItems(isAdmin: boolean): CommandItem[] {
  return visibleSectionItems(isAdmin).map((s): CommandItem => {
    const admin = s.label.startsWith("Admin");
    return {
      id: `section:${s.key}`,
      group: "sections",
      label: s.label,
      hint: admin ? "Admin" : "Settings",
      to: s.to,
      icon: admin ? <ShieldIcon /> : <GearIcon />,
      keywords: s.keywords,
    };
  });
}

export function agentCommandItems(templates: ReadonlyArray<AgentTemplate>): CommandItem[] {
  return templates.map((t) => ({
    id: `agent:${t.id}`,
    group: "agents",
    label: t.name,
    hint: "Agent",
    to: `/agents/${encodeURIComponent(t.id)}`,
    icon: <BotIcon />,
  }));
}

export function scheduleCommandItems(schedules: ReadonlyArray<Schedule>, demo: boolean): CommandItem[] {
  return schedules.map((s) => {
    const name = scheduleDisplayName(s, new Map());
    const repo = s.repo_path ? maskRepoPath(s.repo_path, demo) : "";
    const isDefault = s.origin === "default" && !!s.catalog_slug;
    return {
      id: `schedule:${s.id}`,
      group: "schedules",
      label: repo ? `${name} · ${repo}` : name,
      hint: "Schedule",
      to: `/schedules?tab=schedules#schedule-name-${encodeURIComponent(s.id)}`,
      icon: <ClockIcon />,
      keywords: isDefault ? [(s.catalog_slug as string).replace(/-/g, " ")] : undefined,
    };
  });
}

export function workerCommandItems(workers: ReadonlyArray<Worker>): CommandItem[] {
  return workers.map((w) => ({
    id: `worker:${w.id}`,
    group: "workers",
    label: w.name,
    hint: "Worker",
    to: `/workers?tab=workers#worker-row-${encodeURIComponent(w.id)}`,
    icon: <ServerIcon />,
  }));
}

export function skillCommandItems(skills: ReadonlyArray<Skill>): CommandItem[] {
  return skills
    .filter((s) => s.scope !== "product")
    .map((s) => ({
      id: `skill:${s.id}`,
      group: "skills",
      label: s.name,
      hint: "Skill",
      to: `/skills?skill=${encodeURIComponent(s.id)}`,
      icon: <SkillIcon />,
    }));
}

export function runCommandItems(runs: ReadonlyArray<RunSummaryItem>, demo: boolean): CommandItem[] {
  const seen = new Set<string>();
  const out: CommandItem[] = [];
  for (const r of runs) {
    if (seen.has(r.id)) continue;
    seen.add(r.id);
    const label =
      r.issue_iid != null
        ? r.issue_title
          ? `#${r.issue_iid} · ${r.issue_title}`
          : `#${r.issue_iid}`
        : r.title || r.issue_title || r.id;
    const repo = r.repo_path ? maskRepoPath(r.repo_path, demo) : "";
    out.push({
      id: `run:${r.id}`,
      group: "runs",
      label,
      hint: repo ? `Recent runs · ${repo}` : "Recent runs",
      to: `/runs/${encodeURIComponent(r.id)}`,
      icon: <ActivityIcon />,
    });
  }
  return out;
}

// One item per doc title, plus one per heading when the build-time heading index has loaded
// (`headingsFor` is null until then, or if its import failed).
export function docCommandItems(
  docs: ReadonlyArray<Doc>,
  headingsFor: ((slug: string) => DocHeading[]) | null,
): CommandItem[] {
  const out: CommandItem[] = [];
  for (const d of docs) {
    const title = d.meta.title || d.slug;
    out.push({
      id: `doc:${d.slug}`,
      group: "docs",
      label: title,
      hint: "Doc",
      to: `/docs/${d.slug}`,
      icon: <BookIcon />,
    });
    if (!headingsFor) continue;
    for (const h of headingsFor(d.slug)) {
      if (!h.id) continue;
      // The page's own H1 repeats the title; GFM's sr-only footnotes heading is not content.
      if (h.depth === 1 && h.text === title) continue;
      if (h.id === "footnotes" && h.text === "Footnotes") continue;
      out.push({
        id: `doc:${d.slug}#${h.id}`,
        group: "docs",
        label: `${title} › ${h.text}`,
        hint: "Doc section",
        to: `/docs/${d.slug}#${encodeURIComponent(h.id)}`,
        icon: <BookIcon />,
      });
    }
  }
  return out;
}
