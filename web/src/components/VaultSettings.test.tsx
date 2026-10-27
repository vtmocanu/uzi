// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useAuth } from "../auth/AuthContext";
import { api, ApiError } from "../lib/api";
import { CredentialStorageHint, VaultIntroduction, VaultSettings } from "./VaultSettings";
import { onSidebarTokensChanged } from "../lib/sidebarTokens";

vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));
vi.mock("../lib/api", async original => {
  const actual = await original<typeof import("../lib/api")>();
  return { ...actual, api: { ...actual.api, vaultEnable: vi.fn(), vaultDisable: vi.fn(), vaultLock: vi.fn() } };
});
const refresh = vi.fn().mockResolvedValue(undefined);
const reload = vi.fn().mockResolvedValue(undefined);
function auth(over: Partial<ReturnType<typeof useAuth>> = {}) {
  vi.mocked(useAuth).mockReturnValue({
    user: { id: "vault-user" }, vaultMode: "unprotected", vaultPolicy: "optional",
    vaultUnlocked: false, hasPassword: true, refresh, ...over,
  } as ReturnType<typeof useAuth>);
}
beforeEach(() => { localStorage.clear(); auth(); });
afterEach(() => { cleanup(); vi.clearAllMocks(); });

it("requires unlock before disabling; required policy blocks off even after unlock", () => {
  auth({ vaultMode: "protected" });
  const view = render(<VaultSettings onChanged={reload} />);
  expect((screen.getByRole("button", { name: "Disable protection" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText(/Unlock your vault from the banner/)).toBeTruthy();
  expect(api.vaultDisable).not.toHaveBeenCalled();
  auth({ vaultMode: "protected", vaultUnlocked: true, vaultPolicy: "required" });
  view.rerender(<VaultSettings onChanged={reload} />);
  expect((screen.getByRole("button", { name: "Disable protection" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByText(/Off is unavailable because/)).toBeTruthy();
  auth({ vaultMode: "protected", vaultUnlocked: true });
  view.rerender(<VaultSettings onChanged={reload} />);
  expect((screen.getByRole("button", { name: "Disable protection" }) as HTMLButtonElement).disabled).toBe(false);
});

it("rejects a wrong login password without refreshing or claiming protection", async () => {
  vi.mocked(api.vaultEnable).mockRejectedValue(new ApiError(403, "incorrect password"));
  render(<VaultSettings onChanged={reload} />);
  fireEvent.click(screen.getByRole("button", { name: "Enable protection" }));
  fireEvent.change(screen.getByLabelText("Verify login password"), { target: { value: "wrong-login-password" } });
  fireEvent.click(screen.getByRole("button", { name: "Confirm enable protection" }));
  expect(await screen.findByText(/Incorrect login password/)).toBeTruthy();
  expect(api.vaultEnable).toHaveBeenCalledWith("wrong-login-password");
  expect(refresh).not.toHaveBeenCalled();
  expect(reload).not.toHaveBeenCalled();
});

it("enrolls OIDC off users intentionally, validates confirmation and refreshes caches with backup advice", async () => {
  auth({ hasPassword: false, vaultPolicy: "required" });
  vi.mocked(api.vaultEnable).mockResolvedValue({ mode: "protected", rotation_advice: "Rotate credentials exposed in older backups." });
  const changed = vi.fn();
  const unsubscribe = onSidebarTokensChanged(changed);
  render(<VaultSettings onChanged={reload} />);
  expect(screen.getByText(/Your administrator requires credential protection/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Enable protection" }));
  const submit = screen.getByRole("button", { name: "Confirm enable protection" }) as HTMLButtonElement;
  fireEvent.change(screen.getByLabelText("New vault passphrase"), { target: { value: "oidc-passphrase-123" } });
  expect(submit.disabled).toBe(true);
  fireEvent.change(screen.getByLabelText("Confirm new vault passphrase"), { target: { value: "oidc-passphrase-123" } });
  fireEvent.click(submit);
  expect(await screen.findByText("Rotate credentials exposed in older backups.")).toBeTruthy();
  await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  expect(api.vaultEnable).toHaveBeenCalledWith("oidc-passphrase-123");
  expect(refresh).toHaveBeenCalledWith(true);
  expect(changed).toHaveBeenCalledTimes(1);
  unsubscribe();
});

it("disables unlocked optional protection and refreshes credentials", async () => {
  auth({ vaultMode: "protected", vaultUnlocked: true });
  vi.mocked(api.vaultDisable).mockResolvedValue({ mode: "unprotected" });
  render(<VaultSettings onChanged={reload} />);
  fireEvent.click(screen.getByRole("button", { name: "Disable protection" }));
  await waitFor(() => expect(reload).toHaveBeenCalledTimes(1));
  expect(api.vaultDisable).toHaveBeenCalledTimes(1);
  expect(refresh).toHaveBeenCalledWith(true);
});

it("dismisses the off introduction across mounts without dismissing required enrollment", () => {
  auth({ vaultPolicy: "required" });
  const view = render(<><VaultIntroduction /><VaultSettings onChanged={reload} /></>);
  expect(screen.getByText("Choose credential protection")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Dismiss vault introduction" }));
  view.unmount();
  render(<><VaultIntroduction /><VaultSettings onChanged={reload} /></>);
  expect(screen.queryByText("Choose credential protection")).toBeNull();
  expect(screen.getByRole("button", { name: "Enable protection" })).toBeTruthy();
  expect(screen.getByText(/Your administrator requires credential protection/)).toBeTruthy();
});

it.each(["unprotected", "protected"] as const)("keeps threat explanation and guide visible in %s mode", vaultMode => {
  auth({ vaultMode });
  render(<VaultSettings onChanged={reload} />);
  const text = screen.getByText(/TL;DR:/).textContent;
  for (const term of ["Claude, OpenAI and Codex", "database together with the server key", "release or restart", "forge bot tokens"]) expect(text).toContain(term);
  expect(screen.getByRole("link", { name: "Learn more" }).getAttribute("href")).toBe("/docs/anthropic-token#vault-choice");
});

it.each([
  ["unprotected", true, "Encrypted with the server key"],
  ["protected", true, "Encrypted with your login password"],
  ["protected", false, "Encrypted with your vault passphrase"],
] as const)("describes %s storage truthfully (password=%s)", (vaultMode, hasPassword, expected) => {
  auth({ vaultMode, hasPassword });
  render(<CredentialStorageHint />);
  expect(screen.getByText(new RegExp(expected))).toBeTruthy();
});
