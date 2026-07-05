// PRD #16 M4: deliver DB-stored skills to the SDK WITHOUT loosening the
// `settingSources: []` isolation (the repo-borne prompt-injection defense).
//
// Skills are materialized into a LOCAL PLUGIN directory OUTSIDE the clone (a
// sibling of the worktree, never inside it) and enabled via the SDK's explicit
// `skills` list + per-`AgentDefinition.skills`. Filesystem skill discovery stays
// off; the plugin channel is a separate top-level option (SdkPluginConfig,
// sdk.d.ts:1727), independent of `settingSources`, so the isolation never
// loosens. The `skills` field is the SDK's single enable switch — sdk.d.ts:1869
// is explicit that you do NOT add `'Skill'` to the tools allowlist (passing it
// there is deprecated), which resolves the PRD's "Skill grant if needed" as
// "not needed": a tools-restricted subagent expands its allocated skills via
// `AgentDefinition.skills` alone.

import fs from "node:fs/promises";
import path from "node:path";
import type { ClaimSkill } from "./protocol.js";

/** Plugin name written to .claude-plugin/plugin.json. The SDK addresses our
 *  skills as `uzi:<name>` (plugin-qualified enable-list, sdk.d.ts:1876). */
export const SKILLS_PLUGIN_NAME = "uzi";

/** Kebab-case skill name — the identity + on-disk directory. Mirrors the server
 *  regex (api/internal/skilltmpl NameRe). It also makes a name path-safe: the
 *  charset excludes separators and dots, so an M6 repo-borne name can never write
 *  a SKILL.md outside skills/. */
export const SKILL_NAME_RE = /^[a-z0-9][a-z0-9-]{0,63}$/;

/** Reason codes for skills the WORKER drops locally (distinct from the server's
 *  assembly drops that ride the claim as skills_dropped). */
export const DROP_TOO_LARGE = "too_large";
export const DROP_OVER_LIMIT = "over_limit";

export interface SkillDrop {
  name: string;
  reason: string;
}

/** Qualify a bare skill name to the SDK's `plugin:skill` enable-list form. Used
 *  for both the top-level `skills` list and each `AgentDefinition.skills`. */
export function qualifiedSkillName(name: string): string {
  return `${SKILLS_PLUGIN_NAME}:${name}`;
}

/** The plugin dir for a run: a SIBLING of the worktree, never inside it, so the
 *  clone's contents and the synthesized plugin never overlap and the SDK cwd
 *  (= the worktree) never traverses into it. Rebuilt from the claim on every
 *  claim including resume. */
export function skillsPluginDir(worktreePath: string): string {
  return path.join(path.dirname(worktreePath), `.uzi-skills-${path.basename(worktreePath)}`);
}

/**
 * Emit a YAML double-quoted scalar that neutralizes EVERY frontmatter
 * metacharacter class. Inside double quotes `:` `#` `|` `>` `&` `*` `!`, leading
 * spaces, and `---` are all literal and inert; only `\` and `"` are structural
 * (escaped here), and every control char (newline/CR/tab/…) is escaped so a value
 * can never break onto a new line or forge a second key. This is the
 * frontmatter-injection guard (PRD Risks): the model routes on `description`, so
 * a crafted description must not be able to redefine the frontmatter.
 */
export function yamlQuote(value: string): string {
  let out = '"';
  for (const ch of value) {
    const code = ch.codePointAt(0)!;
    if (ch === "\\") out += "\\\\";
    else if (ch === '"') out += '\\"';
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "\t") out += "\\t";
    // Escape every control char (C0 <0x20, DEL+C1 0x7f-0x9f) AND the Unicode line
    // separators U+2028/U+2029, which some YAML parsers treat as line breaks.
    // None can close the quoted scalar, but escaping them keeps the round-trip
    // byte-exact and parser-independent (M4 auditor hardening).
    else if (code < 0x20 || (code >= 0x7f && code <= 0x9f) || code === 0x2028 || code === 0x2029)
      out += "\\u" + code.toString(16).padStart(4, "0");
    else out += ch;
  }
  return out + '"';
}

/** Render one SKILL.md: escaped name/description frontmatter, then the body. The
 *  body sits BELOW the frontmatter, so it cannot redefine it. */
export function renderSkillMd(skill: ClaimSkill): string {
  const frontmatter = `---\nname: ${yamlQuote(skill.name)}\ndescription: ${yamlQuote(skill.description)}\n---\n\n`;
  const body = skill.body.endsWith("\n") ? skill.body : `${skill.body}\n`;
  return frontmatter + body;
}

/** A skill directory name must be a single, safe path segment. Server names are
 *  kebab-case; this guards the M6 repo-borne path (untrusted input) so a crafted
 *  name can never write a SKILL.md outside the skills/ tree. */
function isSafeSkillDirName(name: string): boolean {
  return name.length > 0 && !name.includes("/") && !name.includes("\\") && name !== "." && name !== "..";
}

/**
 * Rebuild the local skills plugin from scratch (remove any prior dir, then write
 * `.claude-plugin/plugin.json` + `skills/<name>/SKILL.md` per skill). Called on
 * EVERY claim including resume, so a skill deleted between claim and resume
 * simply disappears from the rebuilt plugin.
 */
export async function materializeSkillsPlugin(dir: string, skills: ClaimSkill[]): Promise<void> {
  await fs.rm(dir, { recursive: true, force: true });
  await fs.mkdir(path.join(dir, ".claude-plugin"), { recursive: true });
  await fs.writeFile(
    path.join(dir, ".claude-plugin", "plugin.json"),
    `${JSON.stringify({ name: SKILLS_PLUGIN_NAME })}\n`,
    "utf8",
  );
  for (const skill of skills) {
    if (!isSafeSkillDirName(skill.name)) continue;
    const skillDir = path.join(dir, "skills", skill.name);
    await fs.mkdir(skillDir, { recursive: true });
    await fs.writeFile(path.join(skillDir, "SKILL.md"), renderSkillMd(skill), "utf8");
  }
}

/**
 * Re-enforce the server-configured caps worker-side over a PRECEDENCE-ORDERED
 * list (highest precedence first). A skill whose body exceeds maxBytes is dropped
 * (too_large); then, if the count still exceeds maxPerRun, the lowest-precedence
 * tail is dropped (over_limit). For the delivered-only M4 input the server has
 * already enforced both caps, so this is a no-op — the seam is here for M6, which
 * appends repo-borne skills (lowest precedence) to the tail so they evict first.
 */
export function enforceSkillCaps(
  skills: ClaimSkill[],
  caps: { maxBytes: number; maxPerRun: number },
): { kept: ClaimSkill[]; dropped: SkillDrop[] } {
  const dropped: SkillDrop[] = [];
  const sized = skills.filter((s) => {
    if (caps.maxBytes > 0 && Buffer.byteLength(s.body, "utf8") > caps.maxBytes) {
      dropped.push({ name: s.name, reason: DROP_TOO_LARGE });
      return false;
    }
    return true;
  });
  let kept = sized;
  if (caps.maxPerRun > 0 && sized.length > caps.maxPerRun) {
    kept = sized.slice(0, caps.maxPerRun);
    for (const over of sized.slice(caps.maxPerRun)) {
      dropped.push({ name: over.name, reason: DROP_OVER_LIMIT });
    }
  }
  return { kept, dropped };
}
