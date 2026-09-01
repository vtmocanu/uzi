import { describe, expect, it } from "vitest";
import { ApiError, errorMessage } from "./apiError";

describe("errorMessage", () => {
  it("returns the ApiError's message when given an ApiError", () => {
    expect(errorMessage(new ApiError(500, "boom"), "fb")).toBe("boom");
  });

  it("returns the fallback for a plain Error", () => {
    expect(errorMessage(new Error("x"), "fb")).toBe("fb");
  });

  it("returns the fallback for a string", () => {
    expect(errorMessage("str", "fb")).toBe("fb");
  });

  it("returns the fallback for undefined", () => {
    expect(errorMessage(undefined, "fb")).toBe("fb");
  });

  it("returns the fallback for null", () => {
    expect(errorMessage(null, "fb")).toBe("fb");
  });
});
