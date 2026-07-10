import { afterEach, describe, it, expect, vi } from "vitest";

// Each test gets a fresh mockApi module so the in-memory allocation state
// (global defaults + per-user overlays) starts from seed.
async function fresh() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

const ADMIN = "vlad@uzi.local";
const USER = "mira@uzi.local"; // seeded non-admin persona

const draft = (name: string, scope: "global" | "user") => ({
  name,
  description: "does a thing.",
  model: null,
  tools: null,
  prompt_body: "You do the thing.",
  scope,
});

describe("mockApi template allocations (PRD #18 M7)", () => {
  afterEach(() => vi.resetModules());

  it("seeds every builtin/global as a global default (no empty-means-all cliff)", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const { templates } = await api.getTemplateAllocations();
    const shared = templates.filter((t) => t.scope !== "user");
    expect(shared.length).toBeGreaterThan(0);
    for (const t of shared) {
      expect(t.global_default).toBe(true);
      expect(t.my_override).toBeNull();
      expect(t.effective).toBe(true); // no overlay ⇒ follows the default
    }
  });

  it("a user overlay wins over the global default without changing it", async () => {
    const api = await fresh();
    await api.login(ADMIN, "x");
    const first = (await api.getTemplateAllocations()).templates[0];
    await api.setTemplateAllocations({ my_overrides: [{ template_id: first.id, enabled: false }] });
    const after = (await api.getTemplateAllocations()).templates.find((t) => t.id === first.id)!;
    expect(after.my_override).toBe(false);
    expect(after.effective).toBe(false); // overlay off beats default on
    expect(after.global_default).toBe(true); // the shared default is untouched
  });

  it("rejects a reserved lead name and requires admin for global scope", async () => {
    const api = await fresh();
    await api.login(USER, "x");
    await expect(api.createAgentTemplate(draft("orchestrator", "user"))).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.createAgentTemplate(draft("my-helper", "global"))).rejects.toMatchObject({
      status: 403,
    });
  });

  it("a private template is scope=user, no global default, delivered only via the owner overlay", async () => {
    const api = await fresh();
    await api.login(USER, "x");
    const { template } = await api.createAgentTemplate(draft("mira-helper", "user"));
    expect(template.scope).toBe("user");

    const before = (await api.getTemplateAllocations()).templates.find((t) => t.id === template.id)!;
    expect(before.global_default).toBe(false);
    expect(before.effective).toBe(false); // no overlay yet ⇒ not delivered

    await api.setTemplateAllocations({ my_overrides: [{ template_id: template.id, enabled: true }] });
    const after = (await api.getTemplateAllocations()).templates.find((t) => t.id === template.id)!;
    expect(after.effective).toBe(true);
  });

  it("a non-admin never sees another user's private template in the allocation view", async () => {
    const api = await fresh();
    await api.login(USER, "x");
    const { template } = await api.createAgentTemplate(draft("mira-secret", "user"));
    await api.login(ADMIN, "x"); // switch persona
    const adminView = (await api.getTemplateAllocations()).templates;
    // The admin can see it (admins see all); a different non-admin cannot. Assert
    // the visibility filter by logging in as a second non-admin.
    expect(adminView.some((t) => t.id === template.id)).toBe(true);
    await api.login("andrei@uzi.local", "x");
    const otherView = (await api.getTemplateAllocations()).templates;
    expect(otherView.some((t) => t.id === template.id)).toBe(false);
  });
});
