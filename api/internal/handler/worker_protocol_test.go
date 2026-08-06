package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// protocolStore is a minimal workersvc.Store for the worker-protocol HTTP tests:
// it embeds the interface (unused methods panic) and overrides only the queries
// the tested paths reach.
type protocolStore struct {
	workersvc.Store
	claimErr      error
	ownedRun      store.Run
	ownedErr      error
	completedRows int64
	heartbeatArg  store.HeartbeatWorkerParams
}

func (p *protocolStore) ClaimRun(context.Context, store.ClaimRunParams) (store.Run, error) {
	return store.Run{}, p.claimErr
}
func (p *protocolStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return p.ownedRun, p.ownedErr
}
func (p *protocolStore) SetRunCompleted(context.Context, store.SetRunCompletedParams) (int64, error) {
	return p.completedRows, nil
}

// Register path: orphan recovery + the online transition. The counts are
// irrelevant to the wire-decode test, so they return 0/empty.
func (p *protocolStore) FailWorkerRunsOverCap(context.Context, store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (p *protocolStore) RequeueWorkerRuns(context.Context, store.RequeueWorkerRunsParams) (int64, error) {
	return 0, nil
}
func (p *protocolStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.RegisterWorkerRow, error) {
	return store.RegisterWorkerRow{ID: arg.ID, Status: "online", Version: arg.Version, TemplateReported: arg.TemplateReported, MaxConcurrentRuns: arg.MaxConcurrentRuns}, nil
}

// HeartbeatWorker echoes the stats params back onto the returned worker, so a
// heartbeat test can assert exactly what would have been persisted (and rendered in
// the DTO) — a valid sample round-trips; a dropped one leaves every stats_ column NULL.
func (p *protocolStore) HeartbeatWorker(_ context.Context, arg store.HeartbeatWorkerParams) (store.Worker, error) {
	p.heartbeatArg = arg
	return store.Worker{
		ID:                 arg.ID,
		Status:             "online",
		StatsCpuPct:        arg.StatsCpuPct,
		StatsMemBytes:      arg.StatsMemBytes,
		StatsMemLimitBytes: arg.StatsMemLimitBytes,
		StatsSource:        arg.StatsSource,
	}, nil
}

func newProtocolHandler(t *testing.T, st workersvc.Store) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{})}
}

// workerCtx builds a request carrying an authenticated worker plus a chi {id}
// route param, the way RequireWorker + the router would.
func workerReq(method, body string, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, "/api/worker/x", strings.NewReader(body))
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})
	if runID != uuid.Nil {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

func TestWorkerClaimIdleReturns204NoBody(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
	rec := httptest.NewRecorder()
	h.WorkerClaim(rec, workerReq(http.MethodPost, "", uuid.Nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 must have an empty body, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterAcceptsNameField(t *testing.T) {
	// The M2 worker announces {name, version}; DecodeJSON rejects unknown fields,
	// so register must declare name (accepted, ignored) or every worker 400s on
	// register and never comes online. Posts the worker's exact body.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (register must accept the name field), body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerRegisterReadsTemplate(t *testing.T) {
	// PRD #18: unlike name, the template field IS read and persisted as
	// template_reported, and echoed back in the worker DTO. DecodeJSON must accept
	// it (no 400) and the value must round-trip.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","template":"jvm"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"template_reported":"jvm"`) {
		t.Fatalf("expected template_reported=jvm in DTO, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterReadsMaxConcurrentRuns(t *testing.T) {
	// PRD #42: the worker advertises its concurrency cap. Register must accept it
	// (no 400 from the unknown-field-rejecting decoder) and round-trip it into the
	// DTO as max_concurrent_runs so the fleet UI can render "N/M runs".
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","max_concurrent_runs":2}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":2`) {
		t.Fatalf("expected max_concurrent_runs=2 in DTO, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterOmitsMaxConcurrentRuns(t *testing.T) {
	// An older image (and every M3a worker) sends no cap ⇒ max_concurrent_runs is
	// null in the DTO, never 0.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":null`) {
		t.Fatalf("absent cap must be null, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterDropsOutOfRangeMaxConcurrentRuns(t *testing.T) {
	// A garbled/hostile worker reports a cap outside the sane [1, 256] band (zero,
	// negative, or absurdly large). Register must still succeed (a soft observability
	// field never wedges the register-retry loop) but the nonsensical value must not
	// reach the DB/UI — dropped to null, so it never flows into the "N/M runs" math.
	for _, body := range []string{
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":0}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":-3}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":100000}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":257}`,
	} {
		h := newProtocolHandler(t, &protocolStore{})
		rec := httptest.NewRecorder()
		h.WorkerRegister(rec, workerReq(http.MethodPost, body, uuid.Nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a bad cap must not fail register) for %s, body %q", rec.Code, body, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":null`) {
			t.Fatalf("out-of-range cap %s must be dropped to null, got %q", body, rec.Body.String())
		}
	}
}

func TestWorkerRegisterAcceptsCeilingMaxConcurrentRuns(t *testing.T) {
	// The ceiling value itself is in-band and must round-trip (boundary check that
	// the guard rejects 257 but not 256).
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","max_concurrent_runs":256}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":256`) {
		t.Fatalf("ceiling cap 256 must be accepted, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterToleratesCapabilities(t *testing.T) {
	// PRD #83 Q1: an M1 worker with a daemon wired sends {"capabilities":["docker"]}.
	// DecodeJSON rejects unknown fields, so register MUST declare the field or the
	// worker 400s on register and wedges its retry loop — the compat rule (the api
	// tolerates `capabilities` in the SAME release the worker starts sending it). M1 is
	// accept-and-ignore: no column, no DTO surface, just no 400. An empty array and an
	// absent field must both be tolerated too.
	for _, body := range []string{
		`{"name":"laptop","version":"1.2.3","capabilities":["docker"]}`,
		`{"name":"laptop","version":"1.2.3","capabilities":[]}`,
		`{"name":"laptop","version":"1.2.3"}`,
		// Alongside the other optional self-reported fields.
		`{"name":"laptop","version":"1.2.3","template":"base","max_concurrent_runs":2,"capabilities":["docker"]}`,
	} {
		h := newProtocolHandler(t, &protocolStore{})
		rec := httptest.NewRecorder()
		h.WorkerRegister(rec, workerReq(http.MethodPost, body, uuid.Nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (capabilities must not 400 register) for %s, body %q", rec.Code, body, rec.Body.String())
		}
	}
}

func TestSanitizeSelfReported(t *testing.T) {
	// Trims, strips control chars, caps length.
	if got := sanitizeSelfReported("  1.2.3\x07-m4 \n ", maxSelfReportedBytes); got != "1.2.3-m4" {
		t.Fatalf("sanitizeSelfReported = %q, want %q", got, "1.2.3-m4")
	}
	// Issue #124: format characters (Cf) too, not only controls (Cc). The two categories
	// are DISJOINT, so IsControl alone never saw any of these — and this function carries
	// a recommendation's Target (judge_worker.go:369), which is a filename, so a surviving
	// bidi override lets a review name one file and point at another. Corpus mirrored from
	// TestSanitizeTTYStripsControlAndFormatChars (api/cmd/uzi/tui_render_test.go:61).
	for _, tc := range []struct{ name, in, want string }{
		{"RLO bidi override", "a\u202Eb", "ab"},
		{"LRO bidi override", "a\u202Db", "ab"},
		{"bidi isolate", "a\u2066b", "ab"},
		{"RTL mark", "a\u200Fb", "ab"},
		{"zero-width space", "a\u200Bb", "ab"},
		{"zero-width joiner", "a\u200Db", "ab"},
		{"BOM", "a\uFEFFb", "ab"},
		{"soft hyphen", "a\u00ADb", "ab"},
		// The ESC BYTE is what makes a sequence an escape sequence; stripping it leaves
		// inert literal text, which is correct and is what sanitizeTTY's own case asserts.
		{"CSI cursor move loses its ESC", "a\x1b[2Jb", "a[2Jb"},
		// C1 as a RUNE, and DEL: neither is caught by a naive r < 0x20 predicate.
		{"C1 control rune", "a\u009bb", "ab"},
		{"DEL", "a\x7fb", "ab"},
		// A real filename target must survive untouched — the rule is a category test,
		// not an ASCII allowlist.
		{"an ordinary target is unchanged", "api/internal/forge/gitlab.go", "api/internal/forge/gitlab.go"},
		{"non-Latin text survives", "\u65e5\u672c\u8a9e", "\u65e5\u672c\u8a9e"},
		// Cn/Co are NOT stripped: the predicate is Cc|Cf, not the whole C category. U+E000
		// is private use, which Unicode will never assign, so this fixture cannot rot.
		{"private-use code point survives", "a\ue000b", "a\ue000b"},
	} {
		if got := sanitizeSelfReported(tc.in, maxSelfReportedBytes); got != tc.want {
			t.Errorf("%s: sanitizeSelfReported(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
	// TRIM ORDER (#124 follow-up). TrimSpace runs BEFORE the strip and Cf is not
	// White_Space, so one format character at each edge shielded the adjacent ASCII space
	// from the trim; the loop then removed the Cf and left the space. BOTH halves are
	// asserted, because the plain-whitespace row is what makes the padded row a defect
	// rather than a design choice — without it the fix is unfalsifiable in the direction
	// that matters. It bites on `target`, a COORDINATE: " api/foo.go " and "api/foo.go" are
	// different (category, target) pairs, so one Cf-padded run and one clean run become two
	// backlog groups a human reads as identical.
	for _, tc := range []struct{ name, in, want string }{
		{"Cf-padded edges lose the whitespace too", "\u200b  api/internal/forge/gitlab.go  \u200b", "api/internal/forge/gitlab.go"},
		{"leading Cf alone", "\u202e  api/foo.go", "api/foo.go"},
		{"trailing Cf alone", "api/foo.go  \ufeff", "api/foo.go"},
		{"plain whitespace still trims (the control)", "  plain surrounding spaces  ", "plain surrounding spaces"},
		{"interior whitespace is untouched", "api/foo bar.go", "api/foo bar.go"},
	} {
		if got := sanitizeSelfReported(tc.in, maxSelfReportedBytes); got != tc.want {
			t.Errorf("%s: sanitizeSelfReported(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	// The PROPERTY, asserted independently of any per-case expectation: whatever survives
	// is neither Cc nor Cf. A table can only cover the codepoints someone thought of.
	// No whitespace exemption here — this sink is single-line by contract.
	for _, in := range []string{"a\u202Eb", "a\x1b[2Jb", "a\u200db\ufeffc", "x\ty\nz"} {
		for _, r := range sanitizeSelfReported(in, maxSelfReportedBytes) {
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				t.Errorf("sanitizeSelfReported(%q) let %U through", in, r)
			}
		}
	}
	long := strings.Repeat("a", 100)
	if got := sanitizeSelfReported(long, maxSelfReportedBytes); len(got) > maxSelfReportedBytes+4 {
		t.Fatalf("sanitizeSelfReported did not cap length: got %d bytes", len(got))
	}
	if got := sanitizeSelfReported("", maxSelfReportedBytes); got != "" {
		t.Fatalf("empty in must stay empty, got %q", got)
	}
}

func TestCanonicalizeTarget(t *testing.T) {
	const max = workersvc.ReviewTargetMaxBytes

	// COSMETIC FOLD: casing, whitespace runs, and ASCII punctuation runs all collapse, so
	// the same finding phrased differently across runs lands on ONE (category, target)
	// coordinate the cross-run dedup keys on (issue #232). The two inputs below are the
	// motivating case — a trailing space + mixed case + a hyphen vs. doubled interior
	// spaces — and both MUST reach the identical canonical value.
	const want = "worker git identity setup"
	if a, b := canonicalizeTarget("Worker Git-Identity Setup ", max), canonicalizeTarget("worker  git-identity   setup", max); a != b {
		t.Fatalf("cosmetic variants must collapse: %q vs %q", a, b)
	}
	if got := canonicalizeTarget("Worker Git-Identity Setup ", max); got != want {
		t.Fatalf("canonicalizeTarget(%q) = %q, want %q", "Worker Git-Identity Setup ", got, want)
	}

	// NEGATIVE OVER-MERGE GUARD. This is the load-bearing test: it pins that the
	// canonicalizer folds ONLY cosmetic drift and NEVER reorders tokens, drops stopwords,
	// or stems. "worker clone setup (git identity)" and "worker runner clone setup" share
	// most words but describe different findings, and they MUST stay distinct — if someone
	// "improves" canonicalizeTarget into a token-set / stopword / stemming merge, these two
	// collapse and two genuinely different backlog rows fuse into one a human then reads as
	// identical (the unsafe over-merge failure mode). Do not relax this into a fuzzy match.
	if a, b := canonicalizeTarget("worker clone setup (git identity)", max), canonicalizeTarget("worker runner clone setup", max); a == b {
		t.Fatalf("different findings must NOT merge: both folded to %q", a)
	}
	if got := canonicalizeTarget("worker clone setup (git identity)", max); got != "worker clone setup git identity" {
		t.Errorf("canonicalizeTarget punctuation fold = %q, want %q", got, "worker clone setup git identity")
	}
	if got := canonicalizeTarget("worker runner clone setup", max); got != "worker runner clone setup" {
		t.Errorf("canonicalizeTarget already-canonical = %q, want unchanged", got)
	}

	// IDEMPOTENCE: folding a canonical value is a no-op, so ingest-then-backfill (or a
	// double ingest) can never drift the coordinate. Asserted over a spread of shapes.
	for _, in := range []string{
		"Worker Git-Identity Setup ", "worker  git-identity   setup",
		"worker clone setup (git identity)", "api/internal/forge/gitlab.go",
		"UPPER.snake_case--dashes", "", "   ", "trailing punctuation!!!",
	} {
		once := canonicalizeTarget(in, max)
		if twice := canonicalizeTarget(once, max); once != twice {
			t.Errorf("not idempotent for %q: once=%q twice=%q", in, once, twice)
		}
	}

	// A few more shape assertions: empty/whitespace-only fold to empty; a filename-style
	// target folds its separators (that is the point — one coordinate per file, not per
	// punctuation style).
	for _, tc := range []struct{ name, in, want string }{
		{"empty stays empty", "", ""},
		{"whitespace only folds to empty", "  \t \n ", ""},
		{"leading/trailing punctuation trimmed", "  --foo bar--  ", "foo bar"},
		{"case folded", "ShellCheck", "shellcheck"},
	} {
		if got := canonicalizeTarget(tc.in, max); got != tc.want {
			t.Errorf("%s: canonicalizeTarget(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestWorkerRegisterSanitizesVersion(t *testing.T) {
	// A hostile worker smuggles a control char (terminal escape) in `version`.
	// Register succeeds and the persisted/echoed version is stripped clean.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, "{\"name\":\"laptop\",\"version\":\"1.2.3\\u0007evil\"}", uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\x07") {
		t.Fatalf("control char must be stripped from version, got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version":"1.2.3evil"`) {
		t.Fatalf("expected sanitized version 1.2.3evil, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterDropsMalformedTemplate(t *testing.T) {
	// A hostile/misconfigured worker sends junk in `template`. Register must still
	// succeed (a soft field never wedges the register-retry loop) but the malformed
	// value must NOT reach the DB/UI — it is dropped, so template_reported is null.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","template":"../../etc/passwd"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (malformed template must not fail register), body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"template_reported":null`) {
		t.Fatalf("malformed template must be dropped to null, got %q", rec.Body.String())
	}
}

// ── Heartbeat + stats (PRD #49) ────────────────────────────────────────────

func TestWorkerHeartbeatAcceptsVersionBody(t *testing.T) {
	// THE blocking-finding test (PRD #49 Decision 3): every current worker already
	// sends {"version":...} on the heartbeat, and DisallowUnknownFields would 400 a
	// stats-only decode — marking the whole fleet stale within the sweeper window.
	// Register's name field proved the same trap (TestWorkerRegisterAcceptsNameField).
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, `{"version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (heartbeat must accept the version-only body), body %q", rec.Code, rec.Body.String())
	}
	// No stats sent ⇒ every stats_ column is written NULL (null round-trip, Decision 4).
	if !strings.Contains(rec.Body.String(), `"stats_source":null`) || !strings.Contains(rec.Body.String(), `"stats_mem_bytes":null`) {
		t.Fatalf("absent stats must render null, got %q", rec.Body.String())
	}
}

func TestWorkerHeartbeatAcceptsEmptyBody(t *testing.T) {
	// An empty body decodes to io.EOF, tolerated exactly like register's.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, "", uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (empty body tolerated), body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerHeartbeatStoresValidStats(t *testing.T) {
	// A well-formed cgroup sample round-trips: validated, persisted, and echoed in the
	// worker DTO with a percentage, a used/limit pair, and the source.
	st := &protocolStore{}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	body := `{"version":"1","stats":{"cpu_pct":34.5,"mem_bytes":2100000000,"mem_limit_bytes":4294967296,"source":"cgroup"}}`
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, body, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"stats_cpu_pct":34.5`, `"stats_mem_bytes":2100000000`, `"stats_mem_limit_bytes":4294967296`, `"stats_source":"cgroup"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("expected %s in DTO, got %q", want, rec.Body.String())
		}
	}
	if !st.heartbeatArg.StatsSource.Valid || st.heartbeatArg.StatsSource.String != "cgroup" {
		t.Fatalf("valid stats must reach the store, got %+v", st.heartbeatArg)
	}
}

func TestWorkerHeartbeatOmittedCPUPctIsNull(t *testing.T) {
	// The worker's first tick omits cpu_pct (no delta). mem + source still store; the
	// percentage column stays NULL (the UI shows the mem bar without a CPU value).
	st := &protocolStore{}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, `{"version":"1","stats":{"mem_bytes":100,"source":"process"}}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stats_cpu_pct":null`) {
		t.Fatalf("omitted cpu_pct must be null, got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stats_source":"process"`) || !strings.Contains(rec.Body.String(), `"stats_mem_bytes":100`) {
		t.Fatalf("mem + source must still store, got %q", rec.Body.String())
	}
	if st.heartbeatArg.StatsMemLimitBytes.Valid {
		t.Fatalf("absent mem_limit must be NULL, got %+v", st.heartbeatArg.StatsMemLimitBytes)
	}
}

func TestWorkerHeartbeatClampsCPUPct(t *testing.T) {
	// A CPU% above the [0, 6400] ceiling is clamped, not rejected: telemetry stays a
	// 200, and the stored value can never put 6400000% into the DOM.
	st := &protocolStore{}
	h := newProtocolHandler(t, st)
	rec := httptest.NewRecorder()
	h.WorkerHeartbeat(rec, workerReq(http.MethodPost, `{"version":"1","stats":{"cpu_pct":999999,"mem_bytes":1,"source":"cgroup"}}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"stats_cpu_pct":6400`) {
		t.Fatalf("cpu_pct must clamp to 6400, got %q", rec.Body.String())
	}
}

func TestWorkerHeartbeatDropsInvalidStats(t *testing.T) {
	// A malformed or hostile stats object must NEVER fail the heartbeat (telemetry
	// hygiene never costs liveness) — the whole object is dropped and the columns are
	// written NULL. Covers the two-step-decode escapes (float64-overflow 1e999,
	// int64-overflow mem) and the validation rejects (junk source, negative mem, a
	// missing required mem_bytes).
	for _, body := range []string{
		`{"version":"1","stats":{"cpu_pct":1e999,"mem_bytes":1,"source":"cgroup"}}`,     // float64 overflow
		`{"version":"1","stats":{"mem_bytes":99999999999999999999,"source":"cgroup"}}`,  // int64 overflow
		`{"version":"1","stats":{"cpu_pct":10,"mem_bytes":1,"source":"../etc/passwd"}}`, // garbage source enum
		`{"version":"1","stats":{"mem_bytes":-5,"source":"cgroup"}}`,                    // negative mem
		`{"version":"1","stats":{"cpu_pct":10,"source":"cgroup"}}`,                      // missing required mem_bytes
	} {
		st := &protocolStore{}
		h := newProtocolHandler(t, st)
		rec := httptest.NewRecorder()
		h.WorkerHeartbeat(rec, workerReq(http.MethodPost, body, uuid.Nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (invalid stats must not fail the heartbeat) for %s, body %q", rec.Code, body, rec.Body.String())
		}
		if st.heartbeatArg.StatsSource.Valid || st.heartbeatArg.StatsMemBytes.Valid || st.heartbeatArg.StatsCpuPct.Valid {
			t.Fatalf("invalid stats %s must be dropped (all stats_ NULL), got %+v", body, st.heartbeatArg)
		}
	}
}

func TestAdminWorkerDTOIncludesStats(t *testing.T) {
	// The stats fields ride the shared apitypes.WorkerDTO, so the admin worker DTO
	// inherits them for free (PRD #49 Decision 6). A worker row with a sample marshals
	// every stats_ field into the admin JSON.
	w := store.Worker{
		ID:                 uuid.New(),
		Name:               "laptop",
		Status:             "online",
		StatsCpuPct:        pgtype.Float4{Float32: 12.5, Valid: true},
		StatsMemBytes:      pgtype.Int8{Int64: 1073741824, Valid: true},
		StatsMemLimitBytes: pgtype.Int8{Int64: 2147483648, Valid: true},
		StatsSource:        pgtype.Text{String: "cgroup", Valid: true},
	}
	dto := apitypes.AdminWorkerDTO{WorkerDTO: workerDTOFromWorker(w, 0, false, "", "", time.Now(), time.Now()), OwnerEmail: "u@example.test"}
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal admin dto: %v", err)
	}
	for _, want := range []string{`"stats_cpu_pct":12.5`, `"stats_mem_bytes":1073741824`, `"stats_mem_limit_bytes":2147483648`, `"stats_source":"cgroup"`, `"owner_email":"u@example.test"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("expected %s in admin worker DTO, got %s", want, string(b))
		}
	}
}

func TestNoStatsColumnsInSchedulingQueries(t *testing.T) {
	// Decision 5 is ENFORCED, not just prose: stats_ columns are display-only, so the
	// ONLY queries that may name one are the HeartbeatWorker writer and the worker-list
	// DTO reads. Every other query — in ANY queries file, not just runtime.sql — that
	// literally references stats_ would be a claim/scheduling/sweeper path keying on
	// attacker-reported telemetry, exactly what must never happen. Scanning the whole
	// directory (not just runtime.sql) means a future scheduling query added to another
	// queries file can't dodge the guard (auditor M2 minor).
	allowed := map[string]bool{
		"HeartbeatWorker":   true, // the sole writer of the stats_ columns
		"ListWorkersByUser": true, // DTO read; today via w.*, allowlisted for a future explicit select
		"ListAllWorkers":    true, // DTO read (admin); same rationale
	}
	files, err := filepath.Glob("../store/queries/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob queries: %v (matched %d files)", err, len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		blocks := strings.Split(string(raw), "-- name:")
		for _, block := range blocks[1:] { // blocks[0] is the file preamble
			name := strings.Fields(block)[0]
			if allowed[name] {
				continue
			}
			if strings.Contains(block, "stats_") {
				t.Fatalf("query %s in %s references a stats_ column; stats_* are display-only (Decision 5) and may appear only in HeartbeatWorker or the worker-list DTO queries", name, filepath.Base(f))
			}
		}
	}
}

func TestWorkerStateAlreadyTerminalReturns409(t *testing.T) {
	runID := uuid.New()
	// Owned run is cancelled; the guarded completed-update touches 0 rows.
	h := newProtocolHandler(t, &protocolStore{
		ownedRun:      store.Run{ID: runID, Status: "cancelled"},
		completedRows: 0,
	})
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, workerReq(http.MethodPost, `{"status":"completed"}`, runID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (already-terminal)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("409 body should echo the run's real status, got %q", rec.Body.String())
	}
}

func TestWorkerMessagesForeignRunReturns404(t *testing.T) {
	runID := uuid.New()
	h := newProtocolHandler(t, &protocolStore{ownedErr: pgx.ErrNoRows})
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, workerReq(http.MethodPost, `{"messages":[{"seq":1,"kind":"text","payload":{}}]}`, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (run not owned by this worker)", rec.Code)
	}
}
