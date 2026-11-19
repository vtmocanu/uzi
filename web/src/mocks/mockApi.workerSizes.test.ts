// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

// The mock admin worker sizes API (PRD #4213) and the one size list it shares with
// hostedConfig and provisionHostedWorker.
async function fresh() {
  vi.resetModules();
  const api = (await import("./mockApi")).mockApi;
  await api.login("vlad@uzi.local", "x");
  return api;
}
beforeEach(() => { localStorage.clear(); vi.resetModules(); });
afterEach(() => vi.resetModules());

describe("mock admin worker sizes", () => {
  it("serves the builtins, the 4 CPU / 32768 MiB / 50 GiB ceilings, editable, with a quota_unavailable advisory", async () => {
    const api = await fresh();
    const got = await api.getAdminWorkerSizes();
    expect(got.sizes.map((s) => [s.name, s.builtin, s.customized])).toEqual([
      ["s", true, false], ["m", true, false], ["l", true, false], ["l-highmem", true, false],
    ]);
    expect(got.ceilings.cpu_limit_millicores?.value).toBe(4000);
    expect(got.ceilings.memory_limit_mib?.value).toBe(32768);
    expect(got.ceilings.data_gib?.value).toBe(50);
    expect(got.editable).toBe(true);
    expect(got.advisories.map((a) => a.kind)).toEqual(["quota_unavailable"]);
  });

  it("an edit reaches the provision config and new workers; a reset restores the shipped values", async () => {
    const api = await fresh();
    const l = (await api.getAdminWorkerSizes()).sizes.find((s) => s.name === "l")!;
    const saved = await api.updateAdminWorkerSize("l", { ...l, memory_limit_mib: 24576 });
    expect(saved.size).toMatchObject({ name: "l", memory_limit_mib: 24576, customized: true });
    expect((await api.hostedConfig()).sizes?.find((s) => s.name === "l")?.memory_limit_mib).toBe(24576);
    const { worker } = await api.provisionHostedWorker("base", "l");
    expect(worker.hosted_size_spec?.memory_limit_mib).toBe(24576);

    const reset = await api.resetAdminWorkerSize("l");
    expect(reset.size).toMatchObject({ memory_limit_mib: 20480, customized: false });
    expect((await api.hostedConfig()).sizes?.find((s) => s.name === "l")?.memory_limit_mib).toBe(20480);
  });

  it("refuses an over-ceiling value with field errors and warns on a lowered disk", async () => {
    const api = await fresh();
    const m = (await api.getAdminWorkerSizes()).sizes.find((s) => s.name === "m")!;
    await expect(api.updateAdminWorkerSize("m", { ...m, memory_limit_mib: 40960 })).rejects.toMatchObject({
      status: 400,
      body: { fields: { memory_limit_mib: "memory limit 40960 MiB exceeds the 32768 MiB ceiling (workers.limitRange.max.memory)" } },
    });
    const lowered = await api.updateAdminWorkerSize("m", { ...m, data_gib: 20 });
    expect(lowered.warnings.map((w) => [w.kind, w.field])).toEqual([["disk_lowered", "data_gib"]]);
  });

  it("spells every field error and warning exactly as the api's workersize.Validate does", async () => {
    const api = await fresh();
    const l = (await api.getAdminWorkerSizes()).sizes.find((s) => s.name === "l")!;
    const fieldsOf = async (spec: Partial<typeof l>) => {
      try {
        await api.updateAdminWorkerSize("l", { ...l, ...spec });
      } catch (err) {
        return (err as { body: { fields: Record<string, string> } }).body.fields;
      }
      throw new Error("expected a refusal");
    };
    expect(await fieldsOf({ cpu_request_millicores: 3000, cpu_limit_millicores: 2000 })).toEqual({
      cpu_request_millicores: "cpu request 3000 millicores exceeds the cpu limit of 2000 millicores",
    });
    expect(await fieldsOf({ memory_request_mib: 30000, memory_limit_mib: 20000 })).toEqual({
      memory_request_mib: "memory request 30000 MiB exceeds the memory limit of 20000 MiB",
    });
    expect(await fieldsOf({ cpu_limit_millicores: 8000, dind_data_gib: 60 })).toEqual({
      cpu_limit_millicores: "cpu limit 8000 millicores exceeds the 4000 millicores ceiling (workers.limitRange.max.cpu)",
      dind_data_gib: "dind data disk 60 GiB exceeds the 50 GiB ceiling (workers.docker.limitRange.maxPVCStorage)",
    });
    expect(await fieldsOf({ data_gib: 0, memory_limit_mib: 600000 })).toEqual({
      data_gib: "must be greater than 0 GiB",
      memory_limit_mib: "600000 MiB exceeds the absolute maximum of 524288 MiB",
    });
    const lowered = await api.updateAdminWorkerSize("l", { ...l, dind_data_gib: 30 });
    expect(lowered.warnings).toEqual([
      { kind: "disk_lowered", field: "dind_data_gib", message: "dind data disk lowered from 50 GiB to 30 GiB: existing claims keep their size; only new claims use the lower value" },
    ]);
  });

  it("?mock=sizes-readonly serves a read-only list and refuses every write with the api's 409", async () => {
    localStorage.setItem("uzi_mock_scenario", "sizes-readonly");
    const api = await fresh();
    const got = await api.getAdminWorkerSizes();
    const reason = "The controller does not support admin worker sizes yet (upgrade it); edits would be ignored.";
    expect(got).toMatchObject({ editable: false, read_only_code: "controller_unsupported", read_only_reason: reason });
    const l = got.sizes.find((s) => s.name === "l")!;
    const refusal = { status: 409, body: { error: `worker sizes are read-only: ${reason}`, reason: "controller_unsupported", read_only_reason: reason } };
    await expect(api.updateAdminWorkerSize("l", { ...l, data_gib: 40 })).rejects.toMatchObject(refusal);
    await expect(api.resetAdminWorkerSize("l")).rejects.toMatchObject(refusal);
  });

  it("rejects provisioning a size the list does not have", async () => {
    const api = await fresh();
    await expect(api.provisionHostedWorker("base", "xl")).rejects.toMatchObject({ status: 400, message: "unknown worker size" });
  });

  it("keeps default-size settings builtins-only, with the server's error text", async () => {
    const api = await fresh();
    await expect(api.updateSettings({ hosted_default_size: "xl" })).rejects.toMatchObject({
      status: 400,
      message: "hosted_default_size: must be one of: s, m, l, l-highmem",
    });
  });
});
