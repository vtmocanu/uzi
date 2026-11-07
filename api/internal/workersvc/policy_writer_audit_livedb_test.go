package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Writer audit (issue #3607 M2a). Every ordinary writer is exercised against three episode shapes:
//
//	unresolved        an open episode (preserve_required, outcome set, resolved_at NULL): untouched
//	attempt-path done a resolved fresh_from_saved_work episode (preserve_required=false): ordinary
//	preserving done   a resolved same_conversation episode (preserve_required=true, resolved_at set):
//	                  ordinary, and the policy columns survive (terminal writers clear only the
//	                  required-recovery identity)
const (
	episodeUnresolved    = "unresolved"
	episodeAttemptDone   = "attempt-path resolved"
	episodePreservedDone = "preserving resolved"
	// episodeSavedWork is a resolved fresh_from_saved_work episode that still owes the saved-work
	// requirement: preserve_required=false, policy_required_head kept, verification pair cleared.
	episodeSavedWork = "saved-work head pending"
)

func (p policyLive) seedEpisode(t *testing.T, id uuid.UUID, shape string, gen int64) {
	t.Helper()
	head := hexHead("a")
	switch shape {
	case episodeUnresolved:
		p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='same_conversation',policy_applying_worker_id=$3,
			policy_applying_generation=$4,policy_source_generation=$4,policy_preserve_required=true,policy_required_head=$5,
			policy_hold_cause='refused' WHERE id=$1`, id, uuid.New(), p.wid, gen, head)
	case episodeAttemptDone:
		p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='fresh_from_saved_work',policy_applying_worker_id=$3,
			policy_applying_generation=$4,policy_source_generation=$4,policy_preserve_required=false,policy_resolved_at=now(),
			policy_hold_cause='attempt_path' WHERE id=$1`, id, uuid.New(), p.wid, gen)
	case episodePreservedDone:
		p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='same_conversation',policy_applying_worker_id=$3,
			policy_applying_generation=$4,policy_source_generation=$4,policy_preserve_required=true,policy_resolved_at=now(),
			policy_required_head=$5,policy_recovery_verified_generation=$4,policy_recovery_verified_head=$5,
			policy_hold_cause='refused' WHERE id=$1`, id, uuid.New(), p.wid, gen, head)
	case episodeSavedWork:
		p.e.exec(t, `UPDATE runs SET policy_outcome_id=$2,policy_continuation_mode='fresh_from_saved_work',policy_applying_worker_id=$3,
			policy_applying_generation=$4,policy_source_generation=$4,policy_preserve_required=false,policy_resolved_at=now(),
			policy_required_head=$5,policy_hold_cause='attempt_path' WHERE id=$1`, id, uuid.New(), p.wid, gen, head)
	default:
		t.Fatalf("unknown episode shape %q", shape)
	}
}

// policyColumns returns every policy_* column as one JSON object, optionally without the
// required-recovery identity keys.
func (p policyLive) policyColumns(t *testing.T, id uuid.UUID, withoutRecoveryIdentity bool) string {
	t.Helper()
	var s string
	err := p.e.pool.QueryRow(p.e.ctx, `SELECT COALESCE((SELECT jsonb_object_agg(k, v) FROM jsonb_each(to_jsonb(r)) AS e(k, v)
		WHERE k LIKE 'policy\_%' AND NOT ($2 AND k IN ('policy_required_head','policy_recovery_verified_generation','policy_recovery_verified_head'))),'{}'::jsonb)::text
		FROM runs r WHERE r.id=$1`, id, withoutRecoveryIdentity).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (p policyLive) wholeRow(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := p.e.pool.QueryRow(p.e.ctx, "SELECT to_jsonb(r)::text FROM runs r WHERE r.id=$1", id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

type auditWriter struct {
	name     string
	terminal bool
	// apply runs the writer as worker w and reports whether it changed the run.
	apply func(t *testing.T, p policyLive, id uuid.UUID, w uuid.UUID, gen int64) bool
}

func auditWriters() []auditWriter {
	zero := func(err error, n int64, t *testing.T) bool {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	return []auditWriter{
		{name: "SetRunAwaitingInput", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			n, err := p.e.q.SetRunAwaitingInput(p.e.ctx, store.SetRunAwaitingInputParams{OpenQuestionID: pgconv.Text("q-1"), ID: id, WorkerID: pgconv.UUID(w)})
			return zero(err, n, t)
		}},
		{name: "SetRunAwaitingFollowup", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			n, err := p.e.q.SetRunAwaitingFollowup(p.e.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: pgconv.UUID(w)})
			return zero(err, n, t)
		}},
		{name: "SetRunFailed", terminal: true, apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			n, err := p.e.q.SetRunFailed(p.e.ctx, store.SetRunFailedParams{FailureReason: pgconv.Text("x"), FailOrigin: pgconv.Text("agent_failure"), ID: id, WorkerID: pgconv.UUID(w), ClaimGeneration: pgconv.Int8Ptr(&gen)})
			return zero(err, n, t)
		}},
		{name: "SetRunFailedPlanRejected", terminal: true, apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			n, err := p.e.q.SetRunFailedPlanRejected(p.e.ctx, store.SetRunFailedPlanRejectedParams{FailureReason: pgconv.Text("x"), FailOrigin: pgconv.Text("agent_failure"), ID: id, WorkerID: pgconv.UUID(w), ClaimGeneration: pgconv.Int8Ptr(&gen)})
			return zero(err, n, t)
		}},
		{name: "SupersedeRunByWorker", terminal: true, apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			n, err := p.e.q.SupersedeRunByWorker(p.e.ctx, store.SupersedeRunByWorkerParams{StopReason: "moved", ID: id, WorkerID: pgconv.UUID(w)})
			return zero(err, n, t)
		}},
		{name: "RequeueRunsMissingFromSnapshot", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			now := time.Now()
			rows, err := p.e.q.RequeueRunsMissingFromSnapshot(p.e.ctx, store.RequeueRunsMissingFromSnapshotParams{
				WorkerID: pgconv.UUID(w), MissingCutoff: pgconv.Time(now.Add(time.Minute)), MaxRequeues: 5, Now: pgconv.Time(now), GlobalTimeoutSeconds: 7200,
			})
			return zero(err, int64(len(rows)), t)
		}},
		{name: "generic state report", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			worker, err := p.e.q.GetWorkerByID(p.e.ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			sid := "fresh-session"
			before := p.wholeRow(t, id)
			_, _, err = p.svc.SetState(p.e.ctx, worker, id, StateRequest{State: "running", ClaimGeneration: &gen, SessionID: &sid})
			if err != nil && !errors.Is(err, ErrRunNotOwned) && !errors.Is(err, ErrStaleClaim) {
				t.Fatal(err)
			}
			return before != p.wholeRow(t, id)
		}},
		{name: "iteration park", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			worker, err := p.e.q.GetWorkerByID(p.e.ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			p.e.exec(t, "UPDATE runs SET iteration_count=5 WHERE id=$1", id)
			_, applied, _, err := p.svc.ReportIterationPark(p.e.ctx, worker, id, IterationParkEvidence{ClaimGeneration: gen, IterationCount: 5, MaxIterations: 5})
			if err != nil && !errors.Is(err, ErrRunNotOwned) && !errors.Is(err, ErrStaleClaim) {
				t.Fatal(err)
			}
			return applied
		}},
		{name: "policy recovery verification", apply: func(t *testing.T, p policyLive, id, w uuid.UUID, gen int64) bool {
			worker, err := p.e.q.GetWorkerByID(p.e.ctx, w)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.svc.RecordPolicyRecoveryVerified(p.e.ctx, worker, id, PolicyRecoveryVerifiedRequest{ClaimGeneration: gen, RequiredHead: hexHead("a")})
			if err != nil {
				t.Fatal(err)
			}
			return got.Disposition == "recorded"
		}},
	}
}

func TestPolicyWriterAuditLiveDB(t *testing.T) {
	for _, w := range auditWriters() {
		t.Run(w.name, func(t *testing.T) {
			// (i) an unresolved episode refuses the owning worker and a foreign worker alike.
			t.Run(episodeUnresolved, func(t *testing.T) {
				p := newPolicyLive(t)
				foreign := p.e.seedWorker(t, p.caps)
				for label, worker := range map[string]uuid.UUID{"owning worker": p.wid, "foreign worker": foreign} {
					id := p.running(t, 3)
					p.seedEpisode(t, id, episodeUnresolved, 3)
					p.e.exec(t, "UPDATE runs SET iteration_count=5 WHERE id=$1", id) // iteration-park precondition
					before := p.wholeRow(t, id)
					applied := w.apply(t, p, id, worker, 3)
					// The verification route is a record, not an episode writer: it is exempt only from
					// the "owning worker" half (it is bound to the claim, not the episode).
					if w.name == "policy recovery verification" && label == "owning worker" {
						continue
					}
					if applied || before != p.wholeRow(t, id) {
						t.Fatalf("%s: writer changed an unresolved episode", label)
					}
				}
			})
			// (ii) a resolved attempt-path episode is ordinary again.
			t.Run(episodeAttemptDone, func(t *testing.T) {
				p := newPolicyLive(t)
				id := p.running(t, 3)
				p.seedEpisode(t, id, episodeAttemptDone, 3)
				policyBefore := p.policyColumns(t, id, w.terminal)
				applied := w.apply(t, p, id, p.wid, 3)
				if w.name == "policy recovery verification" {
					if applied {
						t.Fatal("verification recorded for a run that is not preserving")
					}
				} else if !applied {
					t.Fatal("ordinary writer was blocked by a resolved attempt-path episode")
				}
				if got := p.policyColumns(t, id, w.terminal); got != policyBefore {
					t.Fatalf("writer rewrote policy columns:\nbefore %s\nafter  %s", policyBefore, got)
				}
			})
			// (iii) a resolved preserving episode is ordinary and keeps its constraint.
			t.Run(episodePreservedDone, func(t *testing.T) {
				p := newPolicyLive(t)
				id := p.running(t, 3)
				p.seedEpisode(t, id, episodePreservedDone, 3)
				if w.name == "policy recovery verification" {
					p.e.exec(t, "UPDATE runs SET policy_recovery_verified_generation=NULL,policy_recovery_verified_head=NULL WHERE id=$1", id)
				}
				policyBefore := p.policyColumns(t, id, w.terminal)
				if !w.apply(t, p, id, p.wid, 3) {
					t.Fatal("ordinary writer was blocked by a resolved preserving episode")
				}
				row := p.run(t, id)
				if !row.PolicyPreserveRequired || !row.PolicyResolvedAt.Valid {
					t.Fatalf("constraint lost: %+v", row)
				}
				if got := p.policyColumns(t, id, w.terminal); got != policyBefore && w.name != "policy recovery verification" {
					t.Fatalf("writer rewrote policy columns:\nbefore %s\nafter  %s", policyBefore, got)
				}
				switch {
				case w.terminal:
					if row.PolicyRequiredHead.Valid || row.PolicyRecoveryVerifiedGeneration.Valid || row.PolicyRecoveryVerifiedHead.Valid {
						t.Fatalf("terminal writer kept the required-recovery identity: %+v", row)
					}
				case w.name == "policy recovery verification":
					if row.PolicyRequiredHead.String != hexHead("a") || row.PolicyRecoveryVerifiedGeneration.Int64 != 3 {
						t.Fatalf("verification must only add its record: %+v", row)
					}
				default:
					if row.PolicyRequiredHead.String != hexHead("a") || row.PolicyRecoveryVerifiedGeneration.Int64 != 3 || row.PolicyRecoveryVerifiedHead.String != hexHead("a") {
						t.Fatalf("non-terminal writer disturbed the required-recovery identity: %+v", row)
					}
				}
			})
			// (iv) a resolved fresh_from_saved_work episode that still owes its saved-work head: the head
			// survives every non-terminal writer, terminal writers clear it, and only the worker's
			// verification (current generation, owning worker) clears it without a terminal state.
			t.Run(episodeSavedWork, func(t *testing.T) {
				p := newPolicyLive(t)
				id := p.running(t, 3)
				p.seedEpisode(t, id, episodeSavedWork, 3)
				if !w.apply(t, p, id, p.wid, 3) {
					t.Fatal("writer was blocked by a saved-work requirement")
				}
				row := p.run(t, id)
				if row.PolicyPreserveRequired || row.PolicyRecoveryVerifiedGeneration.Valid || row.PolicyRecoveryVerifiedHead.Valid {
					t.Fatalf("writer disturbed the constraint or pair: %s", facts(row))
				}
				wantHead := hexHead("a")
				if w.terminal || w.name == "policy recovery verification" {
					wantHead = ""
				}
				if row.PolicyRequiredHead.String != wantHead {
					t.Fatalf("saved-work head after %s = %q, want %q", w.name, row.PolicyRequiredHead.String, wantHead)
				}
			})
		})
	}
}

// ClaimRun, ResumePausedRun and ResumePolicyHold read the episode from queued/paused rows.
func TestPolicyClaimAndResumeWritersAuditLiveDB(t *testing.T) {
	for _, shape := range []string{episodeUnresolved, episodeAttemptDone, episodePreservedDone, episodeSavedWork} {
		t.Run("ResumePausedRun/"+shape, func(t *testing.T) {
			p := newPolicyLive(t)
			id := p.running(t, 3)
			p.seedEpisode(t, id, shape, 3)
			p.e.exec(t, "UPDATE runs SET status='paused',status_since=now() WHERE id=$1", id)
			before, policyBefore := p.wholeRow(t, id), p.policyColumns(t, id, false)
			_, err := p.e.q.ResumePausedRun(p.e.ctx, store.ResumePausedRunParams{ID: id, UserID: p.e.userID, GlobalTimeoutSeconds: 3600})
			if shape == episodeUnresolved {
				if !errors.Is(err, pgx.ErrNoRows) || before != p.wholeRow(t, id) {
					t.Fatalf("unresolved episode resumed ordinarily: %v", err)
				}
				return
			}
			if err != nil || p.run(t, id).Status != "queued" {
				t.Fatalf("ordinary resume refused: %v", err)
			}
			if p.policyColumns(t, id, false) != policyBefore {
				t.Fatal("ResumePausedRun wrote a policy column")
			}
		})
		t.Run("ResumePolicyHold/"+shape, func(t *testing.T) {
			p := newPolicyLive(t)
			id := p.running(t, 3)
			p.seedEpisode(t, id, shape, 3)
			p.e.exec(t, "UPDATE runs SET status='paused',status_since=now(),hold_reason=$2 WHERE id=$1", id,
				map[bool]any{true: "codex_policy_hold", false: nil}[shape == episodeUnresolved])
			row := p.run(t, id)
			mode := PolicyResumePreserve
			// A stale request: right mode for the stored lane, wrong episode id.
			p.untouched(t, id, "stale id", func() {
				_, err := p.svc.ResumePolicyHold(p.e.ctx, p.e.userID, id, PolicyResumeRequest{Mode: mode, ExpectedEpisodeID: uuid.New()})
				if !errors.Is(err, ErrPolicyResumeConflict) {
					t.Fatalf("stale id: %v", err)
				}
			})
			if shape == episodeUnresolved {
				return
			}
			// The exact id of a RESOLVED episode (without its audit) is a safe conflict.
			p.untouched(t, id, "resolved episode without audit", func() {
				_, err := p.svc.ResumePolicyHold(p.e.ctx, p.e.userID, id, PolicyResumeRequest{Mode: mode, ExpectedEpisodeID: uuid.UUID(row.PolicyOutcomeID.Bytes)})
				if !errors.Is(err, ErrPolicyResumeConflict) {
					t.Fatalf("resolved episode: %v", err)
				}
			})
		})
		t.Run("ClaimRun/"+shape, func(t *testing.T) {
			p := newPolicyLive(t)
			id := p.running(t, 3)
			p.seedEpisode(t, id, shape, 3)
			p.e.exec(t, "UPDATE runs SET status='queued',status_since=now() WHERE id=$1", id)
			policyBefore := p.policyColumns(t, id, false)
			if shape != episodeAttemptDone {
				// (the saved-work shape is protected by policy_required_head, not by preserve_required)
				// A worker without the policy capability can never claim a constrained run.
				bare := []string{p.caps[1], p.caps[2]}
				bareWorker := p.e.seedWorker(t, bare)
				before := p.wholeRow(t, id)
				if _, err := p.e.q.ClaimRun(p.e.ctx, p.e.claimParams(bareWorker, bare, false)); !errors.Is(err, pgx.ErrNoRows) || before != p.wholeRow(t, id) {
					t.Fatalf("incapable worker claimed a constrained run: %v", err)
				}
			}
			if shape == episodeUnresolved {
				return // an open episode is never queued; only the capability gate is in scope here
			}
			run := p.reclaim(t, id)
			if p.policyColumns(t, id, false) != policyBefore {
				t.Fatal("ClaimRun wrote a policy column")
			}
			head := policyExpectedHead(run)
			if shape == episodePreservedDone && (head == nil || *head != hexHead("a") || !run.PolicyPreserveRequired) {
				t.Fatalf("preserving claim lost its constraint facts: %+v", run)
			}
			if shape == episodeAttemptDone && (head != nil || run.PolicyPreserveRequired || policySavedWorkHead(run) != nil) {
				t.Fatalf("attempt-path claim carries a constraint: %+v", run)
			}
			if shape == episodeSavedWork {
				if saved := policySavedWorkHead(run); head != nil || run.PolicyPreserveRequired || saved == nil || *saved != hexHead("a") {
					t.Fatalf("saved-work claim facts: %s", facts(run))
				}
			}
		})
	}
}
