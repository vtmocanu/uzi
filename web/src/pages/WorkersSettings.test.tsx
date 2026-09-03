// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router-dom";
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

// HostedWorkers (a child of this page) reads useAuth for the ephemeral toggle (PRD
// #649). These tests keep ephemeral_enabled off, so the toggle never renders; a null
// user is enough to keep the hook from throwing.
vi.mock("../auth/AuthContext", () => ({ useAuth: () => ({ user: null, refresh: vi.fn() }) }));

const mockApi = vi.mocked(api);

// Hosting off unless a test says otherwise: that is the default an instance ships
// with (PRD #58 Decision 12, compose is zero-diff), so the pre-#58 tests below render
// the page they always rendered.
beforeEach(() => {
  mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
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

// A probe that mirrors the router's current search string into the DOM, so a test can
// assert the URL after a tab switch (the tab param is written with { replace: true }).
function LocationProbe() {
  const { search } = useLocation();
  return <div data-testid="location-search">{search}</div>;
}

function renderPage(initialEntries?: string[]) {
  return render(
    <MemoryRouter initialEntries={initialEntries}>
      <WorkersSettings />
      <LocationProbe />
    </MemoryRouter>,
  );
}

// The add-panel content (join-token card, HostedWorkers, register form) lives behind a
// tab. When the fleet is non-empty the page lands on Your workers (D11), so reaching an
// add-panel control BY ROLE means clicking the "Add a worker" tab first — the inactive
// panel carries the real `hidden` attribute, which removes it from getByRole. Text
// queries still see hidden content, so those tests need no switch.
function openAddTab() {
  fireEvent.click(screen.getByRole("tab", { name: /^Add a worker$/ }));
}
// The inverse: Delete lives in the Your workers panel, so a test that provisioned or
// checked add-panel state and now needs a worker row switches back.
function openWorkersTab() {
  fireEvent.click(screen.getByRole("tab", { name: /^Your workers/ }));
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

  it("shows an ephemeral badge for an auto-provisioned hosted worker, and none otherwise (PRD #649 M4)", async () => {
    // Ephemeral (PRD #529/#649) is real TEXT — the word "ephemeral". true renders the
    // badge; false/undefined renders nothing (absence needs no badge), same convention
    // as the docker badge above.
    const ephemeralHosted = aWorker({ id: "w-he", name: "auto (M)", kind: "hosted", hosted_size: "m", ephemeral: true });
    const plainHosted = aWorker({ id: "w-hp", name: "base (S)", kind: "hosted", hosted_size: "s", ephemeral: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [ephemeralHosted, plainHosted] });
    renderPage();

    await screen.findByText("auto (M)");
    expect(screen.getByText("ephemeral")).toBeTruthy();
    // Exactly one ephemeral badge: the manually-provisioned worker (ephemeral:false)
    // shows none.
    expect(screen.getAllByText("ephemeral")).toHaveLength(1);

    // And a hosted worker with ephemeral undefined shows no badge either.
    cleanup();
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker({ id: "w-hu", name: "base (L)", kind: "hosted", hosted_size: "l" })] });
    renderPage();
    await screen.findByText("base (L)");
    expect(screen.queryByText("ephemeral")).toBeNull();
  });

  it("badges a hosted row even when hosting is switched off (never leave a row lying)", async () => {
    // An admin can turn hosting off while a user still holds hosted workers. The rows
    // must stay listed and deletable — and stay honest about what they are, or they
    // read as workers the user forgot to start.
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [hosted] });
    renderPage();
    expect(await screen.findByText("hosted")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Delete" })).toBeTruthy();
    // The card renders a single "Hosted workers" heading whenever it shows; hosting off
    // means it shows nothing at all.
    expect(screen.queryByText("Hosted workers")).toBeNull();
  });

  it("shows the provision card only when the instance has hosting on", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: fleet });
    renderPage();
    await screen.findByText("laptop");
    expect(screen.queryByText("Hosted workers")).toBeNull();

    cleanup();
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    renderPage();
    expect(await screen.findByText("Hosted workers")).toBeTruthy();
  });

  it("counts only hosted workers against the quota, not the whole fleet", async () => {
    // The count comes from the list the page already polls — three external workers
    // must not eat the hosted allowance.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
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

  it("keeps the join-token card across a tab round-trip and clears it on Done (M3)", async () => {
    // The token card must survive a tab switch (both panels stay mounted, D4) and clear only
    // on Done — it holds a secret shown exactly once.
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.createWorker.mockResolvedValue({
      worker: aWorker({ id: "w-new", name: "nas" }),
      token: "uzi_wk_deadbeef",
    });
    renderPage();

    // Empty fleet lands on the add tab, where the register form and token card live.
    fireEvent.change(await screen.findByPlaceholderText(/laptop, ci-runner-1/), { target: { value: "nas" } });
    fireEvent.click(screen.getByRole("button", { name: "Generate join token" }));
    expect(await screen.findByText("uzi_wk_deadbeef")).toBeTruthy();

    // Round-trip: add → workers → add. The card is still there (query by role excludes the
    // hidden panel, so this also proves the card is on the currently-shown add panel).
    openWorkersTab();
    openAddTab();
    expect(screen.getByText("uzi_wk_deadbeef")).toBeTruthy();

    // Done clears it.
    fireEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByText("uzi_wk_deadbeef")).toBeNull();
  });
});

describe("WorkersSettings — Option B header grouping (PRD #560)", () => {
  // jsdom has no layout engine, so "badges beside the name / controls on their own
  // row" cannot be asserted as visual adjacency. Option B is expressed instead as DOM
  // order and containment, in the style of the cordon-order test above.
  it("groups read-only badges before the controls, and keeps status→cordon→run order", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [
        aWorker({
          id: "w-opt-b",
          name: "opt-b-one",
          kind: "hosted",
          hosted_size: "m",
          // Cordoned (idle → "cordoned") with a cap above 1 so both the cordon pill and
          // the run badge render, letting us assert their order survives the regrouping.
          draining_since: "2026-08-21T12:00:00Z",
          active_runs: 0,
          max_concurrent_runs: 2,
        }),
      ],
    });
    // Two tokens so the per-worker token picker (Select) renders — it only appears when
    // there is more than one credential to choose between — letting us verify the MOVED
    // control actually landed in the controls row, not just that Delete did (CodeRabbit #653).
    mockApi.listSecrets.mockResolvedValue({
      secrets: [aSecret(), aSecret({ id: "sec-extra", label: "extra", is_default: false })],
    });
    renderPage();
    await screen.findByText("opt-b-one");

    const status = screen.getByText("online");
    const cordon = screen.getByText("cordoned");
    const run = screen.getByText("0/2 runs");
    const deleteBtn = screen.getByRole("button", { name: "Delete" });
    const select = screen.getByLabelText("Anthropic token for opt-b-one");

    // Read-only status cluster (Row 1) precedes the interactive Delete control (Row 2).
    expect(status.compareDocumentPosition(deleteBtn) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // status → cordon → run order within the badge cluster is unchanged.
    expect(status.compareDocumentPosition(cordon) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(cordon.compareDocumentPosition(run) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // Containment: the moved Select shares the controls row with Delete, and that row is
    // distinct from the read-only status/badge cluster. Walk to the nearest ancestor common
    // to Select and Delete (robust to any wrapper the Select/Button components add), then
    // assert the read-only status badge falls outside it.
    let controlsRow: HTMLElement | null = deleteBtn.parentElement;
    while (controlsRow && !controlsRow.contains(select)) controlsRow = controlsRow.parentElement;
    expect(controlsRow).not.toBeNull();
    expect(controlsRow!.contains(status)).toBe(false);
  });

  it("carries the wrap-safety classes on the identity wrapper and the badge cluster (M4)", async () => {
    // Structural proxy for wrap-not-overflow: jsdom cannot measure layout, but it can
    // confirm the load-bearing utilities are present. The badge cluster must be
    // flex-wrap (the pre-Option-B group was not) and the identity wrapper must let a
    // long name shrink/wrap (min-w-0 break-words) rather than shove the cluster.
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker({ id: "w-wrap", name: "wrap-check" })],
    });
    renderPage();
    const nameEl = await screen.findByText("wrap-check");

    // The name span's parent is the identity wrapper.
    const identityWrapper = nameEl.parentElement;
    expect(identityWrapper?.className).toContain("min-w-0");
    expect(identityWrapper?.className).toContain("break-words");

    // The status badge's parent is the read-only badge cluster.
    const badgeCluster = screen.getByText("online").parentElement;
    expect(badgeCluster?.className).toContain("flex-wrap");
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
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1, h2] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();

    // A non-empty fleet lands on Your workers (D11); the Provision button is on the add
    // tab, so open it to reach the control by role.
    await screen.findByText("base (M)");
    openAddTab();
    const provision = () => screen.getByRole("button", { name: /^Provision$/ });
    expect(await screen.findByText(/2 of 2 used/)).toBeTruthy();
    expect(provision().hasAttribute("disabled")).toBe(true);

    // Delete one → the page reloads the fleet → the count drops → the gate lifts.
    // Hosted deletes confirm first, so the journey out of the quota is now two clicks.
    // Delete happens on Your workers; the Provision gate is re-checked back on the add tab.
    openWorkersTab();
    mockApi.listWorkers.mockResolvedValue({ workers: [h1] });
    fireEvent.click(screen.getAllByRole("button", { name: "Delete" })[1]);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-h2"));
    openAddTab();
    expect(await screen.findByText(/1 of 2 used/)).toBeTruthy();
    await waitFor(() => expect(provision().hasAttribute("disabled")).toBe(false));
  });

  it("the at-quota 'delete one to provision another' link crosses to Your workers and focuses the first hosted row, not a Delete button (M3, D10)", async () => {
    const h1 = aWorker({ id: "w-h1", name: "base (S)", kind: "hosted", hosted_size: "s" });
    const h2 = aWorker({ id: "w-h2", name: "base (M)", kind: "hosted", hosted_size: "m" });
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1, h2] });
    renderPage();

    // A full fleet lands on Your workers; the at-quota link lives on the add tab.
    await screen.findByText("base (M)");
    openAddTab();
    const link = await screen.findByRole("button", { name: "delete one to provision another" });
    fireEvent.click(link);

    // It selects Your workers and parks focus on the FIRST hosted row's <li> container.
    expect(screen.getByRole("tab", { name: /^Your workers/ }).getAttribute("aria-selected")).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(document.getElementById("worker-row-w-h1")));
    // The focused element is the row container (an <li>), never a Delete button — the
    // never-park-on-a-destructor rule stands (D10).
    expect((document.activeElement as HTMLElement).tagName).toBe("LI");
    for (const b of screen.getAllByRole("button", { name: "Delete" })) {
      expect(document.activeElement).not.toBe(b);
    }
  });

  it("names each worker row via aria-labelledby pointing at the worker-name span (M4, a11y)", async () => {
    const h1 = aWorker({ id: "w-h1", name: "base (S)", kind: "hosted", hosted_size: "s" });
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1] });
    renderPage();

    await screen.findByText("base (S)");
    // The row <li> is focused programmatically by the at-quota jump; without an accessible
    // name a screen reader announces something AT-dependent on landing. aria-labelledby
    // resolves to the worker-name span (not a blanket aria-label, which would suppress the
    // badges), so the row is announced as the worker it holds.
    const row = document.getElementById("worker-row-w-h1")!;
    const labelledby = row.getAttribute("aria-labelledby");
    expect(labelledby).toBeTruthy();
    const nameEl = document.getElementById(labelledby!);
    expect(nameEl).not.toBeNull();
    expect(nameEl!.textContent).toBe("base (S)");
  });

  it("keeps the at-quota focus parked on the row across a 10s poll tick with an unchanged fleet (M3, Risks)", async () => {
    // Fake timers so the 10s liveness poll can be driven deterministically; the afterEach
    // restores real timers. Loads/config are flushed with advanceTimersByTimeAsync(0), so
    // this test uses getBy/queryBy after each flush rather than findBy (which needs real
    // timers).
    vi.useFakeTimers();
    const h1 = aWorker({ id: "w-h1", name: "base (S)", kind: "hosted", hosted_size: "s" });
    const h2 = aWorker({ id: "w-h2", name: "base (M)", kind: "hosted", hosted_size: "m" });
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [h1, h2] });
    renderPage();

    // Flush the full async chain: the Promise.all initial load, the landing-tab latch
    // effect, and HostedWorkers' separate one-shot config fetch — each resolves on its own
    // microtask/effect turn, so pump several 0ms ticks until the page settles.
    const flush = async () => {
      for (let i = 0; i < 6; i++) await vi.advanceTimersByTimeAsync(0);
    };
    await flush(); // initial load + landing-tab latch (2 workers → Your workers) + config fetch
    openAddTab();
    await flush(); // un-hide the add panel; the at-quota button now renders
    fireEvent.click(screen.getByRole("button", { name: "delete one to provision another" }));
    await flush(); // pendingRowFocus effect runs after the panel un-hides
    expect(document.activeElement).toBe(document.getElementById("worker-row-w-h1"));

    // The 10s liveness poll re-fetches the SAME fleet and re-renders the rows. This guards
    // that the poll does NOT remount the row subtree and drop the parked focus (rows are
    // keyed by w.id and pendingRowFocus was cleared after the jump, so the focus effect does
    // not re-fire) — focus must stay on the row the user landed on, never yanked to <body>
    // (M3 Risks: "focus survives one poll tick"). A random-key remount fails this test.
    await vi.advanceTimersByTimeAsync(10000);
    expect(document.activeElement).toBe(document.getElementById("worker-row-w-h1"));
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

  it("announces a provision, lands on Your workers, and takes focus (M3 batches tab+announce)", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "Provision" }));

    // New copy (M3), and the notice now lives on the Your workers panel, which the handler
    // un-hides in the SAME batch as the announce — so the focus lands. If the tab switch and
    // the announce were not batched the notice would render inside a still-hidden panel and
    // .focus() would silently no-op: this focus assertion is the regression guard for that.
    const msg = await screen.findByText("Provisioned base (S) — it is listed below and comes online on its own.");
    await waitFor(() =>
      expect(screen.getByRole("tab", { name: /^Your workers/ }).getAttribute("aria-selected")).toBe("true"),
    );
    // toBe the wrapper, not a textContent match: focus on <body> would match anything,
    // since body.textContent is the entire page.
    await waitFor(() => expect(document.activeElement).toBe(msg.parentElement));
  });

  it("a delete REPLACES the provision notice — it must not outlive the row it describes", async () => {
    // The bug this exists for: provision, delete, and the page still said "it appears in
    // your workers below" about a row that was gone. Nothing cleared it but the NEXT
    // provision, so the only message left in a live region was the false one.
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 2, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValueOnce({ workers: [] }).mockResolvedValue({ workers: [provisioned] });
    mockApi.provisionHostedWorker.mockResolvedValue({ worker: provisioned });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();

    // Empty fleet lands on the add tab (D11), where the Provision button lives. The M3
    // handler switches to Your workers as it announces, so the row it describes is already
    // visible where Delete lives.
    fireEvent.click(await screen.findByRole("button", { name: "Provision" }));
    expect(await screen.findByText(/Provisioned base \(S\)/)).toBeTruthy();
    const del = await screen.findByRole("button", { name: "Delete" });

    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // gone after the delete
    fireEvent.click(del);
    fireEvent.click(await screen.findByRole("button", { name: "Delete anyway" }));

    expect(await screen.findByText("Deleted base (S).")).toBeTruthy();
    // The provision notice must not outlive the row it named (new M3 copy).
    expect(screen.queryByText(/it is listed below/)).toBeNull();
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
  // because such a worker HOLDS every claim in pool_wait rather than spending the default.
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
    // Every claim HOLDS in pool_wait rather than spending the owner's default (#754).
    // Saying "auto-selects from your token pool" here is R7's silent no-op moved up one
    // level, on the surface where the choice is actually made.
    // Matched on ONE text node: `token pool` is a <Link>, so the sentence is split
    // across elements and a regex spanning them finds nothing.
    expect(await screen.findByText(/is empty — its runs will hold until you add a token/i)).toBeTruthy();
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
    expect(screen.queryByText(/is empty — its runs will hold/i)).toBeNull();
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
      expect(
        screen.getByText(/token pool is empty, so its runs will hold until you add a token/i),
      ).toBeTruthy();
    });
    // 🔴 AND IT IS AMBER, NOT GREEN. F18 made the two channels agree in WORDS and left
    // them disagreeing in COLOUR: the row span was text-warn while the announcement
    // went through one unconditional success Alert, so a GREEN banner read "your token
    // pool is empty, so its runs will hold until you add a token to the pool". That is
    // the same category error one level down from the one F18 fixed, and this file's own
    // argument is that the visible and the announced must not disagree.
    const banner = screen.getByText(/token pool is empty, so its runs will hold until you add a token/i);
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

// \u2500\u2500 Two-tab page: tablist, landing tab, deep link, panels (PRD #1063 M1) \u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500\u2500
describe("WorkersSettings \u2014 two tabs (PRD #1063 M1)", () => {
  const selected = (name: RegExp) =>
    screen.getByRole("tab", { name }).getAttribute("aria-selected");

  it("renders a two-tab tablist labelled Workers, with the live count on Your workers", async () => {
    mockApi.listWorkers.mockResolvedValue({
      workers: [aWorker(), aWorker({ id: "w2", name: "ci" })],
    });
    renderPage();
    await screen.findByText("laptop");

    expect(screen.getByRole("tablist", { name: "Workers" })).toBeTruthy();
    const tabs = screen.getAllByRole("tab");
    expect(tabs).toHaveLength(2);
    // `Your workers \u00B7 N` (Schedules `\u00B7 count` convention), Add a worker exact.
    expect(tabs[0].textContent).toBe("Your workers \u00B7 2");
    expect(tabs[1].textContent).toBe("Add a worker");
  });

  it("lands on Your workers with a non-empty fleet; the add panel is hidden and one tabpanel shows", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");

    expect(selected(/^Your workers/)).toBe("true");
    expect(selected(/^Add a worker$/)).toBe("false");
    // `hidden` removes the inactive panel from getByRole and the a11y tree (D4), so
    // exactly one tabpanel is queryable, and it is the Your workers one.
    const panels = screen.getAllByRole("tabpanel");
    expect(panels).toHaveLength(1);
    expect(panels[0].getAttribute("aria-labelledby")).toBe("workers-tab-workers");
    // The add panel is present in the DOM but carries the real `hidden` attribute (not a
    // CSS class), which is what keeps it out of the a11y tree.
    expect(document.getElementById("workers-panel-add")?.hasAttribute("hidden")).toBe(true);
  });

  it("lands on Add a worker with an empty fleet (D11) and omits the count suffix", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    renderPage();
    await screen.findByText("Register your own worker");

    expect(selected(/^Add a worker$/)).toBe("true");
    const yourTab = screen.getByRole("tab", { name: /^Your workers$/ });
    expect(yourTab.getAttribute("aria-selected")).toBe("false");
    expect(yourTab.textContent).toBe("Your workers"); // suffix omitted at 0
  });

  it("selects no tab and shows no panel before the first load resolves", async () => {
    let resolveWorkers: (v: { workers: Worker[] }) => void = () => {};
    mockApi.listWorkers.mockReturnValue(
      new Promise<{ workers: Worker[] }>((r) => {
        resolveWorkers = r;
      }),
    );
    renderPage();

    const tabs = await screen.findAllByRole("tab");
    expect(tabs).toHaveLength(2);
    tabs.forEach((t) => expect(t.getAttribute("aria-selected")).toBe("false"));
    // Both panels hidden behind the skeleton until the landing tab is latched.
    expect(screen.queryByRole("tabpanel")).toBeNull();

    // Resolve so the pending load settles inside act (empty \u2192 add tab latches).
    resolveWorkers({ workers: [] });
    await screen.findByText("Register your own worker");
  });

  it("keeps Your workers selected when the last worker is deleted (1 \u2192 0)", async () => {
    const w = aWorker({ id: "w-x", name: "laptop" }); // external \u2192 one-click delete
    mockApi.listWorkers
      .mockResolvedValueOnce({ workers: [w] })
      .mockResolvedValue({ workers: [] });
    mockApi.deleteWorker.mockResolvedValue(null);
    renderPage();
    await screen.findByText("laptop");
    expect(selected(/^Your workers/)).toBe("true");

    fireEvent.click(screen.getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(mockApi.deleteWorker).toHaveBeenCalledWith("w-x"));
    await screen.findByText(/No workers yet/);

    // The empty state shows on Your workers; the selection did not move on its own.
    expect(screen.getByRole("tab", { name: /^Your workers$/ }).getAttribute("aria-selected")).toBe("true");
  });

  it("keeps Add a worker selected when a first worker arrives via the poll (0 \u2192 1)", async () => {
    vi.useFakeTimers();
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    renderPage();
    // Flush the initial load's promise chain AND the landing-tab latch effect (several
    // microtask/render rounds resolve the fetch, commit loading:false, then run the latch).
    for (let i = 0; i < 5; i++) await vi.advanceTimersByTimeAsync(0);
    expect(selected(/^Add a worker$/)).toBe("true");

    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    await vi.advanceTimersByTimeAsync(10000); // one poll interval

    // A worker arriving mid-session must not yank the user off the add tab.
    expect(selected(/^Add a worker$/)).toBe("true");
    expect(selected(/^Your workers/)).toBe("false");
  });

  it("moves selection and focus by click and by ArrowRight/ArrowLeft (wrap), Home and End", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    const workersTab = screen.getByRole("tab", { name: /^Your workers/ });
    const addTab = screen.getByRole("tab", { name: /^Add a worker$/ });

    // Click selects (order is [workers, add]).
    fireEvent.click(addTab);
    expect(addTab.getAttribute("aria-selected")).toBe("true");

    // ArrowRight from add(1) wraps to workers(0); focus follows (automatic activation).
    fireEvent.keyDown(addTab, { key: "ArrowRight" });
    expect(workersTab.getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(workersTab);

    // ArrowLeft from workers(0) wraps to add(1).
    fireEvent.keyDown(workersTab, { key: "ArrowLeft" });
    expect(addTab.getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(addTab);

    // Home jumps to the first tab, End to the last.
    fireEvent.keyDown(addTab, { key: "Home" });
    expect(workersTab.getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(workersTab);

    fireEvent.keyDown(workersTab, { key: "End" });
    expect(addTab.getAttribute("aria-selected")).toBe("true");
    expect(document.activeElement).toBe(addTab);
  });

  it("?tab=add selects the add tab on load even with a non-empty fleet", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage(["/workers?tab=add"]);
    await screen.findByText("Register your own worker");

    expect(selected(/^Add a worker$/)).toBe("true");
    expect(selected(/^Your workers/)).toBe("false");
  });

  it("?tab=workers selects Your workers even with an empty fleet", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    renderPage(["/workers?tab=workers"]);
    await screen.findByText(/No workers yet/);

    expect(selected(/^Your workers$/)).toBe("true");
    expect(selected(/^Add a worker$/)).toBe("false");
  });

  it("writes the selected tab to the URL param (replace: true keeps it shareable)", async () => {
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    // Landing does not write the param; only an explicit switch does.
    expect(screen.getByTestId("location-search").textContent).toBe("");

    fireEvent.click(screen.getByRole("tab", { name: /^Add a worker$/ }));
    expect(screen.getByTestId("location-search").textContent).toBe("?tab=add");

    fireEvent.click(screen.getByRole("tab", { name: /^Your workers/ }));
    expect(screen.getByTestId("location-search").textContent).toBe("?tab=workers");
  });

  it("orders the Hosted workers card before the register card in the add panel", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // empty \u2192 add tab
    renderPage();

    const hosted = await screen.findByText("Hosted workers");
    const register = await screen.findByText("Register your own worker");
    // Hosted first (D3): the primary worker path leads.
    expect(
      hosted.compareDocumentPosition(register) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
  });
});

// ── Header action + hosted-first empty-state CTAs (PRD #1063 M2) ────────────────
describe("WorkersSettings — header button + empty-state CTAs (PRD #1063 M2)", () => {
  const selected = (name: RegExp) =>
    screen.getByRole("tab", { name }).getAttribute("aria-selected");
  const hostedTemplate = () => document.getElementById("hosted-worker-template");
  const registerName = () => document.getElementById("register-worker-name");

  it("header button selects the add tab and focuses the hosted Template select when hosting is on", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] }); // non-empty → lands on Your workers
    renderPage();
    await screen.findByText("laptop");
    // The hosted select existing implies the config resolved and onAvailability fired, so
    // hostedManual is set true (both happen in the same fetch .then).
    await waitFor(() => expect(hostedTemplate()).toBeTruthy());

    fireEvent.click(screen.getByRole("button", { name: /^Add a worker$/ }));
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(hostedTemplate()));
  });

  it("header button focuses the register Name input when hosting is off", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    // Hosting off: no hosted select renders anywhere.
    expect(hostedTemplate()).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /^Add a worker$/ }));
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(registerName()));
  });

  it("header button focuses the register Name input when the config read rejects (fail closed)", async () => {
    mockApi.hostedConfig.mockRejectedValue(new Error("boom"));
    mockApi.listWorkers.mockResolvedValue({ workers: [aWorker()] });
    renderPage();
    await screen.findByText("laptop");
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());

    fireEvent.click(screen.getByRole("button", { name: /^Add a worker$/ }));
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(registerName()));
  });

  it("empty state leads with the hosted CTA when hosting is on; the hosted CTA focuses the Template select", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // empty → lands on add tab (D11)
    renderPage();
    // Config resolved → hostedManual true, so the empty state renders the hosted CTA.
    await waitFor(() => expect(hostedTemplate()).toBeTruthy());
    openWorkersTab(); // the empty state lives on the Your workers panel

    const provisionCta = await screen.findByRole("button", { name: "Provision a hosted worker" });
    expect(screen.getByRole("button", { name: "Register your own" })).toBeTruthy();
    // The hosting-off single-button copy is absent when hosting is on.
    expect(screen.queryByRole("button", { name: "Register a worker" })).toBeNull();

    fireEvent.click(provisionCta);
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(hostedTemplate()));
  });

  it("the Register your own CTA selects the add tab and focuses the register Name input", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: true, quota: 5, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [] });
    renderPage();
    await waitFor(() => expect(hostedTemplate()).toBeTruthy());
    openWorkersTab();

    fireEvent.click(await screen.findByRole("button", { name: "Register your own" }));
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(registerName()));
  });

  it("empty state shows only Register a worker (no hosted CTA) when hosting is off, and it focuses the Name input", async () => {
    mockApi.hostedConfig.mockResolvedValue({ enabled: false, quota: 0, ephemeral_enabled: false });
    mockApi.listWorkers.mockResolvedValue({ workers: [] }); // empty → add tab
    renderPage();
    await screen.findByText(/No workers yet/); // text queries see the hidden Your-workers panel
    await waitFor(() => expect(mockApi.hostedConfig).toHaveBeenCalled());
    openWorkersTab();

    expect(await screen.findByRole("button", { name: "Register a worker" })).toBeTruthy();
    expect(screen.queryByRole("button", { name: "Provision a hosted worker" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Register your own" })).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: "Register a worker" }));
    expect(selected(/^Add a worker$/)).toBe("true");
    await waitFor(() => expect(document.activeElement).toBe(registerName()));
  });
});
