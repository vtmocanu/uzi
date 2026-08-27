import { describe, it, expect } from "vitest";
import type { AgentTemplate } from "./api";
import { provenanceBadgeKind, templateOrigin } from "./agentTemplates";

// A minimal row factory: only the fields the provenance helpers read matter, so the
// tests stay about the branching rather than a full AgentTemplate literal.
function row(over: Partial<AgentTemplate>): Pick<AgentTemplate, "origin" | "is_builtin" | "differs_from_builtin"> {
  return {
    origin: over.origin,
    is_builtin: over.is_builtin ?? false,
    differs_from_builtin: over.differs_from_builtin ?? false,
  };
}

describe("templateOrigin — the effective provenance (PRD #602 M5)", () => {
  it("defaults an origin-less BUILTIN to embedded (the server backfill)", () => {
    expect(templateOrigin(row({ is_builtin: true }))).toBe("embedded");
  });

  it("has NO provenance for an origin-less non-builtin (a plain global / user row)", () => {
    expect(templateOrigin(row({ is_builtin: false }))).toBeNull();
    expect(templateOrigin(row({ is_builtin: false, origin: null }))).toBeNull();
  });

  it("passes through an explicit origin", () => {
    expect(templateOrigin(row({ is_builtin: true, origin: "admin" }))).toBe("admin");
    expect(templateOrigin(row({ is_builtin: true, origin: "synced" }))).toBe("synced");
  });
});

describe("provenanceBadgeKind — which chip shows (PRD #602 M5)", () => {
  it("shows the SYNCED chip for a synced-overridden builtin, INSTEAD of the drift chip", () => {
    // A synced body legitimately differs from the embedded default: it must not be
    // labelled "differs from shipped".
    expect(provenanceBadgeKind(row({ is_builtin: true, origin: "synced", differs_from_builtin: true }))).toBe(
      "synced",
    );
  });

  it("shows the SYNCED chip for a synced-only GLOBAL role with no shipped counterpart", () => {
    // scope='global', origin='synced', differs_from_builtin false (no builtin twin).
    // The badge is the synced provenance; there is nothing to reset to (see the
    // AgentDetail no-reset path — is_builtin is false, so only Delete is offered).
    const synced = row({ is_builtin: false, origin: "synced", differs_from_builtin: false });
    expect(provenanceBadgeKind(synced)).toBe("synced");
  });

  it("shows the DRIFT chip for an admin-edited builtin", () => {
    expect(provenanceBadgeKind(row({ is_builtin: true, origin: "admin", differs_from_builtin: true }))).toBe(
      "drift",
    );
  });

  it("shows NOTHING for a pristine embedded builtin", () => {
    expect(provenanceBadgeKind(row({ is_builtin: true, origin: "embedded", differs_from_builtin: false }))).toBeNull();
  });

  it("shows NOTHING for a plain global / user row that does not drift", () => {
    expect(provenanceBadgeKind(row({ is_builtin: false, origin: null, differs_from_builtin: false }))).toBeNull();
  });

  it("shows the DRIFT chip for a drifting builtin whose origin field is absent (rollout skew)", () => {
    // A pre-#602 server omits origin; an origin-less builtin that drifts still badges
    // "differs from shipped" — it is never mislabelled synced.
    expect(provenanceBadgeKind(row({ is_builtin: true, differs_from_builtin: true }))).toBe("drift");
  });
});
