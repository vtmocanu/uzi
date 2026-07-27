package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/autoselect"
	"gitlab.example.com/vtmocanu/uzi/api/internal/config"
)

// PRD #111 M2 — the auto-selection pool toggle and the live eligibility it feeds.
//
// Live-DB because the two things worth proving here are both invisible to a fake:
// the owner scoping is in the QUERY's own predicate (a fake would answer whatever
// it was told), and the eligibility status is computed from columns whose POSITIONS
// in two hand-written queries have to line up with the classifier's inputs — a
// mismatch there is a runtime scan error or, worse, a confidently wrong status.

// autoEligibleReq is the toggle request, with the {id} chi URL param bound the way
// the router binds it.
func patchAutoEligible(t *testing.T, h *Handler, user uuid.UUID, secretID, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.PatchAnthropicTokenAutoEligible(rec, userReq(http.MethodPatch,
		"/api/me/secrets/anthropic_token/"+secretID+"/auto-eligible", body, user,
		map[string]string{"id": secretID}))
	return rec
}

// autoEligibleOf reads one token's pool flag straight out of GET /api/me/secrets,
// so the assertion is on what the API SAYS rather than on what the row holds.
func autoEligibleOf(t *testing.T, h *Handler, user uuid.UUID, secretID string) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ListMySecrets(rec, userReq(http.MethodGet, "/api/me/secrets", "", user, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: code %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Secrets []struct {
			ID           string `json:"id"`
			AutoEligible bool   `json:"auto_eligible"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, s := range body.Secrets {
		if s.ID == secretID {
			return s.AutoEligible
		}
	}
	t.Fatalf("secret %s not in the list", secretID)
	return false
}

func TestAutoEligibleToggleLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	owner := mkSecretUser(t, pool)
	stranger := mkSecretUser(t, pool)

	rec := h.createToken(t, owner, "default", "sk-ant-autoeligible-owner-0000000000", true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code %d, body %s", rec.Code, rec.Body.String())
	}
	tok := decodeSecret(t, rec).Secret.ID

	// --- A new token is OUT of the pool ------------------------------------
	// D2's default, and the one that must never drift: a token that silently
	// arrived in the pool would spend an account the user reserved for something
	// else, which is the whole reason the pool is opt-in.
	if autoEligibleOf(t, h, owner, tok) {
		t.Fatal("a newly created token is already in the pool; the D2 default must be OUT")
	}

	// --- Toggling on, and the response reporting it ------------------------
	rec = patchAutoEligible(t, h, owner, tok, `{"auto_eligible":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle on: code %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Secret struct {
			AutoEligible bool `json:"auto_eligible"`
		} `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if !resp.Secret.AutoEligible {
		t.Fatal("the toggle response reported auto_eligible=false after opting IN")
	}
	if !autoEligibleOf(t, h, owner, tok) {
		t.Fatal("the list does not reflect the opt-in")
	}

	// Idempotent: re-sending the same value is a 200, not a conflict. A UI that
	// re-sends on a double click must not see an error.
	if rec := patchAutoEligible(t, h, owner, tok, `{"auto_eligible":true}`); rec.Code != http.StatusOK {
		t.Fatalf("re-sending the same value: code %d, body %s", rec.Code, rec.Body.String())
	}

	// --- And off again -----------------------------------------------------
	if rec := patchAutoEligible(t, h, owner, tok, `{"auto_eligible":false}`); rec.Code != http.StatusOK {
		t.Fatalf("toggle off: code %d, body %s", rec.Code, rec.Body.String())
	}
	if autoEligibleOf(t, h, owner, tok) {
		t.Fatal("the list still reports the token pooled after opting OUT")
	}

	// --- A foreign id is a 404, NEVER a 403 --------------------------------
	// The distinction is the point: a 403 would confirm that the id names a real
	// credential belonging to someone else, which is exactly what an attacker
	// enumerating ids wants to learn. Asserted from the STRANGER's side against the
	// owner's real token id, so the id is genuinely valid — a random uuid would
	// pass this even if the check were "does this row exist at all".
	rec = patchAutoEligible(t, h, stranger, tok, `{"auto_eligible":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign id: code %d, want 404 (a 403 confirms the id names someone else's credential)", rec.Code)
	}
	// …and it changed nothing.
	if autoEligibleOf(t, h, owner, tok) {
		t.Fatal("a refused cross-user toggle still wrote to the owner's token")
	}

	// --- Body validation ---------------------------------------------------
	// An OMITTED field is a 400, not a silent false. Leniency in that direction
	// would let a client that sent `{}` opt a token OUT of the pool by accident,
	// which is a spend decision the user never made.
	if rec := patchAutoEligible(t, h, owner, tok, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("omitted auto_eligible: code %d, want 400 — an absent field must not be read as false", rec.Code)
	}
	if rec := patchAutoEligible(t, h, owner, tok, `not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: code %d, want 400", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.PatchAnthropicTokenAutoEligible(rec, userReq(http.MethodPatch,
		"/api/me/secrets/anthropic_token/not-a-uuid/auto-eligible", `{"auto_eligible":true}`, owner,
		map[string]string{"id": "not-a-uuid"}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed id: code %d, want 400", rec.Code)
	}
}

// TestSelfRateLimitsAutoStatusLiveDB is the D21 wiring proof: the status the API
// ships is autoselect.Classify's answer over the REAL query, with the real column
// positions and the real config policy.
//
// It matters because everything either side of Classify is hand-written. The two
// rate-limit queries project auto_eligible in the middle of their column lists, the
// generated scan is positional, and the classifier reads four of those columns. A
// unit test over Classify proves the gate; only this proves the gate is fed.
func TestSelfRateLimitsAutoStatusLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	// A real policy, so `stale` and `below_threshold` are reachable at all: the
	// zero Config has MaxStaleness 0, under which every token is stale and three of
	// the four cases below would pass for the wrong reason.
	h.cfg = config.Config{
		UsagePollInterval:      5 * time.Minute,
		AutoselectMinHeadroom:  15,
		AutoselectMaxStaleness: 15 * time.Minute,
	}
	owner := mkSecretUser(t, pool)

	mk := func(label string, isDefault bool) string {
		t.Helper()
		rec := h.createToken(t, owner, label, "sk-ant-autostatus-"+label+"-000000000", isDefault)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create %s: code %d, body %s", label, rec.Code, rec.Body.String())
		}
		return decodeSecret(t, rec).Secret.ID
	}
	// Four tokens, one per reachable status, plus one left un-pooled. Authored
	// deliberately rather than snapshotted: a fixture where every token looks the
	// same cannot tell a working classifier from one that returns a constant.
	notPooled := mk("not-pooled", true)
	fresh := mk("fresh", false)
	stale := mk("stale", false)
	lowRoom := mk("low-room", false)
	noGauge := mk("no-gauge", false)

	for _, id := range []string{fresh, stale, lowRoom, noGauge} {
		if rec := patchAutoEligible(t, h, owner, id, `{"auto_eligible":true}`); rec.Code != http.StatusOK {
			t.Fatalf("pool %s: code %d, body %s", id, rec.Code, rec.Body.String())
		}
	}

	gauge := func(secretID string, five, seven int16, syncedAgo time.Duration) {
		t.Helper()
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO anthropic_rate_limits
			   (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
			 VALUES ($1, $2, $3, $4, 'usage_endpoint', $5)`,
			uuid.MustParse(secretID), owner, five, seven, time.Now().Add(-syncedAgo)); err != nil {
			t.Fatalf("seed gauge for %s: %v", secretID, err)
		}
	}
	gauge(notPooled, 0, 0, time.Minute) // wide open, but never opted in
	gauge(fresh, 20, 10, time.Minute)   // headroom 80
	gauge(stale, 20, 10, time.Hour)     // headroom 80, but the reading aged out
	gauge(lowRoom, 90, 50, time.Minute) // headroom 10 < MinHeadroom 15
	// noGauge deliberately gets no row at all — the D16 "never polled" case, and
	// the one an INNER JOIN would make unrepresentable by dropping the token.

	rec := httptest.NewRecorder()
	h.SelfRateLimits(rec, userReq(http.MethodGet, "/api/me/rate-limits", "", owner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("self rate limits: code %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tokens []struct {
			SecretID     string `json:"secret_id"`
			AutoEligible bool   `json:"auto_eligible"`
			AutoStatus   string `json:"auto_status"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	got := map[string]string{}
	pooled := map[string]bool{}
	for _, tk := range body.Tokens {
		got[tk.SecretID] = tk.AutoStatus
		pooled[tk.SecretID] = tk.AutoEligible
	}
	// Errorf, NOT Fatalf, and the difference is load-bearing rather than stylistic.
	//
	// This count is a PROXY: it prints the same "a token with no gauge row must still
	// be listed" message whichever of the five vanished, so on its own it can
	// misdirect. Worse, as a Fatalf it aborted before the table below — so the
	// LEFT-JOIN mutation (`AND rl.synced_at IS NOT NULL`) reddened here and the one
	// assertion that actually NAMES D16's property, `{"never polled", noGauge,
	// StatusNoReading}`, never executed. The property was documented, not gated.
	//
	// Continuing is safe and is what makes the report specific: the map lookups below
	// are nil-safe (a missing key yields ""), so the table then says
	// `never polled: auto_status = "" want "no_reading"` and names the token that
	// actually disappeared.
	if len(got) != 5 {
		t.Errorf("got %d tokens, want 5 — a token with no gauge row must still be listed: %+v", len(got), got)
	}
	for _, tc := range []struct {
		name   string
		id     string
		status autoselect.Status
		pooled bool
	}{
		{"never opted in", notPooled, autoselect.StatusNotPooled, false},
		{"fresh and roomy", fresh, autoselect.StatusEligible, true},
		{"reading aged out", stale, autoselect.StatusStale, true},
		{"below the floor", lowRoom, autoselect.StatusBelowThreshold, true},
		{"never polled", noGauge, autoselect.StatusNoReading, true},
	} {
		if got[tc.id] != string(tc.status) {
			t.Errorf("%s: auto_status = %q, want %q", tc.name, got[tc.id], tc.status)
		}
		if pooled[tc.id] != tc.pooled {
			t.Errorf("%s: auto_eligible = %v, want %v", tc.name, pooled[tc.id], tc.pooled)
		}
	}
	// The un-pooled token has 100 points of headroom and the freshest possible
	// reading, so anything other than not_pooled here means the opt-in is being
	// ignored — the one failure that would spend a reserved credential.
	if got[notPooled] != string(autoselect.StatusNotPooled) {
		t.Errorf("an un-pooled token with a perfect reading reported %q; the opt-in gate must win", got[notPooled])
	}
}

// TestAdminRateLimitsCarryAutoFieldsLiveDB: the admin view shares TokenRateLimitDTO
// with the self view, so leaving its two new fields unpopulated would report every
// token in the factory as un-pooled with an empty status — uniform, confident and
// wrong, which is worse than an absent field. The admin query projects
// auto_eligible at a DIFFERENT column position than the per-user one, which is the
// specific thing this catches.
func TestAdminRateLimitsCarryAutoFieldsLiveDB(t *testing.T) {
	h, pool := secretsCRUDHandler(t)
	h.cfg = config.Config{
		UsagePollInterval:      5 * time.Minute,
		AutoselectMinHeadroom:  15,
		AutoselectMaxStaleness: 15 * time.Minute,
	}
	owner := mkSecretUser(t, pool)
	rec := h.createToken(t, owner, "admin-pooled", "sk-ant-adminpool-00000000000000000", true)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: code %d, body %s", rec.Code, rec.Body.String())
	}
	tok := decodeSecret(t, rec).Secret.ID
	if rec := patchAutoEligible(t, h, owner, tok, `{"auto_eligible":true}`); rec.Code != http.StatusOK {
		t.Fatalf("pool: code %d", rec.Code)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO anthropic_rate_limits
		   (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
		 VALUES ($1, $2, 20, 10, 'usage_endpoint', now())`, uuid.MustParse(tok), owner); err != nil {
		t.Fatalf("seed gauge: %v", err)
	}

	rec = httptest.NewRecorder()
	h.AdminRateLimits(rec, userReq(http.MethodGet, "/api/admin/rate-limits", "", owner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin rate limits: code %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Users []struct {
			ID     string `json:"id"`
			Tokens []struct {
				SecretID     string `json:"secret_id"`
				AutoEligible bool   `json:"auto_eligible"`
				AutoStatus   string `json:"auto_status"`
			} `json:"tokens"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, u := range body.Users {
		if u.ID != owner.String() {
			continue
		}
		for _, tk := range u.Tokens {
			if tk.SecretID != tok {
				continue
			}
			found = true
			if !tk.AutoEligible {
				t.Error("the admin view reports a pooled token as un-pooled")
			}
			if tk.AutoStatus != string(autoselect.StatusEligible) {
				t.Errorf("admin auto_status = %q, want %q", tk.AutoStatus, autoselect.StatusEligible)
			}
		}
	}
	if !found {
		t.Fatalf("the seeded token %s is absent from the admin view", tok)
	}
}
