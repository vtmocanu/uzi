package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// stored_files_livedb_test.go covers PRD #1909 M1 (D2/D4) from the recovery side against a live
// Postgres: recovery-archive admission counts job-file bytes against the shared stored-file
// budget, reclaims job files in the D2 order (never one attached to a live job), refuses a capture
// only when reclaiming still would not fit it (rolling the reclaim back), stamps reserved_bytes so
// a chunk stream running outside the lock is still counted, and never lets the two stores together
// exceed the budget under concurrency. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// database; run via ./e2e/run-store-it.sh.
//
// The sums are instance-wide and the live-DB packages share one database, so each test empties
// job_files and recovery_captures first: safe only under the sweep's -p 1 on a throwaway database.

type sfEnv struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	q      *store.Queries
	box    *secretbox.Box
	userID uuid.UUID
	wkr    store.Worker
	runID  uuid.UUID
	holdID uuid.UUID
	seq    int
}

func newSFEnv(t *testing.T) *sfEnv {
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
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	e := &sfEnv{ctx: ctx, pool: pool, q: store.New(pool), box: box, userID: uuid.New(), runID: uuid.New(), holdID: uuid.New()}
	e.exec(`DELETE FROM job_files`)
	e.exec(`DELETE FROM recovery_captures`)

	connID, repoID, workerID := uuid.New(), uuid.New(), uuid.New()
	e.exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, e.userID, fmt.Sprintf("sf-%s@e2e", e.userID))
	e.exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	        VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, e.userID, []byte{0x1})
	e.exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	        VALUES ($1, $2, 1, 'g/sf', 'https://forge.e2e/g/sf', 'main', true)`, repoID, connID)
	name := "w-" + workerID.String()
	e.exec(`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, $3, $4, 'online')`, workerID, e.userID, name, workerID[:])
	e.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status)
	        VALUES ($1, $2, $3, 'issue', 1, 't', 'd', 'running')`, e.runID, e.userID, repoID)
	e.exec(`INSERT INTO recovery_custody_holds (id, user_id, repo_id, run_id, generation, state, original_worker_id, original_worker_identity, live_worker_id, live_run_id)
	        VALUES ($1, $2, $3, $4, 1, 'open', $5, 'ident', $5, $4)`, e.holdID, e.userID, repoID, e.runID, workerID)
	e.wkr = store.Worker{ID: workerID, UserID: e.userID, Name: name, ProtocolCapabilities: []string{capability.RecoveryArchiveV1}}
	return e
}

func (e *sfEnv) exec(sql string, args ...any) {
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		panic(fmt.Sprintf("exec %q: %v", sql, err))
	}
}

func (e *sfEnv) recovery(budget int64) *Service {
	return New(e.q, e.pool, e.box, Limits{
		MaxBundleBytes: 1 << 20, ReadyPayloadPerOwner: 1 << 30, InstanceBytes: 1 << 30,
		MaxCapturesPerClaim: 1000, MaxCapturesPerOwner: 1000, MaxConcurrentUploads: 64, MaxConcurrentDownloads: 2,
		StoredFilesBudgetBytes: budget, RequestDeadline: time.Minute,
	}, nil)
}

func (e *sfEnv) jobFiles() *workersvc.JobFiles {
	return workersvc.NewJobFiles(e.pool, e.box, workersvc.JobFileLimits{
		InputFileMaxBytes: 1 << 30, InputsMaxFiles: 1000, InputsMaxBytes: 1 << 40,
		OutputFileMaxBytes: 1 << 30, OutputsMaxFiles: 1000, OutputsMaxBytes: 1 << 40,
		PerOwnerBytes: 1 << 40, InstanceBytes: 1 << 40, StoredFilesBudgetBytes: 1 << 40,
	}, nil)
}

// newCapture reserves a fresh capture under the open hold.
func (e *sfEnv) newCapture(t *testing.T, svc *Service) uuid.UUID {
	t.Helper()
	e.seq++
	res, err := svc.Reserve(e.ctx, e.wkr, e.runID, apitypes.RecoveryReserveRequest{IdempotencyKey: fmt.Sprintf("k%d", e.seq), SourceSha: "aaaa1111"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return uuid.MustParse(res.CaptureID)
}

func manifestOf(body []byte) apitypes.RecoveryUploadManifest {
	sum := sha256.Sum256(body)
	return apitypes.RecoveryUploadManifest{ByteSize: int64(len(body)), Checksum: hex.EncodeToString(sum[:])}
}

// jobFile stores one text file of n bytes as the owner's, in the given state, aged by created.
func (e *sfEnv) jobFile(t *testing.T, jf *workersvc.JobFiles, name string, n int, run *uuid.UUID) uuid.UUID {
	t.Helper()
	p := workersvc.ReserveParams{UserID: e.userID, Direction: workersvc.JobFileInput, DisplayName: name, DeclaredSize: int64(n)}
	if run != nil {
		p.Direction, p.RunID = workersvc.JobFileOutput, run
	}
	row, err := jf.Reserve(e.ctx, p)
	if err != nil {
		t.Fatalf("Reserve %s: %v", name, err)
	}
	if _, err := jf.Write(e.ctx, row.ID, e.userID, bytes.NewReader(bytes.Repeat([]byte("j"), n)), workersvc.WriteOptions{ContentType: "text/plain"}); err != nil {
		t.Fatalf("Write %s: %v", name, err)
	}
	return row.ID
}

func (e *sfEnv) jobState(t *testing.T, id uuid.UUID) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT state FROM job_files WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *sfEnv) captureState(t *testing.T, id uuid.UUID) (state string, reserved *int64) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx, `SELECT state, reserved_bytes FROM recovery_captures WHERE id = $1`, id).Scan(&state, &reserved); err != nil {
		t.Fatal(err)
	}
	return state, reserved
}

// TestRecoveryAdmissionCountsJobBytesLiveDB: job-file bytes count against the shared budget in
// recovery admission (a deliberate change to admission), and a job file attached to a live job is
// never reclaimed to make room. The archive is refused with ErrQuota and the job file is intact.
func TestRecoveryAdmissionCountsJobBytesLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc, jf := e.recovery(1000), e.jobFiles()
	live := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status)
	        VALUES ($1, $2, 'job', 'research', 'j', 'p', true, '{}', 'running')`, live, e.userID)
	attached := e.jobFile(t, jf, "attached.txt", 800, &live) // 800 of 1000, attached to a live job

	body := bytes.Repeat([]byte("r"), 300)
	cap1 := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, cap1, manifestOf(body), bytes.NewReader(body)); !errors.Is(err, ErrQuota) {
		t.Fatalf("300-byte archive with 200 of the budget left = %v, want ErrQuota", err)
	}
	if got := e.jobState(t, attached); got != "attached" {
		t.Fatalf("the attached job file was touched: state %q, want attached (never reclaimed)", got)
	}
	if st, _ := e.captureState(t, cap1); st != "needs_action" {
		t.Fatalf("refused capture state = %q, want needs_action (source retained)", st)
	}

	small := bytes.Repeat([]byte("s"), 200)
	cap2 := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, cap2, manifestOf(small), bytes.NewReader(small)); err != nil {
		t.Fatalf("200-byte archive into the 200 left: %v", err)
	}
	if st, _ := e.captureState(t, cap2); st != "available" {
		t.Fatalf("capture state = %q, want available", st)
	}
}

// TestRecoveryBindReclaimsJobFilesInOrderLiveDB: when only the shared budget is short, a verified
// upload reclaims job files AT BIND (expired-by-time first, then the oldest available), stops as
// soon as the archive fits, and never touches an attached, in-TTL unattached or reserved file. A
// second archive that could not fit even if every reclaimable file were freed is refused at
// ADMISSION (its headroom check, not a bind rollback: nothing is stamped, nothing is reclaimed).
func TestRecoveryBindReclaimsJobFilesInOrderLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc, jf := e.recovery(600), e.jobFiles()
	live := uuid.New()
	e.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, auto_approve, required_capabilities, status)
	        VALUES ($1, $2, 'job', 'research', 'j', 'p', true, '{}', 'running')`, live, e.userID)

	expired := e.jobFile(t, jf, "expired.txt", 100, nil) // unattached, past its expiry
	e.exec(`UPDATE job_files SET expires_at = now() - interval '1 minute' WHERE id = $1`, expired)
	oldest := e.jobFile(t, jf, "oldest.txt", 100, nil) // becomes available, oldest
	newer := e.jobFile(t, jf, "newer.txt", 100, nil)   // becomes available, newer
	for i, id := range []uuid.UUID{oldest, newer} {
		e.exec(`UPDATE job_files SET state = 'available', expires_at = now() + interval '1 day', created_at = now() - make_interval(hours => $2) WHERE id = $1`, id, 10-i)
	}
	attached := e.jobFile(t, jf, "attached.txt", 100, &live)
	fresh := e.jobFile(t, jf, "fresh.txt", 100, nil) // unattached, inside its TTL
	// 500 of 600 used. An archive of 250 needs 150 freed: expired (100) then oldest (100).

	body := bytes.Repeat([]byte("r"), 250)
	c1 := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, c1, manifestOf(body), bytes.NewReader(body)); err != nil {
		t.Fatalf("archive that fits after reclaim: %v", err)
	}
	for name, want := range map[string][2]any{
		"expired": {expired, "expired"}, "oldest": {oldest, "expired"},
		"newer": {newer, "available"}, "attached": {attached, "attached"}, "fresh": {fresh, "unattached"},
	} {
		if got := e.jobState(t, want[0].(uuid.UUID)); got != want[1].(string) {
			t.Errorf("%s job file = %q, want %q", name, got, want[1])
		}
	}
	var chunks int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM job_file_chunks WHERE file_id IN ($1, $2)`, expired, oldest).Scan(&chunks); err != nil || chunks != 0 {
		t.Fatalf("reclaimed files still hold %d chunks (err %v), want 0", chunks, err)
	}

	// Now 300 job + 250 recovery = 550 of 600. A 400-byte archive needs 350 freed, but only the
	// newer available file (100) is reclaimable: admission refuses it before anything is stamped
	// or reclaimed.
	big := bytes.Repeat([]byte("b"), 400)
	c2 := e.newCapture(t, svc)
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, c2, manifestOf(big), bytes.NewReader(big)); !errors.Is(err, ErrQuota) {
		t.Fatalf("archive that cannot fit even after reclaim = %v, want ErrQuota", err)
	}
	if got := e.jobState(t, newer); got != "available" {
		t.Fatalf("a refused archive reclaimed a job file: newer = %q, want available", got)
	}
	if st, reserved := e.captureState(t, c2); st == "uploading" || reserved != nil {
		t.Fatalf("a refused archive was stamped: state %q reserved %v, want no reservation", st, reserved)
	}
}

// TestRecoveryAdmissionStampsReservationLiveDB: admission stamps the capture 'uploading' with
// reserved_bytes = the declared size, the shared sums count it (so the stream outside the lock is
// accounted for), and a failed or stalled upload (needs_action) stops counting.
func TestRecoveryAdmissionStampsReservationLiveDB(t *testing.T) {
	e := newSFEnv(t)
	svc := e.recovery(1000)
	body := bytes.Repeat([]byte("r"), 400)
	id := e.newCapture(t, svc)

	if _, done, err := svc.admit(e.ctx, e.wkr, e.runID, id, manifestOf(body)); err != nil || done {
		t.Fatalf("admit = done %v, err %v", done, err)
	}
	st, reserved := e.captureState(t, id)
	if st != "uploading" || reserved == nil || *reserved != 400 {
		got := int64(-1)
		if reserved != nil {
			got = *reserved
		}
		t.Fatalf("after admission: state %q reserved_bytes %d (-1 = NULL), want uploading / 400", st, got)
	}
	sums, err := store.SumStoredFiles(e.ctx, e.q, e.userID, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if sums.OwnerRecoveryBytes != 400 || sums.InstanceRecoveryBytes != 400 || sums.SharedBytes() != 400 {
		t.Fatalf("sums during the stream = %+v, want the 400-byte reservation counted", sums)
	}
	// The capture's own reservation is excluded from its own admission.
	own, err := store.SumStoredFiles(e.ctx, e.q, e.userID, id)
	if err != nil || own.SharedBytes() != 0 {
		t.Fatalf("sums excluding the capture = %+v, %v; want 0", own, err)
	}

	// A concurrent second admission sees the reservation: 400 + 700 > 1000.
	other := e.newCapture(t, svc)
	huge := bytes.Repeat([]byte("h"), 700)
	if _, _, err := svc.admit(e.ctx, e.wkr, e.runID, other, manifestOf(huge)); !errors.Is(err, ErrQuota) {
		t.Fatalf("admission beside a 400-byte reservation = %v, want ErrQuota", err)
	}

	// The stream completes what admission reserved.
	if _, err := svc.stream(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(body)); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if st, _ := e.captureState(t, id); st != "available" {
		t.Fatalf("after the stream: state %q, want available", st)
	}

	// A stalled upload is flipped to needs_action by the sweep and stops counting.
	stalled := e.newCapture(t, svc)
	if _, _, err := svc.admit(e.ctx, e.wkr, e.runID, stalled, manifestOf(body)); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.SumStoredFiles(e.ctx, e.q, e.userID, uuid.Nil); got.OwnerRecoveryBytes != 800 {
		t.Fatalf("owner recovery bytes with a ready and a reserved capture = %d, want 800", got.OwnerRecoveryBytes)
	}
	e.exec(`UPDATE recovery_captures SET created_at = now() - interval '2 days' WHERE id = $1`, stalled)
	if n, err := e.q.ExpireStalledUploads(e.ctx, interval(time.Hour)); err != nil || n < 1 {
		t.Fatalf("ExpireStalledUploads = %d, %v; want the stalled capture flipped", n, err)
	}
	if got, _ := store.SumStoredFiles(e.ctx, e.q, e.userID, uuid.Nil); got.OwnerRecoveryBytes != 400 {
		t.Fatalf("owner recovery bytes after the stall = %d, want 400 (needs_action stops counting)", got.OwnerRecoveryBytes)
	}
	// A stream that lost its reservation to the sweep refuses instead of committing unaccounted bytes.
	if _, err := svc.stream(e.ctx, e.wkr, e.runID, stalled, manifestOf(body), bytes.NewReader(body)); !errors.Is(err, ErrBusy) {
		t.Fatalf("stream after the reservation was swept = %v, want ErrBusy", err)
	}
	// A retry re-admits and completes.
	if _, err := svc.Upload(e.ctx, e.wkr, e.runID, stalled, manifestOf(body), bytes.NewReader(body)); err != nil {
		t.Fatalf("retry of a needs_action capture: %v", err)
	}
}

// TestStoredFilesCrossStoreNeverExceedsBudgetLiveDB: a job-file reservation and a recovery archive
// upload that each fit the shared budget alone but not together, started at once, admit exactly
// one, so the combined total never exceeds the budget. Repeated because the race window is short.
//
// CALIBRATION: delete the store.LockStoredFiles call from recovery.Service.admit (or from
// workersvc.JobFiles.Reserve); the combined total then exceeds the budget in some round and this
// test goes red.
func TestStoredFilesCrossStoreNeverExceedsBudgetLiveDB(t *testing.T) {
	const budget, size, perStore, rounds = 1000, 600, 4, 12
	e := newSFEnv(t)
	svc := e.recovery(budget)
	// The job store's own limits stay wide; only the shared budget binds, and JobFiles.Reserve
	// reads it from its limits, so build one whose budget matches.
	jf := workersvc.NewJobFiles(e.pool, e.box, workersvc.JobFileLimits{
		InputFileMaxBytes: 1 << 30, InputsMaxFiles: 1000, InputsMaxBytes: 1 << 40,
		OutputFileMaxBytes: 1 << 30, OutputsMaxFiles: 1000, OutputsMaxBytes: 1 << 40,
		PerOwnerBytes: 1 << 40, InstanceBytes: 1 << 40, StoredFilesBudgetBytes: budget,
	}, nil)

	for round := 0; round < rounds; round++ {
		e.exec(`DELETE FROM job_files`)
		e.exec(`DELETE FROM recovery_captures`)
		body := bytes.Repeat([]byte("r"), size)
		captures := make([]uuid.UUID, perStore)
		for i := range captures {
			captures[i] = e.newCapture(t, svc)
		}

		var (
			wg       sync.WaitGroup
			mu       sync.Mutex
			admitted int
		)
		start := make(chan struct{})
		for i := 0; i < perStore; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				row, err := jf.Reserve(e.ctx, workersvc.ReserveParams{UserID: e.userID, Direction: workersvc.JobFileInput, DisplayName: "j.txt", DeclaredSize: size})
				if err != nil {
					return
				}
				if _, err := jf.Write(e.ctx, row.ID, e.userID, strings.NewReader(string(body)), workersvc.WriteOptions{ContentType: "text/plain"}); err == nil {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}()
			go func(id uuid.UUID) {
				defer wg.Done()
				<-start
				if _, err := svc.Upload(e.ctx, e.wkr, e.runID, id, manifestOf(body), bytes.NewReader(body)); err == nil {
					mu.Lock()
					admitted++
					mu.Unlock()
				}
			}(captures[i])
		}
		close(start)
		wg.Wait()

		sums, err := store.SumStoredFiles(e.ctx, e.q, e.userID, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		if sums.SharedBytes() > budget {
			t.Fatalf("round %d: job %d + recovery %d = %d bytes stored, over the %d budget", round, sums.InstanceJobBytes, sums.InstanceRecoveryBytes, sums.SharedBytes(), budget)
		}
		if admitted != 1 {
			t.Fatalf("round %d: %d uploads admitted across both stores, want exactly 1 (600+600 > 1000)", round, admitted)
		}
	}
}

func interval(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}
