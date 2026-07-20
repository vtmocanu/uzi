import { useCallback, useEffect, useRef, useState } from "react";

// Follow-mode scrolling for the live activity feed. The load-bearing rule (PRD
// #11 §2): `follow` lives in a REF updated by the scroll handler, and on append
// we scroll to bottom iff that ref is true — we never re-derive "am I at bottom?"
// AFTER React has appended nodes (scrollHeight has already grown by then, so the
// classic post-append check detaches on the first message and fights the user).

const BOTTOM_THRESHOLD_PX = 48;

// isNearBottom reports whether a scroll container is within `threshold` px of its
// bottom. Pure (takes just the three metrics) so the follow decision is testable
// without a live DOM.
export function isNearBottom(
  el: { scrollTop: number; scrollHeight: number; clientHeight: number },
  threshold = BOTTOM_THRESHOLD_PX,
): boolean {
  return el.scrollHeight - el.scrollTop - el.clientHeight <= threshold;
}

export interface FollowScroll {
  ref: React.RefObject<HTMLDivElement>;
  onScroll: () => void;
  /** True while the user has scrolled up (follow paused). */
  paused: boolean;
  /** Messages that arrived while paused and are below the fold. */
  newCount: number;
  /** Jump to the newest message and re-arm follow. */
  jumpToBottom: () => void;
}

// useFollowScroll wires a scroll container to follow mode. `itemCount` is the
// number of rendered items; each increase triggers the append behaviour.
export function useFollowScroll(itemCount: number): FollowScroll {
  const ref = useRef<HTMLDivElement>(null);
  const followRef = useRef(true);
  const prevCountRef = useRef(itemCount);
  const [paused, setPaused] = useState(false);
  const [newCount, setNewCount] = useState(0);

  const scrollToBottom = useCallback(() => {
    const el = ref.current;
    // Instant, never smooth — a burst of frames makes smooth scrolling lag.
    if (el) el.scrollTop = el.scrollHeight;
  }, []);

  const jumpToBottom = useCallback(() => {
    followRef.current = true;
    setPaused(false);
    setNewCount(0);
    scrollToBottom();
  }, [scrollToBottom]);

  const onScroll = useCallback(() => {
    const el = ref.current;
    if (!el) return;
    const atBottom = isNearBottom(el);
    followRef.current = atBottom;
    setPaused(!atBottom);
    if (atBottom) setNewCount(0);
  }, []);

  // Append handling: read follow from the ref (not from a value re-derived after
  // the DOM grew). Scroll if following; otherwise accrue the unseen count.
  useEffect(() => {
    const delta = itemCount - prevCountRef.current;
    prevCountRef.current = itemCount;
    if (delta <= 0) return;
    if (followRef.current) scrollToBottom();
    else setNewCount((n) => n + delta);
  }, [itemCount, scrollToBottom]);

  return { ref, onScroll, paused, newCount, jumpToBottom };
}

// useTailOnAppend tails ONE bounded scroll region to its bottom when new items
// arrive, but only while `enabled` (PRD #95 M2, Decision 3). It is the per-agent-body
// counterpart to useFollowScroll's whole-pane follow: the activity pane no longer
// auto-jumps globally — instead each EXPANDED agent body is its own bounded scroll
// region, and the opt-in "Follow live" toggle (default off) is what enables tailing.
// With `enabled` false (the default), an append NEVER moves the viewport — that is the
// whole point of making follow opt-in. It also tails once on the false→true transition
// so turning Follow on snaps to the newest line. Instant scroll, never smooth (a burst
// of frames makes smooth scrolling lag), matching useFollowScroll.
export function useTailOnAppend(count: number, enabled: boolean): React.RefObject<HTMLDivElement> {
  const ref = useRef<HTMLDivElement>(null);
  const prevCount = useRef(count);
  const prevEnabled = useRef(enabled);
  useEffect(() => {
    const grew = count > prevCount.current;
    const justEnabled = enabled && !prevEnabled.current;
    prevCount.current = count;
    prevEnabled.current = enabled;
    if (enabled && (grew || justEnabled) && ref.current) {
      ref.current.scrollTop = ref.current.scrollHeight;
    }
  }, [count, enabled]);
  return ref;
}

// useReconnectingBanner promotes a transient disconnect to a visible state only
// after `delayMs`, so a brief WS blip does not flash a banner.
export function useReconnectingBanner(connected: boolean, delayMs = 3000): boolean {
  const [show, setShow] = useState(false);
  useEffect(() => {
    if (connected) {
      setShow(false);
      return;
    }
    const id = window.setTimeout(() => setShow(true), delayMs);
    return () => window.clearTimeout(id);
  }, [connected, delayMs]);
  return show;
}
