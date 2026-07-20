// MockRunSocket: a timer-driven stand-in for the /api/ws WebSocket. It exposes
// exactly the surface useRunStream uses (onopen/onmessage/onclose/onerror,
// close()) and delivers the same JSON frames the real hub sends — sourced from
// the mock store's event bus instead of the network. Subscribing is also what
// wakes a seeded live run's script (ensureLive), so the stream starts when
// someone is actually watching.

import type { RunSocketLike } from "../lib/api";
import { LIVE_RUN_ID } from "./data";
import { ensureLive } from "./engine";
import { subscribe } from "./store";

export class MockRunSocket implements RunSocketLike {
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: string }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;

  private unsub: (() => void) | null = null;
  private openTimer: number;

  constructor(runId: string) {
    // Connect on a micro-delay so the caller can attach handlers first, like a
    // real socket's async handshake.
    this.openTimer = window.setTimeout(() => {
      this.unsub = subscribe(runId, (frame) => {
        this.onmessage?.({ data: JSON.stringify(frame) });
      });
      this.onopen?.();
      // Only the dedicated live-run fixture gets its timed script woken on subscribe
      // (mirrors mockApi.getRun). Waking it for EVERY running run hijacked the seeded
      // crew-demo runs (run-crew, run-degraded) — the planning script overwrote the
      // exact health states they exist to demo (PRD #95 M2). Gate it like getRun does.
      if (runId === LIVE_RUN_ID) ensureLive(runId);
    }, 30);
  }

  close() {
    window.clearTimeout(this.openTimer);
    this.unsub?.();
    this.unsub = null;
    this.onclose?.();
  }
}
