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
    await expect(api.adminCreateProduct("CRM sync", "line one\nline two")).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.adminCreateProduct("CRM sync", "a\tb")).rejects.toMatchObject({ status: 400 });
    // Control: the same product with a clean one-line description is accepted.
    await expect(api.adminCreateProduct("CRM sync", "Syncs tickets.")).resolves.toMatchObject({
      product: { name: "CRM sync", description: "Syncs tickets." },
    });
  });

  it("rejects an invisible formatting character in a product name and in a description update", async () => {
    const api = await reload();
    await expect(api.adminCreateProduct("CRM‮sync", "")).rejects.toMatchObject({ status: 400 });
    await expect(
      api.adminUpdateProduct("prod-helpdesk", { description: "zero​width" }),
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
    await expect(api.adminCreateProduct("helpdesk ASSISTANT", "")).rejects.toMatchObject({
      status: 409,
    });
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
