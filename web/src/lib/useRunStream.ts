import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  createRunSocket,
  isTerminalRun,
  type AgentSelectionInput,
  type Run,
  type RunInputKind,
  type RunMessage,
  type RunSocketLike,
  type SteerInput,
  type WsEvent,
} from "./api";
import { applyFrame, emptyStream, ingestMany, type StreamState } from "./runStream";

const RECONNECT_MS = 1500;
const CATCHUP_DEBOUNCE_MS = 150;

// useRunStream drives the live run view: it fetches the run, opens the WS, and
// keeps a lossless, dup-free message list. The persisted log is authoritative —
// on connect and on every reconnect it REST-replays from the last-seen seq before
// (and while) taking live frames, and a seq gap on a live frame triggers a REST
// catch-up. WS state frames only invalidate the cached run (re-read over REST),
// never carry authoritative state.
export function useRunStream(runId: string) {
  const [run, setRun] = useState<Run | null>(null);
  const [messages, setMessages] = useState<RunMessage[]>([]);
  const [connected, setConnected] = useState(false);
  const [error, setError] = useState("");
  // The follow_up steer queue (PRD #95). Lifted HERE — not into the FollowUpComposer,
  // which is !terminal-gated and unmounts the instant a run completes — so the queue
  // survives the terminal transition (Decision 7/B1) and can still show "Not delivered
  // — run finished". M1 fetches it once on mount; the live refetch triggers (onopen /
  // state / health / the data-less `input` frame) and optimistic send land in M3.
  const [inputs, setInputs] = useState<SteerInput[]>([]);
  // canSteer distinguishes an OWNER (GET /inputs 200, even when the queue is empty)
  // from a NON-OWNER viewer (a non-owner admin can open the owner-or-admin run view,
  // but the owner-only /inputs 404s). Decision 8/N2: the steer surface (queue card +
  // composer) must render empty/hidden and silent for a non-owner — never a broken
  // Send that 404s, never a banner. Defaults true so an owner never flashes hidden;
  // a 404 flips it off. A 200-empty owner is unaffected.
  const [canSteer, setCanSteer] = useState(true);

  const streamRef = useRef<StreamState>(emptyStream());
  const statusRef = useRef<string>("");

  const commit = useCallback((s: StreamState) => {
    streamRef.current = s;
    setMessages(s.messages);
  }, []);

  const replay = useCallback(async () => {
    try {
      const { messages: batch } = await api.getRunMessages(runId, streamRef.current.lastSeq);
      commit(ingestMany(streamRef.current, batch).state);
    } catch {
      // Transient; the next frame or reconnect retries. The log is durable, so a
      // failed catch-up only delays rendering, never loses a message.
    }
  }, [runId, commit]);

  const refreshRun = useCallback(async () => {
    try {
      const { run } = await api.getRun(runId);
      setRun(run);
      statusRef.current = run.status;
    } catch (e) {
      if (e instanceof ApiError) setError(e.message);
    }
  }, [runId]);

  // refreshInputs re-reads the steer queue (PRD #95). Best-effort and SILENT on error:
  // a non-owner viewer (an admin on someone else's run) 404s here, which Decision 8/N2
  // says to treat as "no queue to show" — never a run-level error banner. M1 calls it
  // once on mount; M3 adds the onopen / state / health / `input`-frame triggers so a
  // dropped frame self-heals.
  const refreshInputs = useCallback(async () => {
    try {
      const { inputs } = await api.getRunInputs(runId);
      setInputs(inputs);
      // A 200 (even empty) proves this viewer owns the run → allow steering.
      setCanSteer(true);
    } catch (e) {
      // A 404 on the owner-only endpoint means this viewer is NOT the owner (the run
      // view itself is owner-or-admin, so it can still be open): hide the steer surface
      // silently (Decision 8/N2). Any other error is transient — leave the queue and the
      // last-known canSteer untouched. Never surface a banner either way.
      if (e instanceof ApiError && e.status === 404) setCanSteer(false);
    }
  }, [runId]);

  useEffect(() => {
    let closed = false;
    let ws: RunSocketLike | null = null;
    let reconnect: number | null = null;
    let catchup: number | null = null;

    // Reset for a new run id.
    streamRef.current = emptyStream();
    setMessages([]);
    setRun(null);
    setError("");
    setInputs([]);
    setCanSteer(true);
    // Load the run and its message history on MOUNT, independent of the socket.
    // The persisted log is authoritative and a page load must show existing
    // history (and current status) immediately — even if the WS is slow to
    // connect or never connects. The socket then only layers on live deltas, and
    // its onopen replay backfills anything that landed during the connect window.
    void refreshRun();
    void replay();
    void refreshInputs();

    const scheduleCatchup = () => {
      if (catchup != null) return;
      catchup = window.setTimeout(() => {
        catchup = null;
        void replay();
      }, CATCHUP_DEBOUNCE_MS);
    };

    const connect = () => {
      if (closed) return;
      ws = createRunSocket(runId);
      ws.onopen = () => {
        setConnected(true);
        void replay();
        // Reconcile the steer queue on every (re)connect, alongside the message
        // replay (PRD #95 S1): the `input` frame is droppable, so a follow-up
        // consumed while we were disconnected would otherwise strand at Queued —
        // this refetch is the source of truth that self-heals it.
        void refreshInputs();
      };
      ws.onmessage = (ev) => {
        let frame: WsEvent;
        try {
          frame = JSON.parse(String(ev.data)) as WsEvent;
        } catch {
          return;
        }
        const { state, effects } = applyFrame(streamRef.current, frame);
        commit(state);
        // A state frame (and any message-seq gap) triggers a replay routed through
        // the debounced catch-up, so a hub-dropped tail message is backfilled
        // without a reconnect — and a burst of frames coalesces into one REST call.
        if (effects.replay) scheduleCatchup();
        if (effects.refreshRun) void refreshRun();
        // Refetch the steer queue on the data-less `input` frame (the fast path) AND
        // on any state/health-driven refresh (PRD #95 S1): consuming a follow-up flips
        // the run to running from awaiting_approval too, so state/health frames co-fire
        // with the input frame — refetching on both keeps the queue live even if the
        // hub dropped the input frame for a slow subscriber.
        if (frame.type === "input" || effects.refreshRun) void refreshInputs();
      };
      ws.onclose = () => {
        setConnected(false);
        ws = null;
        if (closed) return;
        // A terminal run emits nothing more — stop reconnecting.
        if (isTerminalRun(statusRef.current)) return;
        reconnect = window.setTimeout(connect, RECONNECT_MS);
      };
      ws.onerror = () => {
        try {
          ws?.close();
        } catch {
          // close after error is best-effort; onclose handles reconnect.
        }
      };
    };

    connect();

    return () => {
      closed = true;
      if (catchup != null) clearTimeout(catchup);
      if (reconnect != null) clearTimeout(reconnect);
      if (ws) {
        ws.onclose = null;
        try {
          ws.close();
        } catch {
          // best-effort teardown.
        }
      }
    };
  }, [runId, replay, refreshRun, refreshInputs, commit]);

  const submit = useCallback(
    async (
      kind: RunInputKind,
      body = "",
      selection?: AgentSelectionInput,
      // PRD #84 M4 4c: the "run without the capability" override, threaded through to
      // submitRunInput. Meaningful only with approve_plan; the server ignores it otherwise.
      overrideCapabilities?: boolean,
    ) => {
      // A follow-up shows in the steer queue immediately as Queued (PRD #95 S2):
      // optimistically prepend a temp entry (the queue is newest-first), then adopt
      // the real id + created_at the write returns so a later refetch — which replaces
      // the list wholesale — reconciles cleanly on the same id. On failure, roll the
      // optimistic entry back and rethrow so the caller's act() surfaces the error.
      if (kind === "follow_up") {
        const tempId = -Date.now();
        const optimistic: SteerInput = {
          id: tempId,
          body,
          created_at: new Date().toISOString(),
          consumed_at: null,
        };
        setInputs((prev) => [optimistic, ...prev]);
        let res: { id?: number; created_at?: string };
        try {
          res = await api.submitRunInput(runId, kind, body, selection);
        } catch (e) {
          setInputs((prev) => prev.filter((i) => i.id !== tempId));
          throw e;
        }
        setInputs((prev) =>
          prev.map((i) =>
            i.id === tempId
              ? { ...i, id: res.id ?? i.id, created_at: res.created_at ?? i.created_at }
              : i,
          ),
        );
        void refreshRun();
        return;
      }
      await api.submitRunInput(runId, kind, body, selection, overrideCapabilities);
      void refreshRun();
    },
    [runId, refreshRun],
  );

  // inputs + refreshInputs are the PRD #95 steer queue: M3 wires refreshInputs into the
  // live triggers and the queue card reads `inputs`. canSteer gates the steer surface
  // for a non-owner viewer (Decision 8/N2).
  return { run, messages, connected, error, submit, refreshRun, inputs, refreshInputs, canSteer };
}
