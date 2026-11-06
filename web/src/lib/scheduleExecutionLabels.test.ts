import { describe, expect, it } from "vitest";
import {
  harnessInheritLabel,
  modelDefaultLabel,
  modelLabelHarness,
  mrReworkInheritLabel,
  overflowMayApply,
  type ExecutionLabelInput,
} from "./scheduleExecutionLabels";

function input(over: Partial<ExecutionLabelInput> = {}): ExecutionLabelInput {
  return {
    target: "issue",
    harnessSel: "inherit",
    model: "",
    credentialMode: "inherit",
    claudeUsable: true,
    codexUsable: true,
    ownerDefaultHarness: "claude",
    overflowEnabled: true,
    ...over,
  };
}

describe("overflowMayApply", () => {
  it("is true for the full conjunction", () => {
    expect(overflowMayApply(input())).toBe(true);
    expect(overflowMayApply(input({ target: "sweep", ownerDefaultHarness: "codex" }))).toBe(true);
  });

  it.each<[string, Partial<ExecutionLabelInput>]>([
    ["overflow off", { overflowEnabled: false }],
    ["claude unusable", { claudeUsable: false }],
    ["codex unusable", { codexUsable: false }],
    ["prompt target", { target: "prompt" }],
    ["self_improve target", { target: "self_improve" }],
    ["ci_watch target", { target: "ci_watch" }],
    ["harness pinned", { harnessSel: "codex" }],
    ["model pinned", { model: "opus" }],
    ["token default", { credentialMode: "default" }],
    ["token auto", { credentialMode: "auto" }],
    ["token pinned", { credentialMode: "pinned" }],
    ["no explicit default", { ownerDefaultHarness: null }],
  ])("is false when %s", (_n, over) => {
    expect(overflowMayApply(input(over))).toBe(false);
  });

  it("treats a whitespace-only model as unpinned", () => {
    expect(overflowMayApply(input({ model: "  " }))).toBe(true);
  });
});

describe("modelLabelHarness ordering", () => {
  it("an explicit pin wins", () => {
    expect(modelLabelHarness(input({ harnessSel: "codex" }))).toBe("codex");
  });
  it("overflow wins over an inherited usable Codex default", () => {
    expect(modelLabelHarness(input({ ownerDefaultHarness: "codex" }))).toBeUndefined();
  });
  it("without overflow, a usable Codex default names codex", () => {
    expect(modelLabelHarness(input({ ownerDefaultHarness: "codex", overflowEnabled: false }))).toBe("codex");
  });
  it("without overflow, a usable Claude default names claude", () => {
    expect(modelLabelHarness(input({ overflowEnabled: false }))).toBe("claude");
  });
  it("Claude-only names claude; both usable with no default is unknown", () => {
    expect(modelLabelHarness(input({ codexUsable: false, ownerDefaultHarness: null }))).toBe("claude");
    expect(modelLabelHarness(input({ ownerDefaultHarness: null }))).toBeUndefined();
  });
});

describe("labels", () => {
  it("harness inherit wording", () => {
    expect(harnessInheritLabel(input())).toBe("Default · Claude (your default; may overflow to Codex when it fires)");
    expect(harnessInheritLabel(input({ ownerDefaultHarness: "codex" }))).toBe(
      "Default · Codex (your default; may overflow to Claude when it fires)",
    );
    expect(harnessInheritLabel(input({ overflowEnabled: false }))).toBe("Default · Claude (your default)");
    expect(harnessInheritLabel(input({ ownerDefaultHarness: null }))).toBe(
      "Default · automatic (chosen when it fires)",
    );
  });
  it("model default wording", () => {
    expect(modelDefaultLabel(input())).toBe("Default · your worker model");
    expect(modelDefaultLabel(input({ overflowEnabled: false, defaultClaudeModel: "opus" }))).toBe(
      "Default · opus (your Claude default)",
    );
    expect(modelDefaultLabel(input({ overflowEnabled: false, defaultClaudeModel: "" }))).toBe(
      "Default · your Claude worker model",
    );
    expect(modelDefaultLabel(input({ harnessSel: "codex", defaultCodexModel: "x" }))).toBe(
      "Default · x (your Codex default)",
    );
  });
  it("auto-rework wording", () => {
    expect(mrReworkInheritLabel(false, true)).toBe("Default · your account setting");
    expect(mrReworkInheritLabel(true, false)).toBe("Default · off (your setting)");
    expect(mrReworkInheritLabel(true, null)).toBe("Default · on (your setting)");
    expect(mrReworkInheritLabel(true, undefined)).toBe("Default · on (your setting)");
  });
});
