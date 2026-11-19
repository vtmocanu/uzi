package provisionsvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Recorded-call test (PRD 4173 M4): for a repo with no issue-tracker link,
// pin every forge call EnableRepo makes, including the live write guard, which
// is the REAL privcheck.Service here rather than the package's fakeGuard. The
// golden was captured before the tracker-link rerouting; regenerate only
// deliberately, with UZI_UPDATE_GOLDEN=1.

// guardedForge answers the guard's reads with a clean, write-role bot on a
// protected default branch, and accepts the column label seed.
type guardedForge struct{ forgetest.BaseFake }

func (*guardedForge) VerifyToken(context.Context) (forge.BotIdentity, error) {
	return forge.BotIdentity{ForgeUserID: 9, Username: "bot"}, nil
}
func (*guardedForge) ProjectRole(context.Context, int64, int64) (forge.Role, bool, error) {
	return forge.RoleWrite, true, nil
}
func (*guardedForge) DefaultBranchProtection(context.Context, int64, string, int64) (forge.BranchProtection, error) {
	return forge.BranchProtection{Protected: true}, nil
}
func (*guardedForge) EnsureLabels(context.Context, int64, []forge.Label) error { return nil }

// recordedForges hands the one Recorder-wrapped forge to every caller, the
// service and the guard alike.
type recordedForges struct{ rec *forgetest.Recorder }

func (recordedForges) EncryptToken(pat string) ([]byte, error) { return []byte("sealed:" + pat), nil }
func (r recordedForges) ForgeForToken(forge.Type, string, string) (forge.Forge, error) {
	return r.rec, nil
}
func (r recordedForges) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return r.rec, nil
}

func TestRecordedCallsEnableRepo(t *testing.T) {
	rec := forgetest.NewRecorder(&guardedForge{})
	st := &fakeStore{repoRow: store.GetRepoForUserRow{
		ID: uuid.New(), ForgeType: "gitlab", BaseUrl: "https://forge.example",
		ForgeProjectID: 11, PathWithNamespace: "g/a", DefaultBranch: pgtype.Text{String: "main", Valid: true},
	}}
	guard := privcheck.NewService(nil, recordedForges{rec})
	svc := New(st, recordedForges{rec}, guard, allowAll)

	repo, err := svc.EnableRepo(context.Background(), uuid.New(), st.repoRow.ID)
	if err != nil {
		t.Fatalf("EnableRepo: %v", err)
	}
	if !repo.Enabled {
		t.Fatal("repo must be enabled")
	}
	forgetest.AssertGolden(t, "recorded_calls_provisionsvc_enablerepo.golden", rec.Calls())
}
