// Memory-guard grant shaping (issue #2471, M0). On a guard-bound Claude flight the native
// `Bash` tool is aliased to an in-process MCP tool, so a subagent whose explicit `tools`
// allowlist names plain `Bash` must name the mapped tool instead, or the CLI would filter
// the call out of its toolset. Pure: never mutates its input.

import type { AgentDefinition } from "@anthropic-ai/claude-agent-sdk";
import { CLAUDE_GUARDED_COMMAND_APPEND, CLAUDE_LONG_COMMAND_APPEND } from "../prompt.js";

export const GUARDED_COMMAND_SERVER = "uzi_cmd";
export const GUARDED_COMMAND_TOOL = "mcp__uzi_cmd__bash";

export interface UnavailableRole {
  name: string;
  reason: string;
}

export interface ShapedGrants {
  agents: Record<string, AgentDefinition>;
  unavailable: UnavailableRole[];
}

const SCOPED_BASH_RE = /^Bash\(/;

/**
 * Rewrite a roster for a guard-bound flight:
 * - explicit `tools` with plain `Bash`: replaced by {@link GUARDED_COMMAND_TOOL}, scoped
 *   `Bash(...)` entries dropped, the server named in `mcpServers`;
 * - explicit `tools` whose only shell grants are scoped `Bash(...)`: the role is unavailable;
 * - explicit `tools` without Bash, and inherit-all definitions: tools unchanged.
 * Every surviving definition swaps the long-command prompt append for the guarded one; an
 * absent append throws (fail closed), so a prompt that still teaches background commands
 * can never ship.
 */
export function shapeGuardedGrants(agents: Record<string, AgentDefinition>): ShapedGrants {
  const out: Record<string, AgentDefinition> = {};
  const unavailable: UnavailableRole[] = [];
  for (const [name, def] of Object.entries(agents)) {
    if (!def.prompt.includes(CLAUDE_LONG_COMMAND_APPEND)) {
      throw new Error(`shapeGuardedGrants: subagent "${name}" prompt lacks the long-command append`);
    }
    const next: AgentDefinition = {
      ...def,
      prompt: def.prompt.replace(CLAUDE_LONG_COMMAND_APPEND, () => CLAUDE_GUARDED_COMMAND_APPEND),
    };
    const tools = def.tools;
    if (tools && tools.length > 0) {
      const plain = tools.includes("Bash");
      const scoped = tools.some((t) => SCOPED_BASH_RE.test(t));
      if (!plain && scoped) {
        unavailable.push({
          name,
          reason: "scoped Bash grants are not supported under the memory guard",
        });
        continue;
      }
      if (plain) {
        const mapped: string[] = [];
        for (const t of tools) {
          if (SCOPED_BASH_RE.test(t)) continue;
          const entry = t === "Bash" ? GUARDED_COMMAND_TOOL : t;
          if (!mapped.includes(entry)) mapped.push(entry);
        }
        next.tools = mapped;
        const servers = def.mcpServers ? [...def.mcpServers] : [];
        if (!servers.includes(GUARDED_COMMAND_SERVER)) servers.push(GUARDED_COMMAND_SERVER);
        next.mcpServers = servers;
      } else {
        next.tools = [...tools];
      }
    }
    out[name] = next;
  }
  return { agents: out, unavailable };
}
