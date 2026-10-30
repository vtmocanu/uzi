package recovery

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only this test's database sees the committed function replacement. A SET LOCAL
// or uncommitted replacement would not exercise the service's pooled connections.
// No test here is parallel; the ordinary store-IT packages also run with -p 1.
func crossRunRolloutDatabase(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	name := "cross_run_rollout_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Errorf("drop owned rollout database: %v", err)
		}
	})
	ownedDSN := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		// A query dbname would override the path.
		q := u.Query()
		q.Del("dbname")
		u.RawQuery = q.Encode()
		ownedDSN = u.String()
	}
	owned, err := store.OpenPool(ctx, ownedDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer owned.Close()
	initialization, err := os.ReadFile("../../../e2e/test-database-marker.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owned.Exec(ctx, string(initialization)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UZI_TEST_DATABASE_URL", ownedDSN)
}

// Retrieve the exact prior function, not a handwritten approximation and not a
// goose Down: Down correctly refuses once witness bindings exist.
func crossRunOldGuard(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("../store/migrations/00338_merged_predecessor_discard.sql")
	if err != nil {
		t.Fatal(err)
	}
	const start = "CREATE FUNCTION merged_predecessor_discard_guard()"
	_, tail, ok := strings.Cut(string(source), start)
	if !ok {
		t.Fatal("00338 universal guard absent")
	}
	definition, _, ok := strings.Cut(tail, "$$;")
	if !ok {
		t.Fatal("00338 universal guard terminator absent")
	}
	return "CREATE OR REPLACE FUNCTION merged_predecessor_discard_guard()" + definition + "$$;"
}

func TestCrossRunRolloutLegacyWriterLiveDB(t *testing.T) {
	// The real pre-cross-run service writer still emits its legacy audit under
	// the new guard; the existing reader and its no-FINAL semantics are unchanged.
	e, s := newMergedPredecessorEnv(t)
	if n, err := e.svc.DiscardMergedPredecessorHolds(e.ctx, s); err != nil || n != 1 {
		t.Fatalf("old API/new guard must retain legacy writes: %d, %v", n, err)
	}
	var legacy bool
	if err := e.pool.QueryRow(e.ctx, `SELECT state='discarded'
		AND release_evidence='merged_predecessor_discard'
		AND NOT (merged_predecessor_discard_binding ? 'basis')
		AND merged_predecessor_discard_binding->>'run_id'=$2
		FROM recovery_custody_holds WHERE id=$1`, e.hold, e.run.String()).Scan(&legacy); err != nil || !legacy {
		t.Fatalf("legacy audit must survive the new guard: %v, %v", legacy, err)
	}
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || out == nil || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" || out.FinalReceipt != nil {
		t.Fatalf("legacy observation changed: %+v, %v", out, err)
	}
}

func TestCrossRunRolloutOldGuardRefusalAndReaderLiveDB(t *testing.T) {
	crossRunRolloutDatabase(t)
	e, w, b := newCrossRunBindingEnv(t, 3, 1)
	a, s := crossRunInputs(t, e, w, b)
	partial := e.reserve("rollout-partial")
	e.exec("INSERT INTO recovery_capture_chunks(capture_id,chunk_index,length,sealed) VALUES($1,0,7,$2)", partial, []byte("partial"))
	before := mergedWorkSnapshot(t, e)
	calls := 0
	e.svc.SetCustodySettledHook(func(uuid.UUID) { calls++ })
	var currentGuard string
	if err := e.pool.QueryRow(e.ctx, "SELECT pg_get_functiondef('merged_predecessor_discard_guard()'::regprocedure)").Scan(&currentGuard); err != nil {
		t.Fatal(err)
	}
	// Registered after fixture pool cleanup, so restoration runs first even
	// after Fatal. All replacement statements commit in this owned DB only.
	t.Cleanup(func() {
		if _, err := e.pool.Exec(e.ctx, currentGuard); err != nil {
			t.Errorf("restore current guard: %v", err)
		}
	})
	oldGuard := crossRunOldGuard(t)
	e.exec(oldGuard)
	n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s)
	var p *pgconn.PgError
	if n != 0 || !errors.As(err, &p) || p.Message != "merged predecessor binding mismatch" {
		t.Errorf("new API/old guard must refuse at the old binding guard: changed=%d err=%v", n, err)
	}
	if calls != 0 || !bytes.Equal(before, mergedWorkSnapshot(t, e)) {
		t.Fatal("old guard refusal must leave holds, captures, chunks and hooks unchanged")
	}
	e.exec(currentGuard)
	if n, err := e.svc.DiscardCrossRunHolds(e.ctx, a, s); err != nil || n != 1 || calls != 1 {
		t.Fatalf("restored guard must permit real service settlement: changed=%d err=%v hooks=%d", n, err, calls)
	}
	capture, err := mergedCapture(t, e, partial)
	chunks, chunkErr := e.q.ListCaptureChunks(e.ctx, partial)
	if err != nil || chunkErr != nil || capture.State != "discarded" || len(chunks) != 0 {
		t.Fatalf("restored service must settle partial capture: state=%s err=%v chunks=%d err=%v", capture.State, err, len(chunks), chunkErr)
	}
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryHeldPublicationV2)
	out, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || out == nil || out.State != "discarded" || out.ReleaseEvidence != "merged_predecessor_discard" || out.FinalReceipt != nil || out.CompletedPublicationReceipt != nil {
		t.Fatalf("new binding must be observable: %+v, %v", out, err)
	}
	closed := mergedWorkSnapshot(t, e)
	e.exec(oldGuard)
	// API rollback seam: call the UNCHANGED pre-cross-run ObserveExactHold
	// implementation and SQL against the real service's new closed binding,
	// with the old write guard installed too. No substitute reader or fake Down.
	rollback, err := e.svc.ObserveExactHold(e.ctx, e.w, e.run, 1)
	if err != nil || !reflect.DeepEqual(out, rollback) {
		t.Fatalf("old API reader must observe new closed binding unchanged: %+v, %v", rollback, err)
	}
	if calls != 1 || !bytes.Equal(closed, mergedWorkSnapshot(t, e)) {
		t.Fatal("rollback observation must leave custody and hooks unchanged")
	}
}
