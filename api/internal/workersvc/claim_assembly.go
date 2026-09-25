package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/toolprofile"
	"github.com/vtmocanu/uzi/api/internal/toolseed"
)

// openWithAutoRetry opens choice's credential and returns the POST-retry choice, so
// the caller records the final reason (open_failed on a fallback). It is the extracted
// open-plus-retry both the run lane and the judge lane now share (PRD #1140 M2): before
// this extraction assembleJudgeClaim opened directly with no retry, so an `auto` judge
// pick that would not decrypt failed the judge run terminally while the identical pick
// on the run lane floored onto another pooled token.
//
// 🔴 D14, reshaped by #754 M2. Without the retry arm, "auto never fails a run" is
// simply untrue. finishRunClaim maps errCredentialUnavailable to
// FailClaimAssemblyExact — a TERMINAL failure — so a token that passes the gauge gate
// and then will not decrypt (a rotated UZI_SECRET_KEY, a corrupt row, a token
// deleted between the ranking query and the open) kills a run another POOLED
// token could have completed.
//
// #754: the retry NEVER lands on workerSecretID(wkr)/nil/the non-pooled owner
// default. autoFloorRetry re-floors onto ANOTHER pooled token (excluding the
// pick that just failed, and still honouring the run's dead-secret exclude); when
// no other pooled token remains it returns errCredentialUnavailable and the run
// fails terminally rather than spending the default.
//
// Scoped as tightly as it can be, on three axes:
//   - only a credential the AUTO lane resolved (choice.autoLaneRetryable) — a
//     selector pick or a floor pick, never pinned/default/judge, which the user
//     named and whose failure is how they learn the token is broken;
//   - only errCredentialUnavailable. NOT errVaultLocked: that path already
//     requeues the run, which is transient and correct, and retrying it would
//     convert a wait into a spend on the wrong account;
//   - exactly ONCE, by STRUCTURE. autoFloorRetry records reason=open_failed, which
//     fails autoLaneRetryable on its REASON conjunct whatever the id turns out to
//     be — so a second open failure is terminal with no counter and no dependency
//     on an invariant enforced three files away.
func (s *Service) openWithAutoRetry(ctx context.Context, run store.Run, choice secretChoice) (claimCred, secretChoice, error) {
	cred, err := s.openAnthropic(ctx, run.UserID, choice.secretID)
	if err != nil {
		if !choice.autoLaneRetryable() || !errors.Is(err, errCredentialUnavailable) {
			return claimCred{}, secretChoice{}, err
		}
		// autoLaneRetryable guarantees a non-nil secretID; read it BEFORE overwriting.
		failedID := *choice.secretID
		choice, err = s.autoFloorRetry(ctx, run, failedID)
		if err != nil {
			return claimCred{}, secretChoice{}, err
		}
		cred, err = s.openAnthropic(ctx, run.UserID, choice.secretID)
		if err != nil {
			return claimCred{}, secretChoice{}, err
		}
	}
	return cred, choice, nil
}

// workerIdentity is the immutable, non-secret provenance label recorded on a custody
// hold and its captures (PRD #1296 M1, D1/D2). It must be reproducible by the SAME worker
// at a post-terminal recovery retry and is bound into the capture AAD, so it is derived
// from stable worker facts — the worker's name, falling back to its id when unnamed. It is
// NEVER the join token or token hash (those are secrets and rotate); it is only an
// identity label paired with the immutable original_worker_id the authorization enforces.
func workerIdentity(wkr store.Worker) string {
	if strings.TrimSpace(wkr.Name) != "" {
		return wkr.Name
	}
	return wkr.ID.String()
}

// resumePhaseFor derives the phase a resumed claim should RESTORE, from the persisted run row
// (PRD #1247 M5, D13). planApproved is the value assembleClaim already computes
// (run.AutoApprove || rc.HumanPlanApproved || run.PlanSource == planSourceSeeded). Ordered, and
// the order matters:
//
//  1. OpenQuestionID set  -> "awaiting_input". Checked FIRST and unconditionally: every other
//     transition clears a resolved open_question_id (the "NO SETTER MAY LEAVE A RESOLVED
//     open_question_id BEHIND" invariant, runtime.sql), so a non-null id can ONLY mean the run's
//     held state was awaiting_input — even when planApproved is independently true (a mid-run
//     clarification, PRD #88).
//  2. has plan && planApproved -> "implementing". The approved-plan resume the worker already
//     skips the planning turn for (preApproved). This bucket ALSO absorbs a released
//     awaiting_followup run: awaiting_followup is NOT distinguishable from implementing once a
//     run is released to queued (ReleaseCredentialSwitch records no source state and
//     open_followup_id is a monotone ratchet), so it collapses here by design. That is safe:
//     the worker's existing interactive-resume logic (Interactive + StopPending + follow-up
//     inputs, all still delivered on the claim) drives the idle-for-follow-up behaviour exactly
//     as it does today, with or without resume_phase.
//  3. has plan && !planApproved && SessionID set -> "awaiting_approval". A plan was submitted
//     (SetRunAwaitingApproval sets plan_md + plan_source='agent' + session_id and clears
//     auto_approve + open_question_id) and not yet approved; the session exists to resume, so
//     the worker restores the GATE with plan_md rather than re-planning. SessionID is required
//     because a gate restore needs a resumable session; SetRunAwaitingApproval always sets it,
//     so this is a safety guard, not a common exclusion.
//  4. else -> "". A fresh queued run (plan_md null, session_id null) falls through here, and the
//     worker plans normally.
func resumePhaseFor(run store.Run, planApproved bool) string {
	hasPlan := run.PlanMd.Valid && strings.TrimSpace(run.PlanMd.String) != ""
	switch {
	case run.OpenQuestionID.Valid && strings.TrimSpace(run.OpenQuestionID.String) != "":
		return "awaiting_input"
	case hasPlan && planApproved:
		return "implementing"
	case hasPlan && !planApproved && run.SessionID.Valid && strings.TrimSpace(run.SessionID.String) != "":
		return "awaiting_approval"
	default:
		return ""
	}
}

// assembleClaim builds the claim payload for an already-claimed run. It takes the
// CLAIMING worker, not just the run, because since PRD #104 M3 the credential a
// run spends can depend on which worker picked it up.
func (s *Service) assembleClaim(ctx context.Context, wkr store.Worker, run store.Run) (*ClaimPayload, error) {
	// Judge lane (PRD #46 Decision 1): a judge run has no repo and no forge
	// connection, so it MUST fork before GetRunClaimContext (which INNER-JOINs
	// repos → forge_connections and would treat a repo-less judge run as vanished)
	// and before the bot-PAT open. Its claim carries only the Anthropic token.
	if run.Kind == runkind.Judge {
		// PRD #1429 M3: the judge lane now threads the CLAIMING worker through too —
		// assembleJudgeClaim's Codex branch needs it (codexClaimSecrets is worker-scoped),
		// mirroring why THIS function itself takes wkr.
		return s.assembleJudgeClaim(ctx, wkr, run)
	}

	rc, err := s.q.GetRunClaimContext(ctx, run.ID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errRunVanished
		}
		return nil, fmt.Errorf("claim context: %w", err)
	}

	// #66 D1 layer 3, the claim backstop: this is the single ForgePAT-attach choke
	// point (the PAT is decrypted just below and shipped at ~1790), reached ONLY by
	// PAT-bearing runs — the judge lane forked above, and chat is claimed on a
	// separate lane (ClaimChat/assembleChatClaim; ClaimRun excludes kind<>'chat'), so
	// no extra kind-guard is needed. This is the security net that SUBSUMES layer 2:
	// a run queued while main was protected and claimed after protection was removed
	// is refused HERE rather than pushing. Placed before box.Open so a blocked run is
	// never decrypted. A nil guard skips (same nil-safety as layer 2; production wires
	// it via SetRepoGuard). Overridden comes from the live guardrail_override_reason
	// column GetRunClaimContext now carries (M8): a non-NULL reason means the admin
	// per-repo override is active, so GuardRepo downgrades the waivable findings
	// post-evaluation — never protection_unreadable (D8/D3), so a queued-then-claimed
	// run whose protection read errors is still refused even on an overridden repo.
	if s.guard != nil {
		res := s.guard.GuardRepo(ctx, privcheck.GuardInput{
			ForgeType:       rc.ForgeType,
			BaseURL:         rc.BaseUrl,
			TokenCiphertext: rc.TokenCiphertext,
			Repo: privcheck.Repo{
				ID:             uuid.UUID(run.RepoID.Bytes).String(),
				Path:           rc.RepoPath,
				ForgeProjectID: rc.ForgeProjectID,
				DefaultBranch:  rc.DefaultBranch.String,
			},
			// Live per-repo override (M8): NULL reason ⇒ no override.
			Overridden: rc.GuardrailOverrideReason.Valid,
		})
		if res.Blocked {
			return nil, fmt.Errorf("%w: %s", errGuardrailBlockedClaim, strings.Join(res.BlockMessages(), "; "))
		}
	}

	botPAT, err := s.box.Open(rc.TokenCiphertext)
	if err != nil {
		// box.Open errors carry no plaintext.
		return nil, fmt.Errorf("%w: bot PAT could not be decrypted", errCredentialUnavailable)
	}

	// PRD #1429 M2 (D4): claim assembly is HARNESS-AUTHORITATIVE — the Codex/Claude split is
	// decided HERE, BEFORE the Anthropic credential ladder, not after the payload is built. A
	// Codex run NEVER traverses the ladder: claimSecretID / openWithAutoRetry / recordRunCredential
	// are skipped, so it opens no Anthropic credential, writes no run_credential_epochs Anthropic
	// row, and carries no anthropic_oauth_token (anthropic stays nil → AnthropicOAuthToken == "").
	// Its Codex secrets are attached just below, in the harnessCodex block after the payload
	// literal. A Claude run keeps the exact existing ladder — byte-identical.
	var anthropic []byte
	recordedLastSeq := run.LastSeq
	if run.Harness != harnessCodex {
		// Which credential this claim spends: the claiming worker's binding for ordinary
		// runs, the owner's judge binding for self_improve, the owner's default when
		// neither names one (PRD #104 D1). Resolution is per-claim, which is what makes a
		// rebind take effect on the worker's next claim with no restart and no re-minted
		// join token — the token has never ridden the worker, only each claim response.
		choice, err := s.claimSecretID(ctx, wkr, run)
		if err != nil {
			return nil, err
		}
		// Open with the shared auto-lane open-failed retry (D14), which returns the
		// post-retry choice so the record below names the credential actually spent.
		cred, choice, err := s.openWithAutoRetry(ctx, run, choice)
		if err != nil {
			return nil, err
		}
		// Record it before anything else can fail (PRD #111 M1): the credential HAS been
		// opened at this point, so from the run's perspective it is already the account
		// this claim commits to, whether or not the rest of assembly succeeds.
		//
		// PRD #1247 M9 (task c): the RUN LANE passes emitSwitchMessage=true, so recordRunCredential
		// emits a 'credential_switch' feed message on an APPLIED token switch (an epoch delta onto a
		// different token) atomically with the credential write, and RETURNS the (possibly bumped)
		// last_seq — the seq of the server-inserted message, or the run's current high-water mark when
		// no message was emitted. ClaimPayload.LastSeq MUST be this returned value, not the stale
		// run.LastSeq snapshot: the worker resumes seq numbering from it, so a stale snapshot would let
		// it re-use the message's seq and collide.
		rls, err := s.recordRunCredential(ctx, run, cred, choice, true)
		if err != nil {
			return nil, err
		}
		recordedLastSeq = rls
		anthropic = cred.Token
	}

	// Only the templates allocated to this run's owner ride the claim (PRD #18
	// M7): builtin/global defaults ± the owner's overlay + the owner's own
	// allocated user templates. The reserved-name check (M6) guarantees at most
	// one lead-matching template can exist, so the payload can never carry two.
	templates, err := s.q.ListClaimAgentTemplates(ctx, pgconv.UUID(run.UserID))
	if err != nil {
		return nil, fmt.Errorf("list claim agent templates: %w", err)
	}

	// PRD #1551 M4 (D4): read the per-harness model lane matching the ALREADY-FROZEN run
	// harness, replacing the legacy shared default_model. A REVIEW run (kind='task' with
	// review_target_run_id set) reads NO Codex lane: a Codex task review uses the agent's
	// built-in gpt-6-sol, so a Codex review ships no default_model at all; a Claude review
	// still ships the Claude lane, keeping Claude reviews equivalent to today's shared slot.
	// Judge runs never reach here (they forked to assembleJudgeClaim above), and chat has its
	// own Claude-only lane read (chat.go). Empty lane ⇒ nil ⇒ the worker's own harness default.
	harness := Harness(run.Harness)
	isReviewRun := run.ReviewTargetRunID.Valid
	var defaultModel pgtype.Text
	if harness != HarnessCodex || !isReviewRun {
		lanes, lerr := s.q.GetUserHarnessModelDefaults(ctx, run.UserID)
		if lerr != nil {
			return nil, fmt.Errorf("harness model defaults lookup: %w", lerr)
		}
		if harness == HarnessCodex {
			defaultModel = lanes.DefaultCodexModel
		} else {
			defaultModel = lanes.DefaultClaudeModel
		}
		// PRD #300 / PRD #1551 D4: a schedule can freeze a per-run model onto the run
		// (runs.model). It wins over the owner's lane ONLY when it is compatible with the run
		// harness; delivered on the SAME default_model claim field, so the worker is unchanged
		// (Decision 7) and a subagent template's own model: pin still wins. An INCOMPATIBLE
		// frozen model records the existing visible fallback note and falls back to the owner's
		// lane for that harness (D4 as written), never the worker's bare harness default. Lane
		// values already carry their harness and NEVER traverse harnessModelCompatible — it now
		// screens only frozen run.model values.
		if run.Model.Valid {
			if harnessModelCompatible(harness, run.Model.String) {
				defaultModel = run.Model
			} else {
				recordedLastSeq = s.recordHarnessModelFallbackNote(ctx, run, recordedLastSeq, harness, run.Model.String)
			}
		}
	}

	// PRD #1551 M4 (D6): post-claim custom-Codex capability re-check. ClaimRun commits BEFORE
	// assembleClaim runs (service.go), so the owner's Codex lane could have flipped to a CUSTOM
	// (non-curated) id in the window between the claim's SQL gate and here. For an ordinary
	// (non-review) Codex run whose effective root is custom, refuse to ship it to a worker that
	// lacks codex_custom_model_v1 and return an error finishRunClaim REQUEUES — the run
	// goes back to queued for a capable worker rather than silently running Astra or failing. A
	// review run left defaultModel nil above and is exempt; judge/chat never reach here. A custom
	// id is exactly one harnessModelCompatible rejects for the Codex harness (non-empty,
	// non-curated), so the same closed vocabulary decides the claim gate and this re-check.
	if harness == HarnessCodex && !isReviewRun &&
		defaultModel.Valid && !harnessModelCompatible(HarnessCodex, defaultModel.String) &&
		!slices.Contains(wkr.ProtocolCapabilities, capability.CodexCustomModelV1) {
		return nil, errCustomModelCapabilityMissing
	}

	// The run owner's per-user default reasoning effort (PRD #617). NULL now
	// resolves to the uzi default `xhigh` at this sink (via resolveEffortPtr /
	// agenttmpl.ResolveDefaultEffort, issue #1157); an explicit choice is carried
	// verbatim. Unlike DefaultModel there is no per-schedule freeze — the owner's
	// per-user value is the only source.
	defaultEffort, err := s.q.GetUserDefaultEffort(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("default effort lookup: %w", err)
	}

	// The run owner's AI-attribution opt-out (issue #916), read live per claim so a
	// flipped toggle takes effect on the next claim/resume with no worker restart —
	// exactly like the effort/model reads above. NOT NULL column ⇒ always a definite bool.
	attributionEnabled, err := s.q.GetUserAttributionEnabled(ctx, run.UserID)
	if err != nil {
		return nil, fmt.Errorf("attribution lookup: %w", err)
	}

	// Skills (PRD #16): the per-run union of every skill allocated to any template
	// for this run's owner (shared ∪ overlay), after precedence + the per-run cap.
	// Re-assembled on every claim, including resume — a skill deleted between claim
	// and resume simply disappears from the resumed session (accepted; the worker
	// logs it). All skill content is user data, never a secret.
	skillRows, err := s.q.ListRunSkillAllocations(ctx, pgconv.UUID(run.UserID))
	if err != nil {
		return nil, fmt.Errorf("list run skill allocations: %w", err)
	}
	skills := assembleRunSkills(skillRows, s.p.SkillsMaxPerRun)

	// Tier-1 tool packages for the worker's provisioning engine (PRD #18 M4): the
	// owner's per-repo profile, re-validated against the current allowlist. A
	// rejected package fails the claim (errToolPackagesRejected → the run is failed
	// in Claim, not delivered). The tier-2 opt-in flag rides from the repos row.
	toolPackages, err := s.resolveTooling(ctx, run)
	if err != nil {
		return nil, err
	}

	// A ci_fix run carries no issue and instead delivers the failed-pipeline
	// snapshot; an issue run carries its issue iid and no pipeline (PRD #6).
	var issueIID *int64
	if run.IssueIid.Valid {
		v := run.IssueIid.Int64
		issueIID = &v
	}
	var pipeline *ClaimPipeline
	if run.Kind == runkind.CIFix {
		pipeline = claimPipelineFromSnapshot(run.FailureSnapshot)
	}

	// PRD #700 / issue #778: for an mr_rework run runs.branch is NULL (the run
	// carries no issue-run branch) and the MR's existing branch lives in
	// pipeline_ref, so source the claim's Branch from pipeline_ref there. The
	// worker still reads it off the already-wired Branch field; no new wire field.
	branch := run.Branch
	if run.Kind == runkind.MRRework {
		branch = run.PipelineRef
	}

	// PRD #122 M1: replay the FROZEN milestone list on every claim. A malformed column
	// degrades to nil-and-log rather than failing the claim, matching the repo_agents
	// decode on the DTO path — the column is data a prior write left, not an invariant
	// of this claim, and stranding a run over it would be worse than serving no list.
	milestones, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		slog.Error("workersvc: decode run milestones", "run_id", run.ID, "error", err)
		milestones = nil
	}

	// PRD #381: replay the structured issue-comments snapshot captured at run
	// creation. A malformed column degrades to nil-and-log rather than failing the
	// claim, matching the milestone decode above — the column is data a prior write
	// left, not an invariant of this claim.
	var issueComments *IssueCommentsSnapshot
	if len(run.IssueComments) > 0 {
		var snap IssueCommentsSnapshot
		if err := json.Unmarshal(run.IssueComments, &snap); err != nil {
			slog.Error("workersvc: decode run issue comments", "run_id", run.ID, "error", err)
		} else {
			issueComments = &snap
		}
	}

	// PRD #700 M2: replay the structured MR review-comments snapshot captured at
	// mr_rework run creation. A malformed column degrades to nil-and-log rather than
	// failing the claim, exactly like the issue-comments decode above.
	var reviewComments *ReviewCommentsSnapshot
	if len(run.ReviewComments) > 0 {
		var snap ReviewCommentsSnapshot
		if err := json.Unmarshal(run.ReviewComments, &snap); err != nil {
			slog.Error("workersvc: decode run review comments", "run_id", run.ID, "error", err)
		} else {
			reviewComments = &snap
		}
	}

	// Run-summary model resolution is user-value-wins (PRD #362 Decision 8), the same
	// shape as the judge model in assembleJudgeClaim but delivered on this ISSUE-run
	// claim: the run owner's per-user summary_model overrides the instance
	// summary_model; NULL/blank inherits the instance value. Guarded by s.settings !=
	// nil — a nil-settings deployment leaves it nil (the summary generator is advisory
	// and skips when no model rides). On a user-row read error we fall back to the
	// instance value best-effort with a log; we never send an empty model.
	var summaryModel *string
	if s.settings != nil {
		if um, uerr := s.q.GetUserSummaryModel(ctx, run.UserID); uerr != nil {
			slog.Warn("issue claim: read user summary model", "user", run.UserID.String(), "error", uerr)
		} else if um.Valid && strings.TrimSpace(um.String) != "" {
			m := um.String
			summaryModel = &m
		}
		if summaryModel == nil {
			if m, merr := s.settings.SummaryModel(ctx); merr == nil && strings.TrimSpace(m) != "" {
				summaryModel = &m
			}
		}
	}

	// PRD #517 M5: deliver the interactive-task park idle backstop ONLY on an interactive
	// task claim (the sole path that parks on awaitFollowUp). Left zero for every other run
	// so omitempty keeps its claim byte-identical to today's wire; the worker falls back to
	// its own TASK_FOLLOWUP_IDLE_MS constant when the field is absent. run.Interactive is
	// immutable from create, so a resumed interactive run re-delivers the same value.
	taskIdleTimeoutSeconds := 0
	if run.Interactive {
		taskIdleTimeoutSeconds = int(s.p.WorkerTaskIdleTimeout.Seconds())
	}

	// plan_approved is computed once here so both the PlanApproved field and the
	// resume_phase derivation (PRD #1247 M5, D13) read the SAME value — see the payload
	// literal for the full provenance of the three disjuncts.
	planApproved := run.AutoApprove || rc.HumanPlanApproved || run.PlanSource == planSourceSeeded
	// PRD #1247 M5, D13: the server-derived phase a resumed claim RESTORES instead of
	// re-entering the planning turn. Empty for a fresh run (omitted by omitempty).
	resumePhase := resumePhaseFor(run, planApproved)
	// The submitted plan's seq rides ONLY the awaiting_approval resume (D13: "awaiting_approval
	// with the submitted plan's seq"), so the query stays off the hot path for every other
	// claim. A query error degrades to seq 0 and NEVER fails the claim — the same degrade-and-log
	// posture as the milestone/issue-comments decodes above; the run still resumes its gate, the
	// worker just cannot correlate a buffered approve_plan to a specific plan revision.
	var resumePlanSeq int64
	if resumePhase == "awaiting_approval" {
		// Issue #1626 (B1): a gate resume re-presents the SUBMITTED plan, whose milestones are
		// still only the CANDIDATE (milestones_frozen is NULL until approval). Deliver that
		// candidate so the worker re-sends the same breakdown with the re-presented plan instead
		// of a milestone-less report. Never overrides a frozen list; a malformed candidate
		// degrades to none, like the frozen decode above (the server's planMilestonesParam
		// guard keeps the stored candidate either way).
		if len(milestones) == 0 {
			if cand, cerr := DecodeMilestones(run.MilestonesCandidate); cerr != nil {
				slog.Error("workersvc: decode run milestone candidate", "run_id", run.ID, "error", cerr)
			} else {
				milestones = cand
			}
		}
		if seq, err := s.q.LatestPlanSeqForRun(ctx, run.ID); err != nil {
			slog.Warn("workersvc: latest plan seq for resume", "run_id", run.ID, "error", err)
		} else {
			resumePlanSeq = seq
		}
	}

	payload := &ClaimPayload{
		RunID:            run.ID.String(),
		Kind:             run.Kind,
		IssueIID:         issueIID,
		IssueTitle:       run.IssueTitle,
		IssueDescription: run.IssueDescription,
		// PRD #381: the structured comments snapshot, decoded above. omitempty keeps a
		// comment-less run's claim byte-identical to today's.
		IssueComments: issueComments,
		// PRD #700 M2: the MR review-comments snapshot, decoded above. omitempty keeps
		// every non-mr_rework run's claim byte-identical to today's wire.
		ReviewComments: reviewComments,
		Status:         run.Status,
		Pipeline:       pipeline,
		Branch:         textPtr(branch),
		SessionID:      textPtr(run.SessionID),
		CheckpointTip:  textPtr(run.CheckpointTip),
		// PRD #1247 M9 (task c): the last_seq recordRunCredential actually landed — bumped past any
		// 'credential_switch' message it just inserted — NOT the stale run.LastSeq snapshot, so the
		// worker never re-uses the server-inserted message's seq. Equals run.LastSeq when no switch
		// message was emitted.
		LastSeq:        recordedLastSeq,
		IterationCount: run.IterationCount,
		RequeueCount:   run.RequeueCount,
		// PRD #1296 M1 (D2): the claim-lane counter the ClaimRun CTE just incremented, read
		// straight off the returned run row and returned in the claim payload.
		ClaimGeneration: run.ClaimGeneration,
		PlanMd:          textPtr(run.PlanMd),
		AutoApprove:     run.AutoApprove,
		// PRD #400 M2: task-run MR gate + source ref. open_mr is a plain bool (false
		// for every non-task run); base_branch is pgtype.Text (nil for a run that has
		// none). Both re-read from the row on every claim, like AutoApprove above.
		OpenMr:     run.OpenMr,
		BaseBranch: textPtr(run.BaseBranch),
		// PRD #517 M1: the interactive opt-in rides every claim (a plain bool, false for
		// every non-interactive and non-task run), re-read from the row like OpenMr above so
		// a resumed run re-delivers it unchanged. It tells the worker to keep the run alive
		// (park in awaiting_followup) after signal_done rather than terminating.
		Interactive: run.Interactive,
		// issue #552 M3: re-deliver the durable stop_kind='stopped' fact so a graceful
		// stop survives a worker crash. Derived from the loaded row, like OpenQuestionID
		// below and for the same reason — the in-memory stopRequested flag is lost on a
		// death, but the runs row keeps stop_kind='stopped', so the resumed worker winds
		// the park down instead of waiting out the idle timeout. Never set for a terminal
		// run (a finished run has nothing left to wind down).
		StopPending: run.Interactive && run.StopKind.Valid && run.StopKind.String == "stopped" && !terminalStatuses[run.Status],
		// PRD #1190 M1: re-deliver a pending pause so a worker that died with one set re-arms
		// it on resume. Derived from the loaded row like StopPending/OpenQuestionID; the
		// columns survive every requeue, so the re-claim carries the intent forward.
		PausePending: run.PauseRequestedAt.Valid,
		PauseMode:    run.PauseMode.String,
		// PRD #400 M4a: when set, this task run is a diff-review of that target task, and
		// the worker (M4b) routes on it. nil for a plain handoff and every non-task run.
		ReviewTargetRunID: uuidPtr(run.ReviewTargetRunID),
		OpenQuestionID:    textPtr(run.OpenQuestionID),
		// PRD #35. Re-read from the row on EVERY claim, like AutoApprove above: a
		// park-resume-park cycle must keep asking the row rather than remembering what
		// the first claim said, so a per-run toggle flipped mid-flight takes effect on
		// the next resume.
		WaitOnLimit: run.WaitOnLimit,
		// plan_approved is derived HERE, not by the worker (Decision 6b). Its two halves
		// are the human one — a consumed approve_plan input, projected by
		// GetRunClaimContext, whose comment carries the gate-bypass invariant this
		// relies on — and autopilot, which never had a gate to pass. A resumed run uses
		// it to skip the Phase-1 planning turn and replay plan_md; without it the resume
		// re-plans, re-parks at awaiting_approval in front of a human who already
		// approved, and can fail with REASON_NO_PLAN when the resumed session declines
		// to re-emit its plan.
		// PRD #209 D8 adds the THIRD disjunct: a seeded run has neither auto_approve
		// (D3 forbids overloading it) nor a consumed approve_plan (M1 asserts none
		// exists), so without this the claim would ship plan_approved:false and the
		// seeded-implement path (D4 row 2) would be unreachable — the feature inert.
		// The compare is a plain string because plan_source is NOT NULL DEFAULT 'agent'
		// (00095), deliberately so this reads `== planSourceSeeded` rather than a
		// pgtype.Text unwrap. Soundness note: this decouples plan_approved from plan_md's
		// provenance, which SetRunAwaitingApproval's plan_source='agent' write re-couples
		// (see GetRunClaimContext's invariant block and runtime.sql D8 comment).
		// PRD #71 M5: the run.AutoApprove disjunct is likewise decoupled from the plan
		// GATE by SetRunAwaitingApproval's symmetric auto_approve=false clear — parking a
		// forceGate ci_fix run for human review clears auto_approve so a restart-requeued
		// resume re-gates rather than shipping plan_approved=true past no human (runtime.sql).
		PlanApproved: planApproved,
		// PRD #1247 M5 (D13): the server-derived phase to RESTORE on a resume claim, so a run
		// re-claimed after a held-state credential switch (or any requeue) resumes its held
		// state — crucially, an UNAPPROVED submitted plan resumes the GATE — instead of
		// re-entering the planning turn. Derived from the run row by resumePhaseFor just above,
		// off the SAME planApproved this literal ships. Empty for a fresh run (omitted). The
		// submitted plan's seq rides ONLY the awaiting_approval phase (0/omitted otherwise).
		ResumePhase:   resumePhase,
		ResumePlanSeq: resumePlanSeq,
		// PlanSource travels to the worker so it can tell D4 row 2 (seeded, no session ⇒
		// implement) from row 3 (dropped session, not seeded ⇒ re-plan). Server writes
		// it in M1; the worker consumes it in M2. Additive on the wire — an old worker
		// ignores the key.
		PlanSource: run.PlanSource,
		// PRD #209 M4 staleness guard: the commit the seeded plan was written against and
		// whether a divergence should fail the run. Read from the runs row like PlanSource,
		// so both re-deliver unchanged on every resume. PlannedBaseCommit is nil for a run
		// that supplied no commit (the compare is then inert); RequireBaseMatch is a plain
		// bool (NOT NULL DEFAULT false), false for every non-opted-in run.
		PlannedBaseCommit: textPtr(run.PlannedBaseCommit),
		RequireBaseMatch:  run.RequireBaseMatch,
		// Ships with PlanApproved, deliberately adjacent: the two halves of one human
		// verdict, and propagating the approval without the exclusions is what silently
		// gives a user back a subagent they excluded. See ClaimPayload.AgentSelection.
		AgentSelection: persistedSelection(run),
		// PRD #122 M1: the frozen milestone list, decoded above. omitempty keeps a
		// milestone-less run's claim byte-identical to today's.
		Milestones: milestones,
		// PRD #362 Decision 8: the model the inline run-summary generator runs on,
		// resolved user-value-wins above. nil (omitted) when settings are unwired, so
		// an old worker's wire shape is unchanged.
		SummaryModel: summaryModel,
		// PRD #362 M3c (Decision 3): tell the worker whether the intent summary is
		// already set, so a resume/re-claim skips INTENT generation rather than
		// re-spending the owner's token. Derived straight off the run row.
		SummaryIntentPresent: run.SummaryIntent.Valid,
		Repo: ClaimRepo{
			ID:              uuid.UUID(run.RepoID.Bytes).String(),
			URL:             rc.RepoWebUrl,
			CloneURL:        rc.RepoWebUrl + ".git",
			DefaultBranch:   textPtr(rc.DefaultBranch),
			SkillsEnabled:   rc.RepoSkillsEnabled,
			ClaudemdEnabled: rc.RepoClaudemdEnabled,
			ForgeType:       rc.ForgeType,
		},
		Secrets: ClaimSecrets{
			ForgeUsername:       rc.BotUsername,
			ForgePAT:            string(botPAT),
			AnthropicOAuthToken: string(anthropic),
		},
		Agents:        agentsFromTemplates(templates, skills.perTemplate),
		Skills:        skills.union,
		SkillsDropped: skills.dropped,
		Config: ClaimConfig{
			// PRD #122 M2 (Decision 5/5b): serve the EFFECTIVE per-run budget from the
			// persisted columns, falling back to the global default when NULL (a 0/1-
			// milestone run, byte-for-byte today). The worker also reads the scaled wall
			// clock off the state-ack, but the claim carries it too for a fresh resume.
			// PRD #1189 M1 (D6, THE CRUX): add budget_extension_seconds so a run extended
			// while queued/parked arms the ALREADY-LARGER hard wall when it starts — without
			// this the worker trips REASON_WALL at the frozen budget before the server's
			// extended deadline, and the extend is a silent no-op. budget_extension_seconds is
			// NOT NULL DEFAULT 0, so this is byte-identical for a run that was never extended.
			// PRD #1497 M1: add budget_finalize_seconds as the THIRD term, so a run that resumed on
			// its Stop finalize allowance arms the larger wall too. NOT NULL DEFAULT 0, byte-identical
			// for a run that never used the allowance.
			RunTimeoutSeconds:      coalesceInt(run.BudgetWallSeconds, int(s.p.RunTimeout.Seconds())) + int(run.BudgetExtensionSeconds) + int(run.BudgetFinalizeSeconds),
			IdleTimeoutSeconds:     int(s.p.RunIdleTimeout.Seconds()),
			TaskIdleTimeoutSeconds: taskIdleTimeoutSeconds,
			MaxIterations:          coalesceInt(run.BudgetMaxIterations, s.p.RunMaxIterations),
			PlanMaxRevisions:       s.p.PlanMaxRevisions,
			QuestionMax:            s.p.QuestionMax,
			QuestionTimeoutSeconds: s.p.QuestionTimeoutSeconds,
			// PRD #1226 M5 (D6): the completion-hold owner-continue window (default 900s), so the
			// worker's completion-question timer uses it instead of question_timeout_seconds for a
			// completion-blocked run. Configured server-side, shipped like the question bounds above.
			CompletionHoldWindowSeconds: s.p.CompletionHoldWindowSeconds,
			DefaultModel:                textPtr(defaultModel),
			DefaultEffort:               resolveEffortPtr(defaultEffort),
			AttributionEnabled:          attributionEnabled,
			// PRD #305 M3: deliver the flag frozen onto the run at fire time (M1). Read
			// straight off the run row — not re-derived from the schedule. false for every
			// run that did not opt in, so omitempty keeps its claim byte-identical to today.
			OverrideSubagentModel: run.OverrideSubagentModel,
			SkillMaxBytes:         s.p.SkillMaxBytes,
			SkillsMaxPerRun:       s.p.SkillsMaxPerRun,
			ToolPackages:          toolPackages,
			RepoDevboxOptIn:       rc.RepoDevboxOptIn,
			// PRD #123 M1b: ship the Decision 6 denylist base names so the worker applies
			// the same credential-CLI policy to TIER-2 (repo devbox.json opt-in) packages,
			// which are shape-filtered server-side and so never denylist-checked there.
			// Compile-time constant list (no new DB read); always a non-nil slice.
			DeniedToolPackages: toolprofile.DenylistNames(),
			// PRD #71 M2: nil for non-ci_fix runs (column NULL) → omitted by omitempty.
			CIConfigPaths: run.CiConfigPaths,
			// PRD #1226 M3 (D1/D2): the completion-interlock discriminator, read straight off
			// runs.completion_contract_version. Non-nil ⇒ the worker runs the structural
			// completion protocol; nil (legacy run / rollout OFF) ⇒ omitted, legacy path. This
			// is WORKER-ONLY claim config, NOT the web RunDTO, so it touches no api-contract fixture.
			CompletionContractVersion: intPtr(run.CompletionContractVersion),
			// PRD #1226 M4 (D5): the frozen structural contract revision, read straight off
			// runs.contract_revision. The worker echoes it in its completion permit request; nil
			// (legacy / non-interlocked / contract not yet frozen) ⇒ omitted, same as above.
			ContractRevision: intPtr(run.ContractRevision),
			// PRD #1227 M2: the worker-only projection of the run's frozen contract's owner
			// decisions (deferred milestone ids+titles+reasons, accepted id+text+reason). nil for a
			// contract with neither a deferred nor an accepted set (legacy / non-interlocked /
			// revision-1), so omitempty keeps such a claim byte-identical to today's.
			CompletionScope: CompletionScopeClaim(run.CompletionContract),
		},
	}

	// PRD #1147 M2 (B7): a live Codex run also carries its Codex credential. It mints
	// the per-claim capability (worker-scoped) and opens the usable credential (subscription
	// access_token or static api_key) — never the refresh/login blob.
	//
	// PRD #1332 M5A (D3): the branch is HARNESS-AUTHORITATIVE — it runs whenever runs.harness is
	// 'codex', NOT when codex_secret_id happens to be non-null. This is the fail-closed fix for the
	// deleted-alias case: the alias FK nulls codex_secret_id while leaving harness='codex' (and the
	// deletion-proof codex_material_revision sentinel), so the OLD `run.CodexSecretID.Valid` guard
	// would skip the Codex path and assemble a CLAUDE claim carrying an Anthropic token for what is
	// still a Codex run. Now such a run enters this branch; codexClaimSecrets finds the binding
	// unavailable/incomplete and returns errCredentialUnavailable (a TERMINAL fail-closed), and the
	// run NEVER emits a Claude claim or falls back to an Anthropic token. A coherence violation
	// indicated by either M1 sentinel fails closed the same way. An ordinary Claude run
	// (harness='claude', no codex fields) skips this entirely and its claim JSON stays
	// byte-identical to today's (ClaimSecrets.Codex stays nil ⇒ no `codex` key).
	if run.Harness == harnessCodex {
		codex, err := s.codexClaimSecrets(ctx, wkr, run)
		if err != nil {
			// finishRunClaim (claim_recovery.go) settles every outcome in one exact-claim
			// transaction and matches these by IDENTITY, so pass them through untouched:
			// errVaultLocked (transient requeue), errRunVanished (drop) and
			// errCodexMintAmbiguous (no payload, no mutation: the mint write may have landed).
			// Every other Codex error is wrapped as errCredentialUnavailable with %w, so the
			// sentinel and any minted epoch/hash survive for that classifier: a quarantine or a
			// same-alias re-login in flight parks the run in recovery_wait, and the other
			// credential failures (kind/mode mismatch, login blob, revoked revision, identity
			// change, a store defect) stay terminal.
			if errors.Is(err, errVaultLocked) || errors.Is(err, errRunVanished) || errors.Is(err, errCodexMintAmbiguous) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: %w", errCredentialUnavailable, err)
		}
		payload.Secrets.Codex = codex
	}

	// issue #297: a self_improve run carries the in-flight avoid-set so the picker skips
	// a recommendation whose fix another active run is already doing. Best-effort and
	// self_improve-only; every other kind's claim stays byte-identical to today's.
	if run.Kind == runkind.SelfImprove {
		payload.InflightTargets = s.inflightTargets(ctx, run)
		// PRD #686 M10 (D11/D12): the repo's currently-OPEN self-improve MRs' "what was
		// proposed" text, so the picker chooses a non-overlapping improvement. Best-effort
		// and forge-sourced; empty ⇒ omitted so the wire stays byte-identical.
		payload.SelfImproveOpenMRs = s.selfImproveOpenMRs(ctx, run, rc)
		// PRD #686 M3: true only for a repo that opted into uzi dogfooding
		// (repos.fold_improve_uzi_backlog, read from the same GetRunClaimContextRow as
		// RepoDevboxOptIn above); false ⇒ the worker runs the generic directive (m4).
		payload.SelfImproveDogfood = rc.FoldImproveUziBacklog
	}

	return payload, nil
}

// inflightTargets builds the self_improve in-flight avoid-set at claim time (issue
// #297): every non-terminal run on the SAME repo (excluding this self_improve run
// itself), formatted as one compact coordinate line each. Best-effort — a query
// failure yields nil and never fails the claim (mirrors the knownTargets posture in
// assembleJudgeClaim).
//
// ListActiveRunsAll is a GLOBAL, all-repos LIMIT-500 window ordered by recency; the
// same-repo filter runs in Go over that window. On a very busy multi-tenant fleet a
// repo's in-flight runs could in principle be crowded out of the 500 newest rows and
// silently drop from the avoid-set. That is acceptable here: this set is ADVISORY
// context for the picker (D4), not a correctness gate — a missed entry only means the
// picker might overlap, which the human MR review still catches. Reusing the existing
// query is the deliberate trade for no new query and no migration (D5).
func (s *Service) inflightTargets(ctx context.Context, run store.Run) []string {
	rows, err := s.q.ListActiveRunsAll(ctx, s.activeRunsPriorityCutoff())
	if err != nil {
		slog.Warn("self_improve claim: list active runs for in-flight set", "run", run.ID.String(), "error", err)
		return nil
	}
	var out []string
	for _, row := range rows {
		r := row.Run
		if r.ID == run.ID || r.RepoID != run.RepoID {
			continue // exclude self and other repos
		}
		out = append(out, formatInflightLine(r))
		if len(out) >= maxInflightTargets {
			break
		}
	}
	return out
}

// formatInflightLine renders one active run as a single compact coordinate line for the
// self_improve in-flight avoid-set (issue #297). Shape:
//
//	issue #<iid> "<title>" (kind=<kind>, status=<status>) — milestones: <id> "<title>"; ...
//
// An issue-less kind (self_improve/ci_fix has a NULL issue_iid) drops the "#<iid>" and
// leads with "<kind> run" instead. The milestone tail is omitted when MilestonesFrozen is
// empty or fails to decode. The whole line is trimmed to maxInflightLineLen. All text is
// untrusted repo content — the worker renders it nonce-fenced, never as instructions.
func formatInflightLine(r store.Run) string {
	var b strings.Builder
	if r.IssueIid.Valid {
		fmt.Fprintf(&b, "issue #%d", r.IssueIid.Int64)
		if r.IssueTitle != "" {
			fmt.Fprintf(&b, " %q", r.IssueTitle)
		}
	} else {
		fmt.Fprintf(&b, "%s run", r.Kind)
	}
	fmt.Fprintf(&b, " (kind=%s, status=%s)", r.Kind, r.Status)

	// MilestonesFrozen is data a prior write left behind, not an invariant of this read:
	// a decode error just omits the milestone tail (best-effort, matching the claim path).
	if ms, err := DecodeMilestones(r.MilestonesFrozen); err == nil && len(ms) > 0 {
		b.WriteString(" — milestones:")
		for i, m := range ms {
			if i > 0 {
				b.WriteByte(';')
			}
			fmt.Fprintf(&b, " %s %q", m.ID, m.Title)
		}
	}

	line := b.String()
	if len(line) > maxInflightLineLen {
		// Trim back to a rune boundary so an untrusted multibyte title is never sliced
		// mid-rune into invalid UTF-8.
		cut := maxInflightLineLen
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut]
	}
	return line
}

// selfImproveOpenMRs builds the self_improve open-MR picker context at claim time (PRD
// #686 D11): the "what was proposed" text of the repo's currently-OPEN self-improve MRs,
// so the picker chooses a non-overlapping improvement. Best-effort throughout — any
// query/forge error yields nil (or skips the offending candidate) and NEVER fails the
// claim, mirroring inflightTargets' posture. Unlike m9's fire-time cap (which must be
// strict), this is advisory context, so a per-candidate GetMergeRequest error skips only
// that candidate and the loop continues.
//
// Open-state is resolved LIVE from the forge per candidate (D12): runs.mr_state is
// unreliable for this multi-MR-per-tracking-issue lane. The proposed text comes from the
// RUN ROW (plan_md if present, else issue_description) — never from the MR title/body:
// GetMergeRequest is used ONLY for the open-state check. Since RC1 (issue #1197) an
// autopilot run's approved plan IS persisted to plan_md (via SetRunAutopilotPlan on the
// running report), so autopilot self_improve rows can now carry a plan_md; the
// prefer-plan_md-else-issue_description order is unchanged and handles both.
func (s *Service) selfImproveOpenMRs(ctx context.Context, run store.Run, rc store.GetRunClaimContextRow) []string {
	if s.forges == nil {
		return nil
	}
	f, err := s.forges.ForgeForConnection(rc.ForgeType, rc.BaseUrl, rc.TokenCiphertext)
	if err != nil {
		slog.Warn("self_improve claim: build forge for open-MR set", "run", run.ID.String(), "error", err)
		return nil
	}
	rows, err := s.q.RecentSelfImproveMRRunsForRepo(ctx, store.RecentSelfImproveMRRunsForRepoParams{
		RepoID: uuid.UUID(run.RepoID.Bytes),
		Lim:    maxOpenSelfImproveMRCandidates,
	})
	if err != nil {
		slog.Warn("self_improve claim: list recent self-improve MR runs", "run", run.ID.String(), "error", err)
		return nil
	}
	var out []string
	for _, row := range rows {
		if row.ID == run.ID || !row.MrIid.Valid {
			continue // exclude self and any row without an MR iid
		}
		mr, err := f.GetMergeRequest(ctx, rc.ForgeProjectID, row.MrIid.Int64)
		if err != nil {
			// Best-effort: this is advisory context, not the strict fire-time cap, so a
			// per-candidate forge error skips only this candidate and the loop continues.
			slog.Warn("self_improve claim: check open-MR state", "run", run.ID.String(), "mr_iid", row.MrIid.Int64, "error", err)
			continue
		}
		if mr.State != forge.MRStateOpened {
			continue
		}
		proposed := row.IssueDescription
		if row.PlanMd.Valid && strings.TrimSpace(row.PlanMd.String) != "" {
			proposed = row.PlanMd.String
		}
		if line := firstNonEmptyLine(proposed); line != "" {
			out = append(out, line)
			if len(out) >= maxOpenSelfImproveMRs {
				break
			}
		}
	}
	return out
}

// firstNonEmptyLine returns the first non-blank line of s, trimmed and bounded to
// maxInflightLineLen on a rune boundary (the same untrusted-text bound the in-flight set
// uses). Empty when s has no non-blank line.
func firstNonEmptyLine(s string) string {
	for _, raw := range strings.Split(s, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if len(line) > maxInflightLineLen {
			cut := maxInflightLineLen
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = strings.TrimSpace(line[:cut])
		}
		return line
	}
	return ""
}

// resolveTooling resolves the run's TIER-1 tool packages for the claim payload
// (PRD #18 M4). The desired list is the run owner's per-(user,repo)
// repo_tool_profiles, RE-VALIDATED against the current tool_allowlist (it can
// shrink after the profile was saved — Technical §3). A rejected package fails the
// claim (Success Criteria 5), not silently drops. The tier-2 repo_devbox_opt_in
// flag rides separately (set from the repos row in assembleClaim); the worker does
// the tier-2 extraction after clone (PRD #18 M5).
func (s *Service) resolveTooling(ctx context.Context, run store.Run) (toolPackages []string, err error) {
	// assembleClaim only reaches resolveTooling for a run-lane (issue/ci_fix) run,
	// whose repo_id is always non-NULL (runs_kind_shape), so the conversion is safe.
	profile, err := s.q.GetRepoToolProfile(ctx, store.GetRepoToolProfileParams{UserID: run.UserID, RepoID: uuid.UUID(run.RepoID.Bytes)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []string{}, nil // no profile ⇒ no tier-1 provisioning
		}
		return nil, fmt.Errorf("get repo tool profile: %w", err)
	}
	desired := decodePackageList(profile.Packages)
	if len(desired) == 0 {
		return []string{}, nil
	}
	rules, err := s.loadToolRules(ctx)
	if err != nil {
		return nil, err
	}
	allowed, rejected := toolprofile.Resolve(desired, rules)
	if len(rejected) > 0 {
		return nil, fmt.Errorf("%w: %s", errToolPackagesRejected, strings.Join(rejected, ", "))
	}
	// PRD #123 M3 (Decision 4c): an allowlisted package that is not in the baked
	// worker toolchain cannot be provisioned behind the egress block, so fail the
	// claim here rather than let the run hang at 0 iterations. Wrap
	// errToolPackagesRejected so finishRunClaim's exact-claim terminal handling
	// applies (the run is failed with the offending names, never secret bytes).
	var unbaked []string
	for _, p := range allowed {
		if !toolseed.Covered(p) {
			unbaked = append(unbaked, p)
		}
	}
	if len(unbaked) > 0 {
		return nil, fmt.Errorf("%w: not in baked toolchain (image roll required): %s", errToolPackagesRejected, strings.Join(unbaked, ", "))
	}
	if allowed == nil {
		allowed = []string{} // always send an array, never null (wire contract)
	}
	return allowed, nil
}

// loadToolRules projects the DB tool_allowlist into the toolprofile.Rules map the
// pure resolver consumes, via the shared loader (identical to the write-time
// loader in the handler, so save and claim can never diverge).
func (s *Service) loadToolRules(ctx context.Context) (toolprofile.Rules, error) {
	rows, err := s.q.ListToolAllowlist(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tool allowlist: %w", err)
	}
	return toolprofile.RulesFromRows(rows), nil
}

// decodePackageList decodes a repo_tool_profiles.packages JSONB array into a slice.
// A NULL/empty/malformed column yields an empty list (never fails the claim on
// bad data — an out-of-band write can't wedge a run).
func decodePackageList(raw []byte) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		slog.Error("workersvc: decode repo tool profile packages", "error", err)
		return nil
	}
	return out
}
