// @vitest-environment jsdom
import { afterEach, describe, it, expect, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { RecoveryArchivesPanel } from "./RecoveryArchives";
import { api, type RecoveryArchive, type RecoveryArchiveSummary, type Run } from "../lib/api";

// The panel fetches its own owner-scoped summary via api.getRunArchives. Mock ONLY that
// call; keep runArchiveDownloadUrl and the recovery display helpers real, since the whole
// point of these pins is the real render path (D6/D7).
vi.mock("../lib/api", async (importActual) => {
  const actual = await importActual<typeof import("../lib/api")>();
  return {
    ...actual,
    api: { getRunArchives: vi.fn(), discardRunArchive: vi.fn() },
  };
});
const mockApi = vi.mocked(api);

afterEach(() => {
  cleanup();
  vi.clearAllMocks();
});

// The panel reads only run.id and run.status; a minimal cast matches the repo convention
// (SteerQueueCard.test.tsx) rather than spelling out the whole Run.
function aRun(over: Partial<Run> = {}): Run {
  return { id: "r1", status: "failed", ...over } as Run;
}

function archive(over: Partial<RecoveryArchive> = {}): RecoveryArchive {
  return {
    id: "cap1",
    run_id: "r1",
    state: "available",
    source_sha: "0123456789abcdef0123456789abcdef01234567",
    created_at: "2026-09-13T00:00:00Z",
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

const SECRET_WARNING = /Treat every archive as if it contains secrets/;

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
          archive({ id: "cap-ok", state: "available", source_sha: "bbbbbbbbbbbb2222" }),
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
    expect(within(links[0].closest("li") as HTMLElement).getByText("Available")).toBeTruthy();

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
