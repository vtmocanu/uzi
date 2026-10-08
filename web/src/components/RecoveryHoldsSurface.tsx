import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { api, type RecoveryCustodyHold, type RecoveryCustodyHolds } from "../lib/api";
import { errorMessage } from "../lib/apiError";
import { custodyHoldView, groupHoldsByWorker, recoveryCapacityUsed, type CustodyCheckpointView } from "../lib/recovery";
import { stripUnsafeChars } from "../lib/safeText";
import { usePollWhileVisible } from "../lib/usePollWhileVisible";
import { Badge, Button, Card, SectionTitle, cx } from "./ui";
import { ShieldIcon, ChevronRightIcon, TrashIcon } from "./icons";

// RecoveryHoldsSurface is the durable, detailed owner resolution surface for custody holds
// (PRD #1349 M6, D6/D7/D8/D9). It lives at the top of the Workers page and is the home for
// resolving held work: every exact hold, GROUPED BY WORKER, classified by its server-derived
// `attention` so healthy active protection is visually separated from work that needs a
// decision. It self-hides when there are no holds to show.
//
// It fetches its own owner-wide hold listing (GET /api/recovery/holds — RequireUser, the
// caller's own holds across every run) and refreshes on the visible poll cadence, so it
// tracks the reconciler live. Each hold links to its Run view, where the archive export /
// delete controls live (and where the D7 secret-review warning sits above every download).
// The one action taken inline is the strongest and most important: Discard held work — the
// exact hold discard that can finally disposition a capture-less, possible-only-copy hold.
export function RecoveryHoldsSurface() {
  const [holds, setHolds] = useState<RecoveryCustodyHolds | null>(null);

  const load = useCallback(async () => {
    try {
      setHolds(await api.getRecoveryHolds());
    } catch {
      // Best-effort: a fetch failure keeps the last-good listing (or stays hidden on first
      // load). This surface never breaks the rest of the Workers page.
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);
  usePollWhileVisible(load, 10000);

  if (!holds) return null;
  // Show owner decisions and exact MAC-rejected holds independently of attention.
  // The diagnostic changes visibility only; classification, actions and counts stay intact.
  const visibleHolds = holds.holds.filter((h) =>
    custodyHoldView(h).needsDecision || h.terminal_record_rejection === "mac_failure",
  );
  if (visibleHolds.length === 0) return null;
  const groups = groupHoldsByWorker(visibleHolds);
  // Open holds per run, counted over the FULL listing (not just the decision rows): the
  // retained checkpoint ref follows custody, so discarding a run's last open hold also deletes
  // that ref on the forge (PRD #1810). A healthy sibling hold filtered out above still counts.
  // Allowlist, mirroring the server: it deletes the ref only when no hold of the run has
  // state 'open', so any other state (released, discarded, or one this client does not
  // know yet) must not suppress the warning.
  const openHoldsByRun = new Map<string, number>();
  for (const h of holds.holds) {
    if (h.state !== "open") continue;
    openHoldsByRun.set(h.run_id, (openHoldsByRun.get(h.run_id) ?? 0) + 1);
  }
  const { open_holds, custody_hold_limit, decision_needed } = holds.aggregate;
  const capacityUsed = recoveryCapacityUsed(holds.aggregate);

  return (
    <Card id="recovery-holds" className="scroll-mt-20 space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-1">
        <div className="flex items-center gap-2">
          <ShieldIcon className="h-4 w-4 text-muted" aria-hidden="true" />
          <SectionTitle>Held work</SectionTitle>
        </div>
        <p className="text-xs text-faint">
          <span className="tabular-nums text-muted">
            {capacityUsed} / {custody_hold_limit}
          </span>{" "}
          custody slots used
          {" · "}
          <span className="tabular-nums text-muted">{open_holds} open custody {open_holds === 1 ? "hold" : "holds"}</span>
          {decision_needed > 0 && (
            <>
              {" · "}
              <span className="font-medium text-warn">{decision_needed} need a decision</span>
            </>
          )}
        </p>
      </div>

      <p className="text-sm text-muted">
        Each hold retains one run's unpublished committed work so it survives a failed
        finalization. These are the holds only you can resolve.
      </p>

      <div className="space-y-4">
        {groups.map((g) => (
          <section key={g.workerId} aria-label={`Holds on ${g.workerName ? stripUnsafeChars(g.workerName) : "a worker"}`}>
            <div className="flex flex-wrap items-baseline gap-x-2 gap-y-0.5 border-b border-edge pb-1">
              {/* worker_name is untrusted display (D7): strip control/format chars. worker_id
                  is the opaque, safe identity shown alongside for disambiguation. */}
              <span className="text-sm font-medium text-fg">
                {g.workerName ? stripUnsafeChars(g.workerName) : "Unnamed worker"}
              </span>
              <span className="font-mono text-xs text-faint">{shortId(g.workerId)}</span>
              {g.decisionCount > 0 && (
                <span className="ml-auto text-xs text-warn">
                  {g.decisionCount} to resolve
                </span>
              )}
            </div>
            <ul className="mt-2 space-y-2">
              {g.holds.map((hold) => (
                <HoldRow
                  key={hold.id}
                  hold={hold}
                  isLastOpenHold={openHoldsByRun.get(hold.run_id) === 1}
                  onChanged={load}
                />
              ))}
            </ul>
          </section>
        ))}
      </div>
    </Card>
  );
}

// HoldRow renders one exact hold and, where the source may be the only copy, the strongest
// possible-only-copy discard with a typed confirmation naming the run, worker, generation and
// hold id (D9). Archive-bearing holds link out to the run's Recovery archives section for
// export (keeping the D7 warning on the one download surface); active/capturing holds are
// information only.
function HoldRow({
  hold,
  isLastOpenHold,
  onChanged,
}: {
  hold: RecoveryCustodyHold;
  // True when this is the run's only open hold, so discarding it releases the run's custody
  // and with it the retained checkpoint ref on the forge.
  isLastOpenHold: boolean;
  // Returns the parent's reload promise so the discard handler can await it and clear `busy`
  // only once the listing has settled (see discard's finally).
  onChanged: () => void | Promise<void>;
}) {
  const view = custodyHoldView(hold);
  const [confirming, setConfirming] = useState(false);
  const [typed, setTyped] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [restoreFocus, setRestoreFocus] = useState(false);
  const confirmRef = useRef<HTMLDivElement>(null);
  // `Button` (components/ui.tsx) does not forward a ref, so the discard trigger is focused
  // by id (the same pattern the worker-delete confirm on this page uses).
  const discardBtnId = `discard-btn-${hold.id}`;

  // Focus the warning when the confirmation arms, so a keyboard user meets it and a screen
  // reader reads it — never auto-focus the destructive button (that makes the gate a
  // formality), mirroring the worker-delete confirm precedent on this page.
  useEffect(() => {
    if (confirming) confirmRef.current?.focus();
  }, [confirming]);

  // Return focus to the discard trigger after a cancel re-mounts it (it renders only while
  // not confirming), so backing out never dumps a keyboard user at <body>.
  useEffect(() => {
    if (restoreFocus && !confirming) {
      document.getElementById(discardBtnId)?.focus();
      setRestoreFocus(false);
    }
  }, [restoreFocus, confirming, discardBtnId]);

  const dismiss = () => {
    setConfirming(false);
    setTyped("");
    setError("");
    setRestoreFocus(true);
  };

  const discard = async () => {
    setBusy(true);
    setError("");
    try {
      await api.discardHold(hold.run_id, hold.id);
      // The row disappears on the next listing; reload rather than optimistically mutate so
      // the aggregate counts stay server-truthful. Await it so `busy` stays set (controls
      // disabled, "Discarding…") until the reload settles.
      await onChanged();
    } catch (e) {
      setError(errorMessage(e, "Could not discard the held work."));
    } finally {
      // Clear busy on every path — including a successful discard followed by a failed
      // reload, where `load` keeps the last-good listing so THIS row stays mounted. Without
      // the finally the row would wedge on "Discarding…" with its controls disabled until
      // the next poll. On a successful reload the row unmounts and this is a harmless no-op.
      setBusy(false);
    }
  };

  const runShort = shortId(hold.run_id);
  const holdShort = shortId(hold.id);
  const workerName = hold.worker_name ? stripUnsafeChars(hold.worker_name) : "this worker";
  const warningId = `discard-warning-${hold.id}`;
  const refWarningId = `discard-ref-warning-${hold.id}`;
  // Every checkpoint-backed hold warns: the server decides at discard time, so a sibling hold
  // that settles after this listing loaded would otherwise delete the ref with no warning.
  const refWarning = view.checkpoint;
  const inputId = `discard-input-${hold.id}`;
  const canDiscard = typed.trim().toLowerCase() === "discard";

  return (
    <li className="rounded-lg border border-edge bg-raised/40 p-3">
      <div className="flex flex-wrap items-start justify-between gap-x-3 gap-y-2">
        <div className="min-w-0 space-y-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <Badge tone={view.tone} wrap>
              {view.stateLabel}
            </Badge>
            <Badge tone="neutral" title="Claim generation — the exact attempt this hold protects.">
              gen {hold.generation}
            </Badge>
            {hold.capture_state && (
              <Badge tone="info" title="Archive capture state.">
                capture: {stripUnsafeChars(hold.capture_state)}
              </Badge>
            )}
          </div>
          <p className="text-sm text-muted">{view.summary}</p>
          {view.terminalRejection && <p className="text-sm text-muted">{view.terminalRejection}</p>}
          <p className="flex flex-wrap items-center gap-x-2 text-xs text-faint">
            <span className="font-mono">run {runShort}</span>
            <span className="font-mono">hold {holdShort}</span>
            <span>updated {new Date(hold.updated_at).toLocaleString()}</span>
          </p>
          {view.checkpoint && <CheckpointLocation checkpoint={view.checkpoint} />}
        </div>

        <div className="flex shrink-0 flex-wrap items-center justify-end gap-1.5">
          {/* The run title is not carried on the hold DTO (owner-safe fields only), so the
              link is labelled by run id; it takes the owner to the run for full context —
              and to the archive export/delete controls. */}
          <Link
            to={`/runs/${hold.run_id}`}
            className="inline-flex items-center gap-0.5 rounded-md border border-edge bg-raised px-2 py-1 text-xs font-medium text-fg hover:border-edge-strong"
          >
            View run <ChevronRightIcon />
          </Link>
          {view.actions.includes("export") && (
            <Link
              to={`/runs/${hold.run_id}#recovery-archives`}
              className="inline-flex items-center gap-0.5 rounded-md border border-ok/40 bg-ok/10 px-2 py-1 text-xs font-medium text-ok hover:bg-ok/20"
            >
              Export archive <ChevronRightIcon />
            </Link>
          )}
          {view.actions.includes("discard") && !confirming && (
            <Button
              id={discardBtnId}
              variant="danger"
              size="sm"
              onClick={() => setConfirming(true)}
            >
              <TrashIcon /> Discard held work
            </Button>
          )}
        </div>
      </div>

      {/* archive_ready reads "releasing automatically" and is NOT presented as a decision
          (D8/D9): custody clears itself through the reconciler. */}
      {view.autoReleasing && (
        <p className="mt-2 text-xs text-ok">Releasing automatically — no action needed.</p>
      )}

      {confirming && (
        <div
          ref={confirmRef}
          tabIndex={-1}
          role="group"
          aria-label={`Discard held work for run ${runShort} on ${workerName}, generation ${hold.generation}`}
          aria-describedby={refWarning ? `${warningId} ${refWarningId}` : warningId}
          onKeyDown={(e) => {
            if (e.key === "Escape") dismiss();
          }}
          className="mt-3 space-y-3 rounded-lg border border-danger/40 bg-danger/10 p-3 outline-hidden"
        >
          {/* The strongest confirmation (D9): name the run, the exact worker + claim
              generation, and the hold id, and state that this may be the only copy. */}
          <p id={warningId} className="text-sm text-danger">
            This discards run <span className="font-mono">{runShort}</span>&rsquo;s held work on{" "}
            <span className="font-medium">{workerName}</span> (generation {hold.generation}, hold{" "}
            <span className="font-mono">{holdShort}</span>).{" "}
            {hold.has_available_capture ? (
              <>
                An earlier available archive can restore older work but may not cover the latest
                worker-local work. The latest worker-local source may be the only copy.
              </>
            ) : (
              <>
                The worker-local source may be the only copy — no server archive can restore it.
              </>
            )}{" "}
            Discarding permits the worker or its disk to be torn down, which can destroy this work
            permanently. This cannot be undone.
          </p>
          {refWarning && (
            <p id={refWarningId} className="text-sm text-danger">
              {isLastOpenHold ? (
                <>This is the run&rsquo;s last open hold: discarding it also deletes the retained</>
              ) : (
                <>
                  If no other hold of this run is still open when you discard, this also deletes
                  the retained
                </>
              )}{" "}
              checkpoint ref <code className="break-all font-mono">{refWarning.ref}</code> on the
              forge. Fetch it first if you need it.
            </p>
          )}
          <div className="space-y-1.5">
            <label htmlFor={inputId} className="block text-xs font-medium text-muted">
              Type <span className="font-mono text-danger">discard</span> to confirm.
            </label>
            <input
              id={inputId}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoComplete="off"
              spellCheck={false}
              className="w-full max-w-xs rounded-lg border border-edge bg-raised px-3 py-2 text-sm text-fg outline-hidden focus:border-danger/70"
            />
          </div>
          {error && <p className="text-xs text-danger">{error}</p>}
          <div className="flex items-center gap-1.5">
            <Button
              variant="dangerSolid"
              size="sm"
              disabled={!canDiscard || busy}
              onClick={discard}
            >
              {busy ? "Discarding…" : "Discard held work"}
            </Button>
            <Button variant="ghost" size="sm" onClick={dismiss} disabled={busy}>
              Cancel
            </Button>
          </div>
        </div>
      )}
    </li>
  );
}

// CheckpointLocation says where this run's retained published checkpoint lives on the forge
// (PRD #1810): the branch checkpoint slot, or a recovery ref once a newer run on the same
// branch took that slot. The ref is the answer the owner needs to fetch the work, so it is
// the one monospace, full-contrast element; it breaks anywhere so a long branch name wraps on
// a phone instead of overflowing the row. A moved ref reads in the info tone so "moved, not
// lost" is distinguishable at a glance from work that still sits at its branch.
function CheckpointLocation({
  checkpoint,
}: {
  checkpoint: CustodyCheckpointView;
}) {
  const moved = checkpoint.kind === "recovery";
  return (
    <div
      className={cx("mt-1 space-y-0.5 border-l-2 pl-2 text-xs", moved ? "border-info/50" : "border-edge")}
    >
      <p className="flex flex-wrap items-baseline gap-x-2">
        <span className="text-faint">Checkpoint on forge</span>
        <span className={moved ? "font-medium text-info" : "text-muted"}>{checkpoint.label}</span>
      </p>
      <p className="break-all font-mono text-fg">
        {checkpoint.ref}
        {checkpoint.shortTip && (
          <span className="text-faint">
            {" "}@ {checkpoint.shortTip}
          </span>
        )}
      </p>
      {checkpoint.note && <p className="text-faint">{checkpoint.note}</p>}
    </div>
  );
}

// shortId trims an opaque id (UUID / run id / hold id) to its leading 8 for compact display.
// A shorter id is left untouched. These ids are server-minted identity, not untrusted free
// text, so no sanitizing is needed here.
function shortId(id: string): string {
  return id.length > 8 ? id.slice(0, 8) : id;
}
