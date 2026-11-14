import { describe, expect, it } from "vitest";
import { describeCredentialSwitch, isInPlaceSwitch, selectReasonText } from "./credentialSwitchFeed";

describe("credentialSwitchFeed (issue #4201)", () => {
  it.each([
    [{ ordinal: 1 }, true],
    [{ ordinal: 3 }, true],
    [{ ordinal: 0 }, false],
    [{ ordinal: 1.5 }, false],
    [{ ordinal: "1" }, false],
    [{}, false],
    [null, false],
  ])("isInPlaceSwitch(%j) = %s", (payload, want) => {
    expect(isInPlaceSwitch(payload)).toBe(want);
  });

  it("names an in-place switch, its token and why", () => {
    expect(describeCredentialSwitch({ label: "console-key", select_reason: "override_auto", secret_id: "s", ordinal: 1 }))
      .toBe("Switched in place to “console-key” (auto-selected by override)");
  });

  it("a claim-time switch omits 'in place'; an empty reason drops the parenthetical", () => {
    expect(describeCredentialSwitch({ label: "  ", select_reason: "", secret_id: "s" })).toBe("Switched to an unlabelled token");
  });

  it("tolerates a malformed payload", () => {
    expect(describeCredentialSwitch("nope")).toBe("Switched to an unlabelled token");
    expect(describeCredentialSwitch({ label: 7, select_reason: 9 })).toBe("Switched to an unlabelled token");
  });

  it("selectReasonText maps known reasons and passes an unknown one through", () => {
    expect(selectReasonText("codex_binding")).toBe("Codex binding");
    expect(selectReasonText("future_reason")).toBe("future_reason");
    expect(selectReasonText(null)).toBe("");
  });
});
