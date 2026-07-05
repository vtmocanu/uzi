// MockRunSocket: a timer-driven stand-in for the /api/ws WebSocket. It exposes
// exactly the surface useRunStream uses (onopen/onmessage/onclose/onerror,
// close()) and delivers the same JSON frames the real hub sends — sourced from
// the mock store's event bus instead of the network. Subscribing is also what
// wakes a seeded live run's script (ensureLive), so the stream starts when
// someone is actually watching.

import type { RunSocketLike } from "../lib/api";
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
      ensureLive(runId);
    }, 30);
  }

  close() {
    window.clearTimeout(this.openTimer);
    this.unsub?.();
    this.unsub = null;
    this.onclose?.();
  }
}
