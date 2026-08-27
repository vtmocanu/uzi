import { describe, it, expect } from "vitest";
import {
  coordKey,
  isCategory,
  JUDGE_CATEGORIES,
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
    expect(recommendationLabel("cost_efficiency")).toBe("Cost efficiency");
  });

  it("humanizes an unknown category rather than showing a raw enum", () => {
    expect(recommendationLabel("some_future_category")).toBe("some future category");
  });

  // cost_efficiency (the seventh category) must be END-TO-END live in the UI, not just
  // labelled: JUDGE_CATEGORIES drives both the label-filter chip row AND the ?category=
  // URL guard (isCategory), so this pins that a filter chip renders for it and a
  // ?category=cost_efficiency deep-link is accepted rather than silently dropped.
  it("wires cost_efficiency as a real filter chip and a valid ?category= deep-link", () => {
    expect(JUDGE_CATEGORIES).toContain("cost_efficiency");
    expect(isCategory("cost_efficiency")).toBe(true);
    // and an unknown token is still rejected by the same guard
    expect(isCategory("cost_savings")).toBe(false);
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
  // NOTE WHICH SIDE THIS PINS: RECOMMENDATION_LABELS is the TS MIRROR of the category enum,
  // not the database's own CHECK constraint (00059_run_reviews.sql, widened in 00127). So a
  // future DB category containing a space, added without updating this union, would slip past
  // this test. That is
  // a real gap and a small one: recommendationLabel already falls back to humanising an
  // unknown category, so the display side degrades gracefully, and the server's CHECK is the
  // thing that actually decides what categories exist. Recorded so a reader does not mistake
  // this for a guarantee about the DB.
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
    // The control-character class is the assertion. Narrowing it to satisfy the
    // linter would retire the regression guard this test exists to be.
    // eslint-disable-next-line no-control-regex
    expect(coordKey("install_worker_tool", "rg")).not.toMatch(/[\u0000-\u001f]/);
  });
});
