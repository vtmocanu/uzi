import { describe, it, expect } from "vitest";
import { brandTabTitle, DEFAULT_TITLE } from "./brandTitle";
import type { Branding } from "./api";

// brandingWith builds a Branding starting from the default (unbranded) shape, so each
// case only names the fields the white-label gate reads (mode / keep_name / company).
const DEFAULT_BRANDING: Branding = {
  app_logo_mode: "default",
  app_logo_preset: "",
  app_logo_present: false,
  app_logo_keep_name: true,
  brand_mode: "none",
  brand_company: "",
  brand_placement: "below",
  brand_plaque: false,
  brand_logo_present: false,
};

function brandingWith(overrides: Partial<Branding>): Branding {
  return { ...DEFAULT_BRANDING, ...overrides };
}

describe("brandTabTitle", () => {
  it("null branding → DEFAULT_TITLE", () => {
    expect(brandTabTitle(null)).toBe(DEFAULT_TITLE);
  });

  it("default mode, keep_name true, empty company → DEFAULT_TITLE", () => {
    expect(brandTabTitle(brandingWith({}))).toBe(DEFAULT_TITLE);
  });

  it("default mode with a non-empty company → DEFAULT_TITLE (default is never white-label)", () => {
    expect(brandTabTitle(brandingWith({ brand_company: "Acme, Inc." }))).toBe(DEFAULT_TITLE);
  });

  it("custom mode, keep_name TRUE (co-brand) → DEFAULT_TITLE (keep_name keeps uzi)", () => {
    expect(
      brandTabTitle(brandingWith({ app_logo_mode: "custom", app_logo_keep_name: true, brand_company: "Acme, Inc." })),
    ).toBe(DEFAULT_TITLE);
  });

  it("custom mode, keep_name FALSE → the brand_company", () => {
    expect(
      brandTabTitle(brandingWith({ app_logo_mode: "custom", app_logo_keep_name: false, brand_company: "Acme, Inc." })),
    ).toBe("Acme, Inc.");
  });

  it("preset mode, keep_name FALSE → the brand_company", () => {
    expect(
      brandTabTitle(brandingWith({ app_logo_mode: "preset", app_logo_keep_name: false, brand_company: "Acme, Inc." })),
    ).toBe("Acme, Inc.");
  });

  it("white-label with an empty brand_company → DEFAULT_TITLE (fallback)", () => {
    expect(
      brandTabTitle(brandingWith({ app_logo_mode: "custom", app_logo_keep_name: false, brand_company: "" })),
    ).toBe(DEFAULT_TITLE);
  });

  it("white-label with a whitespace-only brand_company → DEFAULT_TITLE (trim)", () => {
    expect(
      brandTabTitle(brandingWith({ app_logo_mode: "custom", app_logo_keep_name: false, brand_company: "   " })),
    ).toBe(DEFAULT_TITLE);
  });

  it("DEFAULT_TITLE uses the U+2014 em dash, not a hyphen", () => {
    expect(DEFAULT_TITLE).toBe("Uzi — AI dark factory");
  });
});
