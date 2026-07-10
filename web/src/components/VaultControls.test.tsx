// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { VaultBadge, VaultLockedBanner } from "./VaultControls";
import { api, ApiError } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { vaultUnlock: vi.fn(), vaultLock: vi.fn() } };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const refresh = vi.fn();

function setAuth(over: Partial<ReturnType<typeof useAuth>>) {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "u1" },
    vaultUnlocked: true,
    refresh,
    ...over,
  } as unknown as ReturnType<typeof useAuth>);
}

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

describe("VaultBadge", () => {
  it("reflects the unlocked/locked state", () => {
    setAuth({ vaultUnlocked: true });
    const { rerender } = render(<VaultBadge />);
    expect(screen.getByText(/Vault unlocked/)).toBeTruthy();

    setAuth({ vaultUnlocked: false });
    rerender(<VaultBadge />);
    expect(screen.getByText(/Vault locked/)).toBeTruthy();
  });
});

describe("VaultLockedBanner", () => {
  it("renders nothing while unlocked", () => {
    setAuth({ vaultUnlocked: true });
    const { container } = render(<VaultLockedBanner />);
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when signed out", () => {
    setAuth({ user: null, vaultUnlocked: false });
    const { container } = render(<VaultLockedBanner />);
    expect(container.firstChild).toBeNull();
  });

  it("unlocks with the entered password and refreshes the session", async () => {
    setAuth({ vaultUnlocked: false });
    mockApi.vaultUnlock.mockResolvedValue(null);

    render(<VaultLockedBanner />);
    fireEvent.change(screen.getByLabelText("Vault password"), { target: { value: "hunter2hunter2" } });
    fireEvent.click(screen.getByRole("button", { name: /unlock/i }));

    await waitFor(() => expect(mockApi.vaultUnlock).toHaveBeenCalledWith("hunter2hunter2"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("shows a specific message on a 403 wrong password", async () => {
    setAuth({ vaultUnlocked: false });
    mockApi.vaultUnlock.mockRejectedValue(new ApiError(403, "incorrect password"));

    render(<VaultLockedBanner />);
    fireEvent.change(screen.getByLabelText("Vault password"), { target: { value: "wrong-password" } });
    fireEvent.click(screen.getByRole("button", { name: /unlock/i }));

    await waitFor(() => expect(screen.getByText("Incorrect password.")).toBeTruthy());
    expect(refresh).not.toHaveBeenCalled();
  });
});
