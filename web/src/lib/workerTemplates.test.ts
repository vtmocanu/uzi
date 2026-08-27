import { describe, it, expect } from "vitest";
import {
  DEFAULT_WORKER_TEMPLATE,
  WORKER_TEMPLATES,
  hasTemplateDrift,
} from "./workerTemplates";

describe("workerTemplates", () => {
  it("lists base first (the default)", () => {
    expect(WORKER_TEMPLATES[0]).toBe("base");
    expect(DEFAULT_WORKER_TEMPLATE).toBe("base");
    expect(WORKER_TEMPLATES).toContain("jvm");
  });

  describe("hasTemplateDrift", () => {
    it("is true only when both are set and differ", () => {
      expect(hasTemplateDrift("jvm", "base")).toBe(true);
      expect(hasTemplateDrift("base", "jvm")).toBe(true);
    });

    it("is false when they match", () => {
      expect(hasTemplateDrift("base", "base")).toBe(false);
      expect(hasTemplateDrift("jvm", "jvm")).toBe(false);
    });

    it("is false when either side is unknown (null)", () => {
      // No declared choice, or an older image that reports nothing, is unknown —
      // never drift, so it never badges.
      expect(hasTemplateDrift(null, "base")).toBe(false);
      expect(hasTemplateDrift("jvm", null)).toBe(false);
      expect(hasTemplateDrift(null, null)).toBe(false);
    });
  });
});
