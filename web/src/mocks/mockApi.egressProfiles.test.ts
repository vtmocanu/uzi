// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";

// The demo backend for Admin → Site lists (PRD #1906 M1w) must answer with the same 422
// envelope and codes the page branches on, and the fetch-cap settings must keep the
// server's bounds. Each test re-imports a fresh mockApi so writes do not bleed across tests;
// the session starts signed in as admin. Rejections are asserted on fields (toMatchObject),
// never instanceof, because resetModules gives each import its own ApiError class.
async function freshApi() {
  vi.resetModules();
  return (await import("./mockApi")).mockApi;
}

afterEach(() => vi.resetModules());

describe("mockApi egress profiles", () => {
  it("seeds lists that exercise every warning the page renders", async () => {
    const api = await freshApi();
    const { egress_profiles } = await api.adminListEgressProfiles();
    const codes = egress_profiles.flatMap((p) => p.warnings.map((w) => w.code));
    expect(codes).toContain("multi_publisher_override");
    expect(codes).toContain("multi_publisher_needs_override");
    expect(egress_profiles.some((p) => p.warnings.length === 0)).toBe(true);
  });

  it("refuses a multi-publisher host without an override with a 422 problem on its index", async () => {
    const api = await freshApi();
    await expect(
      api.adminCreateEgressProfile({
        name: "kernel",
        description: "",
        hosts: ["docs.kernel.org", "GitHub.com"],
        multi_publisher_override: [],
      }),
    ).rejects.toMatchObject({
      status: 422,
      body: {
        reason: "invalid_egress_profile",
        problems: [{ field: "hosts[1]", entry: "GitHub.com", code: "multi_publisher_needs_override" }],
      },
    });
  });

  it("stores the entry with a warning once the override names it, and then deletes it", async () => {
    const api = await freshApi();
    const { egress_profile } = await api.adminCreateEgressProfile({
      name: "kernel",
      description: "Kernel sources",
      hosts: ["docs.kernel.org", "github.com."],
      multi_publisher_override: ["github.com"],
    });
    expect(egress_profile.hosts).toEqual(["docs.kernel.org", "github.com"]);
    expect(egress_profile.multi_publisher_override).toEqual(["github.com"]);
    expect(egress_profile.warnings.map((w) => w.code)).toEqual(["multi_publisher_override"]);

    await api.adminDeleteEgressProfile("kernel");
    const { egress_profiles } = await api.adminListEgressProfiles();
    expect(egress_profiles.map((p) => p.name)).not.toContain("kernel");
    expect(egress_profiles.length).toBeGreaterThan(0);
  });

  it("reports every refused entry at once and refuses an override naming no host", async () => {
    const api = await freshApi();
    await expect(
      api.adminCreateEgressProfile({
        name: "Bad Name",
        description: "",
        hosts: ["https://a.example.com", "*", "10.0.0.1"],
        multi_publisher_override: ["gitlab.com"],
      }),
    ).rejects.toMatchObject({
      status: 422,
      body: {
        problems: [
          { field: "name", code: "invalid_name" },
          { field: "hosts[0]", code: "scheme" },
          { field: "hosts[1]", code: "bare_wildcard" },
          { field: "hosts[2]", code: "ip_address" },
          { field: "multi_publisher_override[0]", code: "override_not_in_hosts" },
        ],
      },
    });
  });

  it("keeps the name immutable on update and 404s an unknown name", async () => {
    const api = await freshApi();
    const { egress_profile } = await api.adminUpdateEgressProfile("vendor-x-docs", {
      description: "Updated",
      hosts: ["docs.vendor-x.com"],
      multi_publisher_override: [],
    });
    expect(egress_profile.name).toBe("vendor-x-docs");
    expect(egress_profile.hosts).toEqual(["docs.vendor-x.com"]);
    await expect(
      api.adminUpdateEgressProfile("nope", { description: "", hosts: ["a.example.com"], multi_publisher_override: [] }),
    ).rejects.toMatchObject({ status: 404 });
  });
});

describe("mockApi fetch-cap settings keep the server's bounds", () => {
  it("accepts in-range values and refuses zero and over-ceiling ones", async () => {
    const api = await freshApi();
    const res = await api.updateSettings({ fetch_max_file_bytes: String(1 << 30), fetch_max_concurrent_per_run: "32" });
    expect(res.settings.fetch_max_file_bytes).toBe(String(1 << 30));
    expect(res.settings.fetch_max_concurrent_per_run).toBe("32");
    await expect(api.updateSettings({ fetch_max_run_files: "0" })).rejects.toMatchObject({ status: 400 });
    await expect(api.updateSettings({ fetch_max_file_bytes: String((1 << 30) + 1) })).rejects.toMatchObject({
      status: 400,
    });
    await expect(api.updateSettings({ fetch_max_concurrent_per_run: "33" })).rejects.toMatchObject({ status: 400 });
  });
});
