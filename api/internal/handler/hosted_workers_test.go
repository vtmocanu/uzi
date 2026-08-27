package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The offline half of M2's tests (PRD #58). The provision TRANSACTION is not
// reachable from here and that is a property of the repo, not a gap: Handler.pool
// and Handler.q are concrete types, so no fake store can stand in for a
// transaction — the same reason oidc_flow_integration_test.go exists. The quota
// race, per-user isolation, and the co-write invariant therefore live in
// hosted_provision_livedb_test.go, against a real Postgres.
//
// What that leaves here is better than a mock would be. Every handler below is
// built with a NIL POOL, which turns "this request must not reach the database"
// from an assertion about a spy into a mechanical fact: if a gate ever stopped
// gating, the request would dereference a nil pool and panic the test rather than
// quietly seal a token. That is exactly the shape of M2's constraint 2 (nothing but
// the flag stops a flag-off seal), so the flag-off test below IS the constraint's
// test.

// newHostedHandler builds a provision-capable Handler with NO pool. quota is the
// hosted_worker_quota setting's stored value; pass "" to leave the row absent so
// the compiled-in default applies.
func newHostedHandler(enabled bool, quota string) *Handler {
	rows := []store.AppSetting{}
	if quota != "" {
		rows = append(rows, store.AppSetting{Key: settings.KeyHostedWorkerQuota, Value: quota})
	}
	return &Handler{
		cfg:      config.Config{WorkerHostingEnabled: enabled},
		settings: settings.New(&settingsStore{rows: rows}, time.Minute),
		// pool and q stay nil: any DB touch panics, which is the point.
	}
}

// provisionReq drives ProvisionHostedWorker directly with an authenticated user in
// context (the route's RequireAuth is exercised by the router tests; these are the
// handler's own gates).
func provisionReq(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/workers/hosted", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	user := store.User{ID: uuid.New(), Email: "u@uzi.local", IsActive: true}
	req = req.WithContext(mw.ContextWithUser(req.Context(), user))
	rec := httptest.NewRecorder()
	h.ProvisionHostedWorker(rec, req)
	return rec
}

const validProvisionBody = `{"template":"base","size":"m"}`

// M2 constraint 2, and the reason the pool is nil: with hosting disabled nothing
// else in the system stops a provision from minting and sealing a join token. A
// 403 here plus the absence of a panic is proof that no token was generated and
// nothing was written.
func TestProvisionHostedWorkerRefusedWhenHostingDisabled(t *testing.T) {
	h := newHostedHandler(false, "2")
	rec := provisionReq(t, h, validProvisionBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// The flag is checked before the body is read (Register's shape). A garbage body on
// a disabled instance must 403, not 400: the policy answer must not depend on, or
// leak, whether the request was well-formed.
func TestProvisionHostedWorkerChecksFlagBeforeBody(t *testing.T) {
	h := newHostedHandler(false, "2")
	rec := provisionReq(t, h, `{not json at all`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (flag must be checked before the body)", rec.Code)
	}
}

// Decision 8's "0 disables self-service". Policy, not state — a 403, and again no
// DB touch.
func TestProvisionHostedWorkerRefusedWhenQuotaIsZero(t *testing.T) {
	h := newHostedHandler(true, "0")
	rec := provisionReq(t, h, validProvisionBody)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// Membership validation, before any transaction. Every case here would otherwise
// reach a pod spec as free text, or violate ck_workers_hosted_metadata.
func TestProvisionHostedWorkerValidatesTemplateAndSize(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty template", `{"template":"","size":"m"}`},
		{"missing template", `{"size":"m"}`},
		{"unknown template", `{"template":"nope","size":"m"}`},
		// Well-formed but not ours: workertmpl.WellFormed would accept this. Provision
		// must not — the weaker check exists for self-reported drift, not for input.
		{"well-formed unknown template", `{"template":"kubectl-heavy","size":"m"}`},
		{"path-ish template", `{"template":"../base","size":"m"}`},
		{"empty size", `{"template":"base","size":""}`},
		{"missing size", `{"template":"base"}`},
		{"unknown size", `{"template":"base","size":"xl"}`},
		// The UI upper-cases for display; the wire value is lowercase. An accepted
		// "M" would be stored and sent, and never resolve on the controller side.
		{"upper-case size", `{"template":"base","size":"M"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostedHandler(true, "2")
			rec := provisionReq(t, h, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
		})
	}
}

func TestProvisionHostedWorkerRejectsOversizedName(t *testing.T) {
	h := newHostedHandler(true, "2")
	rec := provisionReq(t, h, `{"template":"base","size":"m","name":"`+strings.Repeat("a", maxWorkerNameBytes+1)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestProvisionHostedWorkerRejectsMalformedBody(t *testing.T) {
	h := newHostedHandler(true, "2")
	rec := provisionReq(t, h, `{"template":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// The M5 dialog collects type + size only, but workers.name is NOT NULL. The
// derived name is cosmetic (the controller never reads it — its poll deliberately
// does not select the name) and carries a random hex suffix so two same-size workers
// self-disambiguate, so this pins the FORMAT (base.l-<rand>) rather than an exact
// string.
func TestDerivedHostedWorkerName(t *testing.T) {
	got, err := derivedHostedWorkerName("base", "m")
	if err != nil {
		t.Fatalf("derivedHostedWorkerName: %v", err)
	}
	if !regexp.MustCompile(`^base\.m-[0-9a-f]{4}$`).MatchString(got) {
		t.Fatalf("derivedHostedWorkerName = %q, want to match base.m-<hex4>", got)
	}
	if len(got) > maxWorkerNameBytes {
		t.Fatalf("derived name %q must fit the name cap", got)
	}

	got, err = derivedHostedWorkerName("jvm", "l")
	if err != nil {
		t.Fatalf("derivedHostedWorkerName: %v", err)
	}
	if !regexp.MustCompile(`^jvm\.l-[0-9a-f]{4}$`).MatchString(got) {
		t.Fatalf("derivedHostedWorkerName = %q, want to match jvm.l-<hex4>", got)
	}

	// The suffix must actually vary — that is the whole point of appending random
	// hex (two same-size workers self-disambiguate). A constant/zero suffix would
	// still pass the format regex above, so prove distinctness directly. Across 8
	// calls with identical inputs we require at least two distinct names; the only
	// way that fails is if the suffix is not random. (Two independent 16-bit
	// suffixes collide with p≈1/65536, so 8 draws collapsing to a single value has
	// probability ~(1/65536)^7 — a negligible-but-nonzero flake we accept.)
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		n, err := derivedHostedWorkerName("base", "m")
		if err != nil {
			t.Fatalf("derivedHostedWorkerName: %v", err)
		}
		seen[n] = true
	}
	if len(seen) < 2 {
		t.Fatalf("derivedHostedWorkerName produced only one distinct name across 8 calls (%v); random suffix is not varying", seen)
	}
}

func hostedConfig(t *testing.T, h *Handler) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/workers/hosted/config", nil)
	rec := httptest.NewRecorder()
	h.HostedConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return got
}

// Decision 12: off, the API reports hosting disabled so the UI can hide everything
// hosted rather than discovering it from a 403.
func TestHostedConfigReportsDisabled(t *testing.T) {
	got := hostedConfig(t, newHostedHandler(false, "2"))
	if got["enabled"] != false {
		t.Fatalf("enabled = %v, want false", got["enabled"])
	}
}

func TestHostedConfigReportsEnabledAndQuota(t *testing.T) {
	got := hostedConfig(t, newHostedHandler(true, "5"))
	if got["enabled"] != true {
		t.Fatalf("enabled = %v, want true", got["enabled"])
	}
	if got["quota"] != float64(5) {
		t.Fatalf("quota = %v, want 5", got["quota"])
	}
}

// No seeded row: the compiled-in default (Decision 8's 2) must surface, since
// that is the value a fresh instance actually provisions against.
func TestHostedConfigReportsDefaultQuotaWhenUnset(t *testing.T) {
	got := hostedConfig(t, newHostedHandler(true, ""))
	if got["quota"] != float64(2) {
		t.Fatalf("quota = %v, want the compiled-in default 2", got["quota"])
	}
}

// newHostedHandlerEphemeral builds a HostedConfig-capable Handler (nil pool, like
// newHostedHandler) whose ephemeral kill-switch setting is seeded to ephemeral. quota
// keeps its usual meaning ("" leaves the row absent so the default applies).
func newHostedHandlerEphemeral(enabled bool, quota string, ephemeral bool) *Handler {
	rows := []store.AppSetting{}
	if quota != "" {
		rows = append(rows, store.AppSetting{Key: settings.KeyHostedWorkerQuota, Value: quota})
	}
	ephVal := "false"
	if ephemeral {
		ephVal = "true"
	}
	rows = append(rows, store.AppSetting{Key: settings.KeyEphemeralWorkersEnabled, Value: ephVal})
	return &Handler{
		cfg:      config.Config{WorkerHostingEnabled: enabled},
		settings: settings.New(&settingsStore{rows: rows}, time.Minute),
	}
}

// PRD #649 M2: HostedConfig reports the ephemeral admin kill-switch as
// ephemeral_enabled, so the Workers card knows whether to render the per-user
// auto-provision toggle.
func TestHostedConfigReportsEphemeralEnabled(t *testing.T) {
	got := hostedConfig(t, newHostedHandlerEphemeral(true, "5", true))
	if got["ephemeral_enabled"] != true {
		t.Fatalf("ephemeral_enabled = %v, want true", got["ephemeral_enabled"])
	}
}

func TestHostedConfigReportsEphemeralDisabled(t *testing.T) {
	got := hostedConfig(t, newHostedHandlerEphemeral(true, "5", false))
	if got["ephemeral_enabled"] != false {
		t.Fatalf("ephemeral_enabled = %v, want false", got["ephemeral_enabled"])
	}
}

// The setting read fails closed: with no seeded row the compiled-in default (false)
// must surface, so a fresh instance hides the toggle rather than showing an
// enabled-looking control for a feature nobody turned on.
func TestHostedConfigEphemeralDefaultsClosed(t *testing.T) {
	got := hostedConfig(t, newHostedHandler(true, "5"))
	if got["ephemeral_enabled"] != false {
		t.Fatalf("ephemeral_enabled = %v, want the compiled-in default false", got["ephemeral_enabled"])
	}
}
