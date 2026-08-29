// @vitest-environment jsdom
import { afterEach, describe, it, expect } from "vitest";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { RepoSetupChip, setupTone } from "./RepoSetupChip";
import type { Repo } from "../lib/api";

afterEach(cleanup);

// Minimal Repo builder: every capability off by default (the design default), so a
// test opts INTO the flags it exercises. The chip only reads five booleans, but the
// component takes a whole Repo, so the rest is filled with inert values.
function repo(over: Partial<Repo> = {}): Repo {
  return {
    id: "r1",
    connection_id: "conn-1",
    forge_project_id: 1,
    path_with_namespace: "acme/widgets",
    web_url: "https://example.com/acme/widgets",
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
    ...over,
  };
}

// The chip's visual pill is a Badge span whose className carries the tone tokens.
// Grab it off the trigger button so a tone assertion reads the ACTUAL rendered
// class, not a reverse-engineered one.
function chipBadgeClass(name: string): string {
  const btn = screen.getByRole("button", { name });
  const badge = btn.querySelector("span");
  if (!badge) throw new Error("chip has no badge span");
  return badge.className;
}

describe("setupTone (pure helper)", () => {
  it("all four off → neutral 'Setup'", () => {
    expect(setupTone(repo())).toEqual({ tone: "neutral", label: "Setup" });
  });

  it("all four on → ok 'Ready'", () => {
    expect(
      setupTone(
        repo({
          repo_skills_enabled: true,
          repo_claudemd_enabled: true,
          repo_devbox_opt_in: true,
          docker_allowlisted: true,
        }),
      ),
    ).toEqual({ tone: "ok", label: "Ready" });
  });

  it("docker_blocked wins over everything → info", () => {
    // Even with all four on, an active block escalates to info (priority 1).
    expect(
      setupTone(
        repo({
          repo_skills_enabled: true,
          repo_claudemd_enabled: true,
          repo_devbox_opt_in: true,
          docker_allowlisted: true,
          docker_blocked: true,
        }),
      ),
    ).toEqual({ tone: "info", label: "Setup" });
  });

  it("three-of-four on is still neutral, not ok", () => {
    expect(
      setupTone(
        repo({
          repo_skills_enabled: true,
          repo_claudemd_enabled: true,
          repo_devbox_opt_in: true,
          docker_allowlisted: false,
        }),
      ).tone,
    ).toBe("neutral");
  });
});

describe("RepoSetupChip — rendered tones", () => {
  it("(a) all off → neutral 'Setup' pill, and NO warning/danger tone", () => {
    render(<RepoSetupChip repo={repo()} />);
    const cls = chipBadgeClass("Setup");
    // POSITIVE: the neutral tone tokens ARE present…
    expect(cls).toContain("text-neutral-fg");
    // …PAIRED with the negative, so the "no red/amber" check is not vacuous.
    expect(cls).not.toContain("text-danger");
    expect(cls).not.toContain("text-warn");
  });

  it("(b) all on → ok 'Ready' pill, and NO warning/danger tone", () => {
    render(
      <RepoSetupChip
        repo={repo({
          repo_skills_enabled: true,
          repo_claudemd_enabled: true,
          repo_devbox_opt_in: true,
          docker_allowlisted: true,
        })}
      />,
    );
    const cls = chipBadgeClass("Ready");
    expect(cls).toContain("text-ok");
    expect(cls).not.toContain("text-danger");
    expect(cls).not.toContain("text-warn");
  });

  it("(c) docker_blocked → info 'Setup' pill, and NO warning/danger tone", () => {
    render(<RepoSetupChip repo={repo({ docker_blocked: true })} />);
    const cls = chipBadgeClass("Setup");
    expect(cls).toContain("text-info");
    expect(cls).not.toContain("text-danger");
    expect(cls).not.toContain("text-warn");
  });
});

describe("RepoSetupChip — popover", () => {
  it("opens on click and lists the four capabilities, where each is set, and the footer", () => {
    render(<RepoSetupChip repo={repo()} />);
    const tooltip = screen.getByRole("tooltip");

    // Click OPENS (never toggles).
    expect(tooltip.getAttribute("data-open")).toBe("false");
    fireEvent.click(screen.getByRole("button", { name: "Setup" }));
    expect(tooltip.getAttribute("data-open")).toBe("true");
    // A second click keeps it open rather than closing it under the pointer.
    fireEvent.click(screen.getByRole("button", { name: "Setup" }));
    expect(tooltip.getAttribute("data-open")).toBe("true");

    const panel = within(tooltip);
    // The four capability names (getByText throws if absent — presence is the assertion).
    expect(panel.getByText("Repo skills")).toBeTruthy();
    expect(panel.getByText("Repo instructions")).toBeTruthy();
    expect(panel.getByText("Tool profile")).toBeTruthy();
    expect(panel.getByText("Docker workers")).toBeTruthy();
    // Where each is set.
    expect(panel.getAllByText("Trusted repo settings")).toHaveLength(2);
    expect(panel.getByText("Tools")).toBeTruthy();
    expect(panel.getByText("Admin → Docker workers")).toBeTruthy();
    // Footer copy, verbatim.
    expect(
      panel.getByText("Defaults are off on purpose; each one widens what an agent may do."),
    ).toBeTruthy();
  });

  it("tags the Docker workers capability as admin-only", () => {
    render(<RepoSetupChip repo={repo()} />);
    const dockerRow = screen.getByText("Docker workers").closest("li");
    expect(dockerRow).not.toBeNull();
    expect(within(dockerRow as HTMLElement).getByText("admin")).toBeTruthy();
  });

  it("shows the queued-run note on the Docker row only when actually blocked", () => {
    const { rerender } = render(<RepoSetupChip repo={repo()} />);
    expect(screen.queryByText(/A queued run is waiting/)).toBeNull();
    rerender(<RepoSetupChip repo={repo({ docker_blocked: true })} />);
    expect(screen.getByText(/A queued run is waiting/)).toBeTruthy();
  });

  it("uses a real tooltip element, not a native title attribute", () => {
    render(<RepoSetupChip repo={repo()} />);
    const tooltip = screen.getByRole("tooltip");
    // The panel is a real, describedby-linked element…
    const btn = screen.getByRole("button", { name: "Setup" });
    expect(btn.getAttribute("aria-describedby")).toBe(tooltip.id);
    // …and NOTHING leans on a native title tooltip (the blind-instrument trap).
    expect(btn.getAttribute("title")).toBeNull();
    expect(tooltip.getAttribute("title")).toBeNull();
  });
});
