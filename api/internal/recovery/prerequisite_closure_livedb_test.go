package recovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// isolateClosureUploadDatabase registers the drop before newInventoryEnv opens
// its pool, so LIFO cleanup closes that pool before dropping the database.
func isolateClosureUploadDatabase(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse closure database DSN: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect closure database admin: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := admin.Close(ctx); err != nil {
			t.Errorf("close closure database admin: %v", err)
		}
	})
	name := "closure_upload_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("create closure database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+ident); err != nil {
			t.Errorf("drop closure database: %v", err)
		}
	})
	childDSN := dsn + " dbname='" + name + "'"
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("parse closure database URL: %v", err)
		}
		u.Path, u.RawPath = "/"+name, ""
		query := u.Query()
		query.Set("dbname", name)
		u.RawQuery = query.Encode()
		childDSN = u.String()
	}
	t.Setenv("UZI_TEST_DATABASE_URL", childDSN)
}

func closureUploadEnv(t *testing.T) *inventoryEnv {
	t.Helper()
	isolateClosureUploadDatabase(t)
	e := newInventoryEnv(t)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryPrerequisiteClosureV1)
	e.exec("UPDATE forge_connections SET base_url='https://github.com' WHERE user_id=$1", e.w.UserID)
	e.exec("UPDATE repos SET web_url='https://github.com/g/r' WHERE id=(SELECT repo_id FROM runs WHERE id=$1)", e.run)
	e.svc.SetPrerequisiteClosure([]string{"github"}, nil)
	return e
}
func closureManifest() ([]byte, apitypes.RecoveryUploadManifest) {
	body := []byte("thin archive declaration")
	m := manifestOf(body)
	m.PrerequisiteShas = []string{strings.Repeat("b", 40)}
	return body, m
}
func closureCount(t *testing.T, e *inventoryEnv, id uuid.UUID, want int) {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx, "SELECT count(*) FROM recovery_prerequisite_closures WHERE capture_id=$1", id).Scan(&n); err != nil || n != want {
		t.Fatalf("closure count %d want %d: %v", n, want, err)
	}
}

func TestRecoveryPrerequisiteClosureUploadFixtureIsolationLiveDB(t *testing.T) {
	originalDSN := os.Getenv("UZI_TEST_DATABASE_URL")
	if originalDSN == "" {
		t.Skip("UZI_TEST_DATABASE_URL unset; run ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	shared, err := pgx.Connect(ctx, originalDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := shared.Close(ctx); err != nil {
			t.Errorf("close shared database connection: %v", err)
		}
	}()
	var originalDatabase string
	if err := shared.QueryRow(ctx, "SELECT current_database()").Scan(&originalDatabase); err != nil {
		t.Fatal(err)
	}
	t.Run("child-upload", func(t *testing.T) {
		e := closureUploadEnv(t)
		var childDatabase string
		if err := e.pool.QueryRow(e.ctx, "SELECT current_database()").Scan(&childDatabase); err != nil {
			t.Fatal(err)
		}
		if childDatabase == originalDatabase {
			t.Fatalf("closure fixture reused shared database %q", originalDatabase)
		}
		id := e.reserve("isolated-upload")
		body, manifest := closureManifest()
		ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, manifest, bytes.NewReader(body))
		if err != nil || ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.State != "pending" {
			t.Fatalf("isolated upload: %+v %v", ack, err)
		}
		closureCount(t, e, id, 1)
		e.bytes(id, body)
		// Distinct actual database identities establish that the shared database
		// cannot observe this capture, even when it has not been migrated.
	})
	if got := os.Getenv("UZI_TEST_DATABASE_URL"); got != originalDSN {
		t.Fatal("closure fixture did not restore the parent database environment")
	}
}

func TestRecoveryPrerequisiteClosureUploadRollbackLiveDB(t *testing.T) {
	e := closureUploadEnv(t)
	id := e.reserve("rollback")
	body, m := closureManifest()
	injected := errors.New("injected after ready")
	var kicks int
	e.svc.SetPrerequisiteClosure([]string{"github"}, func(uuid.UUID, uuid.UUID) { kicks++ })
	e.svc.prerequisitePreCommit = func(tx pgx.Tx) error {
		var ok bool
		err := tx.QueryRow(e.ctx, "SELECT c.state='available' AND p.state='pending' AND p.live_run_id IS NULL FROM recovery_captures c JOIN recovery_prerequisite_closures p ON p.capture_id=c.id WHERE c.id=$1", id).Scan(&ok)
		if err != nil || !ok {
			t.Fatalf("failure seam must follow actual ready and S0: %v %v", ok, err)
		}
		return injected
	}
	if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body)); !errors.Is(err, injected) {
		t.Fatal(err)
	}
	closureCount(t, e, id, 0)
	var ready bool
	if err := e.pool.QueryRow(e.ctx, "SELECT state='available' FROM recovery_captures WHERE id=$1", id).Scan(&ready); err != nil || ready || kicks != 0 {
		t.Fatalf("rollback ready=%v kicks=%d: %v", ready, kicks, err)
	}
	chunks, err := e.q.ListCaptureChunks(e.ctx, id)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("rolled back chunks: %v %v", chunks, err)
	}
}

func TestRecoveryPrerequisiteClosureUploadLostACKLiveDB(t *testing.T) {
	e := closureUploadEnv(t)
	id := e.reserve("receipt")
	body, m := closureManifest()
	kicks := 0
	e.svc.SetPrerequisiteClosure([]string{"github"}, func(user, capture uuid.UUID) {
		if user != e.w.UserID || capture != id {
			t.Fatal("kick identity")
		}
		closureCount(t, e, id, 1) // visible from another connection: commit already happened.
		kicks++
	})
	ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body))
	if err != nil || ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.State != "pending" {
		t.Fatalf("ack %+v: %v", ack, err)
	}
	before, err := e.q.GetPrerequisiteClosure(e.ctx, store.GetPrerequisiteClosureParams{UserID: e.w.UserID, CaptureID: id})
	if err != nil {
		t.Fatal(err)
	}
	if before.LiveRunID.Valid || before.Checksum != m.Checksum || !slices.Equal(before.PrerequisiteShas, m.PrerequisiteShas) {
		t.Fatalf("S0 binding: %+v", before)
	}
	// Re-registration and a disabled knob cannot remove, recreate, or mutate the receipt.
	e.w.ProtocolCapabilities = nil
	e.svc.SetPrerequisiteClosure(nil, func(uuid.UUID, uuid.UUID) { kicks++ })
	for range 2 {
		ack, err = e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(nil))
		if err != nil || ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.Checksum != m.Checksum {
			t.Fatalf("retry %+v %v", ack, err)
		}
	}
	status, err := e.svc.Status(e.ctx, e.w, e.run, id)
	if err != nil || status.PrerequisiteClosure == nil || status.PrerequisiteClosure.State != "pending" {
		t.Fatalf("status %+v %v", status, err)
	}
	// Deterministically exercise the stream-loser receipt branch: a concurrent
	// start alone may instead observe availability in the earlier admission step.
	streamReceipt, err := e.svc.stream(e.ctx, e.w, e.run, id, m, bytes.NewReader(nil))
	if err != nil || !reflect.DeepEqual(streamReceipt.PrerequisiteClosure, status.PrerequisiteClosure) {
		t.Fatalf("stream-loser closure receipt: %+v want %+v: %v", streamReceipt.PrerequisiteClosure, status.PrerequisiteClosure, err)
	}
	after, err := e.q.GetPrerequisiteClosure(e.ctx, store.GetPrerequisiteClosureParams{UserID: e.w.UserID, CaptureID: id})
	if err != nil || after.ID != before.ID || !after.UpdatedAt.Time.Equal(before.UpdatedAt.Time) || kicks != 1 {
		t.Fatalf("receipt changed: %+v kicks=%d %v", after, kicks, err)
	}
	closureCount(t, e, id, 1)
}

func TestRecoveryPrerequisiteClosureUploadConcurrentLiveDB(t *testing.T) {
	e := closureUploadEnv(t)
	id := e.reserve("concurrent")
	body, m := closureManifest()
	e.svc.uploads = make(chan struct{}, 2)
	var kicks atomic.Int32
	e.svc.SetPrerequisiteClosure([]string{"github"}, func(uuid.UUID, uuid.UUID) { kicks.Add(1) })
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body))
			if err == nil && (ack.State != "available" || ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.State != "pending") {
				err = fmt.Errorf("unexpected receipt %+v", ack)
			}
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	closureCount(t, e, id, 1)
	if kicks.Load() != 1 {
		t.Fatalf("kicks %d", kicks.Load())
	}
	e.bytes(id, body)
}

func TestRecoveryPrerequisiteClosureUploadNoBackfillLiveDB(t *testing.T) {
	for _, mode := range []string{"no-capability", "knob-off", "unreviewed-host", "gitlab", "forgejo", "unguarded", "self-contained", "snapshot-no-capability"} {
		t.Run(mode, func(t *testing.T) {
			e := closureUploadEnv(t)
			if mode == "unguarded" {
				e.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", e.run)
				old := e.hold
				e.hold = uuid.New()
				e.gen = 2
				e.exec(`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded,state,repo_id)
    SELECT $1,user_id,run_id,2,original_worker_id,original_worker_identity,live_worker_id,live_run_id,false,state,repo_id FROM recovery_custody_holds WHERE id=$2`, e.hold, old)
			}
			id := e.reserve(mode)
			body, m := closureManifest()
			switch mode {
			case "no-capability", "snapshot-no-capability":
				e.w.ProtocolCapabilities = nil
			case "knob-off":
				e.svc.SetPrerequisiteClosure(nil, nil)
			case "unreviewed-host":
				e.exec("UPDATE forge_connections SET base_url='https://enterprise.example.com' WHERE user_id=$1", e.w.UserID)
			case "gitlab", "forgejo":
				e.svc.SetPrerequisiteClosure([]string{mode}, nil)
				e.exec("UPDATE forge_connections SET forge_type=$2 WHERE user_id=$1", e.w.UserID, mode)
			case "unguarded": // Legacy hold was inserted without inventory authority.
			case "self-contained":
				m.PrerequisiteShas = nil
			}
			// The DB registration is newer than the authenticated request snapshot; never consult it.
			e.exec("UPDATE workers SET protocol_capabilities=ARRAY[$2] WHERE id=$1", e.w.ID, capability.RecoveryPrerequisiteClosureV1)
			ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "self-contained" {
				if ack.PrerequisiteClosure != nil {
					t.Fatal("self contained closure")
				}
			} else if ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.Reason != "closure_unsupported" {
				t.Fatalf("unsupported receipt %+v", ack)
			}
			closureCount(t, e, id, 0)
			e.w.ProtocolCapabilities = []string{capability.RecoveryPrerequisiteClosureV1}
			e.svc.SetPrerequisiteClosure([]string{"github"}, nil)
			e.exec("UPDATE forge_connections SET forge_type='github',base_url='https://github.com' WHERE user_id=$1", e.w.UserID)
			for range 2 {
				if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(nil)); err != nil {
					t.Fatal(err)
				}
				if _, err := e.svc.Status(e.ctx, e.w, e.run, id); err != nil {
					t.Fatal(err)
				}
			}
			closureCount(t, e, id, 0)
		})
	}
}

func TestRecoveryPrerequisiteClosureUploadIntegrityLiveDB(t *testing.T) {
	for _, mode := range []string{"checksum", "length", "over64", "max64", "immutable"} {
		t.Run(mode, func(t *testing.T) {
			e := closureUploadEnv(t)
			id := e.reserve(mode)
			body, m := closureManifest()
			wantErr := ErrIntegrity
			switch mode {
			case "checksum":
				m.Checksum = strings.Repeat("a", 64)
			case "length":
				m.ByteSize++
			case "over64", "max64":
				n := 64
				if mode == "over64" {
					n = 65
					wantErr = ErrBadRequest
				}
				m.PrerequisiteShas = nil
				for i := range n {
					m.PrerequisiteShas = append(m.PrerequisiteShas, fmt.Sprintf("%040x", i+1))
				}
			}
			ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body))
			if mode == "checksum" || mode == "length" || mode == "over64" {
				if !errors.Is(err, wantErr) {
					t.Fatalf("error %v want %v", err, wantErr)
				}
				closureCount(t, e, id, 0)
				return
			}
			if err != nil || ack.PrerequisiteClosure == nil || !slices.Equal(ack.PrerequisiteClosure.PrerequisiteShas, m.PrerequisiteShas) {
				t.Fatalf("ack %+v %v", ack, err)
			}
			closureCount(t, e, id, 1)
			if mode == "immutable" {
				changed := m
				changed.PrerequisiteShas = []string{strings.Repeat("c", 40)}
				if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, changed, bytes.NewReader(body)); !errors.Is(err, ErrManifestConflict) {
					t.Fatal(err)
				}
				row, err := e.q.GetPrerequisiteClosure(e.ctx, store.GetPrerequisiteClosureParams{UserID: e.w.UserID, CaptureID: id})
				if err != nil || row.Checksum != m.Checksum || !slices.Equal(row.PrerequisiteShas, m.PrerequisiteShas) {
					t.Fatalf("immutable row %+v %v", row, err)
				}
			}
		})
	}
}

// A pinned row is deliberately insufficient in C1b. Assert the exact Go refusal
// to distinguish the prerequisite check from the later SQL authorization refusal.
func TestRecoveryPrerequisiteClosurePinnedThinFinalLiveDB(t *testing.T) {
	e := closureUploadEnv(t)
	id := e.reserve("pinned-final")
	body, m := closureManifest()
	if _, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	row, err := e.q.GetPrerequisiteClosure(e.ctx, store.GetPrerequisiteClosureParams{UserID: e.w.UserID, CaptureID: id})
	if err != nil {
		t.Fatal(err)
	}
	row, err = e.q.ReservePrerequisitePreparation(e.ctx, store.ReservePrerequisitePreparationParams{ID: row.ID, LeaseSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	// Freeze the valid fixture binding via pending -> invoked -> pinned, with no forge operation.
	e.exec(`UPDATE recovery_prerequisite_closures p SET state='invoked',live_run_id=p.run_id,
 invoked_at=now(),invoked_deadline=now()+interval '30 seconds',pin_tip=repeat('a',40),
 connection_id=r.connection_id,forge_kind='github',normalized_base_url='https://github.com',
 project_id=r.forge_project_id,default_branch=r.default_branch
 FROM repos r WHERE p.id=$1 AND r.id=p.repo_id`, row.ID)
	e.exec("UPDATE recovery_prerequisite_closures SET state='pinned' WHERE id=$1", row.ID)
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	status, err := e.svc.Status(e.ctx, e.w, e.run, id)
	if err != nil || status.PrerequisiteClosure == nil || status.PrerequisiteClosure.State != "pinned" {
		t.Fatalf("pinned fixture: %+v %v", status, err)
	}
	e.reject(e.w, e.request(id), ErrManifestConflict)
	var open bool
	if err := e.pool.QueryRow(e.ctx, "SELECT state='open' AND final_capture_id IS NULL FROM recovery_custody_holds WHERE id=$1", e.hold).Scan(&open); err != nil || !open {
		t.Fatalf("custody changed: %v %v", open, err)
	}
}

type closureReregisterReader struct {
	io.Reader
	register func()
	once     sync.Once
}

func (r *closureReregisterReader) Read(p []byte) (int, error) {
	r.once.Do(r.register)
	return r.Reader.Read(p)
}

func TestRecoveryPrerequisiteClosureUploadRegistrationSnapshotLiveDB(t *testing.T) {
	for _, capable := range []bool{false, true} {
		t.Run(fmt.Sprint(capable), func(t *testing.T) {
			e := closureUploadEnv(t)
			id := e.reserve("registration-snapshot")
			body, m := closureManifest()
			if !capable {
				e.w.ProtocolCapabilities = nil
			}
			reader := &closureReregisterReader{Reader: bytes.NewReader(body), register: func() {
				replacement := []string{capability.RecoveryPrerequisiteClosureV1}
				if capable {
					replacement = []string{}
				}
				e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.w.ID, replacement)
			}}
			ack, err := e.svc.Upload(e.ctx, e.w, e.run, id, m, reader)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			state := "unsupported"
			if capable {
				want = 1
				state = "pending"
			}
			closureCount(t, e, id, want)
			if ack.PrerequisiteClosure == nil || ack.PrerequisiteClosure.State != state {
				t.Fatalf("snapshot receipt %+v", ack)
			}
		})
	}
}
