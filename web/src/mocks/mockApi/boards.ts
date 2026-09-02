import {
  type Board,
  type BoardPrefs,
  type Card,
} from "../../lib/api";
import { ApiError } from "../../lib/apiError";
import { state } from "../store";
import { delay, requireSession } from "./shared";
import { appSettings } from "./settings";

// boardResponse clones a board fixture for return. Cards are shallow-cloned so a caller
// mutating the response never touches the stored fixture.
function boardResponse(b: Board): { board: Board } {
  return {
    board: {
      ...b,
      cards: b.cards.map((c) => ({ ...c })),
    },
  };
}

export const boardsApi = {
  // ── Board ───────────────────────────────────────────────────────────────────
  getBoard: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b));
  },
  configureColumns: async (repoId: string, columns: { label_name: string }[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    b.columns = columns.map((c, i) => ({ label_name: c.label_name, position: i }));
    const names = new Set(b.columns.map((c) => c.label_name));
    for (const card of b.cards) if (card.column && !names.has(card.column)) card.column = "";
    return delay(boardResponse(b));
  },
  // Manual board order (PRD #102 M5). This is a SECOND IMPLEMENTATION of the server's
  // freeze, so it is a contract, not a convenience: mockApi.reorder.test.ts pins the
  // four behaviours below one case each, because a fixture that only walks the happy
  // path agrees with a broken mock on everything it covers.
  //
  // The demo board has no evicted iid and no unlisted open card, so a snapshot-style
  // fixture would pass against a mock missing (2) and (3) entirely.
  //
  //   1. cards are reordered to the submitted iid order;
  //   2. an iid not on the board is SKIPPED, not thrown on (the server no-ops per iid,
  //      because an eviction can land between a client's render and its submit);
  //   3. open cards absent from the list fall to the end in iid order (the mirror of
  //      the server's ClearBoardOrderExcept nulling them, plus its NULLS-LAST read);
  //   4. closed cards are untouched and keep their place.
  reorderBoard: async (repoId: string, iids: number[]) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    if (iids.length > 0) {
      const byIID = new Map(b.cards.map((c) => [c.iid, c]));
      const seen = new Set<number>();
      const ordered: Card[] = [];
      for (const iid of iids) {
        const card = byIID.get(iid);
        // (2) unknown iid: skip. (Also skips a duplicate, matching the server's dedupe.)
        //
        // KNOWN DIVERGENCE, recorded rather than left to be discovered (review M5-7):
        // this also skips a CLOSED card, and SetBoardOrderPositions does not — the
        // server would happily rank one it was handed. Unreachable from the product,
        // because dropIntent filters closed cards out before the request is built, so
        // neither side ever sees one. Kept on the mock side because it is the safer
        // half of the divergence and because the demo board contains closed cards: a
        // hand-built mock-mode request that ranked one would render it in the Closed
        // lane at a rank, which is exactly the state Decision 7b forbids. If the server
        // ever gains its own filter, delete this clause rather than adding a second.
        if (!card || card.closed || seen.has(iid)) continue;
        seen.add(iid);
        ordered.push(card);
      }
      // (3) + (4): everything not named keeps a NULL position server-side, which reads
      // back after the positioned rows, in iid order. Closed cards live here too and so
      // are never given a rank.
      const rest = b.cards.filter((c) => !seen.has(c.iid)).sort((x, y) => x.iid - y.iid);
      b.cards = [...ordered, ...rest];
    }
    return delay(boardResponse(b));
  },
  moveIssue: async (repoId: string, iid: number, toColumn: string) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    const to = toColumn === "open" ? "" : toColumn;
    const columnNames = b.columns.map((c) => c.label_name);
    // Preserve every non-column label (incl. `uzi`) and set the new column label.
    card.labels = [...card.labels.filter((l) => !columnNames.includes(l)), ...(to ? [to] : [])];
    card.column = to;
    card.conflict = false;
    return delay({ card: { ...card } }, 320);
  },
  // Promote (PRD #102 M6, Decision 15; PRD #764): add the configured `uzi` label,
  // apply-only and idempotent. Refuses uzi's own self-improvement tracker the way the
  // server does (Decision 13a), so the demo build cannot show a promote the real API
  // would 422.
  promoteIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    if (card.labels.includes("uzi-self-improve")) {
      throw new ApiError(422, "this issue is uzi's own self-improvement tracker and cannot be promoted");
    }
    const label = appSettings.uzi_label;
    if (!card.labels.includes(label)) card.labels = [label, ...card.labels];
    return delay({ card: { ...card } }, 320);
  },
  getIssue: async (repoId: string, iid: number) => {
    const b = state.boards.get(repoId);
    const card = b?.cards.find((c) => c.iid === iid);
    if (!b || !card) throw new ApiError(404, "issue not found");
    // IssueDetail is the card fields (minus latest_run) plus a live description.
    // Synthesize one consistent with has_prd_link so the PRD badge lines up with what
    // the description shows (a linked PRD is optional — PRD #764).
    const { latest_run: _latestRun, ...rest } = card;
    const description = card.has_prd_link
      ? `## Summary\n\nImplement the change described in the linked PRD.\n\nSee \`prds/${iid}-feature.md\` for the full specification.`
      : "This issue has no linked `prds/*.md` file. A PRD is optional — label the issue `uzi` and a run can still be started from it.";
    // bot_forge_user_id rides the issue detail (PRD #767 M5), from the board's single
    // connection, so the issue view evaluates the same "uzi OR assigned-to-bot" predicate
    // as the board card — assignee_ids comes through on ...rest.
    return delay({ issue: { ...rest, assignee_ids: rest.assignee_ids ?? [], description, bot_forge_user_id: b.bot_forge_user_id ?? 0 } });
  },
  syncRepo: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    return delay(boardResponse(b), 650);
  },
  createIssue: async (repoId: string, title: string, description: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    const iid = Math.max(0, ...b.cards.map((c) => c.iid)) + 1;
    const card = {
      iid,
      title,
      state: "opened",
      labels: [appSettings.uzi_label],
      assignee_ids: [] as number[],
      web_url: `${b.web_url}/-/issues/${iid}`,
      forge_type: "gitlab",
      author: requireSession().display_name?.toLowerCase() ?? "you",
      has_prd_link: /prds\/[\w.-]+\.md/.test(description),
      column: "",
      closed: false,
      conflict: false,
      // A just-created issue is the most recently updated thing on the board, so it
      // must lead in "Last updated" mode rather than sinking on a zero value.
      forge_updated_at: new Date().toISOString(),
      latest_run: null,
      pipeline: null,
    };
    b.cards.unshift(card);
    return delay({ card: { ...card } }, 450);
  },

  // Per-user, per-repo board preferences (PRD #196 M3). A SECOND IMPLEMENTATION of the
  // server contract, so it persists across calls within the session and matches the
  // wire shape exactly: null extra_labels = "not customised" (fall back to the admin
  // default), an array (incl. []) = the user's absolute set (Decision 9).
  getBoardPrefs: async (repoId: string) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // No row yet reads as the pristine default rather than being seeded, so a later
    // reset back to null and "never touched" stay indistinguishable to the client.
    const prefs = state.boardPrefs.get(repoId) ?? { extra_labels: null, show_all: false };
    return delay<BoardPrefs>({ extra_labels: prefs.extra_labels, show_all: prefs.show_all });
  },
  setBoardPrefs: async (repoId: string, prefs: BoardPrefs) => {
    const b = state.boards.get(repoId);
    if (!b) throw new ApiError(404, "board not found");
    // Loose validation for mock-mode parity with the server: each extra label must be
    // a non-empty, comma-free, ≤64-char string; an over-cap list is clamped rather
    // than rejected. null (not customised) is preserved as the sentinel.
    let extraLabels: string[] | null = null;
    if (Array.isArray(prefs.extra_labels)) {
      const cleaned: string[] = [];
      for (const raw of prefs.extra_labels) {
        const l = String(raw).trim();
        if (l === "" || l.includes(",") || l.length > 64) continue;
        if (!cleaned.includes(l)) cleaned.push(l);
      }
      extraLabels = cleaned.slice(0, 64);
    }
    const stored: BoardPrefs = { extra_labels: extraLabels, show_all: Boolean(prefs.show_all) };
    state.boardPrefs.set(repoId, stored);
    return delay<BoardPrefs>({ extra_labels: stored.extra_labels, show_all: stored.show_all }, 320);
  },
};
