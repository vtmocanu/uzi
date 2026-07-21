import { describe, it, expect } from "vitest";
import {
  coordKey,
  RECOMMENDATION_LABELS,
  recommendationLabel,
  verdictLabel,
  verdictTone,
} from "./judge";

describe("judge display helpers (PRD #46 M4)", () => {
  it("maps each verdict to a tone and label", () => {
    expect(verdictTone("ideal")).toBe("ok");
    expect(verdictTone("ok")).toBe("info");
    expect(verdictTone("issues")).toBe("warning");
    expect(verdictLabel("ideal")).toBe("Ideal");
    expect(verdictLabel("ok")).toBe("OK");
    expect(verdictLabel("issues")).toBe("Issues found");
  });

  it("maps each recommendation category to user copy", () => {
    expect(recommendationLabel("install_worker_tool")).toBe("Install a worker tool");
    expect(recommendationLabel("improve_uzi")).toBe("Improve uzi");
    expect(recommendationLabel("add_agent")).toBe("Add a missing agent");
  });

  it("humanizes an unknown category rather than showing a raw enum", () => {
    expect(recommendationLabel("some_future_category")).toBe("some future category");
  });
});

// PRD #98 review B1/N3: coordKey is now defined ONCE and imported by RunView, the Judge
// page and mockApi, instead of being restated in each. The two properties that make a
// single space a sound separator, and the one that makes it a sound BYTE, are pinned here.
describe("coordKey (PRD #68/#94/#98)", () => {
  // The space separator is collision-free CONDITIONALLY, and the condition is the thing
  // worth testing. `coordKey("improve_uzi", "docs site")` and
  // `coordKey("improve_uzi docs", "site")` are in fact EQUAL — an earlier draft of this
  // test asserted they differ and was simply wrong. What makes the separator sound is that
  // the second of those calls is unreachable: `category` is a closed wire enum whose
  // members contain no space, so the split point is unambiguous however the arbitrary
  // `target` is spelled. Pinning the enum is therefore the real guard; asserting a
  // general non-collision would be asserting something false.
  it("keeps the categories space-free — the precondition the separator rests on", () => {
    for (const category of Object.keys(RECOMMENDATION_LABELS)) {
      expect(category).not.toMatch(/\s/);
    }
  });

  it("distinguishes distinct coordinates across legal categories", () => {
    expect(coordKey("improve_uzi", "docs")).not.toBe(coordKey("tests", "docs"));
    expect(coordKey("tests", "unit")).not.toBe(coordKey("tests", "integration"));
    expect(coordKey("tests", "unit")).toBe(coordKey("tests", "unit"));
    // Whitespace in the target is still significant, so a trailing space is a distinct
    // target rather than being folded away.
    expect(coordKey("tests", "unit")).not.toBe(coordKey("tests", "unit "));
  });

  it("emits no control byte — a NUL separator makes the SOURCE binary to git and grep", () => {
    // The regression this exists for: the Judge page shipped with a literal NUL here, so
    // its 821 lines landed as `Bin 0 -> 32202 bytes` with a zero-line diff, and plain
    // grep/rg silently returned no matches on the file. Runtime behaviour was fine;
    // reviewability was not. Asserting the OUTPUT is byte-clean is what makes a
    // re-introduction fail rather than merely be discouraged by a comment.
    expect(coordKey("install_worker_tool", "rg")).not.toMatch(/[\u0000-\u001f]/);
  });
});
