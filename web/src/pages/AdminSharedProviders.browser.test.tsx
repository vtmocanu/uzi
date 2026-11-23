import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { AdminSharedProviders } from "./AdminSharedProviders";
import { api, type AdminSharedProvider } from "../lib/api";

// PRD #4324 M4: Admin → Shared providers at phone width. A long fixed endpoint, the counts and
// the delete confirmation must wrap inside the card, never push the page sideways, and the
// Cancel takes focus (never the destructive button) with a visible focus ring.
vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { adminListSharedProviders: vi.fn(), listProviders: vi.fn(), adminDeleteSharedProvider: vi.fn() } };
});
afterEach(cleanup);

it("keeps the card, fixed facts and delete confirmation inside a 390px viewport", async () => {
  await page.viewport(390, 844);
  const mock = vi.mocked(api);
  const url = "https://gateway.example.com/a/very/long/path/that/must/wrap/inside/the/card/anthropic/v1";
  const row: AdminSharedProvider = {
    id: "s1", name: "platform-gateway-with-a-rather-long-name", engine: "claude", retired: false, current_revision: 2,
    protocol: "anthropic_messages", base_url: url, auth_style: "x_api_key",
    model_map: { haiku: { model: "platform-small", context_window: 65536 } },
    credential_mode: "per_user_key", selected_user_count: 2, attached_key_count: 5,
  };
  mock.adminListSharedProviders.mockResolvedValue({ providers: [row] });
  mock.listProviders.mockResolvedValue({ enabled: true, allowed_base_urls: [url], providers: [] });
  render(<MemoryRouter><div className="px-4"><AdminSharedProviders /></div></MemoryRouter>);
  const list = await screen.findByRole("list", { name: "Shared providers" });
  const fixed = within(list).getByTestId("fixed-facts");
  expect(fixed.scrollWidth).toBeLessThanOrEqual(fixed.clientWidth + 1);
  await userEvent.click(within(list).getByRole("button", { name: `Delete ${row.name}` }));
  const group = within(list).getByRole("group", { name: `Confirm deleting ${row.name}` });
  expect(within(group).getByTestId("delete-keys-s1").textContent).toMatch(/5 personal keys/);
  expect(within(group).getByTestId("delete-selected-s1").textContent).toMatch(/2 users/);
  const confirm = within(group).getByRole("button", { name: "Delete definition and keys" }) as HTMLButtonElement;
  expect(confirm.disabled).toBe(true);
  // Focus lands on Cancel, the safe choice. Keyboard focus shows the global
  // :focus-visible ring (a programmatic focus after a mouse click deliberately does not).
  const cancel = within(group).getByRole("button", { name: "Cancel" });
  expect(document.activeElement).toBe(cancel);
  await userEvent.tab({ shift: true });
  await userEvent.tab();
  expect(document.activeElement).toBe(cancel);
  expect(cancel.matches(":focus-visible")).toBe(true);
  expect(getComputedStyle(cancel).boxShadow).not.toBe("none"); // the index.css ring-2 ring
  expect(group.scrollWidth).toBeLessThanOrEqual(group.clientWidth + 1);
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(390);
  expect(mock.adminDeleteSharedProvider).not.toHaveBeenCalled();
});
