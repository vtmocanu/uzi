package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #108 M1a — the poison-pill repro, against the real path (handler ->
// workersvc.AppendMessages -> Postgres), not a mock of it.
//
// The incident: a headless Chromium's stderr reached a tool_result payload
// carrying 84 NUL bytes; run_messages.payload is jsonb, which cannot represent
// one; AppendMessages returned the store error; WorkerRunMessages' default: arm
// mapped it to 500; the worker's batcher treats any throw as retryable and
// re-posted the identical batch at ~2 Hz for 27 minutes.
//
// These MUST be live-DB tests. The whole defect lives in what Postgres does with
// the bytes — a fake store accepts every one of these payloads and would report a
// tidy green against the unfixed code.
//
// Measured against the unfixed tree on 2026-07-21 (throwaway postgres:17):
//
//	jsonb, the six-byte u0000 escape ...... SQLSTATE 22P05 unsupported Unicode escape sequence
//	jsonb, a lone surrogate escape ........ SQLSTATE 22P02 invalid input syntax for type json
//	jsonb, a raw 0xff byte ................ SQLSTATE 22021 invalid byte sequence for encoding "UTF8"
//	text  (agent_label), a raw NUL byte ... SQLSTATE 22021 invalid byte sequence for encoding "UTF8"
//
// all four surfacing as HTTP 500. A batch over the 1 MiB body cap is a 400
// instead — the second poison trigger, and the class change that disarms a
// "same error each time" guard.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

// The hostile escapes, built by concatenation so the six-character sequence is
// never a literal in this source. A literal one is fragile in a way that matters
// here: an editor, a formatter or a copy-paste that folds it into a real 0x00
// byte turns every fixture below into a payload that fails DECODE (400) instead
// of reaching the sink it targets — a test that then passes vacuously while
// proving nothing about the store.
var (
	poisonNulEsc = `\` + `u0000`
	poisonHiSurr = `\` + `ud800`
	// An escaped BACKSLASH followed by u0000. Legal JSON data that decodes to the
	// six literal characters of the escape itself, and the exact string a naive
	// byte-substring strip would corrupt.
	poisonEscapedBackslash = `\` + `\` + `u0000`
)

// poisonFixture is one seeded (worker, run) pair on a live database, plus the
// handler wired to it. Every test derives fresh uuids: the LiveDB runner shares
// one database across the whole suite, workers.token_hash is UNIQUE, and
// uq_runs_one_active_per_issue is unique per (repo_id, issue_iid) for non-terminal
// runs, so fixed literals would collide across tests.
type poisonFixture struct {
	h     *Handler
	pool  *pgxpool.Pool
	wkr   store.Worker
	runID uuid.UUID
}

func newPoisonFixture(t *testing.T) poisonFixture {
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

	q := store.New(pool)
	box := newHandlerTestBox(t)
	h := &Handler{pool: pool, q: q, box: box, wsvc: workersvc.New(q, box, workersvc.Params{})}

	userID, connID, repoID, workerID, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.Fields(strings.TrimSpace(sql))[1], err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("poison-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main', true)`,
		repoID, connID, rand.Int63n(1<<40), fmt.Sprintf("g/r-%s", repoID))
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'poison', $3)`,
		workerID, userID, append([]byte("poison-"), workerID[:]...))
	exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id)
	      VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5)`, runID, userID, repoID, rand.Int63n(1<<40), workerID)

	return poisonFixture{
		h:     h,
		pool:  pool,
		wkr:   store.Worker{ID: workerID, UserID: userID},
		runID: runID,
	}
}

// post drives the REAL handler entry point with these exact body bytes, the way
// the router + RequireWorker would. Body bytes, not a decoded struct: the escapes
// under test only exist on the wire, and json.RawMessage is what carries them
// through untouched.
func (f poisonFixture) post(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/messages", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", f.runID.String())
	ctx := context.WithValue(mw.ContextWithWorker(req.Context(), f.wkr), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	f.h.WorkerRunMessages(rec, req.WithContext(ctx))
	return rec
}

// diagnose says WHICH of the two stages rejected this body, and reports the
// rejection verbatim. It exists because "status = 500" and "status = 400" are
// different defects on this route — a store rejection versus a decode rejection —
// and a failure message that named the wrong one would send the next reader into
// the wrong subsystem. It asserts nothing; it only measures, and each branch
// reports only what that branch observed.
func (f poisonFixture) diagnose(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Messages []workersvc.IncomingMessage `json:"messages"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return fmt.Sprintf("the batch never reached the store: the JSON decode of the %d-byte body failed (%v)", len(body), err)
	}
	err := f.h.wsvc.AppendMessages(context.Background(), f.wkr, f.runID, req.Messages)
	if err == nil {
		return "a direct AppendMessages of the same batch succeeded, so the rejection did not come from the store"
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Sprintf("the store rejected the insert: SQLSTATE %s %q", pgErr.Code, pgErr.Message)
	}
	if errors.Is(err, workersvc.ErrInvalidMessage) {
		return fmt.Sprintf("the batch was rejected by AppendMessages' own validation before any insert: %v", err)
	}
	return fmt.Sprintf("AppendMessages returned a non-Postgres error: %v", err)
}

// storedText reads one field back out of the persisted row through Postgres'
// own JSON accessor, so the assertion holds a VALUE and not a byte pattern —
// jsonb does not preserve key order or whitespace, so there is no byte-fidelity
// claim to be made here in the first place.
func (f poisonFixture) storedPayloadField(t *testing.T, seq int, key string) (string, bool) {
	t.Helper()
	return f.scanOne(t, seq, `payload ->> `+fmt.Sprintf("'%s'", key))
}

func (f poisonFixture) storedColumn(t *testing.T, seq int, column string) (string, bool) {
	t.Helper()
	return f.scanOne(t, seq, column)
}

// scanOne reads one projection off the row, distinguishing "no such row" (the
// answer this suite cares about) from every other database error, which is a
// broken fixture and must never be reported as an absent row. expr is a
// test-local literal, never request data.
func (f poisonFixture) scanOne(t *testing.T, seq int, expr string) (string, bool) {
	t.Helper()
	var v *string
	err := f.pool.QueryRow(context.Background(),
		`SELECT `+expr+` FROM run_messages WHERE run_id = $1 AND seq = $2`, f.runID, seq).Scan(&v)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "", false
	case err != nil:
		t.Fatalf("read %s for seq %d: %v", expr, seq, err)
	case v == nil:
		return "", false
	}
	return *v, true
}

func (f poisonFixture) lastSeq(t *testing.T) int32 {
	t.Helper()
	var n int32
	if err := f.pool.QueryRow(context.Background(), `SELECT last_seq FROM runs WHERE id = $1`, f.runID).Scan(&n); err != nil {
		t.Fatalf("read runs.last_seq: %v", err)
	}
	return n
}

// requireFixtureIsHostile is the positive control on the FIXTURE, checked before
// every POST. A payload clamped, trimmed or mangled on its way into this test
// would reach a sink it never intended and pass vacuously; this asserts the exact
// bytes under test are still in the body at the moment it is sent.
func requireFixtureIsHostile(t *testing.T, body []byte, wantSub string) {
	t.Helper()
	if !bytes.Contains(body, []byte(wantSub)) {
		t.Fatalf("the fixture lost its poison before it was sent: %q is not in the %d-byte body — "+
			"this test would exercise a benign payload and pass without proving anything", wantSub, len(body))
	}
}

// The incident itself. A tool_result payload carrying the six-byte u0000 escape
// must be persisted SANITIZED and the run must continue.
//
// Unfixed, this returns 500 (SQLSTATE 22P05) and the batcher re-posts forever.
func TestWorkerMessagesNulEscapeInPayloadLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result","agent":"lead","payload":{"t":"a` + poisonNulEsc + `b"}}]}`)
	requireFixtureIsHostile(t, body, poisonNulEsc)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a u0000 escape in the payload: status = %d, want 204 — %s",
			rec.Code, f.diagnose(t, body))
	}
	got, ok := f.storedPayloadField(t, 1, "t")
	if !ok {
		t.Fatal("the handler answered 204 but no run_messages row with seq=1 exists — nothing was persisted")
	}
	if want := "ab"; got != want {
		t.Errorf("persisted payload.t = %q (% x), want %q — the NUL must be stripped and the "+
			"surrounding bytes left alone", got, got, want)
	}
	if strings.ContainsRune(got, 0) {
		t.Errorf("a NUL survived into the persisted payload: % x", got)
	}
	if n := f.lastSeq(t); n != 1 {
		t.Errorf("runs.last_seq = %d, want 1 — the run's high-water mark did not advance, so the run did not continue", n)
	}
}

// A lone surrogate: the realistic Node-side trigger, since a well-formed
// JSON.stringify emits a lone \udXXX escape whenever a JS string was sliced
// mid-surrogate-pair — so ANY worker-side truncation of tool output can produce
// one. PRD #108 predicted jsonb rejects it; measured, it does, with 22P02
// (invalid input syntax for type json) rather than 22P05.
func TestWorkerMessagesLoneSurrogateInPayloadLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result","agent":"lead","payload":{"t":"a` + poisonHiSurr + `b"}}]}`)
	requireFixtureIsHostile(t, body, poisonHiSurr)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a lone surrogate escape in the payload: status = %d, want 204 — %s",
			rec.Code, f.diagnose(t, body))
	}
	got, ok := f.storedPayloadField(t, 1, "t")
	if !ok {
		t.Fatal("the handler answered 204 but no run_messages row with seq=1 exists — nothing was persisted")
	}
	if want := "a�b"; got != want {
		t.Errorf("persisted payload.t = %q (% x), want %q — an unpaired surrogate must become U+FFFD", got, got, want)
	}
}

// A PAIRED surrogate is legal and must survive untouched. Without this, a strip
// that flattened every \udXXX escape to U+FFFD would pass the test above while
// destroying every emoji in every tool result.
func TestWorkerMessagesPairedSurrogateSurvivesLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	pair := `\` + `ud83d` + `\` + `ude00` // U+1F600
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result","agent":"lead","payload":{"t":"a` + pair + `b"}}]}`)
	requireFixtureIsHostile(t, body, pair)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a well-formed surrogate PAIR: status = %d, want 204 — %s", rec.Code, f.diagnose(t, body))
	}
	got, _ := f.storedPayloadField(t, 1, "t")
	if want := "a\U0001F600b"; got != want {
		t.Errorf("persisted payload.t = %q, want %q — a well-formed surrogate pair is legal jsonb "+
			"and must not be touched by the unpaired-surrogate strip", got, want)
	}
}

// Raw invalid UTF-8 in the payload. PRD #108 predicted SQLSTATE 22021 and noted
// json.Valid does not validate UTF-8; both measured true — the 0xff rides through
// Go's scanner (json.Valid = true, utf8.Valid = false) and Postgres rejects it.
func TestWorkerMessagesRawInvalidUTF8InPayloadLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result","agent":"lead","payload":{"t":"a\xffb"}}]}`)
	body = bytes.Replace(body, []byte(`\xff`), []byte{0xff}, 1)
	if !bytes.Contains(body, []byte{0xff}) {
		t.Fatal("the fixture lost its raw 0xff byte before it was sent")
	}
	if utf8.Valid(body) {
		t.Fatal("the fixture body is valid UTF-8, so it carries no invalid sequence to test")
	}

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a raw 0xff byte in the payload: status = %d, want 204 — %s",
			rec.Code, f.diagnose(t, body))
	}
	got, ok := f.storedPayloadField(t, 1, "t")
	if !ok {
		t.Fatal("the handler answered 204 but no run_messages row with seq=1 exists — nothing was persisted")
	}
	if want := "a�b"; got != want {
		t.Errorf("persisted payload.t = %q (% x), want %q — an invalid UTF-8 byte must become U+FFFD", got, got, want)
	}
}

// The sibling TEXT columns. agent / agent_instance / agent_label are
// worker-controlled and inserted verbatim (truncateRunes truncates but strips
// nothing), and Postgres text cannot hold a NUL any more than jsonb can — so this
// wedges a run identically and ENTIRELY OUTSIDE a payload-only fix.
//
// Measured asymmetry, and it is why this case needs its own test: encoding/json
// decodes the u0000 escape into these Go strings as a REAL 0x00 byte (so the text
// columns need NUL handling), while it has ALREADY replaced a lone surrogate and
// a raw invalid byte with U+FFFD before the value is ever seen (so they do not
// need surrogate or UTF-8 handling). The payload, being json.RawMessage, is the
// mirror image: nothing is decoded, so all three survive verbatim.
func TestWorkerMessagesNulEscapeInTextColumnsLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result",` +
		`"agent":"a` + poisonNulEsc + `b",` +
		`"agent_instance":"c` + poisonNulEsc + `d",` +
		`"agent_label":"e` + poisonNulEsc + `f",` +
		`"payload":{"t":"clean"}}]}`)
	requireFixtureIsHostile(t, body, poisonNulEsc)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a u0000 escape in agent/agent_instance/agent_label: status = %d, want 204 — %s",
			rec.Code, f.diagnose(t, body))
	}
	for _, tc := range []struct{ column, want string }{
		{"agent", "ab"},
		{"agent_instance", "cd"},
		{"agent_label", "ef"},
	} {
		got, ok := f.storedColumn(t, 1, tc.column)
		if !ok {
			t.Errorf("%s: no value persisted", tc.column)
			continue
		}
		if got != tc.want {
			t.Errorf("persisted %s = %q (% x), want %q — the NUL must be stripped from the sibling "+
				"text columns too, not just the payload", tc.column, got, got, tc.want)
		}
	}
}

// The naive-replace trap, as its own test rather than a footnote. An escaped
// BACKSLASH followed by u0000 is legal data: it decodes to the six characters of
// the escape itself as literal text, which a byte-substring strip corrupts. This
// one is GREEN against the unfixed tree and must stay green — it is the guard on
// the fix, not a repro of the bug.
func TestWorkerMessagesEscapedBackslashU0000IsNotStrippedLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool_result",` +
		`"agent_label":"L` + poisonEscapedBackslash + `M",` +
		`"payload":{"t":"a` + poisonEscapedBackslash + `b"}}]}`)
	requireFixtureIsHostile(t, body, poisonEscapedBackslash)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages carrying the LEGAL literal text %s: status = %d, want 204 — %s",
			poisonEscapedBackslash, rec.Code, f.diagnose(t, body))
	}
	literal := `\` + `u0000` // the seven characters the escape above decodes to
	got, ok := f.storedPayloadField(t, 1, "t")
	if !ok {
		t.Fatal("the handler answered 204 but no run_messages row with seq=1 exists — nothing was persisted")
	}
	if want := "a" + literal + "b"; got != want {
		t.Errorf("persisted payload.t = %q, want %q — %s is an escaped backslash followed by u0000, "+
			"i.e. legal data. A byte-substring strip corrupts it; the strip must be JSON-aware",
			got, want, poisonEscapedBackslash)
	}
	label, _ := f.storedColumn(t, 1, "agent_label")
	if want := "L" + literal + "M"; label != want {
		t.Errorf("persisted agent_label = %q, want %q — same trap on the text columns", label, want)
	}
}

// The SECOND poison trigger, found by review rather than by the incident: the
// retry batch GROWS (doFlush re-buffers the whole batch ahead of new messages —
// the incident's own 206 -> 239), and DecodeJSON caps bodies at 1 MiB via an
// io.LimitReader. So a long-enough wedge crosses the cap and the failure CHANGES
// CLASS, from a 500 store rejection to a 400 decode failure.
//
// This is a characterization test, not a repro: 400 is already the honest answer
// here and M2 does not change it. It is what makes Track B's byte cap mandatory
// (a 4xx-is-fatal batcher without one would permanently fail a healthy run that
// merely grew across the cap), and it is what disarms any "same error each time"
// guard — which is why the class change has to be pinned rather than assumed.
func TestWorkerMessagesOversizedBatchIsADifferentClassLiveDB(t *testing.T) {
	f := newPoisonFixture(t)

	// A benign batch of the same shape, comfortably over the 1 MiB cap.
	var b strings.Builder
	b.WriteString(`{"messages":[`)
	const filler = 4096
	for i := 1; i <= 300; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"seq":%d,"kind":"text","agent":"lead","payload":{"t":%q}}`, i, strings.Repeat("x", filler))
	}
	b.WriteString(`]}`)
	body := []byte(b.String())
	if len(body) <= 1<<20 {
		t.Fatalf("the oversized fixture is %d bytes, which is under the 1 MiB cap it exists to cross", len(body))
	}

	rec := f.post(t, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /messages with a %d-byte body (over the 1 MiB DecodeJSON cap): status = %d, want 400 — %s",
			len(body), rec.Code, f.diagnose(t, body))
	}
	// And the class really is different: nothing landed, and the same batch under
	// the cap is a plain 204. Without this half, "400" alone would be consistent
	// with the route rejecting the SHAPE rather than the SIZE.
	if n := f.lastSeq(t); n != 0 {
		t.Errorf("runs.last_seq = %d after a rejected oversized batch, want 0 — the batch must be all-or-nothing", n)
	}
	small := []byte(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":{"t":"small"}}]}`)
	if rec := f.post(t, small); rec.Code != http.StatusNoContent {
		t.Fatalf("the same message shape UNDER the cap: status = %d, want 204 — the 400 above would then "+
			"be about the batch's shape, not its size, and this test would not be measuring the cap at all",
			rec.Code)
	}
}
