// Issue #2085: project one Codex agent file, `.codex/agents/*.toml`, onto the same
// AgentTemplate the `.claude/agents/*.md` loader produces. Containment, caps,
// ordering and dedupe are the shared loader's job (repoagents.ts); this module only
// turns the TOML text of ONE file into a template or a skip reason.
//
// The template is built FRESH from three validated string fields (`name`,
// `description`, `developer_instructions`) and never spread from the parsed object:
// no tools, no model. `model`, `model_reasoning_effort`, `nickname_candidates`,
// `config_file`, `includes` and every other key are ignored. Only the four keys in
// CODEX_RESTRICTION_KEYS (`features`, `skills`, `sandbox_mode`, `tools`) skip a file,
// rather than run it without the restriction its author asked for; any other Codex
// config key (e.g. `approval_policy`, `sandbox_workspace_write`) is ignored like the rest.

import { parse } from "smol-toml";
import type { AgentTemplate } from "./protocol.js";
import { isValidAgentName, isValidDescription, type ParsedAgentFile } from "./repoagents.js";

/** Codex config keys that restrict what an agent may do. uzi does not translate
 *  them, so their presence (with any value) skips the agent. Our own constants:
 *  only these names ever reach a run message. */
const CODEX_RESTRICTION_KEYS = ["features", "skills", "sandbox_mode", "tools"] as const;

export function parseCodexAgentFile(raw: string, slug: string): ParsedAgentFile {
  const invalid = (name: string): ParsedAgentFile => ({ ok: false, name, reason: "invalid" });

  let doc: Record<string, unknown>;
  try {
    doc = parse(raw);
  } catch {
    return invalid(slug);
  }

  // No filename fallback: a Codex agent's identity is its declared name.
  const rawName = doc["name"];
  if (typeof rawName !== "string") return invalid(slug);
  const name = rawName.trim();
  if (!isValidAgentName(name)) return invalid(slug);

  const rawDescription = doc["description"];
  if (typeof rawDescription !== "string") return invalid(name);
  const description = rawDescription.trim();
  if (!isValidDescription(description)) return invalid(name);

  const instructions = doc["developer_instructions"];
  if (typeof instructions !== "string" || instructions.trim() === "") return invalid(name);

  const restrictions = CODEX_RESTRICTION_KEYS.filter((k) => Object.hasOwn(doc, k));
  if (restrictions.length > 0) return { ok: false, name, reason: "restriction_unsupported", restrictions };

  const template: AgentTemplate = { name, description, prompt_body: instructions };
  return { ok: true, template, notes: [] };
}
