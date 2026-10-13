import type { RunMessage } from "./api";

// stageForMessages: latest-message → human stage label (a tool-slug → stage
// map, adapted to uzi's message kinds).
const TOOL_STAGE: Record<string, string> = {
  Bash: "Running a command",
  Read: "Reading files",
  Glob: "Reading files",
  Grep: "Searching code",
  Edit: "Making edits",
  MultiEdit: "Making edits",
  Write: "Making edits",
  NotebookEdit: "Making edits",
  WebFetch: "Searching the web",
  WebSearch: "Searching the web",
  Task: "Delegating to a subagent",
};

export function stageForMessages(messages: RunMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (m.kind === "tool_result") return "Working";
    if (m.kind === "tool_use") {
      const name = (m.payload as { name?: unknown } | null)?.name;
      return typeof name === "string" && Object.prototype.hasOwnProperty.call(TOOL_STAGE, name) ? TOOL_STAGE[name] : "Working";
    }
    if (m.kind === "thinking") return "Thinking";
    if (m.kind === "text") return "Writing";
    if (m.kind === "plan") return "Planning";
  }
  return "Starting up";
}

