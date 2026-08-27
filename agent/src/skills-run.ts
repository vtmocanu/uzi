// PRD #16: the single skill-assembly + plugin-materialization path, shared by the
// SDK executor (production) and the stub executor (M6 E2E), so they can never
// drift into two lenient implementations. Delivered skills (server-assembled,
// precedence-resolved) come first; repo-borne skills (opt-in, M6) are appended at
// lowest precedence with name collisions dropped; the combined set is capped
// (repo evicts first); the plugin dir is rebuilt from the survivors.

import type { ClaimConfig, ClaimSkill } from "./protocol.js";
import { DROP_REPO_COLLISION, enumerateRepoSkills, repoSkillsDir } from "./repo-skills.js";
import { enforceSkillCaps, materializeSkillsPlugin, skillsPluginDir, type SkillDrop } from "./skills-plugin.js";

// Fallbacks only when the claim omits config; the server normally supplies both
// via ClaimConfig so the worker enforces the same limits (no drift).
const DEFAULT_SKILL_MAX_BYTES = 65536;
const DEFAULT_SKILLS_MAX_PER_RUN = 32;

export interface SkillCaps {
  maxBytes: number;
  maxPerRun: number;
}

/** Resolve the skill caps from the claim config, applying the defaults. */
export function resolveSkillCaps(config?: ClaimConfig | null): SkillCaps {
  const pos = (v: number | undefined, d: number): number => (typeof v === "number" && v > 0 ? Math.floor(v) : d);
  return {
    maxBytes: pos(config?.skill_max_bytes, DEFAULT_SKILL_MAX_BYTES),
    maxPerRun: pos(config?.skills_max_per_run, DEFAULT_SKILLS_MAX_PER_RUN),
  };
}

/** Subset of RunContext prepareSkillPlugin reads. */
export interface SkillRunInput {
  skills?: ClaimSkill[];
  repoSkillsEnabled?: boolean;
  worktreePath: string;
}

export interface PreparedSkills {
  /** The materialized run union: delivered survivors ∪ repo survivors. */
  runSkills: ClaimSkill[];
  /** Repo-borne survivor names — all-templates (they carry no allocation), so the
   *  caller attaches them to every subagent. */
  repoSurvivorNames: string[];
  /** Every skill dropped worker-side: repo collision/invalid/too_large + cap
   *  over_limit. The caller logs them (worker owns the gapless seq). */
  drops: SkillDrop[];
  /** The synthesized local plugin dir (a sibling of the worktree). */
  pluginPath: string;
}

/**
 * Assemble the run's skill set and materialize the local plugin dir. Rebuilt on
 * every claim including resume. The plugin dir is OUTSIDE the clone, so it loads
 * under `settingSources: []` and the injection defense never loosens.
 */
export async function prepareSkillPlugin(ctx: SkillRunInput, caps: SkillCaps): Promise<PreparedSkills> {
  const deliveredSkills = ctx.skills ?? [];
  const drops: SkillDrop[] = [];

  // M6: repo-borne skills, opt-in, lowest precedence. A repo skill whose name
  // collides with a delivered (or earlier repo) skill is dropped and logged.
  const repoKept: ClaimSkill[] = [];
  if (ctx.repoSkillsEnabled) {
    const repo = await enumerateRepoSkills(repoSkillsDir(ctx.worktreePath), caps.maxBytes);
    drops.push(...repo.dropped);
    const seen = new Set(deliveredSkills.map((s) => s.name));
    for (const rs of repo.skills) {
      if (seen.has(rs.name)) {
        drops.push({ name: rs.name, reason: DROP_REPO_COLLISION });
        continue;
      }
      seen.add(rs.name);
      repoKept.push(rs);
    }
  }

  // Delivered first (higher precedence), repo last so the per-run cap evicts repo
  // skills first (the server already capped the delivered set).
  const { kept: runSkills, dropped: capDrops } = enforceSkillCaps([...deliveredSkills, ...repoKept], caps);
  drops.push(...capDrops);

  const pluginPath = skillsPluginDir(ctx.worktreePath);
  await materializeSkillsPlugin(pluginPath, runSkills);

  const repoKeptNames = new Set(repoKept.map((s) => s.name));
  const repoSurvivorNames = runSkills.filter((s) => repoKeptNames.has(s.name)).map((s) => s.name);

  return { runSkills, repoSurvivorNames, drops, pluginPath };
}
