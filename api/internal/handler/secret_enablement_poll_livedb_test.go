package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// recordingUsagePoker records the rate-limit poller pokes a handler requests.
type recordingUsagePoker struct {
	mu      sync.Mutex
	users   []uuid.UUID
	secrets [][2]uuid.UUID
}

func (p *recordingUsagePoker) Poke(userID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, userID)
}

func (p *recordingUsagePoker) PokeSecret(userID, secretID uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.secrets = append(p.secrets, [2]uuid.UUID{userID, secretID})
}

func (p *recordingUsagePoker) snapshot() ([]uuid.UUID, [][2]uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]uuid.UUID(nil), p.users...), append([][2]uuid.UUID(nil), p.secrets...)
}

// TestSecretReenablePokesPollAndHidesStaleReadingLiveDB (PRD #1732 M3a, D13): a
// re-enabled Anthropic token is polled at once, by id (it is not the default), and
// until that poll lands the owner's meters show it as unavailable rather than
// serving its pre-disable reading as current. A disable, an idempotent repeat and a
// Codex re-enable poke nothing.
func TestSecretReenablePokesPollAndHidesStaleReadingLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	poker := &recordingUsagePoker{}
	h.SetUsagePoker(poker)
	user := mkSecretUser(t, pool)
	def, spare, codex := uuid.New(), uuid.New(), uuid.New()
	for _, s := range []struct {
		id         uuid.UUID
		kind, name string
		isDefault  bool
	}{{def, "anthropic_token", "default", true}, {spare, "anthropic_token", "spare", false}, {codex, "openai_api_key", "codex", true}} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with) VALUES ($1,$2,$3,$4,$5,'x','master')`, s.id, user, s.kind, s.name, s.isDefault); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := h.q.UpsertRateLimits(t.Context(), store.UpsertRateLimitsParams{
		UserSecretID: spare, UserID: user, EnablementRev: 0,
		FiveHourPct: pgtype.Int2{Int16: 12, Valid: true}, SevenDayPct: pgtype.Int2{Int16: 12, Valid: true},
		Source:   pgtype.Text{String: "usage_endpoint", Valid: true},
		SyncedAt: pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	}); err != nil || n != 1 {
		t.Fatalf("seed reading: n=%d err=%v", n, err)
	}
	spareStatus := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.SelfRateLimits(rec, userReq(http.MethodGet, "/api/me/rate-limits", "", user, nil))
		var out struct {
			Tokens []struct {
				SecretID string `json:"secret_id"`
				Limits   struct {
					Status string `json:"status"`
				} `json:"limits"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		for _, tok := range out.Tokens {
			if tok.SecretID == spare.String() {
				return tok.Limits.Status
			}
		}
		t.Fatalf("spare token missing from %s", rec.Body.String())
		return ""
	}
	if got := spareStatus(); got != "ok" {
		t.Fatalf("control: spare status = %q, want ok", got)
	}

	for i := range 2 { // a disable, then its idempotent repeat
		if code, _ := enablementRequest(t, h, user, "anthropic_token", spare, `{"enabled":false}`); code != 200 {
			t.Fatalf("disable #%d: %d", i+1, code)
		}
	}
	if users, secrets := poker.snapshot(); len(users)+len(secrets) != 0 {
		t.Fatalf("a disable poked the poller: users=%v secrets=%v", users, secrets)
	}

	if code, _ := enablementRequest(t, h, user, "anthropic_token", spare, `{"enabled":true}`); code != 200 {
		t.Fatalf("re-enable: %d", code)
	}
	users, secrets := poker.snapshot()
	if len(users) != 0 || len(secrets) != 1 || secrets[0] != [2]uuid.UUID{user, spare} {
		t.Fatalf("re-enable pokes: users=%v secrets=%v, want exactly PokeSecret(%s, %s)", users, secrets, user, spare)
	}
	if got := spareStatus(); got != "unavailable" {
		t.Fatalf("after re-enable spare status = %q, want unavailable (the pre-disable reading is not current)", got)
	}

	if code, _ := enablementRequest(t, h, user, "anthropic_token", spare, `{"enabled":true}`); code != 200 {
		t.Fatalf("repeat enable: %d", code)
	}
	for _, body := range []string{`{"enabled":false}`, `{"enabled":true}`} {
		if code, _ := enablementRequest(t, h, user, "openai_api_key", codex, body); code != 200 {
			t.Fatalf("codex %s: %d", body, code)
		}
	}
	if users, secrets := poker.snapshot(); len(users) != 0 || len(secrets) != 1 {
		t.Fatalf("a repeat or Codex transition poked the Anthropic poller: users=%v secrets=%v", users, secrets)
	}
}
