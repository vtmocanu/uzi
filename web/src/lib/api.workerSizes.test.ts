import { afterEach, expect, it, vi } from "vitest";
import { api } from "./api";

afterEach(() => vi.unstubAllGlobals());

const spec = { cpu_request_millicores: 250, cpu_limit_millicores: 1000, memory_request_mib: 1024, memory_limit_mib: 2048, data_gib: 5, dind_data_gib: 10 };

function stub() {
  vi.stubGlobal("document", { cookie: "uzi_csrf=tok", hidden: false });
  const fetch = vi.fn(async (_url: string, _init?: RequestInit) => new Response("{}", { status: 200 }));
  vi.stubGlobal("fetch", fetch);
  return fetch;
}

it("createAdminWorkerSize is a create-only PUT: If-None-Match * with the CSRF token", async () => {
  const fetch = stub();
  await api.createAdminWorkerSize("xs", spec);
  const [url, init] = fetch.mock.calls[0];
  expect(url).toBe("/api/admin/worker-sizes/xs");
  expect(init?.method).toBe("PUT");
  expect(init?.headers).toEqual({ "Content-Type": "application/json", "If-None-Match": "*", "X-CSRF-Token": "tok" });
  expect(JSON.parse(init?.body as string)).toEqual(spec);
});

it("updateAdminWorkerSize sends no If-None-Match", async () => {
  const fetch = stub();
  await api.updateAdminWorkerSize("xs", spec);
  const [, init] = fetch.mock.calls[0];
  expect(init?.method).toBe("PUT");
  expect(init?.headers).toEqual({ "Content-Type": "application/json", "X-CSRF-Token": "tok" });
});
