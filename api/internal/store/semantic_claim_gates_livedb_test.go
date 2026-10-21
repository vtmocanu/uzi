package store_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1230: the two semantic claim clauses are hard gates in every claim, placement, health
// and spread copy. Semantic enrollment is seeded BEFORE the first claim, as the dormant
// production enrollment will be.

const semanticEnrollmentJSON = `{"profile":"semantic","policy":{"version":1,"model":"sonnet","max_reviews":3,"max_invocations":2,"max_tokens":120000,"max_seconds":900}}`

func enrollSemantic(fx *fleetFixture, run uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `INSERT INTO semantic_planning(run_id, enrollment) VALUES ($1, $2::jsonb)`, run, semanticEnrollmentJSON)
}

func claimWithProtocolCaps(fx *fleetFixture, w uuid.UUID, caps []string) (store.Run, error) {
	now := time.Now()
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{WorkerID: pgU(w), UserID: fx.userID,
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
		SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true},
		WorkerCaps:               []string{}, WorkerProtocolCaps: caps, CapabilityAware: false})
}

// A plan-stage checker child of a semantic-enrolled lead is claimable, countable as placement
// and exempt from provisioning triggers only for a worker advertising cross_check_semantic_plan_v1;
// the same child of a structural lead is unaffected by the capability.
func TestSemanticPlanCheckerClaimGateLiveDB(t *testing.T) {
	for _, round := range []int{1, 2} {
		for _, enrolled := range []bool{false, true} {
			for _, with := range []bool{false, true} {
				t.Run(fmt.Sprintf("round %d/enrolled=%v/capability=%v", round, enrolled, with), func(t *testing.T) {
					fx := newFleetFixture(t)
					optInEphemeral(fx)
					t.Cleanup(func() {
						mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
						mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID)
					})
					caps := claudeCheckerProtocols(true, round)
					if with {
						caps = append(caps, capability.CrossCheckSemanticPlanV1)
					}
					w := fx.worker("persistent", capOf(1), false)
					mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, caps)
					child, lead := seedClaudeChild(fx, fx.worker("lead-host", nil, false), round)
					if enrolled {
						enrollSemantic(fx, lead)
					}
					wantClaimable := !enrolled || with
					iv := leaseInterval(leaseTwoHours)
					want := int64(0)
					if wantClaimable {
						want = 1
					}
					if got := claimableCount(fx, child, iv); got != want {
						t.Fatalf("claimable=%d, want %d", got, want)
					}
					if gap := listedUnplaceable(fx, 1000, iv)[child]; gap != !wantClaimable {
						t.Fatalf("capability-gap trigger listed=%v, want %v", gap, !wantClaimable)
					}
					// A capable but full worker is saturation; an incapable one stays a gap.
					fx.holdActive(w, 1)
					if sat := listedSaturation(fx, 1000, iv)[child]; sat != wantClaimable {
						t.Fatalf("saturation trigger listed=%v, want %v", sat, wantClaimable)
					}
					// ClaimRun itself, on a worker with a free slot.
					mustExec(fx.ctx, t, fx.pool, `DELETE FROM runs WHERE worker_id=$1 AND status='running'`, w)
					got, err := claimWithProtocolCaps(fx, w, caps)
					if wantClaimable {
						if err != nil || got.ID != child {
							t.Fatalf("capable worker could not claim: run=%s err=%v", got.ID, err)
						}
						return
					}
					if !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("old checker claimed a semantic plan check: run=%s err=%v", got.ID, err)
					}
				})
			}
		}
	}
}

// The peer-deferral mirror inside ClaimRun applies the same checker clause.
func TestSemanticPlanCheckerPeerDeferralLiveDB(t *testing.T) {
	for _, peerCapable := range []bool{true, false} {
		t.Run(fmt.Sprintf("peer capable=%v", peerCapable), func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			a, b := fx.worker("busy", capOf(2), false), fx.worker("idle", capOf(2), false)
			aCaps := append(claudeCheckerProtocols(true, 1), capability.CrossCheckSemanticPlanV1)
			bCaps := claudeCheckerProtocols(true, 1)
			if peerCapable {
				bCaps = append(bCaps, capability.CrossCheckSemanticPlanV1)
			}
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", a, aCaps)
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", b, bCaps)
			child, lead := seedClaudeChild(fx, a, 1)
			enrollSemantic(fx, lead)
			got, err := claimWithProtocolCaps(fx, a, aCaps)
			if peerCapable {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("busy claimant must defer to a capable idle peer: run=%s err=%v", got.ID, err)
				}
				if got, err = claimWithProtocolCaps(fx, b, bCaps); err != nil || got.ID != child {
					t.Fatalf("capable idle peer must claim: run=%s err=%v", got.ID, err)
				}
				return
			}
			if err != nil || got.ID != child {
				t.Fatalf("incapable peer must not suppress the claim: run=%s err=%v", got.ID, err)
			}
		})
	}
}

// A semantic-enrolled lead run needs completion_semantic_publication_v1; completion_non_closing_v1
// alone, or the interlock alone, never satisfies it. A structural run stays claimable by the same
// older workers. Enrollment is seeded before the first claim.
func TestSemanticLeadClaimGateLiveDB(t *testing.T) {
	interlock := []string{"completion_interlock_v1"}
	for _, tc := range []struct {
		name      string
		caps      []string
		enrolled  bool
		claimable bool
	}{
		{"old worker, semantic lead", interlock, true, false},
		{"completion_non_closing_v1 only, semantic lead", append(append([]string{}, interlock...), "completion_non_closing_v1"), true, false},
		{"semantic checker capability only, semantic lead", append(append([]string{}, interlock...), capability.CrossCheckSemanticPlanV1), true, false},
		{"publication capability, semantic lead", append(append([]string{}, interlock...), capability.CompletionSemanticPublicationV1), true, true},
		{"old worker, structural lead", interlock, false, true},
		{"completion_non_closing_v1 only, structural lead", append(append([]string{}, interlock...), "completion_non_closing_v1"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			t.Cleanup(func() {
				mustExec(fx.ctx, t, fx.pool, `UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1`, fx.userID)
				mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID)
			})
			// The lead carries a docker requirement and the worker is docker-capable: the
			// provisioning triggers list only runs with a capability requirement, so this
			// is what lets them exercise the lead clause.
			w := fx.worker("persistent", capOf(1), true)
			mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, w, tc.caps)
			lead := fx.queuedRun()
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET completion_contract_version=1, required_capabilities='{docker}' WHERE id=$1`, lead)
			if tc.enrolled {
				enrollSemantic(fx, lead)
			}
			iv := leaseInterval(leaseTwoHours)
			want := int64(0)
			if tc.claimable {
				want = 1
			}
			if got := claimableCount(fx, lead, iv); got != want {
				t.Fatalf("claimable=%d, want %d", got, want)
			}
			if gap := listedUnplaceable(fx, 1000, iv)[lead]; gap != !tc.claimable {
				t.Fatalf("capability-gap trigger listed=%v, want %v", gap, !tc.claimable)
			}
			fx.holdActive(w, 1)
			if sat := listedSaturation(fx, 1000, iv)[lead]; sat != tc.claimable {
				t.Fatalf("saturation trigger listed=%v, want %v", sat, tc.claimable)
			}
			mustExec(fx.ctx, t, fx.pool, `DELETE FROM runs WHERE worker_id=$1 AND status='running'`, w)
			got, err := claimWithProtocolCaps(fx, w, tc.caps)
			if tc.claimable {
				if err != nil || got.ID != lead {
					t.Fatalf("worker could not claim: run=%s err=%v", got.ID, err)
				}
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("worker without completion_semantic_publication_v1 claimed a semantic lead: run=%s err=%v", got.ID, err)
			}
		})
	}
}

// The lead clause never gates a semantic lead's own cross-check child, and the peer-deferral
// mirror applies the lead clause to a plain semantic lead.
func TestSemanticLeadPeerDeferralLiveDB(t *testing.T) {
	for _, peerCapable := range []bool{true, false} {
		t.Run(fmt.Sprintf("peer capable=%v", peerCapable), func(t *testing.T) {
			fx := newFleetFixture(t)
			t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
			a, b := fx.worker("busy", capOf(2), false), fx.worker("idle", capOf(2), false)
			aCaps := []string{"completion_interlock_v1", capability.CompletionSemanticPublicationV1}
			bCaps := []string{"completion_interlock_v1"}
			if peerCapable {
				bCaps = aCaps
			}
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", a, aCaps)
			mustExec(fx.ctx, t, fx.pool, "UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", b, bCaps)
			fx.holdActive(a, 1)
			lead := fx.queuedRun()
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET completion_contract_version=1 WHERE id=$1`, lead)
			enrollSemantic(fx, lead)
			got, err := claimWithProtocolCaps(fx, a, aCaps)
			if peerCapable {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("busy claimant must defer to a capable idle peer: run=%s err=%v", got.ID, err)
				}
				if got, err = claimWithProtocolCaps(fx, b, bCaps); err != nil || got.ID != lead {
					t.Fatalf("capable idle peer must claim: run=%s err=%v", got.ID, err)
				}
				return
			}
			if err != nil || got.ID != lead {
				t.Fatalf("incapable peer must not suppress the claim: run=%s err=%v", got.ID, err)
			}
		})
	}
}

// GetRunSemanticHealth feeds the queued-reason rung: enrollment flags and the owner's online
// capable-worker counts, agreeing with the claim clauses.
func TestGetRunSemanticHealthLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, "DELETE FROM users WHERE id=$1", fx.userID) })
	plain := fx.worker("plain", capOf(1), false)
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, plain, claudeCheckerProtocols(true, 1))
	child, lead := seedClaudeChild(fx, fx.worker("lead-host", nil, false), 1)
	structuralLead := fx.queuedRun()
	read := func(run uuid.UUID) store.GetRunSemanticHealthRow {
		t.Helper()
		row, err := fx.q.GetRunSemanticHealth(fx.ctx, store.GetRunSemanticHealthParams{RunID: run, UserID: fx.userID})
		if err != nil {
			t.Fatal(err)
		}
		return row
	}
	if row := read(structuralLead); row.LeadEnrolled || row.PlanCheckerEnrolled || row.PlanCheckerCapable != 0 || row.PublicationCapable != 0 {
		t.Fatalf("structural run read as semantic: %+v", row)
	}
	enrollSemantic(fx, lead)
	if row := read(lead); !row.LeadEnrolled || row.PlanCheckerEnrolled || row.PublicationCapable != 0 {
		t.Fatalf("semantic lead: %+v", row)
	}
	if row := read(child); row.LeadEnrolled || !row.PlanCheckerEnrolled || row.PlanCheckerCapable != 0 {
		t.Fatalf("semantic checker before a capable worker: %+v", row)
	}
	capable := fx.worker("capable", capOf(1), false)
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, capable,
		append(claudeCheckerProtocols(true, 1), capability.CrossCheckSemanticPlanV1, capability.CompletionSemanticPublicationV1))
	if row := read(child); row.PlanCheckerCapable != 1 || row.PublicationCapable != 1 {
		t.Fatalf("capable worker not counted: %+v", row)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET draining_since=now() WHERE id=$1`, capable)
	if row := read(child); row.PlanCheckerCapable != 0 || row.PublicationCapable != 0 {
		t.Fatalf("draining worker counted: %+v", row)
	}
}
