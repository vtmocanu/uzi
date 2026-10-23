package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/producttoken"
)

// All requests use cliLiveDB's full mounted router and real worker service.
// These tests require UZI_TEST_DATABASE_URL; an unset DSN is a skip, not DB proof.
func manualCordonSeedWorker(t *testing.T, pool *pgxpool.Pool, owner uuid.UUID, kind string, ephemeral bool) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	token := "uzw_" + id.String()
	cliMustExec(t, pool, `INSERT INTO workers
		(id, user_id, name, token_hash, status, last_heartbeat_at, kind, hosted_size, template_declared, ephemeral)
		VALUES ($1, $2, 'manual-cordon', $3, 'online', now(), $4,
		        CASE WHEN $4 = 'hosted' THEN 'm' ELSE NULL END, 'base', $5)`,
		id, owner, jointoken.Hash(token), kind, ephemeral)
	return id, token
}

func manualCordonWorkerResponse(t *testing.T, rec *httptest.ResponseRecorder, id uuid.UUID) apitypes.WorkerDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("response = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body struct {
		Worker *apitypes.WorkerDTO `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Worker == nil || body.Worker.ID != id.String() {
		t.Fatalf("worker response for %s: %s", id, rec.Body.String())
	}
	w := *body.Worker
	if w.Status != "online" || w.ActiveRuns != 0 || w.ActiveCrossChecks != 0 || w.Busy || len(w.ReportedRuns) != 0 {
		t.Fatalf("cordon changed idle online activity: %+v", w)
	}
	return w
}

func manualCordonEventCounts(t *testing.T, pool *pgxpool.Pool, worker uuid.UUID, cordon, uncordon int) {
	t.Helper()
	var gotCordon, gotUncordon int
	if err := pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE action = 'cordon'), count(*) FILTER (WHERE action = 'uncordon')
		FROM worker_cordon_events WHERE worker_id = $1`, worker).Scan(&gotCordon, &gotUncordon); err != nil {
		t.Fatal(err)
	}
	if gotCordon != cordon || gotUncordon != uncordon {
		t.Fatalf("worker %s events = cordon:%d uncordon:%d, want %d/%d", worker, gotCordon, gotUncordon, cordon, uncordon)
	}
}

// manualCordonStateSnapshot compares the complete persisted tuple and every audit row.
func manualCordonStateSnapshot(t *testing.T, pool *pgxpool.Pool, worker uuid.UUID) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'tuple', jsonb_build_array(manual_cordoned_at,manual_cordoned_by,manual_cordoned_by_label,manual_cordon_reason),
 'events', (SELECT COALESCE(jsonb_agg(to_jsonb(e) ORDER BY e.occurred_at,e.id),'[]'::jsonb) FROM worker_cordon_events e WHERE e.worker_id=w.id))::text
 FROM workers w WHERE id=$1`, worker).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestManualCordonTransitionsLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	cliMustExec(t, pool, `UPDATE users SET display_name = $2 WHERE id = $1`, owner, "  Op\ner\tator\u202e  ")
	jwt := cliMintJWT(t, pool, owner)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	for _, kind := range []string{"hosted", "external"} {
		t.Run(kind, func(t *testing.T) {
			id, _ := manualCordonSeedWorker(t, pool, owner, kind, false)
			path := "/api/workers/" + id.String() + "/cordon"
			before := time.Now().Add(-time.Minute)
			first := manualCordonWorkerResponse(t, cookieReq(t, router, http.MethodPost, path, jwt,
				`{"reason":"  Fix\nthis\t\u202e\u001b[31m now  "}`), id)
			if first.Kind != kind || first.ManualCordonedAt == nil || first.ManualCordonedAt.Before(before) ||
				first.ManualCordonedAt.After(time.Now().Add(time.Minute)) || first.ManualCordonedBy == nil ||
				*first.ManualCordonedBy != owner.String() || first.ManualCordonedByLabel == nil ||
				*first.ManualCordonedByLabel != "Op er ator" || first.ManualCordonReason == nil ||
				*first.ManualCordonReason != "Fix this  now" || !first.EffectiveCordoned {
				t.Fatalf("cordoned metadata: %+v", first)
			}
			manualCordonEventCounts(t, pool, id, 1, 0)
			// Different actor label and reason must not overwrite the first transition.
			cliMustExec(t, pool, `UPDATE users SET display_name = 'Changed' WHERE id = $1`, owner)
			repeat := manualCordonWorkerResponse(t, bearerReqBody(router, http.MethodPost, path, token, `{"reason":"replacement"}`), id)
			if !reflect.DeepEqual(first.ManualCordonedAt, repeat.ManualCordonedAt) ||
				!reflect.DeepEqual(first.ManualCordonedBy, repeat.ManualCordonedBy) ||
				!reflect.DeepEqual(first.ManualCordonedByLabel, repeat.ManualCordonedByLabel) ||
				!reflect.DeepEqual(first.ManualCordonReason, repeat.ManualCordonReason) || !repeat.EffectiveCordoned {
				t.Fatalf("repeat POST overwrote metadata: first=%+v repeat=%+v", first, repeat)
			}
			manualCordonEventCounts(t, pool, id, 1, 0)
			for i := 0; i < 2; i++ {
				var rec *httptest.ResponseRecorder
				if i == 0 {
					rec = cookieReq(t, router, http.MethodDelete, path, jwt, "")
				} else {
					rec = bearerReq(router, http.MethodDelete, path, token)
				}
				cleared := manualCordonWorkerResponse(t, rec, id)
				if cleared.ManualCordonedAt != nil || cleared.ManualCordonedBy != nil ||
					cleared.ManualCordonedByLabel != nil || cleared.ManualCordonReason != nil || cleared.EffectiveCordoned {
					t.Fatalf("DELETE did not clear cordon: %+v", cleared)
				}
				manualCordonEventCounts(t, pool, id, 1, 1)
			}
			// Restore the hostile fixture for the next worker kind.
			cliMustExec(t, pool, `UPDATE users SET display_name = $2 WHERE id = $1`, owner, "  Op\ner\tator\u202e  ")
			var actor, label, reason string
			if err := pool.QueryRow(context.Background(), `SELECT actor_id::text, actor_label, reason
				FROM worker_cordon_events WHERE worker_id = $1 AND action = 'cordon'`, id).Scan(&actor, &label, &reason); err != nil {
				t.Fatal(err)
			}
			if actor != owner.String() || label != "Op er ator" || reason != "Fix this  now" {
				t.Fatalf("stored audit snapshot = %q %q %q", actor, label, reason)
			}
		})
	}
}

func TestManualCordonReasonStorageLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	token := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	for _, tc := range []struct {
		name, body string
		want       *string
	}{
		{name: "empty body"},
		{name: "missing reason", body: "{}"},
		{name: "null reason", body: `{"reason":null}`},
		{name: "empty reason", body: `{"reason":""}`},
		{name: "sanitized empty", body: `{"reason":" \n\t\u202e "}`},
		{name: "500 emoji", body: `{"reason":"` + strings.Repeat("😀", 500) + `"}`, want: func() *string { s := strings.Repeat("😀", 500); return &s }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, _ := manualCordonSeedWorker(t, pool, owner, "external", false)
			w := manualCordonWorkerResponse(t, bearerReqBody(router, http.MethodPost, "/api/workers/"+id.String()+"/cordon", token, tc.body), id)
			if !reflect.DeepEqual(w.ManualCordonReason, tc.want) || !w.EffectiveCordoned ||
				w.ManualCordonedByLabel == nil || *w.ManualCordonedByLabel != owner.String() {
				t.Fatalf("reason/fallback actor = %+v, want reason %v, actor %s", w, tc.want, owner)
			}
			var stored *string
			if err := pool.QueryRow(context.Background(), `SELECT manual_cordon_reason FROM workers WHERE id=$1`, id).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(stored, tc.want) {
				t.Fatalf("stored reason = %v, want %v", stored, tc.want)
			}
			manualCordonEventCounts(t, pool, id, 1, 0)
		})
	}
}

func TestManualCordonMountedAuthLiveDB(t *testing.T) {
	h, _, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	other := cliSeedUser(t, pool, false)
	admin := cliSeedUser(t, pool, true)
	id, workerToken := manualCordonSeedWorker(t, pool, owner, "external", false)
	ephemeral, _ := manualCordonSeedWorker(t, pool, owner, "hosted", true)
	ownerJWT := cliMintJWT(t, pool, owner)
	controllerToken := uuid.NewString()
	h.cfg.ControllerTokenSHA256 = jointoken.Hash(controllerToken)
	h.cfg.WorkerHostingEnabled = true
	lim := mw.NewLimiter(100000, time.Minute, nil)
	router := h.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
	product := v1MintProductToken(t, h.q, owner, v1SeedProduct(t, h.q, admin), producttoken.Scopes, nil).token
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			if method == http.MethodDelete {
				manualCordonWorkerResponse(t, bearerReqBody(router, http.MethodPost, "/api/workers/"+id.String()+"/cordon", cliMintToken(t, pool, owner, clitoken.ScopeUser), `{"reason":"retain rejected DELETE evidence"}`), id)
			}
			for _, tc := range []struct {
				name, jwt, bearer, csrf string
				worker                  uuid.UUID
				want                    int
				errText                 string
			}{
				{name: "unauthenticated", worker: id, want: 401},
				{name: "missing CSRF", jwt: ownerJWT, worker: id, want: 403},
				{name: "bad CSRF", jwt: ownerJWT, csrf: "bad", worker: id, want: 403},
				{name: "nonowner session", jwt: cliMintJWT(t, pool, other), csrf: "valid", worker: id, want: 404},
				{name: "nonowner uzc", bearer: cliMintToken(t, pool, other, clitoken.ScopeUser), worker: id, want: 404},
				{name: "admin session foreign", jwt: cliMintJWT(t, pool, admin), csrf: "valid", worker: id, want: 404},
				{name: "admin uza foreign", bearer: cliMintToken(t, pool, admin, clitoken.ScopeAdminRO), worker: id, want: 404},
				{name: "worker token", bearer: workerToken, worker: id, want: 401},
				{name: "controller token", bearer: controllerToken, worker: id, want: 401},
				{name: "product token", bearer: product, worker: id, want: 401},
				{name: "invalid bearer overrides cookie", jwt: ownerJWT, csrf: "valid", bearer: "invalid", worker: id, want: 401},
				{name: "owner ephemeral", jwt: ownerJWT, csrf: "valid", worker: ephemeral, want: 409, errText: "manual cordon is only supported for persistent workers"},
				{name: "foreign ephemeral", jwt: cliMintJWT(t, pool, other), csrf: "valid", worker: ephemeral, want: 404},
			} {
				t.Run(tc.name, func(t *testing.T) {
					before := manualCordonStateSnapshot(t, pool, tc.worker)
					req := httptest.NewRequest(method, "/api/workers/"+tc.worker.String()+"/cordon", nil)
					if tc.jwt != "" {
						req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: tc.jwt}) //nolint:gosec // Test client cookie; response cookie attributes do not apply.
					}
					if tc.csrf == "valid" {
						req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, tc.jwt))
					} else if tc.csrf != "" {
						req.Header.Set(auth.CSRFHeaderName, tc.csrf)
					}
					if tc.bearer != "" {
						req.Header.Set("Authorization", "Bearer "+tc.bearer)
					}
					rec := httptest.NewRecorder()
					router.ServeHTTP(rec, req)
					if rec.Code != tc.want {
						t.Fatalf("response = %d %s, want %d", rec.Code, rec.Body.String(), tc.want)
					}
					if tc.errText != "" {
						var body struct {
							Error string `json:"error"`
						}
						if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
							t.Fatal(err)
						}
						if body.Error != tc.errText {
							t.Fatalf("error = %q, want %q", body.Error, tc.errText)
						}
					}
					if after := manualCordonStateSnapshot(t, pool, tc.worker); after != before {
						t.Fatal("rejected request changed manual tuple or audit rows")
					}
					wantCordon := 0
					if method == http.MethodDelete && tc.worker == id {
						wantCordon = 1
					}
					manualCordonEventCounts(t, pool, tc.worker, wantCordon, 0)
				})
			}
		})
	}
	// The valid owner DELETE clears the pre-existing cordon exactly once.
	cleared := manualCordonWorkerResponse(t, bearerReq(router, http.MethodDelete, "/api/workers/"+id.String()+"/cordon", cliMintToken(t, pool, owner, clitoken.ScopeUser)), id)
	if cleared.ManualCordonedAt != nil || cleared.ManualCordonedBy != nil || cleared.ManualCordonedByLabel != nil || cleared.ManualCordonReason != nil {
		t.Fatal("valid DELETE retained manual tuple")
	}
	manualCordonEventCounts(t, pool, id, 1, 1)
	for _, worker := range []uuid.UUID{id, ephemeral} {
		var cordoned bool
		if err := pool.QueryRow(context.Background(), `SELECT manual_cordoned_at IS NOT NULL FROM workers WHERE id=$1`, worker).Scan(&cordoned); err != nil {
			t.Fatal(err)
		}
		if cordoned {
			t.Fatalf("rejected request cordoned worker %s", worker)
		}
	}
}
