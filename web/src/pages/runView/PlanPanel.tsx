import { useCallback, useMemo, useState } from "react";
import {
  type AgentSelectionInput,
  type Run,
  type RunMessage,
  type Worker,
} from "../../lib/api";
import { stripUnsafeChars } from "../../lib/safeText";
import { effectiveWorkerCaps } from "../../lib/workerCaps";
import { AgentPicker, selectionLabel, type OwnTemplate } from "../../components/AgentPicker";
import { Markdown } from "../../components/Markdown";
import { Badge, Button, Spinner, Textarea, cx } from "../../components/ui";

// The server enforces the real revision cap; 3 is only the display default for the
// "revision N of MAX" counter (PRD #41 Decision 9).
const MAX_REVISION_ROUNDS = 3;

// PlanRevision is the panel's derived view of the revision history (PRD #41 Decision
// 9): all of it comes from the run's feed messages, never a separate fetch.
export interface PlanRevision {
  // versions = number of `plan` messages so far (v1, v2, …).
  versions: number;
  // rounds = number of `plan_feedback` rounds — the "revision N of MAX" counter.
  rounds: number;
  // revising = the LATEST of {`plan`, `plan_revising`} by seq is a `plan_revising`
  // frame, i.e. the planner is reworking and the gate is parked (not open).
  revising: boolean;
  // latestFeedback = the newest `plan_feedback` steering text (the user's bubble).
  latestFeedback: string | null;
  // priorPlans = the plan_md of every SUPERSEDED plan version (all but the latest),
  // oldest-first — the collapsed history accordion once a v2+ is re-gated.
  priorPlans: string[];
}

// derivePlanRevision folds the feed into the panel's revision state. Exported for a
// direct unit test of the derivation (the component test drives the UI on top of it).
export function derivePlanRevision(messages: RunMessage[]): PlanRevision {
  const plans = messages.filter((m) => m.kind === "plan");
  const feedbacks = messages.filter((m) => m.kind === "plan_feedback");

  // The gate's live state is the latest of {plan, plan_revising} BY SEQ — a newer
  // `plan` (re-gated) beats an earlier `plan_revising`, and vice-versa.
  let latestGating: RunMessage | undefined;
  for (const m of messages) {
    if (m.kind !== "plan" && m.kind !== "plan_revising") continue;
    if (!latestGating || m.seq > latestGating.seq) latestGating = m;
  }

  let latestFeedback: string | null = null;
  let latestFeedbackSeq = -Infinity;
  for (const m of feedbacks) {
    const fb = (m.payload as { feedback?: string } | null)?.feedback;
    if (typeof fb === "string" && m.seq > latestFeedbackSeq) {
      latestFeedback = fb;
      latestFeedbackSeq = m.seq;
    }
  }

  const priorPlans = plans
    .slice(0, Math.max(0, plans.length - 1))
    .map((m) => (m.payload as { plan_md?: string } | null)?.plan_md ?? "")
    .filter((s) => s.trim() !== "");

  return {
    versions: plans.length,
    rounds: feedbacks.length,
    revising: latestGating?.kind === "plan_revising",
    latestFeedback,
    priorPlans,
  };
}

// VersionChip is the mono v1/v2 badge in the panel head. Info-toned in the revising
// (parked) state, warn-toned at an open gate — matching the panel's own tone.
function VersionChip({ label, parked }: { label: string; parked?: boolean }) {
  return (
    <span
      className={cx(
        "inline-flex items-center rounded-md border px-1.5 py-px font-mono text-[11px] font-semibold",
        parked ? "border-info/40 bg-info/[0.12] text-info" : "border-warn/40 bg-warn/[0.12] text-warn",
      )}
    >
      {label}
    </span>
  );
}

// RevisionThread renders the user's steering bubble (PRD #41). The feedback is
// UNTRUSTED, so it goes through the hardened <Markdown> — never a raw sink. `working`
// appends the info-toned "planner is reworking" spinner line (the parked state).
function RevisionThread({ feedback, working = false }: { feedback: string | null; working?: boolean }) {
  if (!feedback && !working) return null;
  return (
    <div className="space-y-2.5">
      <span className="text-[11px] font-semibold uppercase tracking-wider text-faint">Revision thread</span>
      {feedback && (
        <div className="ml-auto max-w-[85%] rounded-lg border border-brand/30 bg-brand/[0.12] px-3 py-2 text-sm">
          <span className="mb-1 block text-[11px] font-semibold text-brand">You requested</span>
          <Markdown content={feedback} />
        </div>
      )}
      {working && (
        <div className="flex items-center gap-2 text-sm italic text-muted">
          <Spinner /> planner is reworking the plan…
        </div>
      )}
    </div>
  );
}

// PlanPanel: the run's one human decision point — visually the loudest thing on
// the page while it is pending. Grows the PRD #37 agent picker: the user chooses
// the subagent roster (repo agents when detected, else their templates) with the
// approve verdict; the choice is submitted as a structured selection on approve.
// PRD #41 adds the third gate action (Request changes → revise_plan), the revising
// parked state, the version chip, and the collapsed history of superseded plans.
export function PlanPanel({
  run,
  messages = [],
  workers = [],
  busy,
  canSteer = true,
  onApprove,
  onReject,
  onRequestChanges,
  onCancel,
}: {
  run: Run;
  // The run's feed, used to derive the version chip / round counter / revising state
  // (PRD #41 Decision 9). Optional so a caller/test that only exercises the base gate
  // can omit it — the derivation degrades to v1 / revision 0 of 3.
  messages?: RunMessage[];
  // PRD #84 M4 4d: the fleet, so the readiness summary can look up the run's assigned
  // worker (run.worker_id) and decide which required capabilities it satisfies. Optional
  // + defaulted to [] so a base-gate-only caller/test need not wire it — an empty list
  // simply renders every required capability as unmet (the honest fail-closed reading).
  workers?: Worker[];
  busy: boolean;
  // False for a NON-OWNER viewer. PRE-EXISTING and unrelated to PRD #88: POST /inputs is
  // user-scoped, so a non-owner admin — who can legitimately OPEN this owner-or-admin run
  // view — got an Approve button that 404s. useRunStream states the rule ("never a broken
  // Send that 404s") and SteerQueueCard already obeyed it; this panel never did. Fixed
  // alongside the question composer because fixing only the newer one would have left the
  // identical hole one panel over. Defaults true so an owner is never gated by an absent
  // prop, and it hides the ACTIONS only — the plan body stays readable, which is the whole
  // reason a non-owner admin opens this page.
  canSteer?: boolean;
  // PRD #84 M4 4d: onApprove takes an optional overrideCapabilities flag — the "run without
  // the capability" false-positive correction. The normal Approve button omits it (undefined
  // ≡ false); the readiness block's override button passes true.
  onApprove: (selection: AgentSelectionInput, overrideCapabilities?: boolean) => void;
  onReject: (reason: string) => void;
  // Request-changes (PRD #41) and the revising-state Cancel-run affordance. Optional
  // with a no-op default so a base-gate-only caller need not wire them.
  onRequestChanges?: (feedback: string) => void;
  onCancel?: () => void;
}) {
  const [rejecting, setRejecting] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const [reason, setReason] = useState("");
  const [feedback, setFeedback] = useState("");

  const rev = useMemo(() => derivePlanRevision(messages), [messages]);

  const repoAgents = useMemo(() => run.repo_agents ?? [], [run.repo_agents]);
  const repoDetected = repoAgents.length > 0;

  // The "My agent templates" card is sourced from the run's own_agents — the
  // server's allocation-resolved roster (what the worker actually runs for
  // source="own"), with the lead already stripped. Reading it off the run (instead
  // of a separate listAgentTemplates fetch of the broader VISIBLE set) is the
  // M4-fix: a chip can never name a template the approve validator rejects, and the
  // count is exact for owners with a disabled/shadowed template. own_agents carries
  // no scope, so the "custom" badge is not shown here.
  const ownTemplates = useMemo<OwnTemplate[]>(
    () => (run.own_agents ?? []).map((a) => ({ name: a.name, description: a.description, custom: false })),
    [run.own_agents],
  );

  // The picker reports the live selection here; the approve button submits it. The
  // default (repo when detected, else own, no exclusions) is what the picker emits
  // on mount, so approving without touching anything sends the right thing.
  const [selection, setSelection] = useState<AgentSelectionInput>({
    source: repoDetected ? "repo" : "own",
    exclusions: [],
  });
  const onSelectionChange = useCallback((s: AgentSelectionInput) => setSelection(s), []);

  const activeRoster = selection.source === "repo" ? repoAgents.map((a) => a.name) : ownTemplates.map((t) => t.name);
  const activeCount = activeRoster.length - selection.exclusions.length;
  const approveLabel =
    activeRoster.length > 0 ? `Approve plan · ${selectionLabel(selection.source, activeCount)}` : "Approve plan";

  // PRD #84 M4 4d: the pre-run readiness summary. Requirements are inferred/hinted
  // server-side and surfaced RAW on the run (run.required_*); "met" means the run's ASSIGNED
  // worker (run.worker_id) advertises the capability. required_tools never block (they are
  // provisioned at run time); size_class is advisory. An empty workers list (fetch not yet
  // in, or a non-owner with no read) renders every required cap as unmet — the honest
  // fail-closed reading, matching the 409 the approve gate would return.
  const requiredCaps = useMemo(() => run.required_capabilities ?? [], [run.required_capabilities]);
  const requiredTools = useMemo(() => run.required_tools ?? [], [run.required_tools]);
  const sizeClass = run.size_class ?? "";
  const workerCaps = useMemo(() => {
    const w = run.worker_id ? workers.find((x) => x.id === run.worker_id) : undefined;
    // Fold docker_enabled in exactly as the server does, via the shared effectiveWorkerCaps
    // (web mirror of fn_effective_worker_caps / capability.EffectiveWorkerCaps, single source
    // since #512 M5: capabilities ∪ {docker if docker_enabled}).
    return effectiveWorkerCaps(w?.capabilities, w?.docker === true);
  }, [run.worker_id, workers]);
  const unmetCaps = useMemo(() => requiredCaps.filter((c) => !workerCaps.has(c)), [requiredCaps, workerCaps]);
  // Whether to render the readiness panel at all. size_class is DELIBERATELY excluded: the
  // agent's detectToolchain always emits a non-empty size_class (s/m/l), so including it here
  // made the panel render for EVERY plan gate. size is advisory-only, so it never justifies the
  // panel on its own — it is shown as a minor detail INSIDE the panel when a capability or tool
  // already opened it, and simply not shown when nothing else was inferred. A block is a subset
  // of requiredCaps, so "a capability, a tool, or a block" reduces to caps-or-tools here.
  const hasRequirements = requiredCaps.length > 0 || requiredTools.length > 0;

  // The rounds counter is always shown at the head; MAX is the display default (the
  // server owns the real cap).
  const roundsLabel = `revision ${rev.rounds} of ${MAX_REVISION_ROUNDS}`;

  // Revising (parked) state: the planner is reworking after a request-changes. The
  // panel goes info-toned, swaps the gate for the revision thread + a Cancel-run
  // affordance, and shows a v(N)→v(N+1) chip (the next version has not landed yet).
  if (rev.revising) {
    return (
      <div className="overflow-hidden rounded-xl border border-info/40 bg-info/5">
        <div className="flex flex-wrap items-center justify-between gap-3 border-b border-info/25 bg-info/[0.08] px-4 py-3">
          <div>
            <h2 className="flex items-center gap-2 text-sm font-semibold text-info">
              Revising the plan
              <VersionChip parked label={`v${rev.versions} → v${rev.versions + 1}`} />
            </h2>
            <p className="text-xs text-muted">
              Your feedback was sent to the planning session. The updated plan will return here for approval.
            </p>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[11px] text-faint">{roundsLabel}</span>
            {canSteer && (
              <Button variant="danger" disabled={busy} onClick={() => onCancel?.()}>
                Cancel run
              </Button>
            )}
          </div>
        </div>
        <div className="p-4">
          <RevisionThread feedback={rev.latestFeedback} working />
        </div>
      </div>
    );
  }

  // Open gate. A v2+ re-gate reads "Updated plan…" and shows the superseded history.
  const revised = rev.versions > 1;
  const currentVersion = Math.max(rev.versions, 1);
  const disclosing = rejecting || requesting;

  return (
    <div className="overflow-hidden rounded-xl border border-warn/50 bg-warn/5">
      <div className="flex flex-wrap items-center justify-between gap-3 border-b border-warn/30 bg-warn/10 px-4 py-3">
        <div>
          <h2 className="flex items-center gap-2 text-sm font-semibold text-warn">
            {/* The HEADING is conditional on canSteer for the same reason the subtitle
                below is. Leaving it unconditional put "Plan awaiting your approval"
                directly above "Only they can approve or reject it" for a non-owner —
                a card contradicting itself on adjacent lines. QuestionPanel's non-owner
                branch changes both, and this is the older panel's half of that fix. */}
            {!canSteer
              ? revised
                ? "Updated plan awaiting the owner's approval"
                : "Plan awaiting the owner's approval"
              : revised
                ? "Updated plan awaiting your approval"
                : "Plan awaiting your approval"}
            <VersionChip label={`v${currentVersion}`} />
          </h2>
          <p className="text-xs text-muted">
            {!canSteer
              ? "The run is parked until its owner decides. Only they can approve or reject it."
              : requesting
                ? "Describe what should change; the other actions return if you cancel."
                : "The run is parked until you decide. Agent choice locks in on approval."}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <span className="text-[11px] text-faint">{roundsLabel}</span>
          {canSteer && !disclosing && (
            <div className="flex gap-2">
              <Button disabled={busy} onClick={() => onApprove(selection)}>
                {approveLabel}
              </Button>
              <Button variant="secondary" disabled={busy} onClick={() => setRequesting(true)}>
                Request changes
              </Button>
              <Button variant="danger" disabled={busy} onClick={() => setRejecting(true)}>
                Reject
              </Button>
            </div>
          )}
        </div>
      </div>
      <div className="space-y-4 p-4">
        {/* The picker exists to shape the approve verdict, so it is an action surface:
            shown only to someone who can approve. The locked-in roster is a separate,
            read-only card (AgentRosterSummary) once the run is past the gate. */}
        {canSteer && (
          <AgentPicker repoAgents={repoAgents} ownTemplates={ownTemplates} onChange={onSelectionChange} />
        )}

        {/* PRD #122: the PRE-APPROVAL candidate milestones, shown ONLY at the plan gate,
            beside the plan body. Rendered as PLAIN JSX (never <Markdown>): a candidate
            title is agent/repo-authored untrusted text, and this is an approval dialog —
            the exact place an attacker-authored title must not become a clickable link
            (same rule as the repo-agent descriptions in the picker above). Nothing renders
            when the list is null/empty. */}
        {run.milestones_candidate && run.milestones_candidate.length > 0 && (
          <div className="rounded-lg border border-edge bg-surface/60 p-3">
            <p className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-faint">Proposed milestones</p>
            <ol className="list-decimal space-y-1 pl-5 text-sm text-fg">
              {run.milestones_candidate.map((m) => (
                <li key={m.id} className="min-w-0">
                  {stripUnsafeChars(m.title)}
                </li>
              ))}
            </ol>
          </div>
        )}

        {/* PRD #84 M4 4d: the pre-run readiness summary — the run's inferred/hinted
            requirements checked against the assigned worker, shown between the candidate
            milestones and the plan body. Renders NOTHING when no CAPABILITY or TOOL was
            inferred — size_class alone (always emitted) does not open it (see hasRequirements).
            Capability/tool names are a server-Filter-ed vocabulary, but rendered through
            stripUnsafeChars anyway (defense-in-depth, matching the candidate milestones above) —
            this is an approval dialog. */}
        {hasRequirements && (
          <div className="rounded-lg border border-edge bg-surface/60 p-3">
            <p className="mb-2 text-[11px] font-semibold uppercase tracking-wider text-faint">Run requirements</p>

            {requiredCaps.length > 0 && (
              <div className="mb-2">
                <div className="flex flex-wrap items-center gap-1.5">
                  {requiredCaps.map((cap) => {
                    const unmet = !workerCaps.has(cap);
                    return (
                      <Badge
                        key={cap}
                        tone={unmet ? "warning" : "ok"}
                        title={
                          unmet
                            ? `The assigned worker does not advertise "${stripUnsafeChars(cap)}" — this plan cannot run here until one that does claims it, or you run without it.`
                            : `The assigned worker advertises "${stripUnsafeChars(cap)}".`
                        }
                      >
                        {unmet ? "⚠ " : "✓ "}
                        {stripUnsafeChars(cap)}
                      </Badge>
                    );
                  })}
                </div>
                {unmetCaps.length > 0 && (
                  <p className="mt-1.5 text-xs text-warn">
                    Provision or start a worker with:{" "}
                    <span className="font-medium">{unmetCaps.map((c) => stripUnsafeChars(c)).join(", ")}</span>, or run
                    without it.
                  </p>
                )}
              </div>
            )}

            {requiredTools.length > 0 && (
              <div className="mb-2 flex flex-wrap items-center gap-1.5">
                {requiredTools.map((tool) => (
                  <Badge key={tool} tone="neutral" title={`Toolchain provisioned at run time: ${tool}`}>
                    {stripUnsafeChars(tool)}
                  </Badge>
                ))}
                <span className="text-[11px] text-faint">will be provisioned</span>
              </div>
            )}

            {sizeClass !== "" && (
              <span className="inline-flex items-center rounded-md border border-edge bg-raised/50 px-1.5 py-px text-[11px] font-medium text-muted">
                {`size: ${stripUnsafeChars(sizeClass)}`}
              </span>
            )}

            {/* The false-positive override (PRD #84 M4 4c/4d, Decision 12): when a required
                capability is unmet AND the viewer can steer, offer a distinct SECONDARY
                approve that clears the inferred requirements server-side ("run without it").
                Visually separated from the header's primary Approve so it never reads as the
                default path. */}
            {unmetCaps.length > 0 && canSteer && (
              <div className="mt-2.5 border-t border-edge/60 pt-2.5">
                <Button variant="secondary" size="sm" disabled={busy} onClick={() => onApprove(selection, true)}>
                  Run without {unmetCaps.map((c) => stripUnsafeChars(c)).join(", ")}
                </Button>
                <p className="mt-1 text-[11px] text-faint">
                  Approves the plan and drops the inferred capability requirement, in case the inference is a false
                  positive. The runtime guardrail still refuses the capability&rsquo;s use on a worker that lacks it.
                </p>
              </div>
            )}
          </div>
        )}

        {run.plan_md ? (
          <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
            <Markdown content={run.plan_md} />
          </div>
        ) : (
          <p className="text-sm text-faint">The agent has not attached a plan body.</p>
        )}

        {/* PRD #212: the git-status porcelain lines the plan turn wrote to the worktree,
            surfaced at the gate so the approving human sees writes that would otherwise be
            swept into the first implement commit unseen. Renders ONLY when non-empty (a
            clean plan turn and any pre-#212 run both show nothing). Each line is UNTRUSTED
            repo-controlled text — rendered as escaped plain JSX through stripUnsafeChars,
            NEVER <Markdown> and never into an href/URL sink (issue #124 hardening). One
            element may be a synthetic non-porcelain truncation marker (`… (+K more)`) that
            has no status code, so lines are printed verbatim, not parsed. */}
        {run.plan_changed_files && run.plan_changed_files.length > 0 && (
          <div className="rounded-lg border border-edge bg-surface p-3">
            <p className="mb-1 text-[11px] font-semibold uppercase tracking-wider text-faint">
              Files changed during planning
            </p>
            <p className="mb-2 text-xs text-faint">
              The planning turn wrote these changes to the worktree; they would be swept into the first commit if you
              approve.
            </p>
            <ul className="space-y-0.5 font-mono text-xs text-fg">
              {run.plan_changed_files.map((line, i) => (
                <li key={i} className="min-w-0 whitespace-pre-wrap break-all">
                  {stripUnsafeChars(line)}
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* Request-changes composer: disclosed only after selecting the action, the
            same pattern as the reject-with-reason disclosure. "Send & revise" submits
            revise_plan; Cancel restores the header actions. */}
        {requesting && (
          <div className="space-y-2">
            <Textarea
              rows={3}
              placeholder="What should change? (sent to the planning session; the plan returns here for approval)"
              value={feedback}
              onChange={(e) => setFeedback(e.target.value)}
            />
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-[11px] text-faint">
                Feedback goes to the planning session; the agent revises and the plan returns here for approval.{" "}
                {MAX_REVISION_ROUNDS} revision rounds max.
              </span>
              <div className="flex gap-2">
                <Button variant="ghost" disabled={busy} onClick={() => setRequesting(false)}>
                  Cancel
                </Button>
                <Button disabled={busy || feedback.trim() === ""} onClick={() => onRequestChanges?.(feedback)}>
                  Send &amp; revise
                </Button>
              </div>
            </div>
          </div>
        )}

        {/* Reject-with-reason disclosure (unchanged semantics). */}
        {rejecting && (
          <div className="space-y-2">
            <Textarea
              rows={3}
              placeholder="What should change? (sent back to the agent as the next turn)"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
            />
            <div className="flex gap-2">
              <Button variant="danger" disabled={busy} onClick={() => onReject(reason)}>
                Send rejection
              </Button>
              <Button variant="ghost" disabled={busy} onClick={() => setRejecting(false)}>
                Cancel
              </Button>
            </div>
          </div>
        )}

        {/* After a revision the user's steering bubble stays visible above the gate. */}
        {revised && !disclosing && <RevisionThread feedback={rev.latestFeedback} />}

        {/* Collapsed history of superseded plan versions (v1, …) once a v2+ is gated. */}
        {rev.priorPlans.length > 0 && !disclosing && (
          <details className="rounded-lg border border-edge bg-surface/50">
            <summary className="cursor-pointer px-3 py-2 text-xs text-muted">
              {rev.priorPlans.length === 1
                ? "Plan v1 · superseded"
                : `${rev.priorPlans.length} superseded plan versions`}
            </summary>
            <div className="space-y-3 border-t border-edge p-3">
              {rev.priorPlans.map((md, i) => (
                <div key={i} className="border-l-2 border-edge-strong pl-3 text-xs text-muted">
                  <div className="mb-1 font-semibold text-faint">Plan v{i + 1}</div>
                  <Markdown content={md} />
                </div>
              ))}
            </div>
          </details>
        )}
      </div>
    </div>
  );
}

// SeededPlanPanel (PRD #209 M5): a seeded run receives its plan from the user at
// create time and never enters awaiting_approval, so PlanPanel — the approval UI —
// never renders and run.plan_md would otherwise be UNREACHABLE on the run page. This
// read-only collapsible surfaces the seeded plan in any state (queued through
// terminal). It is deliberately NOT the gate: it carries no approve/reject/revise
// actions and does not read the feed, so it cannot be confused with the approval UI
// and does not touch the awaiting_approval path (Success Criterion 2 / anti-regression).
//
// The plan is UNTRUSTED input (user-authored, D5) but is size-capped and secret-scrubbed
// server-side at create time; it renders through the same hardened <Markdown> the gate
// uses for plan_md, never a raw HTML sink. Defaults OPEN: the whole point of the
// milestone is that this content was unreachable, so hiding it behind a click would
// re-hide it; the inner max-h scroll keeps a long plan from dominating the page.
export function SeededPlanPanel({ run }: { run: Run }) {
  // Belt-and-braces: a seeded run always carries a non-empty plan_md (M1 rejects an
  // empty/whitespace plan with a 422 at create time), but render nothing rather than an
  // empty box if a caller ever passes a run without one.
  if (!run.plan_md) return null;
  return (
    <details open className="overflow-hidden rounded-xl border border-edge bg-raised/40">
      <summary className="cursor-pointer px-4 py-3 text-sm font-semibold text-fg">
        Seeded plan
        <span className="ml-2 text-xs font-normal text-faint">
          you supplied this plan when starting the run — it is implemented without an approval gate
        </span>
      </summary>
      <div className="border-t border-edge p-4">
        <div className="max-h-96 overflow-auto rounded-lg border border-edge bg-surface p-3">
          <Markdown content={run.plan_md} />
        </div>
      </div>
    </details>
  );
}
