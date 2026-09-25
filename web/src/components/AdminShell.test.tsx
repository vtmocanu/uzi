// @vitest-environment jsdom
//
// PRD #1648 D9: the Admin > Health tab carries the shared count pill with a worded label
// naming every non-zero severity. Asserted by accessible name and visible count, never by
// a colour class. AdminShell is mounted standalone, so useHealthStatus self-fetches.
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";

import { AdminShell } from "./AdminShell";
import { api, type HealthDoc } from "../lib/api";
import { healthySilentDoc, incidentDoc, unknownDoc } from "../mocks/data/health";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { getAdminHealth: vi.fn() } };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

async function renderShell(doc: HealthDoc) {
  mockApi.getAdminHealth.mockResolvedValue(doc);
  render(
    <MemoryRouter initialEntries={["/admin/users"]}>
      <AdminShell description="d">
        <div>body</div>
      </AdminShell>
    </MemoryRouter>,
  );
  await waitFor(() => expect(mockApi.getAdminHealth).toHaveBeenCalled());
}

describe("AdminShell health tab pip", () => {
  it("names danger and warning separately on an incident, with the attention total visible", async () => {
    await renderShell(incidentDoc()); // 3 danger + 1 warn
    const pip = await screen.findByLabelText("4 health checks need attention: 3 danger, 1 warning");
    expect(within(pip).getByText("4")).toBeTruthy();
    // The pip sits inside the Health tab link.
    expect(screen.getByRole("link", { name: /Health/ }).contains(pip)).toBe(true);
  });

  it("names unknown as unknown rather than folding it into warning", async () => {
    await renderShell(unknownDoc()); // 4 unknown
    expect(await screen.findByLabelText("4 health checks need attention: 4 unknown")).toBeTruthy();
    expect(screen.queryByLabelText(/warning/)).toBeNull();
  });

  it("renders no pip when nothing needs attention", async () => {
    await renderShell(healthySilentDoc());
    await screen.findByRole("link", { name: "Health" });
    expect(screen.queryByLabelText(/health checks? needs? attention/)).toBeNull();
  });
});
