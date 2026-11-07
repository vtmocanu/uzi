import { useEffect, useId, useRef, useState } from "react";
import { Button, Card } from "../../components/ui";
import { api, ApiError, type PolicyHoldResumeMode, type Run } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { POLICY_HOLD_WORDING, isPolicyHoldResumeMode, policyHoldCauseText } from "../../lib/policyHoldWording";

/** The server's guidance cap, in UTF-8 bytes (issue #3607). */
const GUIDANCE_MAX_BYTES = 16_000;
const encoder = new TextEncoder();

// Only the run state confirms a hold. Delivery telemetry blocks continuation controls;
// it does not establish a paused state, preservation evidence, or consent.
export function isPolicyHeld(run: Run): boolean {
  return run.status === "paused" && run.hold_reason === "codex_policy_hold";
}

type Notice = { tone: "status" | "alert"; text: string; refresh: boolean; episode: string };

/**
 * Issue #3607: a confirmed Codex policy hold. It says why the run is held (the shared
 * wording table, `unknown_cause` when the server recorded none), what was preserved, and
 * offers exactly the resume modes the server lists for this episode, each behind a confirm
 * step that names the mode's consequence. Cancel stays where it was (the run controls);
 * policy Stop and revise-turn are not offered.
 *
 * It stays mounted across run updates so a result outlives the refresh that ends the hold:
 * once the run leaves the hold, only the last notice remains, and nothing at all without one.
 */
export function PolicyHoldPanel({ run, canSteer, refreshRun }: {
  run: Run;
  canSteer: boolean;
  refreshRun: () => void | Promise<void>;
}) {
  const held = isPolicyHeld(run);
  const hold = run.policy_hold;
  const episode = hold?.outcome_id ?? "";
  const modes = held ? (hold?.resume_modes ?? []).filter(isPolicyHoldResumeMode) : [];
  const [choice, setChoice] = useState<{ mode: PolicyHoldResumeMode; episode: string } | null>(null);
  // Typed guidance is kept per mode and per held episode: closing the confirm (Escape, Back)
  // keeps it; a successful resume, a new episode or a different run drops it.
  const [drafts, setDrafts] = useState<{ episode: string; byMode: Partial<Record<PolicyHoldResumeMode, string>> }>({ episode: "", byMode: {} });
  const [busy, setBusy] = useState(false);
  const [rawNotice, setNotice] = useState<Notice | null>(null);
  // A result belongs to the episode it was made for; a new held episode makes it stale.
  const notice = rawNotice && (!held || rawNotice.episode === episode) ? rawNotice : null;
  const [returnFocusTo, setReturnFocusTo] = useState<PolicyHoldResumeMode | null>(null);
  const confirmRef = useRef<HTMLDivElement>(null);
  const noticeRef = useRef<HTMLDivElement>(null);
  const request = useRef<object | null>(null);
  const ids = useId();

  // A different run is a different panel: drop any choice, notice and in-flight result.
  useEffect(() => {
    setChoice(null);
    setDrafts({ episode: "", byMode: {} });
    setNotice(null);
    setBusy(false);
    request.current = null;
  }, [run.id]);

  // The confirm is valid only for the episode and mode it was opened on; a refresh that
  // changes either closes it rather than posting a choice the owner made for another hold.
  const active = choice != null && held && choice.episode === episode && modes.includes(choice.mode) ? choice : null;

  const activeKey = active ? `${active.episode}:${active.mode}` : "";
  useEffect(() => {
    if (activeKey !== "") confirmRef.current?.focus();
  }, [activeKey]);
  useEffect(() => {
    if (returnFocusTo && activeKey === "") {
      document.getElementById(`${ids}-mode-${returnFocusTo}`)?.focus();
      setReturnFocusTo(null);
    }
  }, [returnFocusTo, activeKey, ids]);
  // A result that closed the confirm takes focus, so it is not left on a removed control;
  // again when the hold ends, since the notice then moves into its own card.
  useEffect(() => {
    if (notice && activeKey === "") noticeRef.current?.focus();
  }, [notice, activeKey, held]);

  // After a fresh_one_claim resume the hold is resolved but the one-pickup fresh start is
  // still pending; say so quietly until the server clears it.
  const freshPending = !held && hold?.pending_fresh === true
    && run.status !== "completed" && run.status !== "failed" && run.status !== "cancelled";
  if (!held && !notice && !freshPending) return null;

  const draftsNow = drafts.episode === episode ? drafts.byMode : {};
  const guidance = active ? (draftsNow[active.mode] ?? "") : "";
  const setGuidance = (text: string) => {
    if (active) setDrafts({ episode: active.episode, byMode: { ...draftsNow, [active.mode]: text } });
  };
  const bytes = encoder.encode(guidance).length;
  const overCap = bytes > GUIDANCE_MAX_BYTES;
  const hasNul = guidance.includes("\u0000");
  const blocked = overCap || hasNul;

  const open = (mode: PolicyHoldResumeMode) => {
    setNotice(null);
    setChoice({ mode, episode });
  };
  const back = () => {
    if (!active) return;
    setReturnFocusTo(active.mode);
    setChoice(null);
  };
  const submit = async () => {
    if (!active || blocked || busy) return;
    const token = {};
    request.current = token;
    const runId = run.id;
    setBusy(true);
    setNotice(null);
    try {
      const res = await api.resumePolicyHold(runId, {
        mode: active.mode,
        expected_episode_id: active.episode,
        ...(guidance.trim() !== "" ? { guidance } : {}),
      });
      if (request.current !== token) return;
      setChoice(null);
      setDrafts({ episode: "", byMode: {} });
      setNotice(res.replay
        ? { tone: "status", text: "Already resumed (this request was applied earlier).", refresh: false, episode: active.episode }
        : { tone: "status", text: `${POLICY_HOLD_WORDING.modes[active.mode].label}: resumed. The run is queued for its next pickup.`, refresh: false, episode: active.episode });
      // The resume already applied: a failed re-read must not turn into a resume error.
      await Promise.resolve(refreshRun()).catch(() => undefined);
    } catch (e) {
      if (request.current !== token) return;
      if (e instanceof ApiError && e.status === 409) {
        setNotice({ tone: "alert", text: e.message, refresh: true, episode: active.episode });
      } else {
        setNotice({ tone: "alert", text: errorMessage(e, "Could not resume the run."), refresh: false, episode: active.episode });
      }
    } finally {
      if (request.current === token) {
        request.current = null;
        setBusy(false);
      }
    }
  };

  const noticeBlock = notice && (
    <div ref={noticeRef} tabIndex={-1} className="mt-3 space-y-2 outline-hidden">
      <p role={notice.tone} className={notice.tone === "alert" ? "text-sm text-danger" : "text-sm text-fg"}>
        {notice.text}
      </p>
      {notice.refresh && (
        <Button variant="secondary" size="sm" onClick={() => { setNotice(null); void refreshRun(); }}>
          Refresh run
        </Button>
      )}
    </div>
  );

  if (!held) {
    return <Card className="p-4">
      {freshPending && (
        <p className="text-xs text-muted">One-pickup fresh start pending: the next pickup may start one new conversation from the saved work.</p>
      )}
      {noticeBlock}
    </Card>;
  }

  const head = hold?.captured_head;
  const preservation = head
    ? hold?.published === true
      ? "Worker-reported published checkpoint"
      : "Verified local checkpoint; publication is not confirmed"
    : hold?.published === false
      ? "Local-only retention; checkpoint capture is unverified"
      : "Preservation is unverified";
  const consequenceId = `${ids}-consequence`;
  const counterId = `${ids}-counter`;
  const guidanceId = `${ids}-guidance`;

  return <Card className="p-4">
    <h2 className="text-sm font-semibold text-warn">Codex policy hold</h2>
    <p className="mt-1 text-sm text-fg">{policyHoldCauseText(hold?.hold_cause)}</p>
    {hold?.pending_fresh && (
      <p className="mt-1 text-xs text-muted">A one-pickup fresh start is pending: the next pickup may start one new conversation.</p>
    )}
    <p className="mt-2 text-xs text-muted">{preservation}{head ? <>: <span className="font-mono break-all">{head}</span></> : "."} Preservation evidence was last reported by the worker.</p>
    <p className="mt-1 text-xs text-muted">
      {hold?.continuation_mode === "same_conversation"
        ? "Continuation requires preserving the provider session."
        : hold?.continuation_mode === "fresh_from_saved_work"
          ? "Any future continuation would use a fresh conversation from the saved work."
          : "Continuation requirements are unverified."}
      {" "}The provider conversation's availability and ability to resume are not confirmed.
    </p>
    {hold?.recovery_required && (
      <p className="mt-1 text-xs text-muted">
        A required recovery checkpoint is recorded; {hold.recovery_verified_this_claim ? "the current pickup verified it." : "the current pickup has not verified it."}
      </p>
    )}

    {modes.length > 0 && canSteer && !active && (
      <div className="mt-3 flex flex-wrap gap-2" role="group" aria-label="Continue this run">
        {modes.map(mode => (
          <Button key={mode} id={`${ids}-mode-${mode}`} variant="secondary" size="sm" onClick={() => open(mode)}>
            {POLICY_HOLD_WORDING.modes[mode].label}
          </Button>
        ))}
      </div>
    )}
    {modes.length > 0 && !canSteer && (
      <p className="mt-2 text-xs text-muted">The run's owner can choose how to continue or Cancel.</p>
    )}

    {active && (
      <div
        ref={confirmRef}
        tabIndex={-1}
        role="group"
        aria-label={POLICY_HOLD_WORDING.modes[active.mode].label}
        aria-describedby={consequenceId}
        onKeyDown={e => { if (e.key === "Escape" && !busy) back(); }}
        className="mt-3 space-y-3 rounded-lg border border-edge-strong bg-raised p-3 outline-hidden focus-visible:ring-2 focus-visible:ring-ring"
      >
        <p className="text-sm font-medium text-fg">{POLICY_HOLD_WORDING.modes[active.mode].label}</p>
        <p id={consequenceId} className="text-sm text-muted">{POLICY_HOLD_WORDING.modes[active.mode].consequence}</p>
        <div className="space-y-1">
          <label htmlFor={guidanceId} className="block text-xs font-medium text-fg">Guidance for the agent (optional)</label>
          <textarea
            id={guidanceId}
            value={guidance}
            onChange={e => setGuidance(e.target.value)}
            aria-describedby={counterId}
            aria-invalid={blocked || undefined}
            rows={4}
            disabled={busy}
            className="block w-full resize-y rounded-lg border border-edge bg-surface px-2.5 py-2 text-sm text-fg placeholder:text-faint focus:border-brand"
          />
          <p id={counterId} className={`text-xs tabular-nums ${overCap ? "text-danger" : "text-faint"}`}>
            {bytes.toLocaleString("en-US")} / {GUIDANCE_MAX_BYTES.toLocaleString("en-US")} bytes
          </p>
          {overCap && <p role="alert" className="text-xs text-danger">Guidance is over the 16,000-byte limit. Shorten it to continue.</p>}
          {!overCap && hasNul && <p role="alert" className="text-xs text-danger">Guidance cannot contain a NUL character.</p>}
        </div>
        <div className="flex flex-wrap items-center gap-1.5">
          <Button size="sm" disabled={blocked || busy} onClick={() => void submit()}>
            {busy ? "Resuming…" : "Confirm and resume"}
          </Button>
          <Button variant="ghost" size="sm" disabled={busy} onClick={back}>Back</Button>
        </div>
      </div>
    )}

    {noticeBlock}
    <p className="mt-3 text-xs text-muted">Stop is not available for a policy hold. Cancel remains available to the run owner.</p>
  </Card>;
}
