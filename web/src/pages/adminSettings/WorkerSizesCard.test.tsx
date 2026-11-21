// @vitest-environment jsdom
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkerSizesCard } from "./WorkerSizesCard";
import { api, type AdminWorkerSize, type AdminWorkerSizes } from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { BUILTIN_WORKER_SIZES } from "../../lib/workerSizes";

vi.mock("../../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../../lib/api")>();
  return {
    ...actual,
    api: { updateAdminWorkerSize: vi.fn(), resetAdminWorkerSize: vi.fn(), createAdminWorkerSize: vi.fn(), deleteAdminWorkerSize: vi.fn() },
  };
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
    expect(screen.getAllByRole("rowheader").map((h) => h.textContent)).toEqual(["SBuiltinEdit", "MBuiltinEdit", "LBuiltinEdit", "L-HIGHMEMBuiltinEdit"]);
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

  it("checks each limit against the chart ceilings and the absolute caps locally, in the editor's units", async () => {
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    fireEvent.change(input("L CPU limit (cores)"), { target: { value: "8" } });
    fireEvent.change(input("L Memory limit (GiB)"), { target: { value: "40" } });
    fireEvent.change(input("L Data disk (GiB)"), { target: { value: "5000" } });
    // No DinD ceiling applies in this list, so only the absolute cap binds it.
    fireEvent.change(input("L DinD data disk (GiB)"), { target: { value: "3000" } });
    fireEvent.change(input("L CPU request (cores)"), { target: { value: "100" } });
    fireEvent.change(input("L Memory request (GiB)"), { target: { value: "1.0000000001" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    expect(await screen.findByText("At most 4 cores (workers.limitRange.max.cpu)")).toBeTruthy();
    expect(screen.getByText("At most 32 GiB (workers.limitRange.max.memory)")).toBeTruthy();
    expect(screen.getByText("At most 50 GiB (workers.limitRange.maxPVCStorage)")).toBeTruthy();
    expect(screen.getByText("At most 2048 GiB (the api's absolute maximum)")).toBeTruthy();
    expect(screen.getByText("At most 64 cores (the api's absolute maximum)")).toBeTruthy();
    expect(screen.getByText("Must be a whole number of MiB (GiB × 1024)")).toBeTruthy();
    expect(mockApi.updateAdminWorkerSize).not.toHaveBeenCalled();
  });

  it("maps a 400's field errors onto the matching inputs and asks to fix them", async () => {
    const serverText = "memory limit 30720 MiB exceeds the 28672 MiB ceiling (workers.docker.limitRange.max.memory)";
    mockApi.updateAdminWorkerSize.mockRejectedValue(
      new ApiError(400, "the size has values that cannot be saved", {
        error: "the size has values that cannot be saved",
        fields: { memory_limit_mib: serverText },
      }),
    );
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    fireEvent.change(input("L Memory limit (GiB)"), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    const msg = await screen.findByText(serverText);
    const field = input("L Memory limit (GiB)");
    expect(field.getAttribute("aria-invalid")).toBe("true");
    expect(field.getAttribute("aria-describedby")).toBe(msg.id);
    expect(input("L CPU limit (cores)").getAttribute("aria-invalid")).toBeNull();
    expect(screen.getByRole("alert").textContent).toBe("Fix the highlighted fields and save again.");
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

  it("resets a customized builtin to its shipped values after a confirm, then returns focus to Edit", async () => {
    mockApi.resetAdminWorkerSize.mockResolvedValue(writeOk());
    const onChanged = renderCard(sizes({ l: { memory_limit_mib: 24576, customized: true } }));
    const l = within(rowOf("L"));
    expect(l.getByText("Customized")).toBeTruthy();
    expect(l.getByText("Shipped: 1 / 4 CPU, 14 / 20 GiB memory, 50 GiB data, 50 GiB DinD")).toBeTruthy();
    // Only a customized builtin can be reset.
    expect(screen.queryByRole("button", { name: "Reset size M to shipped values" })).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "Reset size L to shipped values" }));
    // Nothing is sent until the inline confirm; no disk lowers, so the prompt says only that.
    const confirm = within(screen.getByRole("group", { name: "Confirm reset of size L" }));
    expect(confirm.getByText("Reset L to shipped values?")).toBeTruthy();
    expect(document.activeElement).toBe(confirm.getByRole("button", { name: "Confirm" }));
    expect(mockApi.resetAdminWorkerSize).not.toHaveBeenCalled();
    fireEvent.click(confirm.getByRole("button", { name: "Confirm" }));
    await waitFor(() => expect(mockApi.resetAdminWorkerSize).toHaveBeenCalledWith("l"));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    expect(screen.getByText("Size L reset to shipped values.")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit size L" })));
  });

  it("says before resetting that a reset lowering a disk keeps existing workers' larger disk; Keep sends nothing", () => {
    renderCard(sizes({ l: { data_gib: 80, customized: true } }));
    fireEvent.click(screen.getByRole("button", { name: "Reset size L to shipped values" }));
    const group = screen.getByRole("group", { name: "Confirm reset of size L" });
    expect(group.textContent).toContain(
      "Reset L to shipped values? Existing workers keep their larger disk; only new workers get the smaller size.",
    );
    fireEvent.click(within(group).getByRole("button", { name: "Keep" }));
    expect(screen.queryByRole("group", { name: "Confirm reset of size L" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit size L" }));
    // Escape is Keep too.
    fireEvent.click(screen.getByRole("button", { name: "Reset size L to shipped values" }));
    fireEvent.keyDown(screen.getByRole("button", { name: "Confirm" }), { key: "Escape" });
    expect(screen.queryByRole("group", { name: "Confirm reset of size L" })).toBeNull();
    expect(mockApi.resetAdminWorkerSize).not.toHaveBeenCalled();
  });

  it("moves focus into the form on Edit, and back to that row's Edit on Escape, Cancel and Save", async () => {
    mockApi.updateAdminWorkerSize.mockResolvedValue(writeOk());
    // A re-read that takes a moment, as a real one does: Edit is disabled until it lands.
    renderCard(sizes(), vi.fn(() => new Promise<void>((r) => setTimeout(r, 30))));
    const editL = () => screen.getByRole("button", { name: "Edit size L" });
    fireEvent.click(editL());
    expect(document.activeElement).toBe(input("L CPU request (cores)"));
    fireEvent.keyDown(input("L Memory limit (GiB)"), { key: "Escape" });
    expect(screen.queryByRole("form", { name: "Edit size L" })).toBeNull();
    expect(document.activeElement).toBe(editL());

    fireEvent.click(editL());
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(document.activeElement).toBe(editL());

    fireEvent.click(editL());
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    await waitFor(() => expect(screen.getByText("Size L saved.")).toBeTruthy());
    await waitFor(() => expect(document.activeElement).toBe(editL()));
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
    // One muted label instead of a disabled Edit per row; the alert says why.
    expect(screen.getByText("Read-only")).toBeTruthy();
    expect(screen.queryByRole("button", { name: /^Edit size / })).toBeNull();
    expect(screen.queryByRole("button", { name: "Reset size L to shipped values" })).toBeNull();
  });

  it("switches to read-only when a save is refused with 409", async () => {
    const reason = "The controller report is stale: the controller has not reported recently, so an edit might not be applied.";
    mockApi.updateAdminWorkerSize.mockRejectedValue(
      new ApiError(409, `worker sizes are read-only: ${reason}`, { error: `worker sizes are read-only: ${reason}`, reason: "controller_report_stale", read_only_reason: reason }),
    );
    const onChanged = vi.fn().mockResolvedValue(undefined);
    const card = (data: AdminWorkerSizes) => (
      <MemoryRouter>
        <WorkerSizesCard data={data} loadError="" onChanged={onChanged} />
      </MemoryRouter>
    );
    const { rerender } = render(card(sizes()));
    fireEvent.click(screen.getByRole("button", { name: "Edit size S" }));
    fireEvent.change(input("S CPU limit (cores)"), { target: { value: "2" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size S" }));
    expect(await screen.findByText(`Sizes are read-only. ${reason}`)).toBeTruthy();
    expect(input("S CPU limit (cores)").disabled).toBe(true);
    expect((screen.getByRole("button", { name: "Save size S" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole("button", { name: "Edit size M" })).toBeNull();
    // The 409 asks the page to re-read the list, which is then the authority.
    expect(onChanged).toHaveBeenCalledTimes(1);

    // The re-read says editable again (the controller reported meanwhile): the card follows it.
    rerender(card(sizes()));
    expect(screen.queryByText(`Sizes are read-only. ${reason}`)).toBeNull();
    expect(input("S CPU limit (cores)").disabled).toBe(false);
    expect((screen.getByRole("button", { name: "Edit size M" }) as HTMLButtonElement).disabled).toBe(false);
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

  it("shows a stored size the controller refused, with its reason and an Admin health link, in its row", () => {
    const reason = "cpu limit 4000 millicores exceeds the controller's 3000 millicores ceiling";
    renderCard(
      sizes({ m: { customized: true } }, {
        refusals: [
          { size: "m", worker_id: "w-1", worker_name: "base.m-9f3c", kind: "size_refused", reason, reported_at: "2026-11-18T14:20:00Z" },
        ],
      }),
    );
    const m = within(rowOf("M"));
    expect(m.getByText("Refused")).toBeTruthy();
    const item = within(m.getByRole("list", { name: "Workers refused at size M" })).getByRole("listitem");
    expect(item.textContent).toContain("base.m-9f3c");
    expect(item.textContent).toContain("controller refused this size");
    expect(item.textContent).toContain(reason);
    const link = within(item).getByRole("link", { name: "base.m-9f3c in Admin health" });
    expect(link.getAttribute("href")).toBe("/admin/health");
    // Only the refused row says so; the generic Admin health link stays.
    expect(within(rowOf("S")).queryByText("Refused")).toBeNull();
    expect(screen.getByRole("link", { name: "Admin health" }).getAttribute("href")).toBe("/admin/health");
  });

  it("names a rejected volume claim in plain words and bounds a long list at three workers", () => {
    const r = (n: number, kind: "size_refused" | "pvc_rejected" = "size_refused") => ({
      size: "l", worker_id: `w-${n}`, worker_name: `base.l-${n}`, kind, reason: `reason ${n}`, reported_at: "2026-11-18T14:20:00Z",
    });
    renderCard(sizes({}, { refusals: [r(1, "pvc_rejected"), r(2), r(3), r(4), r(5)] }));
    const list = within(rowOf("L")).getByRole("list", { name: "Workers refused at size L" });
    const items = within(list).getAllByRole("listitem");
    expect(items).toHaveLength(4);
    expect(items[0].textContent).toContain("cluster rejected a volume claim");
    expect(list.textContent).not.toContain("base.l-4");
    expect(within(items[3]).getByRole("link", { name: "and 2 more workers in Admin health" }).getAttribute("href")).toBe("/admin/health");
  });

  describe("refusal totals", () => {
    const r = (n: number) => ({
      size: "l", worker_id: `w-${n}`, worker_name: `base.l-${n}`, kind: "size_refused" as const, reason: `reason ${n}`, reported_at: "2026-11-18T14:20:00Z",
    });
    const items = () => within(within(rowOf("L")).getByRole("list", { name: "Workers refused at size L" })).getAllByRole("listitem");

    it("counts the remainder from the api's total when it is past the three details", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3)], refusal_totals: { l: 7 } }));
      const list = items();
      expect(list).toHaveLength(4);
      expect(within(list[3]).getByRole("link", { name: "and 4 more workers in Admin health" })).toBeTruthy();
    });

    // An older api sends details without totals: the remainder is what was sent.
    it("falls back to the details count when the api sent no totals", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3), r(4), r(5)] }));
      expect(within(items()[3]).getByRole("link", { name: "and 2 more workers in Admin health" })).toBeTruthy();
    });

    it("falls back to the details count when totals have no entry for the size", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3), r(4), r(5)], refusal_totals: { m: 9 } }));
      expect(within(items()[3]).getByRole("link", { name: "and 2 more workers in Admin health" })).toBeTruthy();
    });

    it("never shrinks the remainder below the details sent", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3), r(4), r(5)], refusal_totals: { l: 1 } }));
      expect(within(items()[3]).getByRole("link", { name: "and 2 more workers in Admin health" })).toBeTruthy();
    });

    it("shows no more-item when the total equals the three details", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3)], refusal_totals: { l: 3 } }));
      expect(items()).toHaveLength(3);
      expect(screen.queryByText(/more workers?/)).toBeNull();
    });

    // Above the details count, so only the integer check keeps it out (unguarded: "and 4.5 more").
    it("ignores a total that is not a non-negative integer", () => {
      renderCard(sizes({}, { refusals: [r(1), r(2), r(3)], refusal_totals: { l: 7.5 } }));
      expect(items()).toHaveLength(3);
      expect(screen.queryByText(/more workers?/)).toBeNull();
    });

    // A valid size name can be an Object.prototype key; it must never read the prototype's value.
    it("reads no total for a size named like a prototype key", () => {
      const base = sizes();
      const row = { ...base.sizes[0], name: "constructor", builtin: false, shipped: null, display_order: 9 };
      const refused = [1, 2, 3, 4, 5].map((n) => ({ ...r(n), size: "constructor" }));
      renderCard({ ...base, sizes: [...base.sizes, row], refusals: refused, refusal_totals: {} });
      const list = within(within(rowOf("CONSTRUCTOR")).getByRole("list", { name: "Workers refused at size CONSTRUCTOR" })).getAllByRole("listitem");
      expect(within(list[3]).getByRole("link", { name: "and 2 more workers in Admin health" })).toBeTruthy();
    });
  });

  it("renders no refusal when an older api sends no refusals field", () => {
    const data = sizes();
    delete (data as Partial<AdminWorkerSizes>).refusals;
    renderCard(data);
    expect(screen.queryByText("Refused")).toBeNull();
    expect(screen.queryByRole("list", { name: /Workers refused/ })).toBeNull();
    expect(screen.getByRole("link", { name: "Admin health" })).toBeTruthy();
  });

  it("offers a retry when the first read failed", () => {
    const onChanged = renderCard(null, vi.fn().mockResolvedValue(undefined), "Failed to load worker sizes");
    expect(screen.getByRole("alert").textContent).toBe("Failed to load worker sizes");
    fireEvent.click(screen.getByRole("button", { name: "Retry loading worker sizes" }));
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  // PRD #4213 M3: adding and removing sizes.
  const xsSpec = { cpu_request_millicores: 250, cpu_limit_millicores: 1000, memory_request_mib: 1024, memory_limit_mib: 2048, data_gib: 5, dind_data_gib: 10 };
  const added = (name: string, i: number): AdminWorkerSize => ({
    name, ...xsSpec, display_order: 4 + i, builtin: false, customized: false, shipped: null, updated_at: "2026-11-19T09:00:00Z",
  });
  const withAdded = (...names: string[]): AdminWorkerSizes => {
    const base = sizes();
    return { ...base, sizes: [...base.sizes, ...names.map(added)] };
  };

  // A page stand-in: onChanged re-reads by swapping in `next`, as AdminWorkers lifts the list.
  function Harness({ first, next }: { first: AdminWorkerSizes; next: AdminWorkerSizes }) {
    const [data, setData] = useState(first);
    return (
      <MemoryRouter>
        <WorkerSizesCard data={data} loadError="" onChanged={async () => setData(next)} />
      </MemoryRouter>
    );
  }

  it("adds a size: local name checks, the exact integers on the wire, then the new row with focus on its Edit", async () => {
    mockApi.createAdminWorkerSize.mockResolvedValue({ size: added("xs", 0), warnings: [], advisories: [] });
    render(<Harness first={sizes()} next={withAdded("xs")} />);
    expect(screen.getByText("4 of 16 sizes.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    const name = input("New size name");
    expect(document.activeElement).toBe(name);
    const submit = () => fireEvent.click(within(screen.getByRole("form", { name: "Add a size" })).getByRole("button", { name: /^Add size/ }));

    // Nothing typed: every field says what it needs, and nothing is sent.
    submit();
    expect(await screen.findByText("Enter a name for the size, such as xs")).toBeTruthy();
    expect(name.getAttribute("aria-invalid")).toBe("true");
    expect(input("New size CPU limit (cores)").getAttribute("aria-invalid")).toBe("true");
    for (const [typed, why] of [
      ["XS", "Use lowercase letters only, such as xs"],
      ["1x", "Start with a lowercase letter, then use only lowercase letters, digits or hyphens"],
      ["x_s", "Start with a lowercase letter, then use only lowercase letters, digits or hyphens"],
      ["abcdefghijklmnopq", "Use at most 16 characters"],
      // An existing name is refused here for a fast answer; the api refuses it too (412).
      ["l", "A size named l already exists; edit it in the table instead"],
    ] as const) {
      fireEvent.change(input("New size name"), { target: { value: typed } });
      submit();
      expect(await screen.findByText(why)).toBeTruthy();
    }
    expect(mockApi.createAdminWorkerSize).not.toHaveBeenCalled();

    fireEvent.change(input("New size name"), { target: { value: "xs" } });
    fireEvent.change(input("New size CPU request (cores)"), { target: { value: "0.25" } });
    fireEvent.change(input("New size CPU limit (cores)"), { target: { value: "1" } });
    fireEvent.change(input("New size Memory request (GiB)"), { target: { value: "1" } });
    fireEvent.change(input("New size Memory limit (GiB)"), { target: { value: "2" } });
    fireEvent.change(input("New size Data disk (GiB)"), { target: { value: "5" } });
    fireEvent.change(input("New size DinD data disk (GiB)"), { target: { value: "10" } });
    // The option text the pickers will show, before saving.
    expect(screen.getByText("XS — up to 1 CPU / 2Gi RAM / 5Gi disk")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await waitFor(() => expect(mockApi.createAdminWorkerSize).toHaveBeenCalledWith("xs", xsSpec));
    expect(await screen.findByText("Size XS added.")).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    const xs = within(rowOf("XS"));
    expect(xs.queryByText("Builtin")).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit size XS" })));
    expect(screen.getByText("5 of 16 sizes.")).toBeTruthy();
  });

  it("maps a refused add onto its inputs, and Cancel or Escape returns focus to Add size", async () => {
    const serverText = "memory limit 40960 MiB exceeds the 32768 MiB ceiling (workers.limitRange.max.memory)";
    mockApi.createAdminWorkerSize.mockRejectedValue(
      new ApiError(400, "the size has values that cannot be saved", { error: "the size has values that cannot be saved", fields: { memory_limit_mib: serverText } }),
    );
    // The local check passes for 32 GiB, so the server's refusal is the one shown.
    renderCard(sizes({}, { ceilings: { ...sizes().ceilings, memory_limit_mib: null } }));
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.change(input("New size name"), { target: { value: "big" } });
    for (const [label, value] of [["CPU request", "1"], ["CPU limit", "2"], ["Memory request", "4"], ["Memory limit", "40"], ["Data disk", "10"], ["DinD data disk", "10"]]) {
      fireEvent.change(input(`New size ${label} (${label.startsWith("CPU") ? "cores" : "GiB"})`), { target: { value } });
    }
    fireEvent.click(screen.getByRole("button", { name: "Add size BIG" }));
    expect(await screen.findByText(serverText)).toBeTruthy();
    expect(input("New size Memory limit (GiB)").getAttribute("aria-invalid")).toBe("true");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add size" }));
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.keyDown(input("New size name"), { key: "Escape" });
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add size" }));
  });

  it("hides Add at the 16-size cap and says so; a 409 max_sizes says so too and re-reads", async () => {
    const full = withAdded(...Array.from({ length: 12 }, (_, i) => `x${i}`));
    renderCard(full);
    expect(screen.queryByRole("button", { name: "Add size" })).toBeNull();
    expect(screen.getByText("16 of 16 sizes. At most 16 sizes: remove an added size to add another.")).toBeTruthy();
    cleanup();

    mockApi.createAdminWorkerSize.mockRejectedValue(new ApiError(409, "at most 16 worker sizes", { error: "at most 16 worker sizes", reason: "max_sizes" }));
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.change(input("New size name"), { target: { value: "xs" } });
    for (const [label, value] of [["CPU request", "1"], ["CPU limit", "2"], ["Memory request", "4"], ["Memory limit", "8"], ["Data disk", "10"], ["DinD data disk", "10"]]) {
      fireEvent.change(input(`New size ${label} (${label.startsWith("CPU") ? "cores" : "GiB"})`), { target: { value } });
    }
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    expect(await screen.findByText("At most 16 sizes. Remove an added size to add another.")).toBeTruthy();
    // Not the read-only 409: the editor stays writable.
    expect(screen.queryByText(/Sizes are read-only/)).toBeNull();
    expect(onChanged).toHaveBeenCalledTimes(1);
  });

  it("closes the add form on a 409 max_sizes: the re-read at 16 shows the cap note, no form, and a later Remove does not bring the form back", async () => {
    mockApi.createAdminWorkerSize.mockRejectedValue(new ApiError(409, "at most 16 worker sizes", { error: "at most 16 worker sizes", reason: "max_sizes" }));
    const full = withAdded(...Array.from({ length: 12 }, (_, i) => `x${i}`));
    const view = render(
      <MemoryRouter>
        <WorkerSizesCard data={sizes()} loadError="" onChanged={vi.fn().mockResolvedValue(undefined)} />
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.change(input("New size name"), { target: { value: "xs" } });
    for (const [label, value] of [["CPU request", "1"], ["CPU limit", "2"], ["Memory request", "4"], ["Memory limit", "8"], ["Data disk", "10"], ["DinD data disk", "10"]]) {
      fireEvent.change(input(`New size ${label} (${label.startsWith("CPU") ? "cores" : "GiB"})`), { target: { value } });
    }
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await screen.findByText("At most 16 sizes. Remove an added size to add another.");
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    view.rerender(
      <MemoryRouter>
        <WorkerSizesCard data={full} loadError="" onChanged={vi.fn().mockResolvedValue(undefined)} />
      </MemoryRouter>,
    );
    expect(screen.getByText("16 of 16 sizes. At most 16 sizes: remove an added size to add another.")).toBeTruthy();
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    expect(document.activeElement).not.toBe(document.body);
  });

  const fillAdd = (name: string) => {
    fireEvent.change(input("New size name"), { target: { value: name } });
    for (const [label, value] of [["CPU request", "1"], ["CPU limit", "2"], ["Memory request", "4"], ["Memory limit", "8"], ["Data disk", "10"], ["DinD data disk", "20"]]) {
      fireEvent.change(input(`New size ${label} (${label.startsWith("CPU") ? "cores" : "GiB"})`), { target: { value } });
    }
  };
  const sizeExists = () => new ApiError(412, "worker size xs already exists", { error: "worker size xs already exists", reason: "size_exists" });
  const EXISTS_TEXT = "A size named xs already exists; edit it in the table instead";

  it("keeps the add form and every typed value on a 412 size_exists, names the clash and re-reads", async () => {
    mockApi.createAdminWorkerSize.mockRejectedValue(sizeExists());
    const onChanged = renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fillAdd("xs");
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await waitFor(() => expect(onChanged).toHaveBeenCalledTimes(1));
    const form = within(screen.getByRole("form", { name: "Add a size" }));
    expect(form.getByText(EXISTS_TEXT)).toBeTruthy();
    expect(screen.getByRole("alert").textContent).toContain(EXISTS_TEXT);
    expect(input("New size name").value).toBe("xs");
    expect(input("New size name").getAttribute("aria-invalid")).toBe("true");
    expect(input("New size CPU request (cores)").value).toBe("1");
    expect(input("New size CPU limit (cores)").value).toBe("2");
    expect(input("New size Memory request (GiB)").value).toBe("4");
    expect(input("New size Memory limit (GiB)").value).toBe("8");
    expect(input("New size Data disk (GiB)").value).toBe("10");
    expect(input("New size DinD data disk (GiB)").value).toBe("20");
    expect(screen.queryByText(/Size XS added/)).toBeNull();
    expect(screen.queryByText(/read-only/i)).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(input("New size name")));
  });

  // The re-read resolves later, so the 412 lands while the form still sees 15 rows.
  function DeferredHarness({ release }: { release: { run: () => void } }) {
    const [data, setData] = useState(withAdded(...Array.from({ length: 11 }, (_, i) => `x${i}`)));
    const full = withAdded(...Array.from({ length: 12 }, (_, i) => `x${i}`));
    return (
      <MemoryRouter>
        <WorkerSizesCard
          data={data}
          loadError=""
          onChanged={() => new Promise<void>((resolve) => { release.run = () => { setData(full); resolve(); }; })}
        />
      </MemoryRouter>
    );
  }

  const atCapAfter412 = async () => {
    mockApi.createAdminWorkerSize.mockRejectedValue(sizeExists());
    const release = { run: () => {} };
    render(<DeferredHarness release={release} />);
    expect(screen.getByText("15 of 16 sizes.")).toBeTruthy();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fillAdd("xs");
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await waitFor(() => expect(screen.getByRole("form", { name: "Add a size" }).textContent).toContain(EXISTS_TEXT));
    act(() => release.run());
    return within(screen.getByRole("form", { name: "Add a size" }));
  };

  it("keeps the form, a disabled submit and focus on the name when the 412 re-read lands at the cap; Cancel then lands on the cap note", async () => {
    const form = await atCapAfter412();
    expect(form.getByText("16 of 16 sizes. At most 16 sizes: remove an added size to add another.")).toBeTruthy();
    expect((form.getByRole("button", { name: /^Add size/ }) as HTMLButtonElement).disabled).toBe(true);
    expect(input("New size name").value).toBe("xs");
    expect(document.activeElement).toBe(input("New size name"));

    fireEvent.click(form.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    const note = screen.getByText("16 of 16 sizes. At most 16 sizes: remove an added size to add another.");
    expect(document.activeElement).toBe(note);
  });

  it("Escape in the form at the cap also lands on the cap note", async () => {
    await atCapAfter412();
    fireEvent.keyDown(input("New size name"), { key: "Escape" });
    expect(screen.queryByRole("form", { name: "Add a size" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByText("16 of 16 sizes. At most 16 sizes: remove an added size to add another."));
  });

  it("clears a stale already-exists name error when a size is removed", async () => {
    mockApi.createAdminWorkerSize.mockRejectedValue(sizeExists());
    mockApi.deleteAdminWorkerSize.mockResolvedValue({ deleted: "x0" });
    renderCard(withAdded("x0"));
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fillAdd("xs");
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    expect(await screen.findAllByText(EXISTS_TEXT)).not.toHaveLength(0);
    fireEvent.click(screen.getByRole("button", { name: "Remove size X0" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteAdminWorkerSize).toHaveBeenCalledWith("x0"));
    await waitFor(() => expect(screen.queryByText(EXISTS_TEXT)).toBeNull());
    expect(input("New size name").getAttribute("aria-invalid")).toBeNull();
  });

  it("moves focus to the first invalid input when local validation refuses a submit", async () => {
    renderCard();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    fireEvent.click(within(screen.getByRole("form", { name: "Add a size" })).getByRole("button", { name: /^Add size/ }));
    await waitFor(() => expect(document.activeElement).toBe(input("New size name")));
    // A valid name: the first invalid field in order takes focus.
    fireEvent.change(input("New size name"), { target: { value: "xs" } });
    fireEvent.change(input("New size CPU request (cores)"), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "Add size XS" }));
    await waitFor(() => expect(document.activeElement).toBe(input("New size CPU limit (cores)")));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    fireEvent.change(input("L Memory request (GiB)"), { target: { value: "x" } });
    fireEvent.change(input("L Data disk (GiB)"), { target: { value: "x" } });
    fireEvent.click(screen.getByRole("button", { name: "Save size L" }));
    await waitFor(() => expect(document.activeElement).toBe(input("L Memory request (GiB)")));
    expect(mockApi.updateAdminWorkerSize).not.toHaveBeenCalled();
  });

  it("offers no Add while read-only", () => {
    renderCard({ ...withAdded("xs"), editable: false, read_only_reason: "The controller report is stale.", read_only_code: "controller_report_stale" });
    expect(screen.queryByRole("button", { name: "Add size" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Remove size XS" })).toBeNull();
  });

  it("offers Remove on added sizes only, confirms inline, then removes and announces it", async () => {
    mockApi.deleteAdminWorkerSize.mockResolvedValue({ deleted: "xs" });
    render(<Harness first={withAdded("xs")} next={sizes()} />);
    for (const builtin of ["S", "M", "L", "L-HIGHMEM"]) {
      expect(screen.queryByRole("button", { name: `Remove size ${builtin}` })).toBeNull();
    }
    expect(screen.getAllByRole("rowheader").map((h) => h.textContent)).toEqual(["SBuiltinEdit", "MBuiltinEdit", "LBuiltinEdit", "L-HIGHMEMBuiltinEdit", "XSEditRemove"]);
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    const group = within(screen.getByRole("group", { name: "Confirm removal of size XS" }));
    expect(group.getByText("Remove size XS? Workers can no longer be provisioned with it.")).toBeTruthy();
    expect(document.activeElement).toBe(group.getByRole("button", { name: "Remove" }));
    expect(mockApi.deleteAdminWorkerSize).not.toHaveBeenCalled();

    // Keep and Escape send nothing.
    fireEvent.click(group.getByRole("button", { name: "Keep" }));
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.keyDown(screen.getByRole("button", { name: "Remove" }), { key: "Escape" });
    expect(screen.queryByRole("group", { name: "Confirm removal of size XS" })).toBeNull();
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Remove size XS" }));
    expect(mockApi.deleteAdminWorkerSize).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(mockApi.deleteAdminWorkerSize).toHaveBeenCalledWith("xs"));
    const done = await screen.findByText("Size XS removed.");
    expect(screen.queryByRole("button", { name: "Edit size XS" })).toBeNull();
    // The row is gone, so focus lands on the message that says so.
    await waitFor(() => expect(document.activeElement).toBe(done.parentElement));
  });

  it("says what still uses a size the api refuses to remove, and keeps the row", async () => {
    const error = "size is in use: 2 worker(s) still use it; it is the stored value of ephemeral_default_size, hosted_default_size";
    mockApi.deleteAdminWorkerSize.mockRejectedValue(
      new ApiError(409, error, { error, reason: "in_use", used_by: { workers: 2, settings: ["ephemeral_default_size", "hosted_default_size"] } }),
    );
    const onChanged = renderCard(withAdded("xs"));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe(
        "XS is still in use, so it was not removed. Still used by 2 workers. Set as the default size for ephemeral workers and new workers; choose another default first.",
      ),
    );
    expect(screen.queryByText(/Sizes are read-only/)).toBeNull();
    expect(onChanged).not.toHaveBeenCalled();
    expect(rowOf("XS")).toBeTruthy();
    await waitFor(() => expect(document.activeElement).toBe(screen.getByRole("button", { name: "Edit size XS" })));
    // The refusal sits inside that row, under its actions.
    expect(within(rowOf("XS")).getByRole("alert").textContent).toMatch(/^XS is still in use/);
    cleanup();

    // One worker and no setting, or only a setting, say only that.
    mockApi.deleteAdminWorkerSize.mockRejectedValueOnce(new ApiError(409, "size is in use: 1 worker(s) still use it", { error: "", reason: "in_use", used_by: { workers: 1, settings: [] } }));
    renderCard(withAdded("xs"));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe("XS is still in use, so it was not removed. Still used by 1 worker."));
    cleanup();
    mockApi.deleteAdminWorkerSize.mockRejectedValueOnce(new ApiError(409, "x", { error: "x", reason: "in_use", used_by: { workers: 0, settings: ["hosted_default_size"] } }));
    renderCard(withAdded("xs"));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe("XS is still in use, so it was not removed. Set as the default size for new workers; choose another default first."),
    );
  });

  // #4516: a removal refusal belongs to the failed attempt only; another action or a
  // re-read of the list clears it.
  const inUse = () => new ApiError(409, "size is in use: 1 worker(s) still use it", { error: "", reason: "in_use", used_by: { workers: 1, settings: [] } });
  const refuseXs = async () => {
    mockApi.deleteAdminWorkerSize.mockRejectedValueOnce(inUse());
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(within(rowOf("XS")).getByRole("alert").textContent).toBe("XS is still in use, so it was not removed. Still used by 1 worker."),
    );
  };
  const noXsAlert = () => expect(within(rowOf("XS")).queryByRole("alert")).toBeNull();

  it("keeps a removal refusal in its row until another action starts", async () => {
    renderCard(withAdded("xs", "xl"));
    await refuseXs();
    // Still shown while nothing else happens; never in another row.
    expect(within(rowOf("XL")).queryByRole("alert")).toBeNull();
    expect(screen.getAllByRole("alert")).toHaveLength(1);
  });

  it("clears a removal refusal when Add size starts, and the new size's success stands alone", async () => {
    mockApi.createAdminWorkerSize.mockResolvedValue({ size: added("tiny", 1), warnings: [], advisories: [] });
    render(<Harness first={withAdded("xs")} next={withAdded("xs", "tiny")} />);
    await refuseXs();
    fireEvent.click(screen.getByRole("button", { name: "Add size" }));
    // Cleared by starting the add, before anything is submitted or re-read.
    noXsAlert();
    fireEvent.change(input("New size name"), { target: { value: "tiny" } });
    fireEvent.change(input("New size CPU request (cores)"), { target: { value: "0.25" } });
    fireEvent.change(input("New size CPU limit (cores)"), { target: { value: "1" } });
    fireEvent.change(input("New size Memory request (GiB)"), { target: { value: "1" } });
    fireEvent.change(input("New size Memory limit (GiB)"), { target: { value: "2" } });
    fireEvent.change(input("New size Data disk (GiB)"), { target: { value: "5" } });
    fireEvent.change(input("New size DinD data disk (GiB)"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "Add size TINY" }));
    expect(await screen.findByText("Size TINY added.")).toBeTruthy();
    noXsAlert();
    expect(screen.queryByText(/still in use/)).toBeNull();
  });

  it("clears a removal refusal when a re-read of the list arrives", async () => {
    const view = render(
      <MemoryRouter>
        <WorkerSizesCard data={withAdded("xs")} loadError="" onChanged={vi.fn().mockResolvedValue(undefined)} />
      </MemoryRouter>,
    );
    await refuseXs();
    // A fresh list from a re-read (the list carries no usage, so equal content stands in
    // for usage that changed elsewhere): the old explanation may no longer be current.
    view.rerender(
      <MemoryRouter>
        <WorkerSizesCard
          data={{ ...withAdded("xs"), advisories: [] }}
          loadError=""
          onChanged={vi.fn().mockResolvedValue(undefined)}
        />
      </MemoryRouter>,
    );
    noXsAlert();
    expect(rowOf("XS")).toBeTruthy();
  });

  it("clears a removal refusal when another row's Remove or Edit starts", async () => {
    renderCard(withAdded("xs", "xl"));
    await refuseXs();
    fireEvent.click(screen.getByRole("button", { name: "Remove size XL" }));
    noXsAlert();
    fireEvent.click(screen.getByRole("button", { name: "Keep" }));

    await refuseXs();
    fireEvent.click(screen.getByRole("button", { name: "Edit size L" }));
    noXsAlert();
  });

  it("a 404 on remove says the size was already removed and moves focus to that message", async () => {
    mockApi.deleteAdminWorkerSize.mockRejectedValue(new ApiError(404, "size not found", { error: "size not found" }));
    renderCard(withAdded("xs"));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    const msg = await screen.findByText("Size XS was already removed.");
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
  });

  it("spells a builtin refusal in the page's words if the api ever returns one", async () => {
    mockApi.deleteAdminWorkerSize.mockRejectedValue(
      new ApiError(409, "builtin sizes cannot be removed; reset it instead", { error: "builtin sizes cannot be removed; reset it instead", reason: "builtin", used_by: { workers: 0, settings: [] } }),
    );
    // A row the list marks added but the api knows as builtin (a stale list).
    renderCard(withAdded("xs"));
    fireEvent.click(screen.getByRole("button", { name: "Remove size XS" }));
    fireEvent.click(screen.getByRole("button", { name: "Remove" }));
    await waitFor(() =>
      expect(screen.getByRole("alert").textContent).toBe("XS is a builtin size and cannot be removed. Reset it to shipped values instead."),
    );
  });
});
