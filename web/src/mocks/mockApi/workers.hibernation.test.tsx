// TSX keeps this test outside the production-only *.ts glob in mockApi.parity.test.ts.
import { beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("./shared", async (importOriginal) => ({
  ...await importOriginal<typeof import("./shared")>(),
  delay: <T,>(value: T) => Promise.resolve(value),
  mockScenario: vi.fn(() => ""),
}));

async function fresh() {
  vi.resetModules();
  const { workersApi } = await import("./workers");
  const { settingsApi } = await import("./settings");
  return { api: workersApi, settings: settingsApi };
}

beforeEach(() => vi.clearAllMocks());

describe("mock worker hibernation routes", () => {
  it("lists a hibernated and a waking worker in the admin fleet", async () => {
    const { api } = await fresh();
    const states = (await api.adminListWorkers()).workers.map((w) => w.hibernation_state);
    expect(states).toContain("hibernated");
    expect(states).toContain("waking");
  });

  it("refuses hibernate with the setting_off reason code while the switch is off", async () => {
    const { api } = await fresh();
    await expect(api.adminHibernateWorker("w-hib-idle")).rejects.toMatchObject({
      status: 409,
      body: { reason_code: "setting_off" },
    });
  });

  it("hibernates an idle persistent hosted worker once enabled, then wakes it", async () => {
    const { api, settings } = await fresh();
    await settings.updateSettings({ worker_hibernation_enabled: "true" });
    const res = await api.adminHibernateWorker("w-hib-idle");
    expect(res).toMatchObject({ worker_id: "w-hib-idle", state: "hibernated" });
    const after = (await api.adminListWorkers()).workers.find((w) => w.id === "w-hib-idle")!;
    expect(after.hibernation_state).toBe("hibernated");
    await expect(api.adminHibernateWorker("w-hib-idle")).rejects.toMatchObject({ body: { reason_code: "already_hibernating" } });
    expect(await api.adminWakeWorker("w-hib-idle")).toMatchObject({ state: "waking" });
    await expect(api.adminWakeWorker("w-laptop")).rejects.toMatchObject({ body: { reason_code: "not_hibernating" } });
  });

  it("refuses an external worker as not persistent hosted", async () => {
    const { api, settings } = await fresh();
    await settings.updateSettings({ worker_hibernation_enabled: "true" });
    await expect(api.adminHibernateWorker("w-laptop")).rejects.toMatchObject({ body: { reason_code: "not_persistent_hosted" } });
  });
});
