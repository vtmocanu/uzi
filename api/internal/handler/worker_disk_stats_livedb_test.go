package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #837 M1 — the disk-stats persistence acceptance test: a heartbeat carrying the
// four stats_disk_* numbers is validated, written to the workers row, and surfaces on
// the WorkerDTO through BOTH mapper sites — the get-by-id/heartbeat DTO
// (workerDTOFromWorker, on the heartbeat response) AND the list DTO (workerDTOFromRow,
// via ListWorkersByUser). Exercising both is the point: a bug that wires the columns
// into only one mapper would pass a single-site test.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package (…LiveDB) so this auto-runs in CI's
// test-api-store-it job with no workflow edit.
func TestWorkerDiskStatsRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
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
	h := &Handler{
		pool:      pool,
		q:         q,
		box:       box,
		wsvc:      workersvc.New(q, box, workersvc.Params{}),
		version:   "dev",
		now:       time.Now,
		startedAt: time.Now(),
	}
	noLimit := mw.NewLimiter(1000, time.Minute, nil)
	srv := h.WorkerRoutes(noLimit)

	owner := uuid.New()
	workerID := uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		owner, fmt.Sprintf("disk-owner-%s@e2e", uuid.NewString()[:8]))

	token, hash, err := jointoken.Generate()
	if err != nil {
		t.Fatalf("generate join token: %v", err)
	}
	mustExecT(ctx, t, pool,
		`INSERT INTO workers (id, user_id, name, token_hash, status) VALUES ($1, $2, 'disk-worker', $3, 'online')`,
		workerID, owner, hash)

	// (1) heartbeat with both volumes' used/total → the response is the heartbeat DTO,
	// built by workerDTOFromWorker. Distinct byte values per field so a used↔total or a
	// nix↔data mixup fails.
	const (
		nixUsed   = int64(3_072_000)
		nixTotal  = int64(4_096_000)
		dataUsed  = int64(2_048_000)
		dataTotal = int64(8_192_000)
	)
	body := fmt.Sprintf(`{"version":"1","stats":{"mem_bytes":100,"source":"cgroup",`+
		`"disk_nix_bytes":%d,"disk_nix_total_bytes":%d,`+
		`"disk_data_bytes":%d,"disk_data_total_bytes":%d}}`, nixUsed, nixTotal, dataUsed, dataTotal)

	req := httptest.NewRequest(http.MethodPost, "/api/worker/heartbeat", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}

	var hbResp struct {
		Worker apitypes.WorkerDTO `json:"worker"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &hbResp); err != nil {
		t.Fatalf("decode heartbeat response: %v\nbody: %s", err, rec.Body.String())
	}
	// workerDTOFromWorker (heartbeat/get-by-id mapper) site.
	assertDiskDTO(t, "workerDTOFromWorker (heartbeat response)", hbResp.Worker, nixUsed, nixTotal, dataUsed, dataTotal)

	// (2) list DTO — ListWorkersByUser scans the persisted row (real DB round-trip of the
	// new columns) and workerDTOFromRow maps it. This is the SECOND mapper site.
	rows, err := q.ListWorkersByUser(ctx, owner)
	if err != nil {
		t.Fatalf("ListWorkersByUser: %v", err)
	}
	var found bool
	for _, row := range rows {
		if row.ID != workerID {
			continue
		}
		found = true
		dto := workerDTOFromRow(row, "", "", time.Now(), time.Now())
		assertDiskDTO(t, "workerDTOFromRow (list)", dto, nixUsed, nixTotal, dataUsed, dataTotal)
	}
	if !found {
		t.Fatalf("worker %s not returned by ListWorkersByUser", workerID)
	}
}

func assertDiskDTO(t *testing.T, site string, dto apitypes.WorkerDTO, nixUsed, nixTotal, dataUsed, dataTotal int64) {
	t.Helper()
	check := func(name string, got *int64, want int64) {
		if got == nil {
			t.Fatalf("%s: %s is nil, want %d", site, name, want)
		}
		if *got != want {
			t.Fatalf("%s: %s = %d, want %d", site, name, *got, want)
		}
	}
	check("StatsDiskNixBytes", dto.StatsDiskNixBytes, nixUsed)
	check("StatsDiskNixTotalBytes", dto.StatsDiskNixTotalBytes, nixTotal)
	check("StatsDiskDataBytes", dto.StatsDiskDataBytes, dataUsed)
	check("StatsDiskDataTotalBytes", dto.StatsDiskDataTotalBytes, dataTotal)
}
