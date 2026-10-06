package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/config"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #649 M2 live-DB seam proof. The three seams are hand-written Go over generated
// code, so the risk they carry is invisible to a fake: whether the RETURNING column
// list of setUserEphemeralWorkersEnabled actually scans into User.EphemeralWorkersEnabled,
// whether toDTO's mapping lines the column up with the json field, and whether the
// column round-trips at all. This drives all three against a real Postgres.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix).
func ephemeralSeamHandler(t *testing.T, ephemeralAdmin bool) (*Handler, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ephVal := "false"
	if ephemeralAdmin {
		ephVal = "true"
	}
	h := &Handler{
		pool: pool,
		q:    store.New(pool),
		cfg:  config.Config{WorkerHostingEnabled: true},
		settings: settings.New(&settingsStore{rows: []store.AppSetting{
			{Key: settings.KeyEphemeralWorkersEnabled, Value: ephVal},
		}}, time.Minute),
	}
	return h, pool
}

// meReq builds a GET /api/me request whose context carries the user AS LOADED FROM
// THE DB, mirroring what RequireAuth does on a real request — so Me's toDTO runs over
// the real row, not a stubbed one.
func meReq(t *testing.T, h *Handler, id uuid.UUID) map[string]any {
	t.Helper()
	u, err := h.q.GetUserByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), u))
	rec := httptest.NewRecorder()
	h.Me(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me: code %d, body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		User map[string]any `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /api/me: %v (body=%s)", err, rec.Body.String())
	}
	return body.User
}

func TestEphemeralWorkersSeamLiveDB(t *testing.T) {
	h, pool := ephemeralSeamHandler(t, true)
	owner := mkSecretUser(t, pool)

	// --- Default is OUT: a fresh user is not opted in --------------------------
	if got := meReq(t, h, owner)["ephemeral_workers_enabled"]; got != false {
		t.Fatalf("fresh user /api/me ephemeral_workers_enabled = %v, want false", got)
	}

	// --- PUT /me/ephemeral-workers {enabled:true} flips it, over the real DB ---
	rec := httptest.NewRecorder()
	h.SetEphemeralWorkersEnabled(rec, userReq(http.MethodPut, "/api/me/ephemeral-workers", `{"enabled":true}`, owner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle on: code %d, body %s", rec.Code, rec.Body.String())
	}
	var toggleResp struct {
		User struct {
			ID                      string `json:"id"`
			EphemeralWorkersEnabled bool   `json:"ephemeral_workers_enabled"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &toggleResp); err != nil {
		t.Fatalf("decode toggle response: %v", err)
	}
	if toggleResp.User.ID != owner.String() {
		t.Errorf("toggle response user id = %s, want session %s", toggleResp.User.ID, owner)
	}
	if !toggleResp.User.EphemeralWorkersEnabled {
		t.Fatal("toggle response reported ephemeral_workers_enabled=false after opting IN")
	}

	// --- The column persisted and /api/me reflects it -------------------------
	if got := meReq(t, h, owner)["ephemeral_workers_enabled"]; got != true {
		t.Fatalf("after opt-in, /api/me ephemeral_workers_enabled = %v, want true", got)
	}

	// --- And off again --------------------------------------------------------
	rec = httptest.NewRecorder()
	h.SetEphemeralWorkersEnabled(rec, userReq(http.MethodPut, "/api/me/ephemeral-workers", `{"enabled":false}`, owner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle off: code %d, body %s", rec.Code, rec.Body.String())
	}
	if got := meReq(t, h, owner)["ephemeral_workers_enabled"]; got != false {
		t.Fatalf("after opt-out, /api/me ephemeral_workers_enabled = %v, want false", got)
	}

	// --- The toggle targets the SESSION user, never another user --------------
	// A second user must be untouched by the owner's toggles.
	stranger := mkSecretUser(t, pool)
	rec = httptest.NewRecorder()
	h.SetEphemeralWorkersEnabled(rec, userReq(http.MethodPut, "/api/me/ephemeral-workers", `{"enabled":true}`, owner, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("re-toggle on: code %d, body %s", rec.Code, rec.Body.String())
	}
	if got := meReq(t, h, stranger)["ephemeral_workers_enabled"]; got != false {
		t.Fatalf("stranger flipped by owner's toggle: ephemeral_workers_enabled = %v, want false", got)
	}

	// --- HostedConfig reflects the admin gate ---------------------------------
	if got := hostedConfig(t, h)["ephemeral_enabled"]; got != true {
		t.Fatalf("hosted config ephemeral_enabled = %v, want true (admin gate on)", got)
	}
	hOff, _ := ephemeralSeamHandler(t, false)
	if got := hostedConfig(t, hOff)["ephemeral_enabled"]; got != false {
		t.Fatalf("hosted config ephemeral_enabled = %v, want false (admin gate off)", got)
	}
}

func TestEphemeralDockerPreferencesLiveDB(t *testing.T) {
	h, pool := ephemeralSeamHandler(t, true)
	h.cfg.WorkerDockerEnabled = true
	owner, stranger := mkSecretUser(t, pool), mkSecretUser(t, pool)
	for _, tc := range []struct {
		name, body            string
		tier, enabled, docker bool
		status                int
	}{
		{"docker only", `{"docker":true}`, true, false, true, 200},
		{"enabled on", `{"enabled":true}`, true, true, true, 200},
		{"enabled off retains", `{"enabled":false}`, true, false, true, 200},
		{"enabled on retains", `{"enabled":true}`, true, true, true, 200},
		{"neither", `{}`, true, true, true, 200},
		{"empty", ``, true, true, true, 200},
		{"denial no partial write", `{"enabled":false,"docker":true}`, false, true, true, 409},
		{"disable tier unavailable", `{"docker":false}`, false, true, false, 200},
		{"both", `{"enabled":false,"docker":true}`, true, false, true, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.cfg.WorkerDockerEnabled = tc.tier
			rec := httptest.NewRecorder()
			h.SetEphemeralWorkersEnabled(rec, userReq(http.MethodPut, "/api/me/ephemeral-workers", tc.body, owner, nil))
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			u := meReq(t, h, owner)
			if u["ephemeral_workers_enabled"] != tc.enabled || u["ephemeral_docker_enabled"] != tc.docker {
				t.Fatalf("persisted=%v", u)
			}
			if tc.status == 200 {
				var out struct {
					User map[string]any `json:"user"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
				if out.User["ephemeral_docker_enabled"] != tc.docker || out.User["ephemeral_workers_enabled"] != tc.enabled {
					t.Fatalf("response=%v", out.User)
				}
			}
			u = meReq(t, h, stranger)
			if u["ephemeral_workers_enabled"] != false || u["ephemeral_docker_enabled"] != false {
				t.Fatal("stranger changed")
			}
			if got := hostedConfig(t, h)["docker_enabled"]; got != tc.tier {
				t.Fatalf("hosted docker=%v", got)
			}
		})
	}
	h.cfg.WorkerHostingEnabled = false
	if got := hostedConfig(t, h)["docker_enabled"]; got != false {
		t.Fatalf("hosting off docker=%v", got)
	}
}
