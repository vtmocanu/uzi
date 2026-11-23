package workersvc

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexOwnerRequestDB accepts one generated owner lookup and rejects all other
// database calls. It checks query wiring, not PostgreSQL owner isolation.
type codexOwnerRequestDB struct {
	t        *testing.T
	observed store.Run
	reads    int
}

func (db *codexOwnerRequestDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	db.t.Fatal("unexpected Exec during owner request refusal")
	return pgconn.CommandTag{}, errors.New("unexpected Exec")
}

func (db *codexOwnerRequestDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	db.t.Fatal("unexpected Query during owner request refusal")
	return nil, errors.New("unexpected Query")
}

func (db *codexOwnerRequestDB) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	db.reads++
	if db.reads != 1 {
		db.t.Fatal("additional QueryRow during owner request refusal")
	}
	if !strings.HasPrefix(query, "-- name: GetRunByIDForUser :one\n") ||
		!strings.HasSuffix(strings.TrimSpace(query), "FROM runs WHERE id = $1 AND user_id = $2") {
		db.t.Fatal("expected generated owner lookup name and predicate")
	}
	if len(args) != 2 {
		db.t.Fatal("owner lookup must have exactly two arguments")
	}
	runID, runOK := args[0].(uuid.UUID)
	ownerID, ownerOK := args[1].(uuid.UUID)
	if !runOK || !ownerOK || runID == uuid.Nil || ownerID == uuid.Nil ||
		runID != db.observed.ID || ownerID != db.observed.UserID {
		db.t.Fatal("owner lookup must receive ordered nonzero run and owner UUIDs")
	}
	return crossCheckBoundaryRow{value: db.observed}
}

// These public-method cases cover Codex routing and early refusal ordering.
// They do not exercise transactions, credential material, or pending writes.
func TestSetRunCredentialCodexOwnerRequestRefusals(t *testing.T) {
	type refusal struct {
		name     string
		status   string
		mode     string
		secretID *uuid.UUID
		want     error
	}
	var cases []refusal
	secretID := uuid.New()
	for _, status := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		cases = append(cases, refusal{
			name: status + "/pinned_without_secret", status: status,
			mode: CredentialOverrideModePinned, want: ErrCredentialOverridePinnedNeedsSecret,
		})
		for _, mode := range []string{CredentialOverrideModeAuto, CredentialOverrideModeDefault, CredentialOverrideModeInherit} {
			cases = append(cases, refusal{
				name: status + "/" + mode + "_with_secret", status: status,
				mode: mode, secretID: &secretID, want: ErrCredentialOverridePinnedNeedsSecret,
			})
		}
		cases = append(cases, refusal{
			name: status + "/pinned_without_worker", status: status,
			mode: CredentialOverrideModePinned, secretID: &secretID, want: ErrCredentialSwitchNoLiveWorker,
		})
	}
	cases = append(cases, refusal{
		name: "claimed", status: "claimed", mode: CredentialOverrideModePinned,
		want: ErrCredentialSwitchClaimAssembling,
	})
	for _, status := range []string{"queued", "paused", "limit_wait", "pool_wait", "recovery_wait"} {
		cases = append(cases, refusal{
			name: status, status: status, mode: CredentialOverrideModePinned,
			want: ErrCodexSwitchIneligible,
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, ownerID := uuid.New(), uuid.New()
			db := &codexOwnerRequestDB{
				t: t,
				observed: store.Run{
					ID: runID, UserID: ownerID, Harness: harnessCodex,
					Kind: "issue", Status: tc.status,
				},
			}
			svc := Service{q: store.New(db)}
			got, err := svc.SetRunCredential(context.Background(), ownerID, runID, tc.mode, tc.secretID)
			if !errors.Is(err, tc.want) {
				t.Errorf("got error %v, want %v", err, tc.want)
			}
			if !reflect.ValueOf(got).IsZero() {
				t.Error("refusal returned a nonzero result")
			}
			if db.reads != 1 {
				t.Errorf("owner lookup count = %d, want 1", db.reads)
			}
		})
	}
}

func TestCodexSwitchOwnerStateRefusals(t *testing.T) {
	var svc Service
	for _, status := range []string{"queued", "paused", "limit_wait", "pool_wait", "recovery_wait", "completed", "failed", "cancelled", "claimed"} {
		_, err := svc.requestCodexCredentialSwitch(context.Background(), store.Run{Status: status}, BindModePinned, nil)
		want := ErrCodexSwitchIneligible
		if status == "claimed" {
			want = ErrCredentialSwitchClaimAssembling
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: got %v, want %v", status, err, want)
		}
	}
}

func TestCodexSwitchNegotiation(t *testing.T) {
	w := store.Worker{
		SnapshotRegisterNonce:      pgtype.Text{String: "registration", Valid: true},
		CredentialSwitchApplyNonce: pgtype.Text{String: "registration", Valid: true},
		ProtocolCapabilities:       []string{capability.CredentialSwitchInPlaceV1},
	}
	if codexSwitchNegotiated(w) {
		t.Fatal("Claude negotiation authorized Codex")
	}
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.CodexCredentialSwitchV1)
	if !codexSwitchNegotiated(w) {
		t.Fatal("explicit Codex negotiation refused")
	}
	w.CredentialSwitchApplyNonce.String = "older-registration"
	if codexSwitchNegotiated(w) {
		t.Fatal("older API registration accepted for Codex switching")
	}
	w.CredentialSwitchApplyNonce.String = "registration"
	w.SnapshotRegisterNonce.Valid = false
	if codexSwitchNegotiated(w) {
		t.Fatal("missing registration accepted")
	}
}

func TestCodexSwitchHistoricalBaseline(t *testing.T) {
	id := uuid.New()
	r := store.Run{
		ClaimGeneration:       7,
		CodexSecretID:         pgtype.UUID{Bytes: id, Valid: true},
		CodexAuthMode:         pgtype.Text{String: codexAuthModeAPIKey, Valid: true},
		CodexMaterialRevision: pgtype.Int8{Int64: 3, Valid: true},
	}
	b := store.RunCredentialEpoch{ClaimGeneration: 7, SecretID: r.CodexSecretID}
	if !codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("nullable historical snapshots rejected")
	}
	b.CodexMaterialRevision = pgtype.Int8{Int64: 4, Valid: true}
	if codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("recorded revision mismatch accepted")
	}
	b.CodexMaterialRevision.Valid = false
	b.CodexAuthMode = pgtype.Text{String: codexAuthModeSubscription, Valid: true}
	if codexSwitchBaselineMatches(r, []store.RunCredentialEpoch{b}, nil) {
		t.Fatal("recorded mode mismatch accepted")
	}
	if codexSwitchBaselineMatches(r, nil, nil) {
		t.Fatal("missing ordinal zero accepted")
	}
}

// TestCodexSwitchBaselineActiveBinding covers binding invariants without a database.
func TestCodexSwitchBaselineActiveBinding(t *testing.T) {
	type fixture struct {
		run      store.Run
		bases    []store.RunCredentialEpoch
		children []store.RunCredentialSwitchEpoch
	}
	newFixture := func() fixture {
		a := store.RunCredentialEpoch{
			ClaimGeneration:       7,
			SecretID:              pgtype.UUID{Bytes: uuid.New(), Valid: true},
			CodexAuthMode:         pgtype.Text{String: codexAuthModeAPIKey, Valid: true},
			CodexMaterialRevision: pgtype.Int8{Int64: 3, Valid: true},
		}
		b := store.RunCredentialSwitchEpoch{
			ClaimGeneration:       7,
			Ordinal:               1,
			SecretID:              pgtype.UUID{Bytes: uuid.New(), Valid: true},
			CodexAuthMode:         pgtype.Text{String: codexAuthModeSubscription, Valid: true},
			CodexMaterialRevision: pgtype.Int8{Int64: 4, Valid: true},
			CodexAccountKey:       pgtype.Text{String: `["provider","account-b"]`, Valid: true},
			CodexClaimEpoch:       pgtype.Int8{Int64: 80, Valid: true},
			CodexAccountID:        pgtype.Text{String: "account-b", Valid: true},
		}
		c := b
		c.Ordinal = 2
		c.SecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		c.CodexMaterialRevision.Int64 = 5
		c.CodexAccountKey.String = `["provider","account-c"]`
		c.CodexClaimEpoch.Int64 = 42
		c.CodexAccountID.String = "account-c"
		return fixture{
			run: store.Run{
				ClaimGeneration:       7,
				CodexSecretID:         c.SecretID,
				CodexAuthMode:         c.CodexAuthMode,
				CodexMaterialRevision: c.CodexMaterialRevision,
				CodexAccountKey:       c.CodexAccountKey,
				CodexClaimEpoch:       42,
			},
			bases:    []store.RunCredentialEpoch{a},
			children: []store.RunCredentialSwitchEpoch{b, c},
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*fixture)
		want   bool
	}{
		{"current_c_after_baseline_a_and_b_epoch_42_ordinal_2", func(*fixture) {}, true},
		{"current_b_after_baseline_a", func(f *fixture) {
			b := f.children[0]
			f.children = f.children[:1]
			f.run.CodexSecretID = b.SecretID
			f.run.CodexMaterialRevision = b.CodexMaterialRevision
			f.run.CodexAccountKey = b.CodexAccountKey
			f.run.CodexClaimEpoch = 80
		}, true},
		{"older_child_cannot_authorize_current_binding", func(f *fixture) {
			b := f.children[0]
			f.run.CodexSecretID = b.SecretID
			f.run.CodexMaterialRevision = b.CodexMaterialRevision
			f.run.CodexAccountKey = b.CodexAccountKey
			f.run.CodexClaimEpoch = 80
		}, false},
		{"baseline_required_with_children", func(f *fixture) {
			f.bases = nil
		}, false},
		{"other_generation_baseline_is_insufficient", func(f *fixture) {
			f.bases[0].ClaimGeneration = 6
		}, false},
		{"ignores_other_generation_bases_and_children", func(f *fixture) {
			otherBase := f.bases[0]
			otherBase.ClaimGeneration = 6
			otherBase.SecretID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			f.bases = append([]store.RunCredentialEpoch{otherBase}, f.bases...)
			otherChild := f.children[0]
			otherChild.ClaimGeneration = 6
			otherChild.Ordinal = 99
			f.children = append([]store.RunCredentialSwitchEpoch{otherChild}, f.children...)
			otherChild.ClaimGeneration = 8
			f.children = append(f.children, otherChild)
		}, true},
		{"highest_ordinal_first", func(f *fixture) {
			f.children[0], f.children[1] = f.children[1], f.children[0]
		}, true},
		{"no_children_uses_baseline", func(f *fixture) {
			a := f.bases[0]
			f.children = nil
			f.run.CodexSecretID = a.SecretID
			f.run.CodexAuthMode = a.CodexAuthMode
			f.run.CodexMaterialRevision = a.CodexMaterialRevision
			f.run.CodexAccountKey = pgtype.Text{}
			f.run.CodexClaimEpoch = 0
		}, true},
		{"no_children_rejects_nonbaseline_binding", func(f *fixture) {
			f.children = nil
		}, false},
		{"child_secret_mismatch", func(f *fixture) {
			f.children[1].SecretID = f.bases[0].SecretID
		}, false},
		{"child_mode_mismatch", func(f *fixture) {
			f.children[1].CodexAuthMode.String = codexAuthModeAPIKey
		}, false},
		{"child_material_mismatch", func(f *fixture) {
			f.children[1].CodexMaterialRevision.Int64++
		}, false},
		{"child_account_key_mismatch", func(f *fixture) {
			f.children[1].CodexAccountKey.String = `["other-provider","account-c"]`
		}, false},
		{"child_claim_epoch_mismatch", func(f *fixture) {
			f.children[1].CodexClaimEpoch.Int64 = 2
		}, false},
		{"child_account_id_tuple_mismatch", func(f *fixture) {
			f.children[1].CodexAccountID.String = "other-account"
		}, false},
		{"child_account_id_requires_tuple", func(f *fixture) {
			f.run.CodexAccountKey.Valid = false
			f.children[1].CodexAccountKey.Valid = false
		}, false},
		{"child_account_id_rejects_malformed_tuple", func(f *fixture) {
			f.run.CodexAccountKey.String = "invalid-json"
			f.children[1].CodexAccountKey = f.run.CodexAccountKey
		}, false},
		{"child_account_id_rejects_short_tuple", func(f *fixture) {
			f.run.CodexAccountKey.String = `["account-c"]`
			f.children[1].CodexAccountKey = f.run.CodexAccountKey
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.change(&f)
			if got := codexSwitchBaselineMatches(f.run, f.bases, f.children); got != tc.want {
				t.Fatalf("baseline matches = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexSwitchTargetShapes(t *testing.T) {
	for _, tc := range []struct {
		kind, status string
		linked, want bool
	}{
		{store.KindOpenAIAPIKey, "static", false, true},
		{store.KindCodexAuth, "linked", true, true},
		{store.KindOpenAIAPIKey, "static", true, false},
		{store.KindCodexAuth, "linked", false, false},
		{store.KindAnthropicToken, "static", false, false},
	} {
		if got := codexSwitchTargetUsable(tc.kind, tc.status, tc.linked); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func TestCodexSwitchPolicyFence(t *testing.T) {
	for _, r := range []store.Run{
		{HoldReason: pgtype.Text{String: "codex_policy_hold", Valid: true}},
		{PolicyOutcomeID: pgtype.UUID{Bytes: uuid.New(), Valid: true}},
		{PolicyPendingFresh: true},
	} {
		if !codexSwitchPolicyBlocked(r) {
			t.Fatal("policy fence accepted blocked episode")
		}
	}
	if codexSwitchPolicyBlocked(store.Run{PolicyPreserveRequired: true, PolicyRefusalCount: 1}) {
		t.Fatal("historical resolved policy constraint treated as an unresolved episode")
	}
	if codexSwitchPolicyBlocked(store.Run{}) {
		t.Fatal("empty episode blocked")
	}
}

// This checks lane admission through request validation, not database persistence.
func TestCodexSwitchSelfImproveLaneAdmission(t *testing.T) {
	var svc Service
	for _, status := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(status, func(t *testing.T) {
			run := store.Run{Harness: harnessCodex, Status: status, Kind: "self_improve"}
			eligible := codexSwitchLaneEligible(run)
			_, err := svc.requestCodexCredentialSwitch(context.Background(), run, BindModePinned, nil)
			if !eligible || !errors.Is(err, ErrCredentialOverridePinnedNeedsSecret) {
				t.Fatalf("eligible=%v, got %v, want pinned secret validation after lane admission", eligible, err)
			}
		})
	}
}

func TestCodexSwitchOwnerLaneRefusals(t *testing.T) {
	var svc Service
	for _, kind := range []string{"chat", "judge", "job", "cross_check", "task"} {
		run := store.Run{Status: "running", Kind: kind}
		if kind == "task" {
			run.ReviewTargetRunID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
		}
		_, err := svc.requestCodexCredentialSwitch(context.Background(), run, BindModePinned, nil)
		if !errors.Is(err, ErrCredentialOverrideLaneNotSwitchable) {
			t.Fatalf("%s: got %v, want lane refusal", kind, err)
		}
	}
}

func TestSetRunCredentialCodexProviderRefusal(t *testing.T) {
	for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "awaiting_input", "paused", "completed"} {
		for _, mode := range []string{CredentialOverrideModeDefault, BindModePinned} {
			t.Run(status+"/"+mode, func(t *testing.T) {
				target := uuid.New()
				r := store.Run{ID: uuid.New(), UserID: uuid.New(), Harness: harnessCodex, Kind: "issue", Status: status,
					ModelProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
				db := &codexOwnerRequestDB{t: t, observed: r}
				s := Service{q: store.New(db)}
				got, err := s.SetRunCredential(context.Background(), r.UserID, r.ID, mode, &target)
				if !errors.Is(err, ErrCredentialOverrideProviderSource) || !reflect.ValueOf(got).IsZero() || db.reads != 1 {
					t.Fatalf("provider refusal: err=%v reads=%d", err, db.reads)
				}
				// The internal observed-run backstop must also precede status/mode dispatch.
				_, err = s.requestCodexCredentialSwitch(context.Background(), r, mode, &target)
				if !errors.Is(err, ErrCredentialOverrideProviderSource) || db.reads != 1 {
					t.Fatal("internal provider refusal touched storage or lost typed error", err)
				}
			})
		}
	}
}

func TestCodexSwitchLockedProviderRefusalAfterFence(t *testing.T) {
	w := store.Worker{ID: uuid.New(), UserID: uuid.New(),
		SnapshotRegisterNonce:      pgtype.Text{String: "registered", Valid: true},
		CredentialSwitchApplyNonce: pgtype.Text{String: "registered", Valid: true},
		ProtocolCapabilities:       []string{capability.CodexCredentialSwitchV1}}
	r := store.Run{ID: uuid.New(), UserID: w.UserID, Harness: harnessCodex, Kind: "issue", Status: "running",
		WorkerID: pgtype.UUID{Bytes: w.ID, Valid: true}, ClaimGeneration: 7,
		ModelProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}
	req := CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: CredentialSwitchApplyRequest{
		ClaimGeneration: 7, RegisterNonce: "registered", OpID: uuid.NewString()}, TargetSecretID: uuid.New()}
	db := &codexOwnerRequestDB{t: t}
	var s Service
	for _, stale := range []bool{false, true} {
		req.ClaimGeneration = 7
		want, reason := ApplyOutcomeNotApplied, applyReasonProviderBound
		if stale {
			req.ClaimGeneration--
			want, reason = ApplyOutcomeFenced, ""
		}
		got, err := s.validateCodexSwitchLocked(context.Background(), store.New(db), w, r, req)
		if err != nil || got.Outcome != want || got.Reason != reason || db.reads != 0 {
			t.Fatalf("stale=%v: outcome=%s reason=%s err=%v", stale, got.Outcome, got.Reason, err)
		}
	}
}
