package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
//	jsonb, 1e1000000 ...................... SQLSTATE 22003 value overflows numeric format
//
// The last one is not sanitizable — jsonb stores numbers as `numeric` and there is
// no benign rewriting of an out-of-range one — so it is closed by the status code
// alone, which is the case the 400 mapping exists for.
//
// all four surfacing as HTTP 500. A batch over the 1 MiB body cap was a 400 —
// the second poison trigger, and the class change that disarms a "same error
// each time" guard. It is a 413 as of M2's follow-up: the worker answers oversize
// by splitting and poison by bisecting, and it could not tell the two apart while
// they shared a status code and a generic body.
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
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /messages with a %d-byte body (over the 1 MiB cap): status = %d, want 413 — %s",
			len(body), rec.Code, f.diagnose(t, body))
	}
	// The body must be uzi's own prose. net/http's MaxBytesError.Error() is the
	// fixed literal "http: request body too large", pinned by Hyrum's law in the
	// stdlib; echoing it would couple this wire contract to a stdlib constant.
	if strings.Contains(rec.Body.String(), "http:") {
		t.Errorf("the 413 body echoes the net/http error string: %s", rec.Body.String())
	}
	// 413 must be DISTINGUISHABLE from the 400 the poison path returns, which is
	// the entire reason it exists: the worker answers one by splitting and the
	// other by bisecting, and it cannot tell them apart from a shared 400 without
	// prose-matching an error string. Assert the separation, not just the code.
	poison := []byte(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":"unclosed`)
	if rec := f.post(t, poison); rec.Code != http.StatusBadRequest {
		t.Errorf("a MALFORMED (not oversized) body: status = %d, want 400 — if this is also 413 then the "+
			"two causes are still indistinguishable and the split is decorative", rec.Code)
	}
	// Nothing landed, and the same shape under the cap is a plain 204. Without this
	// half, the 413 would be consistent with the route rejecting the SHAPE rather
	// than the SIZE, and this test would not be measuring the cap at all.
	if n := f.lastSeq(t); n != 0 {
		t.Errorf("runs.last_seq = %d after a rejected oversized batch, want 0", n)
	}
	small := []byte(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":{"t":"small"}}]}`)
	if rec := f.post(t, small); rec.Code != http.StatusNoContent {
		t.Fatalf("the same message shape UNDER the cap: status = %d, want 204", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// The 400 arm itself (PRD #108 M2b). No live database: after sanitation the
// known triggers no longer reach Postgres, so the only honest way to exercise
// this arm is to inject the rejection the classifier is built to recognise.
// ---------------------------------------------------------------------------

// unstorableStore fails every insert with a fixed error. It overrides exactly the
// two methods AppendMessages reaches before that error, so anything else it is
// asked for panics rather than quietly returning a zero value.
type unstorableStore struct {
	workersvc.Store
	insertErr error
}

func (u *unstorableStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return store.Run{}, nil
}

func (u *unstorableStore) InsertRunMessage(context.Context, store.InsertRunMessageParams) (int64, error) {
	return 0, u.insertErr
}

func postToFakeStore(t *testing.T, insertErr error) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{wsvc: workersvc.New(&unstorableStore{insertErr: insertErr}, newHandlerTestBox(t), workersvc.Params{})}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/messages",
		strings.NewReader(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":{"t":"clean"}}]}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	ctx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()}), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, req.WithContext(ctx))
	return rec
}

// THE load-bearing fix. 500 means "try again", and the batcher believes it: a
// permanent failure answered 500 is what turned one poisoned payload into a
// 27-minute wedge. Each of the three enumerated SQLSTATEs must reach 400.
func TestWorkerMessagesUnstorableStoreErrorReturns400(t *testing.T) {
	for _, code := range []string{"22P05", "22P02", "22021"} {
		rec := postToFakeStore(t, &pgconn.PgError{Code: code, Message: "boom"})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("SQLSTATE %s: status = %d, want 400 — the worker reads 5xx as retryable and "+
				"will re-post this identical batch forever", code, rec.Code)
		}
	}
}

// The mirror-image bug, and the worse one: a transient failure answered 400 fails
// a healthy run that would have succeeded on the next attempt. These must all
// stay on the retry path.
func TestWorkerMessagesTransientStoreErrorStays500(t *testing.T) {
	for _, code := range []string{"08006", "53300", "40P01", "57014", "23503", "22001"} {
		rec := postToFakeStore(t, &pgconn.PgError{Code: code, Message: "boom"})
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("SQLSTATE %s: status = %d, want 500 — classifying a retryable failure as permanent "+
				"kills healthy runs, which is strictly worse than the bug being fixed", code, rec.Code)
		}
	}
	// A non-Postgres error — context.Canceled from a worker that hung up mid-batch
	// is the realistic one — has no SQLSTATE to consult and must never be permanent.
	if rec := postToFakeStore(t, errors.New("context canceled")); rec.Code != http.StatusInternalServerError {
		t.Errorf("a non-Postgres store error: status = %d, want 500", rec.Code)
	}
}

// A jsonb numeric overflow: json.Valid, survives every class the sanitizer
// strips, and permanently unstorable because jsonb stores numbers as `numeric`
// (measured: 1e1000000, SQLSTATE 22003 "value overflows numeric format").
//
// This test previously asserted 500 and named itself a residual — the enumerated
// set was the PRD's three and this fell outside it. That shipped a demonstrated
// poison pill still on the retry-forever path, which falsifies the PRD's own
// Success Criterion 2, so the set is four and this is now a 400. The failure below
// is deliberately the mirror of what the old one said, so the next reader can see
// which way the decision went rather than only that it was made.
func TestWorkerMessagesNumericOverflowReturns400LiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":{"n":1e1000000}}]}`)

	rec := f.post(t, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a jsonb numeric overflow: status = %d, want 400 — 22003 is permanent by construction "+
			"(the value determines the failure, not the environment), so a 500 tells the worker to retry "+
			"a batch that can never land. %s", rec.Code, f.diagnose(t, body))
	}
	if n := f.lastSeq(t); n != 0 {
		t.Errorf("runs.last_seq = %d after a rejected batch, want 0", n)
	}
}

// The ordering claim the whole classifier placement rests on, made mechanical
// rather than left as an assertion in a comment.
//
// foldRunUsage inserts a MODEL NAME into run_usage.model, and that name is a JSON
// object key decoded out of the worker's own payload — a second text sink on this
// path, reached after the insert. It is safe only because sanitation writes
// through the index (m := &msgs[i]) in a pass that completes BEFORE foldRunUsage
// iterates msgs, so the fold reads bytes that have already been cleaned. If that
// ordering ever inverts, this route grows a second permanent wedge in a place no
// other test looks.
//
// A model name carrying the u0000 escape is the probe: unsanitized it reaches
// Postgres as a real NUL and the fold raises 22021; sanitized it lands clean.
func TestWorkerMessagesUsageFoldSeesSanitizedModelNamesLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"status","agent":"lead","payload":{` +
		`"event":"result","modelUsage":{"claude-a` + poisonNulEsc + `b":{"inputTokens":10,"outputTokens":5,"costUSD":0.01}}}}]}`)
	requireFixtureIsHostile(t, body, poisonNulEsc)

	// A precondition, not the assertion: this only says the batch persisted at all.
	// It cannot distinguish the insert from the fold — both read m.Payload, so
	// either one rejecting shows up here identically — which is why the message
	// names neither and the real claim is the column read below.
	if rec := f.post(t, body); rec.Code != http.StatusNoContent {
		t.Fatalf("a result frame whose MODEL NAME carries a u0000 escape did not persist: status = %d, "+
			"want 204. Some sink on this path read the payload before sanitation; the column check below "+
			"is what would have told them apart. %s", rec.Code, f.diagnose(t, body))
	}
	// THE assertion, and the only one specific to the fold: run_usage.model exists
	// solely because foldRunUsage ran, and its contents are what that call inserted.
	var model string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT model FROM run_usage WHERE run_id = $1`, f.runID).Scan(&model); err != nil {
		t.Fatalf("no run_usage row for this run (%v) — the fold never ran, so this test measured "+
			"nothing about the ordering it exists to pin", err)
	}
	if want := "claude-ab"; model != want {
		t.Errorf("run_usage.model = %q (% x), want %q — foldRunUsage read the payload as it arrived "+
			"rather than as sanitation left it, so run_usage.model is a second unstorable sink on this "+
			"path and the strip is not covering it", model, model, want)
	}
}

// foldingStore fails a chosen store call OTHER than the insert, so the narrowness
// of the unstorable classification can be exercised. The insert itself always
// succeeds here.
type foldingStore struct {
	workersvc.Store
	lastSeqErr error
}

func (s *foldingStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return store.Run{}, nil
}

func (s *foldingStore) InsertRunMessage(context.Context, store.InsertRunMessageParams) (int64, error) {
	return 1, nil
}

func (s *foldingStore) UpdateRunLastSeq(context.Context, store.UpdateRunLastSeqParams) (int64, error) {
	return 0, s.lastSeqErr
}

// THE narrowness guard. 400 on this route tells the worker "this batch is
// permanently poisoned, stop retrying it", and only the INSERT's rejection is
// evidence for that. A 22P05 raised anywhere else concerns a value the worker did
// not send — UpdateRunLastSeq takes the run id and an int — so reporting it as
// the batch's fault makes the worker drop messages that were never the problem.
//
// This is the test that fails if the classifier is ever moved back out to the
// function boundary, where it cannot tell the two apart.
func TestWorkerMessagesUnstorableOutsideTheInsertStays500(t *testing.T) {
	h := &Handler{wsvc: workersvc.New(
		&foldingStore{lastSeqErr: &pgconn.PgError{Code: "22P05", Message: "boom"}},
		newHandlerTestBox(t), workersvc.Params{})}
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/messages",
		strings.NewReader(`{"messages":[{"seq":1,"kind":"text","agent":"lead","payload":{"t":"clean"}}]}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	ctx := context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()}), chi.RouteCtxKey, rctx)
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, req.WithContext(ctx))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("a 22P05 raised by UpdateRunLastSeq (not by the insert): status = %d, want 500 — the "+
			"batch's own messages were accepted, so telling the worker they are permanently poisoned "+
			"makes it drop data that was never at fault", rec.Code)
	}
}

// A partially-applied batch must leave runs.last_seq at what ACTUALLY landed.
//
// The insert loop is not transactional, so [good, good, poison] commits the first
// two. If the high-water mark stays behind them, the resumed worker restarts from
// the stale value and re-emits those seq numbers carrying DIFFERENT content; the
// idempotent insert answers rows == 0, the server reads a re-delivery, and the new
// content is silently dropped. The poison here is the measured jsonb numeric
// overflow, which is the only unstorable payload that survives sanitation.
func TestWorkerMessagesPartialBatchAdvancesLastSeqLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[` +
		`{"seq":1,"kind":"text","agent":"lead","payload":{"t":"one"}},` +
		`{"seq":2,"kind":"text","agent":"lead","payload":{"t":"two"}},` +
		`{"seq":3,"kind":"text","agent":"lead","payload":{"n":1e1000000}}]}`)

	if rec := f.post(t, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("a batch whose third message the database refuses: status = %d, want 400 (22003 is in "+
			"the enumerated permanent set) — %s", rec.Code, f.diagnose(t, body))
	}
	// The first two really did commit: that is what makes the stale mark harmful
	// rather than merely untidy, so assert it rather than assume it.
	for _, seq := range []int{1, 2} {
		if _, ok := f.storedPayloadField(t, seq, "t"); !ok {
			t.Fatalf("seq %d is absent — the insert loop is transactional after all, and this test is "+
				"measuring something other than a partial apply", seq)
		}
	}
	if _, ok := f.storedPayloadField(t, 3, "n"); ok {
		t.Error("the poisoned seq 3 was persisted; the database was supposed to refuse it")
	}
	if n := f.lastSeq(t); n != 2 {
		t.Errorf("runs.last_seq = %d, want 2 — it must advance to the last seq that actually landed. "+
			"Left at 0, the worker resumes from before seq 1, re-emits 1 and 2 with different content, "+
			"and the idempotent insert silently discards it", n)
	}
}

// agent had NO cap before PRD #108 M2 — not "truncated but not stripped" like its
// two siblings, but wholly unbounded into an unbounded text column, on the same
// untrusted route and repeated on every frame of an invocation.
func TestWorkerMessagesAgentIsCappedLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	long := strings.Repeat("a", 500)
	body := []byte(`{"messages":[{"seq":1,"kind":"text","agent":"` + long + `","payload":{"t":"x"}}]}`)

	if rec := f.post(t, body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 — an over-long agent is truncated, never rejected: a rejected "+
			"batch is a lost message and this field is attribution. %s", rec.Code, f.diagnose(t, body))
	}
	got, ok := f.storedColumn(t, 1, "agent")
	if !ok {
		t.Fatal("no agent persisted")
	}
	if len(got) != 64 {
		t.Errorf("persisted agent is %d bytes, want 64 (agenttmpl.MaxNameLen) — an untrusted "+
			"worker field reached an unbounded text column", len(got))
	}
}

// `kind` is the FIFTH worker-controlled sink, and the one the PRD and my own M2
// both missed. It is `text NOT NULL` with its vocabulary in a COMMENT and no
// CHECK constraint (00020_workers_runs.sql), and it decodes into a Go string
// exactly like agent/agent_instance/agent_label — so a u0000 escape becomes a
// real 0x00 and Postgres answers 22021.
//
// Before the strip, M2's status code already contained this: the batch returned
// 400, not 500, so the run could not wedge. That is the contract working exactly
// where sanitation was incomplete — but the message was permanently DROPPED where
// a strip would have persisted it, and once M3's bisection lands that drop becomes
// designed behaviour rather than an accident.
func TestWorkerMessagesNulEscapeInKindLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"tool` + poisonNulEsc + `_result","agent":"lead","payload":{"t":"x"}}]}`)
	requireFixtureIsHostile(t, body, poisonNulEsc)

	rec := f.post(t, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /messages with a u0000 escape in KIND: status = %d, want 204 — %s", rec.Code, f.diagnose(t, body))
	}
	got, ok := f.storedColumn(t, 1, "kind")
	if !ok {
		t.Fatal("the handler answered 204 but no run_messages row with seq=1 exists — the message was dropped, not persisted")
	}
	if want := "tool_result"; got != want {
		t.Errorf("persisted kind = %q (% x), want %q — the NUL must be stripped from kind too", got, got, want)
	}
}

// A kind of NOTHING BUT NUL escapes is the ordering trap: it is non-empty on the
// wire, so it passes the emptiness check, and would then strip to "" — which
// `text NOT NULL` accepts happily, persisting a message with no kind at all.
// Rejecting it is what the emptiness check exists to do.
func TestWorkerMessagesKindOfOnlyNULsIsRejectedLiveDB(t *testing.T) {
	f := newPoisonFixture(t)
	body := []byte(`{"messages":[{"seq":1,"kind":"` + poisonNulEsc + poisonNulEsc + `","agent":"lead","payload":{"t":"x"}}]}`)
	requireFixtureIsHostile(t, body, poisonNulEsc)

	if rec := f.post(t, body); rec.Code != http.StatusBadRequest {
		t.Fatalf("a kind of nothing but NUL escapes: status = %d, want 400 — it is non-empty on the wire, "+
			"so it passes the emptiness check and then strips to \"\", which text NOT NULL accepts. %s",
			rec.Code, f.diagnose(t, body))
	}
	if _, ok := f.storedColumn(t, 1, "kind"); ok {
		t.Error("a message with an all-NUL kind was persisted; it must be rejected before the insert")
	}
}

// The 500 arm logs the WRAPPED error, and its safety rests on a stdlib detail
// rather than on anything this file does: slog special-cases values implementing
// `error` and renders them via Error(). pgconn.PgError.Error() emits only
// Severity + Message + SQLSTATE, so Detail/Where/File/Hint never reach the line.
//
// This is a TEST rather than a comment because a comment cannot stop the
// "improvement" it warns about. Attaching the error as a plain value ("pg",
// pgErr) or moving to a handler that marshals structs field by field would start
// leaking worker-controlled bytes into the logs — and for 22P02/22021 those
// fields quote the offending payload. Both handlers are exercised: the risk is a
// property of how the error is ATTACHED, not of which handler is configured.
func TestWorkerMessagesLogDoesNotLeakPgErrorFields(t *testing.T) {
	// A code deliberately OUTSIDE the enumerated permanent set, so this reaches the
	// 500 arm — the only arm that logs the error at all.
	pgErr := &pgconn.PgError{
		Code:     "22001",
		Message:  "value too long for type character varying(8)",
		Detail:   "POISON_DETAIL_the_offending_payload_bytes",
		Where:    "POISON_WHERE_internal_query_context",
		File:     "POISON_FILE_varlena.c",
		Hint:     "POISON_HINT",
		Routine:  "POISON_ROUTINE",
		Severity: "ERROR",
	}
	for _, tc := range []struct {
		name string
		make func(*bytes.Buffer) slog.Handler
	}{
		{"json", func(b *bytes.Buffer) slog.Handler { return slog.NewJSONHandler(b, nil) }},
		{"text", func(b *bytes.Buffer) slog.Handler { return slog.NewTextHandler(b, nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(tc.make(&buf)))
			t.Cleanup(func() { slog.SetDefault(prev) })

			if rec := postToFakeStore(t, pgErr); rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500 (22001 is deliberately not in the permanent set)", rec.Code)
			}
			logged := buf.String()
			for _, poison := range []string{"POISON_DETAIL", "POISON_WHERE", "POISON_FILE", "POISON_HINT", "POISON_ROUTINE"} {
				if strings.Contains(logged, poison) {
					t.Errorf("the log line leaked PgError.%s — these fields quote the offending value for "+
						"22P02/22021, i.e. worker-controlled payload bytes. The error must stay attached AS "+
						"an error so slog renders it via Error().\n%s", strings.TrimPrefix(poison, "POISON_"), logged)
				}
			}
			// The positive control: without it this test would pass against a version
			// that logs nothing at all, which is not the property being claimed.
			if !strings.Contains(logged, "22001") {
				t.Errorf("the log line does not carry the SQLSTATE, so this test is not measuring a real "+
					"log line and its absence-of-poison checks are vacuous\n%s", logged)
			}
		})
	}
}
