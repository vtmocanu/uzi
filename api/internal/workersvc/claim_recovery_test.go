package workersvc

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestClaimRecoveryAuthorityClasses(t *testing.T) {
	key, err := codexAccountKey("provider", "workspace")
	if err != nil {
		t.Fatal(err)
	}
	baseRun := store.Run{
		Kind:                  runkind.Issue,
		CodexAuthMode:         pgtype.Text{String: codexAuthModeSubscription, Valid: true},
		CodexAccountKey:       pgtype.Text{String: key, Valid: true},
		CodexMaterialRevision: pgtype.Int8{Int64: 1, Valid: true},
	}
	base := store.GetRunCodexAuthContextRow{
		CodexAuthMode:             baseRun.CodexAuthMode,
		CodexAccountKey:           baseRun.CodexAccountKey,
		CodexMaterialRevision:     baseRun.CodexMaterialRevision,
		CodexAccountRevision:      pgtype.Int8{Int64: 1, Valid: true},
		CurrentMaterialRevision:   1,
		CurrentCredentialRevision: pgtype.Int8{Int64: 1, Valid: true},
		ProviderUserID:            pgtype.Text{String: "provider", Valid: true},
		WorkspaceAccountID:        pgtype.Text{String: "workspace", Valid: true},
		CurrentCoordState:         pgtype.Text{String: "idle", Valid: true},
		BoundKind:                 store.KindCodexAuth,
		Status:                    "claimed",
	}
	baseRun.CodexAccountRevision = base.CodexAccountRevision
	cases := []struct {
		name     string
		change   func(*store.Run, *store.GetRunCodexAuthContextRow)
		state    string
		material int64
		hold     bool
		want     error
	}{
		{"healthy", nil, "linked", 1, false, nil},
		{"quarantined", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) {
			r.CurrentCoordState.String = codexCoordQuarantined
		}, "linked", 1, true, ErrCodexAccountQuarantined},
		{"refresh lease", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.CurrentCoordState.String = "in_progress" }, "linked", 1, false, nil},
		{"staging relogin", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.CurrentMaterialRevision = 2 }, "staging", 2, true, ErrCodexMaterialRevisionStale},
		{"failed relogin", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.CurrentMaterialRevision = 2 }, "failed", 2, true, ErrCodexMaterialRevisionStale},
		{"linked verified relogin", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.CurrentMaterialRevision = 2 }, "linked", 2, true, ErrCodexMaterialRevisionStale},
		{"linked different identity", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) {
			r.CurrentMaterialRevision = 2
			r.WorkspaceAccountID.String = "elsewhere"
		}, "linked", 2, false, ErrCodexMaterialRevisionStale},
		{"linked revoked revision", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) {
			r.CurrentMaterialRevision = 2
			r.CurrentCredentialRevision.Int64 = 2
		}, "linked", 2, false, ErrCodexMaterialRevisionStale},
		{"revision revoked", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.CurrentCredentialRevision.Int64 = 2 }, "linked", 1, false, ErrCodexAccountRevisionStale},
		{"identity changed", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.WorkspaceAccountID.String = "elsewhere" }, "linked", 1, false, ErrCodexAccountTupleMismatch},
		{"kind mismatch", func(_ *store.Run, r *store.GetRunCodexAuthContextRow) { r.BoundKind = store.KindOpenAIAPIKey }, "linked", 1, false, ErrCodexKindModeMismatch},
		{"first link absent", func(run *store.Run, r *store.GetRunCodexAuthContextRow) {
			run.CodexAccountKey.Valid = false
			r.CodexAccountKey.Valid = false
			r.CurrentMaterialRevision = 2
		}, "staging", 2, false, ErrCodexMaterialRevisionStale},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run, row := baseRun, base
			if tc.change != nil {
				tc.change(&run, &row)
			}
			hold, got := classifyCodexClaimAuthority(run, row, tc.state, tc.material)
			if hold != tc.hold || !errors.Is(got, tc.want) || (tc.want == nil && got != nil) {
				t.Fatalf("hold=%v err=%v, want hold=%v err=%v", hold, got, tc.hold, tc.want)
			}
		})
	}
}

func TestTransientClaimDBError(t *testing.T) {
	for _, tc := range []struct {
		code string
		want bool
	}{
		{"40001", true}, {"40P01", true}, {"08006", true}, {"53300", true},
		{"57P03", true}, {"23505", false}, {"22001", false},
	} {
		err := errors.Join(errCredentialUnavailable, &pgconn.PgError{Code: tc.code})
		if got := isTransientClaimDBError(err); got != tc.want {
			t.Errorf("SQLSTATE %s: transient=%v want %v", tc.code, got, tc.want)
		}
	}
}

func TestClaimCustodyExpectation(t *testing.T) {
	for _, tc := range []struct {
		kind          string
		capable, want bool
	}{
		{runkind.Issue, true, true}, {runkind.MRRework, true, true},
		{runkind.Judge, true, false}, {runkind.Issue, false, false},
	} {
		if got := claimOpenedCustody(tc.kind, tc.capable); got != tc.want {
			t.Errorf("%s capable=%v: got %v want %v", tc.kind, tc.capable, got, tc.want)
		}
	}
}
