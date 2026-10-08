// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";

beforeEach(() => {
  window.localStorage.clear();
  vi.resetModules();
});
afterEach(() => vi.resetModules());

it("lists enabled instance repos and only currently trusted disabled existing repos with minimal metadata", async () => {
  const { mockApi } = await import("./mockApi");
  const { repos } = await import("./mockApi/forge");
  const enabled = "a1111111-1111-4111-8111-111111111111";
  const disabled = "b2222222-2222-4222-8222-222222222222";
  const deleted = "c3333333-3333-4333-8333-333333333333";
  const initial = await mockApi.adminListDockerAllowlistRepos();
  expect(initial.repos.find((r) => r.id === enabled)).toMatchObject({
    enabled: true, owner_email: "dana@example.com", connection_id: "conn-dana",
    forge_type: "forgejo", base_url: "https://forge.dana.example.com",
  });
  expect(initial.repos.some((r) => r.id === disabled)).toBe(false);
  expect(initial.repos.some((r) => r.id === "repo-www")).toBe(false);
  await mockApi.updateSettings({ docker_repo_allowlist: [disabled.toUpperCase(), deleted].join(",") });
  const trusted = await mockApi.adminListDockerAllowlistRepos();
  expect(trusted.repos.find((r) => r.id === disabled)).toMatchObject({ enabled: false });
  expect(trusted.repos.some((r) => r.id === deleted)).toBe(false);
  for (const r of trusted.repos) {
    expect(Object.keys(r).sort()).toEqual(["id", "path_with_namespace", "enabled", "owner_email", "connection_id", "forge_type", "base_url"].sort());
  }
  repos.find((r) => r.id === "repo-uzi")!.enabled = false;
  expect((await mockApi.adminListDockerAllowlistRepos()).repos.some((r) => r.id === "repo-uzi")).toBe(false);
  await mockApi.updateSettings({ docker_repo_allowlist: "" });
  expect((await mockApi.adminListDockerAllowlistRepos()).repos.some((r) => r.id === disabled)).toBe(false);
  expect((await mockApi.listRepos()).repos.some((r) => r.id === enabled)).toBe(false);
});

it("resolves stored UUID aliases when retaining disabled repositories", async () => {
  const { mockApi } = await import("./mockApi");
  const { appSettings } = await import("./mockApi/settings");
  const id = "b2222222-2222-4222-8222-222222222222";
  for (const value of [
    id.replace(/-/g, ""), "x" + id.toUpperCase() + "y",
    "UrN:UuId:" + id, "\u0085" + id + "\u0085",
  ]) {
    // Seed an already stored spelling; the listing reads current state, independently of writes.
    appSettings.docker_repo_allowlist = value;
    expect((await mockApi.adminListDockerAllowlistRepos()).repos.some((r) => r.id === id)).toBe(true);
  }
  appSettings.docker_repo_allowlist = "é" + id + "é";
  expect((await mockApi.adminListDockerAllowlistRepos()).repos.some((r) => r.id === id)).toBe(false);
});

it("requires an administrator session", async () => {
  const { mockApi } = await import("./mockApi");
  const { state } = await import("./store");
  state.session = { ...state.session!, is_admin: false };
  await expect(mockApi.adminListDockerAllowlistRepos()).rejects.toMatchObject({ status: 403 });
  state.session = null;
  await expect(mockApi.adminListDockerAllowlistRepos()).rejects.toMatchObject({ status: 401 });
});
