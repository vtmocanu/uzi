import { useCallback, useEffect, useRef, useState } from "react";
import {
  api,
  ApiError,
  isTerminalRun,
  runSocketUrl,
  type Run,
  type RunInputKind,
  type RunMessage,
  type WsEvent,
} from "./api";
import { emptyStream, ingest, ingestMany, type StreamState } from "./runStream";

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

  useEffect(() => {
    let closed = false;
    let ws: WebSocket | null = null;
    let reconnect: number | null = null;
    let catchup: number | null = null;

    // Reset for a new run id.
    streamRef.current = emptyStream();
    setMessages([]);
    setRun(null);
    setError("");
    void refreshRun();

    const scheduleCatchup = () => {
      if (catchup != null) return;
      catchup = window.setTimeout(() => {
        catchup = null;
        void replay();
      }, CATCHUP_DEBOUNCE_MS);
    };

    const connect = () => {
      if (closed) return;
      ws = new WebSocket(runSocketUrl(runId));
      ws.onopen = () => {
        setConnected(true);
        void replay();
      };
      ws.onmessage = (ev) => {
        let frame: WsEvent;
        try {
          frame = JSON.parse(String(ev.data)) as WsEvent;
        } catch {
          return;
        }
        if (frame.type === "message" && typeof frame.seq === "number") {
          const msg: RunMessage = {
            seq: frame.seq,
            kind: frame.kind ?? "",
            agent: frame.agent ?? null,
            payload: frame.payload,
            created_at: frame.created_at ?? new Date().toISOString(),
          };
          const r = ingest(streamRef.current, msg);
          commit(r.state);
          if (r.gap) scheduleCatchup();
        } else if (frame.type === "state") {
          void refreshRun();
        }
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
  }, [runId, replay, refreshRun, commit]);

  const submit = useCallback(
    async (kind: RunInputKind, body = "") => {
      await api.submitRunInput(runId, kind, body);
      void refreshRun();
    },
    [runId, refreshRun],
  );

  return { run, messages, connected, error, submit, refreshRun };
}
