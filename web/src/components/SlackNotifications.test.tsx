// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SlackNotifications } from "./SlackNotifications";
import { api, ApiError, type SlackLink } from "../lib/api";

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
  return { member_id: null, notify: true, resolved_id: null, confirmed: false, state: "unlinked", ...overrides };
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

describe("SlackNotifications (PRD #25 M3)", () => {
  it("shows the confirmed state with the resolved id", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ member_id: null, resolved_id: "U123", confirmed: true, state: "confirmed" }),
    });
    render(<SlackNotifications />);
    expect(await screen.findByText("Linked")).toBeTruthy();
    expect(screen.getByText("U123")).toBeTruthy();
  });

  it("shows the pending state after an unconfirmed match", async () => {
    mockApi.getMySlack.mockResolvedValue({
      slack: link({ resolved_id: "U1", state: "pending" }),
    });
    render(<SlackNotifications />);
    expect(await screen.findByText("Pending confirmation")).toBeTruthy();
  });

  it("reflects and toggles the notify switch", async () => {
    render(<SlackNotifications />);
    await waitFor(() => expect(notifyToggle().checked).toBe(true));
    fireEvent.click(notifyToggle());
    await waitFor(() => expect(mockApi.setMySlackNotify).toHaveBeenCalledWith(false));
    await waitFor(() => expect(notifyToggle().checked).toBe(false));
  });

  it("saves a member-ID override and reports the pending confirmation", async () => {
    render(<SlackNotifications />);
    await waitFor(() => expect(overrideInput()).toBeTruthy());
    fireEvent.change(overrideInput(), { target: { value: "U9" } });
    fireEvent.click(screen.getByText("Save override"));
    await waitFor(() => expect(mockApi.setMySlackOverride).toHaveBeenCalledWith("U9"));
    expect(await screen.findByText(/confirmation DM/i)).toBeTruthy();
  });

  it("surfaces a 409 collision when the id is already linked", async () => {
    mockApi.setMySlackOverride.mockRejectedValue(
      new ApiError(409, "that Slack member ID is already linked to another account"),
    );
    render(<SlackNotifications />);
    await waitFor(() => expect(overrideInput()).toBeTruthy());
    fireEvent.change(overrideInput(), { target: { value: "U9" } });
    fireEvent.click(screen.getByText("Save override"));
    expect(await screen.findByText(/already linked to another account/i)).toBeTruthy();
  });

  const testButton = () => screen.getByText("Send test DM").closest("button") as HTMLButtonElement;

  it("disables the test DM while unlinked", async () => {
    render(<SlackNotifications />);
    await waitFor(() => expect(testButton().disabled).toBe(true));
  });

  it("sends a test DM once a Slack id resolves", async () => {
    mockApi.getMySlack.mockResolvedValue({ slack: link({ resolved_id: "U9", state: "pending" }) });
    render(<SlackNotifications />);
    await waitFor(() => expect(testButton().disabled).toBe(false));
    fireEvent.click(testButton());
    await waitFor(() => expect(mockApi.testMySlackDM).toHaveBeenCalled());
    expect(await screen.findByText(/Test DM sent/i)).toBeTruthy();
  });
});
