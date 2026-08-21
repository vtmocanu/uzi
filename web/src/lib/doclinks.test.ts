import { describe, it, expect } from "vitest";
import { listUserDocs } from "./docs";
import { ALL_DOC_SLUGS } from "./doclinks";

describe("doclinks registry", () => {
  // Non-empty guard: the bundled user-doc glob must actually populate under
  // vitest, or Test B would pass vacuously (every slug trivially "found" in an
  // empty set is impossible, but an empty user-doc set would make the Set empty
  // and every assertion fail — so assert the corpus is real first).
  it("bundles a non-empty set of user docs", () => {
    expect(listUserDocs().length).toBeGreaterThan(0);
  });

  const userSlugs = new Set(listUserDocs().map((d) => d.slug));
  it.each(ALL_DOC_SLUGS)("registry slug %s is an audience:user doc", (slug) => {
    expect(userSlugs.has(slug)).toBe(true);
  });
});
