// The Chat page (PRD #39 M4): a conversation list (/chat) and a conversation view
// (/chat/:id). Chat rides the run machinery, so the conversation view reuses the
// run-view streaming machinery verbatim (useRunStream: WS subscribe + REST replay
// + follow mode) and only layers chat-bubble presentation, a composer, and the
// human-gated proposal cards on top. Everything talks to the api-client seam
// (api.listChats/createChat/sendChatMessage/endChat/continueChat/*Proposal),
// which is mocked in demo mode and real-wired in Phase 3.

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { api, ApiError, isTerminalRun, type Chat as ChatDTO, type Run, type Worker } from "../lib/api";
import {
  CHAT_MAX_TURNS,
  chatFromRun,
  chatIsEnded,
  composerGate,
  conversationTitle,
  countUserTurns,
  hasOnlineWorker,
  queuedBehindActive,
  sortConversations,
  turnCapNotice,
} from "../lib/chat";
import { useRunStream } from "../lib/useRunStream";
import { ChatMessages } from "../components/ChatMessages";
import { ChatComposer } from "../components/ChatComposer";
import {
  Alert,
  Badge,
  Button,
  Card,
  EmptyState,
  ListSkeleton,
  PageHeader,
  SectionTitle,
  StatusPill,
  Textarea,
  cx,
} from "../components/ui";
import { ChatIcon } from "../components/icons";
import { stripUnsafeChars } from "../lib/safeText";

// WorkerOfflineBanner (Decision 15): honest liveness, derived from the existing
// heartbeat-tracked worker list — never a silent forever-queue.
function WorkerOfflineBanner() {
  return (
    <div className="rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-sm text-warn">
      No worker connected — chat needs your worker running. Messages queue until one comes online.{" "}
      <Link to="/settings/workers" className="font-medium underline underline-offset-2">
        Set up a worker
      </Link>
      .
    </div>
  );
}

// ChatStatusBadge reads a chat conversation's run status as chat semantics: a
// terminal run is an "ended" conversation, not a "completed"/"failed" job.
function ChatStatusBadge({ status }: { status: string }) {
  if (isTerminalRun(status)) return <Badge tone="neutral">ended</Badge>;
  return <StatusPill status={status} />;
}

// ── Conversation list (/chat) ────────────────────────────────────────────────

export function ChatList() {
  const navigate = useNavigate();
  const [chats, setChats] = useState<ChatDTO[]>([]);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");
  const [prompt, setPrompt] = useState("");
  const [starting, setStarting] = useState(false);

  const load = useCallback(async () => {
    setError("");
    try {
      const [chatRes, { workers }] = await Promise.all([api.listChats(), api.listWorkers()]);
      setChats(sortConversations(chatRes.chats));
      setWorkers(workers);
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to load chats");
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    load();
  }, [load]);

  const startChat = async () => {
    const p = prompt.trim();
    if (!p || starting) return;
    setError("");
    setStarting(true);
    try {
      // create returns a full runDTO under `run`; navigate to the new conversation,
      // seeding its meta optimistically so the header renders before the list loads.
      const { run } = await api.createChat(p);
      navigate(`/chat/${run.id}`, { state: { seed: chatFromRun(run) } });
    } catch (e) {
      setError(e instanceof ApiError ? e.message : "Failed to start the chat");
      setStarting(false);
    }
  };

  const online = hasOnlineWorker(workers);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Chat"
        description="Talk to uzi about itself, your runs, and ideas. It answers on your worker, from your token — and can draft issues you confirm."
      />

      {error && <Alert message={error} />}
      {!loading && !online && <WorkerOfflineBanner />}

      <Card className="space-y-3">
        <SectionTitle>New chat</SectionTitle>
        <Textarea
          rows={3}
          aria-label="Start a new chat"
          placeholder="Ask uzi anything — “how does the plan-approval gate work?”, “why did my last run fail?”, or describe an idea to turn into an issue."
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
        />
        <div className="flex justify-end">
          <Button disabled={starting || prompt.trim() === ""} onClick={startChat}>
            Start chat
          </Button>
        </div>
      </Card>

      {loading ? (
        <ListSkeleton rows={3} />
      ) : chats.length === 0 ? (
        <EmptyState
          icon={<ChatIcon />}
          title="No conversations yet"
          description="Start one above. uzi knows its own source at the deployed version and can read your runs to answer “why did this fail?”."
        />
      ) : (
        <div className="space-y-2">
          <SectionTitle>Conversations</SectionTitle>
          <ul className="space-y-2">
            {chats.map((c) => (
              <ConversationRow
                key={c.id}
                chat={c}
                onContinued={(run) => navigate(`/chat/${run.id}`, { state: { seed: chatFromRun(run) } })}
                onError={setError}
              />
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

function ConversationRow({
  chat,
  onContinued,
  onError,
}: {
  chat: ChatDTO;
  onContinued: (run: Run) => void;
  onError: (msg: string) => void;
}) {
  const [busy, setBusy] = useState(false);
  const ended = chatIsEnded(chat);
  const last = chat.last_message_at ?? chat.updated_at;

  const continueChat = async () => {
    setBusy(true);
    try {
      // continue returns the NEW chat run under `run`.
      const { run } = await api.continueChat(chat.id);
      onContinued(run);
    } catch (err) {
      onError(err instanceof ApiError ? err.message : "Failed to continue the chat");
      setBusy(false);
    }
  };

  // The Continue button lives OUTSIDE the Link — a button nested inside an anchor
  // is invalid interactive-in-interactive nesting.
  return (
    <li className="flex flex-wrap items-center justify-between gap-2 rounded-lg border border-edge bg-raised/40 px-3 py-2.5 transition-colors hover:border-edge-strong hover:bg-raised/70">
      <Link to={`/chat/${chat.id}`} className="min-w-0 flex-1">
        <p className="truncate text-sm font-medium text-fg">{stripUnsafeChars(conversationTitle(chat))}</p>
        <p className="mt-0.5 text-xs text-faint">Last activity {new Date(last).toLocaleString()}</p>
      </Link>
      <div className="flex items-center gap-2">
        <ChatStatusBadge status={chat.status} />
        {ended && (
          <Button size="sm" variant="secondary" disabled={busy} onClick={continueChat}>
            Continue
          </Button>
        )}
      </div>
    </li>
  );
}

// ── Conversation view (/chat/:id) ────────────────────────────────────────────

export function ChatConversation() {
  const { id = "" } = useParams();
  const navigate = useNavigate();
  const location = useLocation();
  const { run, messages, connected, error, refreshRun } = useRunStream(id);

  // Optimistic seed (Decision 11 UX): create/continue navigate here carrying
  // chatFromRun(run) in the route state, so the header/title/status/resume-note
  // render immediately — before the list refetch populates turn_count and
  // last_message_at. The list + stream then refine it.
  const seed = (location.state as { seed?: ChatDTO } | null)?.seed;
  const [meta, setMeta] = useState<ChatDTO | null>(seed ?? null);
  const [siblings, setSiblings] = useState<ChatDTO[]>([]);
  const [maxTurns, setMaxTurns] = useState(CHAT_MAX_TURNS);
  const [workers, setWorkers] = useState<Worker[]>([]);
  const [busy, setBusy] = useState(false);
  const [actionErr, setActionErr] = useState("");

  // Conversation meta (title, server turn_count, resume-of, the sibling set for
  // the one-live-chat note) + the turn-cap envelope constant + worker liveness.
  // Refetched on status change and after each action so the count/treatment stay
  // honest. A just-created chat may not be in the list yet — keep the optimistic
  // seed (or the prior value) rather than clobbering it to null until it appears.
  const loadMeta = useCallback(async () => {
    try {
      const [chatRes, { workers }] = await Promise.all([api.listChats(), api.listWorkers()]);
      setSiblings(chatRes.chats);
      setMeta((prev) => chatRes.chats.find((c) => c.id === id) ?? prev);
      setMaxTurns(chatRes.max_turns);
      setWorkers(workers);
    } catch {
      // Non-fatal: the stream still renders the conversation; meta just degrades.
    }
  }, [id]);

  useEffect(() => {
    void loadMeta();
  }, [loadMeta, run?.status]);

  const act = async (fn: () => Promise<unknown>) => {
    setActionErr("");
    setBusy(true);
    try {
      await fn();
    } catch (e) {
      setActionErr(e instanceof ApiError ? e.message : "Action failed");
    } finally {
      setBusy(false);
    }
  };

  const status = run?.status ?? meta?.status ?? "queued";
  const ended = isTerminalRun(status);
  // Prefer the server's turn_count (reviewer M4 minor); fall back to the
  // stream-derived count only until the list meta loads.
  const streamTurns = useMemo(() => countUserTurns(messages), [messages]);
  const turnCount = meta?.turn_count ?? streamTurns;
  const gate = composerGate({ status, turnCount, maxTurns });
  const notice = turnCapNotice({ turnCount, maxTurns });
  const queued = meta ? queuedBehindActive(meta, siblings) : false;
  const online = hasOnlineWorker(workers);

  const send = (text: string) =>
    act(async () => {
      await api.sendChatMessage(id, text);
      void refreshRun();
      // Refresh the server turn_count so the gate/notice track the new turn.
      void loadMeta();
    });

  const end = () =>
    act(async () => {
      await api.endChat(id); // 202 {server_side}; the run flips terminal server-side.
      void refreshRun();
      void loadMeta();
    });

  const continueChat = () =>
    act(async () => {
      const { run: next } = await api.continueChat(id);
      navigate(`/chat/${next.id}`, { state: { seed: chatFromRun(next) } });
    });

  if (!run && !meta) {
    return (
      <div className="space-y-4">
        <PageHeader backTo="/chat" backLabel="Chat" title="Conversation" />
        {error ? <Alert message={error} /> : <Card className="animate-pulse text-sm text-faint">Loading conversation…</Card>}
      </div>
    );
  }

  // Issue #124. BOTH branches are untrusted free text: the derived chat title is
  // agent-written, and the fallback is the forge ISSUE title. One strip covers both. This
  // site is the one the audit's enumeration missed — it looked for judge-DTO consumers, and
  // a run title reaches a fourth page through this fallback.
  const title = stripUnsafeChars(meta ? conversationTitle(meta) : (run?.issue_title?.trim() || "Untitled chat"));

  return (
    <div className="space-y-4">
      <PageHeader
        backTo="/chat"
        backLabel="Chat"
        titleNode={
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-x-2">
              <h1 className="truncate text-xl font-semibold tracking-tight">{title}</h1>
              <ChatStatusBadge status={status} />
              {!ended && (
                <span
                  title={connected ? "Live" : "Reconnecting…"}
                  className={cx(
                    "inline-flex items-center gap-1 text-xs",
                    connected ? "text-ok" : "text-faint",
                  )}
                >
                  <span className={cx("h-1.5 w-1.5 rounded-full", connected ? "bg-ok" : "bg-faint")} />
                  {connected ? "live" : "offline"}
                </span>
              )}
            </div>
            {meta?.resume_of_run_id && (
              <p className="mt-1 text-xs text-faint">Continued from an earlier conversation.</p>
            )}
          </div>
        }
      />

      {error && <Alert message={error} />}
      {actionErr && <Alert message={actionErr} />}

      <Card className="p-3 sm:p-4">
        <ChatMessages chatId={id} messages={messages} connected={connected} live={!ended} />
      </Card>

      {ended ? (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-edge bg-raised/40 px-4 py-3">
          <div>
            <p className="text-sm font-medium text-fg">This conversation has ended.</p>
            <p className="mt-0.5 text-xs text-faint">
              Continue picks up where it left off when your worker still holds the session.
            </p>
          </div>
          <Button variant="secondary" disabled={busy} onClick={continueChat}>
            Continue
          </Button>
        </div>
      ) : (
        <ChatComposer
          gate={gate}
          busy={busy}
          workerOffline={!online}
          turnNotice={notice}
          queuedBehindActive={queued}
          onSend={send}
          onEnd={end}
        />
      )}
    </div>
  );
}
