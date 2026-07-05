import { describe, it, expect } from "vitest";
import { countFindings, privilegeBadge, repoFindings } from "./privilege";
import type { PrivilegeReport } from "./api";

function report(over: Partial<PrivilegeReport> = {}): PrivilegeReport {
  return {
    checked_at: "2026-07-05T12:00:00Z",
    status: "ok",
    token: { scopes: ["api"], active: true, violations: [], warnings: [] },
    repos: [],
    ...over,
  };
}

describe("privilegeBadge", () => {
  it("renders unchecked for a null status, never a tick", () => {
    const b = privilegeBadge(null, null);
    expect(b.tone).toBe("neutral");
    expect(b.label).toBe("unchecked");
  });
  it("renders least-privilege for ok", () => {
    expect(privilegeBadge("ok", report())).toEqual({ tone: "ok", label: "least-privilege ✓" });
  });
  it("counts and pluralizes warnings", () => {
    const r = report({
      status: "warnings",
      token: { scopes: ["api"], active: true, violations: [], warnings: ["token expires within 14 days"] },
    });
    expect(privilegeBadge("warnings", r)).toEqual({ tone: "warning", label: "1 warning" });
  });
  it("counts violations across token and repos", () => {
    const r = report({
      status: "violations",
      token: { scopes: ["api", "sudo"], active: true, violations: ["token scopes exceed"], warnings: [] },
      repos: [
        { repo_id: "r1", path: "g/one", role: 40, member: true, violations: ["bot role is Maintainer"], warnings: [] },
      ],
    });
    expect(privilegeBadge("violations", r)).toEqual({ tone: "danger", label: "2 violations" });
  });
  it("renders check failed for error", () => {
    expect(privilegeBadge("error", report({ status: "error" })).label).toBe("check failed");
  });
});

describe("countFindings", () => {
  it("is 0 for a null report", () => {
    expect(countFindings(null, "violations")).toBe(0);
  });
});

describe("repoFindings", () => {
  const r = report({
    repos: [
      { repo_id: "clean", path: "g/clean", role: 30, member: true, violations: [], warnings: [] },
      { repo_id: "bad", path: "g/bad", role: 40, member: true, violations: ["bot role is Maintainer (40)"], warnings: [] },
    ],
  });
  it("returns null for a repo with no entry", () => {
    expect(repoFindings(r, "missing")).toBeNull();
  });
  it("returns null for a clean repo", () => {
    expect(repoFindings(r, "clean")).toBeNull();
  });
  it("returns the findings for a repo with violations", () => {
    expect(repoFindings(r, "bad")).toEqual({ violations: ["bot role is Maintainer (40)"], warnings: [] });
  });
});
