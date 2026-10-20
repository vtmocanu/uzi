import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";
import { CIWatchDetail } from "./CIWatchDetail";
import { api, type Schedule } from "../lib/api";

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
vi.mock("../lib/api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../lib/api")>(),
  api: {
    listScheduleCatalog: vi.fn().mockResolvedValue({ entries: [] }),
    listRepos: vi.fn().mockResolvedValue({ repos: [{ id: "repo-test", path_with_namespace: "owner/repo" }] }),
    previewSchedule: vi.fn().mockResolvedValue({ fires: [] }),
    listSecrets: vi.fn().mockResolvedValue({ secrets: [] }),
    getMySettings: vi.fn().mockResolvedValue({ settings: { default_harness: null } }),
    getSchedule: vi.fn().mockResolvedValue({ ci_episodes: { episodes: [], next_cursor: null } }),
    createSchedule: vi.fn().mockResolvedValue({}),
  },
}));
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it("associates artifact errors with the labelled field and keeps narrow actions reachable", async () => {
  await page.viewport(390, 844);
  render(<MemoryRouter><ScheduleModal onClose={vi.fn()} onSaved={vi.fn()} /></MemoryRouter>);
  await screen.findByLabelText("owner/repo");
  fireEvent.click(screen.getByRole("radio", { name: /CI repair/ }));
  const field = screen.getByRole("textbox", { name: "Artifact names or globs (optional)" });
  fireEvent.change(field, { target: { value: "[" } });
  expect(field.getAttribute("aria-invalid")).toBe("true");
  const description = field.getAttribute("aria-describedby")!.split(" ").map(id => document.getElementById(id)?.textContent).join(" ");
  expect(description).toContain("valid path.Match globs");
  const create = screen.getByRole("button", { name: "Create schedule" }) as HTMLButtonElement;
  expect(create.disabled).toBe(true);
  fireEvent.change(field, { target: { value: "logs-*" } });
  await waitFor(() => expect(create.disabled).toBe(false));
  field.scrollIntoView();
  field.focus();
  expect(document.activeElement).toBe(field);
  expect(field.getBoundingClientRect().width).toBeGreaterThan(200);
  const cancel = screen.getByRole("button", { name: "Cancel" });
  for (const action of [cancel, create]) {
    action.scrollIntoView();
    const rect = action.getBoundingClientRect();
    expect(rect.left).toBeGreaterThanOrEqual(0);
    expect(rect.right).toBeLessThanOrEqual(390);
    expect(rect.height).toBeGreaterThan(0);
  }
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
  fireEvent.click(create);
  await waitFor(() => expect(api.createSchedule).toHaveBeenCalledWith("repo-test", expect.objectContaining({ ci_artifacts: ["logs-*"] })));
});

it("wraps long artifact selectors within a narrow detail panel", async () => {
  await page.viewport(390, 844);
  render(<div className="p-4"><CIWatchDetail s={{ id: "ci-test", auto_approve: false, ci_artifacts: ["x".repeat(200)] } as Schedule} panelId="ci-test" /></div>);
  const selector = screen.getByText("x".repeat(200));
  expect(selector.getBoundingClientRect().height).toBeGreaterThan(30);
  expect(selector.getBoundingClientRect().right).toBeLessThanOrEqual(390);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
});
