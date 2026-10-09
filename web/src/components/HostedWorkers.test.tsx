// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { HostedWorkers } from "./HostedWorkers";
import { api, ApiError, type User, type Worker } from "../lib/api";
import { useAuth } from "../auth/AuthContext";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
      setEphemeralWorkersEnabled: vi.fn(),
    },
  };
});
vi.mock("../auth/AuthContext", () => ({ useAuth: vi.fn() }));

const mockApi = vi.mocked(api);
const updateUser = vi.fn();
const confirmedUser: User = {
  id: "u1", email: "alice@example.com", display_name: "Alice",
  is_admin: false, is_active: true, autopilot_enabled: false,
  judge_enabled: false, ci_autofix_enabled: null, attribution_enabled: true,
  ephemeral_workers_enabled: false, ephemeral_docker_enabled: false,
  wait_on_limit: false, notify_early_limit_reset: true,
  judge_anthropic_secret_id: null, judge_anthropic_secret_label: null,
  judge_anthropic_bind_mode: "default", created_at: "2026-01-01T00:00:00Z",
  last_login: null,
};

// HostedWorkers reads only `user` and `updateUser()` from the auth context, so a light
// stub (the AdminUsers.test.tsx convention) is enough; the cast keeps it type-safe
// without spelling out the whole AuthState.
function mockUser(over: Partial<User> = {}) {
  vi.mocked(useAuth).mockReturnValue({
    user: { ...confirmedUser, ...over },
    updateUser,
  } as unknown as ReturnType<typeof useAuth>);
}

beforeEach(() => {
  updateUser.mockReset();
  mockUser();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

const provisioned: Worker = {
  id: "w-new",
  name: "base (M)",
  status: "offline",
  kind: "hosted",
  hosted_size: "m",
  busy: false,
  active_runs: 0,
  max_concurrent_runs: null,
  active_cross_checks: 0,
  max_cross_check_slots: null,
  template_declared: "base",
  template_reported: null,
  version: null,
  upgrade_status: "unknown",
  upgrade_detail: null,
  upgrade_target: "0.11.7",
  upgrade_blocking_container: null,
  upgrade_blocking_reason: null,
  upgrade_last_exit_code: null,
  last_heartbeat_at: null,
  created_at: "2026-07-16T00:00:00Z",
  stats_cpu_pct: null,
  stats_mem_bytes: null,
  stats_mem_limit_bytes: null,
  stats_source: null,
  stats_disk_nix_bytes: null,
  stats_disk_nix_total_bytes: null,
  stats_disk_data_bytes: null,
  stats_disk_data_total_bytes: null,
  stats_disk_dind_bytes: null,
  stats_disk_dind_total_bytes: null,
  stats_disk_dind_inodes: null,
  stats_disk_dind_total_inodes: null,
  anthropic_secret_id: null,
  anthropic_secret_label: null,
  anthropic_bind_mode: "default",
  draining_since: null,
};

function renderCard(hostedCount = 0, onProvisioned = vi.fn()) {
  // onShowWorkers is REQUIRED (M3); a stub keeps every existing test type-clean.
  render(<HostedWorkers hostedCount={hostedCount} onProvisioned={onProvisioned} onShowWorkers={() => {}} />);
  return onProvisioned;
}

// Anchored at the start so it matches the submit button ("Provision" / "Provisioning…")
// but NOT the at-quota "delete one to provision another" link, which also contains
// "provision" (M3).
const provisionButton = () => screen.findByRole("button", { name: /^Provision/ });

describe("HostedWorkers visibility (the card is hidden, never disabled-with-excuse)", () => {
  it("renders nothing while the config is still in flight", () => {
    mockApi.hostedConfig.mockReturnValue(new Promise(() => {})); // never settles
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    // No skeleton either: a placeholder would promise a card that may never come.
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when hosting is disabled on the instance", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 2, ephemeral_enabled: false });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    // Disabled beats a non-zero quota: a client reading enabled:false renders nothing
    // hosted regardless of the number beside it.
    expect(container.firstChild).toBeNull();
  });

  it("fails closed when the config read rejects — no card, no error banner", async () => {
    mockApi.hostedConfig.mockRejectedValue(new ApiError(500, "internal error"));
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    expect(container.firstChild).toBeNull();
    // A capability probe that blipped must not shout at a user who may not even have
    // the feature.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides the form when the quota is 0 — that is policy, not a full quota", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: false });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    // No "0 of 0 used" and no disabled button: deleting a worker would not help, so
    // offering the affordance would be a lie. The user's existing hosted rows are the
    // page's business and keep rendering — this card is only the provision path.
    expect(container.firstChild).toBeNull();
  });

  it("reads the policy once and never polls it", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(0);
    await provisionButton();
    expect(mockApi.hostedConfig).toHaveBeenCalledTimes(1);
  });
});

describe("HostedWorkers onAvailability (D8 — tells the page whether the hosted path is on)", () => {
  it("fires once with { manual: true } when hosting is enabled and quota > 0", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    const onAvailability = vi.fn();
    render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />);
    await waitFor(() => expect(onAvailability).toHaveBeenCalledWith({ manual: true }));
    expect(onAvailability).toHaveBeenCalledTimes(1);
  });

  it("fires with { manual: false } when enabled but the quota is 0", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true });
    const onAvailability = vi.fn();
    render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />);
    await waitFor(() => expect(onAvailability).toHaveBeenCalledWith({ manual: false }));
    expect(onAvailability).toHaveBeenCalledTimes(1);
  });

  it("fires with { manual: false } when hosting is disabled", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 5, ephemeral_enabled: false });
    const onAvailability = vi.fn();
    render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />);
    await waitFor(() => expect(onAvailability).toHaveBeenCalledWith({ manual: false }));
    expect(onAvailability).toHaveBeenCalledTimes(1);
  });

  it("fires with { manual: false } when the config read rejects (fail closed)", async () => {
    mockApi.hostedConfig.mockRejectedValue(new ApiError(500, "internal error"));
    const onAvailability = vi.fn();
    render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />);
    await waitFor(() => expect(onAvailability).toHaveBeenCalledWith({ manual: false }));
    expect(onAvailability).toHaveBeenCalledTimes(1);
  });

  it("never re-fires on a rerender (exactly once per mount)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    const onAvailability = vi.fn();
    const { rerender } = render(
      <HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />,
    );
    await waitFor(() => expect(onAvailability).toHaveBeenCalledTimes(1));
    // Force a rerender (a new prop identity) — the one-shot fetch effect must not re-run and
    // the latch must hold, so the count stays 1.
    rerender(<HostedWorkers hostedCount={1} onProvisioned={vi.fn()} onShowWorkers={() => {}} onAvailability={onAvailability} />);
    await Promise.resolve();
    expect(onAvailability).toHaveBeenCalledTimes(1);
  });
});

describe("HostedWorkers quota states", () => {
  it("counts the user's hosted workers against the quota and allows a provision", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(1);
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    expect((await provisionButton()).hasAttribute("disabled")).toBe(false);
  });

  it("disables the submit at quota and offers a link back to Your workers (M3)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(2);
    expect(await screen.findByText(/2 of 2 used/)).toBeTruthy();
    // The escape hatch is now a link-styled BUTTON, not plain text (M3).
    expect(await screen.findByRole("button", { name: "delete one to provision another" })).toBeTruthy();
    expect((await provisionButton()).hasAttribute("disabled")).toBe(true);
  });

  it("does not render the delete-one link below quota", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(1);
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    expect(screen.queryByRole("button", { name: "delete one to provision another" })).toBeNull();
  });

  it("calls onShowWorkers when the at-quota delete-one link is clicked (M3)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    const onShowWorkers = vi.fn();
    render(
      <HostedWorkers hostedCount={2} onProvisioned={vi.fn()} onShowWorkers={onShowWorkers} />,
    );
    fireEvent.click(await screen.findByRole("button", { name: "delete one to provision another" }));
    expect(onShowWorkers).toHaveBeenCalledTimes(1);
  });

  it("never calls the endpoint while at quota", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(2);
    fireEvent.click(await provisionButton());
    expect(mockApi.provisionHostedWorker).not.toHaveBeenCalled();
  });
});

describe("HostedWorkers provisioning", () => {
  it("sends the selected template and the LOWERCASE size, then refreshes the fleet", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    const onProvisioned = renderCard(0);

    fireEvent.change(await screen.findByLabelText("Hosted worker template"), { target: { value: "jvm" } });
    fireEvent.change(screen.getByLabelText("Hosted worker size"), { target: { value: "m" } });
    fireEvent.click(await provisionButton());

    // "M" is what the user reads; "m" is the only thing the api accepts
    // (workersize.Valid("M") is false), so the label must never become the value.
    // docker defaults false (the box is unchecked) and is sent explicitly.
    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalledWith("jvm", "m", false));
    expect(onProvisioned).toHaveBeenCalled();
  });

  it("sends docker=true when the Docker-capable box is ticked (PRD #83 M3)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderCard(0);

    fireEvent.change(await screen.findByLabelText("Hosted worker template"), { target: { value: "jvm" } });
    fireEvent.change(screen.getByLabelText("Hosted worker size"), { target: { value: "m" } });
    fireEvent.click(screen.getByLabelText("Docker-capable worker"));
    fireEvent.click(await provisionButton());

    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalledWith("jvm", "m", true));
  });

  it("renders NO token after a successful provision", async () => {
    // The copy-paste guard for the sibling createWorker card on this page, which shows
    // a prominent one-time token. A hosted worker's token goes to the controller and
    // never to the browser — the response cannot even carry one — so anything
    // token-shaped in this DOM means someone copied the wrong flow.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);

    fireEvent.click(await provisionButton());
    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalled());

    // Token-SHAPED, not the word "token": the card's own copy says there is no join
    // token to copy, which is the point of saying it. Each assertion below
    // independently catches a copied createWorker card — it renders the secret in a
    // <code> block with a Copy button under a "Join token for …" heading.
    expect(screen.queryByText(/join token for/i)).toBeNull();
    expect(screen.queryByText(/uzi_wk_/)).toBeNull();
    expect(screen.queryByRole("button", { name: /copy/i })).toBeNull();
    expect(container.querySelector("code")).toBeNull();
  });

  it("hands the provisioned worker to the page, which owns the announcement", async () => {
    // The notice is deliberately NOT this component's: one slot serves provisioning and
    // deleting, a delete has to be able to replace a provision's message, and deletes
    // are the page's. So this component's job ends at reporting what the server made —
    // and it reports the SERVER's worker, not a guess, since the server names it.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    const onProvisioned = renderCard(0);
    fireEvent.click(await provisionButton());

    await waitFor(() => expect(onProvisioned).toHaveBeenCalledWith(provisioned));
    // And it renders no confirmation of its own — that would be a second slot.
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("preselects the large hosted size when provisioning without a size change", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderCard(0);
    expect((await screen.findByLabelText("Hosted worker size") as HTMLSelectElement).value).toBe("l");
    fireEvent.click(await provisionButton());
    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalledWith("base", "l", false));
  });

  it("resets a chosen size to large after successful provisioning", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderCard(0);
    fireEvent.change(await screen.findByLabelText("Hosted worker size"), { target: { value: "s" } });
    fireEvent.click(await provisionButton());
    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalledWith("base", "s", false));
    await waitFor(() => expect((screen.getByLabelText("Hosted worker size") as HTMLSelectElement).value).toBe("l"));
  });

  it("shows the 409 quota refusal verbatim (the server's words, not ours)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    // The client-side gate is a hint, not the rule: a stale count gets here and the
    // server's advisory-locked count is what refuses. Pinned verbatim because this
    // exact string is what the user reads, and it names the QUOTA, never their count.
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(409, "hosted worker quota reached (2)"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("hosted worker quota reached (2)")).toBeTruthy();
  });

  it("shows a 403 (the affordance was shown off a stale config)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockRejectedValue(
      new ApiError(403, "self-service worker provisioning is disabled"),
    );
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("self-service worker provisioning is disabled")).toBeTruthy();
  });

  it("shows the rate limiter's 429", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(429, "too many requests"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("too many requests")).toBeTruthy();
  });

  it("re-enables the submit after a failure so the user can retry", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(429, "too many requests"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    await screen.findByText("too many requests");
    expect((await provisionButton()).hasAttribute("disabled")).toBe(false);
  });
});

describe("HostedWorkers ephemeral auto-provision toggle (PRD #649)", () => {
  // The control is now the app's Toggle primitive — a role="switch" button whose
  // accessible name is its `label` — not a checkbox, so it is selected by the switch
  // role and its state is read from aria-checked.
  const toggle = () => screen.findByRole("switch", { name: /auto-provision on demand/i });

  it("renders the toggle and reflects the user's current value when the admin gate is on", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: true });
    mockUser({ ephemeral_workers_enabled: true });
    renderCard(0);
    expect(await toggle()).toBeTruthy();
    expect((await toggle()).getAttribute("aria-checked")).toBe("true");
  });

  it("shows the toggle unchecked when the user has not opted in", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: true });
    mockUser({ ephemeral_workers_enabled: false });
    renderCard(0);
    expect((await toggle()).getAttribute("aria-checked")).toBe("false");
  });

  it("associates the shared provisioning description with the toggle via aria-describedby", async () => {
    // The informed-consent caveat must be programmatically linked so a screen reader
    // reads it alongside the switch's name.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: true });
    mockUser({ ephemeral_workers_enabled: false });
    renderCard(0);
    const describedby = (await toggle()).getAttribute("aria-describedby");
    expect(describedby).toBeTruthy();
    const caveat = document.getElementById(describedby as string);
    expect(caveat?.textContent).toMatch(/every capable worker stays busy/);
    expect(caveat?.textContent).toMatch(/kept warm/);
    expect(caveat?.textContent).toMatch(/ephemeral limit/);
  });

  it("writes the opt-in and updates auth with the saved user on toggle", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: true });
    mockApi.setEphemeralWorkersEnabled.mockResolvedValue({ user: { ...confirmedUser, ephemeral_workers_enabled: true } });
    mockUser({ ephemeral_workers_enabled: false });
    renderCard(0);
    fireEvent.click(await toggle());
    await waitFor(() => expect(mockApi.setEphemeralWorkersEnabled).toHaveBeenCalledWith(true));
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith({ ...confirmedUser, ephemeral_workers_enabled: true }));
  });

  it("surfaces a write failure in the toggle's own local alert (not the manual form's)", async () => {
    // quota 0 removes the manual form entirely, so its error slot ({error && <Alert>},
    // guarded by showManual) is not in the DOM. The ONLY alert that can render is the
    // ephemeral one — so this gates the slot: if the write error were routed to the
    // manual `error` state instead of `ephemeralError`, no alert would render and
    // findByRole("alert") would time out.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true });
    mockApi.setEphemeralWorkersEnabled.mockRejectedValue(new ApiError(500, "boom"));
    mockUser({ ephemeral_workers_enabled: false });
    renderCard(0);
    fireEvent.click(await toggle());
    // The local ephemeral error slot renders an alert beside the toggle.
    const alert = await screen.findByRole("alert");
    expect(alert.textContent).toBe("boom");
    expect(updateUser).not.toHaveBeenCalled();
  });

  it("is ABSENT when the admin gate is off, while the manual form still shows", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderCard(0);
    // Manual form present (its size select is a reliable marker), toggle absent.
    expect(await screen.findByLabelText("Hosted worker size")).toBeTruthy();
    expect(screen.queryByRole("switch", { name: /auto-provision on demand/i })).toBeNull();
  });

  it("survives quota 0: toggle shows, manual form absent (independence from HostedWorkerQuota)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true });
    mockUser({ ephemeral_workers_enabled: false });
    renderCard(0);
    expect(await toggle()).toBeTruthy();
    // The card keeps its heading in this ephemeral-only branch — the exact case the
    // always-rendered SectionTitle was added for (no heading-less bare toggle).
    expect(screen.getByText("Hosted workers")).toBeTruthy();
    // No manual provision affordance at quota 0.
    expect(screen.queryByLabelText("Hosted worker size")).toBeNull();
    expect(screen.queryByRole("button", { name: /provision/i })).toBeNull();
  });
});


describe("HostedWorkers ephemeral Docker preference", () => {
  const dockerBox = () => screen.findByRole("checkbox", { name: "Docker-capable ephemeral workers" });
  const autoSwitch = () => screen.getByRole("switch", { name: "Auto-provision on demand" });

  it.each([undefined, false])("hides Docker and its sentence when tier flag is %s (old API compatibility)", async (tier) => {
    // Omit the field entirely for the old-API mount case.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true,
      ...(tier === undefined ? {} : { docker_enabled: tier }) });
    renderCard();
    await screen.findByText("Ephemeral workers");
    expect(screen.queryByRole("checkbox", { name: "Docker-capable ephemeral workers" })).toBeNull();
    expect(document.getElementById("ephemeral-toggle-desc")?.textContent).not.toContain("Docker-capable includes");
    expect(autoSwitch()).toBeTruthy();
    expect(screen.queryByText("Persistent worker")).toBeNull();
  });

  it("renders independent subsections and one row with shared approved copy", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: true, docker_enabled: true });
    renderCard();
    const checkbox = await dockerBox();
    expect(screen.getByText("Persistent worker")).toBeTruthy();
    expect(screen.getByText("Ephemeral workers")).toBeTruthy();
    expect(screen.getByText(/Runs in the cluster/).textContent).toBe(
      "Runs in the cluster, not on your machine: no join token, no container to start. It shows up under Your workers and you delete it there. Docker-capable gives it a Docker daemon for container builds and tests, at extra CPU and storage.",
    );
    expect(checkbox.parentElement?.parentElement).toBe(autoSwitch().parentElement?.parentElement);
    expect(checkbox.getAttribute("aria-describedby")).toBe(autoSwitch().getAttribute("aria-describedby"));
    const copy = document.getElementById("ephemeral-toggle-desc")?.textContent;
    expect(copy).toContain("Docker-capable includes Docker for repositories your admin allows, at extra CPU and storage.");
    expect(copy).toMatch(/every capable worker stays busy/);
    expect(document.body.textContent).not.toMatch(/experimental|2\.6|rootless/i);
  });

  it("hides the ephemeral subsection under the admin gate even with the Docker tier", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false, docker_enabled: true });
    renderCard();
    await screen.findByText("Persistent worker");
    expect(screen.queryByText("Ephemeral workers")).toBeNull();
    expect(screen.queryByRole("checkbox", { name: "Docker-capable ephemeral workers" })).toBeNull();
  });

  it("retains Docker and lets it be changed while auto-provision is off", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true, docker_enabled: true });
    mockUser({ ephemeral_docker_enabled: true });
    mockApi.setEphemeralWorkersEnabled.mockResolvedValue({ user: confirmedUser });
    renderCard();
    const checkbox = await dockerBox() as HTMLInputElement;
    expect(checkbox.checked).toBe(true);
    expect(checkbox.disabled).toBe(false);
    expect(autoSwitch().getAttribute("aria-checked")).toBe("false");
    fireEvent.click(checkbox);
    await waitFor(() => expect(mockApi.setEphemeralWorkersEnabled).toHaveBeenCalledWith({ docker: false }));
  });

  it.each(["docker", "enabled"] as const)("locks both controls through %s save, then applies the authoritative response", async (field) => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true, docker_enabled: true });
    let finishWrite!: (result: { user: User }) => void;
    mockApi.setEphemeralWorkersEnabled.mockReturnValue(new Promise((resolve) => { finishWrite = resolve; }));
    updateUser.mockImplementation((savedUser: User) => mockUser(savedUser));
    const { rerender } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    const checkbox = await dockerBox() as HTMLInputElement;
    fireEvent.click(field === "docker" ? checkbox : autoSwitch());
    expect(mockApi.setEphemeralWorkersEnabled).toHaveBeenCalledWith(field === "docker" ? { docker: true } : true);
    expect(checkbox.disabled).toBe(true);
    expect(autoSwitch().hasAttribute("disabled")).toBe(true);
    fireEvent.click(field === "docker" ? autoSwitch() : checkbox);
    expect(mockApi.setEphemeralWorkersEnabled).toHaveBeenCalledTimes(1);
    const savedUser = { ...confirmedUser,
      ephemeral_docker_enabled: field === "docker", ephemeral_workers_enabled: field === "enabled" };
    finishWrite({ user: savedUser });
    await waitFor(() => expect(updateUser).toHaveBeenCalledWith(savedUser));
    rerender(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} onShowWorkers={() => {}} />);
    await waitFor(() => expect(checkbox.disabled).toBe(false));
    expect(autoSwitch().hasAttribute("disabled")).toBe(false);
    expect(checkbox.checked).toBe(field === "docker");
    expect(autoSwitch().getAttribute("aria-checked")).toBe(String(field === "enabled"));
  });

  it("restores auto-provision and releases both controls after an enabled write fails", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true, docker_enabled: true });
    mockUser({ ephemeral_workers_enabled: true, ephemeral_docker_enabled: true });
    let rejectWrite!: (error: ApiError) => void;
    mockApi.setEphemeralWorkersEnabled.mockReturnValue(new Promise((_resolve, reject) => { rejectWrite = reject; }));
    renderCard();
    const checkbox = await dockerBox() as HTMLInputElement;
    fireEvent.click(autoSwitch());
    expect(mockApi.setEphemeralWorkersEnabled).toHaveBeenCalledWith(false);
    expect(checkbox.disabled).toBe(true);
    expect(autoSwitch().hasAttribute("disabled")).toBe(true);
    rejectWrite(new ApiError(500, "auto-provision update failed"));
    expect((await screen.findByRole("alert")).textContent).toBe("auto-provision update failed");
    expect(autoSwitch().getAttribute("aria-checked")).toBe("true");
    expect(checkbox.checked).toBe(true);
    expect(checkbox.disabled).toBe(false);
    expect(autoSwitch().hasAttribute("disabled")).toBe(false);
    expect(updateUser).not.toHaveBeenCalled();
  });

  it.each([
    [false, 400, 'json: unknown field "docker"'],
    [true, 500, "preference update failed"],
  ] as const)("restores confirmed Docker %s after rejection (%s)", async (confirmed, status, message) => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0, ephemeral_enabled: true, docker_enabled: true });
    mockUser({ ephemeral_docker_enabled: confirmed });
    let rejectWrite!: (error: ApiError) => void;
    mockApi.setEphemeralWorkersEnabled.mockReturnValue(new Promise((_resolve, reject) => { rejectWrite = reject; }));
    renderCard();
    const checkbox = await dockerBox() as HTMLInputElement;
    fireEvent.click(checkbox);
    expect(checkbox.disabled).toBe(true);
    expect(autoSwitch().hasAttribute("disabled")).toBe(true);
    rejectWrite(new ApiError(status, message));
    expect((await screen.findByRole("alert")).textContent).toBe(message);
    expect(checkbox.checked).toBe(confirmed);
    expect(checkbox.disabled).toBe(false);
    expect(autoSwitch().hasAttribute("disabled")).toBe(false);
    expect(updateUser).not.toHaveBeenCalled();
  });
});
