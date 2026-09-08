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
//
// Issue #1205: skills are read from TWO real-directory roots — `.claude/skills`
// (what Claude Code reads) and `.agents/skills` (what Codex reads) — under the
// IDENTICAL validation. The cross-agent layout keeps the skill bodies in a real
// `.agents/skills/<name>/` and projects them under `.claude/skills` with a symlink
// (per-skill or whole-dir). Because the guard REFUSES symlinks for both roots, the
// symlinked `.claude/skills` side is skipped and the real `.agents/skills` side is
// what supplies the skills; a repo on the old `.claude/skills`-real layout is
// unaffected. Adding the second root does NOT widen the containment seam: symlink
// refusal is unchanged, both roots are REAL directories inside the clone, and no
// realpath-follow is introduced. When the SAME name resolves to a real skill under
// BOTH roots (only possible on a hand-rolled dual-real layout, never the canonical
// symlink one), `.claude/skills` wins — it is the Claude-specific projection and
// the worker runs a Claude Code SDK agent — and the shadowed `.agents/skills` entry
// is recorded as a drop, never silently discarded.

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
/** Reason code for a real `.agents/skills/<name>` shadowed by a same-named real
 *  `.claude/skills/<name>`. On a real-vs-real collision the Claude-specific
 *  projection wins (the worker runs a Claude Code SDK agent); the `.agents/skills`
 *  copy is recorded here rather than silently discarded (issue #1205). */
export const DROP_SHADOWED_BY_CLAUDE = "shadowed_by_claude";

/** The repo's Claude-side skills directory inside the clone (`.claude/skills`).
 *  Nothing else under .claude/ is ever read. */
export function repoSkillsDir(clonePath: string): string {
  return path.join(clonePath, ".claude", "skills");
}

/** The repo's cross-agent skills directory inside the clone (`.agents/skills`).
 *  In the canonical cross-agent layout this holds the REAL skill bodies and
 *  `.claude/skills` is a symlink projection of it; read under the identical rules
 *  as `repoSkillsDir` (issue #1205). Nothing else under .agents/ is ever read. */
export function repoAgentsSkillsDir(clonePath: string): string {
  return path.join(clonePath, ".agents", "skills");
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
 * Enumerate one real-directory skills root (`<root>/<name>/SKILL.md`) into
 * ClaimSkills, keeping only name+description and applying the name regex +
 * single-line description + non-empty body + size (maxBytes) validation. Symlinks
 * are never followed (the root dir, each skill dir, and each SKILL.md must be a
 * REAL directory/file), so a hostile repo cannot escape its tree. Sorted by name
 * for a stable result. Missing dir ⇒ no skills. Delivered-skill collision is
 * resolved by the caller (repo is lowest precedence); this returns every valid
 * repo skill. Called once per root by `collectRepoSkills` (issue #1205) — the
 * SAME validation path runs for `.claude/skills` and `.agents/skills`.
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

/**
 * Enumerate BOTH real-directory skill roots in the clone — `.claude/skills` and
 * `.agents/skills` — under the identical per-root validation (`enumerateRepoSkills`
 * is reused verbatim for each), then merge with precedence (issue #1205).
 *
 * Precedence: on a real-vs-real name collision `.claude/skills` WINS (it is the
 * Claude-specific projection and the worker runs a Claude Code SDK agent). The
 * shadowed `.agents/skills` entry is recorded in `dropped` with
 * `DROP_SHADOWED_BY_CLAUDE`, never silently discarded. In the canonical
 * cross-agent layout there is no real-vs-real collision — `.claude/skills` is a
 * symlink (skipped by the guard) — so this rule fires only on a hand-rolled
 * dual-real layout, but it is deterministic and tested.
 *
 * The symlink refusal is unchanged for both roots; a symlinked root, skill dir,
 * or SKILL.md is skipped exactly as before. Result is sorted by name for a stable
 * shape identical to `enumerateRepoSkills`.
 */
export async function collectRepoSkills(
  clonePath: string,
  maxBytes: number,
): Promise<{ skills: ClaimSkill[]; dropped: SkillDrop[] }> {
  // `.claude/skills` is the higher-precedence root; enumerate it first so its
  // names shadow a same-named real `.agents/skills` entry.
  const claude = await enumerateRepoSkills(repoSkillsDir(clonePath), maxBytes);
  const agents = await enumerateRepoSkills(repoAgentsSkillsDir(clonePath), maxBytes);

  const skills: ClaimSkill[] = [...claude.skills];
  const dropped: SkillDrop[] = [...claude.dropped, ...agents.dropped];
  const claudeNames = new Set(claude.skills.map((s) => s.name));

  for (const rs of agents.skills) {
    if (claudeNames.has(rs.name)) {
      dropped.push({ name: rs.name, reason: DROP_SHADOWED_BY_CLAUDE });
      continue;
    }
    skills.push(rs);
  }

  skills.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  return { skills, dropped };
}
