import "../../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page } from "vitest/browser";
import { cleanup, render, screen } from "@testing-library/react";
import { DockerAllowlistCard } from "./DockerAllowlistCard";
import { api, type AdminDockerAllowlistRepos, type AppSettings } from "../../lib/api";

vi.mock("../../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../../lib/api")>();
  return { ...actual, api: { adminListDockerAllowlistRepos: vi.fn(), updateSettings: vi.fn() } };
});
const mockApi = vi.mocked(api);

// Unbreakable tokens: no spaces, hyphens or slashes for the line breaker to use.
const LONG_PATH = "group" + "x".repeat(120) + "_project" + "y".repeat(40);
const LONG_EMAIL = "owner" + "e".repeat(110) + "@example.com";
const LONG_HOST = "https://" + "h".repeat(110) + ".example.com";

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// #4428: a <fieldset> defaults to min-inline-size: min-content, so one long repository row
// used to stretch the card (and the page) past a 400px viewport.
it("keeps a long repository row inside a 400px viewport (#4428)", async () => {
  await page.viewport(400, 900);
  mockApi.adminListDockerAllowlistRepos.mockResolvedValue({
    repos: [
      {
        id: "11111111-1111-4111-8111-111111111111",
        path_with_namespace: LONG_PATH,
        enabled: false,
        owner_email: LONG_EMAIL,
        connection_id: "22222222-2222-4222-8222-222222222222",
        forge_type: "gitlab",
        base_url: LONG_HOST,
      },
    ],
  } as unknown as AdminDockerAllowlistRepos);
  render(
    <DockerAllowlistCard
      settings={{ docker_repo_allowlist: "" } as unknown as AppSettings}
      sources={{}}
      onSaved={() => {}}
    />,
  );
  // Measure the loaded rows, never the "Loading repositories…" placeholder.
  await screen.findByText(LONG_PATH);

  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(400);
  const fieldset = document.querySelector("fieldset")!;
  expect(fieldset.getBoundingClientRect().right).toBeLessThanOrEqual(400);
  // The row text ellipsizes instead of scrolling the list box sideways.
  const list = fieldset.querySelector("div.overflow-y-auto") as HTMLElement;
  expect(list.scrollWidth).toBeLessThanOrEqual(list.clientWidth);
  // The checkbox keeps its 16px size rather than being squeezed by the long text.
  const box = screen.getByRole("checkbox");
  expect(Math.round(box.getBoundingClientRect().width)).toBe(16);
  expect(screen.getByText("Disabled").getBoundingClientRect().right).toBeLessThanOrEqual(400);
});
