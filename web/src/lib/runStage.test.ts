import { describe, expect, it } from "vitest";
import { stageForMessages } from "./runStage";
import type { RunMessage } from "./api";

const tool = (name: unknown): RunMessage => ({seq: 1, kind: "tool_use", agent: "lead", agent_instance: "stage-lead", agent_label: "", payload: {name}, created_at: "2026-10-12T00:00:00Z"});

describe("run-wide stage", () => {
  it.each(["constructor", "__proto__", "toString", "future-tool"])("unknown tool %s stays plain Working text", name => {
    expect(stageForMessages([tool(name)])).toBe("Working");
  });
  it.each([null, 42, {toString: "not callable"}])("malformed tool name %j stays plain Working text", name => {
    expect(stageForMessages([tool(name)])).toBe("Working");
  });
  it("keeps the known tool stage and newest-frame precedence", () => {
    expect(stageForMessages([tool("Read"), tool("Bash")])).toBe("Running a command");
  });
  it("keeps the no-frame fallback", () => {
    expect(stageForMessages([])).toBe("Starting up");
  });
});
