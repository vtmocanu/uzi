// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkerSizesCard } from "./WorkerSizesCard";
import { api, type AdminWorkerSize, type AdminWorkerSizes } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { BUILTIN_WORKER_SIZES } from "../../lib/workerSizes";

vi.mock("../../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../../lib/api")>();
  return { ...actual, api: { updateAdminWorkerSize: vi.fn(), resetAdminWorkerSize: vi.fn() } };
});

const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const sizes = (over: Record<string, Partial<AdminWorkerSize>> = {}, top: Partial<AdminWorkerSizes> = {}): AdminWorkerSizes => ({
  sizes: BUILTIN_WORKER_SIZES.map((s, i) => ({
    ...s,
    display_order: i,
    builtin: true,
    customized: false,
    shipped: { ...s },
    updated_at: "2026-11-01T09:00:00Z",
    ...over[s.name],
  })),
  ceilings: {
    cpu_limit_millicores: { value: 4000, chart_path: "workers.limitRange.max.cpu" },
    memory_limit_mib: { value: 32768, chart_path: "workers.limitRange.max.memory" },
    data_gib: { value: 50, chart_path: "workers.limitRange.maxPVCStorage" },
    dind_data_gib: null,
    dind_overridden: false,
    enabled_tiers: ["restricted"],
  },
  editable: true,
  read_only_reason: null,
  read_only_code: null,
  advisories: [],
  max_sizes: 16,
  ...top,
});

const writeOk = (row: Partial<AdminWorkerSize> = {}, warnings: { kind: string; field: string; message: string }[] = []) => ({
  size: { ...sizes().sizes[2], ...row },
  warnings,
  advisories: [],
});

function renderCard(data: AdminWorkerSizes | null = sizes(), onChanged = vi.fn().mockResolvedValue(undefined), loadError = "") {
  render(
    <MemoryRouter>
      <WorkerSizesCard data={data} loadError={loadError} onChanged={onChanged} />
    </MemoryRouter>,
  );
  return onChanged;
}

const rowOf = (name: string) => screen.getAllByText(name, { exact: true }).find((el) => el.closest("th[scope=row]"))!.closest("tr")!;
const input = (label: string) => screen.getByLabelText(label) as HTMLInputElement;

describe("WorkerSizesCard", () => {
  it("renders one row per size in human units, with the chart ceilings", () => {
    renderCard();
    const s = within(rowOf("S"));
    expect(s.getByText("0.25")).toBeTruthy(); // 250 millicores request
    expect(s.getByText("1")).toBeTruthy(); // 1000 millicores limit
    expect(s.getByText("2")).toBeTruthy(); // 2048 MiB request
    expect(s.getByText("4")).toBeTruthy(); // 4096 MiB limit
    expect(s.getByText("5")).toBeTruthy(); // data GiB
    expect(s.getByText("20")).toBeTruthy(); // DinD GiB
    expect(s.getByText("Builtin")).toBeTruthy();
    expect(s.queryByText("Customized")).toBeNull();
    expect(screen.getAllByRole("rowheader").map((h) => h.textContent)).toEqual(["SBuiltin", "MBuiltin", "LBuiltin", "L-HIGHMEMBuiltin"]);
    expect(
      screen.getByText(
        "Limits: up to 4 CPU / 32 GiB memory / 50 GiB disk (from chart values workers.limitRange.max.cpu, workers.limitRange.max.memory, workers.limitRange.maxPVCStorage).",
      ),
    ).toBeTruthy();
    expect(screen.getByRole("link", { name: "Admin health" }).getAttribute("href")).toBe("/admin/health");
  });

  it("saves the edited human units as the api's exact integers, then re-reads the list", async () => {
    mockApi.updateAdminWorkerSize.mockResolvedValue(writeOk({ memory_limit_mib: 24576, customized: true }));
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    // Every input names its size and field.
    expect(input("L CPU request (cores)").value).toBe("1");
    expect(input("L Memory limit (GiB)").value).toBe("20");
    fireEvent.change(input("L CPU request (cores)"), { target: { value: "1.5" } });
    fireEvent.change(input("L Memory request (GiB)"), { target: { value: "13.5" } });
    fireEvent.change(input("L Memory limit (GiB)"), { target: { value: "24" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    await waitFor(() =>
      expect(mockApi.updateAdminWorkerSize).toHaveBeenCalledWith("l", {
        cpu_request_millicores: 1500,
        cpu_limit_millicores: 4000,
        memory_request_mib: 13824,
        memory_limit_mib: 24576,
        data_gib: 50,
        dind_data_gib: 50,
      }),
    );
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(screen.getByText("Size L saved.")).toBeTruthy();
    expect(screen.queryByLabelText("L Memory limit (GiB)")).toBeNull();
  });

  it("refuses an obviously bad value locally and sends nothing", async () => {
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size M" }));
    fireEvent.change(input("M Memory limit (GiB)"), { target: { value: "0.1" } });
    fireEvent.change(input("M CPU request (cores)"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size M" }));
    expect(await screen.findByText("Must be a whole number of MiB (GiB × 1024)")).toBeTruthy();
    expect(screen.getByText("The request cannot be above the limit")).toBeTruthy();
    expect(input("M Memory limit (GiB)").getAttribute("aria-invalid")).toBe("true");
    expect(mockApi.updateAdminWorkerSize).not.toHaveBeenCalled();
  });

  it("maps a 400's field errors onto the matching inputs", async () => {
    mockApi.updateAdminWorkerSize.mockRejectedValue(
      new ApiError(400, "the size has values that cannot be saved", {
        error: "the size has values that cannot be saved",
        fields: { memory_limit_mib: "must be at most 32768 MiB (workers.limitRange.max.memory)" },
      }),
    );
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    fireEvent.change(input("L Memory limit (GiB)"), { target: { value: "40" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    const msg = await screen.findByText("must be at most 32768 MiB (workers.limitRange.max.memory)");
    const field = input("L Memory limit (GiB)");
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(field.getAttribute("aria-describedby")).toBe(msg.id);
    expect(input("L CPU limit (cores)").getAttribute("aria-invalid")).toBeNull();
    expect(screen.getByRole("alert").textContent).toBe("the size has values that cannot be saved");
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("warns before saving that a lowered disk keeps existing claims, then shows the server's warning", async () => {
    mockApi.updateAdminWorkerSize.mockResolvedValue(
      writeOk({ data_gib: 25 }, [{ kind: "disk_lowered", field: "data_gib", message: "data disk lowered from 50 GiB to 25 GiB: existing claims keep their size; only new claims use the lower value" }]),
    );
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    const note = "Existing workers keep their larger disk; only new workers get the smaller size.";
    expect(screen.queryByText(note)).toBeNull();
    fireEvent.change(input("L Data disk (GiB)"), { target: { value: "60" } });
    expect(screen.queryByText(note)).toBeNull(); // growing is not lowering
    fireEvent.change(input("L Data disk (GiB)"), { target: { value: "25" } });
    expect(screen.getByText(note)).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    expect(await screen.findByText(/data disk lowered from 50 GiB to 25 GiB/)).toBeTruthy();
  });

  it("resets a customized builtin to its shipped values, which it shows", async () => {
    mockApi.resetAdminWorkerSize.mockResolvedValue(writeOk());
    const onChanged = renderCard(sizes({ l: { memory_limit_mib: 24576, customized: true } }));
    const l = within(rowOf("L"));
    expect(l.getByText("Customized")).toBeTruthy();
    expect(l.getByText("Shipped: 1 / 4 CPU, 14 / 20 GiB memory, 50 GiB data, 50 GiB DinD")).toBeTruthy();
    // Only a customized builtin can be reset.
    expect(screen.queryByRole("button", { name: "Reset size M to shipped values" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Reset size L to shipped values" }));
    await waitFor(() => expect(mockApi.resetAdminWorkerSize).toHaveBeenCalledWith("l"));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(screen.getByText("Size L reset to shipped values.")).toBeTruthy();
  });

  it("is read-only with the api's reason when the controller cannot apply edits", () => {
    renderCard(
      sizes(
        { l: { customized: true, memory_limit_mib: 24576 } },
        {
          editable: false,
          read_only_code: "controller_unsupported",
          read_only_reason: "The controller does not support admin worker sizes yet (upgrade it); edits would be ignored.",
        },
      ),
    );
    expect(screen.getByRole("status").textContent).toBe(
      "Sizes are read-only. The controller does not support admin worker sizes yet (upgrade it); edits would be ignored.",
    );
    for (const name of ["S", "M", "L", "L-HIGHMEM"]) {
      expect((screen.getByRole("button", { name: `Edit size ${name}` }) as HTMLButtonElement).disabled).toBe(true);
    }
    expect((screen.getByRole("button", { name: "Reset size L to shipped values" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("switches to read-only when a save is refused with 409", async () => {
    const reason = "The controller report is stale: the controller has not reported recently, so an edit might not be applied.";
    mockApi.updateAdminWorkerSize.mockRejectedValue(
      new ApiError(409, `worker sizes are read-only: ${reason}`, { error: `worker sizes are read-only: ${reason}`, reason: "controller_report_stale", read_only_reason: reason }),
    );
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size S" }));
    fireEvent.change(input("S CPU limit (cores)"), { target: { value: "2" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size S" }));
    expect(await screen.findByText(`Sizes are read-only. ${reason}`)).toBeTruthy();
    expect(input("S CPU limit (cores)").disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save size S" }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Edit size M" }) as HTMLButtonElement).disabled).toBe(true);
    expect(onChanged).not.toHaveBeenCalled();
  });

  it("reads an unavailable quota as unavailable, never as fitting", () => {
    renderCard(
      sizes({}, {
        advisories: [
          { size: "l-highmem", tier: "restricted", kind: "quota_unavailable", field: "", message: "quota advisory unavailable for the restricted tier: its quota values (workers.quota.*) are not available to the api" },
        ],
      }),
    );
    const list = screen.getByRole("list");
    const item = within(list).getByRole("listitem");
    expect(within(item).getByText("Quota check unavailable")).toBeTruthy();
    expect(item.textContent).toContain("L-HIGHMEM, restricted tier");
    expect(item.textContent).not.toMatch(/\bfits\b|\bOK\b|within quota/i);
  });

  it("shows a zero quota and an exceeded quota as their own advisories", () => {
    renderCard(
      sizes({}, {
        advisories: [
          { size: "m", tier: "docker", kind: "quota_zero", field: "", message: "a quota value for the docker tier is 0 (workers.docker.quota.*): no worker can be admitted on this tier" },
          { size: "l", tier: "restricted", kind: "quota_exceeded", field: "cpu_request_millicores", message: "cpu request 1000 millicores x 8 deployments exceeds the restricted tier quota of 4000 millicores (workers.quota.requestsCPU)" },
        ],
      }),
    );
    const items = within(screen.getByRole("list")).getAllByRole("listitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "Quota is zeroM, docker tiera quota value for the docker tier is 0 (workers.docker.quota.*): no worker can be admitted on this tier",
      "Exceeds quotaL, restricted tiercpu request 1000 millicores x 8 deployments exceeds the restricted tier quota of 4000 millicores (workers.quota.requestsCPU)",
    ]);
  });

  it("offers a retry when the first read failed", () => {
    const onChanged = renderCard(null, vi.fn().mockResolvedValue(undefined), "Failed to load worker sizes");
    expect(screen.getByRole("alert").textContent).toBe("Failed to load worker sizes");
    fireEvent.click(screen.getByRole("button", { name: "Retry loading worker sizes" }));
    expect(onChanged).toHaveBeenCalledTimes(1);
  });
});
