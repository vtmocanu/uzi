import { describe, expect, it } from "vitest";
import type { AgentSourceView, AgentTemplate } from "./api";
import { agentSourceChipModel } from "./agentSourceChip";

const NOW = Date.parse("2026-11-15T12:00:00Z");
const VIEWER = "viewer";

function template(name: string, scope: AgentTemplate["scope"], origin: AgentTemplate["origin"], userId: string | null = null): AgentTemplate {
  return { id: name, name, scope, origin, user_id: userId, is_builtin: scope === "builtin" } as AgentTemplate;
}

const source: AgentSourceView = {
  config: { url: "https://github.com/acme/agents", ref: "v1.4", folder: ".claude/agents", enabled: true, interval: "1h", credential_configured: false },
  status: { last_sync_status: "ok", last_sync_at: "2026-11-15T10:00:00Z", last_applied_sha: "a1b2c3d4e5f6" },
  staged: null,
};

const options = { viewerId: VIEWER, isAdmin: true, source, demo: false, now: NOW };

describe("agentSourceChipModel", () => {
  it("counts templates directly, including lead and a shadowed own row, without allocations", () => {
    const rows = [
      template("lead", "builtin", "embedded"),
      template("coder", "builtin", "synced"),
      template("coder", "user", null, VIEWER),
      template("other-private", "user", null, "another-user"),
    ];
    const model = agentSourceChipModel(rows, options);
    expect(model.total).toBe(3);
    expect(model.groups.map((g) => [g.key, g.agents.length])).toEqual([["source", 1], ["shipped", 1], ["yours", 1]]);
    expect(model.groups.flatMap((g) => g.agents.map((a) => a.name))).not.toContain("other-private");
  });

  it("groups synced overrides and globals, admin-edited builtins, plain globals and own private templates", () => {
    const model = agentSourceChipModel([
      template("lead", "builtin", "embedded"),
      template("reviewer", "builtin", "synced"),
      template("tester", "builtin", "admin"),
      template("release", "global", "synced"),
      template("ui-copy", "global", null),
      template("mine", "user", null, VIEWER),
    ], options);
    expect(model.groups.map((g) => [g.key, g.agents.map((a) => a.name)])).toEqual([
      ["source", ["reviewer", "release"]],
      ["shipped", ["lead", "tester"]],
      ["admin", ["ui-copy"]],
      ["yours", ["mine"]],
    ]);
    expect(model.groups.find((g) => g.key === "shipped")?.agents[1].detail).toBe("edited by an admin");
  });

  it("orders sync suffixes failed, pending changes, update, synced, then none", () => {
    const pending = { fetched_sha: "new", source_url: "", source_ref: "", roles: [], diff: [], counts: { staged: 3, changed: 3, failed: 0 }, pending: true };
    const all = agentSourceChipModel([], { ...options, source: { ...source, status: { ...source.status, last_sync_status: "error", update_available: true }, staged: pending } });
    expect(all.suffix).toEqual({ text: "sync failed", tone: "danger" });
    expect(all.reviewInSettings).toBe(true);
    const review = agentSourceChipModel([], { ...options, source: { ...source, status: { ...source.status, update_available: true }, staged: pending } });
    expect(review.suffix).toEqual({ text: "3 changes to review", tone: "warn" });
    const zero = agentSourceChipModel([], { ...options, source: { ...source, staged: { ...pending, counts: { ...pending.counts, changed: 0 } } } });
    expect(zero.suffix?.text).toBe("review pending");
    const update = agentSourceChipModel([], { ...options, source: { ...source, status: { ...source.status, update_available: true } } });
    expect(update.suffix?.text).toBe("update available");
    expect(agentSourceChipModel([], options).suffix).toEqual({ text: "synced", tone: "muted" });
    expect(agentSourceChipModel([], { ...options, source: { ...source, status: {} } }).suffix).toBeNull();
  });

  it("hides admin sync status for non-admins, disabled sources and failed reads", () => {
    for (const opts of [
      { ...options, isAdmin: false },
      { ...options, source: { ...source, config: { ...source.config, enabled: false } } },
      { ...options, source: null },
    ]) {
      const model = agentSourceChipModel([template("lead", "builtin", "embedded")], opts);
      expect(model.total).toBe(1);
      expect(model.suffix).toBeNull();
      expect(model.sourceMeta).toBeNull();
      expect(model.reviewInSettings).toBe(false);
    }
  });

  it("strips control and bidi text from source metadata and masks URL/ref in demo mode", () => {
    const unsafe = { ...source, config: { ...source.config, url: "https://github.com/acme/\u202Eagents\u0007", ref: "v1\u202E.4\u0007" } };
    const normal = agentSourceChipModel([], { ...options, source: unsafe }).sourceMeta ?? "";
    expect(normal).not.toContain("\u202e");
    expect(normal).not.toContain("\u0007");
    expect(normal).toContain("github.com");
    const demo = agentSourceChipModel([], { ...options, source: unsafe, demo: true }).sourceMeta ?? "";
    expect(demo).toContain("https://forge.example.com@demo-ref");
    expect(demo).not.toContain("acme");
    expect(demo).not.toContain("\u202e");
    expect(demo).not.toContain("\u0007");
  });
});
