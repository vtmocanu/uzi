package handler

// Issue #2545 live-DB proofs for the OWNER held-publication routes, driven through the REAL
// h.Routes() router so the auth MOUNT is load-bearing: GET /api/runs/{id}/held-publications and
// POST /api/runs/{id}/held-publications/expire sit in the RequireUser group (cookie + CSRF, or a
// uzc_ Bearer), refuse a cookie without CSRF on the POST and a worker (uzw_) token, and answer a
// foreign owner or admin with 404.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const (
	heldHTTPTip = "3333333333333333333333333333333333333333"
	heldHTTPCov = "5555555555555555555555555555555555555555555555555555555555555555"
)

// heldSeedPublication seeds a failed run with a guarded hold and one held publication, and walks
// it to created (acknowledge false) or acknowledged (acknowledge true) through the real queries.
func heldSeedPublication(t *testing.T, pool *pgxpool.Pool, owner, conn uuid.UUID, projectID int64, acknowledge bool) (run, pub uuid.UUID) {
	t.Helper()
	repo := rmSeedRepo(t, pool, conn, projectID, true)
	run = rmSeedRun(t, pool, owner, repo, "running")
	return run, heldSeedOnRun(t, pool, owner, repo, run, acknowledge)
}

// heldSeedOnRun fails run (owned by owner, on repo) and gives it a guarded hold and a held
// publication walked to created or acknowledged. It returns the publication id.
func heldSeedOnRun(t *testing.T, pool *pgxpool.Pool, owner, repo, run uuid.UUID, acknowledge bool) uuid.UUID {
	t.Helper()
	ctx := t.Context()
	q := store.New(pool)
	worker := custodySeedWorker(t, pool, owner)
	hold := uuid.New()
	cliMustExec(t, pool, `UPDATE runs SET worker_id=$2 WHERE id=$1`, run, worker)
	cliMustExec(t, pool, `INSERT INTO recovery_custody_holds
		(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
		VALUES ($1,$2,$3,$4,1,'open',$5,'held-ident',$5,$4,true)`, hold, owner, repo, run, worker)
	cliMustExec(t, pool, `UPDATE runs SET status='failed', fail_origin='agent_failure', claim_generation=1 WHERE id=$1`, run)
	row, err := q.UpsertHeldPublication(ctx, store.UpsertHeldPublicationParams{
		RunID: run, Generation: 1, HoldID: hold, UserID: owner, RepoID: repo, WorkerID: worker,
		Ref: "refs/uzi-held/" + run.String() + "/1", Tip: heldHTTPTip, CoverageDigest: heldHTTPCov,
	})
	if err != nil {
		t.Fatalf("upsert publication: %v", err)
	}
	if _, err := q.MarkHeldCreateInvoked(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.RecordHeldCreateOutcome(ctx, store.RecordHeldCreateOutcomeParams{State: "created", ID: row.ID}); err != nil {
		t.Fatal(err)
	}
	if acknowledge {
		if n, err := q.ReleaseHeldPublicationHold(ctx, store.ReleaseHeldPublicationHoldParams{
			ID: hold, RunID: run, UserID: owner, Generation: 1, WorkerID: worker,
			PublicationID: row.ID, Tip: heldHTTPTip, CoverageDigest: heldHTTPCov,
		}); err != nil || n != 1 {
			t.Fatalf("release hold: rows=%d err=%v", n, err)
		}
		if _, err := q.AcknowledgeHeldPublication(ctx, store.AcknowledgeHeldPublicationParams{ID: row.ID, RetentionSeconds: 7 * 24 * 3600}); err != nil {
			t.Fatal(err)
		}
	}
	return row.ID
}

// heldCookieReq is a browser request for jwt; withCSRF false omits the CSRF header.
func heldCookieReq(t *testing.T, router http.Handler, method, path, jwt string, withCSRF bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.AddCookie(&http.Cookie{Name: auth.AuthCookieName, Value: jwt}) //nolint:gosec // G124: test-only client cookie on an httptest request.
	if withCSRF {
		req.Header.Set(auth.CSRFHeaderName, cliCSRFHeader(t, jwt))
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestHeldPublicationsOwnerRoutesLiveDB(t *testing.T) {
	_, router, pool := cliLiveDB(t)
	owner := cliSeedUser(t, pool, false)
	uzc := cliMintToken(t, pool, owner, clitoken.ScopeUser)
	jwt := cliMintJWT(t, pool, owner)
	conn := rmSeedConn(t, pool, owner)
	run, pub := heldSeedPublication(t, pool, owner, conn, 4301, true)
	listPath := fmt.Sprintf("/api/runs/%s/held-publications", run)
	expirePath := listPath + "/expire"

	// A worker join token, a foreign user and a foreign admin.
	wkrTok, wkrHash, err := jointoken.Generate()
	if err != nil {
		t.Fatal(err)
	}
	cliMustExec(t, pool, `INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1,$2,'held-owner-worker',$3,'online')`,
		uuid.New(), owner, wkrHash)
	foreign := cliSeedUser(t, pool, false)
	foreignTok := cliMintToken(t, pool, foreign, clitoken.ScopeUser)
	foreignJWT := cliMintJWT(t, pool, foreign)
	admin := cliSeedUser(t, pool, true)
	adminJWT := cliMintJWT(t, pool, admin)

	decodeList := func(rec *httptest.ResponseRecorder) apitypes.HeldPublicationsDTO {
		t.Helper()
		var got apitypes.HeldPublicationsDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v\n%s", err, rec.Body.String())
		}
		return got
	}

	t.Run("list", func(t *testing.T) {
		for who, rec := range map[string]*httptest.ResponseRecorder{
			"cookie+csrf": heldCookieReq(t, router, http.MethodGet, listPath, jwt, true),
			"uzc bearer":  bearerReq(router, http.MethodGet, listPath, uzc),
		} {
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: GET = %d, want 200\n%s", who, rec.Code, rec.Body.String())
			}
			got := decodeList(rec)
			if len(got.Publications) != 1 || got.Publications[0].ID != pub.String() || got.Publications[0].State != "acknowledged" ||
				got.Publications[0].Ref != "refs/uzi-held/"+run.String()+"/1" || got.Publications[0].Tip != heldHTTPTip ||
				got.Publications[0].ExpiresAt == nil || got.Publications[0].AcknowledgedAt == nil {
				t.Fatalf("%s: publications = %+v", who, got.Publications)
			}
		}
		if rec := bearerReq(router, http.MethodGet, listPath, "uzw_"+wkrTok); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("a worker token reached the owner list: %d", rec.Code)
		}
		if rec := bearerReq(router, http.MethodGet, listPath, wkrTok); rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("a bare worker token reached the owner list: %d", rec.Code)
		}
		if rec := bearerReq(router, http.MethodGet, listPath, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("no credential = %d, want 401", rec.Code)
		}
		for who, rec := range map[string]*httptest.ResponseRecorder{
			"foreign bearer": bearerReq(router, http.MethodGet, listPath, foreignTok),
			"foreign cookie": heldCookieReq(t, router, http.MethodGet, listPath, foreignJWT, true),
			"admin cookie":   heldCookieReq(t, router, http.MethodGet, listPath, adminJWT, true),
		} {
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s: GET = %d, want 404\n%s", who, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("expire", func(t *testing.T) {
		if rec := heldCookieReq(t, router, http.MethodPost, expirePath, jwt, false); rec.Code != http.StatusForbidden {
			t.Fatalf("cookie without CSRF = %d, want 403\n%s", rec.Code, rec.Body.String())
		}
		for who, rec := range map[string]*httptest.ResponseRecorder{
			"worker token":   bearerReq(router, http.MethodPost, expirePath, "uzw_"+wkrTok),
			"no credential":  bearerReq(router, http.MethodPost, expirePath, ""),
			"foreign bearer": bearerReq(router, http.MethodPost, expirePath, foreignTok),
			"foreign cookie": heldCookieReq(t, router, http.MethodPost, expirePath, foreignJWT, true),
			"admin cookie":   heldCookieReq(t, router, http.MethodPost, expirePath, adminJWT, true),
		} {
			want := http.StatusNotFound
			if who == "worker token" || who == "no credential" {
				want = http.StatusUnauthorized
			}
			if rec.Code != want {
				t.Fatalf("%s: POST = %d, want %d\n%s", who, rec.Code, want, rec.Body.String())
			}
		}
		var expiresBefore time.Time
		if err := pool.QueryRow(t.Context(), `SELECT expires_at FROM run_held_publications WHERE id=$1`, pub).Scan(&expiresBefore); err != nil {
			t.Fatal(err)
		}
		if !expiresBefore.After(time.Now().Add(24 * time.Hour)) {
			t.Fatalf("a refused request changed expires_at to %v", expiresBefore)
		}

		rec := heldCookieReq(t, router, http.MethodPost, expirePath, jwt, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("cookie+csrf POST = %d, want 200\n%s", rec.Code, rec.Body.String())
		}
		var got apitypes.HeldPublicationsExpireDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Expired != 1 || len(got.Publications) != 1 || got.Publications[0].OwnerExpiredAt == nil ||
			got.Publications[0].ExpiresAt == nil || got.Publications[0].ExpiresAt.After(time.Now().Add(time.Minute)) {
			t.Fatalf("expire = %+v", got)
		}
		// Expiry deletes nothing itself: the row is still acknowledged and now due.
		var state string
		if err := pool.QueryRow(t.Context(), `SELECT state FROM run_held_publications WHERE id=$1`, pub).Scan(&state); err != nil || state != "acknowledged" {
			t.Fatalf("state after expire = %q, %v", state, err)
		}
		if rec := bearerReq(router, http.MethodPost, expirePath, uzc); rec.Code != http.StatusOK {
			t.Fatalf("uzc bearer POST = %d, want 200\n%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("expire touches only acknowledged rows", func(t *testing.T) {
		run2, _ := heldSeedPublication(t, pool, owner, conn, 4302, false)
		path := fmt.Sprintf("/api/runs/%s/held-publications/expire", run2)
		rec := bearerReq(router, http.MethodPost, path, uzc)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST = %d\n%s", rec.Code, rec.Body.String())
		}
		var got apitypes.HeldPublicationsExpireDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || got.Expired != 0 || got.Publications == nil {
			t.Fatalf("expire of a created (unacknowledged) publication = %+v, %v; want 0 and an empty, non-null list", got, err)
		}
	})
}
