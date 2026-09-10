// Pinned Codex 0.153.2 app-server dynamic-tool registration for the worker-owned
// callback boundary. The app-server receives only the immutable grant's model-visible
// subset. Native environments are disabled separately on every fresh thread.

import { CODEX_DELEGATE_TOOLS, codexDynamicToolWireName, type RunGrants } from "./broker.js";

export interface CodexDynamicToolSpec {
  readonly type: "function";
  readonly name: string;
  readonly description: string;
  readonly inputSchema: Readonly<Record<string, unknown>>;
}

const STRING = { type: "string" } as const;

function objectSchema(
  properties: Readonly<Record<string, unknown>> = {},
  required: readonly string[] = [],
  additionalProperties = true,
): Readonly<Record<string, unknown>> {
  return {
    type: "object",
    properties,
    ...(required.length === 0 ? {} : { required: [...required] }),
    additionalProperties,
  };
}

function definition(name: string): Omit<CodexDynamicToolSpec, "type" | "name"> {
  switch (name) {
    case "Bash":
      return {
        description: "Run one worker-screened shell command inside the current worktree.",
        inputSchema: objectSchema({ command: STRING, cwd: STRING }, ["command"], false),
      };
    case "apply_patch":
      return {
        description: "Create or edit one worktree file through the worker-owned no-symlink file boundary.",
        inputSchema: objectSchema({
          path: STRING,
          file_path: STRING,
          content: STRING,
          data: STRING,
          text: STRING,
          old_string: STRING,
          new_string: STRING,
          edits: { type: "array", items: objectSchema({ old_string: STRING, new_string: STRING }, ["old_string", "new_string"], false) },
        }),
      };
    case "Read":
      return {
        description: "Read one file inside the current worktree through the worker-owned no-symlink file boundary.",
        inputSchema: {
          ...objectSchema({ path: STRING, file_path: STRING }, [], false),
          anyOf: [{ required: ["path"] }, { required: ["file_path"] }],
        },
      };
    case "Skill":
      return {
        description: "Load one skill already granted to this role.",
        inputSchema: {
          ...objectSchema({ skill: STRING, name: STRING }, [], false),
          anyOf: [{ required: ["skill"] }, { required: ["name"] }],
        },
      };
    case "spawn_agent":
      return {
        description: "Run one known subagent role synchronously and return its bounded result.",
        inputSchema: {
          ...objectSchema({
          subagent_type: STRING,
          role: STRING,
          agent_type: STRING,
          prompt: STRING,
          description: STRING,
          task: STRING,
          input: STRING,
          message: STRING,
          }, [], false),
          anyOf: [{ required: ["subagent_type"] }, { required: ["role"] }, { required: ["agent_type"] }],
        },
      };
    case "submit_plan":
      return {
        description: "Submit the implementation plan for the worker's approval gate.",
        inputSchema: objectSchema({
          plan_md: STRING,
          milestones: { type: "array", items: objectSchema({ id: STRING, title: STRING }, ["id", "title"], false) },
        }, ["plan_md"], false),
      };
    case "signal_done":
      return {
        description: "Signal that the root run has completed its current task.",
        inputSchema: objectSchema({
          summary: STRING,
          report_only: { type: "boolean" },
          milestones_completed: { type: "array", items: STRING },
          prd_done_path: STRING,
          proposal: objectSchema({ title: STRING, body: STRING }, ["title", "body"], false),
        }, [], false),
      };
    case "ask_user":
      return {
        description: "Send structured blocking questions to the user.",
        inputSchema: objectSchema({
          questions: {
            type: "array",
            items: objectSchema({
              question: STRING,
              header: STRING,
              options: {
                type: "array",
                items: objectSchema({ label: STRING, description: STRING }, ["label", "description"], false),
              },
              multiSelect: { type: "boolean" },
            }, ["question", "header"], false),
          },
        }, ["questions"], false),
      };
    case "report_progress":
      return {
        description: "Report structured milestone progress for the current run.",
        inputSchema: objectSchema({
          completed: { type: "array", items: STRING },
          in_progress: { type: "array", items: STRING },
        }, [], false),
      };
    case "checkpoint":
      return {
        description: "Request a cooperative worker checkpoint at a safe milestone boundary.",
        inputSchema: objectSchema({}, [], false),
      };
    case "mcp__forge__get_issue":
    case "mcp__forge__get_merge_request":
    case "mcp__forge__list_issue_label_events":
      return {
        description: "Read one granted forge object by its positive integer number.",
        inputSchema: objectSchema({ iid: { type: "integer", minimum: 1 } }, ["iid"], false),
      };
    case "mcp__forge__get_pipeline_jobs":
      return {
        description: "List jobs for one granted forge pipeline.",
        inputSchema: objectSchema({ pipeline_id: { type: "integer", minimum: 1 } }, ["pipeline_id"], false),
      };
    case "mcp__forge__list_issues":
      return {
        description: "List forge issues through the worker-owned bounded reader.",
        inputSchema: objectSchema({
          state: { type: "string", enum: ["opened", "closed"] },
          labels: { type: "array", items: STRING },
          updated_after: STRING,
        }, [], false),
      };
    case "mcp__forge__latest_pipeline":
      return {
        description: "Read the latest pipeline for one branch or merge request selector.",
        inputSchema: objectSchema({ ref: STRING, mr_iid: { type: "integer", minimum: 1 } }, [], false),
      };
    case "mcp__forge__reply_mr_thread":
      return {
        description: "Reply to one review thread already authorized in this run's snapshot.",
        inputSchema: objectSchema({ reply_id: STRING, body: STRING }, ["reply_id", "body"], false),
      };
    case "mcp__forge__resolve_mr_thread":
      return {
        description: "Resolve one review thread already authorized in this run's snapshot.",
        inputSchema: objectSchema({ resolve_id: STRING }, ["resolve_id"], false),
      };
    case "mcp__memory__save_memory":
      return {
        description: "Save one granted cross-run repository memory through the worker.",
        inputSchema: objectSchema({
          title: STRING,
          body: STRING,
          basis: { type: "string", enum: ["observed", "inferred"] },
          evidence: STRING,
        }, ["title", "body"], false),
      };
    case "mcp__findings__report_incidental_issue":
      return {
        description: "Record one actionable off-task finding for later human triage.",
        inputSchema: objectSchema({
          title: STRING,
          description: STRING,
          location: STRING,
          labels: { type: "array", items: STRING },
          confidence: { type: "string", enum: ["low", "medium", "high"] },
        }, ["title", "description", "location"], false),
      };
    default:
      return {
        description: "Invoke one worker-owned tool already granted to this role.",
        inputSchema: objectSchema(),
      };
  }
}

/** Build the stable model-visible callback list for one immutable role grant.
 * Code-mode lifecycle callback names remain recognized by the broker defensively,
 * but code mode is disabled and those internal names are never offered to the model. */
export function buildCodexDynamicTools(grants: RunGrants): readonly CodexDynamicToolSpec[] {
  return [...grants.allowedTools]
    .filter((canonical) => !CODEX_DELEGATE_TOOLS.has(canonical) || canonical === "spawn_agent")
    .map((canonical) => ({ canonical, wire: codexDynamicToolWireName(canonical) }))
    .filter((entry): entry is { canonical: string; wire: string } => entry.wire !== undefined)
    .sort((a, b) => a.wire < b.wire ? -1 : a.wire > b.wire ? 1 : 0)
    .map(({ canonical, wire }) => ({ type: "function", name: wire, ...definition(canonical) }));
}
