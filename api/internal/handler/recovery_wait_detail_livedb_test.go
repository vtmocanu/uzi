package handler

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// parkAdoptionFailed moves the recoveryEnv run into a retained_recovery_adoption_failed
// recovery_wait park carrying detail (nil = unclassified NULL).
func (e *recoveryEnv) parkAdoptionFailed(detail *string) {
	e.t.Helper()
	e.mustExec(`UPDATE runs SET status='recovery_wait', worker_id=$2, claim_generation=7,
		claim_released_at=now(), recovery_wait_cause='retained_recovery_adoption_failed',
		recovery_wait_detail=$3, recovery_retry_not_before=NULL, retained_park_count=1,
		worker_recovery_episode=0, started_at=now()-interval '10 minutes',
		budget_wall_seconds=3600, status_since=now()-interval '2 minutes' WHERE id=$1`,
		e.run, e.worker.ID, detail)
}

type resumeSnapshot struct {
	Status          string
	ClaimGeneration int64
	Episode         int64
	ParkCount       int32
	Cause, Detail   string
	Inputs          int
	Holds           string
}

func (e *recoveryEnv) resumeSnapshot() resumeSnapshot {
	e.t.Helper()
	var s resumeSnapshot
	if err := e.pool.QueryRow(e.ctx, `SELECT status, claim_generation, worker_recovery_episode, retained_park_count,
		COALESCE(recovery_wait_cause,''), COALESCE(recovery_wait_detail,''),
		(SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='resume')
		FROM runs WHERE id=$1`, e.run).Scan(&s.Status, &s.ClaimGeneration, &s.Episode, &s.ParkCount, &s.Cause, &s.Detail, &s.Inputs); err != nil {
		e.t.Fatal(err)
	}
	holds, err := e.service(recoveryTestLimits()).ListHoldsForOwner(e.ctx, e.user, false)
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := json.Marshal(holds)
	s.Holds = string(b)
	return s
}

// TestResumeRecoveryWaitDetailFenceLiveDB executes ResumeWorkerRecoveryEpisode and the Resume
// handler against a real database: an active secret remediation is refused with a 409 before any
// write (so no claim, generation, hold or audit row), and an available capture stays downloadable
// byte-identically around the refusal; NULL and unknown details still resume.
func TestResumeRecoveryWaitDetailFenceLiveDB(t *testing.T) {
	e := newRecoveryEnv(t)
	svc := e.service(recoveryTestLimits())
	data := make([]byte, 1<<20+12345)
	for i := range data {
		data[i] = byte((i*17 + 3) % 251)
	}
	res := e.reserve(svc, e.worker, "k-detail", "dddd4444")
	if _, err := svc.Upload(e.ctx, e.worker, e.run, uuid.MustParse(res.CaptureID), manifestFor(data), cappedBody(data, e.svcCap())); err != nil {
		t.Fatalf("upload: %v", err)
	}
	h := e.handler()
	h.cfg.RunTimeout = time.Hour
	active := workersvc.RecoveryDetailSecretRemediationActive
	unknown := workersvc.RecoveryDetailRemediationProofUnknown
	download := func() []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		if err := svc.Download(e.ctx, rec, e.user, e.run, uuid.MustParse(res.CaptureID)); err != nil {
			t.Fatalf("download: %v", err)
		}
		return rec.Body.Bytes()
	}

	// The owner's archive list (the service call behind GET recovery archives) must be unchanged by the refusal.
	archiveList := func() any {
		t.Helper()
		summary, err := svc.Summary(e.ctx, e.user, e.run)
		if err != nil {
			t.Fatalf("archive list: %v", err)
		}
		return summary
	}

	e.parkAdoptionFailed(&active)
	listBefore := archiveList()
	if !bytes.Equal(download(), data) {
		t.Fatal("pre-refusal download is not byte-identical")
	}
	before := e.resumeSnapshot()

	// Store level: the SQL fence itself matches no row.
	_, err := store.New(e.pool).ResumeWorkerRecoveryEpisode(e.ctx, store.ResumeWorkerRecoveryEpisodeParams{
		ID: e.run, UserID: e.user, GlobalTimeoutSeconds: 3600,
	})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("ResumeWorkerRecoveryEpisode on an active remediation = %v, want ErrNoRows", err)
	}
	if got := e.resumeSnapshot(); got != before {
		t.Fatalf("fenced store call changed state: %+v -> %+v", before, got)
	}

	// Handler level: 409 naming the run id, nothing written.
	rec := recoveryResumeRequest(h, e.run, e.user)
	if rec.Code != http.StatusConflict {
		t.Fatalf("Resume status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "uzi run export "+e.run.String()) || !strings.Contains(rec.Body.String(), "active secret-scan remediation") {
		t.Fatalf("409 body lacks the guidance: %s", rec.Body.String())
	}
	if got := e.resumeSnapshot(); got != before {
		t.Fatalf("refused Resume changed state: %+v -> %+v", before, got)
	}
	if !bytes.Equal(download(), data) {
		t.Fatal("post-refusal download is not byte-identical")
	}
	if listAfter := archiveList(); !reflect.DeepEqual(listBefore, listAfter) {
		t.Fatalf("refused Resume changed the archive list: %+v -> %+v", listBefore, listAfter)
	}

	// Absent and unknown classifications never refuse; they resume as before.
	for name, detail := range map[string]*string{"null": nil, "unknown": &unknown} {
		e.parkAdoptionFailed(detail)
		rec := recoveryResumeRequest(h, e.run, e.user)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: Resume status = %d, want 200; body=%s", name, rec.Code, rec.Body.String())
		}
		after := e.resumeSnapshot()
		if after.Status != "queued" || after.Episode != 1 || after.Detail != "" || after.Cause != "" {
			t.Fatalf("%s: Resume did not requeue and clear the park: %+v", name, after)
		}
	}
}

// TestWorkerParkRecoveryDetailLiveDB drives the real /state handler: a detail on an adoption-failed
// park is stored and survives an idempotent replay, a park without one stores NULL, and a detail
// riding another cause is a 400 that writes nothing.
func TestWorkerParkRecoveryDetailLiveDB(t *testing.T) {
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
	q := store.New(pool)
	box := newHandlerTestBox(t)
	wsvc := workersvc.New(q, box, workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), RunRetainedRecoveryMaxParks: 3})
	wsvc.SetTxBeginner(pool)
	h := &Handler{pool: pool, q: q, box: box, wsvc: wsvc}

	userID, connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	uniq := func(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8]) >> 1) }
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("detail-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, uniq(repoID), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'detail', $3)`,
		workerID, userID, append([]byte("detail-"), workerID[:]...))
	newRun := func() uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, claim_generation, retained_park_count, started_at, status_since)
		      VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5, 3, 0, now() - interval '10 minutes', now())`,
			id, userID, repoID, uniq(id), workerID)
		return id
	}
	post := func(runID uuid.UUID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/state", bytes.NewReader([]byte(body)))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		reqCtx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: userID}), chi.RouteCtxKey, rctx)
		rec := httptest.NewRecorder()
		h.WorkerRunState(rec, req.WithContext(reqCtx))
		return rec
	}
	read := func(id uuid.UUID) store.Run {
		t.Helper()
		r, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	const adoption = `"recovery_cause":"retained_recovery_adoption_failed","claim_generation":3`

	withDetail := newRun()
	if rec := post(withDetail, `{"status":"recovery_wait",`+adoption+`,"recovery_detail":"secret_remediation_active"}`); rec.Code != http.StatusOK {
		t.Fatalf("park with detail = %d; body=%s", rec.Code, rec.Body.String())
	}
	if r := read(withDetail); r.Status != "recovery_wait" || r.RecoveryWaitDetail.String != "secret_remediation_active" || !r.RecoveryWaitDetail.Valid {
		t.Fatalf("detail not stored: status=%s detail=%+v", r.Status, r.RecoveryWaitDetail)
	}
	// An idempotent replay of the same park, even one carrying another value, keeps the first.
	// The released claim makes the replay a stale_claim 409; what matters is that nothing is rewritten.
	if rec := post(withDetail, `{"status":"recovery_wait",`+adoption+`,"recovery_detail":"remediation_proof_unknown"}`); rec.Code != http.StatusConflict {
		t.Fatalf("replayed park = %d, want 409; body=%.200s", rec.Code, rec.Body.String())
	}
	if r := read(withDetail); r.RecoveryWaitDetail.String != "secret_remediation_active" || r.RecoveryWaitCount != 1 {
		t.Fatalf("replay rewrote the park: detail=%+v wait_count=%d", r.RecoveryWaitDetail, r.RecoveryWaitCount)
	}

	plain := newRun()
	if rec := post(plain, `{"status":"recovery_wait",`+adoption+`}`); rec.Code != http.StatusOK {
		t.Fatalf("park without detail = %d; body=%s", rec.Code, rec.Body.String())
	}
	if r := read(plain); r.Status != "recovery_wait" || r.RecoveryWaitDetail.Valid {
		t.Fatalf("absent detail must store NULL: status=%s detail=%+v", r.Status, r.RecoveryWaitDetail)
	}

	for name, body := range map[string]string{
		"exhausted cause": `{"status":"recovery_wait","recovery_cause":"retained_recovery_exhausted","claim_generation":3,"recovery_detail":"secret_remediation_active"}`,
		"non-enum value":  `{"status":"recovery_wait",` + adoption + `,"recovery_detail":"anything_else"}`,
		"no generation":   `{"status":"recovery_wait","recovery_cause":"retained_recovery_adoption_failed","recovery_detail":"remediation_proof_unknown"}`,
	} {
		id := newRun()
		if rec := post(id, body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400; body=%s", name, rec.Code, rec.Body.String())
		}
		if r := read(id); r.Status != "running" || r.RecoveryWaitDetail.Valid || r.ClaimReleasedAt.Valid || r.RetainedParkCount != 0 {
			t.Fatalf("%s: refused request wrote: status=%s detail=%+v", name, r.Status, r.RecoveryWaitDetail)
		}
	}
}
