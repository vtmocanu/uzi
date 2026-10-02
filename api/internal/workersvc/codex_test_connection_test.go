package workersvc

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/codexauth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type linkedProbeStore struct {
	Store
	account store.CodexProviderAccount
	reads   int
}

func (f *linkedProbeStore) GetCodexProviderAccountByID(_ context.Context, _ store.GetCodexProviderAccountByIDParams) (store.CodexProviderAccount, error) {
	f.reads++
	return f.account, nil
}
func (*linkedProbeStore) CountEnabledLinkedAliasesForCodexAccount(context.Context, store.CountEnabledLinkedAliasesForCodexAccountParams) (int64, error) {
	return 1, nil
}
func (*linkedProbeStore) MarkCodexReauthRequired(context.Context, store.MarkCodexReauthRequiredParams) (int64, error) {
	panic("linked test must not set reauth")
}

type linkedProbeReader func(context.Context, string, string) (codexauth.UsageReading, error)

func (f linkedProbeReader) ReadUsage(ctx context.Context, token, workspace string) (codexauth.UsageReading, error) {
	return f(ctx, token, workspace)
}

func TestCodexLinkedProbeUnit(t *testing.T) {
	for _, tc := range []struct {
		name, state, refresh, wantStatus, wantReason string
		err                                          error
		calls                                        int
	}{
		{"renewable 401", codexCoordIdle, "renewal", "inconclusive", "generic", &codexauth.AuthError{StatusCode: http.StatusUnauthorized}, 1},
		{"unrenewable 401", codexCoordIdle, "", "rejected", "", &codexauth.AuthError{StatusCode: http.StatusUnauthorized}, 1},
		{"quarantined", codexCoordQuarantined, "renewal", "inconclusive", "generic", nil, 0},
		{"in progress", codexCoordInProgress, "renewal", "inconclusive", "generic", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, err := secretbox.New(make([]byte, secretbox.KeySize))
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(codexLoginBlob{AccessToken: "access", RefreshToken: tc.refresh}) //nolint:gosec // G117: fixture login blob with placeholder tokens
			if err != nil {
				t.Fatal(err)
			}
			sealed, err := box.Seal(raw)
			if err != nil {
				t.Fatal(err)
			}
			userID, accountID := uuid.New(), uuid.New()
			q := &linkedProbeStore{account: store.CodexProviderAccount{
				UserID: userID, ID: accountID, CoordState: tc.state,
				SealedLogin: sealed, SealedWith: store.SealedWithMaster,
				ProviderUserID: "user", WorkspaceAccountID: "workspace",
			}}
			svc := &Service{q: q, box: box}
			calls := 0
			reader := linkedProbeReader(func(_ context.Context, token, workspace string) (codexauth.UsageReading, error) {
				calls++
				if token != "access" || workspace != "workspace" {
					t.Fatal("wrong credential or workspace")
				}
				return codexauth.UsageReading{UserID: "user"}, tc.err
			})
			status, reason := svc.TestCodexLinkedAccount(context.Background(), userID, accountID, reader)
			if status != tc.wantStatus || reason != tc.wantReason || calls != tc.calls {
				t.Fatalf("status=%q reason=%q calls=%d", status, reason, calls)
			}
			if q.account.ReauthRequired {
				t.Fatal("reauth changed")
			}
		})
	}
}
