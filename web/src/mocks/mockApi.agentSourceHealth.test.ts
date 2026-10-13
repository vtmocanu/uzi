// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";

// API assertions need no simulated network jitter.
vi.mock("./mockApi/shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./mockApi/shared")>(),
  delay: <T>(value: T) => Promise.resolve(value),
}));

async function fresh(scenario: string) {
  vi.resetModules();
  window.history.replaceState({}, "", scenario ? `/?mock=${scenario}` : "/");
  Object.defineProperty(window, "localStorage", {
    configurable: true,
    value: { getItem: () => null, setItem: () => {}, removeItem: () => {} },
  });
  const { mockApi } = await import("./mockApi");
  await mockApi.login("vlad@uzi.local", "x");
  return mockApi;
}

afterEach(() => {
  window.history.replaceState({}, "", "/");
  vi.resetModules();
});

describe("mutable agent source Health", () => {
  it.each(["codex-only", "oidc", "run-progress", "judge-admin"])("seeds passing Health and stable card facts for unrelated scenario %s", async (scenario) => {
    const api = await fresh(scenario);
    const doc = await api.getAdminHealth();
    expect(doc).toMatchObject({ status: "ok", blocking: false });
    expect(doc.counts).toEqual({ ok: 21, warn: 0, danger: 0, unknown: 0, na: 0 });
    expect(doc.checks).toHaveLength(21);
    expect(doc.checks.every((c) => c.severity === "ok")).toBe(true);
    expect(doc.checks.find((c) => c.id === "agentsource.update"))
      .toMatchObject({ severity: "ok", summary: "Agent source is up to date." });
    const card = (await api.getAgentSource()).agent_source;
    expect(card.status.update_available).toBe(false);
    expect(card.status.latest_ref).toBeUndefined();
    expect(card.status.update_checked_at).toBeTruthy();
    await api.getAdminHealth();
    expect((await api.getAgentSource()).agent_source.status).toMatchObject({
      update_available: false,
      update_checked_at: card.status.update_checked_at,
    });
  });

  it.each(["health-first", "card-first"])("seeds once before either read (%s), then keeps a saved pin", async (order) => {
    const api = await fresh("agent-source-update");
    const health = order === "health-first" ? await api.getAdminHealth() : null;
    const card = (await api.getAgentSource()).agent_source;
    const doc = health ?? await api.getAdminHealth();
    const index = doc.checks.findIndex((c) => c.id === "agentsource.update");
    expect(doc.checks).toHaveLength(21);
    expect(doc.checks[index - 1].id).toBe("release.check");
    expect(doc.checks[index + 1].id).toBe("pricing.codex");
    expect(doc.checks[index]).toMatchObject({
      scope: "instance", group: "housekeeping", title: "Agent source update", doc: "agent-source",
      severity: "warn", summary: "Agent source newer tag v1.10.0 is available.",
      action: "Review agent source updates at /admin/settings#agent-source.",
    });
    expect(doc).toMatchObject({ status: "warn", blocking: false, counts: { ok: 20, warn: 1, unknown: 0 } });
    expect(card.config).toMatchObject({ enabled: true, ref: "v1.2.0" });
    expect(card.status).toMatchObject({ update_available: true, latest_ref: "v1.10.0" });
    expect(card.status.update_checked_at).toBeTruthy();
    await api.updateSettings({ agent_source_ref: "v1.10.0" });
    for (let i = 0; i < 2; i++) {
      expect((await api.getAdminHealth()).checks[index]).toMatchObject({ severity: "ok", summary: "Agent source is up to date." });
      expect((await api.getAgentSource()).agent_source.config.ref).toBe("v1.10.0");
      expect((await api.getAgentSource()).agent_source.status.update_available).toBe(false);
    }
  });

  it("tracks branch apply against the same staged remote tip and preserves unrelated incident/snooze", async () => {
    const api = await fresh("agent-source-update");
    await api.updateSettings({ agent_source_ref: "main" });
    const before = (await api.getAgentSource()).agent_source;
    expect(before.status.update_available).toBe(true);
    expect((await api.getAdminHealth()).checks.find((c) => c.id === "agentsource.update"))
      .toMatchObject({ severity: "warn", summary: "Agent source tracked branch moved." });
    await api.applyAgentSource(before.staged!.fetched_sha);
    const after = (await api.getAgentSource()).agent_source;
    expect(after.status.last_applied_sha).toBe(before.staged!.fetched_sha);
    expect(after.status.update_available).toBe(false);
    expect((await api.getAdminHealth()).counts).toEqual({ ok: 21, warn: 0, danger: 0, unknown: 0, na: 0 });
    window.history.replaceState({}, "", "/?mock=health-incident");
    await api.snoozeAdminHealth();
    const incident = await api.getAdminHealth();
    expect(incident).toMatchObject({ status: "danger", blocking: true, episode_id: "b1f0c2ep" });
    expect(incident.snoozed_until).toBeTruthy();
    expect(incident.counts).toEqual({ ok: 17, warn: 1, danger: 3, unknown: 0, na: 0 });
    expect((await api.getAgentSource()).agent_source.config.ref).toBe("main");
  });

  it.each([
    [{ agent_source_enabled: "false" }, "na", "Agent source disabled or unconfigured."],
    [{ agent_source_repo_url: "" }, "na", "Agent source disabled or unconfigured."],
    [{ agent_source_ref: "a".repeat(40) }, "na", "Agent source is pinned to an immutable SHA."],
    [{}, "unknown", "Agent source not checked yet."],
  ] as const)("matches M1 applicability and unchecked summaries for %j", async (patch, severity, summary) => {
    const api = await fresh("unchecked-source");
    await api.updateSettings(patch);
    expect((await api.getAdminHealth()).checks.find((c) => c.id === "agentsource.update"))
      .toMatchObject({ severity, summary });
  });

  it.each(["", "codex-only", "oidc", "run-progress", "judge-admin"])("passing facts in %s do not clobber edits or an explicit update check before Health reads", async (scenario) => {
    const api = await fresh(scenario);
    await api.updateSettings({ agent_source_ref: "v1.3.0" });
    const checked = (await api.updateCheckAgentSource()).agent_source;
    window.history.replaceState({}, "", "/?mock=health-silent");
    const doc = await api.getAdminHealth();
    expect(doc.checks.find((c) => c.id === "agentsource.update"))
      .toMatchObject({ severity: "warn", summary: "Agent source newer tag v1.10.0 is available." });
    const read = (await api.getAgentSource()).agent_source;
    expect(read.config.ref).toBe("v1.3.0");
    expect(read.status.update_checked_at).toBe(checked.status.update_checked_at);
    await api.updateSettings({ agent_source_ref: "v1.10.0" });
    expect((await api.getAdminHealth()).counts.warn).toBe(0);
    expect((await api.getAgentSource()).agent_source.config.ref).toBe("v1.10.0");
  });

  it("reads the scenario from localStorage when the query is absent", async () => {
    vi.resetModules();
    window.history.replaceState({}, "", "/");
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      value: { getItem: (key: string) => key === "uzi_mock_scenario" ? "agent-source-update" : null, setItem: () => {}, removeItem: () => {} },
    });
    const { mockApi: api } = await import("./mockApi");
    await api.login("vlad@uzi.local", "x");
    expect((await api.getAgentSource()).agent_source.status.update_available).toBe(true);
    expect((await api.getAdminHealth()).counts.warn).toBe(1);
  });
});
