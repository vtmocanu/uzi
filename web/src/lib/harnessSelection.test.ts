// PRD #1429 M4a: the harness selection body-shaping rules, mirroring
// credentialOverride.ts's pattern for its own pure helpers.

import { describe, expect, it } from "vitest";
import {
  bothHarnessesUsable,
  effectiveHarnessIsCodex,
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

describe("effectiveHarnessIsCodex", () => {
  it("an explicit pick is authoritative regardless of usability facts", () => {
    expect(effectiveHarnessIsCodex("codex", true, true)).toBe(true);
    expect(effectiveHarnessIsCodex("codex", false, false)).toBe(true);
    expect(effectiveHarnessIsCodex("claude", true, true)).toBe(false);
    expect(effectiveHarnessIsCodex("claude", false, false)).toBe(false);
  });

  it("inherit with no default resolves Codex only when it is the SOLE usable harness", () => {
    expect(effectiveHarnessIsCodex("inherit", false, true)).toBe(true);
    expect(effectiveHarnessIsCodex("inherit", true, false)).toBe(false);
    // Both usable, no default ⇒ Claude (D11 rule 4) — the token control stays visible.
    expect(effectiveHarnessIsCodex("inherit", true, true)).toBe(false);
  });

  // PRD #1429 M4a review Fix 1 (D11 rule 2): a USABLE user default wins over plain
  // availability on an untouched "inherit" pick, in either direction.
  it("inherit with a usable Codex default resolves to Codex even when both are usable", () => {
    expect(effectiveHarnessIsCodex("inherit", true, true, "codex")).toBe(true);
  });

  it("inherit with a usable Claude default resolves to Claude even when both are usable", () => {
    expect(effectiveHarnessIsCodex("inherit", true, true, "claude")).toBe(false);
  });

  it("a Codex default that is NOT usable falls through to plain availability", () => {
    // Codex unusable ⇒ the default is skipped (resolveHarness's fall-through), so a
    // Claude-only user still reads Claude despite the stale Codex default.
    expect(effectiveHarnessIsCodex("inherit", true, false, "codex")).toBe(false);
    // Sole-Codex-usable still resolves Codex via availability, default aside.
    expect(effectiveHarnessIsCodex("inherit", false, true, "codex")).toBe(true);
  });

  it("a Claude default that is NOT usable falls through to plain availability", () => {
    expect(effectiveHarnessIsCodex("inherit", false, true, "claude")).toBe(true);
  });

  it("no default (null) behaves exactly as omitting the parameter", () => {
    expect(effectiveHarnessIsCodex("inherit", true, true, null)).toBe(
      effectiveHarnessIsCodex("inherit", true, true),
    );
  });
});
