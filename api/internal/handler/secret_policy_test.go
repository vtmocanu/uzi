package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHTTPSecretMaterialPolicy(t *testing.T) {
	for _, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
		for _, protected := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/off", true: "/protected"}[protected], func(t *testing.T) {
				var logs bytes.Buffer
				previous := slog.Default()
				slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
				defer slog.SetDefault(previous)
				box := newHandlerTestBox(t)
				anthropic := &fakeUpsertDB{}
				codex := newFakeCodexDB()
				var db store.DBTX = codex
				if kind == store.KindAnthropicToken {
					db = anthropic
				}
				h := newMaterialTestHandler(box, db)
				uid := uuid.New()
				marker := store.SealedWithMasterAAD
				if protected {
					h.vault = testVault(t)
					if err := h.vault.Enable(context.Background(), uid, "fixture-long-password"); err != nil {
						t.Fatal(err)
					}
					anthropic.mode = store.VaultModeProtected
					anthropic.epoch = 1
					codex.mode = store.VaultModeProtected
					codex.epoch = 1
					marker = store.SealedWithDEK
				}
				value := "secret-policy-canary"
				if kind == store.KindCodexAuth {
					value = `{"access_token":"policy-access-canary","refresh_token":"policy-refresh-canary"}`
				}
				body, _ := json.Marshal(map[string]any{"token": value, "label": "policy"})
				invoke := func() *httptest.ResponseRecorder {
					rec := httptest.NewRecorder()
					req := authedAs(http.MethodPost, "/", body, uid)
					switch kind {
					case store.KindAnthropicToken: // compatibility writer has the same policy seam as POST.
						body, _ := json.Marshal(map[string]any{"token": value})
						h.PutAnthropicToken(rec, authedAs(http.MethodPut, "/", body, uid))
					case store.KindOpenAIAPIKey:
						h.CreateOpenAIAPIKey(rec, req)
					case store.KindCodexAuth:
						h.CreateCodexAuth(rec, req)
					}
					return rec
				}
				rec := invoke()
				if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
					t.Fatalf("save=%d %s", rec.Code, rec.Body.String())
				}
				var ct []byte
				var sw string
				if kind == store.KindAnthropicToken {
					ct = anthropic.lastArgs[2].([]byte)
					sw = anthropic.lastArgs[3].(string)
				} else {
					ct = codex.sealedArgs[0]
					sw = codex.sealedWith[0]
				}
				if sw != marker {
					t.Fatalf("marker=%q want %q", sw, marker)
				}
				plain, err := h.vault.Open(uid, kind, sw, ct)
				if err != nil || string(plain) != value {
					t.Fatalf("open: %v", err)
				}
				if _, err := h.vault.Open(uuid.New(), kind, sw, ct); err == nil {
					t.Fatal("cross-user open succeeded")
				}
				if _, err := h.vault.Open(uid, "other-kind", sw, ct); err == nil {
					t.Fatal("cross-kind open succeeded")
				}
				if strings.Contains(rec.Body.String(), value) || strings.Contains(logs.String(), value) || strings.Contains(rec.Body.String(), "policy-access-canary") || strings.Contains(logs.String(), "policy-access-canary") || strings.Contains(rec.Body.String(), "policy-refresh-canary") || strings.Contains(logs.String(), "policy-refresh-canary") {
					t.Fatal("credential leaked")
				}
				if protected {
					h.vault.Lock(uid)
					anthropic.lastArgs = nil
					codex.sealedArgs = nil
					if rec := invoke(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "vault_locked") {
						t.Fatalf("locked=%d %s", rec.Code, rec.Body.String())
					}
					if anthropic.lastArgs != nil || len(codex.sealedArgs) != 0 {
						t.Fatal("locked save wrote material")
					}
				}
			})
		}
	}
}

// Manual pre-ack and eviction must finish before a later unlock can clear the
// acknowledgment. Channel barriers keep both requests in flight without sleeps.
func TestVaultManualLockSerializesPreAckWithUnlock(t *testing.T) {
	m := newMemVaultQ()
	h := &Handler{vault: newHandlerMemoryVault(newHandlerTestBox(t), m)}
	uid := uuid.New()
	const password = "serialization-passphrase"
	if err := h.vault.Enable(context.Background(), uid, password); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var acknowledged atomic.Bool
	m.notice = func(ctx context.Context, id uuid.UUID) (uuid.UUID, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return uuid.Nil, ctx.Err()
		}
		acknowledged.Store(true)
		return id, nil
	}
	var clearedBeforeAck atomic.Bool
	m.clear = func(context.Context, uuid.UUID) error {
		if !acknowledged.Load() {
			clearedBeforeAck.Store(true)
		}
		return nil
	}
	lockDone := make(chan int, 1)
	unlockDone := make(chan int, 1)
	lockJoined, unlockStarted, unlockJoined := false, false, false
	defer func() {
		unblock()
		cancel()
		if !lockJoined {
			<-lockDone
		}
		if unlockStarted && !unlockJoined {
			<-unlockDone
		}
	}()
	go func() {
		rec := httptest.NewRecorder()
		req := authedAs(http.MethodPost, "/", nil, uid)
		req = req.WithContext(mw.ContextWithUser(ctx, store.User{ID: uid}))
		h.VaultLock(rec, req)
		lockDone <- rec.Code
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("manual pre-ack did not start")
	}
	unlockStarted = true
	go func() {
		raw, _ := json.Marshal(map[string]string{"password": password})
		req := authedAs(http.MethodPost, "/", raw, uid)
		req = req.WithContext(mw.ContextWithUser(ctx, store.User{ID: uid}))
		rec := httptest.NewRecorder()
		h.VaultUnlock(rec, req)
		unlockDone <- rec.Code
	}()
	unblock()
	if code := <-lockDone; code != http.StatusNoContent {
		lockJoined = true
		t.Fatalf("lock=%d", code)
	}
	lockJoined = true
	if code := <-unlockDone; code != http.StatusNoContent {
		unlockJoined = true
		t.Fatalf("unlock=%d", code)
	}
	unlockJoined = true
	if clearedBeforeAck.Load() {
		t.Fatal("unlock cleared notice before manual lock pre-ack finished")
	}
	if !h.vault.Unlocked(uid) {
		t.Fatal("later unlock must publish after manual eviction")
	}
}

func TestHTTPSecretWriterConfigurationAndFence(t *testing.T) {
	for _, failure := range []string{"nil-vault", "missing-tx", "nil-tx", "mode", "epoch", "ABA"} {
		t.Run(failure, func(t *testing.T) {
			db := newFakeCodexDB()
			existing := db.seed(store.KindCodexAuth, "original", true)
			h := newMaterialTestHandler(newHandlerTestBox(t), db)
			tx := &handlerSecretTx{db: db}
			h.secretBeginTx = func(context.Context) (pgx.Tx, error) { return tx, nil }
			switch failure {
			case "nil-vault":
				h.vault = nil
			case "missing-tx":
				h.secretBeginTx = nil
			case "nil-tx":
				h.secretBeginTx = func(context.Context) (pgx.Tx, error) { return nil, nil }
			default:
				h.secretBeginTx = func(context.Context) (pgx.Tx, error) {
					if failure == "mode" {
						db.mode = store.VaultModeProtected
					} else {
						db.epoch = 2
					}
					return tx, nil
				}
			}
			rec := httptest.NewRecorder()
			h.CreateCodexAuth(rec, codexReq(t, http.MethodPost, "/", `{"token":"{\"access_token\":\"canary\"}","label":"replacement","default":true}`, uuid.New(), ""))
			want := http.StatusInternalServerError
			if failure == "mode" || failure == "epoch" || failure == "ABA" {
				want = http.StatusConflict
			}
			if rec.Code != want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, want, rec.Body.String())
			}
			if tx.committed {
				t.Fatal("failed admission committed transaction")
			}
			if (failure == "mode" || failure == "epoch" || failure == "ABA") && !tx.rolledBack {
				t.Fatal("stale transaction was not rolled back")
			}
			if len(db.secrets) != 1 || !db.secrets[existing].isDefault || db.clearCalls != 0 || db.bumpCalls != 0 || len(db.sealedArgs) != 0 {
				t.Fatal("failed admission mutated credentials/defaults/aliases")
			}
		})
	}
}
