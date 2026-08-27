// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { VaultBadge, VaultLockedBanner } from "./VaultControls";
import { api, ApiError } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../lib/api")>();
  return { ...actual, api: { vaultUnlock: vi.fn(), vaultLock: vi.fn(), vaultCreatePassphrase: vi.fn() } };
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

  it("adds a re-login hint after repeated failures (dead-end recovery)", async () => {
    setAuth({ vaultUnlocked: false });
    mockApi.vaultUnlock.mockRejectedValue(new ApiError(403, "incorrect password"));

    render(<VaultLockedBanner />);
    const submit = () => {
      fireEvent.change(screen.getByLabelText("Vault password"), { target: { value: "believed-correct" } });
      fireEvent.click(screen.getByRole("button", { name: /unlock/i }));
    };

    // First failure: plain message, no hint.
    submit();
    await waitFor(() => expect(screen.getByText("Incorrect password.")).toBeTruthy());
    expect(screen.queryByText(/Sign out and back in/i)).toBeNull();

    // Second consecutive failure: the recovery hint appears.
    submit();
    await waitFor(() => expect(screen.getByText(/Sign out and back in to re-create your vault/i)).toBeTruthy());
  });
});

// PRD #45 (Decision 6): the locked banner dispatches on has_password + vault.exists.
describe("VaultLockedBanner passwordless (SSO) dispatch", () => {
  it("shows the passphrase-create dialog for a passwordless user with no vault", () => {
    setAuth({ vaultUnlocked: false, hasPassword: false, vaultExists: false });
    render(<VaultLockedBanner />);
    expect(screen.getByText(/Set a vault passphrase/)).toBeTruthy();
    expect(screen.getByLabelText("Vault passphrase")).toBeTruthy();
    expect(screen.getByLabelText("Confirm vault passphrase")).toBeTruthy();
    expect(screen.queryByLabelText("Vault password")).toBeNull();
  });

  it("shows the unlock banner with passphrase wording for a passwordless user WITH a vault", () => {
    setAuth({ vaultUnlocked: false, hasPassword: false, vaultExists: true });
    render(<VaultLockedBanner />);
    expect(screen.getByLabelText("Vault passphrase")).toBeTruthy();
    expect(screen.getByRole("button", { name: /unlock/i })).toBeTruthy();
    expect(screen.queryByText(/Set a vault passphrase/)).toBeNull();
  });

  it("keeps the password unlock banner for a password user", () => {
    setAuth({ vaultUnlocked: false, hasPassword: true, vaultExists: true });
    render(<VaultLockedBanner />);
    expect(screen.getByLabelText("Vault password")).toBeTruthy();
    expect(screen.queryByText(/Set a vault passphrase/)).toBeNull();
  });

  it("creates the vault from a valid passphrase and refreshes the session", async () => {
    setAuth({ vaultUnlocked: false, hasPassword: false, vaultExists: false });
    mockApi.vaultCreatePassphrase.mockResolvedValue(null);

    render(<VaultLockedBanner />);
    fireEvent.change(screen.getByLabelText("Vault passphrase"), { target: { value: "a-strong-passphrase" } });
    fireEvent.change(screen.getByLabelText("Confirm vault passphrase"), { target: { value: "a-strong-passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: /set passphrase/i }));

    await waitFor(() => expect(mockApi.vaultCreatePassphrase).toHaveBeenCalledWith("a-strong-passphrase"));
    await waitFor(() => expect(refresh).toHaveBeenCalled());
  });

  it("blocks submit on a short passphrase and on a mismatch", () => {
    setAuth({ vaultUnlocked: false, hasPassword: false, vaultExists: false });
    render(<VaultLockedBanner />);
    const button = () => screen.getByRole("button", { name: /set passphrase/i }) as HTMLButtonElement;

    // Too short.
    fireEvent.change(screen.getByLabelText("Vault passphrase"), { target: { value: "short" } });
    fireEvent.change(screen.getByLabelText("Confirm vault passphrase"), { target: { value: "short" } });
    expect(button().disabled).toBe(true);

    // Long enough but mismatched.
    fireEvent.change(screen.getByLabelText("Vault passphrase"), { target: { value: "a-strong-passphrase" } });
    fireEvent.change(screen.getByLabelText("Confirm vault passphrase"), { target: { value: "different-passphrase" } });
    expect(button().disabled).toBe(true);
    expect(screen.getByText(/do not match/i)).toBeTruthy();

    // Matching + long enough → enabled, and nothing was submitted along the way.
    fireEvent.change(screen.getByLabelText("Confirm vault passphrase"), { target: { value: "a-strong-passphrase" } });
    expect(button().disabled).toBe(false);
    expect(mockApi.vaultCreatePassphrase).not.toHaveBeenCalled();
  });
});
