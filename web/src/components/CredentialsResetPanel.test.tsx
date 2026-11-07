// @vitest-environment jsdom
//
// PRD #3989 M3: the panel for a run held on credentials_reset. It explains the reset, says
// the run resumes once the owner adds a credential, links to Settings, and offers no Resume.
import { afterEach, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { CredentialsResetPanel, isCredentialsResetHold } from "./CredentialsResetPanel";
import type { Run } from "../lib/api";

afterEach(cleanup);

const held = { id: "r1", kind: "issue", harness: "claude", status: "paused", hold_reason: "credentials_reset" } as unknown as Run;

function renderPanel(run: Run, canSteer = true) {
  return render(
    <MemoryRouter>
      <CredentialsResetPanel run={run} canSteer={canSteer} />
    </MemoryRouter>,
  );
}

it("explains the reset and links to Settings for the owner, with no Resume", () => {
  renderPanel(held);
  expect(screen.getByText("Waiting: credentials were reset")).toBeTruthy();
  expect(screen.getByTestId("credentials-reset-panel").textContent).toMatch(/resumes by itself once the owner adds a credential in Settings/);
  expect(screen.getByRole("link", { name: "Add a credential in Settings" }).getAttribute("href")).toBe("/settings");
  expect(screen.queryByRole("button", { name: /resume/i })).toBeNull();
});

it("shows a non-owner text instead of the Settings link", () => {
  renderPanel(held, false);
  expect(screen.queryByRole("link")).toBeNull();
  expect(screen.getByText(/Only the run.s owner can add the credential/)).toBeTruthy();
});

it("hides on every other state", () => {
  expect(isCredentialsResetHold({ status: "paused", hold_reason: "credential_disabled" })).toBe(false);
  expect(isCredentialsResetHold({ status: "queued", hold_reason: "credentials_reset" })).toBe(false);
  renderPanel({ ...held, hold_reason: null } as Run);
  expect(screen.queryByTestId("credentials-reset-panel")).toBeNull();
});
