// @vitest-environment jsdom
import { afterEach, beforeEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import cases from "../../../fixtures/recovery-prerequisite-closure/cases.json";
import { RecoveryArchivesPanel } from "./RecoveryArchives";
import { api, type RecoveryArchive, type RecoveryArchiveSummary, type Run } from "../lib/api";

// The panel fetches only its owner-scoped summary. Spy on holds to forbid a new read;
// keep runArchiveDownloadUrl and the recovery display helpers real, since the whole
// point of these pins is the real render path (D6/D7).
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { getRunArchives: vi.fn(), getRecoveryHolds: vi.fn(), discardRunArchive: vi.fn() },
  };
});
const mockApi = vi.mocked(api);
const RUN_UUID = "00000000-0000-0000-0000-000000000002";
const CAP_UUID = "00000000-0000-0000-0000-000000000001";
const EXPORT_CMD = `uzi run export ${RUN_UUID} --output <path> --capture ${CAP_UUID}`;

beforeEach(() => {
  mockApi.getRecoveryHolds.mockReset();
});

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// A minimal run fixture matches the repo convention
// (SteerQueueCard.test.tsx) rather than spelling out the whole Run.
function aRun(over: Partial<Run> = {}): Run {
  return { id: "r1", status: "failed", ...over } as Run;
}

function archive(over: Partial<RecoveryArchive> = {}): RecoveryArchive {
  return {
    id: "cap1",
    run_id: "r1",
    hold_id: "h1",
    state: "available",
    source_sha: "0123456789abcdef0123456789abcdef01234567",
    created_at: "2026-09-13T00:00:00Z",
    checksum: "a".repeat(64),
    byte_size: 123,
    ...over,
  };
}

function summary(over: Partial<RecoveryArchiveSummary> = {}): RecoveryArchiveSummary {
  return {
    supported: true,
    legacy: false,
    has_open_hold: false,
    counts: {
      preparing: 0,
      uploading: 0,
      available: 0,
      needs_action: 0,
      expired: 0,
      discarded: 0,
    },
    archives: [],
    ...over,
  };
}

// Render the panel and wait for its async fetch + state update to settle. The panel returns
// null until the summary resolves, so a bare render would race the effect.
async function renderPanel(run: Run, s: RecoveryArchiveSummary) {
  mockApi.getRunArchives.mockResolvedValue(s);
  const utils = render(<RecoveryArchivesPanel run={run} />);
  await waitFor(() => expect(mockApi.getRunArchives).toHaveBeenCalledWith(run.id));
  // Flush the resolved promise's .then so the setSummary re-render lands before assertions.
  await act(async () => {
    await Promise.resolve();
  });
  return utils;
}

const DIAGNOSTIC = "terminal record rejected after restart (MAC failure); completion is unverified; see run recovery for source custody";
const SECRET_WARNING = /Treat every archive as if it contains secrets/;

describe("RecoveryArchivesPanel fixed failed-run copy", () => {
  it("uses only the exact failed-run reason and suppresses inferred zero-row preparation", async () => {
    await renderPanel(aRun({ failure_reason: DIAGNOSTIC }), summary({ has_open_hold: true }));
    expect(screen.getByText(DIAGNOSTIC).textContent).toBe(DIAGNOSTIC);
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
    expect(screen.queryByText(/Preparing the recovery archive/)).toBeNull();
    expect(screen.queryByText(/No server archive exists|Decision required|gen 7|hold h1/)).toBeNull();
    expect(screen.queryByRole("link", { name: "Export archive" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Export archive" })).toBeNull();
    expect(screen.queryByText(/uzi run export/)).toBeNull();
  });

  it.each([
    "mac_failure",
    "unknown MAC failure",
    DIAGNOSTIC + " ",
    " " + DIAGNOSTIC,
    DIAGNOSTIC.toUpperCase(),
  ])("does not interpret an unknown or approximate reason: %s", async (failure_reason) => {
    await renderPanel(aRun({ failure_reason }), summary({ has_open_hold: true }));
    expect(screen.getByText(/Preparing the recovery archive/)).toBeTruthy();
    expect(screen.queryByText(DIAGNOSTIC)).toBeNull();
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it("does not show rejection copy for a non-failed run", async () => {
    await renderPanel(aRun({ status: "completed", failure_reason: DIAGNOSTIC }),
      summary({ has_open_hold: true }));
    expect(screen.getByText(/Preparing the recovery archive/)).toBeTruthy();
    expect(screen.queryByText(DIAGNOSTIC)).toBeNull();
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it("keeps actual available capture export and the CLI hint", async () => {
    await renderPanel(aRun({ id: RUN_UUID, failure_reason: DIAGNOSTIC, landing_state: "needs_landing" }),
      summary({ archives: [archive({ id: CAP_UUID })] }));
    expect(screen.getByText(DIAGNOSTIC)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Export archive" }).getAttribute("href"))
      .toBe(`/api/runs/${RUN_UUID}/archives/${CAP_UUID}/download`);
    expect(screen.getByText(EXPORT_CMD)).toBeTruthy();
    expect(screen.getByText(SECRET_WARNING)).toBeTruthy();
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it.each([
    ["preparing", "Preparing"],
    ["uploading", "Uploading"],
    ["needs_action", "Needs action"],
  ] as const)("preserves an actual %s capture without enabling export", async (state, label) => {
    await renderPanel(aRun({ failure_reason: DIAGNOSTIC, landing_state: "needs_landing" }),
      summary({ has_open_hold: true, archives: [archive({ state })] }));
    expect(screen.getByText(DIAGNOSTIC)).toBeTruthy();
    expect(screen.getByText(label)).toBeTruthy();
    expect(screen.queryByRole("link", { name: "Export archive" })).toBeNull();
    expect((screen.getByRole("button", { name: "Export archive" }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByText(/uzi run export/)).toBeNull();
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it("hides the panel when the owner summary fails even with the fixed reason", async () => {
    mockApi.getRunArchives.mockRejectedValueOnce(new Error("404"));
    const { container } = render(<RecoveryArchivesPanel run={aRun({ failure_reason: DIAGNOSTIC })} />);
    await act(async () => { await Promise.resolve(); });
    expect(mockApi.getRunArchives).toHaveBeenCalledWith("r1");
    expect(container.innerHTML).toBe("");
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it("clears a loaded summary immediately when changing runs", async () => {
    const { rerender, container } = await renderPanel(aRun({ failure_reason: DIAGNOSTIC }),
      summary({ archives: [archive()] }));
    expect(screen.getByRole("link", { name: "Export archive" })).toBeTruthy();
    let resolveNext!: (value: RecoveryArchiveSummary) => void;
    mockApi.getRunArchives.mockImplementationOnce(() => new Promise((resolve) => { resolveNext = resolve; }));
    rerender(<RecoveryArchivesPanel run={aRun({ id: "r2", status: "completed" })} />);
    expect(container.innerHTML).toBe("");
    await act(async () => { resolveNext(summary()); });
    expect(container.innerHTML).toBe("");
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });

  it("ignores a late summary from the previous run after the new owner's summary fails", async () => {
    let resolveOld!: (value: RecoveryArchiveSummary) => void;
    mockApi.getRunArchives.mockImplementationOnce(() => new Promise((resolve) => { resolveOld = resolve; }));
    const { rerender, container } = render(<RecoveryArchivesPanel run={aRun({ failure_reason: DIAGNOSTIC })} />);
    mockApi.getRunArchives.mockRejectedValueOnce(new Error("404"));
    rerender(<RecoveryArchivesPanel run={aRun({ id: "r2", failure_reason: DIAGNOSTIC })} />);
    await act(async () => { resolveOld(summary({ archives: [archive()] })); });
    expect(mockApi.getRunArchives).toHaveBeenCalledWith("r2");
    expect(container.innerHTML).toBe("");
    expect(mockApi.getRecoveryHolds).not.toHaveBeenCalled();
  });
});

describe("RecoveryArchivesPanel — zero-capture truthfulness", () => {
  it("shows the legacy/unsupported note on a failed run recovery never armed for", async () => {
    await renderPanel(aRun({ status: "failed" }), summary({ supported: false, legacy: true }));
    expect(screen.getByText(/Durable recovery was not available for this run/)).toBeTruthy();
    // No capture list, so no download surface and no secret warning.
    expect(screen.queryByText(SECRET_WARNING)).toBeNull();
    expect(screen.queryByRole("button", { name: "Export archive" })).toBeNull();
  });

  it("shows the preparing note on a terminal run with an open hold and no capture", async () => {
    await renderPanel(aRun({ status: "failed" }), summary({ has_open_hold: true }));
    expect(screen.getByText(/Preparing the recovery archive/)).toBeTruthy();
    expect(screen.queryByText(SECRET_WARNING)).toBeNull();
  });

  it("renders nothing on a healthy run with neither a capture nor a hold", async () => {
    const { container } = await renderPanel(
      aRun({ status: "completed" }),
      summary({ supported: true, has_open_hold: false }),
    );
    // The fetch resolved (waitFor above), and the section still chose to say nothing.
    expect(container.innerHTML).toBe("");
    expect(screen.queryByText("Recovery archives")).toBeNull();
  });
});

describe("RecoveryArchivesPanel — download gating (D7)", () => {
  it("enables the download only for an 'available' capture, disabling every other state", async () => {
    await renderPanel(
      aRun({ status: "failed" }),
      summary({
        archives: [
          archive({ id: "cap-needs", state: "needs_action", source_sha: "aaaaaaaaaaaa1111" }),
          archive({ id: "cap-ok", state: "available", source_sha: "bbbbbbbbbbbb2222", prerequisite_shas: ["cccccccccccc3333"] }),
        ],
      }),
    );

    // Exactly ONE download control is a link — the available capture's — proving the other
    // state did not render a usable download. If the gate were removed both rows would link.
    const links = screen.getAllByRole("link", { name: "Export archive" });
    expect(links).toHaveLength(1);
    expect(links[0].getAttribute("href")).toBe("/api/runs/r1/archives/cap-ok/download");
    // a11y: the enabled control is a SINGLE <a> styled as a button (its download attribute
    // kept), never an <a> wrapping a <Button> — one tab stop, one screen-reader announcement.
    expect(links[0].tagName).toBe("A");
    expect(links[0].querySelector("button")).toBeNull();
    expect(links[0].hasAttribute("download")).toBe(true);
    // The link's own row is the available one.
    const availableRow = within(links[0].closest("li") as HTMLElement);
    expect(availableRow.getByText("Available")).toBeTruthy();
    expect(availableRow.getByText("Prerequisites:")).toBeTruthy();
    expect(availableRow.getByText(/required commits must already exist in your destination clone/)).toBeTruthy();
    expect(availableRow.getByText(/run git bundle verify <bundle-path> before import/)).toBeTruthy();
    expect(availableRow.getByText(/Byte count and checksum alone cannot prove independent recovery/)).toBeTruthy();

    // The non-downloadable row renders a DISABLED button (not a link), so there is exactly
    // one "Export archive" button and it is disabled and not inside a link.
    const buttons = screen.getAllByRole("button", { name: "Export archive" });
    expect(buttons).toHaveLength(1);
    expect((buttons[0] as HTMLButtonElement).disabled).toBe(true);
    expect(buttons[0].closest("a")).toBeNull();
    expect(within(buttons[0].closest("li") as HTMLElement).getByText("Needs action")).toBeTruthy();
  });

  it("shows the secret-review warning on the download surface whenever captures render", async () => {
    await renderPanel(aRun({ status: "failed" }), summary({ archives: [archive({ state: "available" })] }));
    expect(screen.getByText(SECRET_WARNING)).toBeTruthy();
  });
});

describe("RecoveryArchivesPanel — Delete archive (D7/D9)", () => {
  it("gates Delete archive behind a warn-only confirmation that recommends export first, then calls discardRunArchive and reloads", async () => {
    // First fetch: one available capture. Second fetch (after delete): empty, so the row
    // clears — proving the panel reloads from the server rather than optimistically mutating.
    mockApi.getRunArchives
      .mockResolvedValueOnce(summary({ archives: [archive({ id: "cap-ok", state: "available" })] }))
      .mockResolvedValueOnce(summary({ archives: [] }));
    mockApi.discardRunArchive.mockResolvedValue({ discarded: true });

    render(<RecoveryArchivesPanel run={aRun({ status: "failed" })} />);
    await waitFor(() => expect(mockApi.getRunArchives).toHaveBeenCalled());
    await act(async () => {
      await Promise.resolve();
    });

    // The confirmation is not shown until the owner asks for it.
    expect(screen.queryByText(/permanently deletes the archived committed history/)).toBeNull();

    fireEvent.click(screen.getByRole("button", { name: /Delete archive/ }));

    // The warning recommends exporting first (D9), and nothing has been deleted yet.
    expect(screen.getByText(/export it first if you might need it/)).toBeTruthy();
    expect(mockApi.discardRunArchive).not.toHaveBeenCalled();

    // Confirm — while confirming, the trigger unmounts, so the only "Delete archive" button
    // left is the solid confirm inside the warning panel.
    fireEvent.click(screen.getByRole("button", { name: /Delete archive/ }));
    await waitFor(() => expect(mockApi.discardRunArchive).toHaveBeenCalledWith("r1", "cap-ok"));
    // The reload replaces the list with the empty second fetch, so the capture is gone.
    await waitFor(() => expect(screen.queryByText("Available")).toBeNull());
  });

  it("does not offer Delete archive for an in-flight (uploading) capture", async () => {
    await renderPanel(
      aRun({ status: "failed" }),
      summary({ archives: [archive({ id: "cap-up", state: "uploading" })] }),
    );
    expect(screen.queryByRole("button", { name: /Delete archive/ })).toBeNull();
  });

  it("cancelling the confirmation performs no mutation", async () => {
    await renderPanel(
      aRun({ status: "failed" }),
      summary({ archives: [archive({ id: "cap-ok", state: "available" })] }),
    );
    fireEvent.click(screen.getByRole("button", { name: /Delete archive/ }));
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText(/permanently deletes the archived committed history/)).toBeNull();
    expect(mockApi.discardRunArchive).not.toHaveBeenCalled();
  });
});

describe("RecoveryArchivesPanel — untrusted-text escaping (D6)", () => {
  // Hostile characters are written as \u ESCAPES, never literals — a literal U+202E would
  // reorder THIS source in a reviewer's editor (the very attack), and the repo forbids raw
  // invisible bytes in source (safeText.test.ts makes the same choice for the same reason).
  const ZWSP = String.fromCharCode(0x200b); // ZERO WIDTH SPACE
  const RLO = String.fromCharCode(0x202e); // RIGHT-TO-LEFT OVERRIDE

  // A hostile capture reason carrying BOTH an HTML-injection payload and invisible chars.
  // React escapes HTML on its own; only stripUnsafeChars removes the invisible chars — the
  // two tests below deliberately distinguish the two guards, so removing either reddens.
  const HOSTILE_REASON = `boom<img src=x onerror="steal()">wo${ZWSP}rd${RLO}evil`;

  it("never injects HTML from a capture reason (React escaping)", async () => {
    const { container } = await renderPanel(
      aRun({ status: "failed" }),
      summary({ archives: [archive({ state: "needs_action", reason: HOSTILE_REASON })] }),
    );
    // HTML-INJECTION-SAFE: no real <img> element was created from the payload...
    expect(container.querySelector("img")).toBeNull();
    // ...and the tag is present as literal, visible text instead of markup.
    expect(container.textContent).toContain('<img src=x onerror="steal()">');
  });

  it("strips invisible/control characters from a capture reason (stripUnsafeChars)", async () => {
    await renderPanel(
      aRun({ status: "failed" }),
      summary({ archives: [archive({ state: "needs_action", reason: HOSTILE_REASON })] }),
    );
    const reasonEl = screen.getByText(/boom<img/);
    const text = reasonEl.textContent ?? "";
    // INVISIBLE-CHAR-STRIPPED: the zero-width space and the RTL override are gone — a proof
    // distinct from HTML escaping, which would leave both in place.
    expect(text).not.toContain(ZWSP);
    expect(text).not.toContain(RLO);
    // The letters they hid between are now contiguous.
    expect(text).toContain("word");
    expect(text).toContain("evil");
  });

  it("escapes and sanitizes the source_sha the row renders, too", async () => {
    // shortSha only trims >12 chars, so keep the payload short enough to survive to the
    // render site: a full <img> tag plus a zero-width space, 7 chars total.
    const { container } = await renderPanel(
      aRun({ status: "failed" }),
      summary({ archives: [archive({ state: "available", source_sha: `<img>${ZWSP}z` })] }),
    );
    // No <img> from the source_sha (rendered twice — the badge row and "Original head").
    expect(container.querySelector("img")).toBeNull();
    // The literal tag survives as text, and the zero-width space does not.
    const shaEls = screen.getAllByText(/<img>z/);
    expect(shaEls.length).toBeGreaterThan(0);
    for (const el of shaEls) {
      expect(el.textContent ?? "").not.toContain(ZWSP);
    }
  });
});

// Issue #1418: for a run whose server-derived landing_state is "needs_landing", each AVAILABLE
// capture surfaces its id as text plus a copyable `uzi run export <run-id> --output <path>
// --capture <id>` CLI hint alongside the download link. It is doubly recoverability-aware:
// gated on needs_landing (the panel) AND on an available capture (the row), so it never
// appears where export could read nothing, and it leaves the ordinary recovery render
// (non-needs_landing runs whose captures were retained) untouched.
describe("RecoveryArchivesPanel — needs_landing export hint (issue #1418)", () => {


  it("surfaces the capture id and a copyable export command beside an available capture", async () => {
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });

    await renderPanel(
      aRun({ id: RUN_UUID, status: "failed", landing_state: "needs_landing" }),
      summary({ archives: [archive({ id: CAP_UUID, state: "available" })] }),
    );

    // The capture id is surfaced as text, and the CLI hint carries the REAL run + capture ids
    // (server UUIDs) with a literal <path> placeholder for the operator to fill in.
    expect(screen.getByText(/Capture id:/)).toBeTruthy();
    expect(screen.getByText(EXPORT_CMD)).toBeTruthy();

    // Copyable: clicking Copy writes the exact command to the clipboard and reflects the state.
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith(EXPORT_CMD));
    expect(await screen.findByRole("button", { name: "Copied" })).toBeTruthy();

    // The existing download link is intact — the hint sits ALONGSIDE it, not instead of it.
    expect(screen.getByRole("link", { name: "Export archive" })).toBeTruthy();
  });

  it("does not show the hint for a non-available capture, even on a needs_landing run", async () => {
    // The row gate is view.downloadable (only the `available` state), so a retained-but-not-
    // downloadable capture on a needs_landing run still shows no export command.
    await renderPanel(
      aRun({ status: "failed", landing_state: "needs_landing" }),
      summary({ archives: [archive({ id: "cap-na", state: "needs_action" })] }),
    );
    expect(screen.queryByText(/uzi run export/)).toBeNull();
    expect(screen.queryByText(/Capture id:/)).toBeNull();
  });

  it("leaves the ordinary recovery render unchanged for a non-needs_landing run with an available capture", async () => {
    // A completed run whose captures were retained (landing_state !== needs_landing): the panel
    // gate is off, so no CLI hint — the download link and the rest of the row are unchanged.
    await renderPanel(
      aRun({ status: "completed", landing_state: "none" }),
      summary({ archives: [archive({ state: "available" })] }),
    );
    expect(screen.getByRole("link", { name: "Export archive" })).toBeTruthy();
    expect(screen.queryByText(/uzi run export/)).toBeNull();
    expect(screen.queryByText(/Capture id:/)).toBeNull();
  });
});


// Feed the actual owner DTO, not DB hints, through the public panel.
describe("prerequisite closure owner projection", () => {
  it.each(["preparing", "uploading", "needs_action"].flatMap(state =>
    [false, true].flatMap(bound =>
      [false, true].map(emptyList => ({ state, bound, emptyList })),
    ),
  ))("$state bound=$bound emptyList=$emptyList renders only established self-containment", async ({ state, bound, emptyList }) => {
    await renderPanel(aRun(), summary({ archives: [archive({
      state,
      prerequisite_shas: emptyList ? [] : undefined,
      checksum: bound ? "a".repeat(64) : undefined,
      byte_size: bound ? 123 : undefined,
    })] }));
    const section = screen.getByLabelText("Prerequisite protection");
    expect(section.textContent).toContain(bound ? "Self-contained archive" : "Prerequisite protection unknown");
    expect(section.textContent!.includes("Self-contained archive")).toBe(bound);
    expect(section.textContent).not.toContain("git fetch origin");
  });

  const explanations: Record<string, RegExp> = {
    pending: /Pin pending: prerequisite protection is not established/,
    invoked: /Pin creation invoked: prerequisite protection is not established/,
    create_unknown: /Pin creation outcome unknown: prerequisite protection is not established/,
    pinned: /Pin recorded by the server/,
    refused: /Pin creation refused/,
    foreign: /A foreign ref occupies the pin name/,
    lost: /The recorded pin was lost/,
    deleted: /Pin deletion was recorded/,
  };
  it.each(cases.cases)("$name preserves semantics and independent archive actions", async c => {
    const capture = archive({ id: CAP_UUID, state: c.input.capture_state,
      prerequisite_shas: c.input.thin ? [cases.pin_tip] : [],
      ...(c.expected.closure ? { prerequisite_closure: c.expected.closure } : {}),
    });
    await renderPanel(aRun({ id: RUN_UUID, status: "completed", landing_state: "needs_landing" }),
      summary({ has_open_hold: false, archives: [capture] }));
    const section = screen.getByLabelText("Prerequisite protection");
    const text = section.textContent!;
    if (!c.input.thin) expect(text).toContain("Self-contained archive");
    else if (!c.expected.closure) expect(text).toContain("Prerequisite protection unknown");
    else expect(text).toMatch(explanations[c.expected.closure.state]);
    if (c.expected.closure?.state === "pinned") {
      expect(text).toContain("not a fresh probe");
      expect(text).toContain("custody release");
    }
    expect(text.includes("Pin absence is unconfirmed")).toBe(c.expected.closure?.absence_unconfirmed === true);
    expect(text.includes("automatic deletes have stopped")).toBe(c.expected.cleanup === "exhausted");
    if (c.expected.closure) {
      expect(text).toContain(cases.pin_ref);
      expect(text).toContain("Pin record updated: " + cases.updated_at);
    }
    expect(!!screen.queryByRole("link", { name: "Export archive" })).toBe((c.expected.permitted_actions as string[]).includes("download"));
    expect(!!screen.queryByRole("button", { name: "Delete archive" })).toBe((c.expected.permitted_actions as string[]).includes("discard"));
    expect(!!screen.queryByText(EXPORT_CMD)).toBe((c.expected.permitted_actions as string[]).includes("download"));
    expect(section.querySelectorAll("a")).toHaveLength(1);
  });

  it.each(cases.malformed)("rejects malformed wire $name", async c => {
    const capture = archive({ id: CAP_UUID, prerequisite_shas: [cases.pin_tip], prerequisite_closure: JSON.parse(c.wire) });
    await renderPanel(aRun(), summary({ archives: [capture] }));
    expect(screen.getByText(/Prerequisite protection unknown/)).toBeTruthy();
    expect(screen.getByRole("link", { name: "Export archive" })).toBeTruthy();
  });

  it.each(["$(echo bad)", "a;echo bad", CAP_UUID + "\n", "--help", "a".repeat(10000)])("suppresses unsafe command identifiers %s", async id => {
    await renderPanel(aRun({ id, landing_state: "needs_landing" }), summary({ archives: [archive({ id: CAP_UUID })] }));
    expect(screen.queryByRole("button", { name: "Copy" })).toBeNull();
    expect(screen.getByText(/Export command unavailable/)).toBeTruthy();
  });

  it("bounds untrusted plain diagnostics and never renders Markdown or control bytes", async () => {
    const hostile = "\u001b[31m[link](https://evil.invalid) **bold**\u202e" + "x".repeat(10000);
    const { container } = await renderPanel(aRun(), summary({ archives: [archive({ id: CAP_UUID,
      prerequisite_shas: [hostile], source_sha: hostile, reason: hostile,
      prerequisite_closure: { ...cases.cases[3].expected.closure!, reason: hostile, pin_ref: hostile, pin_tip: hostile },
    })] }));
    expect(container.querySelector('a[href="https://evil.invalid"]')).toBeNull();
    expect(container.querySelector("strong")).toBeNull();
    expect(container.textContent).not.toContain("\u001b");
    expect(container.textContent).not.toContain("\u202e");
    expect(container.textContent!.length).toBeLessThan(6500);
    expect(screen.queryByText(/git fetch origin/)).toBeNull();
  });

  it("does not invent positive protection on an owner API error", async () => {
    mockApi.getRunArchives.mockRejectedValueOnce(new Error("unavailable"));
    const { container } = render(<RecoveryArchivesPanel run={aRun()} />);
    await act(async () => { await Promise.resolve(); });
    expect(mockApi.getRunArchives).toHaveBeenCalled();
    expect(container.textContent).toBe("");
  });
});
