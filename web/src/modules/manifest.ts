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
// C0/C1 controls, zero-width and directional marks (U+200B-200F), bidi embeddings and
// overrides (U+202A-202E), bidi isolates (U+2066-2069), the line/paragraph separators, the Arabic
// letter mark (U+061C), the BOM (U+FEFF), word joiner and invisible operators (U+2060-2064), the
// soft hyphen (U+00AD) and the Mongolian vowel separator (U+180E). Keep equal to $unsafe in
// deploy/chart/templates/_web-modules.tpl.
const UNSAFE_TEXT_RE =
  /[\p{Cc}\u200b-\u200f\u2028\u2029\u202a-\u202e\u2066-\u2069\u061c\ufeff\u2060-\u2064\u00ad\u180e]/u;

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
    for (const field of ["label", "version", "page"] as const) {
      if (UNSAFE_TEXT_RE.test(e[field] as string)) {
        skip(id, `${field} contains control or invisible formatting characters`);
        return;
      }
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
