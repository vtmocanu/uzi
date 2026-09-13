import type { JudgeDispositionCoord, JudgeSettledMember } from "../../lib/api";

// A member the page can Undo: the (run, rec) it will clear a disposition on. Taken from the
// RESPONSE's `settled` list — the members the server actually wrote — never from this page's
// own view of which occurrences were open.
//
// The distinction is not pedantry, it is the difference between a revert and a destructive
// delete (PRD #98 review BLK-UNDO). scope=open membership is decided SERVER-SIDE at write
// time; this page's `backlog` is as old as its last load. Any member settled in between is
// `todo` here and excluded there, so a snapshot-based undo issues deleteDisposition for a
// disposition this action never created. For an M6 issue-close auto-done that is
// IRREVERSIBLE: close_synced_at is already stamped, so the edge-triggered poller never
// re-fires and the set_via='issue_close' provenance is destroyed. The page cannot narrow
// this itself — `updated` is a bare count, and its own view is the stale thing.
type UndoMember = JudgeSettledMember;

export type Toast = {
  message: string;
  // The members a bulk action settled; Undo clears each one's disposition. Empty when
  // there is nothing to undo (e.g. the action matched no open member). Used by the OWNER
  // scope, where a disposition has a (run, rec) address.
  undo: UndoMember[];
  // The admin cross-user Undo (PRD #1184 M4): the coordinates to clear the set_via='admin'
  // disposition on. Present only under scope="all" — the admin write spans every user's rows
  // and has NO run address, so its undo is BY COORDINATE, through the admin DELETE endpoint,
  // not the per-member deleteDisposition loop `undo` drives. Absent/empty under the owner scope.
  adminUndoCoords?: JudgeDispositionCoord[];
};
