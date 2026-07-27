// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { HostedWorkers } from "./HostedWorkers";
import { api, ApiError, type Worker } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

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
  anthropic_secret_id: null,
  anthropic_secret_label: null,
  anthropic_bind_mode: "default",
};

function renderCard(hostedCount = 0, onProvisioned = vi.fn()) {
  render(<HostedWorkers hostedCount={hostedCount} onProvisioned={onProvisioned} />);
  return onProvisioned;
}

const provisionButton = () => screen.findByRole("button", { name: /provision/i });

describe("HostedWorkers visibility (the card is hidden, never disabled-with-excuse)", () => {
  it("renders nothing while the config is still in flight", () => {
    mockApi.hostedConfig.mockReturnValue(new Promise(() => {})); // never settles
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} />);
    // No skeleton either: a placeholder would promise a card that may never come.
    expect(container.firstChild).toBeNull();
  });

  it("renders nothing when hosting is disabled on the instance", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 2 });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    // Disabled beats a non-zero quota: a client reading enabled:false renders nothing
    // hosted regardless of the number beside it.
    expect(container.firstChild).toBeNull();
  });

  it("fails closed when the config read rejects — no card, no error banner", async () => {
    mockApi.hostedConfig.mockRejectedValue(new ApiError(500, "internal error"));
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    expect(container.firstChild).toBeNull();
    // A capability probe that blipped must not shout at a user who may not even have
    // the feature.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("hides the form when the quota is 0 — that is policy, not a full quota", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 0 });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} />);
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    // No "0 of 0 used" and no disabled button: deleting a worker would not help, so
    // offering the affordance would be a lie. The user's existing hosted rows are the
    // page's business and keep rendering — this card is only the provision path.
    expect(container.firstChild).toBeNull();
  });

  it("reads the policy once and never polls it", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderCard(0);
    await provisionButton();
    expect(mockApi.hostedConfig).toHaveBeenCalledTimes(1);
  });
});

describe("HostedWorkers quota states", () => {
  it("counts the user's hosted workers against the quota and allows a provision", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderCard(1);
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    expect((await provisionButton()).hasAttribute("disabled")).toBe(false);
  });

  it("disables the submit at quota and says how to get out of it", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderCard(2);
    expect(await screen.findByText(/2 of 2 used/)).toBeTruthy();
    expect(await screen.findByText(/delete one to provision another/)).toBeTruthy();
    expect((await provisionButton()).hasAttribute("disabled")).toBe(true);
  });

  it("never calls the endpoint while at quota", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderCard(2);
    fireEvent.click(await provisionButton());
    expect(mockApi.provisionHostedWorker).not.toHaveBeenCalled();
  });
});

describe("HostedWorkers provisioning", () => {
  it("sends the selected template and the LOWERCASE size, then refreshes the fleet", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
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
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
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
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    const { container } = render(<HostedWorkers hostedCount={0} onProvisioned={vi.fn()} />);

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
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    const onProvisioned = renderCard(0);
    fireEvent.click(await provisionButton());

    await waitFor(() => expect(onProvisioned).toHaveBeenCalledWith(provisioned));
    // And it renders no confirmation of its own — that would be a second slot.
    expect(screen.queryByRole("status")).toBeNull();
  });

  it("defaults to compose parity — the base template at M", async () => {
    // M, not S: it is what the repo's own sizing formula computes as the floor for a
    // working worker (1 run slot + 1 unturnoffable chat session, + headroom = 4 GiB),
    // and it matches both compose's AGENT_MEM_LIMIT default and the k8s block in
    // docs/worker-setup.md. It is the preset a user hands themselves by not choosing,
    // so it must be the known-good one rather than the cheapest.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderCard(0);
    fireEvent.click(await provisionButton());
    await waitFor(() => expect(mockApi.provisionHostedWorker).toHaveBeenCalledWith("base", "m", false));
  });

  it("shows the 409 quota refusal verbatim (the server's words, not ours)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    // The client-side gate is a hint, not the rule: a stale count gets here and the
    // server's advisory-locked count is what refuses. Pinned verbatim because this
    // exact string is what the user reads, and it names the QUOTA, never their count.
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(409, "hosted worker quota reached (2)"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("hosted worker quota reached (2)")).toBeTruthy();
  });

  it("shows a 403 (the affordance was shown off a stale config)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockRejectedValue(
      new ApiError(403, "self-service worker provisioning is disabled"),
    );
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("self-service worker provisioning is disabled")).toBeTruthy();
  });

  it("shows the rate limiter's 429", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(429, "too many requests"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    expect(await screen.findByText("too many requests")).toBeTruthy();
  });

  it("re-enables the submit after a failure so the user can retry", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.provisionHostedWorker.mockRejectedValue(new ApiError(429, "too many requests"));
    renderCard(0);
    fireEvent.click(await provisionButton());
    await screen.findByText("too many requests");
    expect((await provisionButton()).hasAttribute("disabled")).toBe(false);
  });
});
