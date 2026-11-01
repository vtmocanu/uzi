package recovery

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// scriptedFinalPort is a PrerequisiteFinalPort whose database seams are the real guarded
// queries and whose forge listing is scripted. It has no method that could mutate a ref.
type scriptedFinalPort struct {
	e *inventoryEnv

	mu                                     sync.Mutex
	obs                                    PrerequisiteFinalObservation
	obsErr                                 error
	reserveErr                             error
	reserveMiss                            bool
	recordRows                             int64 // -1: use the real query
	reserves, observes, absences, foreigns int
	onObserve                              func()
	block                                  bool  // Observe answers only when its context ends
	recordErr                              error // RecordAbsence/RecordForeign fail with this
	passes                                 []uuid.UUID
}

func newScriptedFinalPort(e *inventoryEnv) *scriptedFinalPort {
	return &scriptedFinalPort{e: e, obs: PrerequisiteFinalPresent, recordRows: -1}
}

func (p *scriptedFinalPort) counts() (reserves, observes, absences, foreigns int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.reserves, p.observes, p.absences, p.foreigns
}

func (p *scriptedFinalPort) Reserve(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (bool, error) {
	p.mu.Lock()
	p.reserves++
	p.passes = append(p.passes, pass)
	miss, err := p.reserveMiss, p.reserveErr
	p.mu.Unlock()
	if err != nil {
		return false, err
	}
	if miss {
		return false, nil
	}
	_, e := p.e.q.ReservePrerequisiteFinalListing(ctx, store.ReservePrerequisiteFinalListingParams{
		ID: row.ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: row.CleanupAttempts})
	if e != nil {
		if strings.Contains(e.Error(), "no rows") {
			return false, nil
		}
		return false, e
	}
	return true, nil
}

func (p *scriptedFinalPort) Observe(ctx context.Context, row store.RecoveryPrerequisiteClosure) (PrerequisiteFinalObservation, error) {
	p.mu.Lock()
	p.observes++
	obs, err, hook := p.obs, p.obsErr, p.onObserve
	p.mu.Unlock()
	if hook != nil {
		hook()
	}
	if p.block {
		<-ctx.Done()
		return PrerequisiteFinalUnknown, ctx.Err()
	}
	if ctx.Err() != nil {
		return PrerequisiteFinalUnknown, ctx.Err()
	}
	return obs, err
}

func (p *scriptedFinalPort) RecordAbsence(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	p.mu.Lock()
	p.absences++
	rows, rerr := p.recordRows, p.recordErr
	p.mu.Unlock()
	if rerr != nil {
		return 0, rerr
	}
	if rows >= 0 {
		return rows, nil
	}
	return p.e.q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{
		ID: row.ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: row.CleanupAttempts})
}

func (p *scriptedFinalPort) RecordForeign(ctx context.Context, row store.RecoveryPrerequisiteClosure, pass uuid.UUID) (int64, error) {
	p.mu.Lock()
	p.foreigns++
	rows, rerr := p.recordRows, p.recordErr
	p.mu.Unlock()
	if rerr != nil {
		return 0, rerr
	}
	if rows >= 0 {
		return rows, nil
	}
	return p.e.q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{
		ID: row.ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: row.CleanupAttempts})
}

// admissionGate is a fixed-budget limiter that never refunds: calls counts every Allow.
type admissionGate struct {
	mu         sync.Mutex
	calls, max int
}

func (g *admissionGate) Allow(uuid.UUID) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls++
	return g.calls <= g.max
}

func (g *admissionGate) total() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

type closureFinalFx struct {
	*inventoryEnv
	port    *scriptedFinalPort
	gate    *admissionGate
	capture uuid.UUID
	body    []byte
}

// newClosureFinalFx builds a completed run holding one thin capture whose closure is driven to
// state through the production queries only. The service is fully wired (GitHub knob, port,
// limiter) and the worker registered with the closure capability.
func newClosureFinalFx(t *testing.T, state string) *closureFinalFx {
	t.Helper()
	return newClosureFinalFxOpts(t, state, closureFxOpts{})
}

// closureFxOpts varies the one fixture fact a refusal case needs; the closure's invocation
// facts are immutable once written, so they are chosen at invocation time.
type closureFxOpts struct {
	forgeKind, baseURL string
	noClosure          bool // the thin capture was uploaded with the GitHub knob off
}

func newClosureFinalFxOpts(t *testing.T, state string, o closureFxOpts) *closureFinalFx {
	t.Helper()
	if o.forgeKind == "" {
		o.forgeKind = "github"
	}
	if o.baseURL == "" {
		o.baseURL = "https://github.com"
	}
	e := newInventoryEnv(t)
	e.w.ProtocolCapabilities = append(e.w.ProtocolCapabilities, capability.RecoveryPrerequisiteClosureV1, capability.RecoveryHeldPublicationV2)
	e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", e.w.ID, e.w.ProtocolCapabilities)
	e.exec("UPDATE forge_connections SET base_url='https://github.com' WHERE user_id=$1", e.w.UserID)
	e.exec("UPDATE repos SET web_url='https://github.com/g/r' WHERE id=(SELECT repo_id FROM runs WHERE id=$1)", e.run)
	if !o.noClosure {
		e.svc.SetPrerequisiteClosure([]string{"github"}, nil)
	}
	fx := &closureFinalFx{inventoryEnv: e, port: newScriptedFinalPort(e), gate: &admissionGate{max: 1000}}
	e.svc.SetPrerequisiteFinal(fx.port)
	e.svc.SetPrerequisiteFinalAllow(fx.gate.Allow)

	fx.capture = e.reserve("thin-final")
	var m apitypes.RecoveryUploadManifest
	fx.body, m = closureManifest()
	if _, err := e.svc.Upload(e.ctx, e.w, e.run, fx.capture, m, bytes.NewReader(fx.body)); err != nil {
		t.Fatal(err)
	}
	if o.noClosure {
		e.svc.SetPrerequisiteClosure([]string{"github"}, nil)
		e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
		return fx
	}
	row := fx.closure()
	if state != "pending" {
		var err error
		if row, err = e.q.ReservePrerequisitePreparation(e.ctx, store.ReservePrerequisitePreparationParams{ID: row.ID, LeaseSeconds: 60}); err != nil {
			t.Fatal(err)
		}
	}
	switch state {
	case "pending":
	case "refused":
		fx.rows(e.q.RefusePrerequisitePreparation(e.ctx, store.RefusePrerequisitePreparationParams{ID: row.ID, Generation: row.Attempts, Reason: "archive_unavailable"}))
	default:
		// The closure upload already admitted a GitHub.com connection; the invocation then
		// freezes whatever the connection is when the marker is written.
		e.exec("UPDATE forge_connections SET forge_type=$2,base_url=$3 WHERE user_id=$1", e.w.UserID, o.forgeKind, o.baseURL)
		e.exec(`UPDATE recovery_prerequisite_closures p SET state='invoked',live_run_id=p.run_id,
 invoked_at=now(),invoked_deadline=now()+interval '30 seconds',pin_tip=repeat('a',40),
 connection_id=r.connection_id,forge_kind=$2,normalized_base_url=$3,
 project_id=r.forge_project_id,default_branch=r.default_branch
 FROM repos r WHERE p.id=$1 AND r.id=p.repo_id`, row.ID, o.forgeKind, o.baseURL)
		switch state {
		case "invoked":
		case "create_unknown":
			fx.rows(e.q.MarkPrerequisiteCreateUnknown(e.ctx, store.MarkPrerequisiteCreateUnknownParams{ID: row.ID, Generation: row.Attempts}))
		default:
			fx.rows(e.q.RecordPrerequisitePinned(e.ctx, store.RecordPrerequisitePinnedParams{ID: row.ID, Generation: row.Attempts}))
		}
	}
	switch state {
	case "foreign":
		fx.reserveListing(uuid.New())
	case "absent":
		pass := uuid.New()
		fx.reserveListing(pass)
		fx.rows(e.q.RecordPrerequisiteAbsence(e.ctx, store.RecordPrerequisiteAbsenceParams{ID: row.ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: 0}))
	}
	if state == "foreign" {
		cur := fx.closure()
		fx.rows(e.q.RecordPrerequisitePinnedForeign(e.ctx, store.RecordPrerequisitePinnedForeignParams{ID: row.ID, PassID: cur.LastAuditPassID, Generation: 0}))
	}
	e.exec("UPDATE runs SET status='completed' WHERE id=$1", e.run)
	return fx
}

func (fx *closureFinalFx) rows(n int64, err error) {
	fx.t.Helper()
	if err != nil || n != 1 {
		fx.t.Fatalf("fixture write: rows=%d err=%v", n, err)
	}
}

func (fx *closureFinalFx) reserveListing(pass uuid.UUID) {
	fx.t.Helper()
	if _, err := fx.q.ReservePrerequisiteFinalListing(fx.ctx, store.ReservePrerequisiteFinalListingParams{
		ID: fx.closure().ID, PassID: pgtype.UUID{Bytes: pass, Valid: true}, Generation: 0}); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *closureFinalFx) closure() store.RecoveryPrerequisiteClosure {
	fx.t.Helper()
	row, err := fx.q.GetPrerequisiteClosureForCapture(fx.ctx, fx.capture)
	if err != nil {
		fx.t.Fatal(err)
	}
	return row
}

func (fx *closureFinalFx) snapshot(table string, id uuid.UUID) string {
	fx.t.Helper()
	var s string
	if err := fx.pool.QueryRow(fx.ctx, "SELECT to_jsonb(x)::text FROM "+table+" x WHERE id=$1", id).Scan(&s); err != nil {
		fx.t.Fatal(err)
	}
	return s
}

func (fx *closureFinalFx) release(w store.Worker, req apitypes.RecoveryReleaseRequest) (apitypes.RecoveryReleaseResponse, error) {
	return fx.svc.Release(fx.ctx, w, fx.run, req)
}

func (fx *closureFinalFx) assertOpen() {
	fx.t.Helper()
	var open bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state='open' AND live_worker_id IS NOT NULL AND final_disposition IS NULL
 AND final_capture_id IS NULL AND final_closure_id IS NULL AND release_evidence IS NULL FROM recovery_custody_holds WHERE id=$1`, fx.hold).Scan(&open); err != nil || !open {
		fx.t.Fatalf("hold is not untouched-open: %v %v", open, err)
	}
}

func (fx *closureFinalFx) assertReleased() {
	fx.t.Helper()
	var ok bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT h.state='released' AND h.final_disposition='archive' AND h.release_evidence='prerequisite_closure'
 AND h.final_capture_id=$2 AND h.final_closure_id=p.id AND h.live_worker_id IS NULL AND h.live_run_id IS NULL
 FROM recovery_custody_holds h JOIN recovery_prerequisite_closures p ON p.capture_id=$2 WHERE h.id=$1`, fx.hold, fx.capture).Scan(&ok); err != nil || !ok {
		fx.t.Fatalf("hold is not a closure-backed release: %v %v", ok, err)
	}
}

func TestRecoveryClosureFinalPositiveLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	before := fx.closure()
	capBefore := fx.snapshot("recovery_captures", fx.capture)
	ack, err := fx.release(fx.w, fx.request(fx.capture))
	if err != nil || !ack.Released || ack.HoldsReleased != 1 || ack.Generation == nil || *ack.Generation != fx.gen {
		t.Fatalf("release: %+v %v", ack, err)
	}
	fx.assertReleased()
	after := fx.closure()
	if after.State != "pinned" || after.AbsentSince.Valid {
		t.Fatalf("release changed the closure: %+v", after)
	}
	if !after.AuditNextAt.Time.Equal(before.AuditNextAt.Time) {
		t.Fatalf("reserving a FINAL listing moved audit_next_at: %v -> %v", before.AuditNextAt.Time, after.AuditNextAt.Time)
	}
	if after.CleanupAttempts != before.CleanupAttempts || after.PinTip != before.PinTip || after.LiveRunID != before.LiveRunID {
		t.Fatalf("release moved pin custody: %+v -> %+v", before, after)
	}
	var kept bool
	if err := fx.pool.QueryRow(fx.ctx, `SELECT state='available' AND expires_at=(($2::jsonb)->>'expires_at')::timestamptz
 AND local_replica_worker_id=$3 AND ready_retention_seconds=3600 FROM recovery_captures WHERE id=$1`,
		fx.capture, capBefore, fx.w.ID).Scan(&kept); err != nil || !kept {
		t.Fatalf("capture bytes/expiry not kept and protected: %v %v", kept, err)
	}
	fx.bytes(fx.capture, fx.body)
	if r, o, a, f := fx.port.counts(); r != 1 || o != 1 || a != 0 || f != 0 || fx.gate.total() != 1 {
		t.Fatalf("calls reserve=%d observe=%d absence=%d foreign=%d admissions=%d", r, o, a, f, fx.gate.total())
	}
	// An exact replay is answered before the limiter, the port and the forge.
	again, err := fx.release(fx.w, fx.request(fx.capture))
	if err != nil || again != ack {
		t.Fatalf("replay: %+v %v want %+v", again, err, ack)
	}
	if r, o, _, _ := fx.port.counts(); r != 1 || o != 1 || fx.gate.total() != 1 {
		t.Fatalf("replay reached the forge path: reserve=%d observe=%d admissions=%d", r, o, fx.gate.total())
	}
}

func TestRecoveryClosureFinalRefusalsLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	type tc struct {
		name  string
		state string
		edit  func(fx *closureFinalFx, w *store.Worker, req *apitypes.RecoveryReleaseRequest)
		want  error
	}
	parked := func(cause string) func(*closureFinalFx, *store.Worker, *apitypes.RecoveryReleaseRequest) {
		return func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE runs SET claim_generation=2,worker_id=NULL,status='recovery_wait',recovery_wait_cause=$2 WHERE id=$1", fx.run, cause)
		}
	}
	cases := []tc{
		{"knob off", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.svc.SetPrerequisiteClosure(nil, nil)
		}, ErrManifestConflict},
		{"nil port", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.svc.SetPrerequisiteFinal(nil)
		}, ErrManifestConflict},
		{"nil allow", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.svc.SetPrerequisiteFinalAllow(nil)
		}, ErrManifestConflict},
		{"capability missing", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE workers SET protocol_capabilities=ARRAY[$2,$3] WHERE id=$1", fx.w.ID, capability.RecoveryArchiveV1, capability.RecoveryInventoryV1)
		}, ErrManifestConflict},
		{"wrong worker", "pinned", func(_ *closureFinalFx, w *store.Worker, _ *apitypes.RecoveryReleaseRequest) { w.ID = uuid.New() }, ErrNotAuthorized},
		{"wrong user", "pinned", func(_ *closureFinalFx, w *store.Worker, _ *apitypes.RecoveryReleaseRequest) { w.UserID = uuid.New() }, ErrNotAuthorized},
		{"wrong generation", "pinned", func(_ *closureFinalFx, _ *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			g := int64(9)
			r.Generation = &g
		}, ErrNotAuthorized},
		{"wrong capture", "pinned", func(_ *closureFinalFx, _ *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.CaptureID = uuid.NewString()
		}, ErrNotAuthorized},
		{"unended generation", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE runs SET status='running' WHERE id=$1", fx.run)
		}, ErrNotAuthorized},
		{"park worker_requeue_exhausted", "pinned", parked("worker_requeue_exhausted"), ErrNotAuthorized},
		{"park worker_memory_pressure", "pinned", parked("worker_memory_pressure"), ErrNotAuthorized},
		{"park retained_recovery_exhausted", "pinned", parked("retained_recovery_exhausted"), ErrNotAuthorized},
		{"park retained_recovery_adoption_failed", "pinned", parked("retained_recovery_adoption_failed"), ErrNotAuthorized},
		{"source mismatch", "pinned", func(_ *closureFinalFx, _ *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.SourceSha = "bbbb2222"
		}, ErrManifestConflict},
		{"digest mismatch", "pinned", func(_ *closureFinalFx, _ *store.Worker, r *apitypes.RecoveryReleaseRequest) {
			r.FinalDisposition.CoverageDigest = strings.Repeat("b", 64)
		}, ErrManifestConflict},
		{"checksum mismatch", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE recovery_captures SET checksum=repeat('e',64) WHERE id=$1", fx.capture)
		}, ErrManifestConflict},
		{"prerequisite mismatch", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE recovery_captures SET prerequisite_shas=ARRAY[repeat('c',40)] WHERE id=$1", fx.capture)
		}, ErrManifestConflict},
		{"capture expired", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", fx.capture)
		}, ErrManifestConflict},
		{"capture unavailable", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE recovery_captures SET state='discarded' WHERE id=$1", fx.capture)
		}, ErrManifestConflict},
		{"capture not manifest bound", "pinned", func(fx *closureFinalFx, _ *store.Worker, _ *apitypes.RecoveryReleaseRequest) {
			fx.exec("UPDATE recovery_captures SET manifest_bound=false WHERE id=$1", fx.capture)
		}, ErrManifestConflict},
		{"closure pending", "pending", nil, ErrManifestConflict},
		{"closure invoked", "invoked", nil, ErrManifestConflict},
		{"closure create_unknown", "create_unknown", nil, ErrManifestConflict},
		{"closure refused", "refused", nil, ErrManifestConflict},
		{"closure foreign", "foreign", nil, ErrManifestConflict},
		{"closure absent_since set", "absent", nil, ErrPrerequisitePinMissing},
	}
	cases = append(cases,
		tc{"non-github forge", "pinned", nil, ErrManifestConflict},
		tc{"unreviewed host", "pinned", nil, ErrManifestConflict},
		tc{"no closure row", "pinned", nil, ErrManifestConflict})
	opts := map[string]closureFxOpts{
		"non-github forge": {forgeKind: "gitlab"},
		"unreviewed host":  {baseURL: "https://enterprise.example.com"},
		"no closure row":   {noClosure: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newClosureFinalFxOpts(t, c.state, opts[c.name])
			w, req := fx.w, fx.request(fx.capture)
			if c.edit != nil {
				c.edit(fx, &w, &req)
			}
			closureBefore := ""
			if !opts[c.name].noClosure {
				closureBefore = fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
			}
			ack, err := fx.release(w, req)
			if !errors.Is(err, c.want) {
				t.Fatalf("release: %+v error %v, want %v", ack, err, c.want)
			}
			if ack.Released {
				t.Fatalf("false ACK: %+v", ack)
			}
			fx.assertOpen()
			if !opts[c.name].noClosure {
				if after := fx.snapshot("recovery_prerequisite_closures", fx.closure().ID); after != closureBefore {
					t.Fatalf("a refusal wrote the closure:\n%s\n%s", closureBefore, after)
				}
			}
			if r, o, a, f := fx.port.counts(); r+o+a+f != 0 || fx.gate.total() != 0 {
				t.Fatalf("a refusal spent an admission or reached the forge path: reserve=%d observe=%d absence=%d foreign=%d admissions=%d", r, o, a, f, fx.gate.total())
			}
		})
	}
}

func TestRecoveryClosureFinalListingOutcomesLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	t.Run("listing error", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.port.obs, fx.port.obsErr = PrerequisiteFinalPresent, errors.New("boom")
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinUnavailable)
		fx.assertOpen()
		if row := fx.closure(); row.State != "pinned" || row.AbsentSince.Valid {
			t.Fatalf("an unknown listing wrote the closure: %+v", row)
		}
		if r, o, a, f := fx.port.counts(); r != 1 || o != 1 || a != 0 || f != 0 {
			t.Fatalf("calls %d %d %d %d", r, o, a, f)
		}
	})
	t.Run("unknown verdict", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.port.obs = PrerequisiteFinalUnknown
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinUnavailable)
		fx.assertOpen()
		if fx.closure().AbsentSince.Valid {
			t.Fatal("unknown became absence")
		}
	})
	t.Run("absent", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.port.obs = PrerequisiteFinalAbsent
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinMissing)
		fx.assertOpen()
		row := fx.closure()
		if row.State != "pinned" || !row.AbsentSince.Valid || !row.AbsencePassID.Valid {
			t.Fatalf("absence not committed: %+v", row)
		}
		// The recorded absence now refuses before any admission or listing.
		calls := fx.gate.total()
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinMissing)
		if r, o, _, _ := fx.port.counts(); r != 1 || o != 1 || fx.gate.total() != calls {
			t.Fatalf("pin_missing reached the limiter or forge: reserve=%d observe=%d admissions=%d", r, o, fx.gate.total())
		}
	})
	t.Run("foreign", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.port.obs = PrerequisiteFinalForeign
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinForeign)
		fx.assertOpen()
		if row := fx.closure(); row.State != "foreign" || row.LiveRunID.Valid {
			t.Fatalf("foreign not committed: %+v", row)
		}
	})
	t.Run("limiter denial", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.gate.max = 0
		before := fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
		fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinUnavailable)
		fx.assertOpen()
		if fx.snapshot("recovery_prerequisite_closures", fx.closure().ID) != before {
			t.Fatal("a denied admission reserved or wrote the closure")
		}
		if r, o, a, f := fx.port.counts(); r+o+a+f != 0 || fx.gate.total() != 1 {
			t.Fatalf("denial reached the port: %d %d %d %d admissions=%d", r, o, a, f, fx.gate.total())
		}
	})
}

// A failed request never refunds its admission: with a budget of N, N failing requests of
// every kind leave request N+1 denied with no reservation and no forge call.
func TestRecoveryClosureFinalAdmissionIsNeverRefundedLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	fx.gate.max = 4
	req := fx.request(fx.capture)

	fx.port.obsErr = errors.New("listing failed") // 1: listing error
	fx.reject(fx.w, req, ErrPrerequisitePinUnavailable)
	fx.port.obsErr = nil

	ctx, cancel := context.WithCancel(fx.ctx) // 2: cancelled request
	fx.port.onObserve = cancel
	if _, err := fx.svc.Release(ctx, fx.w, fx.run, req); !errors.Is(err, ErrPrerequisitePinUnavailable) {
		t.Fatalf("cancelled request: %v", err)
	}
	cancel()
	fx.port.onObserve = nil

	fx.port.reserveMiss = true // 3: reservation miss
	fx.reject(fx.w, req, ErrPrerequisitePinUnavailable)
	fx.port.reserveMiss = false

	fx.port.obs, fx.port.recordRows = PrerequisiteFinalAbsent, 0 // 4: zero-row CAS
	fx.reject(fx.w, req, ErrPrerequisitePinUnavailable)
	fx.port.obs, fx.port.recordRows = PrerequisiteFinalPresent, -1

	r, o, a, _ := fx.port.counts()
	if fx.gate.total() != 4 || r != 4 || o != 3 || a != 1 {
		t.Fatalf("after 4 failures: admissions=%d reserve=%d observe=%d absence=%d", fx.gate.total(), r, o, a)
	}
	before := fx.snapshot("recovery_prerequisite_closures", fx.closure().ID)
	fx.reject(fx.w, req, ErrPrerequisitePinUnavailable) // N+1: denied
	if fx.gate.total() != 5 {
		t.Fatalf("admissions %d", fx.gate.total())
	}
	r2, o2, a2, f2 := fx.port.counts()
	if r2 != 4 || o2 != 3 || a2 != 1 || f2 != 0 {
		t.Fatalf("request N+1 reached the port: %d %d %d %d", r2, o2, a2, f2)
	}
	if fx.snapshot("recovery_prerequisite_closures", fx.closure().ID) != before {
		t.Fatal("request N+1 wrote the closure")
	}
	fx.assertOpen()
}

func TestRecoveryClosureFinalReplayNeedsNoForgeLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	for _, tc := range []struct {
		name string
		then func(fx *closureFinalFx)
	}{
		{"knob off", func(fx *closureFinalFx) { fx.svc.SetPrerequisiteClosure(nil, nil) }},
		{"capability removed", func(fx *closureFinalFx) {
			fx.exec("UPDATE workers SET protocol_capabilities=ARRAY[$2] WHERE id=$1", fx.w.ID, capability.RecoveryInventoryV1)
		}},
		{"forge outage", func(fx *closureFinalFx) {
			fx.port.obsErr = errors.New("outage")
			fx.port.obs = PrerequisiteFinalUnknown
		}},
		{"limiter exhausted", func(fx *closureFinalFx) { fx.gate.max = 0 }},
		{"port unwired", func(fx *closureFinalFx) { fx.svc.SetPrerequisiteFinal(nil); fx.svc.SetPrerequisiteFinalAllow(nil) }},
		{"capture expired", func(fx *closureFinalFx) {
			fx.exec("UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", fx.capture)
		}},
		{"pin deleted", func(fx *closureFinalFx) {
			fx.exec("UPDATE recovery_captures SET state='discarded' WHERE id=$1", fx.capture)
			row := fx.closure()
			cleanup, err := fx.q.ReservePrerequisiteCleanup(fx.ctx, store.ReservePrerequisiteCleanupParams{PassID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, ID: row.ID, LeaseSeconds: 60})
			if err != nil {
				t.Fatalf("cleanup reservation: %v", err)
			}
			fx.rows(fx.q.RecordPrerequisiteDeleted(fx.ctx, store.RecordPrerequisiteDeletedParams{ID: row.ID, Generation: cleanup.CleanupAttempts, PassID: cleanup.LastAuditPassID}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			if err != nil || !ack.Released {
				t.Fatalf("release: %+v %v", ack, err)
			}
			tc.then(fx)
			admissions := fx.gate.total()
			r, o, a, f := fx.port.counts()
			again, err := fx.release(fx.w, fx.request(fx.capture))
			if err != nil || again != ack {
				t.Fatalf("replay: %+v %v want %+v", again, err, ack)
			}
			r2, o2, a2, f2 := fx.port.counts()
			if r != r2 || o != o2 || a != a2 || f != f2 || fx.gate.total() != admissions {
				t.Fatal("replay consumed an admission or reached the port")
			}
			// A different declaration against the same receipt is a conflict, not an ACK.
			other := fx.request(fx.capture)
			other.FinalDisposition.SourceSha = "bbbb2222"
			fx.reject(fx.w, other, ErrManifestConflict)
		})
	}
}

// supersede models a newer owner of the closure's listing fence (cleanup, audit or another FINAL).
func (fx *closureFinalFx) supersede(kind string) {
	fx.t.Helper()
	row := fx.closure()
	switch kind {
	case "another FINAL":
		fx.reserveListing(uuid.New())
	case "audit":
		fx.exec("UPDATE recovery_prerequisite_closures SET audit_next_at=now()-interval '1 second' WHERE id=$1", row.ID)
		if _, err := fx.q.ReservePrerequisiteAudit(fx.ctx, store.ReservePrerequisiteAuditParams{ID: row.ID, PassID: pgtype.UUID{Bytes: uuid.New(), Valid: true}}); err != nil {
			fx.t.Fatal(err)
		}
	case "cleanup":
		fx.exec("UPDATE recovery_captures SET state='discarded' WHERE id=$1", fx.capture)
		fx.exec("UPDATE recovery_prerequisite_closures SET next_attempt_at=now()-interval '1 second' WHERE id=$1", row.ID)
		if _, err := fx.q.ReservePrerequisiteCleanup(fx.ctx, store.ReservePrerequisiteCleanupParams{ID: row.ID, PassID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, LeaseSeconds: 60}); err != nil {
			fx.t.Fatal(err)
		}
	}
}

func TestRecoveryClosureFinalSupersededReservationLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	for _, stage := range []string{"reserved", "observed"} {
		for _, kind := range []string{"another FINAL", "audit", "cleanup"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				fx := newClosureFinalFx(t, "pinned")
				fx.svc.finalClosureHook = func(s string) {
					if s == stage {
						fx.supersede(kind)
					}
				}
				want := ErrPrerequisitePinUnavailable
				if kind == "cleanup" {
					want = ErrManifestConflict // cleanup only runs once the capture itself has left 'available'
				}
				fx.reject(fx.w, fx.request(fx.capture), want)
				fx.assertOpen()
				if fx.closure().AbsentSince.Valid {
					t.Fatal("a stale pass wrote an absence")
				}
			})
		}
	}
	// A stale absent or foreign verdict is fenced out: zero rows, nothing overwritten.
	for _, obs := range []PrerequisiteFinalObservation{PrerequisiteFinalAbsent, PrerequisiteFinalForeign} {
		t.Run("stale verdict", func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			fx.port.obs = obs
			fx.svc.finalClosureHook = func(s string) {
				if s == "reserved" {
					fx.supersede("another FINAL")
				}
			}
			fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinUnavailable)
			if row := fx.closure(); row.State != "pinned" || row.AbsentSince.Valid {
				t.Fatalf("stale verdict overwrote the closure: %+v", row)
			}
		})
	}
}

func TestRecoveryClosureFinalStateChangedAfterListingLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	// Every owner-only cause that appears after the listing refuses the release.
	for _, cause := range []string{"worker_requeue_exhausted", "worker_memory_pressure", "retained_recovery_exhausted", "retained_recovery_adoption_failed"} {
		t.Run("run parked "+cause+" after the listing", func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			fx.svc.finalClosureHook = func(s string) {
				if s == "observed" {
					fx.exec("UPDATE runs SET claim_generation=2,worker_id=NULL,status='recovery_wait',recovery_wait_cause=$2 WHERE id=$1", fx.run, cause)
				}
			}
			fx.reject(fx.w, fx.request(fx.capture), ErrNotAuthorized)
			fx.assertOpen()
		})
	}
	t.Run("capability removed after the listing", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" {
				fx.exec("UPDATE workers SET protocol_capabilities=ARRAY[$2] WHERE id=$1", fx.w.ID, capability.RecoveryInventoryV1)
			}
		}
		fx.reject(fx.w, fx.request(fx.capture), ErrManifestConflict)
		fx.assertOpen()
	})
	t.Run("capture expired after the listing", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.svc.finalClosureHook = func(s string) {
			if s == "observed" {
				fx.exec("UPDATE recovery_captures SET expires_at=clock_timestamp()-interval '1 minute' WHERE id=$1", fx.capture)
			}
		}
		fx.reject(fx.w, fx.request(fx.capture), ErrManifestConflict)
		fx.assertOpen()
	})
	t.Run("racing identical FINAL committed", func(t *testing.T) {
		fx := newClosureFinalFx(t, "pinned")
		fx.svc.finalClosureHook = func(s string) {
			if s != "observed" {
				return
			}
			row := fx.closure()
			n, err := fx.q.ProtectFinalClosureCapture(fx.ctx, store.ProtectFinalClosureCaptureParams{
				WorkerID: fx.w.ID, RetentionSeconds: 3600, ID: fx.capture, HoldID: fx.hold, SourceSha: "aaaa1111",
				CoverageDigest: pgtype.Text{String: strings.Repeat("a", 64), Valid: true}, ClosureID: row.ID})
			fx.rows(n, err)
			fx.rows(fx.q.ReleaseFinalClosureHold(fx.ctx, store.ReleaseFinalClosureHoldParams{
				ID: fx.hold, RunID: fx.run, UserID: fx.w.UserID, Generation: fx.gen, WorkerID: fx.w.ID,
				FinalCaptureID: fx.capture, FinalSourceSha: "aaaa1111", FinalCoverageDigest: strings.Repeat("a", 64), ClosureID: row.ID}))
		}
		ack, err := fx.release(fx.w, fx.request(fx.capture))
		if err != nil || !ack.Released {
			t.Fatalf("racing replay: %+v %v", ack, err)
		}
		fx.assertReleased()
	})
}

// The unfenced admission must not leak anything a forge, credential or log reader could use.
func TestRecoveryClosureFinalDiagnosticsLeakNothingLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	token := "ghp_" + strings.Repeat("A", 36)
	secret := "listing failed for https://user:" + token + "@evil.example/x\r\n\x1b[31mred\x1b[0m " + token
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	for _, name := range []string{"observe", "reserve", "record absence", "record foreign"} {
		t.Run(name, func(t *testing.T) {
			fx := newClosureFinalFx(t, "pinned")
			switch name {
			case "observe":
				fx.port.obsErr = errors.New(secret)
			case "reserve":
				fx.port.reserveErr = errors.New(secret)
			case "record absence":
				fx.port.obs, fx.port.recordErr = PrerequisiteFinalAbsent, errors.New(secret)
			case "record foreign":
				fx.port.obs, fx.port.recordErr = PrerequisiteFinalForeign, errors.New(secret)
			}
			ack, err := fx.release(fx.w, fx.request(fx.capture))
			if !errors.Is(err, ErrPrerequisitePinUnavailable) {
				t.Fatalf("error %v", err)
			}
			text := err.Error() + " " + strings.Join([]string{ack.Reason, ack.RunID}, " ")
			row := fx.closure()
			text += " " + row.LastError + " " + row.Reason + " " + fx.snapshot("recovery_prerequisite_closures", row.ID)
			for _, bad := range []string{token, "evil.example", "user:", "\x1b", "\r"} {
				if strings.Contains(text, bad) || strings.Contains(logs.String(), bad) {
					t.Fatalf("diagnostics leaked %q:\nresponse+row: %q\nlogs: %q", bad, text, logs.String())
				}
			}
		})
	}
}

func TestRecoveryClosureFinalReceiptObservedLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	fx := newClosureFinalFx(t, "pinned")
	ack, err := fx.release(fx.w, fx.request(fx.capture))
	if err != nil || !ack.Released {
		t.Fatalf("release: %+v %v", ack, err)
	}
	got, err := fx.svc.ObserveExactHold(fx.ctx, fx.w, fx.run, fx.gen)
	if err != nil || got == nil || got.State != "released" || got.ReleaseEvidence != "prerequisite_closure" ||
		got.FinalReceipt == nil || got.FinalReceipt.Kind != "archive" || got.FinalReceipt.CaptureID != fx.capture.String() {
		t.Fatalf("exact observation of a closure-backed release: %+v %v", got, err)
	}
	// Reconcile echoes the receipt but keeps release_evidence for settled FINALs only.
	rec, err := fx.svc.Reconcile(fx.ctx, fx.w, fx.run, fx.capture, apitypes.RecoveryReconcileRequest{
		Generation: fx.gen, SourceSha: "aaaa1111", CoverageDigest: strings.Repeat("a", 64), Checksum: strings.Repeat("a", 64), ByteSize: 1})
	if err != nil || rec.Outcome != "accepted" || rec.ReleaseEvidence != "" || rec.FinalReceipt == nil || rec.FinalReceipt.Kind != "archive" {
		t.Fatalf("reconcile echo: %+v %v", rec, err)
	}
	// The cross-run classifier accepts the same hold.
	hold, err := fx.q.GetFinalInventoryHold(fx.ctx, store.GetFinalInventoryHoldParams{RunID: fx.run, UserID: fx.w.UserID, WorkerID: fx.w.ID, Generation: fx.gen})
	if err != nil || !hold.FinalClosureID.Valid {
		t.Fatalf("hold: %+v %v", hold, err)
	}
	cap, err := fx.q.GetCaptureForOwner(fx.ctx, store.GetCaptureForOwnerParams{ID: fx.capture, RunID: fx.run, UserID: fx.w.UserID})
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyCrossRunHold(hold, 1, nil, nil, map[uuid.UUID]store.RecoveryCapture{fx.capture: cap}); err != nil {
		t.Fatalf("classifier refused a genuine closure-backed release: %v", err)
	}
}

func TestRecoveryClosureFinalBoundsLiveDB(t *testing.T) {
	// Closure rows and their capture foreign keys outlive a test, and other tests in this package
	// wipe whole tables: own a database per top-level test.
	isolateClosureUploadDatabase(t)
	// A port that only answers when its context ends cannot hold the request past its enforced
	// 6s bound, and the stall is neither an absence nor a release.
	fx := newClosureFinalFx(t, "pinned")
	fx.port.block = true
	start := time.Now()
	fx.reject(fx.w, fx.request(fx.capture), ErrPrerequisitePinUnavailable)
	if d := time.Since(start); d < finalClosureBudget-time.Second || d > finalClosureBudget+500*time.Millisecond {
		t.Fatalf("request took %v, want the %v budget plus a small margin", d, finalClosureBudget)
	}
	fx.assertOpen()
	if fx.closure().AbsentSince.Valid {
		t.Fatal("a timeout became absence")
	}
}
