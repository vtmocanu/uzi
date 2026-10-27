package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// The executed query has scanned its old ciphertext/epoch pair before the
// conversion commits. Refetches use the same wrapper and selected account.
type accountReadSwitchStore struct {
	*store.Queries
	reads     int
	afterRead func(int, store.GetCodexProviderAccountSnapshotRow)
	args      []store.GetCodexProviderAccountSnapshotParams
}

func (q *accountReadSwitchStore) GetCodexProviderAccountSnapshot(ctx context.Context, p store.GetCodexProviderAccountSnapshotParams) (store.GetCodexProviderAccountSnapshotRow, error) {
	row, err := q.Queries.GetCodexProviderAccountSnapshot(ctx, p)
	if err != nil {
		return row, err
	}
	q.reads++
	q.args = append(q.args, p)
	if q.afterRead != nil {
		q.afterRead(q.reads, row)
	}
	return row, nil
}

func TestCodexAccountReaderConversionsLiveDB(t *testing.T) {
	for _, direction := range []string{"enable", "disable", "ABA"} {
		for _, recovery := range []bool{false, true} {
			for _, replace := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/recovery=%v/replacement=%v", direction, recovery, replace), func(t *testing.T) {
					e := setupCodexLiveDB(t)
					f, p := codexRaceFixture(t, e)
					// Both slots contain valid material before conversion, with a real paired
					// discriminator. Conversion must preserve every other account field.
					e.exec(`UPDATE codex_provider_account SET recovery_sealed=sealed_login,
      recovery_sealed_with=sealed_with,recovery_generation=generation WHERE id=$1 AND user_id=$2`, f.accountID, f.userID)
					if direction != "enable" {
						codexRaceToggle(t, e, f, true)
					}
					q := &accountReadSwitchStore{Queries: e.q}
					q.afterRead = func(n int, old store.GetCodexProviderAccountSnapshotRow) {
						if n != 1 {
							return
						}
						if direction == "enable" {
							codexRaceToggle(t, e, f, true)
						} else {
							codexRaceToggle(t, e, f, false)
						}
						if direction == "ABA" {
							codexRaceToggle(t, e, f, true)
							if _, err := f.svc.vlt.Open(f.userID, store.KindCodexAuth, old.CodexProviderAccount.SealedWith, old.CodexProviderAccount.SealedLogin); err == nil {
								t.Fatal("ABA fixture still opens retired-key material")
							}
						}
						if replace {
							raw, err := json.Marshal(codexLoginBlob{AccessToken: "replacement access", RefreshToken: "replacement renewal"}) //nolint:gosec // G117: public synthetic replacement fixture.
							if err != nil {
								t.Fatal(err)
							}
							sealed, err := f.svc.vlt.Seal(e.ctx, f.userID, store.KindCodexAuth, raw, vault.SealForUse)
							if err != nil {
								t.Fatal(err)
							}
							// Deliberate fixture replacement after the serial conversion, not a
							// production writer: the reader must not adopt its new authority.
							e.exec(`UPDATE codex_provider_account SET sealed_login=$3,sealed_with=$4,
        generation=generation+1,credential_revision=credential_revision+1,updated_at=now()
        WHERE id=$1 AND user_id=$2`, f.accountID, f.userID, sealed.Ciphertext, sealed.SealedWith)
						}
					}
					f.svc.q = q
					snapshot, err := readCodexAccountSnapshot(e.ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: f.userID, ID: f.accountID})
					if err != nil {
						t.Fatal(err)
					}
					blob, err := f.svc.openCodexAccountSlot(e.ctx, f.userID, snapshot, recovery)
					if replace {
						if err != store.ErrVaultSealStale || blob.AccessToken != "" {
							t.Fatalf("replacement adopted: err=%v token-present=%v", err, blob.AccessToken != "")
						}
					} else if err != nil || blob.AccessToken != f.accessToken || blob.RefreshToken != f.prevRefreshToken {
						t.Fatalf("conversion changed selected material: err=%v access-match=%v renewal-match=%v", err, blob.AccessToken == f.accessToken, blob.RefreshToken == f.prevRefreshToken)
					}
					if q.reads != 2 || p.calls != 0 || p.discoverCalls != 0 {
						t.Fatalf("reads=%d exchanges=%d identity-calls=%d", q.reads, p.calls, p.discoverCalls)
					}
					for _, arg := range q.args {
						if arg.UserID != f.userID || arg.ID != f.accountID {
							t.Fatal("refetch changed owner or account")
						}
					}
				})
			}
		}
	}
}

func TestCodexAccountReaderSecondTransitionLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	f, p := codexRaceFixture(t, e)
	q := &accountReadSwitchStore{Queries: e.q}
	q.afterRead = func(n int, _ store.GetCodexProviderAccountSnapshotRow) {
		switch n {
		case 1:
			codexRaceToggle(t, e, f, true)
		case 2:
			codexRaceToggle(t, e, f, false)
		}
	}
	f.svc.q = q
	snapshot, err := readCodexAccountSnapshot(e.ctx, q, store.GetCodexProviderAccountSnapshotParams{UserID: f.userID, ID: f.accountID})
	if err != nil {
		t.Fatal(err)
	}
	blob, err := f.svc.openCodexAccountLogin(e.ctx, f.userID, snapshot)
	if err != store.ErrVaultSealStale || blob.AccessToken != "" || q.reads != 2 || p.calls != 0 || p.discoverCalls != 0 {
		t.Fatalf("retry exhaustion: err=%v token-present=%v reads=%d exchanges=%d identity-calls=%d", err, blob.AccessToken != "", q.reads, p.calls, p.discoverCalls)
	}
}
