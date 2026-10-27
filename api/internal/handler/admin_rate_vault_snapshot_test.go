package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Capture typed list rows before the lifecycle transition. sqlc closes the rows
// after scanning, so afterSnapshot runs before either handler observes live state.
type adminRateSnapshotDB struct {
	*fakeRLDB
	codexRows     []store.ListCodexAccountRateLimitsRow
	afterSnapshot func()
}

func (db *adminRateSnapshotDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	var rows pgx.Rows
	if strings.Contains(sql, "-- name: ListCodexAccountRateLimits ") {
		scans := make([]func(...any) error, 0, len(db.codexRows))
		for _, row := range db.codexRows {
			scans = append(scans, scanAdminCodexRateSnapshot(row))
		}
		rows = &fakeNotifRows{scans: scans}
	} else {
		var err error
		rows, err = db.fakeRLDB.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
	}
	return &adminRateSnapshotRows{Rows: rows, afterSnapshot: db.afterSnapshot}, nil
}

type adminRateSnapshotRows struct {
	pgx.Rows
	afterSnapshot func()
}

func (rows *adminRateSnapshotRows) Close() {
	rows.Rows.Close()
	if rows.afterSnapshot != nil {
		fn := rows.afterSnapshot
		rows.afterSnapshot = nil
		fn()
	}
}

// Keep the scan order aligned with store.ListCodexAccountRateLimits.
func scanAdminCodexRateSnapshot(row store.ListCodexAccountRateLimitsRow) func(...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = row.UserID
		*dest[1].(*string) = row.Email
		*dest[2].(*pgtype.Text) = row.DisplayName
		*dest[3].(*string) = row.VaultMode
		*dest[4].(*uuid.UUID) = row.ProviderAccountID
		*dest[5].(*[]string) = row.Aliases
		*dest[6].(*bool) = row.IsDefault
		*dest[7].(*bool) = row.ReauthRequired
		*dest[8].(*pgtype.Text) = row.ReauthReason
		*dest[9].(*[]byte) = row.Buckets
		*dest[10].(*pgtype.Int8) = row.ObservedGeneration
		*dest[11].(*pgtype.Int8) = row.ObservedCredentialRevision
		*dest[12].(*pgtype.Timestamptz) = row.LastSuccessAt
		*dest[13].(*pgtype.Timestamptz) = row.LastAttemptAt
		*dest[14].(*pgtype.Text) = row.AttemptStatus
		*dest[15].(*pgtype.Text) = row.AttemptError
		return nil
	}
}

func newAdminRateSnapshotDB(id uuid.UUID, mode string) *adminRateSnapshotDB {
	db := &adminRateSnapshotDB{fakeRLDB: &fakeRLDB{}}
	// Exercise both children of the same folded owner.
	for range 2 {
		db.listRows = append(db.listRows, store.ListRateLimitsRow{
			UserID: id, Email: "snapshot@example.com", VaultMode: mode,
			UserSecretID: pgUUIDv(uuid.New()), Label: pgTxt("snapshot-alias"),
		})
		db.codexRows = append(db.codexRows, store.ListCodexAccountRateLimitsRow{
			UserID: id, Email: "snapshot@example.com", VaultMode: mode,
			ProviderAccountID: uuid.New(), Aliases: []string{"snapshot-alias"},
		})
	}
	return db
}

type adminRateStatusReader struct {
	*memVaultQ
	fail string
}

func (q *adminRateStatusReader) GetUserVaultState(ctx context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	if q.fail == "state" {
		return store.GetUserVaultStateRow{}, errors.New("private-credential-sentinel")
	}
	return q.memVaultQ.GetUserVaultState(ctx, id)
}

func (q *adminRateStatusReader) GetVaultPolicy(ctx context.Context) (string, error) {
	if q.fail == "policy" {
		return "", errors.New("private-credential-sentinel")
	}
	return q.memVaultQ.GetVaultPolicy(ctx)
}

func (q *adminRateStatusReader) GetUserVault(ctx context.Context, id uuid.UUID) (store.UserVault, error) {
	if q.fail == "wrapper" {
		return store.UserVault{}, errors.New("private-credential-sentinel")
	}
	return q.memVaultQ.GetUserVault(ctx, id)
}

func TestAdminRateVaultSnapshotAfterEnableLock(t *testing.T) {
	for _, codex := range []bool{false, true} {
		name := "ordinary"
		if codex {
			name = "codex"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			id := uuid.New()
			m := newMemVaultQ()
			m.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
			state, err := m.GetUserVaultState(ctx, id)
			if err != nil || state.VaultMode != store.VaultModeUnprotected {
				t.Fatalf("initial state=%+v err=%v", state, err)
			}
			db := newAdminRateSnapshotDB(id, state.VaultMode)
			box := newHandlerTestBox(t)
			core := newHandlerMemoryVault(box, m)
			h := rlHandler(db, time.Minute)
			h.vault = core
			h.cfg.CodexUsagePollInterval = time.Minute
			transitioned := false
			db.afterSnapshot = func() {
				if err := core.Enable(ctx, id, "snapshot-only-passphrase"); err != nil {
					t.Fatal(err)
				}
				if !core.Unlocked(id) {
					t.Fatal("Enable must publish a cached key before Lock")
				}
				core.Lock(id)
				current, err := m.GetUserVaultState(ctx, id)
				if err != nil || current.VaultMode != store.VaultModeProtected || core.Unlocked(id) {
					t.Fatalf("transition state=%+v err=%v cached=%v", current, err, core.Unlocked(id))
				}
				transitioned = true
			}
			rec := httptest.NewRecorder()
			if codex {
				h.AdminCodexRateLimits(rec, rlReq(uuid.New(), true))
			} else {
				h.AdminRateLimits(rec, rlReq(uuid.New(), true))
			}
			var body struct {
				Users []struct {
					VaultMode   string            `json:"vault_mode"`
					VaultLocked bool              `json:"vault_locked"`
					Tokens      []json.RawMessage `json:"tokens"`
					Accounts    []struct {
						Status string `json:"status"`
					} `json:"accounts"`
				} `json:"users"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || len(body.Users) != 1 {
				t.Fatalf("status=%d err=%v body=%s", rec.Code, err, rec.Body.String())
			}
			if !transitioned || body.Users[0].VaultMode != store.VaultModeProtected || !body.Users[0].VaultLocked {
				t.Fatalf("transitioned=%v body=%s", transitioned, rec.Body.String())
			}
			if codex {
				if len(body.Users[0].Accounts) != 2 {
					t.Fatalf("accounts=%d, want 2", len(body.Users[0].Accounts))
				}
				for _, account := range body.Users[0].Accounts {
					if account.Status != codexRateLimitStatusVaultLocked {
						t.Fatalf("account status=%q, want vault_locked", account.Status)
					}
				}
			} else if len(body.Users[0].Tokens) != 2 {
				t.Fatalf("tokens=%d, want 2", len(body.Users[0].Tokens))
			}
		})
	}
}

func TestAdminRateVaultSnapshotNilCore(t *testing.T) {
	for _, mode := range []string{"", "invalid", store.VaultModeProtected, store.VaultModeUnprotected} {
		for _, codex := range []bool{false, true} {
			name := "ordinary/" + mode
			if codex {
				name = "codex/" + mode
			}
			t.Run(name, func(t *testing.T) {
				h := rlHandler(newAdminRateSnapshotDB(uuid.New(), mode), time.Minute)
				h.cfg.CodexUsagePollInterval = time.Minute
				rec := httptest.NewRecorder()
				if codex {
					h.AdminCodexRateLimits(rec, rlReq(uuid.New(), true))
				} else {
					h.AdminRateLimits(rec, rlReq(uuid.New(), true))
				}
				var body struct {
					Users []struct {
						VaultMode   string `json:"vault_mode"`
						VaultLocked bool   `json:"vault_locked"`
					} `json:"users"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || len(body.Users) != 1 {
					t.Fatalf("status=%d err=%v body=%s", rec.Code, err, rec.Body.String())
				}
				wantMode := store.VaultModeProtected
				if mode == store.VaultModeUnprotected {
					wantMode = store.VaultModeUnprotected
				}
				if body.Users[0].VaultMode != wantMode || body.Users[0].VaultLocked != (wantMode == store.VaultModeProtected) {
					t.Fatalf("fallback body=%s", rec.Body.String())
				}
			})
		}
	}
}

func TestAdminRateVaultSnapshotFailure(t *testing.T) {
	for _, fail := range []string{"state", "policy", "wrapper"} {
		for _, codex := range []bool{false, true} {
			name := "ordinary/" + fail
			if codex {
				name = "codex/" + fail
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				id := uuid.New()
				m := newMemVaultQ()
				m.states[id] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeUnprotected}
				box := newHandlerTestBox(t)
				reader := &adminRateStatusReader{memVaultQ: m}
				h := rlHandler(newAdminRateSnapshotDB(id, store.VaultModeUnprotected), time.Minute)
				h.vault = vault.New(box, reader, m.transaction)
				if err := h.vault.Enable(ctx, id, "failure-only-passphrase"); err != nil {
					t.Fatal(err)
				}
				if !h.vault.Unlocked(id) {
					t.Fatal("fixture must retain a cached key")
				}
				reader.fail = fail
				rec := httptest.NewRecorder()
				if codex {
					h.AdminCodexRateLimits(rec, rlReq(uuid.New(), true))
				} else {
					h.AdminRateLimits(rec, rlReq(uuid.New(), true))
				}
				if rec.Code != http.StatusInternalServerError || strings.TrimSpace(rec.Body.String()) != `{"error":"internal error"}` {
					t.Fatalf("status=%d body=%s, want private generic 500", rec.Code, rec.Body.String())
				}
				if rec.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatal("failure response must be private and uncacheable")
				}
				if strings.Contains(rec.Body.String(), "private-credential-sentinel") || strings.Contains(rec.Body.String(), "failure-only-passphrase") {
					t.Fatal("failure response leaked credential bytes")
				}
			})
		}
	}
}
