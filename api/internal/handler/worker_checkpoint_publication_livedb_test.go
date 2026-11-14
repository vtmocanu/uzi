package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4257: server-side ordering of the checkpoint_publication fact through the REAL worker
// state route and the live database. The store seam is wrapped only to hold ONE request at the
// store boundary, so a delayed older report meets a newer one the way a retry over a slow link does.

// holdClearStore blocks the first ClearRunCheckpointWithheld call before it reaches the database.
type holdClearStore struct {
	workersvc.Store
	entered chan struct{}
	release chan struct{}
	armed   bool
}

func (s *holdClearStore) ClearRunCheckpointWithheld(ctx context.Context, arg store.ClearRunCheckpointWithheldParams) (string, error) {
	if s.armed {
		s.armed = false
		close(s.entered)
		<-s.release
	}
	return s.Store.ClearRunCheckpointWithheld(ctx, arg)
}

type cpRoute struct {
	t      *testing.T
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	run    uuid.UUID
	token  string
	hold   *holdClearStore
	gen    int64
	nonce  string
}

func newCPRoute(t *testing.T) *cpRoute {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	hold := &holdClearStore{Store: store.New(pool), entered: make(chan struct{}), release: make(chan struct{})}
	h.wsvc = workersvc.New(hold, newHandlerTestBox(t), workersvc.Params{CustodyHoldLimit: defaultCustodyLimit()})
	h.wsvc.SetTxBeginner(pool)
	owner := cliSeedUser(t, pool, false)
	conn := rmSeedConn(t, pool, owner)
	repo := rmSeedRepo(t, pool, conn, 4257, true)
	run := rmSeedRun(t, pool, owner, repo, "claimed")
	r := &cpRoute{t: t, h: h, router: router, pool: pool, run: run, hold: hold, gen: 4, nonce: "cp-register-nonce-" + uuid.NewString()}
	worker := uuid.New()
	r.token = "cp-worker-" + uuid.NewString()
	sum := sha256.Sum256([]byte(r.token))
	cliMustExec(t, pool, "INSERT INTO workers(id,user_id,name,token_hash,status,protocol_capabilities,snapshot_register_nonce) VALUES($1,$2,'cp',$3,'online',$4,$5)",
		worker, owner, sum[:], []string{}, r.nonce)
	cliMustExec(t, pool, "UPDATE runs SET worker_id=$2, claim_generation=$3, claimed_at=now() - interval '1 hour' WHERE id=$1", run, worker, r.gen)
	if code, _ := r.post(`{"status":"running","claim_generation":4}`); code != http.StatusOK {
		t.Fatalf("running report: code=%d", code)
	}
	return r
}

func (r *cpRoute) post(body string) (int, map[string]json.RawMessage) {
	r.t.Helper()
	rec := bearerReqBody(r.router, http.MethodPost, "/api/worker/runs/"+r.run.String()+"/state", r.token, body)
	var ack map[string]json.RawMessage
	_ = json.Unmarshal(rec.Body.Bytes(), &ack)
	return rec.Code, ack
}

func (r *cpRoute) report(state string, seq int64, since time.Time) (int, string) {
	r.t.Helper()
	cp := map[string]any{"state": state, "claim_generation": r.gen, "register_nonce": r.nonce, "report_seq": seq}
	if state == "withheld" {
		cp["since"] = since.UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(map[string]any{"status": "running", "claim_generation": r.gen, "checkpoint_publication": cp})
	if err != nil {
		r.t.Fatal(err)
	}
	code, ack := r.post(string(body))
	var v string
	_ = json.Unmarshal(ack["checkpoint_publication"], &v)
	return code, v
}

func (r *cpRoute) fact() (since *time.Time, seq *int64) {
	r.t.Helper()
	if err := r.pool.QueryRow(r.t.Context(), `SELECT checkpoint_withheld_since, checkpoint_withheld_report_seq FROM runs WHERE id=$1`, r.run).Scan(&since, &seq); err != nil {
		r.t.Fatal(err)
	}
	return
}

func (r *cpRoute) wantACK(name, got, want string) {
	r.t.Helper()
	if got != want {
		r.t.Fatalf("%s: ACK = %q, want %q", name, got, want)
	}
}

// TestCheckpointPublicationServerOrderingLiveDB covers the four ordering behaviours at the route.
func TestCheckpointPublicationServerOrderingLiveDB(t *testing.T) {
	r := newCPRoute(t)
	s1 := time.Now().Add(-30 * time.Minute).UTC().Truncate(time.Second)

	// (1) an OLD clear (seq 3) is held at the store boundary while a NEWER withheld (seq 4) lands.
	// Releasing the old clear must not undo the newer fact, and its ACK is "refused".
	r.hold.armed = true
	type result struct {
		code int
		ack  string
	}
	done := make(chan result, 1)
	go func() {
		code, ack := r.report("cleared", 3, time.Time{})
		done <- result{code, ack}
	}()
	select {
	case <-r.hold.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the old clear never reached the store boundary")
	}
	code, ack := r.report("withheld", 4, s1)
	r.wantACK("newer withheld", ack, "recorded")
	if code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("newer withheld: code=%d", code)
	}
	close(r.hold.release)
	select {
	case res := <-done:
		r.wantACK("old clear", res.ack, "refused")
	case <-time.After(30 * time.Second):
		t.Fatal("the old clear never returned")
	}
	since, seq := r.fact()
	if since == nil || !since.Equal(s1) || seq == nil || *seq != 4 {
		t.Fatalf("the old clear undid the newer fact: since=%v seq=%v", since, seq)
	}

	// (2) a duplicate retry of the same marker is idempotent for withheld and for clear.
	_, ack = r.report("withheld", 4, s1.Add(5*time.Minute))
	r.wantACK("duplicate withheld", ack, "recorded")
	if since, seq = r.fact(); since == nil || !since.Equal(s1) || *seq != 4 {
		t.Fatalf("duplicate withheld changed the fact: since=%v seq=%v", since, seq)
	}
	_, ack = r.report("cleared", 5, time.Time{})
	r.wantACK("clear", ack, "cleared")
	_, ack = r.report("cleared", 5, time.Time{})
	r.wantACK("duplicate clear", ack, "cleared")
	if since, seq = r.fact(); since != nil || *seq != 5 {
		t.Fatalf("duplicate clear changed the fact: since=%v seq=%v", since, seq)
	}

	// (3) a lower-marker withheld after a higher one neither changes since nor revives a cleared fact.
	_, ack = r.report("withheld", 4, s1)
	r.wantACK("lower withheld after clear", ack, "refused")
	if since, _ = r.fact(); since != nil {
		t.Fatalf("a lower withheld revived the cleared fact: %v", since)
	}

	// (4) the marker survives the clear; a later higher withheld starts a FRESH fact.
	s2 := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	_, ack = r.report("withheld", 6, s2)
	r.wantACK("fresh withheld", ack, "recorded")
	if since, seq = r.fact(); since == nil || !since.Equal(s2) || *seq != 6 {
		t.Fatalf("fresh fact: since=%v seq=%v, want %v / 6", since, seq, s2)
	}
	_, ack = r.report("withheld", 5, s1)
	r.wantACK("lower withheld over a present fact", ack, "refused")
	if since, _ = r.fact(); since == nil || !since.Equal(s2) {
		t.Fatalf("a lower withheld moved since: %v", since)
	}
}

// TestCheckpointPublicationFixtureRouteLiveDB posts the shared worker-protocol fixture to the real
// route (only values substituted) and reads the projection the health pass consumes.
func TestCheckpointPublicationFixtureRouteLiveDB(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/checkpoint-publication-withheld-running.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	r := newCPRoute(t)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	cp, ok := m["checkpoint_publication"].(map[string]any)
	if !ok {
		t.Fatalf("fixture checkpoint_publication is not an object: %T", m["checkpoint_publication"])
	}
	m["claim_generation"] = r.gen
	cp["claim_generation"] = r.gen
	cp["register_nonce"] = r.nonce
	since := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	cp["since"] = since.Format(time.RFC3339)
	body, _ := json.Marshal(m)
	// an unknown nested key is refused by the strict decoder before anything is written
	cp["path"] = "secret/file"
	bad, _ := json.Marshal(m)
	if code, _ := r.post(string(bad)); code != http.StatusBadRequest {
		t.Fatalf("unknown nested key: code=%d, want 400", code)
	}
	if s, seq := r.fact(); s != nil || seq != nil {
		t.Fatalf("a 400 wrote the fact: %v %v", s, seq)
	}
	code, ack := r.post(string(body))
	if code != http.StatusOK && code != http.StatusConflict {
		t.Fatalf("fixture post: code=%d", code)
	}
	var got string
	_ = json.Unmarshal(ack["checkpoint_publication"], &got)
	r.wantACK("fixture", got, "recorded")

	rows, err := store.New(r.pool).ListActiveRunsForHealth(t.Context(), []string{})
	if err != nil {
		t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, row := range rows {
		if row.ID == r.run {
			if !row.PublicationWithheldSince.Valid || !row.PublicationWithheldSince.Time.Equal(since) {
				t.Fatalf("projection = %+v, want %v", row.PublicationWithheldSince, since)
			}
			return
		}
	}
	t.Fatal("run missing from the health read")
}
