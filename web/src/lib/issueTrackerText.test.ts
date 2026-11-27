import { describe, expect, it } from "vitest";
import { issueKey } from "./issueTrackerText";

describe("issueKey", () => {
  it("joins the project short name and the issue number with a hyphen, as the API does", () => {
    expect(issueKey("ATL", 12)).toBe("ATL-12");
    expect(issueKey("A_1", 1)).toBe("A_1-1");
  });
});
