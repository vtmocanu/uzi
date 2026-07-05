// PRD #16 M6: repo-borne skills, opt-in (repo.skills_enabled) and default off.
//
// Trust model (PRD §Trust model): a repo's .claude/ is the config class the
// `settingSources: []` isolation exists to block. When — and only when — the repo
// owner has vouched for the repo's review discipline, uzi loads SKILLS ONLY from
// the clone, through its own controlled channel, at LOWEST precedence. Nothing
// else under .claude/ is ever read: no hooks, no settings, no commands, no
// CLAUDE.md. The capability-granting frontmatter keys (allowed-tools and friends)
// are STRIPPED — only name + description survive, re-synthesized as escaped YAML
// by the same materializer the delivered skills use. Names must pass the skill
// regex (which also makes them path-safe: no separators, no dots), and symlinks
// are never followed, so a hostile repo cannot read outside its own tree.

import fs from "node:fs/promises";
import path from "node:path";
import type { ClaimSkill } from "./protocol.js";
import { DROP_TOO_LARGE, SKILL_NAME_RE, type SkillDrop } from "./skills-plugin.js";

/** Reason code for a repo skill dropped during enumeration/parse (invalid name,
 *  single-line/empty description, empty body, or unparseable frontmatter). */
export const DROP_REPO_INVALID = "repo_invalid";
/** Reason code for a repo skill whose name collides with a higher-precedence
 *  (delivered, or an earlier repo) skill. Repo skills rank below everything. */
export const DROP_REPO_COLLISION = "repo_collision";

/** The repo's skills directory inside the clone. Nothing else under .claude/ is
 *  ever read. */
export function repoSkillsDir(clonePath: string): string {
  return path.join(clonePath, ".claude", "skills");
}

/** Split leading YAML frontmatter, keeping ONLY the name + description keys (every
 *  other key is dropped — that is the security point). CRLF is normalized first so
 *  a Windows repo parses; a value is a single line by construction. Returns null
 *  when there is no well-formed frontmatter block. */
function parseNameDescription(raw: string): { name: string; description: string; body: string } | null {
  const normalized = raw.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
  const lines = normalized.split("\n");
  if (lines[0] !== "---") return null;
  const close = lines.indexOf("---", 1);
  if (close < 0) return null;

  let name = "";
  let description = "";
  for (const line of lines.slice(1, close)) {
    const idx = line.indexOf(":");
    if (idx < 0) continue;
    const key = line.slice(0, idx).trim();
    if (key !== "name" && key !== "description") continue; // DROP every other key
    const val = stripQuotes(line.slice(idx + 1).trim());
    if (key === "name" && name === "") name = val;
    else if (key === "description" && description === "") description = val;
  }

  let bodyLines = lines.slice(close + 1);
  if (bodyLines[0] === "") bodyLines = bodyLines.slice(1); // drop one blank separator
  return { name, description, body: bodyLines.join("\n") };
}

/** Strip one layer of matching surrounding quotes. Anything the name regex would
 *  reject (embedded quotes/colons) is dropped by validation anyway. */
function stripQuotes(v: string): string {
  if (v.length >= 2 && ((v[0] === '"' && v.at(-1) === '"') || (v[0] === "'" && v.at(-1) === "'"))) {
    return v.slice(1, -1);
  }
  return v;
}

/**
 * Enumerate `<clone>/.claude/skills/<name>/SKILL.md` into ClaimSkills, keeping
 * only name+description and applying the name regex + single-line description +
 * non-empty body + size (maxBytes) validation. Symlinks are never followed (the
 * skills dir, each skill dir, and each SKILL.md must be a REAL directory/file),
 * so a hostile repo cannot escape its tree. Sorted by name for a stable result.
 * Missing dir ⇒ no skills. Delivered-skill collision is resolved by the caller
 * (repo is lowest precedence); this returns every valid repo skill.
 */
export async function enumerateRepoSkills(
  skillsDir: string,
  maxBytes: number,
): Promise<{ skills: ClaimSkill[]; dropped: SkillDrop[] }> {
  // The skills dir itself must be a real directory, never a symlink (guards
  // `.claude/skills -> /` from redirecting enumeration outside the clone).
  let dirStat;
  try {
    dirStat = await fs.lstat(skillsDir);
  } catch {
    return { skills: [], dropped: [] };
  }
  if (!dirStat.isDirectory()) return { skills: [], dropped: [] };

  const entries = await fs.readdir(skillsDir, { withFileTypes: true });
  const skills: ClaimSkill[] = [];
  const dropped: SkillDrop[] = [];

  for (const entry of entries) {
    if (!entry.isDirectory()) continue; // real dirs only — a symlinked dir is skipped
    const skillMd = path.join(skillsDir, entry.name, "SKILL.md");
    let fileStat;
    try {
      fileStat = await fs.lstat(skillMd);
    } catch {
      continue; // no SKILL.md in this dir
    }
    if (!fileStat.isFile()) continue; // a symlinked SKILL.md is never read
    if (fileStat.size > maxBytes) {
      dropped.push({ name: entry.name, reason: DROP_TOO_LARGE });
      continue;
    }

    const parsed = parseNameDescription(await fs.readFile(skillMd, "utf8"));
    // The frontmatter `name` is the identity; it must pass the regex (which also
    // guarantees path-safety — no separators or dots — so materialization can
    // never write outside skills/).
    if (
      !parsed ||
      !SKILL_NAME_RE.test(parsed.name) ||
      parsed.description.trim() === "" ||
      parsed.body.trim() === ""
    ) {
      dropped.push({ name: parsed?.name || entry.name, reason: DROP_REPO_INVALID });
      continue;
    }
    skills.push({ name: parsed.name, description: parsed.description, body: parsed.body });
  }

  skills.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { skills, dropped };
}
