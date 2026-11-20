import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { api, type Run, type SecretMeta } from "../lib/api";
import { ApiError } from "../lib/apiError";
import { RunCredentialOverride, SwitchTokenAction } from "./RunCredentialOverride";
import { RunCredential } from "./RunCredential";

vi.mock("../lib/api", () => ({ api: { listSecrets: vi.fn(), setRunCredential: vi.fn() } }));
const originalTheme = document.documentElement.getAttribute("data-theme");
afterEach(() => {
  cleanup();
  vi.resetAllMocks();
  if (originalTheme === null) document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", originalTheme);
});
const run = {
  id: "browser-run", harness: "codex", kind: "issue", status: "running",
  codex_secret_id: "credential-a", codex_secret_label: "Account A",
} as Run;
const choices = [
  { id: "credential-a", kind: "codex_auth", codex_status: "linked" },
  { id: "credential-b", kind: "openai_api_key", codex_status: "static" },
].map(s => ({
  ...s, label: "A very long legacy credential label " + "x".repeat(64),
  enabled: true, is_default: false, auto_eligible: false, disabled_at: null,
  created_at: "", updated_at: "",
})) as SecretMeta[];

it.each([
  ["ember", 320], ["ember", 390], ["dawn", 320], ["dawn", 390],
] as const)("fits long credential labels in %s at %ipx and supports keyboard cancel", async (theme, width) => {
  await page.viewport(width, 900);
  document.documentElement.setAttribute("data-theme", theme);
  vi.mocked(api.listSecrets).mockResolvedValue({ secrets: choices });
  const { container } = render(<div className="p-3"><SwitchTokenAction run={run} canSteer /></div>);
  const trigger = screen.getByRole("button", { name: "Switch credential" });
  trigger.focus();
  await userEvent.type(page.elementLocator(trigger), "{Enter}", { skipClick: true });
  const select = screen.getByLabelText("Codex credential") as HTMLSelectElement;
  await waitFor(() => expect(select.disabled).toBe(false));
  await userEvent.tab();
  expect(document.activeElement).toBe(select);
  expect(select.tagName).toBe("SELECT");
  expect(select.getAttribute("aria-describedby")).toBeTruthy();
  // Native browser selectOption exercises the ID-valued select without opening a platform popup.
  await userEvent.selectOptions(select, "credential-b");
  expect(select.value).toBe("credential-b");
  const confirm = screen.getByRole("button", { name: "Switch to this credential" }) as HTMLButtonElement;
  expect(confirm.disabled).toBe(false);
  await userEvent.tab();
  expect(document.activeElement).toBe(confirm);
  await userEvent.tab();
  const cancel = screen.getByRole("button", { name: "Cancel" });
  expect(document.activeElement).toBe(cancel);
  for (const element of container.querySelectorAll("select, button")) {
    expect(element.getBoundingClientRect().right).toBeLessThanOrEqual(width);
  }
  expect(document.documentElement.scrollWidth).toBeLessThanOrEqual(width);
  await userEvent.type(page.elementLocator(cancel), "{Enter}", { skipClick: true });
  expect(document.activeElement).toBe(trigger);
  expect(api.setRunCredential).not.toHaveBeenCalled();
});

it.each([
  ["credential-a", "Codex subscription"],
  ["credential-b", "OpenAI API key"],
] as const)("pins an explicit %s (%s), returns focus and displays only committed bindings", async (targetId, authKind) => {
  await page.viewport(390, 900);
  const secrets = choices.map((s, i) => ({ ...s, label: `Account ${i === 0 ? "A" : "B"}` + String.fromCodePoint(0x202e) }));
  const target = secrets.find(s => s.id === targetId)!;
  const current = secrets.find(s => s.id !== targetId)!;
  const currentLabel = current.id === "credential-a" ? "Account A" : "Account B";
  const targetLabel = targetId === "credential-a" ? "Account A" : "Account B";
  const committed = { ...run, codex_secret_id: current.id, codex_secret_label: current.label, credential_switch: null } as Run;
  const returned = { ...committed, credential_switch: "requested" } as Run;
  vi.mocked(api.listSecrets).mockResolvedValue({ secrets });
  vi.mocked(api.setRunCredential).mockResolvedValue({ run: returned });
  const onSwitched = vi.fn();
  function Header({ value }: { value: Run }) {
    return <MemoryRouter><RunCredential run={value} /><RunCredentialOverride run={value} />
      <SwitchTokenAction run={value} canSteer onSwitched={onSwitched} /></MemoryRouter>;
  }
  const { container, rerender } = render(<Header value={committed} />);
  expect(screen.getByText(currentLabel)).toBeTruthy();
  const trigger = screen.getByRole("button", { name: "Switch credential" });
  await userEvent.click(trigger);
  const select = screen.getByLabelText("Codex credential") as HTMLSelectElement;
  await waitFor(() => expect(select.disabled).toBe(false));
  expect(select.value).toBe(current.id);
  expect((screen.getByRole("button", { name: "Switch to this credential" }) as HTMLButtonElement).disabled).toBe(true);
  expect(screen.getByRole("option", { name: new RegExp(`${targetLabel} — ${authKind}`) })).toBeTruthy();
  await userEvent.selectOptions(select, targetId);
  expect(select.title).toBe(targetLabel);
  expect(container.innerHTML).not.toContain(String.fromCodePoint(0x202e));
  const confirm = screen.getByRole("button", { name: "Switch to this credential" });
  confirm.focus();
  await userEvent.type(page.elementLocator(confirm), "{Enter}", { skipClick: true });
  await waitFor(() => expect(onSwitched).toHaveBeenCalledWith(returned));
  expect(api.setRunCredential).toHaveBeenCalledExactlyOnceWith(run.id, { mode: "pinned", secret_id: targetId });
  // onSwitched runs before the closed, not-busy render commits; focus returns after that commit.
  await waitFor(() => expect(document.activeElement).toBe(trigger));
  expect(screen.getByText(currentLabel)).toBeTruthy();
  rerender(<Header value={returned} />);
  expect(screen.getByText("switch pending")).toBeTruthy();
  expect(screen.getByText(currentLabel)).toBeTruthy();
  expect(screen.queryByText(targetLabel)).toBeNull();
  rerender(<Header value={{ ...committed, codex_secret_id: targetId, codex_secret_label: target.label }} />);
  expect(screen.getByText(targetLabel)).toBeTruthy();
  expect(screen.queryByText(currentLabel)).toBeNull();
  expect(screen.queryByText("switch pending")).toBeNull();
  expect(container.innerHTML).not.toContain(String.fromCodePoint(0x202e));
});

it.each([
  [409, "Worker does not support switching"],
  [422, "Queued runs cannot switch credentials"],
  [409, "Policy refuses this switch"],
] as const)("shows server refusal %s: %s and preserves the selection", async (status, message) => {
  await page.viewport(390, 900);
  vi.mocked(api.listSecrets).mockResolvedValue({ secrets: choices });
  vi.mocked(api.setRunCredential).mockRejectedValue(new ApiError(status, message));
  const onSwitched = vi.fn();
  render(<SwitchTokenAction run={run} canSteer onSwitched={onSwitched} />);
  await userEvent.click(screen.getByRole("button", { name: "Switch credential" }));
  const select = screen.getByLabelText("Codex credential") as HTMLSelectElement;
  await waitFor(() => expect(select.disabled).toBe(false));
  await userEvent.selectOptions(select, "credential-b");
  await userEvent.click(screen.getByRole("button", { name: "Switch to this credential" }));
  await screen.findByText(message);
  expect(api.setRunCredential).toHaveBeenCalledExactlyOnceWith(run.id, { mode: "pinned", secret_id: "credential-b" });
  expect(onSwitched).not.toHaveBeenCalled();
  expect(select.value).toBe("credential-b");
  expect(select.disabled).toBe(false);
  expect((screen.getByRole("button", { name: "Cancel" }) as HTMLButtonElement).disabled).toBe(false);
});

it("hides the nonowner switch without fetching credentials", async () => {
  vi.mocked(api.listSecrets).mockResolvedValue({ secrets: choices });
  const { rerender } = render(<SwitchTokenAction run={run} canSteer={false} />);
  expect(screen.queryByRole("button", { name: "Switch credential" })).toBeNull();
  expect(screen.queryByLabelText("Codex credential")).toBeNull();
  expect(api.listSecrets).not.toHaveBeenCalled();
  expect(api.setRunCredential).not.toHaveBeenCalled();
  // The same run exposes the control and loads choices for its owner.
  rerender(<SwitchTokenAction run={run} canSteer />);
  await userEvent.click(screen.getByRole("button", { name: "Switch credential" }));
  await waitFor(() => expect((screen.getByLabelText("Codex credential") as HTMLSelectElement).disabled).toBe(false));
  expect(api.listSecrets).toHaveBeenCalledTimes(1);
});
