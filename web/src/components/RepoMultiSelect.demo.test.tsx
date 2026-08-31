// @vitest-environment jsdom
//
// PRD #886 M5 — attribute-aware demo-mode masking test for a repo_path aria-label.
//
// RepoMultiSelect renders each repo's path_with_namespace into BOTH a visible chip /
// checkbox label AND the checkbox `aria-label` (an attribute channel that textContent /
// queryByText are blind to — see .claude/rules/web.md). This proves the raw value with
// demo OFF and the masked value with demo ON, in both channels, asserting the attribute
// explicitly via getByLabelText (per M5).
import { afterEach, beforeEach, describe, it, expect } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { RepoMultiSelect } from "./RepoMultiSelect";
import type { Repo } from "../lib/api";

const repo: Repo = {
  id: "repo-uzi",
  connection_id: "conn-1",
  forge_project_id: 1,
  path_with_namespace: "vtmocanu/uzi",
  web_url: "https://gitlab.example.com/vtmocanu/uzi",
  default_branch: "main",
  enabled: true,
  repo_skills_enabled: false,
  repo_claudemd_enabled: false,
  repo_devbox_opt_in: false,
  repo_fold_improve_uzi_backlog: false,
  pipeline: null,
  guardrail_blocked: false,
  docker_allowlisted: false,
  docker_blocked: false,
};

function renderPicker() {
  return render(
    <RepoMultiSelect repos={[repo]} selected={[repo.id]} onChange={() => {}} />,
  );
}

beforeEach(() => {
  window.localStorage.clear();
});

afterEach(() => {
  cleanup();
  window.localStorage.clear();
});

describe("RepoMultiSelect — repo_path aria-label masking (PRD #886 M5)", () => {
  it("demo OFF: renders the real path in the aria-label AND the visible text", () => {
    renderPicker();

    // Attribute channel: the checkbox aria-label carries the real path.
    const checkbox = screen.getByLabelText("vtmocanu/uzi");
    expect(checkbox.getAttribute("aria-label")).toBe("vtmocanu/uzi");
    // Visible channel: the real path renders as text (checkbox row + selected chip).
    expect(screen.queryAllByText("vtmocanu/uzi").length).toBeGreaterThan(0);
  });

  it("demo ON: masks the path in the aria-label to demo/uzi and the real path is absent from BOTH channels", () => {
    window.localStorage.setItem("uzi_demo_mode", "1");
    renderPicker();

    // Attribute channel: the checkbox aria-label is the masked value, explicitly asserted.
    const checkbox = screen.getByLabelText("demo/uzi");
    expect(checkbox.getAttribute("aria-label")).toBe("demo/uzi");
    // The real owner/namespace must not survive in the attribute channel.
    expect(checkbox.getAttribute("aria-label")).not.toContain("vtmocanu");

    // Real string absent from the ATTRIBUTE channel (no element labelled by the raw path,
    // incl. the "Remove <path>" chip button).
    expect(screen.queryByLabelText("vtmocanu/uzi")).toBeNull();
    expect(screen.queryByLabelText("Remove vtmocanu/uzi")).toBeNull();
    // Real string absent from the VISIBLE channel (chip + checkbox text).
    expect(screen.queryByText("vtmocanu/uzi")).toBeNull();
  });
});
