package handler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These rows model the atomic SQL projection, not a second credential picker.
// The hook runs after Scan has copied a snapshot and before crypto opens it.
type handlerSnapshotDB struct {
	t               *testing.T
	owner, id       uuid.UUID
	rows            []store.GetAnthropicTokenToPollRow
	afterScan       func(int)
	lookups, writes int
}

func (d *handlerSnapshotDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	d.writes++
	return pgconn.CommandTag{}, errors.New("unexpected snapshot test write")
}
func (d *handlerSnapshotDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected snapshot test collection query")
}
func (d *handlerSnapshotDB) QueryRow(_ context.Context, query string, args ...any) pgx.Row {
	d.lookups++
	if !strings.Contains(query, "name: GetAnthropicTokenToPoll") || len(args) != 2 {
		d.t.Error("snapshot retry used an unexpected query")
		return handlerSnapshotRow{err: pgx.ErrNoRows}
	}
	owner, ownerOK := args[0].(uuid.UUID)
	id, idOK := args[1].(pgtype.UUID)
	if !ownerOK || !idOK || owner != d.owner || !id.Valid || id.Bytes != d.id {
		d.t.Error("snapshot retry changed owner or selected credential")
		return handlerSnapshotRow{err: pgx.ErrNoRows}
	}
	if d.lookups > len(d.rows) {
		return handlerSnapshotRow{err: pgx.ErrNoRows}
	}
	return handlerSnapshotRow{row: d.rows[d.lookups-1], after: d.afterScan, call: d.lookups}
}

type handlerSnapshotRow struct {
	row   store.GetAnthropicTokenToPollRow
	after func(int)
	call  int
	err   error
}

func (r handlerSnapshotRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dst) != 8 {
		return errors.New("unexpected token snapshot scan width")
	}
	*dst[0].(*uuid.UUID) = r.row.ID
	*dst[1].(*uuid.UUID) = r.row.UserID
	*dst[2].(*[]byte) = r.row.Ciphertext
	*dst[3].(*string) = r.row.SealedWith
	*dst[4].(*bool) = r.row.NotifyEarlyLimitReset
	*dst[5].(*int64) = r.row.EnablementRev
	*dst[6].(*int64) = r.row.AnthropicSuccessGeneration
	*dst[7].(*int64) = r.row.VaultEpoch
	if r.after != nil {
		r.after(r.call)
	}
	return nil
}

type handlerSnapshotProvider struct {
	tokens [][]byte
	probes int
}

func (p *handlerSnapshotProvider) Usage(_ context.Context, raw []byte) (anthropic.Reading, error) {
	p.tokens = append(p.tokens, append([]byte(nil), raw...))
	// A transport outcome makes reaching the provider observable without adding
	// unrelated usage writes or a credential rejection to this read-race test.
	return anthropic.Reading{}, errors.New("fixture transport unavailable")
}
func (p *handlerSnapshotProvider) ProbeTest(context.Context, []byte) (anthropic.Reading, bool, error) {
	p.probes++
	return anthropic.Reading{}, false, errors.New("unexpected snapshot test probe")
}

func TestHandlerAnthropicSnapshotRetry(t *testing.T) {
	for _, name := range []string{"normal", "enable", "disable", "ABA", "second transition", "enablement", "success generation", "owner", "id"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			owner, id := uuid.New(), uuid.New()
			box := newHandlerTestBox(t)
			m := newMemVaultQ()
			m.states[owner] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 7}
			v := newHandlerMemoryVault(box, m)
			const phrase = "handler-reader-fixture-passphrase"
			if name == "disable" || name == "ABA" {
				m.states[owner] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected, VaultEpoch: 6}
				if err := v.Enable(ctx, owner, phrase); err != nil {
					t.Fatal(err)
				}
			}
			const original = "selected credential material"
			sealed, err := v.Seal(ctx, owner, store.KindAnthropicToken, []byte(original), vault.SealForUse)
			if err != nil || sealed.Epoch != 7 {
				t.Fatalf("initial snapshot: error=%v epoch=%d", err, sealed.Epoch)
			}
			row := store.GetAnthropicTokenToPollRow{ID: id, UserID: owner, Ciphertext: sealed.Ciphertext, SealedWith: sealed.SealedWith,
				EnablementRev: 3, AnthropicSuccessGeneration: 4, VaultEpoch: sealed.Epoch}
			db := &handlerSnapshotDB{t: t, owner: owner, id: id, rows: []store.GetAnthropicTokenToPollRow{row}}
			provider := &handlerSnapshotProvider{}
			h := &Handler{q: store.New(db), vault: v, box: box, secretTestClients: &secretTestClients{anthropic: provider}}
			h.secretTestOnce.Do(func() {})
			db.afterScan = func(call int) {
				if name == "normal" {
					return
				}
				if call == 2 {
					if name == "second transition" {
						if err := v.Disable(ctx, owner); err != nil {
							t.Fatal(err)
						}
					}
					return
				}
				if name == "disable" || name == "ABA" {
					if err := v.Disable(ctx, owner); err != nil {
						t.Fatal(err)
					}
				} else if err := v.Enable(ctx, owner, phrase); err != nil {
					t.Fatal(err)
				}
				if name == "ABA" {
					if err := v.Enable(ctx, owner, phrase); err != nil {
						t.Fatal(err)
					}
				}
				current, err := v.Seal(ctx, owner, store.KindAnthropicToken, []byte(original), vault.SealForUse)
				if err != nil {
					t.Fatal(err)
				}
				fresh := row
				fresh.Ciphertext, fresh.SealedWith, fresh.VaultEpoch = current.Ciphertext, current.SealedWith, current.Epoch
				switch name {
				case "enablement":
					fresh.EnablementRev++
				case "success generation":
					fresh.AnthropicSuccessGeneration++
				case "owner":
					fresh.UserID = uuid.New()
				case "id":
					fresh.ID = uuid.New()
				}
				db.rows = append(db.rows, fresh)
			}
			got := h.testAnthropicSecret(ctx, owner, id, row.EnablementRev)
			wantLookups, wantCalls, wantReason := 2, 0, "superseded"
			switch name {
			case "normal":
				wantLookups, wantCalls, wantReason = 1, 1, "generic"
			case "enable", "disable", "ABA":
				wantCalls, wantReason = 1, "generic"
			}
			if got.Status != "inconclusive" || got.Reason != wantReason || db.lookups != wantLookups || len(provider.tokens) != wantCalls || provider.probes != 0 || db.writes != 0 {
				t.Fatalf("handler snapshot outcome=%+v lookups=%d want=%d provider-calls=%d want=%d probes=%d writes=%d", got, db.lookups, wantLookups, len(provider.tokens), wantCalls, provider.probes, db.writes)
			}
			for _, raw := range provider.tokens {
				if string(raw) != original {
					t.Fatal("snapshot retry spent different credential material")
				}
			}
		})
	}
}
