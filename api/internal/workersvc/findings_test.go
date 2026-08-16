package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ── canonicalizeLocation: the D3 coordinate normalisation (M2, R1) ──────────────────

func TestCanonicalizeLocationD3Variants(t *testing.T) {
	// The three variants the PRD names (line 107) all collapse to one coordinate — the
	// normalisation the M1 live-DB test deferred to M2.
	const want = "api/internal/sweep.go"
	for _, in := range []string{"api/internal/Sweep.go ", "./api/internal/sweep.go", "api/internal/sweep.go"} {
		if got := canonicalizeLocation(in, MaxFindingLocationBytes); got != want {
			t.Errorf("canonicalizeLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalizeLocationRules(t *testing.T) {
	cases := []struct{ in, want string }{
		// symbol preserved and lowercased, at most one token.
		{"api/internal/Foo.go#sweepLoop", "api/internal/foo.go#sweeploop"},
		{"api/internal/foo.go#sweepLoop extra tokens", "api/internal/foo.go#sweeploop"},
		// backslashes → forward slashes.
		{`api\internal\foo.go`, "api/internal/foo.go"},
		// leading ./ and / dropped (repo-root-relative).
		{"././api/foo.go", "api/foo.go"},
		{"/api/foo.go", "api/foo.go"},
		// line numbers excluded on the path and as a numeric "symbol".
		{"api/foo.go:123", "api/foo.go"},
		{"api/foo.go:12:5", "api/foo.go"},
		{"api/foo.go#42", "api/foo.go"},
		// whitespace stripped.
		{"  api/foo.go  #  bar  ", "api/foo.go#bar"},
		// a symbol-less report stays symbol-less (matches only other symbol-less ones).
		{"api/foo.go", "api/foo.go"},
	}
	for _, c := range cases {
		if got := canonicalizeLocation(c.in, MaxFindingLocationBytes); got != c.want {
			t.Errorf("canonicalizeLocation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCanonicalizeLocationSymbolLineNumberStrip(t *testing.T) {
	// M2 review R1: a trailing line reference on the SYMBOL token drifts and must be
	// stripped, exactly like the path token — otherwise `foo.go#bar:42` and `foo.go#bar`
	// are two coordinates for one bug (D3 dedup).
	const want = "api/foo.go#bar"
	for _, in := range []string{"api/foo.go#bar:42", "api/foo.go#bar:42:5", "api/foo.go#bar"} {
		if got := canonicalizeLocation(in, MaxFindingLocationBytes); got != want {
			t.Errorf("canonicalizeLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCanonicalizeLocationBoundsAndEmpty(t *testing.T) {
	if got := canonicalizeLocation("   ###   ", MaxFindingLocationBytes); got != "" {
		t.Errorf("an all-punctuation location must canonicalise to empty, got %q", got)
	}
	long := ""
	for i := 0; i < 1000; i++ {
		long += "a"
	}
	if got := canonicalizeLocation(long, 512); len(got) > 512 {
		t.Errorf("canonicalizeLocation must re-bound to <= 512 bytes, got %d", len(got))
	}
}

// ── findingContentHash: the D3 re-open discriminator (M2) ────────────────────────────

func TestFindingContentHashNormalisation(t *testing.T) {
	// Case + whitespace differences do NOT change the hash (normalised away)...
	a := findingContentHash("Leaked Ticker", "Never   Stops it")
	b := findingContentHash("leaked   ticker", "never stops it")
	if a != b {
		t.Errorf("cosmetic drift must hash identically: %s vs %s", a, b)
	}
	// ...but a materially different description does (D3 re-open trigger).
	c := findingContentHash("Leaked Ticker", "actually it double-Stops")
	if a == c {
		t.Errorf("materially different content must hash differently")
	}
	// A word moving across the title/description boundary is NOT a collision (the '\n'
	// separator).
	d := findingContentHash("leaked", "ticker never stops it")
	e := findingContentHash("leaked ticker", "never stops it")
	if d == e {
		t.Errorf("the title/description boundary must be part of the hash input")
	}
	// Deterministic hex sha256 (64 hex chars).
	if len(a) != 64 {
		t.Errorf("content hash must be a 64-char hex sha256, got %d chars", len(a))
	}
}

func TestSanitizeFindingTextStripsUnsafe(t *testing.T) {
	// A bidi override and a control char are stripped; the byte cap holds.
	got := sanitizeFindingText("safe\u202etext\x07", 64)
	if got == "" {
		t.Fatalf("expected sanitised text, got empty")
	}
	for _, r := range got {
		if r == '\u202e' || r == '\x07' {
			t.Errorf("sanitizeFindingText left an unsafe rune %U in %q", r, got)
		}
	}
}

func TestMarshalFindingLabelsSanitisesEachLabel(t *testing.T) {
	// M2 review (D4): each label is rendered inert (bidi/control stripped, secret shapes
	// scrubbed) before storage, and a label empty after sanitisation is dropped.
	raw := []string{"bug\u202e", "glpat-ABCDEFGHIJ0123456789", "  ", "keep"}
	b, err := marshalFindingLabels(raw)
	if err != nil {
		t.Fatalf("marshalFindingLabels: %v", err)
	}
	var got []string
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// The whitespace-only label drops; bidi + secret survive but stripped/scrubbed.
	if len(got) != 3 {
		t.Fatalf("expected 3 labels after sanitisation, got %d (%v)", len(got), got)
	}
	for _, label := range got {
		for _, r := range label {
			if r == '\u202e' {
				t.Errorf("label %q retained a bidi override", label)
			}
		}
		if label == "glpat-ABCDEFGHIJ0123456789" {
			t.Errorf("label %q left a secret-shaped token unscrubbed", label)
		}
	}
	if got[0] != "bug" {
		t.Errorf("bidi override should strip to %q, got %q", "bug", got[0])
	}
	if got[1] != "[redacted]" {
		t.Errorf("secret-shaped label should scrub to %q, got %q", "[redacted]", got[1])
	}
	if got[2] != "keep" {
		t.Errorf("plain label should survive intact, got %q", got[2])
	}
}

func TestMarshalFindingLabelsEmpty(t *testing.T) {
	// A nil/empty label set marshals to `[]`, not `null` (matching the proposal path).
	for _, in := range [][]string{nil, {}, {"   ", "\u202e"}} {
		b, err := marshalFindingLabels(in)
		if err != nil {
			t.Fatalf("marshalFindingLabels(%v): %v", in, err)
		}
		if string(b) != "[]" {
			t.Errorf("marshalFindingLabels(%v) = %s, want []", in, b)
		}
	}
}

// ── CreateFinding: derive-from-run, cap, and the anti-nag ordering (M2) ──────────────

// findingsFakeStore implements only the queries CreateFinding reaches; the embedded
// Store makes any other call panic.
type findingsFakeStore struct {
	Store
	run       store.Run
	runErr    error
	count     int64
	inserted  *store.InsertFindingParams
	created   store.IncidentalFinding
	upsertErr error // nil = a new open row was inserted; pgx.ErrNoRows = coordinate exists

	reopenRows   int64
	reopenCalled *store.ReopenDispositionOnHashMismatchParams
	updateRows   int64
	updateCalled *store.UpdateDispositionLastTitleParams
}

func (f *findingsFakeStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if f.runErr != nil {
		return store.Run{}, f.runErr
	}
	if arg.ID == f.run.ID && arg.UserID == f.run.UserID {
		return f.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (f *findingsFakeStore) CountFindingsForRun(context.Context, uuid.UUID) (int64, error) {
	return f.count, nil
}
func (f *findingsFakeStore) InsertFinding(_ context.Context, arg store.InsertFindingParams) (store.IncidentalFinding, error) {
	f.inserted = &arg
	f.created = store.IncidentalFinding{
		ID: uuid.New(), RunID: arg.RunID, UserID: arg.UserID, RepoID: arg.RepoID,
		Location: arg.Location, Title: arg.Title, DescriptionMd: arg.DescriptionMd,
		Labels: arg.Labels, Confidence: arg.Confidence,
	}
	return f.created, nil
}
func (f *findingsFakeStore) UpsertOpenDisposition(_ context.Context, _ store.UpsertOpenDispositionParams) (store.FindingDisposition, error) {
	if f.upsertErr != nil {
		return store.FindingDisposition{}, f.upsertErr
	}
	return store.FindingDisposition{}, nil
}
func (f *findingsFakeStore) ReopenDispositionOnHashMismatch(_ context.Context, arg store.ReopenDispositionOnHashMismatchParams) (int64, error) {
	f.reopenCalled = &arg
	return f.reopenRows, nil
}
func (f *findingsFakeStore) UpdateDispositionLastTitle(_ context.Context, arg store.UpdateDispositionLastTitleParams) (int64, error) {
	f.updateCalled = &arg
	return f.updateRows, nil
}

func baseFindingRun() store.Run {
	uid, rid := uuid.New(), uuid.New()
	return store.Run{
		ID:     uuid.New(),
		UserID: uid,
		RepoID: pgtype.UUID{Bytes: rid, Valid: true},
		Status: "running",
		Kind:   "issue",
	}
}

func TestCreateFindingDerivesUserRepoFromRun(t *testing.T) {
	run := baseFindingRun()
	f := &findingsFakeStore{run: run}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}

	got, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title:       "Leaked ticker",
		Description: "sweepLoop never Stops the ticker",
		Location:    "./api/internal/Sweep.go#sweepLoop",
		Labels:      []string{"bug"},
		Confidence:  "high",
	})
	if err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.inserted == nil {
		t.Fatal("no evidence row inserted")
	}
	// user_id/repo_id come from the run, NOT any client value.
	if f.inserted.UserID != run.UserID {
		t.Errorf("stored user_id = %v, want the run's %v", f.inserted.UserID, run.UserID)
	}
	if f.inserted.RepoID != uuid.UUID(run.RepoID.Bytes) {
		t.Errorf("stored repo_id = %v, want the run's %v", f.inserted.RepoID, uuid.UUID(run.RepoID.Bytes))
	}
	// location canonicalised.
	if f.inserted.Location != "api/internal/sweep.go#sweeploop" {
		t.Errorf("stored location = %q, want canonical form", f.inserted.Location)
	}
	// labels are valid JSON.
	var labels []string
	if err := json.Unmarshal(f.inserted.Labels, &labels); err != nil {
		t.Errorf("labels not valid JSON: %v", err)
	}
	if got.ID == uuid.Nil {
		t.Error("returned finding has a nil id")
	}
}

func TestCreateFindingForeignRunIs404(t *testing.T) {
	f := &findingsFakeStore{run: baseFindingRun()}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: f.run.UserID}
	// A run id that is not the worker's user's run.
	_, err := svc.CreateFinding(context.Background(), wkr, uuid.New(), CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	})
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("foreign run = %v, want ErrRunNotFound", err)
	}
	if f.inserted != nil {
		t.Error("no evidence must be inserted for a foreign run")
	}
}

func TestCreateFindingCapReached(t *testing.T) {
	run := baseFindingRun()
	f := &findingsFakeStore{run: run, count: MaxFindingsPerRun}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}
	_, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	})
	if !errors.Is(err, ErrFindingCapReached) {
		t.Fatalf("past cap = %v, want ErrFindingCapReached", err)
	}
	if f.inserted != nil {
		t.Error("no evidence inserted once the cap is reached")
	}
}

func TestCreateFindingRepoRequired(t *testing.T) {
	run := baseFindingRun()
	run.RepoID = pgtype.UUID{} // NULL repo (a repo-less run)
	f := &findingsFakeStore{run: run}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}
	_, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	})
	if !errors.Is(err, ErrFindingRepoRequired) {
		t.Fatalf("repo-less run = %v, want ErrFindingRepoRequired", err)
	}
}

func TestCreateFindingAntiNagOrdering(t *testing.T) {
	// A report on an already-resolved coordinate whose content_hash MATCHES: the upsert
	// hits the existing row (ErrNoRows), the guarded re-open matches 0 rows (identical
	// hash → stays resolved), and the open-only last_title refresh also matches 0 rows
	// (the row is not open). Evidence is still inserted; the coordinate does NOT resurrect.
	run := baseFindingRun()
	f := &findingsFakeStore{
		run:        run,
		upsertErr:  pgx.ErrNoRows, // coordinate already exists
		reopenRows: 0,             // identical hash → no re-open
		updateRows: 0,             // resolved row → refresh is a no-op
	}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}
	_, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	})
	if err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.inserted == nil {
		t.Error("evidence must be inserted even for a suppressed (matching-hash) re-report")
	}
	if f.reopenCalled == nil {
		t.Error("the guarded re-open must be attempted on an existing coordinate")
	}
	if f.updateCalled == nil {
		t.Error("when the re-open matched 0 rows, the last_title refresh must be attempted")
	}
	// Ordering: re-open is tried BEFORE the open-only refresh, so an identical-hash report
	// on a filed/dismissed row never resurrects it.
}

func TestCreateFindingReopenSkipsRefresh(t *testing.T) {
	// A materially-different report on a resolved coordinate: the re-open matches 1 row,
	// so the open-only refresh is NOT attempted.
	run := baseFindingRun()
	f := &findingsFakeStore{run: run, upsertErr: pgx.ErrNoRows, reopenRows: 1}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}
	if _, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	}); err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.reopenCalled == nil {
		t.Error("re-open must be attempted")
	}
	if f.updateCalled != nil {
		t.Error("a successful re-open (1 row) must NOT fall through to the last_title refresh")
	}
}

func TestCreateFindingFirstReportInsertsOpen(t *testing.T) {
	// The first report at a coordinate: upsert inserts a new open row (no error), so
	// neither the re-open nor the refresh runs.
	run := baseFindingRun()
	f := &findingsFakeStore{run: run, upsertErr: nil}
	svc := New(f, nil, Params{})
	wkr := store.Worker{ID: uuid.New(), UserID: run.UserID}
	if _, err := svc.CreateFinding(context.Background(), wkr, run.ID, CreateFindingRequest{
		Title: "T", Description: "D", Location: "a/b.go#f",
	}); err != nil {
		t.Fatalf("CreateFinding: %v", err)
	}
	if f.reopenCalled != nil || f.updateCalled != nil {
		t.Error("a fresh open insert must not touch the re-open / refresh UPDATEs")
	}
}
