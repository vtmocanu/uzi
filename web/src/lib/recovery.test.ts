import { describe, it, expect } from "vitest";
import {
  captureView,
  formatArchiveSize,
  recoverySectionKind,
  shortSha,
  sortedArchives,
} from "./recovery";
import type { RecoveryArchive, RecoveryArchiveSummary } from "./apiTypes";

// Regression pins for PRD #1296 M5's display logic (D6/D7). recoverySectionKind is the
// safety-critical one: it decides whether the run page tells the truth with ZERO captures,
// and captureView.downloadable is the gate that must never open before an archive is
// actually available. Each case below reddens if its rule is loosened.

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

describe("recoverySectionKind", () => {
  it("returns 'captures' whenever at least one capture exists, in ANY run status", () => {
    // D8: a real archive is never hidden — not by a non-terminal status, not by supported
    // being false, not by has_open_hold. Only archives.length drives this branch.
    expect(recoverySectionKind(summary({ archives: [archive()] }), "running")).toBe("captures");
    expect(recoverySectionKind(summary({ archives: [archive()] }), "failed")).toBe("captures");
    expect(
      recoverySectionKind(
        summary({ supported: false, has_open_hold: false, archives: [archive()] }),
        "completed",
      ),
    ).toBe("captures");
  });

  it("returns 'preparing' for a terminal run with an open hold and no capture yet", () => {
    // The archive is still being pinned at the protected boundary. Keyed on the aggregate
    // (has_open_hold), NOT on archives.length, so an empty capture list is still truthful.
    expect(recoverySectionKind(summary({ has_open_hold: true }), "cancelled")).toBe("preparing");
    expect(recoverySectionKind(summary({ has_open_hold: true }), "completed")).toBe("preparing");
    // 'preparing' outranks the failed branch: a failed run with an open hold is still
    // preparing, not unavailable/unsupported.
    expect(recoverySectionKind(summary({ has_open_hold: true }), "failed")).toBe("preparing");
    expect(
      recoverySectionKind(summary({ supported: false, has_open_hold: true }), "failed"),
    ).toBe("preparing");
  });

  it("does NOT treat an open hold on a still-running run as 'preparing'", () => {
    // A claim-time hold on a non-terminal run is just a reservation — nothing to say yet.
    // This is the case that reddens if the terminal gate is dropped from the rule.
    expect(recoverySectionKind(summary({ has_open_hold: true }), "running")).toBeNull();
  });

  it("distinguishes legacy/unsupported from armed-but-unavailable on a FAILED run", () => {
    // A run that failed to finalize with neither a capture nor an open hold: legacy when
    // recovery was never armed (supported:false), unavailable when it was armed but caught
    // nothing (supported:true).
    expect(recoverySectionKind(summary({ supported: false }), "failed")).toBe("unsupported");
    expect(recoverySectionKind(summary({ supported: true }), "failed")).toBe("unavailable");
  });

  it("renders nothing for a healthy run with no capture and no hold", () => {
    // Supported, no hold, no captures, non-failed: an ordinary run with no recovery
    // relevance. NOT gated on archives.length alone — a completed/running run stays quiet.
    expect(recoverySectionKind(summary(), "completed")).toBeNull();
    expect(recoverySectionKind(summary(), "running")).toBeNull();
    // Even the legacy (supported:false) shape says nothing until the run actually failed.
    expect(recoverySectionKind(summary({ supported: false }), "completed")).toBeNull();
    expect(recoverySectionKind(summary({ supported: false }), "running")).toBeNull();
  });
});

describe("captureView", () => {
  it("marks ONLY 'available' as downloadable", () => {
    expect(captureView("available").downloadable).toBe(true);
    for (const state of [
      "preparing",
      "uploading",
      "needs_action",
      "expired",
      "discarded",
      "totally_new_state",
      "",
    ]) {
      expect(captureView(state).downloadable, `${state} must not be downloadable`).toBe(false);
    }
  });

  it("labels and tones each known state", () => {
    expect(captureView("available")).toMatchObject({ label: "Available", tone: "ok" });
    expect(captureView("preparing")).toMatchObject({ label: "Preparing", tone: "info" });
    expect(captureView("uploading")).toMatchObject({ label: "Uploading", tone: "info" });
    expect(captureView("needs_action")).toMatchObject({ label: "Needs action", tone: "warning" });
    expect(captureView("expired")).toMatchObject({ label: "Expired", tone: "neutral" });
    expect(captureView("discarded")).toMatchObject({ label: "Discarded", tone: "neutral" });
  });

  it("falls back to an honest label for an unknown state, never mis-rendered as available", () => {
    // A server that grew a new state before this client did must not read as downloadable.
    const unknown = captureView("quarantined");
    expect(unknown).toMatchObject({ label: "quarantined", tone: "neutral", downloadable: false });
    // An empty state string gets the literal "Unknown" label rather than a blank pill.
    expect(captureView("")).toMatchObject({ label: "Unknown", downloadable: false });
  });
});

describe("formatArchiveSize", () => {
  it("renders an em dash for absent or negative sizes", () => {
    expect(formatArchiveSize(null)).toBe("—");
    expect(formatArchiveSize(undefined)).toBe("—");
    expect(formatArchiveSize(-1)).toBe("—");
  });

  it("renders bytes below 1 KiB verbatim", () => {
    expect(formatArchiveSize(0)).toBe("0 B");
    expect(formatArchiveSize(512)).toBe("512 B");
    expect(formatArchiveSize(1023)).toBe("1023 B");
  });

  it("uses binary units with one decimal below 10 and none at or above", () => {
    expect(formatArchiveSize(1024)).toBe("1.0 KiB");
    expect(formatArchiveSize(1536)).toBe("1.5 KiB");
    expect(formatArchiveSize(10 * 1024)).toBe("10 KiB");
    expect(formatArchiveSize(1024 * 1024)).toBe("1.0 MiB");
    expect(formatArchiveSize(100 * 1024 * 1024)).toBe("100 MiB");
    expect(formatArchiveSize(1024 * 1024 * 1024)).toBe("1.0 GiB");
  });
});

describe("shortSha", () => {
  it("trims a value longer than 12 chars to its leading 12", () => {
    expect(shortSha("0123456789abcdef0123456789abcdef01234567")).toBe("0123456789ab");
    expect(shortSha("0123456789abc")).toBe("0123456789ab");
  });

  it("leaves a value of 12 or fewer chars untouched", () => {
    expect(shortSha("0123456789ab")).toBe("0123456789ab");
    expect(shortSha("abc")).toBe("abc");
    expect(shortSha("")).toBe("");
  });

  it("only shortens — it does not sanitize non-hex content", () => {
    // The caller routes the result through stripUnsafeChars; shortSha itself must not touch
    // anything but length, so a long non-hex string is still just sliced to 12.
    expect(shortSha("abcdefghijklmnop")).toBe("abcdefghijkl");
  });
});

describe("sortedArchives", () => {
  it("returns captures newest-first without mutating the input", () => {
    const input = summary({
      archives: [
        archive({ id: "old", created_at: "2026-09-10T00:00:00Z" }),
        archive({ id: "new", created_at: "2026-09-13T00:00:00Z" }),
        archive({ id: "mid", created_at: "2026-09-11T00:00:00Z" }),
      ],
    });
    const before = input.archives.map((a) => a.id);
    expect(sortedArchives(input).map((a) => a.id)).toEqual(["new", "mid", "old"]);
    // A stable copy: the source order is unchanged.
    expect(input.archives.map((a) => a.id)).toEqual(before);
  });
});
