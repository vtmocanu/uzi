// Parsing and re-validation of /modules.json (PRD #4686, "Module list"). The file is
// operator-written deployment configuration, served unauthenticated and possibly
// hand-edited, so the SPA validates it again instead of trusting the chart or the
// generator. Every skipped entry and every fallback logs exactly one console error that
// names the entry; a malformed file means "no modules". Unknown fields are ignored.

import { isModuleIconName, type ModuleIconName } from "./icons";

export type ModuleNavGroup = "work" | "factory" | "modules";

export interface ModuleEntry {
  id: string;
  version: string;
  // The expose name rendered at /m/<id>/*.
  page: string;
  label: string;
  icon: ModuleIconName;
  navGroup: ModuleNavGroup;
  adminOnly: boolean;
}

const MODULE_ID_RE = /^[a-z][a-z0-9-]{0,31}$/;
const NAV_GROUPS: readonly string[] = ["work", "factory", "modules"];
const LABEL_MAX = 40;

function isNonEmptyString(v: unknown): v is string {
  return typeof v === "string" && v.trim() !== "";
}

export function parseModuleList(json: unknown): ModuleEntry[] {
  if (!Array.isArray(json)) {
    console.error("uzi modules: /modules.json is not a JSON array; no modules loaded");
    return [];
  }
  const out: ModuleEntry[] = [];
  const seen = new Set<string>();
  json.forEach((raw, i) => {
    const skip = (name: string, why: string) =>
      console.error(`uzi modules: skipping /modules.json entry ${i} (${name}): ${why}`);
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
      skip("not an object", "entry is not an object");
      return;
    }
    const e = raw as Record<string, unknown>;
    if (typeof e.id !== "string" || !MODULE_ID_RE.test(e.id)) {
      skip(typeof e.id === "string" ? JSON.stringify(e.id.slice(0, 48)) : "no id", "invalid id");
      return;
    }
    const id = e.id;
    for (const field of ["version", "page", "label", "icon", "navGroup"] as const) {
      if (!isNonEmptyString(e[field])) {
        skip(id, `missing required field "${field}"`);
        return;
      }
    }
    const label = e.label as string;
    if ([...label].length > LABEL_MAX) {
      skip(id, `label is longer than ${LABEL_MAX} characters`);
      return;
    }
    if (!isModuleIconName(e.icon)) {
      skip(id, `unknown icon ${JSON.stringify(String(e.icon).slice(0, 48))}`);
      return;
    }
    if (e.adminOnly !== undefined && typeof e.adminOnly !== "boolean") {
      skip(id, "adminOnly is not a boolean");
      return;
    }
    if (seen.has(id)) {
      skip(id, "duplicate id; keeping the first entry");
      return;
    }
    let navGroup: ModuleNavGroup;
    if (NAV_GROUPS.includes(e.navGroup as string)) {
      navGroup = e.navGroup as ModuleNavGroup;
    } else {
      console.error(
        `uzi modules: /modules.json entry ${i} (${id}): unknown navGroup ${JSON.stringify(String(e.navGroup).slice(0, 48))}; using "modules"`,
      );
      navGroup = "modules";
    }
    seen.add(id);
    out.push({
      id,
      version: e.version as string,
      page: e.page as string,
      label,
      icon: e.icon,
      navGroup,
      adminOnly: e.adminOnly === true,
    });
  });
  return out;
}
