import "../index.css";
import { afterEach, expect, it, vi } from "vitest";
import { page, userEvent } from "vitest/browser";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { api, type Run, type SecretMeta } from "../lib/api";
import { SwitchTokenAction } from "./RunCredentialOverride";

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

it("returns keyboard focus after success and forwards the server result", async () => {
  await page.viewport(390, 900);
  const returned = { ...run, credential_switch: "requested" } as Run;
  vi.mocked(api.listSecrets).mockResolvedValue({ secrets: choices });
  vi.mocked(api.setRunCredential).mockResolvedValue({ run: returned });
  const onSwitched = vi.fn();
  render(<SwitchTokenAction run={run} canSteer onSwitched={onSwitched} />);
  const trigger = screen.getByRole("button", { name: "Switch credential" });
  fireEvent.click(trigger);
  const select = screen.getByLabelText("Codex credential") as HTMLSelectElement;
  await waitFor(() => expect(select.disabled).toBe(false));
  await userEvent.selectOptions(select, "credential-b");
  const confirm = screen.getByRole("button", { name: "Switch to this credential" });
  confirm.focus();
  await userEvent.type(page.elementLocator(confirm), "{Enter}", { skipClick: true });
  await waitFor(() => expect(onSwitched).toHaveBeenCalledWith(returned));
  expect(api.setRunCredential).toHaveBeenCalledExactlyOnceWith(run.id, { mode: "pinned", secret_id: "credential-b" });
  expect(document.activeElement).toBe(trigger);
});
