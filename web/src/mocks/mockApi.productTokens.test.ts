// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";

// PRD #1907: the mock is the browsable spec, so it re-implements the server's product
// gates: termsafe-clean one-line names and descriptions, case-insensitive name
// uniqueness among live products, and the list caps (active first, then newest).
// Each test reloads the module so the in-memory registry starts from its seed.

async function reload() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => vi.resetModules());

describe("mockApi — product registry validation (PRD #1907)", () => {
  it("rejects a description with a newline or tab, as the server's termsafe gate does", async () => {
    const api = await reload();
    await expect(api.adminCreateProduct("CRM sync", "line one\nline two", [])).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.adminCreateProduct("CRM sync", "a\tb", [])).rejects.toMatchObject({ status: 400 });
    // Control: the same product with a clean one-line description is accepted.
    await expect(api.adminCreateProduct("CRM sync", "Syncs tickets.", [])).resolves.toMatchObject({
      product: { name: "CRM sync", description: "Syncs tickets." },
    });
  });

  it("rejects an invisible formatting character in a product name and in a description update", async () => {
    const api = await reload();
    await expect(api.adminCreateProduct("CRM\u202Esync", "", [])).rejects.toMatchObject({ status: 400 });
    await expect(
      api.adminUpdateProduct("prod-helpdesk", { description: "zero\u200Bwidth" }),
    ).rejects.toMatchObject({ status: 400 });
  });

  it("rejects a control character in a product-token name", async () => {
    const api = await reload();
    await expect(
      api.createProductToken({ product_id: "prod-helpdesk", name: "a\nb", scopes: ["jobs:read"], expiry: "90d" }),
    ).rejects.toMatchObject({ status: 400 });
  });

  it("treats product names case-insensitively for uniqueness, like the server's index", async () => {
    const api = await reload();
    await expect(api.adminCreateProduct("helpdesk ASSISTANT", "", [])).rejects.toMatchObject({
      status: 409,
    });
  });
});

describe("mockApi — product allowed_job_types (PRD #1908)", () => {
  it("stores the create list, de-duplicated, and refuses an unknown type", async () => {
    const api = await reload();
    await expect(api.adminCreateProduct("CRM sync", "", ["research", "research"])).resolves.toMatchObject({
      product: { allowed_job_types: ["research"] },
    });
    await expect(api.adminCreateProduct("Other", "", ["mining"])).rejects.toMatchObject({ status: 400 });
  });

  it("PATCH keeps the list when the field is omitted and clears it on []", async () => {
    const api = await reload();
    const kept = await api.adminUpdateProduct("prod-helpdesk", { description: "Still helps." });
    expect(kept.product.allowed_job_types).toEqual(["research"]);
    const cleared = await api.adminUpdateProduct("prod-helpdesk", { allowed_job_types: [] });
    expect(cleared.product.allowed_job_types).toEqual([]);
    const { products } = await api.adminListProducts();
    expect(products.find((p) => p.id === "prod-helpdesk")?.allowed_job_types).toEqual([]);
  });
});

describe("mockApi — product-token lists (PRD #1907)", () => {
  it("lists the caller's tokens active first, then newest, and says the list was not cut", async () => {
    const api = await reload();
    const { tokens, truncated } = await api.listProductTokens();
    expect(truncated).toBe(false);
    const active = (t: (typeof tokens)[number]) =>
      !t.revoked && (t.expires_at === null || Date.parse(t.expires_at) > Date.now());
    const firstInactive = tokens.findIndex((t) => !active(t));
    // The seed has both kinds, so the ordering claim is not vacuous.
    expect(firstInactive).toBeGreaterThan(0);
    expect(tokens.slice(firstInactive).every((t) => !active(t))).toBe(true);
    const activeRows = tokens.slice(0, firstInactive);
    const created = activeRows.map((t) => Date.parse(t.created_at));
    expect(created).toEqual([...created].sort((a, b) => b - a));
  });
});

describe("mockApi — product skill sets (PRD #1909 M6)", () => {
  it("syncs into a staged diff, applies only the named sha, and never returns a token", async () => {
    const api = await reload();
    const before = await api.adminGetProductSkills("prod-helpdesk");
    expect(before.staged).toBeNull();
    const synced = await api.adminSyncProductSkills("prod-helpdesk");
    const staged = synced.staged!;
    expect(staged.diff).toEqual({
      added: ["escalation-check"],
      changed: ["reply-tone"],
      removed: ["legacy-macros"],
      unchanged: ["ticket-triage"],
    });
    await expect(api.adminApplyProductSkills("prod-helpdesk", "f".repeat(40))).rejects.toMatchObject({ status: 409 });
    const applied = await api.adminApplyProductSkills("prod-helpdesk", staged.sha);
    expect(applied.applied.sha).toBe(staged.sha);
    expect(applied.staged).toBeNull();

    const token = "tok" + "en-value-1234";
    await api.adminUpdateProduct("prod-helpdesk", { skills_token: token });
    expect(JSON.stringify(await api.adminGetProductSkills("prod-helpdesk"))).not.toContain(token);
  });

  it("refuses a sync with no repo and drops the token when the repo moves host", async () => {
    const api = await reload();
    await expect(api.adminSyncProductSkills("prod-metrics")).rejects.toMatchObject({ status: 409 });
    await expect(
      api.adminUpdateProduct("prod-helpdesk", { skills_repo_url: "https://evil.example/x" }),
    ).rejects.toMatchObject({ status: 400 });
    await expect(
      api.adminUpdateProduct("prod-helpdesk", { skills_repo_url: "https://user:pw@github.com/x" }),
    ).rejects.toMatchObject({ status: 400 });
    // Same host: the token stays.
    await api.adminUpdateProduct("prod-helpdesk", { skills_repo_url: "https://github.com/acme/other" });
    expect((await api.adminGetProductSkills("prod-helpdesk")).config.skills_token_set).toBe(true);
  });
});

describe("mockApi — product site-list allowance (PRD #1976 M2)", () => {
  it("refuses a write on a deleted product with 409", async () => {
    const api = await reload();
    const { product } = await api.adminCreateProduct("Temp product", "", []);
    await api.adminAllowProductEgressProfile(product.id, "ml-model-cards");
    await api.adminDeleteProduct(product.id);
    await expect(api.adminAllowProductEgressProfile(product.id, "ml-model-cards")).rejects.toMatchObject({
      status: 409,
    });
    await expect(api.adminDisallowProductEgressProfile(product.id, "ml-model-cards")).rejects.toMatchObject({
      status: 409,
    });
    // The audit trail still lists what was allowed.
    await expect(api.adminListProductEgressProfiles(product.id)).resolves.toMatchObject({
      egress_profiles: [{ name: "ml-model-cards" }],
    });
  });

  it("answers 404 for an unknown list, with the real handler's message, on both verbs", async () => {
    const api = await reload();
    await expect(api.adminAllowProductEgressProfile("prod-helpdesk", "no-such-list")).rejects.toMatchObject({
      status: 404,
      message: "egress profile not found",
    });
    await expect(api.adminDisallowProductEgressProfile("prod-helpdesk", "no-such-list")).rejects.toMatchObject({
      status: 404,
      message: "egress profile not found",
    });
  });

  it("answers 404 when a known list is not allowed, and allowing twice is idempotent", async () => {
    const api = await reload();
    await expect(api.adminDisallowProductEgressProfile("prod-helpdesk", "vendor-x-docs")).rejects.toMatchObject({
      status: 404,
    });
    const first = await api.adminAllowProductEgressProfile("prod-helpdesk", "vendor-x-docs");
    const second = await api.adminAllowProductEgressProfile("prod-helpdesk", "vendor-x-docs");
    expect(second).toEqual(first);
    expect(first.egress_profiles.filter((p) => p.name === "vendor-x-docs")).toHaveLength(1);
  });
});

describe("mockApi — OAuth connections and Revoke all (PRD #1910 M3)", () => {
  it("lists the caller's live connection and Revoke all revokes it with the tokens", async () => {
    const api = await reload();
    const { connections } = await api.listOAuthConnections();
    expect(connections).toHaveLength(1);
    expect(connections[0]).toMatchObject({ product_id: "prod-helpdesk", scopes: ["jobs:run", "jobs:read"] });
    // The wire row carries no owner, token or hash.
    expect(Object.keys(connections[0]).sort()).toEqual(
      ["connected_at", "created_at", "id", "last_used_at", "product_id", "product_name", "refresh_issued_at", "scopes"],
    );
    await api.revokeAllCliTokens();
    expect((await api.listOAuthConnections()).connections).toEqual([]);
  });

  it("counts connections apart from manual tokens, and Revoke all and a delete move the counts like the server", async () => {
    const api = await reload();
    const before = (await api.adminListProducts()).products.find((p) => p.id === "prod-helpdesk")!;
    expect(before.live_connection_count).toBe(1);
    // active_token_count is the product's manual tokens only; the connection is counted apart.
    const deleted = await api.adminDeleteProduct("prod-helpdesk");
    expect(deleted.stopped_connection_count).toBe(1);
    expect(deleted.stopped_token_count).toBe(before.active_token_count);
    expect(deleted.product.live_connection_count).toBe(1);
    await api.revokeAllCliTokens();
    const after = (await api.adminListProducts()).products.find((p) => p.id === "prod-helpdesk")!;
    expect(after.live_connection_count).toBe(0);
  });
});

describe("mockApi — connection lists and revoke (PRD #1910 M5)", () => {
  it("revoking a connection removes it from the owner's list and the admin's, the product count follows, and a second revoke is a 404", async () => {
    const api = await reload();
    const { connections } = await api.listOAuthConnections();
    const id = connections[0].id;
    const admin = await api.adminListProductConnections("prod-helpdesk");
    expect(admin.truncated).toBe(false);
    expect(admin.connections.map((c) => c.id)).toEqual([id]);
    expect(admin.connections[0].owner_email).not.toBe("");
    // The admin row carries the user and no token or hash.
    expect(Object.keys(admin.connections[0]).sort()).toEqual(
      ["connected_at", "created_at", "id", "last_used_at", "owner_email", "scopes", "user_id"],
    );

    await api.revokeOAuthConnection(id);
    expect((await api.listOAuthConnections()).connections).toEqual([]);
    expect((await api.adminListProductConnections("prod-helpdesk")).connections).toEqual([]);
    const product = (await api.adminListProducts()).products.find((p) => p.id === "prod-helpdesk")!;
    expect(product.live_connection_count).toBe(0);
    await expect(api.revokeOAuthConnection(id)).rejects.toMatchObject({ status: 404 });
    await expect(api.adminRevokeOAuthConnection(id)).rejects.toMatchObject({ status: 404 });
  });

  it("lets an admin revoke a connection by id", async () => {
    const api = await reload();
    const id = (await api.adminListProductConnections("prod-helpdesk")).connections[0].id;
    await api.adminRevokeOAuthConnection(id);
    expect((await api.listOAuthConnections()).connections).toEqual([]);
  });
});

describe("mockApi — admin token inventory filters (issue #1935)", () => {
  it("lists a matching token that the unfiltered cap cuts, once owner and product filters apply", async () => {
    vi.resetModules();
    vi.doMock("./data", async (importOriginal) => {
      const actual = await importOriginal<typeof import("./data")>();
      const base = actual.mockProductTokens[0];
      // 1001 newer active tokens from other owners push the revoked target past the 1000 cap.
      const filler = Array.from({ length: 1001 }, (_, i) => ({
        ...base,
        id: `filler-${i}`,
        user_id: "u-filler",
        name: `filler-${i}`,
        revoked: false,
        expires_at: null,
        created_at: new Date(Date.now() + 60_000 + i).toISOString(),
      }));
      const target = {
        ...base,
        id: "target-old",
        name: "target-old",
        revoked: true,
        created_at: new Date(Date.now() - 400 * 86_400_000).toISOString(),
      };
      return { ...actual, mockProductTokens: [...actual.mockProductTokens, ...filler, target] };
    });
    try {
      const api = (await import("./mockApi")).mockApi;
      const unfiltered = await api.adminListProductTokens();
      expect(unfiltered.truncated).toBe(true);
      expect(unfiltered.tokens).toHaveLength(1000);
      expect(unfiltered.tokens.some((t) => t.id === "target-old")).toBe(false);

      const base = (await import("./data")).mockProductTokens[0];
      const filtered = await api.adminListProductTokens({ ownerId: base.user_id, productId: base.product_id });
      expect(filtered.truncated).toBe(false);
      expect(filtered.tokens.some((t) => t.id === "target-old")).toBe(true);
      expect(filtered.tokens.every((t) => t.user_id === base.user_id && t.product_id === base.product_id)).toBe(true);
    } finally {
      vi.doUnmock("./data");
    }
  });
});
