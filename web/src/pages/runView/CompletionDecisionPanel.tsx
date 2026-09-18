import { useState, type ReactNode } from "react";
import { api, type Run } from "../../lib/api";
import { errorMessage } from "../../lib/apiError";
import { stripUnsafeChars } from "../../lib/safeText";
import { Badge, Button, Card, Textarea, cx } from "../../components/ui";

// The owner completion-decision surface (PRD #1227 M4). Shown ONLY when the run is
// completion-blocked (completion_phase === "blocked", the SINGLE server-computed
// discriminator that covers both the live completion question and the parked hold — never
// re-derived here). It extends CompletionStatePanel's read-only detail with the three
// owner/admin decisions from #1227's committed wire contract:
//
//   - continue: resume with optional guidance (no contract revision);
//   - partial:  reduce scope to an exact kept-milestone set, deferring the rest (revision N+1);
//   - accept:   waive an exact set of unmet criteria (revision N+1).
//
// Every write is OWNER-SCOPED server-side (a foreign/unknown run is 404), so the controls are
// gated on `canSteer`; a non-owner sees inert explanatory text, never a button that would 404
// (mirroring the Resume/Expedite non-owner branches). partial/accept are irreversible contract
// revisions, so each goes through a "Review & confirm" step before it submits.
//
// EVERY untrusted string — milestone titles/ids, and the reasons/text echoed back from
// completion_deferred / completion_accepted — is run through stripUnsafeChars at the render
// boundary, exactly as CompletionStatePanel does (a bidi/zero-width character in a repo/agent-
// authored title or an owner reason must not survive to the DOM).

// MAX_DECISION_REASON_BYTES mirrors the server's MaxGuidanceBytes cap (8 KiB) applied to a
// partial/accept reason and a continue guidance. The server enforces it (400 "… too long"); we
// disable submit client-side so a valid request is formed rather than round-tripping a refusal.
const MAX_DECISION_REASON_BYTES = 8192;

// The id the inline error is exposed under, so the reason/guidance control can point at it via
// aria-describedby and a screen reader reads a 409/400 alongside the field it belongs to.
const ERROR_ID = "completion-decision-error";

// milestoneCriterionId derives a milestone's SOLE criterion id. It MIRRORS the server's
// buildCompletionContract, which mints `id = "<milestone_id>.c1"` — PRD #1227's current frozen
// contract is 1:1 (one criterion per milestone, its text = the milestone title, multi-criterion
// excluded). Centralized in this one helper so if the convention ever changes there is a single
// site to fix; the server is fail-closed (an unknown criterion id → 400), so the derivation is
// safe to send.
function milestoneCriterionId(milestoneId: string): string {
  return `${milestoneId}.c1`;
}

function byteLength(s: string): number {
  return new TextEncoder().encode(s).length;
}

type DecisionMode = "continue" | "partial" | "accept";

const MODE_LABEL: Record<DecisionMode, string> = {
  continue: "Continue",
  partial: "Reduce scope",
  accept: "Accept criteria",
};

export function CompletionDecisionPanel({
  run,
  canSteer = true,
  busy = false,
  act,
  refreshRun,
}: {
  run: Run;
  // False for a non-owner viewer (mirrors LimitWaitPanel/PausedPanel): the decision endpoint
  // is owner-scoped and 404s a non-owner, so a non-owner sees inert text, never a button.
  canSteer?: boolean;
  // The page's shared `busy` flag (set by `act`), so every control disables while a decision
  // is in flight — matching the rest of the run view's owner actions.
  busy?: boolean;
  // The page's `act` helper: it runs the mutation, lands any error on the page banner
  // (actionErr) and toggles `busy`. Wiring through it keeps the completion controls consistent
  // with every other owner action on the page; the panel ALSO surfaces the error inline.
  act: (fn: () => Promise<unknown>) => Promise<boolean>;
  // The page's run refetch (re-reads the DTO so the new revision + history land).
  refreshRun: () => void | Promise<void>;
}) {
  const [mode, setMode] = useState<DecisionMode>("continue");
  const [guidance, setGuidance] = useState("");
  const [reason, setReason] = useState("");
  // Milestone ids the owner selected to DEFER (partial). The kept set is the in-scope set
  // minus this, so "default keep" needs no seeding.
  const [defer, setDefer] = useState<Set<string>>(new Set());
  // Milestone ids whose sole criterion the owner selected to ACCEPT.
  const [accepting, setAccepting] = useState<Set<string>>(new Set());
  // The "Review & confirm" gate before an irreversible partial/accept submit.
  const [confirming, setConfirming] = useState(false);
  // The specific server message, surfaced inline at the form (a 409 conflict / 400 validation).
  const [inlineErr, setInlineErr] = useState("");

  // completion_phase is the SINGLE server-computed discriminator (D8): render only when
  // "blocked". "" / "checking" / "reworking" are covered by CompletionStatePanel.
  if ((run.completion_phase ?? "") !== "blocked") return null;

  const milestones = run.milestones ?? [];
  const deferred = run.completion_deferred ?? [];
  const accepted = run.completion_accepted ?? [];
  const revision = run.completion_revision ?? null;

  const deferredIds = new Set(deferred.map((d) => d.milestone_id));
  const acceptedIds = new Set(accepted.map((a) => a.milestone_id));
  const completedIds = new Set(run.milestones_completed ?? []);
  const titleOf = new Map(milestones.map((m) => [m.id, m.title]));

  // The current in-scope set = the frozen milestones minus what has already been deferred.
  const inScope = milestones.filter((m) => !deferredIds.has(m.id));
  // The kept set a partial decision sends: in-scope minus what the owner is deferring now. An
  // already-accepted milestone can never be deferred (its checkbox is disabled), so it stays.
  const keptIds = inScope.filter((m) => !defer.has(m.id)).map((m) => m.id);

  // Accept offers only criteria of milestones that are in-scope AND not completed AND not
  // already accepted — derive per the M4 spec, then map each to its single criterion.
  const acceptCandidates = milestones
    .filter((m) => !deferredIds.has(m.id) && !completedIds.has(m.id) && !acceptedIds.has(m.id))
    .map((m) => ({ milestoneId: m.id, id: milestoneCriterionId(m.id), text: m.title }));
  const selectedCriterionIds = acceptCandidates
    .filter((c) => accepting.has(c.milestoneId))
    .map((c) => c.id);

  const reasonBytes = byteLength(reason);
  const reasonOverCap = reasonBytes > MAX_DECISION_REASON_BYTES;
  const reasonOk = reason.trim() !== "" && !reasonOverCap;
  const guidanceBytes = byteLength(guidance);
  const guidanceOverCap = guidanceBytes > MAX_DECISION_REASON_BYTES;

  // A valid partial keeps ≥1 and defers ≥1 (else the server 400s "removes no milestone"), with
  // a reason and a fenced revision. A valid accept names ≥1 criterion with a reason + revision.
  const partialOk = revision != null && keptIds.length > 0 && defer.size > 0 && reasonOk;
  const acceptOk = revision != null && selectedCriterionIds.length > 0 && reasonOk;

  const toggleIn = (set: Set<string>, id: string): Set<string> => {
    const next = new Set(set);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return next;
  };

  const resetForm = () => {
    setGuidance("");
    setReason("");
    setDefer(new Set());
    setAccepting(new Set());
    setConfirming(false);
  };

  // runDecision wires the mutation through the page's `act` (banner + busy) AND captures the
  // specific message inline. The inner catch sets inlineErr then re-throws, so `act` still
  // lands the same message on the page banner and returns false; on success it resets the form.
  //
  // On FAILURE it ALSO refreshes the run: a 409 ErrCompletionRevisionConflict means the sent
  // contract_revision is stale, so without re-reading the DTO the panel would keep rendering the
  // stale `revision N` badge and re-submitting the SAME stale revision, looping on 409. The
  // refresh re-renders the panel against the server's current revision/state while the error is
  // surfaced, closing the loop. It is guarded so a refresh error can't mask the decision error.
  const runDecision = async (call: () => Promise<{ run: Run }>) => {
    setInlineErr("");
    const ok = await act(async () => {
      try {
        await call();
      } catch (e) {
        setInlineErr(errorMessage(e, "The decision could not be applied."));
        try {
          await refreshRun();
        } catch {
          // A refresh failure must not overwrite the decision error the owner needs to see.
        }
        throw e;
      }
      await refreshRun();
    });
    if (ok) resetForm();
  };

  const submitContinue = () =>
    runDecision(() => api.continueCompletionDecision(run.id, guidance));
  const submitPartial = () => {
    if (revision == null) return;
    return runDecision(() =>
      api.partialCompletionDecision(run.id, keptIds, reason.trim(), revision),
    );
  };
  const submitAccept = () => {
    if (revision == null) return;
    return runDecision(() =>
      api.acceptCompletionDecision(run.id, selectedCriterionIds, reason.trim(), revision),
    );
  };

  const switchMode = (m: DecisionMode) => {
    setMode(m);
    setConfirming(false);
    setInlineErr("");
  };

  const errDescribedBy = inlineErr ? ERROR_ID : undefined;

  return (
    <Card className="p-4">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold text-fg">Completion decision</h2>
        {revision != null && (
          <Badge tone="neutral" title="The run's current completion-contract revision">
            revision {revision}
          </Badge>
        )}
      </div>
      <p className="mt-1 text-xs text-muted">
        This run is blocked on a completion question. Decide how to resolve it: continue with
        guidance, reduce the scope to what still matters, or accept specific unmet criteria.
        Reducing scope or accepting a criterion creates a new contract revision and cannot be
        undone.
      </p>

      {/* Immutable decision history — the owner-deferred milestones and owner-accepted criteria
          recorded on earlier revisions. Read-only; every echoed string is sanitized. */}
      {(deferred.length > 0 || accepted.length > 0) && (
        <div className="mt-3 space-y-3">
          <h3 className="text-xs font-medium text-muted">Decision history</h3>
          {deferred.length > 0 && (
            <ul className="space-y-1" aria-label="Deferred milestones">
              {deferred.map((d) => (
                <li key={`${d.milestone_id}-${d.revision}`} className="text-xs text-fg">
                  <span className="font-medium">
                    {stripUnsafeChars(titleOf.get(d.milestone_id) ?? d.milestone_id)}
                  </span>{" "}
                  <span className="font-mono text-muted">({stripUnsafeChars(d.milestone_id)})</span>{" "}
                  — deferred at rev {d.revision} — {stripUnsafeChars(d.reason)}
                </li>
              ))}
            </ul>
          )}
          {accepted.length > 0 && (
            <ul className="space-y-1" aria-label="Accepted criteria">
              {accepted.map((a) => (
                <li key={`${a.id}-${a.revision}`} className="text-xs text-fg">
                  <span className="font-medium">{stripUnsafeChars(a.text)}</span>{" "}
                  <span className="font-mono text-muted">({stripUnsafeChars(a.id)})</span>{" "}
                  — accepted at rev {a.revision} — {stripUnsafeChars(a.reason)}
                </li>
              ))}
            </ul>
          )}
        </div>
      )}

      {!canSteer ? (
        // Non-owner: the decision is the owner's to make. Show who can, never a control that
        // would 404 (mirrors the Resume/Expedite non-owner branches).
        <p className="mt-3 text-xs text-muted">
          Only the run&apos;s owner can decide how to resolve a completion block.
        </p>
      ) : (
        <div className="mt-4">
          {/* Mode selector — a segmented single-select of toggle buttons (aria-pressed marks
              the active mode; a plain button role keeps it operable and legible). */}
          <div className="flex flex-wrap gap-1" role="group" aria-label="Completion decision">
            {(Object.keys(MODE_LABEL) as DecisionMode[]).map((m) => (
              <Button
                key={m}
                aria-pressed={mode === m}
                variant={mode === m ? "primary" : "secondary"}
                size="sm"
                disabled={busy}
                onClick={() => switchMode(m)}
              >
                {MODE_LABEL[m]}
              </Button>
            ))}
          </div>

          <div className="mt-3">
            {mode === "continue" && (
              <div className="space-y-2">
                <label
                  htmlFor="completion-continue-guidance"
                  className="block text-xs font-medium text-muted"
                >
                  Guidance (optional)
                </label>
                <Textarea
                  id="completion-continue-guidance"
                  className="min-h-[4.5rem] text-xs"
                  value={guidance}
                  disabled={busy}
                  aria-describedby={errDescribedBy}
                  onChange={(e) => setGuidance(e.target.value)}
                  placeholder="Optional steering for the run as it continues (e.g. a milestone is genuinely blocked on an upstream fix — try the alternative approach instead)."
                />
                <p className={cx("text-[11px]", guidanceOverCap ? "font-medium text-danger" : "text-muted")}>
                  {guidanceBytes.toLocaleString()} / {MAX_DECISION_REASON_BYTES.toLocaleString()} bytes
                  {guidanceOverCap ? " · too long" : ""}
                </p>
                <Button
                  size="sm"
                  disabled={busy || guidanceOverCap}
                  aria-describedby={errDescribedBy}
                  onClick={submitContinue}
                >
                  Continue run
                </Button>
              </div>
            )}

            {mode === "partial" && !confirming && (
              <div className="space-y-2">
                <p className="text-xs text-muted">
                  Keep the milestones that still matter and defer the rest out of scope. At least
                  one must stay in scope and at least one must be deferred; the reduced-scope PR
                  is visibly partial and does not close the issue.
                </p>
                {inScope.length === 0 ? (
                  <p className="text-xs text-muted">No in-scope milestones remain to reduce.</p>
                ) : (
                  <ul className="space-y-1">
                    {inScope.map((m) => {
                      const locked = acceptedIds.has(m.id);
                      return (
                        <li key={m.id} className="flex items-center gap-2 text-xs text-fg">
                          <input
                            type="checkbox"
                            checked={!defer.has(m.id)}
                            disabled={busy || locked}
                            aria-label={`Keep ${stripUnsafeChars(m.title)} in scope`}
                            onChange={() => setDefer((s) => toggleIn(s, m.id))}
                          />
                          <span className="font-medium">{stripUnsafeChars(m.title)}</span>
                          <span className="font-mono text-muted">{stripUnsafeChars(m.id)}</span>
                          {locked && (
                            <Badge tone="info" title="This milestone's criterion is already accepted, so it stays in scope">
                              criterion accepted
                            </Badge>
                          )}
                        </li>
                      );
                    })}
                  </ul>
                )}
                {deferred.length > 0 && (
                  <div className="rounded border border-edge bg-raised/40 p-2">
                    <h4 className="text-[11px] font-medium text-muted">Already deferred</h4>
                    <ul className="mt-1 space-y-0.5">
                      {deferred.map((d) => (
                        <li key={`${d.milestone_id}-${d.revision}`} className="text-[11px] text-muted">
                          {stripUnsafeChars(titleOf.get(d.milestone_id) ?? d.milestone_id)} — deferred
                          at rev {d.revision} — {stripUnsafeChars(d.reason)}
                        </li>
                      ))}
                    </ul>
                  </div>
                )}
                <ReasonField
                  reason={reason}
                  bytes={reasonBytes}
                  overCap={reasonOverCap}
                  busy={busy}
                  describedBy={errDescribedBy}
                  onChange={setReason}
                />
                <Button
                  size="sm"
                  disabled={busy || !partialOk}
                  aria-describedby={errDescribedBy}
                  onClick={() => {
                    setInlineErr("");
                    setConfirming(true);
                  }}
                >
                  Review &amp; confirm
                </Button>
              </div>
            )}

            {mode === "partial" && confirming && (
              <ConfirmStep
                busy={busy}
                describedBy={errDescribedBy}
                heading="Reduce scope"
                confirmLabel="Confirm scope reduction"
                onConfirm={submitPartial}
                onBack={() => setConfirming(false)}
              >
                <p className="text-xs text-fg">
                  Deferring {defer.size} milestone{defer.size === 1 ? "" : "s"} out of scope;{" "}
                  {keptIds.length} kept.
                </p>
                <ul className="mt-1 space-y-0.5">
                  {inScope
                    .filter((m) => defer.has(m.id))
                    .map((m) => (
                      <li key={m.id} className="text-xs text-muted">
                        Defer {stripUnsafeChars(m.title)}{" "}
                        <span className="font-mono">{stripUnsafeChars(m.id)}</span>
                      </li>
                    ))}
                </ul>
                <p className="mt-2 text-xs text-fg">Reason: {stripUnsafeChars(reason)}</p>
              </ConfirmStep>
            )}

            {mode === "accept" && !confirming && (
              <div className="space-y-2">
                <p className="text-xs text-muted">
                  Accept unmet criteria as delivered. Accepting one criterion never accepts
                  another; a closing PR names each accepted criterion and your reason.
                </p>
                {acceptCandidates.length === 0 ? (
                  <p className="text-xs text-muted">No unmet criteria are available to accept.</p>
                ) : (
                  <ul className="space-y-1">
                    {acceptCandidates.map((c) => (
                      <li key={c.id} className="flex items-center gap-2 text-xs text-fg">
                        <input
                          type="checkbox"
                          checked={accepting.has(c.milestoneId)}
                          disabled={busy}
                          aria-label={`Accept ${stripUnsafeChars(c.text)}`}
                          onChange={() => setAccepting((s) => toggleIn(s, c.milestoneId))}
                        />
                        <span className="font-medium">{stripUnsafeChars(c.text)}</span>
                        <span className="font-mono text-muted">{stripUnsafeChars(c.id)}</span>
                      </li>
                    ))}
                  </ul>
                )}
                <ReasonField
                  reason={reason}
                  bytes={reasonBytes}
                  overCap={reasonOverCap}
                  busy={busy}
                  describedBy={errDescribedBy}
                  onChange={setReason}
                />
                <Button
                  size="sm"
                  disabled={busy || !acceptOk}
                  aria-describedby={errDescribedBy}
                  onClick={() => {
                    setInlineErr("");
                    setConfirming(true);
                  }}
                >
                  Review &amp; confirm
                </Button>
              </div>
            )}

            {mode === "accept" && confirming && (
              <ConfirmStep
                busy={busy}
                describedBy={errDescribedBy}
                heading="Accept criteria"
                confirmLabel="Confirm acceptance"
                onConfirm={submitAccept}
                onBack={() => setConfirming(false)}
              >
                <p className="text-xs text-fg">
                  Accepting {selectedCriterionIds.length} criteri
                  {selectedCriterionIds.length === 1 ? "on" : "a"}.
                </p>
                <ul className="mt-1 space-y-0.5">
                  {acceptCandidates
                    .filter((c) => accepting.has(c.milestoneId))
                    .map((c) => (
                      <li key={c.id} className="text-xs text-muted">
                        Accept {stripUnsafeChars(c.text)}{" "}
                        <span className="font-mono">{stripUnsafeChars(c.id)}</span>
                      </li>
                    ))}
                </ul>
                <p className="mt-2 text-xs text-fg">Reason: {stripUnsafeChars(reason)}</p>
              </ConfirmStep>
            )}
          </div>
        </div>
      )}

      {inlineErr && (
        <p role="alert" id={ERROR_ID} className="mt-2 text-xs font-medium text-danger">
          {inlineErr}
        </p>
      )}
    </Card>
  );
}

// ReasonField is the required free-text reason shared by partial and accept, with the same
// byte-cap readout the guidance field carries.
function ReasonField({
  reason,
  bytes,
  overCap,
  busy,
  describedBy,
  onChange,
}: {
  reason: string;
  bytes: number;
  overCap: boolean;
  busy: boolean;
  describedBy?: string;
  onChange: (next: string) => void;
}) {
  return (
    <div className="space-y-1">
      <label htmlFor="completion-decision-reason" className="block text-xs font-medium text-muted">
        Reason (required)
      </label>
      <Textarea
        id="completion-decision-reason"
        className="min-h-[3.5rem] text-xs"
        value={reason}
        disabled={busy}
        aria-describedby={describedBy}
        onChange={(e) => onChange(e.target.value)}
        placeholder="Why this scope change is the right call — this is recorded in the audit history and the PR."
      />
      <p className={cx("text-[11px]", overCap ? "font-medium text-danger" : "text-muted")}>
        {bytes.toLocaleString()} / {MAX_DECISION_REASON_BYTES.toLocaleString()} bytes
        {overCap ? " · too long" : ""}
      </p>
    </div>
  );
}

// ConfirmStep is the "Review & confirm" gate before an irreversible partial/accept submit.
function ConfirmStep({
  heading,
  confirmLabel,
  busy,
  describedBy,
  onConfirm,
  onBack,
  children,
}: {
  heading: string;
  confirmLabel: string;
  busy: boolean;
  describedBy?: string;
  onConfirm: () => void;
  onBack: () => void;
  children: ReactNode;
}) {
  return (
    <div className="space-y-2 rounded border border-warn/40 bg-warn/10 p-3">
      <h3 className="text-xs font-semibold text-fg">Review &amp; confirm — {heading}</h3>
      {children}
      <p className="text-[11px] text-warn">
        This creates a new contract revision and invalidates the current completion permit. It
        cannot be undone.
      </p>
      <div className="flex items-center gap-2">
        <Button size="sm" disabled={busy} aria-describedby={describedBy} onClick={onConfirm}>
          {confirmLabel}
        </Button>
        <Button variant="secondary" size="sm" disabled={busy} onClick={onBack}>
          Back
        </Button>
      </div>
    </div>
  );
}
