// Status favicon (PRD #70): the browser-tab icon reflects the most urgent live
// state across a user's runs, so a backgrounded tab still telegraphs "something
// failed / needs you / is working". Split into a pure M2 state-derivation half
// (deriveFaviconState / failedRunIds — unit-tested here) and an M3 DOM half
// (renderFavicon / applyFavicon — canvas + <link> swap, exercised in the browser,
// not unit-tested). The favicon is ALWAYS the ember brand mark, theme-independent:
// a tab has no theme context, so it must read on any browser chrome.

import { effectiveRunStatus, isStoppedRun, needsHumanAttention } from "./runBadge";
import type { StopKind } from "./api";

// FaviconState is the derived tab signal, most-urgent first in the priority ladder.
// "idle" restores the plain static mark; the other three tint a corner status dot.
export type FaviconState = "failed" | "attention" | "running" | "idle";

// FaviconRun is the minimal run shape the derivation reads — satisfied by
// RunListItem, so the hook can pass its poll result straight through without a map.
// is_revising (issue #750) rides along so the attention check can classify from the
// EFFECTIVE status: a run re-planning after a revise must not light the attention dot.
export type FaviconRun = {
  id: string;
  status: string;
  stop_kind: StopKind | null;
  is_revising?: boolean;
};

// The non-terminal, actively-progressing statuses that make the mark "running".
const RUNNING_STATUSES = new Set<string>(["queued", "claimed", "running"]);

// isFreshFailure reports a run that should redden the tab: a GENUINE failure
// (isStoppedRun folds the cancelled/plan-rejected nuance — a deliberate stop is not
// breakage, so it never reddens) that is also FRESH — not one already failed at the
// hook's first poll. Scoping to fresh failures keeps a page that loads with old
// failures in history from screaming red forever; only a failure that crosses in
// while the tab is open raises the alarm.
function isFreshFailure(run: FaviconRun, baselineFailedIds: Set<string>): boolean {
  return (
    run.status === "failed" &&
    !isStoppedRun(run.status, run.stop_kind) &&
    !baselineFailedIds.has(run.id)
  );
}

// deriveFaviconState collapses the whole run list plus the unread-notification count
// into one tab signal by a first-match-wins priority ladder (PRD #70 Decision Log):
//   1. failed    — a fresh genuine failure outranks everything: a break the user did
//                  not already know about is the loudest thing a tab can say.
//   2. attention — a run awaiting the user's approval OR their answer to a
//                  clarification question (PRD #88), or any unread notification: the
//                  user is the blocker and a worker is held. The tab dot cannot say
//                  WHICH, and does not need to — it says "you are the blocker", and a
//                  parked question is exactly that.
//   3. running   — work is in flight (queued/claimed/running) but nothing needs the
//                  user right now.
//   4. idle      — nothing live; restore the plain mark.
export function deriveFaviconState(
  runs: FaviconRun[],
  unread: number,
  baselineFailedIds: Set<string>,
): FaviconState {
  if (runs.some((r) => isFreshFailure(r, baselineFailedIds))) return "failed";
  // issue #750: classify from the EFFECTIVE status so a run re-planning after a revise
  // (status still "awaiting_approval" server-side, but effectiveRunStatus → "revising")
  // does NOT light the attention dot. The failure and running branches are unaffected.
  if (unread > 0 || runs.some((r) => needsHumanAttention(effectiveRunStatus(r))))
    return "attention";
  if (runs.some((r) => RUNNING_STATUSES.has(r.status))) return "running";
  return "idle";
}

// failedRunIds is the set of ids of runs currently in the raw `failed` status. The
// hook seeds its baseline with this on its FIRST poll so pre-existing failures never
// read as fresh (they were already there before the tab opened). It intentionally
// keys off the raw status, not isStoppedRun: a run that later loses its "stopped"
// framing should still be baselined by id, and the fresh check re-applies the
// stopped nuance anyway.
export function failedRunIds(runs: FaviconRun[]): Set<string> {
  return new Set(runs.filter((r) => r.status === "failed").map((r) => r.id));
}

// ---------------------------------------------------------------------------
// M3: canvas render + <link> swap (DOM). Not unit-tested — exercised in-browser.
// ---------------------------------------------------------------------------

// The theme-independent brand palette (see PRD #70 / --danger/--warn/--brand). The
// favicon is always drawn on the near-black field in ember, with a per-state dot.
const FIELD = "#080A0F";
const EMBER = "#FB923C";
const DOT_COLOR: Record<Exclude<FaviconState, "idle">, string> = {
  failed: "#FB7185", // rose / --danger
  attention: "#FBBF24", // amber / --warn
  running: "#FB923C", // ember / --brand
};

// FactoryIcon mark paths (viewBox 0 0 24 24), reused on the canvas so the tab icon
// matches the in-app brand mark exactly.
const MARK_OUTLINE = "M2 20a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2V8l-7 5V8l-7 5V4a2 2 0 0 0-2-2H4a2 2 0 0 0-2 2z";
const MARK_TICKS = "M17 18h1M12 18h1M7 18h1";

const SIZE = 64; // 64×64 backing store; the browser shrinks it to ~16px.

// renderFavicon draws the mark (and, for non-idle states, a top-right status dot)
// on a 64×64 canvas and returns a PNG data URL. It throws if 2D context is
// unavailable — applyFavicon wraps it so a render failure never reaches React.
function renderFavicon(state: FaviconState): string {
  const canvas = document.createElement("canvas");
  canvas.width = SIZE;
  canvas.height = SIZE;
  const ctx = canvas.getContext("2d");
  if (!ctx) throw new Error("2d context unavailable");

  // Rounded near-black field.
  const radius = 12;
  ctx.beginPath();
  ctx.moveTo(radius, 0);
  ctx.arcTo(SIZE, 0, SIZE, SIZE, radius);
  ctx.arcTo(SIZE, SIZE, 0, SIZE, radius);
  ctx.arcTo(0, SIZE, 0, 0, radius);
  ctx.arcTo(0, 0, SIZE, 0, radius);
  ctx.closePath();
  ctx.fillStyle = FIELD;
  ctx.fill();

  // Factory mark, stroked ember under a 24→64 transform. lineWidth is bumped so the
  // strokes still read once the browser shrinks the icon to ~16px; round caps/joins
  // keep it soft.
  ctx.save();
  ctx.scale(SIZE / 24, SIZE / 24);
  ctx.strokeStyle = EMBER;
  ctx.lineWidth = 2;
  ctx.lineCap = "round";
  ctx.lineJoin = "round";
  ctx.stroke(new Path2D(MARK_OUTLINE));
  ctx.stroke(new Path2D(MARK_TICKS));
  ctx.restore();

  // Status dot, top-right, with a thin near-black ring so it reads on any tab
  // background. Idle draws no dot (the plain mark).
  if (state !== "idle") {
    const cx = SIZE - 16;
    const cy = 16;
    const r = 12;
    ctx.beginPath();
    ctx.arc(cx, cy, r + 3, 0, Math.PI * 2);
    ctx.fillStyle = FIELD;
    ctx.fill();
    ctx.beginPath();
    ctx.arc(cx, cy, r, 0, Math.PI * 2);
    ctx.fillStyle = DOT_COLOR[state];
    ctx.fill();
  }

  return canvas.toDataURL("image/png");
}

// findIconLink locates the favicon <link>: the static mark carries id="favicon-svg",
// with a rel/type fallback so a future markup tweak does not silently break the swap.
function findIconLink(): HTMLLinkElement | null {
  const byId = document.getElementById("favicon-svg");
  if (byId) return byId as HTMLLinkElement;
  return document.querySelector<HTMLLinkElement>('link[rel~="icon"][type="image/svg+xml"]');
}

// applyFavicon points the icon <link> at the right image for the state, keeping its
// `type` in step: idle restores the static /favicon.svg (type image/svg+xml); any
// other state swaps in a freshly-rendered PNG data URL (type image/png). It is
// defensive on purpose: a missing link is a no-op, and any render failure
// (canvas/toDataURL unsupported, e.g. some Safari configs) is swallowed so the icon
// and its type simply stay the static mark rather than throwing into React.
export function applyFavicon(state: FaviconState): void {
  const link = findIconLink();
  if (!link) return;
  if (state === "idle") {
    link.type = "image/svg+xml";
    link.href = "/favicon.svg";
    return;
  }
  try {
    const url = renderFavicon(state);
    link.type = "image/png";
    link.href = url;
  } catch {
    // Leave the current (static) mark and its type in place on any render failure.
  }
}
