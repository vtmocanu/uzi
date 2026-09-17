// PRD #1429 M4a: the harness selection body-shaping rules, mirroring
// credentialOverride.ts's pattern for its own pure helpers.

import { describe, expect, it } from "vitest";
import {
  bothHarnessesUsable,
  runHarnessBody,
  scheduleHarnessPatch,
  selectionFromHarness,
} from "./harnessSelection";

describe("runHarnessBody", () => {
  it("sends nothing for inherit (byte-identical to a pre-M4a create)", () => {
    expect(runHarnessBody("inherit")).toBeUndefined();
  });
  it("sends the bare harness string for an explicit choice", () => {
    expect(runHarnessBody("claude")).toBe("claude");
    expect(runHarnessBody("codex")).toBe("codex");
  });
});

describe("scheduleHarnessPatch", () => {
  it("omits the field when the control was never touched, regardless of value", () => {
    expect(scheduleHarnessPatch("codex", false)).toBeUndefined();
    expect(scheduleHarnessPatch("inherit", false)).toBeUndefined();
  });
  it("sends an explicit null when the user picks inherit deliberately", () => {
    expect(scheduleHarnessPatch("inherit", true)).toBeNull();
  });
  it("sends the bare harness string for an explicit pin", () => {
    expect(scheduleHarnessPatch("claude", true)).toBe("claude");
    expect(scheduleHarnessPatch("codex", true)).toBe("codex");
  });
});

describe("selectionFromHarness", () => {
  it("reads a stored pin back into a selection", () => {
    expect(selectionFromHarness("codex")).toBe("codex");
    expect(selectionFromHarness("claude")).toBe("claude");
  });
  it("reads null/undefined as inherit", () => {
    expect(selectionFromHarness(null)).toBe("inherit");
    expect(selectionFromHarness(undefined)).toBe("inherit");
  });
});

describe("bothHarnessesUsable", () => {
  it("is true only when both are usable", () => {
    expect(bothHarnessesUsable(true, true)).toBe(true);
    expect(bothHarnessesUsable(true, false)).toBe(false);
    expect(bothHarnessesUsable(false, true)).toBe(false);
    expect(bothHarnessesUsable(false, false)).toBe(false);
  });
});
