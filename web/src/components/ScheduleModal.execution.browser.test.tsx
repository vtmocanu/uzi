import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { ScheduleModal } from "./ScheduleModal";

vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ uziLabel: "uzi" }) }));
vi.mock("../lib/api", async (importOriginal) => ({
  ...await importOriginal<typeof import("../lib/api")>(),
  api: {
    listScheduleCatalog: vi.fn().mockResolvedValue({ entries: [] }),
    listRepos: vi.fn().mockResolvedValue({ repos: [{ id: "repo-test", path_with_namespace: "owner/repo" }] }),
    previewSchedule: vi.fn().mockResolvedValue({ fires: [] }),
    listSecrets: vi.fn().mockResolvedValue({
      secrets: [
        { id: "a", kind: "anthropic_token", label: "claude", is_default: true, enabled: true },
        { id: "c", kind: "openai_api_key", label: "codex", is_default: true, enabled: true },
      ],
    }),
    getMySettings: vi.fn().mockResolvedValue({
      settings: { default_harness: "claude", harness_overflow_enabled: true },
    }),
  },
}));
afterEach(() => cleanup());

it("keeps the Where it runs group reachable and every control named at 390px", async () => {
  await page.viewport(390, 844);
  render(<MemoryRouter><ScheduleModal onClose={vi.fn()} onSaved={vi.fn()} /></MemoryRouter>);
  const group = await screen.findByRole("group", { name: "Where it runs: harness, model & token" });
  // The longest Default option (overflow wording) only exists once settings have loaded.
  await within(group).findByRole("option", { name: /may overflow to Codex/ });
  group.scrollIntoView();
  const rect = group.getBoundingClientRect();
  expect(rect.height).toBeGreaterThan(0);
  expect(rect.left).toBeGreaterThanOrEqual(0);
  expect(rect.right).toBeLessThanOrEqual(390);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);

  const controls = Array.from(group.querySelectorAll<HTMLElement>("select, [role=switch]"));
  expect(controls.length).toBeGreaterThanOrEqual(4);
  for (const el of controls) {
    const labels = (el as HTMLSelectElement).labels;
    const name = el.getAttribute("aria-label") || labels?.[0]?.textContent || "";
    expect(name.trim(), el.outerHTML.slice(0, 80)).not.toBe("");
    const r = el.getBoundingClientRect();
    expect(r.right).toBeLessThanOrEqual(390);
    expect(r.height).toBeGreaterThan(0);
  }
});
