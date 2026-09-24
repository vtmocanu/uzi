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

	"github.com/vtmocanu/uzi/api/internal/config"
)

// fakeCodexPoker records Poke calls per user, so a test can assert a credential save or a
// vault unlock did (or did not) request an out-of-band Codex poll. Satisfies CodexUsagePoker.
type fakeCodexPoker struct {
	mu    sync.Mutex
	pokes []uuid.UUID
}

func (f *fakeCodexPoker) Poke(userID uuid.UUID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pokes = append(f.pokes, userID)
}

func (f *fakeCodexPoker) count(userID uuid.UUID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, id := range f.pokes {
		if id == userID {
			n++
		}
	}
	return n
}

func validTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// TestCodexRateLimitStatusPrecedence pins the closed-set status helper's precedence table
// (PRD #1209 M3): polling_disabled > vault_locked > credential_action_required > the
// reading (fresh/stale) > no_reading > pending.
func TestCodexRateLimitStatusPrecedence(t *testing.T) {
	now := time.Now()
	const interval = 5 * time.Minute
	fresh := validTS(now.Add(-1 * time.Minute))     // < 3×interval old
	aged := validTS(now.Add(-20 * time.Minute))     // > 3×interval (15m) old
	boundary := validTS(now.Add(-15 * time.Minute)) // exactly 3×interval old ⇒ still fresh
	null := pgtype.Timestamptz{}

	cases := []struct {
		name           string
		interval       time.Duration
		vaultLocked    bool
		reauthRequired bool
		lastSuccessAt  pgtype.Timestamptz
		lastAttemptAt  pgtype.Timestamptz
		want           string
	}{
		// polling_disabled wins over EVERYTHING, even a locked vault + reauth + a reading.
		{"disabled beats all", 0, true, true, fresh, fresh, codexRateLimitStatusPollingDisabled},
		// vault_locked wins over reauth + a fresh reading.
		{"vault beats reauth+reading", interval, true, true, fresh, fresh, codexRateLimitStatusVaultLocked},
		// reauth wins over a fresh reading.
		{"reauth beats reading", interval, false, true, fresh, fresh, codexRateLimitStatusCredentialActionRequired},
		{"fresh", interval, false, false, fresh, fresh, codexRateLimitStatusFresh},
		{"fresh at 3x boundary", interval, false, false, boundary, boundary, codexRateLimitStatusFresh},
		{"stale", interval, false, false, aged, aged, codexRateLimitStatusStale},
		// no success, but a poll was attempted ⇒ tried and failed.
		{"no_reading", interval, false, false, null, aged, codexRateLimitStatusNoReading},
		// no success and no attempt ⇒ never polled yet.
		{"pending", interval, false, false, null, null, codexRateLimitStatusPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexRateLimitStatus(tc.interval, tc.vaultLocked, tc.reauthRequired, tc.lastSuccessAt, tc.lastAttemptAt, now)
			if got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCodexAuthSavePokesPollerOpenAIDoesNot proves the credential-save poke is gated to the
// codex_auth LOGIN kind (PRD #1209): a codex_auth save requests an out-of-band Codex poll,
// while an openai_api_key save — a static key with no subscription meter — does not. Uses
// the fake codex store, so no live database is needed.
func TestCodexAuthSavePokesPollerOpenAIDoesNot(t *testing.T) {
	db := newFakeCodexDB()
	h := newCodexHandler(t, db)
	poker := &fakeCodexPoker{}
	h.SetCodexUsagePoker(poker)
	user := uuid.New()

	authBody, _ := json.Marshal(map[string]any{"token": codexAuthBlob("acc-tok"), "label": "sub"})
	rec := httptest.NewRecorder()
	h.CreateCodexAuth(rec, codexReq(t, http.MethodPost, "/api/me/secrets/codex_auth", string(authBody), user, ""))
	if rec.Code != http.StatusCreated {
		t.Fatalf("codex_auth create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if got := poker.count(user); got != 1 {
		t.Fatalf("codex_auth save poked %d times, want exactly 1", got)
	}

	// openai_api_key: a non-token-shaped fixture the validator accepts (non-empty, no
	// whitespace/control). It must NOT poke: the poke count stays at 1.
	keyBody, _ := json.Marshal(map[string]any{"token": "openai-test-key-" + uuid.NewString(), "label": "key"})
	rec2 := httptest.NewRecorder()
	h.CreateOpenAIAPIKey(rec2, codexReq(t, http.MethodPost, "/api/me/secrets/openai_api_key", string(keyBody), user, ""))
	if rec2.Code != http.StatusCreated {
		t.Fatalf("openai_api_key create = %d, want 201; body=%s", rec2.Code, rec2.Body.String())
	}
	if got := poker.count(user); got != 1 {
		t.Fatalf("openai_api_key save poked (count now %d); a static key has no meter to poll", got)
	}
}

// TestCodexAccountDTOReason pins the reason field (issue #1594): it is set to
// provider_rejected only when the derived status is credential_action_required AND the
// stored reauth_reason is exactly provider_rejected. Every other status (including a
// vault_locked or polling_disabled account that is also flagged) and every other stored
// value leaves it absent.
func TestCodexAccountDTOReason(t *testing.T) {
	now := time.Now()
	rejected := pgtype.Text{String: codexRateLimitReasonProviderRejected, Valid: true}
	cases := []struct {
		name        string
		interval    time.Duration
		vaultLocked bool
		reauth      bool
		reason      pgtype.Text
		wantStatus  string
		wantReason  string
	}{
		{"flagged provider_rejected", 5 * time.Minute, false, true, rejected, codexRateLimitStatusCredentialActionRequired, "provider_rejected"},
		{"flagged no reason", 5 * time.Minute, false, true, pgtype.Text{}, codexRateLimitStatusCredentialActionRequired, ""},
		{"flagged other stored reason", 5 * time.Minute, false, true, pgtype.Text{String: "something_else", Valid: true}, codexRateLimitStatusCredentialActionRequired, ""},
		{"flagged empty stored reason", 5 * time.Minute, false, true, pgtype.Text{String: "", Valid: true}, codexRateLimitStatusCredentialActionRequired, ""},
		{"vault_locked takes precedence", 5 * time.Minute, true, true, rejected, codexRateLimitStatusVaultLocked, ""},
		{"polling_disabled takes precedence", 0, false, true, rejected, codexRateLimitStatusPollingDisabled, ""},
		{"not flagged, stale reason ignored", 5 * time.Minute, false, false, rejected, codexRateLimitStatusFresh, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{cfg: config.Config{CodexUsagePollInterval: tc.interval}}
			dto := h.codexAccountDTO(uuid.New(), []string{"a"}, false, tc.reauth, tc.reason, nil, validTS(now), validTS(now), tc.vaultLocked)
			if dto.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", dto.Status, tc.wantStatus)
			}
			if dto.Reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", dto.Reason, tc.wantReason)
			}
			raw, err := json.Marshal(dto)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, present := m["reason"]
			if tc.wantReason == "" && present {
				t.Fatalf("reason key present (%v), want absent", got)
			}
			if tc.wantReason != "" && got != tc.wantReason {
				t.Fatalf("wire reason = %v, want %q", got, tc.wantReason)
			}
		})
	}
}
