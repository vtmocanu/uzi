// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { SteerRequestCard } from "./SteerRequestCard";
import { api, ApiError, type SteerRequest } from "../lib/api";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { steerRunFromChat: vi.fn() } };
});

const mockApi = vi.mocked(api);

function aRequest(over: Partial<SteerRequest> = {}): SteerRequest {
  return { run_id: "run-9", message: "focus on the auth path", ...over };
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("SteerRequestCard — model text is inert", () => {
  it("shows the proposed message in an editable textarea, strips control chars, renders no link", () => {
    const { container } = render(
      <SteerRequestCard request={aRequest({ run_id: "run‮-evil​-9", message: "do ‮the thing" })} />,
    );
    // No control-format chars survive to the DOM (run_id text + textarea value).
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    const textarea = screen.getByRole("textbox") as HTMLTextAreaElement;
    expect(textarea.value).not.toMatch(/[\p{Cf}]/u);
    // A steer card renders ZERO anchors — nothing model-authored is ever clickable.
    expect(container.querySelector("a")).toBeNull();
  });

  it("prefills the textarea with the proposed message and shows Send / Dismiss and the run id", () => {
    render(<SteerRequestCard request={aRequest()} />);
    expect((screen.getByRole("textbox") as HTMLTextAreaElement).value).toBe("focus on the auth path");
    expect(screen.getByRole("button", { name: "Send" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Dismiss" })).toBeTruthy();
    expect(screen.getByText("run-9")).toBeTruthy();
  });
});

describe("SteerRequestCard — Send is the only write path", () => {
  it("on Send, sends the EDITED textarea value and confirms delivery", async () => {
    mockApi.steerRunFromChat.mockResolvedValue({ server_side: false });

    render(<SteerRequestCard request={aRequest()} />);
    const textarea = screen.getByRole("textbox");
    fireEvent.change(textarea, { target: { value: "actually, refactor the parser" } });
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(screen.getByText(/Follow-up sent/)).toBeTruthy());
    expect(mockApi.steerRunFromChat).toHaveBeenCalledWith("run-9", "actually, refactor the parser");
  });

  it("surfaces a chat-run 409 as the card error, sends nothing visible", async () => {
    mockApi.steerRunFromChat.mockRejectedValue(new ApiError(409, "steering applies to issue runs, not chats"));

    render(<SteerRequestCard request={aRequest()} />);
    fireEvent.click(screen.getByRole("button", { name: "Send" }));

    await waitFor(() => expect(screen.getByText(/issue runs, not chats/)).toBeTruthy());
    expect(screen.queryByText(/Follow-up sent/)).toBeNull();
  });

  it("disables Send when the message is emptied", () => {
    render(<SteerRequestCard request={aRequest()} />);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "   " } });
    expect((screen.getByRole("button", { name: "Send" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("on Dismiss, sends nothing", () => {
    render(<SteerRequestCard request={aRequest()} />);
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }));
    expect(screen.getByText(/No follow-up was sent/)).toBeTruthy();
    expect(mockApi.steerRunFromChat).not.toHaveBeenCalled();
  });
});
