// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import { WorkersSettings } from "./WorkersSettings";
import { api, type SecretMeta, type Worker } from "../lib/api";

vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: {
      listWorkers: vi.fn(),
      // The page states the fleet's target release from the same GET /api/version the
      // footer uses (PRD #113 M5), so mounting it now touches this. Resolving "" is the
      // honest default here: these tests assert on gauges and rows, and an unstamped
      // control plane is exactly what a local build looks like.
      //
      // What that renders CHANGED with the tri-state fix: "" used to fold to null and
      // show the pending blank, and now reaches the panel as settled-unknown, so these
      // mounts display "control-plane release unknown — targets unchecked". No
      // assertion here depends on either, which is why the file stayed green — but the
      // next person reading this fixture should not have to rediscover which arm it
      // drives.
      version: vi.fn().mockResolvedValue({ version: "" }),
      // The page reads the user's tokens alongside the workers (PRD #104 M6) to
      // populate the per-row token picker.
      listSecrets: vi.fn(),
      setWorkerBindMode: vi.fn(),
      createWorker: vi.fn(),
      deleteWorker: vi.fn(),
      hostedConfig: vi.fn(),
      provisionHostedWorker: vi.fn(),
    },
  };
});

const mockApi = vi.mocked(api);

// Hosting off unless a test says otherwise: that is the default an instance ships
// with (PRD #58 Decision 12, compose is zero-diff), so the pre-#58 tests below render
// the page they always rendered.
beforeEach(() => {
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0 });
  // One token by default: the picker only renders with more than one, so the
  // pre-#104 tests below see exactly the page they always saw.
  mockApi.listSecrets.mockResolvedValue({ secrets: [aSecret()] });
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
  vi.useRealTimers();
});

function aSecret(over: Partial<SecretMeta> = {}): SecretMeta {
  return {
    id: "sec-default",
    kind: "anthropic_token",
    label: "default",
    is_default: true,
    // PRD #111 M2: the auto-selection pool opt-in, false unless a test says otherwise.
    auto_eligible: false,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function aWorker(over: Partial<Worker> = {}): Worker {
  return {
    // Unbound by default (PRD #104): every worker spends its owner's default token
    // until someone binds one, which is the state these pre-#104 tests assume.
    anthropic_secret_id: null,
    anthropic_secret_label: null,
    anthropic_bind_mode: "default",
    id: "w1",
    name: "laptop",
    status: "online",
    busy: false,
    kind: "external",
    hosted_size: null,
    active_runs: 0,
    max_concurrent_runs: null,
    template_declared: null,
    template_reported: "base",
    version: "0.4.2",
    upgrade_status: "up_to_date",
    upgrade_detail: null,
    upgrade_target: "0.4.2",
    upgrade_blocking_container: null,
    upgrade_blocking_reason: null,
    upgrade_last_exit_code: null,
    last_heartbeat_at: "2026-07-14T00:00:00Z",
    created_at: "2026-07-01T00:00:00Z",
    stats_cpu_pct: null,
    stats_mem_bytes: null,
    stats_mem_limit_bytes: null,
    stats_source: null,
    draining_since: null,
    ...over,
  };
}

const fleet: Worker[] = [
  aWorker({ id: "w-lap", name: "laptop", stats_cpu_pct: 34, stats_mem_bytes: 2254857830, stats_mem_limit_bytes: 4294967296, stats_source: "cgroup" }),
  aWorker({ id: "w-off", name: "ci", status: "offline", stats_cpu_pct: 12, stats_mem_bytes: 1610612736, stats_mem_limit_bytes: 2147483648, stats_source: "cgroup" }),
  aWorker({ id: "w-nas", name: "nas", stats_cpu_pct: 8, stats_mem_bytes: 503316480, stats_mem_limit_bytes: null, stats_source: "process" }),
];

function renderPage() {
  return render(
    <MemoryRouter>
      <WorkersSettings />
    </MemoryRouter>,
  );
}

describe("WorkersSettings — always-visible worker-setup guide link (PRD #57 M2)", () => {
  it("renders the worker-setup guide link in the page header", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    const docLink = await screen.findByRole("link", { name: "worker setup" });
    expect(docLink.getAttribute("href")).toBe("/docs/worker-setup");
  });
});

// Issue #124, item 7 addendum: the worker's self-reported version, same trust class and
// same ingest scrubber as `target`.
describe("WorkersSettings — the reported version carries no format characters (#124)", () => {
  it("strips bidi/zero-width characters out of the version string", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w-1", name: "laptop", version: "0.11\u202E.0\u200B" })],
    });
    const { container } = renderPage();
    // Anchored on the worker NAME, which the mutation cannot move — awaiting the cleaned
    // version would red at the lookup instead of at the assertion.
    await waitFor(() => expect(screen.getByText("laptop")).toBeTruthy());
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
    expect(container.textContent).toContain("v0.11.0");
  });
});

// PRD #251: uptime = now − online_since, rendered as "· up <duration>" ONLY while
// the worker is online. Paired positive/negative per the copy-change rule in
// .claude/rules/web.md: the negative ("no token when offline") is only meaningful
// alongside the positive that proves the token renders at all. The uptime span is
// the only metadata token that reads "· up …", so /· up / matches it uniquely (the
// version and last-seen spans read "· v…" / "· last seen …").
describe("WorkersSettings worker uptime (PRD #251)", () => {
  it("shows an 'up' uptime token for an ONLINE worker with online_since", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          id: "w-up",
          name: "uptimer",
          status: "online",
          online_since: new Date(Date.now() - 90 * 60 * 1000).toISOString(), // ~1h 30m ago
        }),
      ],
    });
    renderPage();
    await screen.findByText("uptimer");
    expect(screen.getByText(/· up \d/)).toBeTruthy();
  });

  it("shows NO uptime token for an OFFLINE worker (online_since null)", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          id: "w-down",
          name: "downed",
          status: "offline",
          online_since: null,
        }),
      ],
    });
    renderPage();
    await screen.findByText("downed");
    expect(screen.queryByText(/· up \d/)).toBeNull();
  });
});

describe("WorkersSettings resource gauges (PRD #49)", () => {
  it("renders per-worker CPU + memory gauges, a no-limit absolute readout, and the process-source label", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();

    // Limited worker → used/limit memory bar with a percentage.
    expect(await screen.findByText(/2\.1\/4 GiB · 52%/)).toBeTruthy();
    // Process-source, no limit → absolute usage, no percentage, labeled.
    expect(screen.getByText(/480 MiB/)).toBeTruthy();
    expect(screen.getByText(/no limit/)).toBeTruthy();
    expect(screen.getByText("worker process only")).toBeTruthy();
  });

  it("dims an offline worker's gauges (last-known, not live-looking)", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    const offlineBlock = await screen.findByLabelText(/worker offline/i);
    expect(offlineBlock.className).toMatch(/opacity-50/);
  });

  it("renders no gauges for a worker that has not reported a sample yet", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ name: "fresh" })] });
    renderPage();
    expect(await screen.findByText("fresh")).toBeTruthy();
    // A worker with no sample keeps its plain row — no progressbars at all.
    expect(screen.queryAllByRole("progressbar")).toHaveLength(0);
  });

  it("re-polls the fleet every 10s while visible (live liveness)", async () => {
    vi.useFakeTimers();
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    await vi.advanceTimersByTimeAsync(0); // flush the initial load
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(10000); // one poll interval
    expect(mockApi.listWorkers).toHaveBeenCalledTimes(2);
  });
});

describe("WorkersSettings hosted workers (PRD #58 M5)", () => {
  const hosted = aWorker({ id: "w-h", name: "base (M)", kind: "hosted", hosted_size: "m" });

  it("marks a hosted row with its kind, leaves external rows unmarked, and shows no size badge", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted, aWorker({ id: "w-x", name: "laptop" })] });
    renderPage();

    // One list, marked — not two lists: a hosted worker is an ordinary worker whose
    // container the controller runs, so it keeps the same row, status and delete.
    expect(await screen.findByText("hosted")).toBeTruthy();
    expect(screen.getAllByText("hosted")).toHaveLength(1);
    expect(screen.getAllByRole("button", { name: "Delete" })).toHaveLength(2);
    // The size now lives in the derived name ("base (M)"), not a badge — the "size X"
    // chip was dropped (it duplicated the name) in favour of a docker badge.
    expect(screen.queryByText(/^size /)).toBeNull();
  });

  it("renders no size badge for a hosted worker (size lives in the name now)", async () => {
    // The "size M" chip is gone: it repeated what the derived name already says
    // ("base (M)"). Nothing in the row carries a bare-letter or "size ..." badge.
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted] });
    renderPage();
    await screen.findByText("hosted");
    expect(screen.queryByText(/^size /)).toBeNull();
    expect(screen.queryByText("size M")).toBeNull();
  });

  it("shows a docker badge for a docker-capable hosted worker, and none otherwise", async () => {
    // The docker capability (PRD #83 M3) is real TEXT — the word "docker". true renders
    // the badge; false/undefined renders nothing (absence needs no badge).
    const dockerHosted = aWorker({ id: "w-hd", name: "base (M)", kind: "hosted", hosted_size: "m", docker: true });
    const plainHosted = aWorker({ id: "w-hp", name: "base (S)", kind: "hosted", hosted_size: "s", docker: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [dockerHosted, plainHosted] });
    renderPage();

    await screen.findByText("base (M)");
    expect(screen.getByText("docker")).toBeTruthy();
    // Exactly one docker badge: the sidecar-less worker (docker:false) shows none.
    expect(screen.getAllByText("docker")).toHaveLength(1);

    // And a hosted worker with docker undefined shows no badge either.
    cleanup();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ id: "w-hu", name: "base (L)", kind: "hosted", hosted_size: "l" })] });
    renderPage();
    await screen.findByText("base (L)");
    expect(screen.queryByText("docker")).toBeNull();
  });

  it("badges a hosted row even when hosting is switched off (never leave a row lying)", async () => {
    // An admin can turn hosting off while a user still holds hosted workers. The rows
    // must stay listed and deletable — and stay honest about what they are, or they
    // read as workers the user forgot to start.
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0 });
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted] });
    renderPage();
    expect(await screen.findByText("hosted")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
    expect(screen.queryByText("Provision a hosted worker")).toBeNull();
  });

  it("shows the provision card only when the instance has hosting on", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.queryByText("Provision a hosted worker")).toBeNull();

    cleanup();
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    renderPage();
    expect(await screen.findByText("Provision a hosted worker")).toBeTruthy();
  });

  it("counts only hosted workers against the quota, not the whole fleet", async () => {
    // The count comes from the list the page already polls — three external workers
    // must not eat the hosted allowance.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValue({ workers: [...fleet, hosted] });
    renderPage();
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
  });

  it("renders the cordon pill AFTER the status badge, and none for a non-cordoned row (PRD #496 M3)", async () => {
    // The cluster only renders where the row does, so this is the place — not the
    // component test — where DOM ORDER is a real property: the pill must sit after the
    // status badge, not replace or precede it.
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          id: "w-cordon",
          name: "cordoned-one",
          kind: "hosted",
          hosted_size: "m",
          draining_since: "2026-08-21T12:00:00Z",
          active_runs: 0,
        }),
      ],
    });
    renderPage();
    await screen.findByText("cordoned-one");

    const pill = screen.getByText("cordoned");
    // The status badge span carries the status word exactly ("online"), which never
    // collides with the "· up …" uptime token — a loose /online/ match would.
    const status = screen.getByText("online");
    // The pill FOLLOWS the status badge in document order.
    expect(status.compareDocumentPosition(pill) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(pill.getAttribute("title")).toBe("Cordoned — not claiming new runs.");
  });

  it("renders NO cordon pill for a non-cordoned hosted worker (paired with the positive above)", async () => {
    // The negative half of the ordering test above: a hosted row that is not draining
    // (draining_since null) shows neither pill wording. Vacuous on its own — meaningful
    // only alongside the positive case that proves the pill renders at all.
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w-plain", name: "plain-one", kind: "hosted", hosted_size: "m", draining_since: null })],
    });
    renderPage();
    await screen.findByText("plain-one");
    expect(screen.queryByText("cordoned")).toBeNull();
    expect(screen.queryByText("draining")).toBeNull();
  });

  it("still shows the one-time token card for an EXTERNAL worker (the hand-run flow is untouched)", async () => {
    // The regression guard for the sibling flow: hosted provisioning returns no token,
    // and adding it must not have cost createWorker the token card that is the only
    // time its secret is ever shown.
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.createWorker.mockResolvedValue({
      worker: aWorker({ id: "w-new", name: "nas" }),
      token: "uzi_wk_deadbeef",
    });
    renderPage();

    fireEvent.change(await screen.findByPlaceholderText(/laptop, ci-runner-1/), { target: { value: "nas" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate join token" }));

    await waitFor(() => expect(mockApi.createWorker).toHaveBeenCalledWith("nas", "base"));
    expect(await screen.findByText("uzi_wk_deadbeef")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Copy" })).toBeTruthy();
  });
});

describe("WorkersSettings hosted quota is escapable (the primary journey)", () => {
  it("re-enables provisioning after the user deletes a hosted worker", async () => {
    // The one thing a component test of HostedWorkers alone CANNOT prove: it asserts
    // "disabled at quota" and passes whether or not the state is escapable. The count
    // is the page's — it comes from the fleet list — so only the page can show the gate
    // releasing rather than dead-ending the journey. Without this, the loop is only
    // ever checked by hand in the demo build.
    const h1 = aWorker({ id: "w-h1", name: "base (S)", kind: "hosted", hosted_size: "s" });
    const h2 = aWorker({ id: "w-h2", name: "base (M)", kind: "hosted", hosted_size: "m" });
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1, h2] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();

    const provision = () => screen.getByRole("button", { name: /^Provision$/ });
    expect(await screen.findByText(/2 of 2 used/)).toBeTruthy();
    expect(provision().hasAttribute("disabled")).toBe(true);

    // Delete one → the page reloads the fleet → the count drops → the gate lifts.
    // Hosted deletes confirm first, so the journey out of the quota is now two clicks.
    mockApi.listWorkers.mockResolvedValue({ workers: [h1] });
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[1]);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-h2"));
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    await waitFor(() => expect(provision().hasAttribute("disabled")).toBe(false));
  });
});

describe("WorkersSettings hosted delete confirms; external delete does not (PRD #58)", () => {
  const hostedW = aWorker({ id: "w-h", name: "base (M)", kind: "hosted", hosted_size: "m" });
  const externalW = aWorker({ id: "w-x", name: "laptop" });

  it("does NOT delete a hosted worker on the first click — it arms a confirmation", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    expect(mockApi.deleteWorker).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Delete anyway" })).toBeTruthy();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeTruthy();
  });

  it("names what is destroyed, rather than asking a content-free 'are you sure'", async () => {
    // The confirmation exists because the cost is invisible at the moment of the click.
    // A generic prompt would add friction and inform nobody.
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    expect(screen.getByText("/data")).toBeTruthy();
    expect(screen.getByText("/nix")).toBeTruthy();
    // v1 ships no restart endpoint, so Delete is the only lifecycle control a hosted
    // user has — they will reach for it to restart a stuck worker. Leading with this
    // is what stops them silently paying the /nix re-fetch.
    expect(screen.getByText(/Delete is not a restart/)).toBeTruthy();
    expect(screen.getByText(/re-downloads its tools from the internet/)).toBeTruthy();
  });

  it("deletes once confirmed", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Delete anyway" }));
    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-h"));
  });

  it("cancelling deletes nothing and restores the row", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    expect(mockApi.deleteWorker).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: "Delete anyway" })).toBeNull();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
  });

  it("Escape cancels, so a keyboard user is not trapped in the confirmation", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.keyDown(screen.getByRole("group", { name: /Confirm deleting base \(M\)/ }), { key: "Escape" });

    expect(screen.queryByRole("button", { name: "Delete anyway" })).toBeNull();
    expect(mockApi.deleteWorker).not.toHaveBeenCalled();
  });

  it("focuses the WARNING, not the destructive button — a confirmation must not be a formality", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    const group = screen.getByRole("group", { name: /Confirm deleting base \(M\)/ });
    expect(document.activeElement).toBe(group);
    // Auto-focusing "Delete anyway" would let a keyboard user confirm blind, which is
    // the confirmation defeating itself.
    expect(document.activeElement).not.toBe(screen.getByRole("button", { name: "Delete anyway" }));
  });

  it("keeps EXTERNAL delete one click — a token revoke, not a disk wipe", async () => {
    // The regression guard for shipped behaviour. Deleting an external worker revokes
    // a token: the container keeps running and the user re-registers to recover. There
    // is nothing to warn about, and adding friction to a cheap, reversible action is
    // how a confirmation becomes noise people click through.
    mockApi.listWorkers.mockResolvedValue({ workers: [externalW] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-x"));
    expect(screen.queryByRole("button", { name: "Delete anyway" })).toBeNull();
  });

  it("arms only the row that was clicked", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW, aWorker({ id: "w-h2", name: "jvm (L)", kind: "hosted", hosted_size: "l" })] });
    renderPage();
    await screen.findByText("base (M)");
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[0]);

    expect(screen.getAllByRole("button", { name: "Delete anyway" })).toHaveLength(1);
    expect(screen.getByRole("group", { name: /Confirm deleting base \(M\)/ })).toBeTruthy();
    // The other hosted row keeps its ordinary Delete.
    expect(screen.getAllByRole("button", { name: "Delete" })).toHaveLength(1);
  });
});

describe("WorkersSettings hosted confirm keeps a keyboard user's place (PRD #58)", () => {
  const hostedW = aWorker({ id: "w-h", name: "base (M)", kind: "hosted", hosted_size: "m" });
  const deleteBtn = () => screen.getByRole("button", { name: "Delete" });
  const group = () => screen.getByRole("group", { name: /Confirm deleting base \(M\)/ });

  it("returns focus to the Delete button when Escape dismisses the confirm", async () => {
    // Backing out correctly must not cost the user their place. Without this, Escape
    // drops focus to <body> and a keyboard user tabs from the top of the document back
    // to the row they were already on — the escape hatch punishing a misclick.
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.keyDown(group(), { key: "Escape" });

    await waitFor(() => expect(document.activeElement).toBe(deleteBtn()));
    expect(document.activeElement).not.toBe(document.body);
  });

  it("returns focus to the Delete button when Cancel dismisses the confirm", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));

    await waitFor(() => expect(document.activeElement).toBe(deleteBtn()));
  });

  it("returns focus to the RIGHT row's Delete button when several are listed", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w-h1", name: "jvm (L)", kind: "hosted", hosted_size: "l" }), hostedW],
    });
    renderPage();
    await screen.findByText("base (M)");
    // Arm the SECOND row.
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[1]);
    fireEvent.keyDown(group(), { key: "Escape" });

    await waitFor(() => expect(document.activeElement).toBe(screen.getAllByRole("button", { name: "Delete" })[1]));
  });

  it("describes the confirm group with the warning, so the payload is announced with the name", async () => {
    // A named container announces its NAME on focus — "Confirm deleting base (M),
    // group" — which sounds like a routine are-you-sure. Without aria-describedby the
    // warning stays untethered text a screen-reader user may never hear, which would
    // defeat the whole control.
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW] });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    const describedBy = group().getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    const description = document.getElementById(describedBy!);
    // The description must be the text that names the losses, not just any element.
    expect(description?.textContent).toMatch(/Delete is not a restart/);
    expect(description?.textContent).toMatch(/\/data/);
    expect(description?.textContent).toMatch(/\/nix/);
  });
});

describe("WorkersSettings announces what just happened (PRD #58 findings 10 + 11)", () => {
  const hostedW = aWorker({ id: "w-h", name: "base (M)", kind: "hosted", hosted_size: "m" });
  const externalW = aWorker({ id: "w-x", name: "laptop" });
  const provisioned = aWorker({ id: "w-new", name: "base (S)", kind: "hosted", hosted_size: "s" });

  it("announces a provision, names the server's worker, and takes focus", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Provision" }));

    const msg = await screen.findByText("Provisioned base (S) — it appears in your workers below.");
    // toBe the wrapper, not a textContent match: focus on <body> would match anything,
    // since body.textContent is the entire page.
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
  });

  it("a delete REPLACES the provision notice — it must not outlive the row it describes", async () => {
    // The bug this exists for: provision, delete, and the page still said "it appears in
    // your workers below" about a row that was gone. Nothing cleared it but the NEXT
    // provision, so the only message left in a live region was the false one.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2 });
    mockApi.listWorkers.mockResolvedValueOnce({ workers: [] }).mockResolvedValue({ workers: [provisioned] });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Provision" }));
    expect(await screen.findByText(/Provisioned base \(S\)/)).toBeTruthy();
    // The row it describes is now listed.
    const del = await screen.findByRole("button", { name: "Delete" });

    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // gone after the delete
    fireEvent.click(del);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    expect(await screen.findByText("Deleted base (S).")).toBeTruthy();
    expect(screen.queryByText(/it appears in your workers below/)).toBeNull();
  });

  it("announces a hosted delete and takes focus — not the next row's Delete button", async () => {
    // Focusing the next row's Delete is the conventional list-deletion pattern and is
    // unsafe here: the remaining rows are mostly external, where Delete is one click and
    // destroys immediately. A keyboard user double-tapping Enter through the confirm
    // would take a second worker with them.
    mockApi.listWorkers.mockResolvedValue({ workers: [hostedW, externalW] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    await screen.findByText("base (M)");
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[0]);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    const msg = await screen.findByText("Deleted base (M).");
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
    // Not parked on any live one-click destructor.
    for (const b of screen.getAllByRole("button", { name: "Delete" })) {
      expect(document.activeElement).not.toBe(b);
    }
  });

  it("announces an EXTERNAL delete too — feedback after the act costs no clicks", async () => {
    // "External delete stays one click" is about friction BEFORE the act. This is
    // feedback after it: one click is still one click, and a silently vanishing row is
    // poor feedback whichever kind it was.
    mockApi.listWorkers.mockResolvedValue({ workers: [externalW] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Delete" }));

    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-x"));
    const msg = await screen.findByText("Deleted laptop.");
    // Still one click: no confirmation was interposed.
    expect(screen.queryByRole("button", { name: "Delete anyway" })).toBeNull();
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
  });

  it("strips bidi/zero-width characters out of the delete announcement (#173)", async () => {
    // The delete-success live region is the ONLY channel confirming the destructive act
    // to a screen-reader user, and the row it names is already gone — so unlike the list
    // name there is no surviving visible counterpart to sanitize against. A self-authored
    // name carrying U+202E/U+200B must land in the announcement already cleaned.
    const spoofed = aWorker({ id: "w-spoof", name: "base \u202E(M)\u200B", kind: "hosted", hosted_size: "m" });
    mockApi.listWorkers.mockResolvedValue({ workers: [spoofed] });
    mockApi.deleteWorker.mockResolvedValue(null);
    const { container } = renderPage();
    await screen.findByRole("button", { name: "Delete" });

    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // gone after the delete
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    // The readable name survives with the control chars removed (fails on the raw
    // interpolation, which would announce "Deleted base \u202E(M)\u200B.").
    expect(await screen.findByText("Deleted base (M).")).toBeTruthy();
    expect(container.textContent ?? "").not.toMatch(/[\p{Cf}]/u);
  });

  it("re-announces when two identically-named workers are deleted in turn", async () => {
    // Derived names are NOT unique — "base (S)" twice is exactly what a quota of 2
    // produces. The slot holds an OBJECT, so every announcement is a value the focus
    // effect sees as new; a bare string would let the second delete set an identical
    // value, React would bail out, and it would go unannounced.
    const a = aWorker({ id: "w-a", name: "base (S)", kind: "hosted", hosted_size: "s" });
    const b = aWorker({ id: "w-b", name: "base (S)", kind: "hosted", hosted_size: "s" });
    mockApi.listWorkers.mockResolvedValue({ workers: [a, b] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Delete" })).toHaveLength(2));

    mockApi.listWorkers.mockResolvedValue({ workers: [b] });
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[0]);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));
    expect(await screen.findByText("Deleted base (S).")).toBeTruthy();
    await waitFor(() => expect(screen.getAllByRole("button", { name: "Delete" })).toHaveLength(1));

    // Drop focus so a no-op second announcement is detectable rather than invisible.
    (document.activeElement as HTMLElement)?.blur();
    expect(document.activeElement).toBe(document.body);

    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    const msg = await screen.findByText("Deleted base (S).");
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
  });
});

// ── Worker → token binding (PRD #104 M3/M6) ─────────────────────────────────

describe("WorkersSettings token binding (PRD #104)", () => {
  // console-key is POOLED, and that is a correction rather than a detail. Two tests
  // below assert that an auto worker reads as auto-selecting — and with an entirely
  // un-pooled fixture that copy is now (correctly) suppressed by web-ux F18's guard,
  // because such a worker resolves pool_empty on every claim and spends the default.
  // The old fixture made those tests assert the misleading string; pooling one token
  // restores what they were actually about, which is the auto MODE's rendering.
  const twoTokens = [
    aSecret(),
    aSecret({ id: "sec-console", label: "console-key", is_default: false, auto_eligible: true }),
  ];

  // The EFFECTIVE token is always stated. An unbound worker says "your default
  // token" rather than nothing, because nothing reads as "no token" when the truth
  // is "the default".
  it("states the effective token for an unbound worker", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.getByText(/your default token/i)).toBeTruthy();
  });

  // The MIRROR of the case above, and the reason "always state it" needs three
  // branches rather than two (web-ux D2): on an account holding NO tokens, "spends
  // your default token" over-claims a credential that does not exist, on an account
  // where every run will fail, with nowhere to go. Blank was wrong in one
  // direction; this was wrong in the other.
  it("does not claim a default token on an account that has none", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: [] });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.queryByText(/your default token/i)).toBeNull();
    expect(screen.getByText(/no Anthropic token/i)).toBeTruthy();
    // And it points somewhere actionable rather than just reporting the problem.
    expect(screen.getByRole("link", { name: /add one in Settings/i })).toBeTruthy();
  });

  it("names the bound credential on a bound worker", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ anthropic_secret_id: "sec-console", anthropic_secret_label: "console-key" })],
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    renderPage();
    await screen.findByText("laptop");
    // The row states it on its effective-token line ("spends console-key"). The
    // picker also lists it as an <option>, so scope the assertion to that line
    // rather than asserting the label appears somewhere on the page.
    const spendsLine = screen.getByText(/^spends/i);
    expect(spendsLine.textContent).toMatch(/console-key/);
  });

  // With ONE token there is nothing to choose between, so no picker is offered —
  // an always-visible picker would imply a choice the user does not have.
  it("offers no picker when the user holds a single token", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.queryByLabelText("Anthropic token for laptop")).toBeNull();
  });

  it("rebinds a worker to a named token by label", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setWorkerBindMode.mockResolvedValue({
      worker: aWorker({
        anthropic_bind_mode: "pinned",
        anthropic_secret_id: "sec-console",
        anthropic_secret_label: "console-key",
      }),
    });
    renderPage();
    await screen.findByText("laptop");
    const picker = await screen.findByLabelText("Anthropic token for laptop");
    // The picker MUST carry the shared field styling, not just its own h-8 sizing —
    // this guards against a refactor silently re-stripping the base class.
    expect(picker.className).toContain("h-8");
    expect(picker.className).toContain("bg-raised");
    expect(picker.className).toContain("border-edge");
    fireEvent.change(picker, { target: { value: "console-key" } });
    await waitFor(() =>
      expect(mockApi.setWorkerBindMode).toHaveBeenCalledWith("w1", "pinned", "console-key"),
    );
    // The announcement says WHEN it takes effect — a user who expects to restart
    // something will otherwise go looking for the control to do it.
    expect(await screen.findByText(/from its next claim/i)).toBeTruthy();
  });

  // Selecting "default token" CLEARS the binding — mode "default", label null.
  it("clears the binding when the picker returns to the default", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          anthropic_bind_mode: "pinned",
          anthropic_secret_id: "sec-console",
          anthropic_secret_label: "console-key",
        }),
      ],
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setWorkerBindMode.mockResolvedValue({ worker: aWorker() });
    renderPage();
    await screen.findByText("laptop");
    const picker = await screen.findByLabelText("Anthropic token for laptop");
    fireEvent.change(picker, { target: { value: "" } });
    await waitFor(() => expect(mockApi.setWorkerBindMode).toHaveBeenCalledWith("w1", "default", null));
  });

  // --- PRD #111 M3: the third mode -----------------------------------------

  // The auto option sends mode "auto" with NO label. The server refuses a label
  // alongside a non-pinned mode rather than reconciling it, so a picker that sent
  // the sentinel through as a label would 400 rather than silently mis-bind.
  it("switches a worker to auto, sending no label", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    mockApi.setWorkerBindMode.mockResolvedValue({
      worker: aWorker({ anthropic_bind_mode: "auto" }),
    });
    renderPage();
    await screen.findByText("laptop");
    const picker = await screen.findByLabelText("Anthropic token for laptop");
    fireEvent.change(picker, { target: { value: "\u0000auto" } });
    await waitFor(() => expect(mockApi.setWorkerBindMode).toHaveBeenCalledWith("w1", "auto", null));
    expect(await screen.findByText(/auto-selects from your token pool/i)).toBeTruthy();
  });

  // An auto worker must READ as auto, not as "spends your default token" — which
  // is what an id-first render would say, since an auto worker holds no pin. That
  // is the state auto DEGRADES to when the pool is empty, not what it is.
  it("describes an auto worker as auto-selecting, not as using the default", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ anthropic_bind_mode: "auto" })],
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.getByText(/auto-selects from your/i)).toBeTruthy();
    expect(screen.queryByText(/spends your default token/i)).toBeNull();
    // …and the picker reflects it rather than showing "default token".
    const picker = (await screen.findByLabelText(
      "Anthropic token for laptop",
    )) as HTMLSelectElement;
    expect(picker.value).toBe("\u0000auto");
  });

  // D9 end to end: the server reports the EFFECTIVE mode, so a worker whose pinned
  // token was deleted arrives as "default" with a null label and must render as
  // using the default — never as a pin to a token that no longer exists.
  it("renders a worker whose pinned token was deleted as using the default", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          anthropic_bind_mode: "default",
          anthropic_secret_id: null,
          anthropic_secret_label: null,
        }),
      ],
    });
    mockApi.listSecrets.mockResolvedValue({ secrets: twoTokens });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.getByText(/spends your default token/i)).toBeTruthy();
    expect(screen.queryByText(/auto-selects/i)).toBeNull();
  });
});

// --- web-ux F18: an auto worker over an EMPTY pool --------------------------------

describe("WorkersSettings auto-mode over an empty pool (web-ux F18)", () => {
  // 🔴 THE FIXTURE HOLDS TOKENS. That is the whole point, and it is why the obvious
  // fix is wrong: the neighbouring precedent in this component guards on
  // `tokens.length === 0`, and copying that shape produces a guard that is SILENT in
  // exactly the case that matters — a user holding several tokens with none opted in.
  // A fixture with zero tokens would pass against either implementation.
  const twoUnpooled = [
    aSecret({ id: "sec-default", label: "default", is_default: true, auto_eligible: false }),
    aSecret({ id: "sec-spare", label: "spare-key", is_default: false, auto_eligible: false }),
  ];

  it("says the pool is empty rather than claiming it auto-selects", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: twoUnpooled });
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ anthropic_bind_mode: "auto" })],
    });
    renderPage();
    // Every claim resolves pool_empty and spends the owner's default. Saying
    // "auto-selects from your token pool" here is R7's silent no-op moved up one
    // level, on the surface where the choice is actually made.
    // Matched on ONE text node: `token pool` is a <Link>, so the sentence is split
    // across elements and a regex spanning them finds nothing.
    expect(await screen.findByText(/is empty — its runs spend your default token/i)).toBeTruthy();
  });

  it("says it auto-selects once ONE token is pooled", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [twoUnpooled[0], aSecret({ id: "sec-spare", label: "spare-key", auto_eligible: true })],
    });
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ anthropic_bind_mode: "auto" })],
    });
    renderPage();
    expect(await screen.findByText(/auto-selects from your/i)).toBeTruthy();
    expect(screen.queryByText(/is empty — its runs spend/i)).toBeNull();
  });

  // The DEPS leg, asserted directly, and the fixture is the ORDINARY path rather than
  // an exotic one: a token is pooled at mount and nothing changes mid-mount.
  //
  // That is what makes the bug live rather than defensive. `tokens` starts [] and is
  // filled asynchronously, so under `[]` deps rebind is created on the first render
  // with pooledCount === 0 and keeps that value for the life of the mount. Open the
  // page with a full pool, switch a worker to auto, hear "your token pool is empty".
  //
  // It was already caught before this test — by a 5s TIMEOUT on a pre-existing test
  // whose name says nothing about pooled counts. A guard that works and cannot explain
  // itself costs the next person the time this one saves.
  it("announces a non-empty pool for a worker switched to auto after load", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [twoUnpooled[0], aSecret({ id: "sec-spare", label: "spare-key", auto_eligible: true })],
    });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ anthropic_bind_mode: "default" })] });
    mockApi.setWorkerBindMode.mockResolvedValue({ worker: aWorker({ anthropic_bind_mode: "auto" }) });
    renderPage();
    await screen.findByText("laptop");
    fireEvent.change(await screen.findByLabelText("Anthropic token for laptop"), {
      target: { value: "\u0000auto" },
    });
    // Waits for WHICHEVER announcement lands, then asserts which one it was. That is
    // what makes it fail in milliseconds: under the stale-closure mutation the wrong
    // announcement appears just as fast as the right one, so there is nothing to time
    // out on. Waiting only for the CORRECT string would burn the full waitFor timeout
    // before saying anything, which is the 5s non-explanation this test replaces.
    await screen.findByText(/from its next claim|token pool is empty/i);
    expect(
      screen.queryByText(/token pool is empty/i),
      "pooledCount is missing from rebind's useCallback deps: the callback captured the " +
        "count from the render that created it, so opting a token in mid-session does not " +
        "reach the announcement",
    ).toBeNull();
    expect(screen.getByText(/now auto-selects from your token pool/i)).toBeTruthy();
  });

  // A non-auto worker must be untouched by any of this: the empty pool is irrelevant
  // to a worker that was never going to consult it.
  it("leaves a default-mode worker's summary alone", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: twoUnpooled });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ anthropic_bind_mode: "default" })] });
    renderPage();
    expect(await screen.findByText(/spends your default token/i)).toBeTruthy();
    expect(screen.queryByText(/is empty — its runs spend/i)).toBeNull();
  });

  // F18's other half. A correct row summary beside a cheerful announcement would
  // leave the misleading half in the one place a screen-reader user actually hears
  // it, and the visual and the announced would then disagree.
  it("announces the empty pool when switching a worker to auto", async () => {
    mockApi.listSecrets.mockResolvedValue({ secrets: twoUnpooled });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ anthropic_bind_mode: "default" })] });
    mockApi.setWorkerBindMode.mockResolvedValue({
      worker: aWorker({ anthropic_bind_mode: "auto" }),
    });
    renderPage();
    await screen.findByText("laptop");
    const picker = await screen.findByLabelText("Anthropic token for laptop");
    // AUTO_OPTION is a NUL-prefixed sentinel, not a readable value — deliberately a
    // string no user label can collide with.
    fireEvent.change(picker, { target: { value: "\u0000auto" } });
    await waitFor(() => {
      expect(screen.getByText(/token pool is empty, so its runs spend your default token/i)).toBeTruthy();
    });
    // 🔴 AND IT IS AMBER, NOT GREEN. F18 made the two channels agree in WORDS and left
    // them disagreeing in COLOUR: the row span was text-warn while the announcement
    // went through one unconditional success Alert, so a GREEN banner read "your token
    // pool is empty, so its runs spend your default token". That is the same category
    // error one level down from the one F18 fixed, and this file's own argument is
    // that the visible and the announced must not disagree.
    const banner = screen.getByText(/token pool is empty, so its runs spend your default token/i);
    expect(banner.className, "a success-toned banner cannot carry a warning").toMatch(/text-warn\b/);
    expect(banner.className).not.toMatch(/text-ok\b/);
  });

  // The other branches stay green: nothing went wrong, and making every announcement
  // amber would cost the distinction this fix exists to create.
  it("keeps an ordinary rebind announcement green", async () => {
    mockApi.listSecrets.mockResolvedValue({
      secrets: [twoUnpooled[0], aSecret({ id: "sec-spare", label: "spare-key", auto_eligible: true })],
    });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ anthropic_bind_mode: "default" })] });
    mockApi.setWorkerBindMode.mockResolvedValue({ worker: aWorker({ anthropic_bind_mode: "auto" }) });
    renderPage();
    await screen.findByText("laptop");
    fireEvent.change(await screen.findByLabelText("Anthropic token for laptop"), {
      target: { value: "\u0000auto" },
    });
    const banner = await screen.findByText(/now auto-selects from your token pool/i);
    expect(banner.className).toMatch(/text-ok\b/);
  });
});


// --- worker names reach the DOM sanitized -----------------------------------------

describe("WorkersSettings sanitizes worker names (PRD #111 pre-PR)", () => {
  // 🔴 WORKER NAMES HAVE LESS PROTECTION THAN TOKEN LABELS, NOT MORE. They are
  // validated for LENGTH ONLY — TrimSpace plus a 200-byte cap — and `workers.name` is
  // a bare `text NOT NULL` with no CHECK, so a bidi override or a zero-width character
  // is storable in one.
  //
  // React escapes HTML, so there is no XSS here and that is not the claim. Escaping
  // does nothing to an RLO, which reorders the text AROUND it and can make a worker
  // render as one it is not — the F12 hazard closed for labels, left open for names.
  //
  // MUTATION THIS CATCHES: dropping the strip from the name span. Measured — reverting
  // it to a bare `{w.name}` reddens exactly this one test, and nothing else.
  //
  // The HELPER is stripUnsafeChars, not sanitizeLabel, and this line said the latter
  // until main's issue #124 work and this branch met in a merge. Both close the same
  // hazard on the same cell; stripUnsafeChars is the superset (Cc+Cf, sparing \n and
  // \t) where sanitizeLabel is Cf-only by design, because sanitizeLabel mirrors the Go
  // validateSecretLabel predicate for TOKEN LABELS and a name has no validator to
  // mirror. The fixture below plants a bidi override, which both strip, so the fixture
  // cannot tell them apart — the reason to prefer the superset is the bare ESC a name
  // can also carry, which only stripUnsafeChars removes.
  //
  // The join-token echo below has converged onto stripUnsafeChars like the other
  // worker.name cells in this file, so every site now agrees on the helper. The
  // argument above applies to it too, which is why it uses the same superset.
  //
  // The join-token echo. LOW severity and fixed anyway: the name here is the one the
  // user typed seconds ago in the same session, so a user can only spoof their own
  // immediate echo — no cross-tenant path, nothing stored-then-surprising, unlike the
  // list (any age) and the admin view (other people's). Same class, same one-word fix.
  //
  // MUTATION THIS CATCHES: reverting the echo to `{newToken.worker}`, AND reverting it
  // to sanitizeLabel. The fixture plants a bare ESC (U+001B, a Cc char) alongside the
  // bidi override (U+202E, a Cf char): both helpers strip the override, but only
  // stripUnsafeChars removes the ESC, so the ESC assertion goes red on a revert to the
  // Cf-only sanitizeLabel.
  it("strips invisible formatting characters from the join-token echo", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.createWorker.mockResolvedValue({
      worker: aWorker({ id: "w-new", name: "safe\u001B\u202Edrowssap" }),
      token: "uzi_wk_deadbeef",
    });
    const { container } = renderPage();
    fireEvent.change(await screen.findByPlaceholderText(/laptop, ci-runner-1/), {
      target: { value: "safe\u001B\u202Edrowssap" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Generate join token" }));
    await screen.findByText("uzi_wk_deadbeef");
    expect(container.textContent).not.toContain("\u202E");
    expect(container.textContent).not.toContain("\u001B");
    expect(container.textContent).toContain("safe");
  });

  it("strips invisible formatting characters from a worker name", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ name: "safe\u202Edrowssap" })],
    });
    const { container } = renderPage();
    await screen.findByText(/safe/);
    expect(container.textContent).not.toContain("\u202E");
    expect(container.textContent).toContain("safe");
  });
});
