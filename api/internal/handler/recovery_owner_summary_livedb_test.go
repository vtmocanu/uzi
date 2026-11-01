package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/recovery"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type ownerSummaryTrace struct {
	mu         sync.Mutex
	statements []string
}

func (tr *ownerSummaryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.statements = append(tr.statements, d.SQL)
	return ctx
}
func (*ownerSummaryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (tr *ownerSummaryTrace) take() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	out := tr.statements
	tr.statements = nil
	return out
}

type ownerSummaryForge struct{ hits atomic.Int32 }

func (f *ownerSummaryForge) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	f.hits.Add(1)
	return nil, errors.New("owner summary must not contact the forge")
}

// seedClosure exercises the durable transition guard; no trigger or constraint is disabled.
// Scheduling timestamps are made due between the eight bounded cleanup attempts.
// A failed transition aborts this fixture rather than seeding misleading sibling cases.
func (f *ownerSummaryFixture) seedClosure(t *testing.T, state string, absent, exhausted bool) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	q := store.New(f.pool)
	id := f.seedCapture(t, true)
	p, err := q.InsertPrerequisiteClosure(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if state == "pending" {
		return id
	}
	p, err = q.ReservePrerequisitePreparation(ctx, store.ReservePrerequisitePreparationParams{ID: p.ID, LeaseSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if state == "refused" {
		n, err := q.RefusePrerequisitePreparation(ctx, store.RefusePrerequisitePreparationParams{ID: p.ID, Generation: p.Attempts, Reason: "prerequisite_not_on_default"})
		if err != nil || n != 1 {
			t.Fatalf("refuse=%d %v", n, err)
		}
		return id
	}
	cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures p SET state='invoked',live_run_id=p.run_id,
 invoked_at=now(),invoked_deadline=now()+interval '30 seconds',pin_tip=repeat('1',40),
 connection_id=r.connection_id,forge_kind='github',normalized_base_url='https://github.com',
 project_id=r.forge_project_id,default_branch=r.default_branch,updated_at=now()
 FROM repos r WHERE p.id=$1 AND r.id=p.repo_id`, p.ID)
	if state == "invoked" {
		return id
	}
	if state == "create_unknown" {
		n, err := q.MarkPrerequisiteCreateUnknown(ctx, store.MarkPrerequisiteCreateUnknownParams{ID: p.ID, Generation: p.Attempts})
		if err != nil || n != 1 {
			t.Fatalf("unknown=%d %v", n, err)
		}
		return id
	}
	if state == "foreign" {
		n, err := q.RecordPrerequisiteForeign(ctx, store.RecordPrerequisiteForeignParams{ID: p.ID, Generation: p.Attempts})
		if err != nil || n != 1 {
			t.Fatalf("foreign=%d %v", n, err)
		}
		return id
	}
	n, err := q.RecordPrerequisitePinned(ctx, store.RecordPrerequisitePinnedParams{ID: p.ID, Generation: p.Attempts})
	if err != nil || n != 1 {
		t.Fatalf("pinned=%d %v", n, err)
	}
	if state == "deleted" || exhausted {
		cliMustExec(t, f.pool, "UPDATE recovery_captures SET state='expired',updated_at=now() WHERE id=$1", id)
		attempts := 1
		if exhausted {
			attempts = 8
		}
		for i := 0; i < attempts; i++ {
			cliMustExec(t, f.pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now()-interval '1 second' WHERE id=$1", p.ID)
			pass := uuid.New()
			// Same reservation/retry transitions as the cleanup queries, under the real guard.
			cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures SET cleanup_attempts=cleanup_attempts+1,
 cleanup_deadline=now()+interval '30 seconds',next_attempt_at=now()+interval '60 seconds',last_audit_pass_id=$2,updated_at=now() WHERE id=$1`, p.ID, pass)
			if state == "deleted" {
				cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures SET state='deleted',live_run_id=NULL,reason='',
 cleanup_exhausted_at=NULL,updated_at=now() WHERE id=$1`, p.ID)
			} else {
				cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures SET cleanup_deadline=NULL,
 cleanup_exhausted_at=CASE WHEN cleanup_attempts=8 THEN now() ELSE NULL END,
 reason=CASE WHEN cleanup_attempts=8 THEN 'pin_cleanup_exhausted' ELSE reason END,updated_at=now() WHERE id=$1`, p.ID)
			}
		}
	}
	if absent || state == "lost" {
		// The first observation may have occurred eleven minutes ago; the guard still
		// requires a distinct later pass before the lost transition.
		pass := uuid.New()
		cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures SET absent_since=now()-interval '11 minutes',
 absence_pass_id=$2,last_audit_pass_id=$2,reason='pin_absence_unconfirmed',updated_at=now() WHERE id=$1`, p.ID, pass)
		if state == "lost" {
			next := uuid.New()
			cliMustExec(t, f.pool, "UPDATE recovery_prerequisite_closures SET last_audit_pass_id=$2 WHERE id=$1", p.ID, next)
			cliMustExec(t, f.pool, `UPDATE recovery_prerequisite_closures SET state='lost',live_run_id=NULL,
 absent_since=NULL,absence_pass_id=NULL,cleanup_exhausted_at=NULL,reason='pin_lost',updated_at=now() WHERE id=$1`, p.ID)
		}
	}
	return id
}

func ownerSummarySnapshot(t *testing.T, f *ownerSummaryFixture) string {
	t.Helper()
	var out string
	// Full persisted rows include attempts, lease/audit timestamps, expiry and byte manifests.
	err := f.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'captures',(SELECT jsonb_agg(to_jsonb(c) ORDER BY c.id) FROM recovery_captures c WHERE c.user_id=$1),
 'closures',(SELECT jsonb_agg(to_jsonb(p) ORDER BY p.id) FROM recovery_prerequisite_closures p WHERE p.user_id=$1),
 'holds',(SELECT jsonb_agg(to_jsonb(h) ORDER BY h.id) FROM recovery_custody_holds h WHERE h.user_id=$1),
 'chunks',(SELECT jsonb_agg(to_jsonb(b) ORDER BY b.capture_id,b.chunk_index) FROM recovery_capture_chunks b JOIN recovery_captures c ON c.id=b.capture_id WHERE c.user_id=$1)
 )::text`, f.owner).Scan(&out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOwnerSummaryClosureHistoryAuthReadOnlyLiveDB(t *testing.T) {
	f := newOwnerSummaryFixture(t)
	ctx := context.Background()
	// A separate traced pool is used only by Summary. Auth and GetRun use cliLiveDB's
	// pool, so their SQL cannot be mistaken for the two recovery statements.
	cfg, err := pgxpool.ParseConfig(os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	tr := &ownerSummaryTrace{}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	limits := recoveryTestLimits()
	limits.MaxCapturesPerOwner = 2
	svc := recovery.New(store.New(pool), pool, f.h.box, limits, nil)
	var kicks atomic.Int32
	svc.SetPrerequisiteClosure([]string{"github"}, func(uuid.UUID, uuid.UUID) { kicks.Add(1) })
	f.h.recoverySvc = svc
	forges := &ownerSummaryForge{}
	f.h.wsvc.SetForges(forges)
	token := cliMintToken(t, f.pool, f.owner, clitoken.ScopeUser)
	jwt := cliMintJWT(t, f.pool, f.owner)
	url := "/api/runs/" + f.run.String() + "/archives"
	expected := map[string]struct {
		state             string
		absent, exhausted bool
	}{f.capture.String(): {"pending", false, false}}
	for _, s := range []string{"invoked", "create_unknown", "pinned", "refused", "foreign", "lost", "deleted"} {
		id := f.seedClosure(t, s, false, false)
		expected[id.String()] = struct {
			state             string
			absent, exhausted bool
		}{s, false, false}
	}
	for _, flags := range [][2]bool{{true, false}, {false, true}, {true, true}} {
		id := f.seedClosure(t, "pinned", flags[0], flags[1])
		expected[id.String()] = struct {
			state             string
			absent, exhausted bool
		}{"pinned", flags[0], flags[1]}
	}
	thin := f.seedCapture(t, true)
	self := f.seedCapture(t, false)
	// A different run and owner each have genuine closure rows; neither may leak.
	otherRun := rmSeedRun(t, f.pool, f.owner, func() uuid.UUID {
		var repo uuid.UUID
		if err := f.pool.QueryRow(ctx, "SELECT repo_id FROM runs WHERE id=$1", f.run).Scan(&repo); err != nil {
			t.Fatal(err)
		}
		return repo
	}(), "failed")
	otherHold := ownerSummarySeedHold(t, f.pool, f.owner, otherRun, f.worker)
	other := *f
	other.run = otherRun
	other.hold = otherHold
	other.seedClosure(t, "pinned", false, false)
	foreign := cliSeedUser(t, f.pool, false)
	foreignConn := rmSeedConn(t, f.pool, foreign)
	cliMustExec(t, f.pool, "UPDATE forge_connections SET forge_type='github',base_url='https://github.com' WHERE id=$1", foreignConn)
	fw := custodySeedWorker(t, f.pool, foreign)
	foreignRepo := rmSeedRepo(t, f.pool, foreignConn, 3729, true)
	fr := rmSeedRun(t, f.pool, foreign, foreignRepo, "failed")
	fh := ownerSummarySeedHold(t, f.pool, foreign, fr, fw)
	ff := *f
	ff.owner = foreign
	ff.run = fr
	ff.hold = fh
	ff.worker = fw
	ff.seedClosure(t, "pinned", false, false)

	read := func(cookie bool) apitypes.RecoveryArchiveSummaryDTO {
		t.Helper()
		var raw []byte
		if cookie {
			rec := cookieReq(t, f.router, http.MethodGet, url, jwt, "")
			if rec.Code != 200 {
				t.Fatalf("cookie summary=%d %s", rec.Code, rec.Body.String())
			}
			raw = rec.Body.Bytes()
		} else {
			rec := bearerReq(f.router, http.MethodGet, url, token)
			if rec.Code != 200 {
				t.Fatalf("bearer summary=%d %s", rec.Code, rec.Body.String())
			}
			raw = rec.Body.Bytes()
		}
		statements := tr.take()
		if len(statements) != 2 || !strings.Contains(statements[0], "GetRecoverySummaryForRun") || !strings.Contains(statements[1], "ListCapturesForRunOwner") {
			t.Fatalf("Summary SQL statements=%v; want aggregate then listing only", statements)
		}
		var got apitypes.RecoveryArchiveSummaryDTO
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		rows, err := f.pool.Query(ctx, "SELECT id FROM recovery_captures WHERE run_id=$1 AND user_id=$2 ORDER BY created_at", f.run, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id.String())
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != len(got.Archives) {
			t.Fatalf("complete history length=%d want %d", len(got.Archives), len(ids))
		}
		for i, a := range got.Archives {
			if a.ID != ids[i] {
				t.Fatalf("first ordering mismatch at %d: got %s want %s", i, a.ID, ids[i])
			}
			want, ok := expected[a.ID]
			if !ok {
				if a.PrerequisiteClosure != nil {
					t.Fatalf("no-row capture %s got closure", a.ID)
				}
				continue
			}
			p := a.PrerequisiteClosure
			if p == nil || p.State != want.state || p.AbsenceUnconfirmed != want.absent || p.CleanupExhausted != want.exhausted || p.PinRef != "refs/uzi-prereq/"+a.ID {
				t.Fatalf("closure %s=%+v want %+v", a.ID, p, want)
			}
			var updated string
			if err := f.pool.QueryRow(ctx, "SELECT to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM recovery_prerequisite_closures WHERE capture_id=$1", uuid.MustParse(a.ID)).Scan(&updated); err != nil {
				t.Fatal(err)
			}
			// Compare persisted time directly, with no request-time substitution.
			stored, err := store.New(f.pool).GetPrerequisiteClosureForCapture(ctx, uuid.MustParse(a.ID))
			if err != nil {
				t.Fatal(err)
			}
			if !p.UpdatedAt.Equal(stored.UpdatedAt.Time) {
				t.Fatalf("closure %s updated_at differs from persisted %s", a.ID, updated)
			}
		}
		var counts apitypes.RecoveryArchiveStateCountsDTO
		for _, a := range got.Archives {
			switch a.State {
			case "available":
				counts.Available++
			case "expired":
				counts.Expired++
			case "discarded":
				counts.Discarded++
			case "preparing":
				counts.Preparing++
			case "uploading":
				counts.Uploading++
			case "needs_action":
				counts.NeedsAction++
			}
		}
		if !reflect.DeepEqual(got.Counts, counts) {
			t.Fatalf("counts=%+v want %+v", got.Counts, counts)
		}
		return got
	}
	_ = thin
	_ = self
	before := ownerSummarySnapshot(t, f)
	read(false)
	read(true)
	if after := ownerSummarySnapshot(t, f); after != before {
		t.Fatal("repeated GET changed persisted capture/closure/hold/chunk rows")
	}
	// Grow historical rows past both the configured active quota (2) and the normal
	// owner quota (256). Each row is retained expired/discarded history, not active work.
	for i := 0; i < 270; i++ {
		id := f.seedCapture(t, false)
		state := "discarded"
		if i%2 == 0 {
			state = "expired"
		}
		cliMustExec(t, f.pool, "UPDATE recovery_captures SET state=$2,updated_at=now() WHERE id=$1", id, state)
	}
	before = ownerSummarySnapshot(t, f)
	read(false)
	if after := ownerSummarySnapshot(t, f); after != before {
		t.Fatal("grown-history GET changed persisted rows")
	}
	// Closed holds must retain identical metadata and full historical captures.
	cliMustExec(t, f.pool, "UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',live_worker_id=NULL,live_run_id=NULL,released_at=now(),updated_at=now() WHERE id=$1", f.hold)
	before = ownerSummarySnapshot(t, f)
	closed := read(true)
	read(false)
	if closed.HasOpenHold {
		t.Fatal("closed hold reported open")
	}
	if after := ownerSummarySnapshot(t, f); after != before {
		t.Fatal("closed-history GET changed persisted rows")
	}
	for _, tc := range []struct {
		name, token string
		want        int
	}{
		{"unauthenticated", "", 401},
		{"foreign", cliMintToken(t, f.pool, foreign, clitoken.ScopeUser), 404},
		{"admin-not-owner", cliMintToken(t, f.pool, cliSeedUser(t, f.pool, true), clitoken.ScopeAdminRO), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := bearerReq(f.router, http.MethodGet, url, tc.token)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	if forges.hits.Load() != 0 || kicks.Load() != 0 {
		t.Fatalf("GET forge builds=%d kicks=%d", forges.hits.Load(), kicks.Load())
	}
	// Closing only Summary's pool leaves auth healthy, and a SQL failure must stay 500.
	pool.Close()
	rec := bearerReq(f.router, http.MethodGet, url, token)
	if rec.Code != 500 {
		t.Fatalf("database failure became status=%d body=%s", rec.Code, rec.Body.String())
	}
	t.Log("verified complete retained history, two Summary statements, auth and read-only metadata")
}
