// Merging runtime modules into the static navigation (PRD #4686, "Routes and navigation").
// NAV_SECTIONS stays static; this one function is the single source for both the sidebar
// (AppShell) and the command menu (visibleNavItems), so the two never disagree.

import { useMemo } from "react";
import { NAV_SECTIONS, type NavDef, type NavSection } from "../components/navItems";
import { moduleIcon } from "./icons";
import type { ModuleEntry } from "./manifest";
import { useModules } from "./registry";

function navDefOf(m: ModuleEntry): NavDef {
  return {
    key: `module:${m.id}`,
    label: m.label,
    to: `/m/${m.id}`,
    icon: moduleIcon(m.icon),
    adminOnly: m.adminOnly || undefined,
  };
}

// `work` and `factory` entries are appended, in manifest order, to those sections. `modules`
// entries form a new "Modules" section after Factory and before the unlabelled bottom
// cluster, only when there is at least one. With no modules the input is returned as is.
export function mergeModuleNav(sections: NavSection[], modules: ModuleEntry[]): NavSection[] {
  if (modules.length === 0) return sections;
  const extra = (group: ModuleEntry["navGroup"]) => modules.filter((m) => m.navGroup === group).map(navDefOf);
  const work = extra("work");
  const factory = extra("factory");
  const own = extra("modules");
  const merged = sections.map((s) => {
    if (s.id === "work" && work.length > 0) return { ...s, items: [...s.items, ...work] };
    if (s.id === "factory" && factory.length > 0) return { ...s, items: [...s.items, ...factory] };
    return s;
  });
  if (own.length === 0) return merged;
  const at = merged.findIndex((s) => s.id === "factory");
  const section: NavSection = { id: "modules", group: "Modules", items: own };
  merged.splice(at === -1 ? merged.length : at + 1, 0, section);
  return merged;
}

// The merged sections for the current module list; re-renders when the list resolves.
export function useNavSections(): NavSection[] {
  const { modules } = useModules();
  return useMemo(() => mergeModuleNav(NAV_SECTIONS, modules), [modules]);
}
