// @vitest-environment jsdom
import { describe, expect, it } from "vitest";
import { Navigate } from "react-router-dom";
import { APP_ROUTES } from "../App";
import { TABS as SETTINGS_TABS } from "./SettingsShell";
import { TABS as ADMIN_TABS } from "./AdminShell";
import { ADMIN_SETTINGS_ANCHOR_IDS, ADMIN_WORKERS_ANCHOR_IDS, visibleSectionItems } from "./navSections";

const admin = visibleSectionItems(true);
const pathOf = (to: string) => to.split("#")[0];

describe("navSections", () => {
  it("points every item at a real, non-redirect route", () => {
    for (const item of admin) {
      const route = APP_ROUTES.find((r) => r.path === pathOf(item.to));
      expect(route, `${item.label} -> ${item.to} has no route`).toBeTruthy();
      expect(route!.element.type, `${item.to} is a redirect`).not.toBe(Navigate);
    }
  });

  it("mirrors the Settings shell tabs, plus Workers at /workers", () => {
    const items = admin.filter((i) => i.key.startsWith("settings:"));
    const tabs = SETTINGS_TABS.map((t) => ({ to: t.to, label: `Settings › ${t.label}` }));
    expect(items.map((i) => ({ to: i.to, label: i.label }))).toEqual([
      ...tabs,
      { to: "/workers", label: "Settings › Workers" },
    ]);
    expect(items.every((i) => !i.adminOnly)).toBe(true);
  });

  // PRD #3263: model vocabulary finds Models; Run defaults keeps only behaviour words, plus
  // "harness"/"overflow" for the harness-overflow setting that stays there.
  it("advertises Models for model searches and keeps Run defaults behaviour-only", () => {
    const models = admin.find((i) => i.to === "/settings/models")!;
    expect(models.label).toBe("Settings › Models");
    expect(models.keywords).toEqual(
      expect.arrayContaining(["model", "models", "effort", "harness", "planning", "cross-check", "judge", "summary"]),
    );
    const runDefaults = admin.find((i) => i.to === "/settings/run-defaults")!;
    expect(runDefaults.keywords).not.toContain("model");
    expect(runDefaults.keywords).toEqual(expect.arrayContaining(["run", "defaults", "harness", "overflow"]));
  });

  it("mirrors the Admin shell tabs, naming the Instance tab Settings", () => {
    const items = admin.filter((i) => i.key.startsWith("admin:"));
    const tabs = ADMIN_TABS.map((t) => ({
      to: t.to,
      label: `Admin › ${t.to === "/admin/settings" ? "Settings" : t.label}`,
    }));
    expect(items.map((i) => ({ to: i.to, label: i.label }))).toEqual(tabs);
    expect(ADMIN_TABS.find((t) => t.to === "/admin/settings")?.label).toBe("Instance");
    const instance = items.find((i) => i.to === "/admin/settings")!;
    expect(instance.keywords).toContain("instance");
  });

  it("advertises exactly the 14 unconditional Admin Settings sections", () => {
    expect([...ADMIN_SETTINGS_ANCHOR_IDS]).toEqual([
      "vault-policy", "model-defaults", "forge-labels", "run-judge", "run-summaries", "agent-source", "maintenance",
      "updates", "slack", "run-health", "review-bots",
      "completion-interlock", "github-project-sync", "fetch-caps",
    ]);
    expect(ADMIN_SETTINGS_ANCHOR_IDS.has("vault-migration")).toBe(false);
    expect(ADMIN_SETTINGS_ANCHOR_IDS.has("sso")).toBe(false);
    const anchors = admin.filter((i) => i.key.startsWith("admin-settings:"));
    expect(anchors.map((i) => i.to.split("#")[1])).toEqual([...ADMIN_SETTINGS_ANCHOR_IDS]);
    for (const a of anchors) expect(a.label.startsWith("Admin › Settings › ")).toBe(true);
    expect(anchors.some((a) => a.label === "Admin › Settings › Agent source")).toBe(true);
  });

  it("advertises the seven worker-fleet sections under Admin › Workers, none under Settings", () => {
    const ids = [
      "docker-allowlist", "worker-sizes", "hosted-worker-quota", "custody-hold-limit",
      "ephemeral-workers", "worker-hibernation", "capability-scheduling",
    ];
    expect([...ADMIN_WORKERS_ANCHOR_IDS]).toEqual(ids);
    for (const id of ids) expect(ADMIN_SETTINGS_ANCHOR_IDS.has(id), id).toBe(false);
    const anchors = admin.filter((i) => i.key.startsWith("admin-workers:"));
    expect(anchors.map((i) => i.to)).toEqual(ids.map((id) => `/admin/workers#${id}`));
    for (const a of anchors) expect(a.label.startsWith("Admin › Workers › ")).toBe(true);
    expect(anchors.some((a) => a.label === "Admin › Workers › Ephemeral workers")).toBe(true);
    const keywords = (id: string) => anchors.find((a) => a.to.endsWith(`#${id}`))!.keywords;
    expect(keywords("ephemeral-workers")).toEqual(expect.arrayContaining(["cap", "per-user", "lease", "ephemeral size", "default size"]));
    expect(keywords("hosted-worker-quota")).toEqual(expect.arrayContaining(["default size", "size", "quota"]));
    expect(admin.some((i) => i.to === "/admin/workers" && i.label === "Admin › Workers")).toBe(true);
  });

  it("hides admin entries from non-admins", () => {
    const user = visibleSectionItems(false);
    expect(user.length).toBeGreaterThan(0);
    expect(user.some((i) => i.adminOnly)).toBe(false);
    expect(user.some((i) => i.to.startsWith("/admin"))).toBe(false);
    expect(admin.some((i) => i.adminOnly)).toBe(true);
    expect(admin.length).toBeGreaterThan(user.length);
  });

  it("has unique keys", () => {
    expect(new Set(admin.map((i) => i.key)).size).toBe(admin.length);
  });
});
