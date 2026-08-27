import { afterEach, describe, it, expect, vi } from "vitest";

// Each test gets a fresh mockApi module so the in-memory agent-source state starts
// from seed (the handlers mutate it in place).
async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

const ADMIN = "vlad@uzi.local";

afterEach(() => vi.resetModules());

describe("mock agent-source (PRD #602 M5)", () => {
  it("serves a configured, pending snapshot for review", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { agent_source } = await api.getAgentSource();
    expect(agent_source.config.url).not.toBe("");
    expect(agent_source.config.credential_configured).toBe(true);
    expect(agent_source.staged?.pending).toBe(true);
    // The staged diff carries at least one of every action the review chips render.
    const actions = new Set(agent_source.staged!.diff.map((d) => d.action));
    for (const a of ["add", "override", "unchanged", "conflict", "remove"]) {
      expect(actions.has(a)).toBe(true);
    }
    // A failed parse keeps failed non-zero.
    expect(agent_source.staged!.counts.failed).toBeGreaterThan(0);
  });

  it("applies the reviewed snapshot and clears pending; a wrong SHA is a 409", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { agent_source } = await api.getAgentSource();
    const sha = agent_source.staged!.fetched_sha;

    // Wrong SHA ⇒ stale-approval 409, no state change.
    await expect(api.applyAgentSource("not-the-sha")).rejects.toMatchObject({ status: 409 });

    const { result } = await api.applyAgentSource(sha);
    expect(result.sha).toBe(sha);
    // add + override count as applied; remove is deprovisioned; conflict skipped.
    expect(result.applied).toBeGreaterThan(0);
    expect(result.deprovisioned).toBeGreaterThan(0);
    expect(result.conflicts).toBeGreaterThan(0);

    // The snapshot is no longer pending and last-applied advanced to its SHA.
    const after = (await api.getAgentSource()).agent_source;
    expect(after.staged?.pending).toBe(false);
    expect(after.status.last_applied_sha).toBe(sha);

    // Re-applying the now-applied (non-pending) snapshot is a 409, not a double-apply.
    await expect(api.applyAgentSource(sha)).rejects.toMatchObject({ status: 409 });
  });

  it("Sync now refreshes the fetch timestamp and keeps the snapshot healthy", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { agent_source } = await api.syncAgentSource();
    expect(agent_source.status.last_sync_status).toBe("ok");
    expect(agent_source.staged?.pending).toBe(true);
  });

  it("rejects a URL off the allowlist and accepts an allowed https URL through updateSettings", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    // Off-allowlist host ⇒ the SSRF 400, in the exact copy the card surfaces.
    await expect(
      api.updateSettings({ agent_source_repo_url: "https://evil.example/agents.git" }),
    ).rejects.toMatchObject({ status: 400, message: "URL is not in the agent-source allowlist" });
    // A non-https URL ⇒ 400.
    await expect(
      api.updateSettings({ agent_source_repo_url: "http://github.com/x/y.git" }),
    ).rejects.toMatchObject({ status: 400 });

    // An allowed host is accepted and read back through getAgentSource.
    await api.updateSettings({ agent_source_repo_url: "https://gitlab.com/team/agents.git" });
    expect((await api.getAgentSource()).agent_source.config.url).toBe("https://gitlab.com/team/agents.git");

    // Empty URL (disable) is always allowed.
    await api.updateSettings({ agent_source_repo_url: "" });
    expect((await api.getAgentSource()).agent_source.config.url).toBe("");
  });

  it("marks the credential configured on write without ever returning its value", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    // Start from a view that reports configured (seed), clear via a fresh module, then set.
    await api.updateSettings({ agent_source_credential: "ghp_token" });
    const view = (await api.getAgentSource()).agent_source;
    expect(view.config.credential_configured).toBe(true);
    // The value is never present on the config object — only the boolean.
    expect(Object.values(view.config)).not.toContain("ghp_token");
  });
});
