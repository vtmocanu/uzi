// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { CancelRequestCard } from "./CancelRequestCard";
import { api, ApiError, type CancelRequest } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { cancelRunFromChat: vi.fn() } };
});

const mockApi = vi.mocked(api);

function aRequest(over: Partial<CancelRequest> = {}): CancelRequest {
  return { run_id: "run-9", ...over };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("CancelRequestCard — model text is inert", () => {
  it("strips bidi/zero-width chars and renders run_id as plain text, never a link", () => {
    const { container } = render(
      <CancelRequestCard request={aRequest({ run_id: "run‮-evil​-9" })} />,
    );
    // No control-format chars survive to the DOM.
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    // A cancel card renders ZERO anchors — nothing model-authored is ever clickable.
    expect(container.querySelector("a")).toBeNull();
  });

  it("shows Cancel run / Dismiss and the run id", () => {
    render(<CancelRequestCard request={aRequest()} />);
    expect(screen.getByRole("button", { name: "Cancel run" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
    expect(screen.getByText("run-9")).toBeTruthy();
  });
});

describe("CancelRequestCard — cancel is the only write path", () => {
  it("on Cancel, calls cancelRunFromChat and confirms the cancellation", async () => {
    mockApi.cancelRunFromChat.mockResolvedValue({ server_side: true });

    render(<CancelRequestCard request={aRequest()} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel run" }));

    await waitFor(() => expect(screen.getByText(/Run cancelled/)).toBeTruthy());
    expect(mockApi.cancelRunFromChat).toHaveBeenCalledWith("run-9");
  });

  it("surfaces a server refusal (409 terminal) as the card error, cancels nothing visible", async () => {
    mockApi.cancelRunFromChat.mockRejectedValue(new ApiError(409, "run has already finished"));

    render(<CancelRequestCard request={aRequest()} />);
    fireEvent.click(screen.getByRole("button", { name: "Cancel run" }));

    await waitFor(() => expect(screen.getByText(/already finished/)).toBeTruthy());
    expect(screen.queryByText(/Run cancelled/)).toBeNull();
  });

  it("on Dismiss, cancels nothing", () => {
    render(<CancelRequestCard request={aRequest()} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.getByText(/was not cancelled/)).toBeTruthy();
    expect(mockApi.cancelRunFromChat).not.toHaveBeenCalled();
  });
});
