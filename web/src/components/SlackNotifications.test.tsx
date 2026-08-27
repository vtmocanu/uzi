// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { SlackNotifications } from "./SlackNotifications";
import { api, ApiError, type SlackLink } from "../lib/api";

// SlackNotifications now renders a DocLink (a react-router Link), so it must mount
// inside a Router context (PRD #57 M2).
const renderCard = () =>
  render(
    <MemoryRouter>
      <SlackNotifications />
    </MemoryRouter>,
  );

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      getMySlack: vi.fn(),
      setMySlackNotify: vi.fn(),
      setMySlackOverride: vi.fn(),
      testMySlackDM: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

function link(overrides: Partial<SlackLink> = {}): SlackLink {
  return { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked", workspace: "connected", ...overrides };
}

const notifyToggle = () =>
  screen.getByLabelText("Send me Slack notifications about my runs") as HTMLInputElement;
const overrideInput = () => screen.getByLabelText("Slack member ID override") as HTMLInputElement;

beforeEach(() => {
  mockApi.getMySlack.mockResolvedValue({ slack: link() });
  mockApi.setMySlackNotify.mockResolvedValue({ slack: link({ notify: false }) });
  mockApi.setMySlackOverride.mockResolvedValue({
    slack: link({ member_id: "U9", resolved_id: "U9", state: "pending" }),
  });
  mockApi.testMySlackDM.mockResolvedValue({ status: "sent" });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SlackNotifications — always-visible Slack guide link (PRD #57 M2)", () => {
  it("renders the Slack notifications guide link in the intro", async () => {
    renderCard();
    const docLink = await screen.findByRole("link", { name: "Slack notifications" });
    expect(docLink.getAttribute("href")).toBe("/docs/slack");
  });
});

describe("SlackNotifications (PRD #25 M3)", () => {
  it("shows the confirmed state with the resolved id", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ member_id: null, resolved_id: "U123", confirmed: true, state: "confirmed" }),
    });
    renderCard();
    expect(await screen.findByText("Linked")).toBeTruthy();
    expect(screen.getByText("U123")).toBeTruthy();
  });

  it("shows the pending state after an unconfirmed match", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ resolved_id: "U1", state: "pending" }),
    });
    renderCard();
    expect(await screen.findByText("Pending confirmation")).toBeTruthy();
  });

  it("reflects and toggles the notify switch", async () => {
    renderCard();
    await waitFor(() => expect(notifyToggle().checked).toBe(true));
    fireEvent.click(notifyToggle());
    await waitFor(() => expect(mockApi.setMySlackNotify).toHaveBeenCalledWith(false));
    await waitFor(() => expect(notifyToggle().checked).toBe(false));
  });

  it("saves a member-ID override and reports the pending confirmation", async () => {
    renderCard();
    await waitFor(() => expect(overrideInput()).toBeTruthy());
    fireEvent.change(overrideInput(), { target: { value: "U9" } });
    fireEvent.click(screen.getByText("Save override"));
    await waitFor(() => expect(mockApi.setMySlackOverride).toHaveBeenCalledWith("U9"));
    // The success notice and the pending helper both mention "confirmation DM";
    // match the notice specifically so the assertion stays unambiguous.
    expect(await screen.findByText(/Override saved/i)).toBeTruthy();
  });

  it("surfaces a 409 collision when the id is already linked", async () => {
    mockApi.setMySlackOverride.mockRejectedValue(
      new ApiError(409, "that Slack member ID is already linked to another account"),
    );
    renderCard();
    await waitFor(() => expect(overrideInput()).toBeTruthy());
    fireEvent.change(overrideInput(), { target: { value: "U9" } });
    fireEvent.click(screen.getByText("Save override"));
    expect(await screen.findByText(/already linked to another account/i)).toBeTruthy();
  });

  const testButton = () => screen.getByText("Send test DM").closest("button") as HTMLButtonElement;

  it("disables the test DM while unlinked", async () => {
    renderCard();
    await waitFor(() => expect(testButton().disabled).toBe(true));
  });

  it("sends a test DM once a Slack id resolves", async () => {
    mockApi.getMySlack.mockResolvedValue({ slack: link({ resolved_id: "U9", state: "pending" }) });
    renderCard();
    await waitFor(() => expect(testButton().disabled).toBe(false));
    fireEvent.click(testButton());
    await waitFor(() => expect(mockApi.testMySlackDM).toHaveBeenCalled());
    expect(await screen.findByText(/Test DM sent/i)).toBeTruthy();
  });

  const saveButton = () => screen.getByText("Save override").closest("button") as HTMLButtonElement;

  // Workspace axis (PRD #56 M2). These compose over the link-state layout above.
  it("explains an unconfigured workspace and disables every control", async () => {
    mockApi.getMySlack.mockResolvedValue({ slack: link({ workspace: "unconfigured" }) });
    const { container } = renderCard();
    await waitFor(() => expect(notifyToggle()).toBeTruthy());
    expect(container.textContent).toContain("Slack isn't connected on this uzi instance yet");
    expect(notifyToggle().disabled).toBe(true);
    expect(overrideInput().disabled).toBe(true);
    expect(saveButton().disabled).toBe(true);
    expect(testButton().disabled).toBe(true);
  });

  it("hints where test DMs come from when unlinked, with no workspace alert", async () => {
    mockApi.getMySlack.mockResolvedValue({ slack: link({ workspace: "connected", state: "unlinked" }) });
    const { container } = renderCard();
    await waitFor(() => expect(notifyToggle()).toBeTruthy());
    expect(container.textContent).toContain("Test DMs become available once uzi resolves your Slack account");
    expect(container.querySelector('[role="status"]')).toBeNull();
  });

  it("guides the user to confirm when pending and keeps the test DM enabled", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ workspace: "connected", state: "pending", resolved_id: "U1" }),
    });
    const { container } = renderCard();
    await waitFor(() => expect(testButton().disabled).toBe(false));
    expect(container.textContent).toContain("Check Slack for a confirmation DM from uzi");
  });

  it("shows the reconnecting alert but leaves controls enabled", async () => {
    mockApi.getMySlack.mockResolvedValue({ slack: link({ workspace: "connecting" }) });
    const { container } = renderCard();
    await waitFor(() => expect(notifyToggle()).toBeTruthy());
    expect(container.textContent).toContain("Slack is reconnecting");
    expect(notifyToggle().disabled).toBe(false);
  });

  // The test DM stays clickable outside `unconfigured`, so a resolved user on an
  // `error` workspace can still fire one and must see the backend failure. Guards
  // the PRD #56 M2 claim that the test-DM error path surfaces the 502.
  it("surfaces the 502 when a test DM fails during an error workspace (PRD #56 M2)", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ workspace: "error", resolved_id: "U9", state: "pending" }),
    });
    mockApi.testMySlackDM.mockRejectedValue(new ApiError(502, "Slack is unavailable right now (502)"));
    renderCard();
    await waitFor(() => expect(testButton().disabled).toBe(false));
    fireEvent.click(testButton());
    await waitFor(() => expect(mockApi.testMySlackDM).toHaveBeenCalled());
    expect(await screen.findByText(/502/)).toBeTruthy();
  });
});
