package workersvc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type stopExecutionReader struct {
	*policyIdentityReader
	scope      store.RunUserInput
	inputs     map[int64]store.RunUserInput
	checks     map[uuid.UUID]store.CrossCheck
	pending    []store.RunUserInput
	fail       string
	err        error
	reads      []string
	inputReads []int64
	checkReads []uuid.UUID
}

func (q *stopExecutionReader) GetPolicyStopScopeInput(_ context.Context, p store.GetPolicyStopScopeInputParams) (store.RunUserInput, error) {
	q.reads = append(q.reads, "scope")
	if q.fail == "scope" {
		return store.RunUserInput{}, q.err
	}
	if q.scope.ID != p.InputID || q.scope.RunID != p.RunID {
		return store.RunUserInput{}, pgx.ErrNoRows
	}
	return q.scope, nil
}

func (q *stopExecutionReader) GetPolicyStopRevisionInput(_ context.Context, p store.GetPolicyStopRevisionInputParams) (store.RunUserInput, error) {
	q.inputReads = append(q.inputReads, p.InputID)
	if q.fail == "human" {
		return store.RunUserInput{}, q.err
	}
	row, ok := q.inputs[p.InputID]
	if !ok {
		return row, pgx.ErrNoRows
	}
	return row, nil
}

func (q *stopExecutionReader) ListPolicyStopRevisionInputs(_ context.Context, _ uuid.UUID) ([]store.RunUserInput, error) {
	q.reads = append(q.reads, "pending")
	if q.fail == "pending" {
		return nil, q.err
	}
	return q.pending, nil
}

func (q *stopExecutionReader) GetPolicyStopCrossCheck(_ context.Context, id uuid.UUID) (store.CrossCheck, error) {
	q.checkReads = append(q.checkReads, id)
	if q.fail == "checker" {
		return store.CrossCheck{}, q.err
	}
	row, ok := q.checks[id]
	if !ok {
		return row, pgx.ErrNoRows
	}
	return row, nil
}

type stopExecutionFixture struct {
	run    store.Run
	worker store.Worker
	record policyStopResolutionV1
	reader *stopExecutionReader
}

func (f *stopExecutionFixture) save(t *testing.T) {
	t.Helper()
	var err error
	f.record.IfMatch, err = policyStopManifestToken(f.record.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.run.PolicyStopResolution = mustStopJSON(t, f.record)
}

func stopExecutionFixtureFor(t *testing.T, profile string, noGate bool) *stopExecutionFixture {
	t.Helper()
	r, execution, identity := stopFrozenFixture(t, profile)
	w := worker()
	r.ID, r.UserID, r.WorkerID = uuid.New(), w.UserID, pgconv.UUID(w.ID)
	r.Harness, r.Kind, r.Status, r.Interactive = "codex", "issue", "claimed", false
	r.TargetRunID, r.ReviewTargetRunID, r.ThenFixOfRunID = pgtype.UUID{}, pgtype.UUID{}, pgtype.UUID{}
	r.ClaimGeneration, r.PolicyRefusalCount = 4, 1
	r.ClaimReleasedAt = pgtype.Timestamptz{}
	r.PolicyLastOutcomeID = pgconv.UUID(uuid.New())
	r.PolicyLastSourceGeneration = pgtype.Int8{Int64: 3, Valid: true}
	record := stopHistoryRecord(t)
	record.AcceptedBy = w.UserID
	record.Manifest.RunID, record.Manifest.OutcomeID = r.ID.String(), uuid.UUID(r.PolicyLastOutcomeID.Bytes).String()
	record.Manifest.Execution = execution
	record.Manifest.SourceGeneration, record.Manifest.ReviewGeneration = "3", "3"
	r.PlanSource, r.AutoApprove, r.PlanCrossCheckRequired = "agent", true, false
	q := &stopExecutionReader{policyIdentityReader: identity, inputs: map[int64]store.RunUserInput{}, checks: map[uuid.UUID]store.CrossCheck{}}
	q.scope = store.RunUserInput{ID: record.ScopeInputID, RunID: r.ID, Kind: "scope",
		CreatedAt: pgtype.Timestamptz{Time: record.AcceptedAt, Valid: true}}
	if noGate {
		r.GateRevision, r.GatePresentationID, r.GatePayloadDigest, r.GatePresentedPayload = 0, pgtype.UUID{}, nil, nil
		record.Manifest.Gate = nil
		record.Manifest.HumanFeedback = []apitypes.PolicyStopHumanFeedback{}
		record.Manifest.SourceContext = apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}}
	} else {
		r.GateRevision, r.GatePresentationID = 2, pgconv.UUID(uuid.New())
		r.PlanMd = pgtype.Text{String: "current unapproved B", Valid: true}
		r.MilestonesCandidate = []byte(`[{"id":"b1","title":"Unapproved B"}]`)
		payload := gatePayloadFromRow(r)
		if profile == "semantic" {
			b := stopFrozenDifferentCandidate(t, 2)
			r.PlanMd.String = "different unapproved B"
			r.MilestonesCandidate = []byte(`[{"id":"b1","title":"Different gate"}]`)
			payload = gatePayloadFromRow(r)
			p := semanticGateProjection(&semanticCoverageTx{candidate: b}, r)
			payload.Semantic = &p
			identity.history[b.Digest] = semanticJSON(t, b)
			identity.planning.Candidate = semanticJSON(t, stopFrozenDifferentCandidate(t, 3))
		}
		raw, digest, err := payload.digest()
		if err != nil {
			t.Fatal(err)
		}
		r.GatePayloadDigest = digest
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		fields["revision_retry_receipt"] = mustStopJSON(t, revisionRetryReceipt{SourceRevision: 1, RevisionInputID: 11, SupersededIDs: []int64{10}})
		r.GatePresentedPayload = mustStopJSON(t, fields)
		gate := apitypes.PolicyStopGateContext{Revision: "2", PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(), PayloadDigest: hex.EncodeToString(digest)}
		record.Manifest.SourceContext.Gate = &gate
		wp, err := policyStopExecutionGatePayload(payload)
		if err != nil {
			t.Fatal(err)
		}
		record.Manifest.Gate = &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: gate, Payload: wp}
		for _, h := range record.Manifest.HumanFeedback {
			id := int64(10)
			switch h.InputID {
			case "11":
				id = 11
			case "12":
				id = 12
			}
			row := store.RunUserInput{ID: id, RunID: r.ID, Kind: "revise_plan", Body: pgtype.Text{String: *h.Body, Valid: true},
				CreatedAt: pgtype.Timestamptz{Time: *h.CreatedAt, Valid: true}, GateBinding: pgtype.Text{String: *h.GateBinding, Valid: true},
				ConsumedAt: pgtype.Timestamptz{Time: *h.ConsumedAt, Valid: true}, ConsumedWorkerID: pgconv.UUID(uuid.MustParse(*h.ConsumedWorkerID)),
				ConsumedClaimGeneration: pgtype.Int8{Int64: 3, Valid: true}, GateRevision: pgtype.Int8{Int64: 1, Valid: true}}
			if h.AppliedAt != nil {
				row.AppliedAt = pgtype.Timestamptz{Time: *h.AppliedAt, Valid: true}
				if h.Disposition != nil {
					row.Disposition = pgtype.Text{String: *h.Disposition, Valid: true}
				}
			} else {
				row.GateRevision.Int64 = 2
				row.Disposition = pgtype.Text{String: "superseded_by_stop", Valid: true}
				row.SettledAt = pgtype.Timestamptz{Time: record.AcceptedAt, Valid: true}
				row.SupersededByStopInputID = pgtype.Int8{Int64: record.ScopeInputID, Valid: true}
			}
			q.inputs[id] = row
		}
	}
	r.PolicyRevisionSource = mustStopJSON(t, record.Manifest.SourceContext)
	f := &stopExecutionFixture{run: r, worker: w, record: record, reader: q}
	f.save(t)
	return f
}

func (f *stopExecutionFixture) addChecker(t *testing.T, selected bool, verdict string) uuid.UUID {
	t.Helper()
	r := f.run
	c := PlanCrossCheckCandidate{PlanMd: r.PlanMd.String, Milestones: r.MilestonesCandidate,
		RequiredCapabilities: r.RequiredCapabilities, RequiredTools: r.RequiredTools, SizeClass: r.SizeClass}
	if !selected {
		c.PlanMd = "independent older candidate"
	}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	now := pgtype.Timestamptz{Time: f.record.AcceptedAt.Add(-time.Minute), Valid: true}
	cc := store.CrossCheck{ID: uuid.New(), LeadRunID: r.ID, Stage: "plan", Round: 1, LeadClaimGeneration: 3,
		CandidateDigest: digest, PlanMd: pgtype.Text{String: c.PlanMd, Valid: true}, Milestones: c.Milestones,
		RequiredCapabilities: c.RequiredCapabilities, RequiredTools: c.RequiredTools, SizeClass: pgtype.Text{String: c.SizeClass, Valid: true},
		BaseCommit: pgtype.Text{Valid: true}, PlanningDiff: pgtype.Text{Valid: true}, Verdict: verdict,
		CreatedAt: now, DecidedAt: now, DeadlineAt: now}
	snapshot, err := policyStopExecutionCrossCheckSnapshot(cc)
	if err != nil {
		t.Fatal(err)
	}
	f.record.Manifest.CrossCheckFeedback = append(f.record.Manifest.CrossCheckFeedback, snapshot)
	id := cc.ID.String()
	f.record.Manifest.SourceContext.AssociatedCrossChecks = append(f.record.Manifest.SourceContext.AssociatedCrossChecks,
		apitypes.PolicyRevisionRef{Source: "plan_cross_check", CrossCheckID: &id})
	if selected {
		f.record.Manifest.CandidateCrossCheckID = &id
		f.run.PolicyCandidateCrossCheckID = pgconv.UUID(cc.ID)
	}
	if verdict == "revise" {
		cc.RevisionDisposition = pgtype.Text{String: "superseded_by_stop", Valid: true}
		cc.RevisionSettledAt = pgtype.Timestamptz{Time: f.record.AcceptedAt, Valid: true}
		cc.RevisionSupersededByStopInputID = pgtype.Int8{Int64: f.record.ScopeInputID, Valid: true}
	}
	f.reader.checks[cc.ID] = cc
	f.run.PolicyRevisionSource = mustStopJSON(t, f.record.Manifest.SourceContext)
	f.save(t)
	return cc.ID
}

func TestPolicyStopExecutionLocked(t *testing.T) {
	for _, profile := range []string{"legacy", "structural", "semantic"} {
		for _, noGate := range []bool{false, true} {
			t.Run(profile+map[bool]string{false: "_gate", true: "_no_gate"}[noGate], func(t *testing.T) {
				f := stopExecutionFixtureFor(t, profile, noGate)
				before := mustStopJSON(t, f.run)
				got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
				if err != nil || got == nil || got.record.ResolutionID != f.record.ResolutionID {
					t.Fatalf("accepted context=%v err=%v", got, err)
				}
				if !reflect.DeepEqual(before, mustStopJSON(t, f.run)) {
					t.Fatal("read-only validator changed run")
				}
				if !reflect.DeepEqual(f.reader.reads, []string{"scope", "pending"}) {
					t.Fatalf("inventory reads: %v", f.reader.reads)
				}
				if !noGate && !reflect.DeepEqual(f.reader.inputReads, []int64{10, 11, 12}) {
					t.Fatalf("exact human reads: %v", f.reader.inputReads)
				}
			})
		}
	}
}

func TestPolicyStopExecutionLockedRefusesDrift(t *testing.T) {
	mutations := map[string]func(*stopExecutionFixture){
		"unknown_source":     func(f *stopExecutionFixture) { f.run.PolicyRevisionSource = nil },
		"source_generation":  func(f *stopExecutionFixture) { f.run.PolicyLastSourceGeneration.Int64++ },
		"outcome":            func(f *stopExecutionFixture) { f.run.PolicyLastOutcomeID = pgconv.UUID(uuid.New()) },
		"selection":          func(f *stopExecutionFixture) { f.run.PolicyCandidateCrossCheckID = pgconv.UUID(uuid.New()) },
		"current_worker":     func(f *stopExecutionFixture) { f.worker.ID = uuid.New() },
		"current_user":       func(f *stopExecutionFixture) { f.worker.UserID = uuid.New() },
		"current_generation": func(f *stopExecutionFixture) { f.run.ClaimGeneration++ },
		"released":           func(f *stopExecutionFixture) { f.run.ClaimReleasedAt.Valid = true },
		"terminal":           func(f *stopExecutionFixture) { f.run.Status = "completed" },
		"queued":             func(f *stopExecutionFixture) { f.run.Status = "queued" },
		"child":              func(f *stopExecutionFixture) { f.run.TargetRunID = pgconv.UUID(uuid.New()) },
		"review_child":       func(f *stopExecutionFixture) { f.run.ReviewTargetRunID = pgconv.UUID(uuid.New()) },
		"fix_child":          func(f *stopExecutionFixture) { f.run.ThenFixOfRunID = pgconv.UUID(uuid.New()) },
		"ineligible_harness": func(f *stopExecutionFixture) { f.run.Harness = "claude" },
		"no_refusal":         func(f *stopExecutionFixture) { f.run.PolicyRefusalCount = 0 },
		"malformed_receipt":  func(f *stopExecutionFixture) { f.run.PolicyStopResolution = []byte("null") },
		"hash": func(f *stopExecutionFixture) {
			f.record.Manifest.Hold.Reason = strPtr("drift")
			f.run.PolicyStopResolution = mustStopJSON(t, f.record)
		},
		"gate_plan":  func(f *stopExecutionFixture) { f.run.PlanMd.String += " drift" },
		"gate_tuple": func(f *stopExecutionFixture) { f.run.GateRevision++ },
		"metadata_history": func(f *stopExecutionFixture) {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(f.run.GatePresentedPayload, &fields)
			fields["revision_retry_receipt"] = mustStopJSON(t, revisionRetryReceipt{SourceRevision: 1, RevisionInputID: 11})
			f.run.GatePresentedPayload = mustStopJSON(t, fields)
		},
		"saved_payload": func(f *stopExecutionFixture) { f.record.Manifest.Gate.Payload.PlanMd = strPtr("other"); f.save(t) },
		"frozen":        func(f *stopExecutionFixture) { f.run.MilestonesFrozen = []byte(`[{"id":"m1","title":"changed"}]`) },
		"ceiling":       func(f *stopExecutionFixture) { f.run.ScopeCeiling.Int32++ },
		"scope_missing": func(f *stopExecutionFixture) { f.reader.scope.ID++ },
		"scope_foreign": func(f *stopExecutionFixture) { f.reader.scope.RunID = uuid.New() },
		"scope_kind":    func(f *stopExecutionFixture) { f.reader.scope.Kind = "follow_up" },
		"scope_time": func(f *stopExecutionFixture) {
			f.reader.scope.CreatedAt.Time = f.reader.scope.CreatedAt.Time.Add(time.Second)
		},
		"scope_settled": func(f *stopExecutionFixture) {
			f.reader.scope.Disposition = pgtype.Text{String: "applied", Valid: true}
		},
		"scope_consumed": func(f *stopExecutionFixture) { f.reader.scope.ConsumedAt.Valid = true },
		"human_missing":  func(f *stopExecutionFixture) { delete(f.reader.inputs, 12) },
		"human_body": func(f *stopExecutionFixture) {
			row := f.reader.inputs[12]
			row.Body.String += " drift"
			f.reader.inputs[12] = row
		},
		"human_worker": func(f *stopExecutionFixture) {
			row := f.reader.inputs[12]
			row.ConsumedWorkerID = pgconv.UUID(uuid.New())
			f.reader.inputs[12] = row
		},
		"human_settlement": func(f *stopExecutionFixture) {
			row := f.reader.inputs[12]
			row.SupersededByStopInputID.Int64++
			f.reader.inputs[12] = row
		},
		"human_applied": func(f *stopExecutionFixture) {
			row := f.reader.inputs[12]
			row.AppliedAt.Valid = true
			f.reader.inputs[12] = row
		},
		"history_settled": func(f *stopExecutionFixture) {
			row := f.reader.inputs[11]
			row.SettledAt.Valid = true
			f.reader.inputs[11] = row
		},
		"new_pending": func(f *stopExecutionFixture) {
			f.reader.pending = []store.RunUserInput{{RunID: f.run.ID, Kind: "revise_plan"}}
		},
	}
	names := make([]string, 0, len(mutations))
	for name := range mutations {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		mutate := mutations[name]
		t.Run(name, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "structural", false)
			mutate(f)
			got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
			if got != nil || err == nil {
				t.Fatalf("invalid evidence accepted: got=%v err=%v", got, err)
			}
		})
	}
}

func TestPolicyStopExecutionLockedFiniteCheckers(t *testing.T) {
	for _, mode := range []string{"valid", "body", "findings", "provenance", "lifecycle", "interrupted", "interrupted_changed", "settlement", "approve_settled", "missing", "extra", "duplicate", "selected_B", "old_history"} {
		t.Run(mode, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", false)
			old := f.addChecker(t, false, "revise")
			selected := f.addChecker(t, true, "approve")
			cc := f.reader.checks[old]
			switch mode {
			case "body":
				cc.PlanMd.String += " changed"
			case "findings":
				cc.Findings = []byte(`[{"body":"changed"}]`)
			case "provenance":
				cc.CheckerModel = pgtype.Text{String: "changed", Valid: true}
			case "lifecycle":
				cc.FinalizedAt.Valid = true
			case "interrupted":
				cc.InterruptedAt = pgtype.Timestamptz{Time: f.record.AcceptedAt, Valid: true}
			case "interrupted_changed":
				savedTime := f.record.AcceptedAt.Add(-time.Second)
				f.record.Manifest.CrossCheckFeedback[0].InterruptedAt = &savedTime
				f.save(t)
				cc.InterruptedAt = pgtype.Timestamptz{Time: f.record.AcceptedAt, Valid: true}
			case "settlement":
				cc.RevisionSupersededByStopInputID.Int64++
			case "approve_settled":
				row := f.reader.checks[selected]
				row.RevisionDisposition = pgtype.Text{String: "superseded_by_stop", Valid: true}
				f.reader.checks[selected] = row
			case "extra":
				f.record.Manifest.CrossCheckFeedback = f.record.Manifest.CrossCheckFeedback[1:]
				f.save(t)
			case "duplicate":
				f.record.Manifest.CrossCheckFeedback[1] = f.record.Manifest.CrossCheckFeedback[0]
				f.save(t)
			case "selected_B":
				// Select historical A: its immutable identity is valid, but it
				// does not apply to the current B gate.
				id := old.String()
				f.run.PolicyCandidateCrossCheckID = pgconv.UUID(old)
				f.record.Manifest.CandidateCrossCheckID = &id
				f.save(t)
			case "old_history":
				cc.LeadClaimGeneration = 2
				f.record.Manifest.CrossCheckFeedback[0].CandidateGeneration = "2"
				f.save(t)
			}
			f.reader.checks[old] = cc
			if mode == "missing" {
				delete(f.reader.checks, old)
			}
			got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
			want := mode == "valid" || mode == "old_history" || mode == "interrupted"
			if (got != nil) != want || (err == nil) != want {
				t.Fatalf("accepted=%v want=%v err=%v", got != nil, want, err)
			}
			seen := map[uuid.UUID]bool{}
			for _, id := range f.reader.checkReads {
				if seen[id] {
					t.Fatalf("checker read twice: %s", id)
				}
				seen[id] = true
			}
		})
	}
}

func TestPolicyStopExecutionLockedSemanticFeedbackTargets(t *testing.T) {
	for _, mode := range []string{"selected_B_history_A", "selected_A", "missing_B_history", "missing_A_history", "history_error"} {
		t.Run(mode, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "semantic", false)
			historicalID := f.addChecker(t, false, "revise")
			selectedID := f.addChecker(t, true, "approve")
			contract, err := parseCompletionContract(f.run.CompletionContract)
			if err != nil {
				t.Fatal(err)
			}
			a := contract.SemanticPresentation.Candidate
			payload, ok, err := decodeGatePresentedPayload(f.run.GatePresentedPayload)
			if err != nil || !ok || payload.Semantic == nil {
				t.Fatalf("semantic B fixture: %v", err)
			}
			b := payload.Semantic.Candidate
			for _, id := range []uuid.UUID{historicalID, selectedID} {
				row := f.reader.checks[id]
				candidate := b
				if id == historicalID {
					candidate = a
					row.PlanMd.String = "implement behavior"
					row.Milestones = f.run.MilestonesFrozen
				}
				row.CandidateDigest, err = hex.DecodeString(candidate.Digest)
				if err != nil {
					t.Fatal(err)
				}
				f.reader.checks[id] = row
				row.RevisionDisposition.Valid, row.RevisionSettledAt.Valid, row.RevisionSupersededByStopInputID.Valid = false, false, false
				snapshot, err := policyStopExecutionCrossCheckSnapshot(row)
				if err != nil {
					t.Fatal(err)
				}
				for i := range f.record.Manifest.CrossCheckFeedback {
					if f.record.Manifest.CrossCheckFeedback[i].CrossCheckID == id.String() {
						f.record.Manifest.CrossCheckFeedback[i] = snapshot
					}
				}
			}
			switch mode {
			case "selected_A":
				id := historicalID.String()
				f.record.Manifest.CandidateCrossCheckID = &id
				f.run.PolicyCandidateCrossCheckID = pgconv.UUID(historicalID)
			case "missing_B_history":
				delete(f.reader.history, b.Digest)
			case "missing_A_history":
				delete(f.reader.history, a.Digest)
			case "history_error":
				f.reader.historyErr = errors.New("immutable history unavailable")
			}
			f.save(t)
			got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
			want := mode == "selected_B_history_A"
			if (got != nil) != want || (err == nil) != want {
				t.Fatalf("semantic feedback target accepted=%v want=%v err=%v", got != nil, want, err)
			}
			if mode == "history_error" && !errors.Is(err, f.reader.historyErr) {
				t.Fatalf("history error swallowed: %v", err)
			}
		})
	}
}

func TestPolicyStopExecutionLockedAbsenceAndErrors(t *testing.T) {
	f := stopExecutionFixtureFor(t, "legacy", false)
	f.run.PolicyStopResolution = nil
	got, err := policyStopExecutionLocked(context.Background(), nil, store.Worker{}, f.run, 0)
	if got != nil || err != nil {
		t.Fatalf("ordinary absence changed: %v %v", got, err)
	}
	for _, slot := range []string{"scope", "human", "pending", "checker", "identity"} {
		t.Run(slot, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", false)
			f.addChecker(t, true, "revise")
			sentinel := errors.New("storage unavailable")
			f.reader.fail, f.reader.err = slot, sentinel
			if slot == "identity" {
				f.reader.planningErr = sentinel
			}
			got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
			if got != nil || !errors.Is(err, sentinel) {
				t.Fatalf("storage failure lost: %v %v", got, err)
			}
		})
	}
}

func TestPolicyStopExecutionLockedHistoricalReceipt(t *testing.T) {
	f := stopExecutionFixtureFor(t, "legacy", false)
	// The saved acceptance is old, and review/source G=3 differs from current
	// reclaimed G=4. Neither age nor that difference invalidates consent.
	old := f.record.AcceptedAt
	if old.Year() != 2026 || old.Month() != time.October || old.Day() != 20 {
		t.Fatal("fixture lost its fixed historical acceptance")
	}
	for _, generation := range []int64{0, 3, 4} {
		got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, generation)
		if (got != nil) != (generation == 4) || (err == nil) != (generation == 4) {
			t.Fatalf("request G=%d accepted=%v err=%v", generation, got != nil, err)
		}
	}
	f.reader.scope.Disposition = pgtype.Text{String: "applied", Valid: true}
	got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
	if got != nil || err == nil {
		t.Fatal("settled historical scope granted live context")
	}
	if _, err := parsePolicyStopResolution(f.run.PolicyStopResolution, f.worker.UserID, f.run.ID); err != nil {
		t.Fatalf("immutable receipt retry acquired mutable validation: %v", err)
	}
	for _, reader := range []policyStopExecutionReader{nil, (*stopExecutionReader)(nil)} {
		got, err := policyStopExecutionLocked(context.Background(), reader, f.worker, f.run, 4)
		if got != nil || err == nil {
			t.Fatal("missing reader granted accepted context")
		}
	}
}

func TestPolicyStopExecutionLockedCanonicalEvidence(t *testing.T) {
	f := stopExecutionFixtureFor(t, "legacy", false)
	f.addChecker(t, false, "revise")
	f.addChecker(t, true, "approve")
	// Associated references are a set, frozen definitions remain ordered.
	source := f.record.Manifest.SourceContext
	source.AssociatedCrossChecks = append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	source.AssociatedCrossChecks[0], source.AssociatedCrossChecks[1] = source.AssociatedCrossChecks[1], source.AssociatedCrossChecks[0]
	f.run.PolicyRevisionSource = mustStopJSON(t, source)
	// Timestamp identity is an instant, not a serialized timezone spelling.
	for i := range f.record.Manifest.HumanFeedback {
		h := &f.record.Manifest.HumanFeedback[i]
		created := h.CreatedAt.In(time.FixedZone("fixture", 3600))
		h.CreatedAt = &created
	}
	f.save(t)
	got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
	if err != nil || got == nil {
		t.Fatalf("canonical evidence refused: %v", err)
	}
}

func TestPolicyStopExecutionLockedPendingApplicability(t *testing.T) {
	for _, mode := range []string{"older", "current", "future", "unbound", "zero", "no_gate"} {
		t.Run(mode, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", mode == "no_gate")
			row := store.RunUserInput{RunID: f.run.ID, Kind: "revise_plan",
				GateBinding: pgtype.Text{String: "bound", Valid: true}, GateRevision: pgtype.Int8{Int64: 1, Valid: true}}
			switch mode {
			case "current":
				row.GateRevision.Int64 = 2
			case "future":
				row.GateRevision.Int64 = 3
			case "unbound":
				row.GateBinding.String = "unbound"
			case "zero":
				row.GateRevision.Int64 = 0
			}
			f.reader.pending = []store.RunUserInput{row}
			got, err := policyStopExecutionLocked(context.Background(), f.reader, f.worker, f.run, 4)
			if (got != nil) != (mode == "older") || (err == nil) != (mode == "older") {
				t.Fatalf("pending applicability: got=%v err=%v", got, err)
			}
		})
	}
}
