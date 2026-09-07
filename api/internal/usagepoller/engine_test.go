package usagepoller

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// --- fakes ---

// fakeStore models each user as holding ONE token whose secret id EQUALS the user
// id — a legal simplification that keeps the single-token tests' by-user assertions
// readable (got(u), got=upserts[secretID]). The genuinely per-token behaviour, where
// one user holds several tokens with distinct ids, has its own test
// (TestPerTokenBackoffIsolation) that does not use this helper.
type fakeStore struct {
	rows []store.ListAnthropicTokensToPollRow

	mu        sync.Mutex
	upserts   map[uuid.UUID]store.UpsertRateLimitsParams // keyed by user_secret_id
	prev      map[uuid.UUID]store.AnthropicRateLimit     // keyed by user_secret_id
	notify    map[uuid.UUID]bool                         // keyed by user id
	upsertErr error                                      // when set, UpsertRateLimits fails (no write)
}

func newFakeStore(users ...uuid.UUID) *fakeStore {
	rows := make([]store.ListAnthropicTokensToPollRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, store.ListAnthropicTokensToPollRow{
			ID: u, UserID: u, Ciphertext: []byte("ct"), SealedWith: store.SealedWithDEK,
		})
	}
	return &fakeStore{
		rows:    rows,
		upserts: map[uuid.UUID]store.UpsertRateLimitsParams{},
		prev:    map[uuid.UUID]store.AnthropicRateLimit{},
		notify:  map[uuid.UUID]bool{},
	}
}
func (f *fakeStore) ListAnthropicTokensToPoll(context.Context) ([]store.ListAnthropicTokensToPollRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Reflect any per-user notify opt-in onto the poll rows (the production JOIN),
	// keeping the tick path and the poke path reading the same flag.
	out := make([]store.ListAnthropicTokensToPollRow, len(f.rows))
	for i, r := range f.rows {
		r.NotifyEarlyLimitReset = f.notify[r.UserID]
		out[i] = r
	}
	return out, nil
}
func (f *fakeStore) GetDefaultUserSecretID(_ context.Context, arg store.GetDefaultUserSecretIDParams) (uuid.UUID, error) {
	// secret id == user id in this fake, so the user's default is the user id.
	return arg.UserID, nil
}
func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	return store.GetUserSecretCiphertextRow{Ciphertext: []byte("ct"), SealedWith: store.SealedWithDEK}, nil
}
func (f *fakeStore) UpsertRateLimits(_ context.Context, arg store.UpsertRateLimitsParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upsertErr != nil {
		// A failed write records nothing: prev stays at the old epoch (SC5).
		return f.upsertErr
	}
	f.upserts[arg.UserSecretID] = arg
	// Mirror the write into prev, so a following tick's GetRateLimitsForToken sees the
	// row this tick just wrote — the once-only edge-consumption property depends on it.
	f.prev[arg.UserSecretID] = store.AnthropicRateLimit(arg)
	return nil
}
func (f *fakeStore) GetRateLimitsForToken(_ context.Context, secretID uuid.UUID) (store.AnthropicRateLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.prev[secretID]; ok {
		return r, nil
	}
	return store.AnthropicRateLimit{}, pgx.ErrNoRows
}
func (f *fakeStore) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return store.User{ID: id, NotifyEarlyLimitReset: f.notify[id]}, nil
}
func (f *fakeStore) setNotify(userID uuid.UUID, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.notify[userID] = on
}
func (f *fakeStore) setPrev(secretID uuid.UUID, r store.AnthropicRateLimit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prev[secretID] = r
}
func (f *fakeStore) got(secretID uuid.UUID) (store.UpsertRateLimitsParams, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.upserts[secretID]
	return v, ok
}
func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upserts)
}

type fakeOpener struct {
	tokens map[uuid.UUID][]byte
	errs   map[uuid.UUID]error
}

func (f *fakeOpener) resolve(userID uuid.UUID) ([]byte, error) {
	if f.errs != nil {
		if e, ok := f.errs[userID]; ok {
			return nil, e
		}
	}
	if tok, ok := f.tokens[userID]; ok {
		return tok, nil
	}
	return []byte("token"), nil
}
func (f *fakeOpener) Open(_ context.Context, userID uuid.UUID, _ string) ([]byte, error) {
	return f.resolve(userID)
}
func (f *fakeOpener) OpenSealed(userID uuid.UUID, _ string, _ string, _ []byte) ([]byte, error) {
	return f.resolve(userID)
}

type fakeClient struct {
	mu    sync.Mutex
	usage func([]byte) (anthropic.Reading, error)
	probe func([]byte) (anthropic.Reading, error)

	usageCalls int
	probeCalls int
}

func (f *fakeClient) Usage(_ context.Context, token []byte) (anthropic.Reading, error) {
	f.mu.Lock()
	f.usageCalls++
	f.mu.Unlock()
	return f.usage(token)
}
func (f *fakeClient) ProbeHeaders(_ context.Context, token []byte) (anthropic.Reading, error) {
	f.mu.Lock()
	f.probeCalls++
	f.mu.Unlock()
	if f.probe == nil {
		return anthropic.Reading{}, &anthropic.Error{Kind: anthropic.KindHTTP, Status: 429}
	}
	return f.probe(token)
}
func (f *fakeClient) calls() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usageCalls, f.probeCalls
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}
func (c *clock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func reading(five, seven int, source string) anthropic.Reading {
	return anthropic.Reading{
		FiveHour: anthropic.Window{Pct: five},
		SevenDay: anthropic.Window{Pct: seven},
		Source:   source,
	}
}

func httpErr(status int) error { return &anthropic.Error{Kind: anthropic.KindHTTP, Status: status} }
func malformed() error         { return &anthropic.Error{Kind: anthropic.KindMalformed} }
func transport() error         { return &anthropic.Error{Kind: anthropic.KindTransport} }

func newEngine(t *testing.T, st Store, op TokenOpener, cl Client, probe bool) (*Engine, *clock) {
	t.Helper()
	clk := &clock{t: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)}
	e := New(st, op, cl, 5*time.Minute, probe, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.now = clk.now
	e.maxConc = 1 // deterministic ordering for tests
	return e, clk
}

// --- tests ---

// D3: a locked dek-sealed user is skipped (no row written); a master-sealed user
// opens regardless and is polled. Both are represented by the opener's result:
// ErrVaultLocked for the locked one, a token for the master-sealed one.
func TestPollSkipsLockedButPollsOpenable(t *testing.T) {
	locked := uuid.New()
	openable := uuid.New()
	st := newFakeStore(locked, openable)
	op := &fakeOpener{
		errs:   map[uuid.UUID]error{locked: secretopen.ErrVaultLocked},
		tokens: map[uuid.UUID][]byte{openable: []byte("tok")},
	}
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(55, 10, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, op, cl, true)

	e.Boot(context.Background())

	if _, ok := st.got(locked); ok {
		t.Error("locked user must not get a row written")
	}
	got, ok := st.got(openable)
	if !ok {
		t.Fatal("openable user should have a row")
	}
	if got.FiveHourPct.Int16 != 55 || got.SevenDayPct.Int16 != 10 {
		t.Errorf("pct = (%d,%d), want (55,10)", got.FiveHourPct.Int16, got.SevenDayPct.Int16)
	}
	if got.Source.String != anthropic.SourceUsageEndpoint {
		t.Errorf("source = %q, want usage_endpoint", got.Source.String)
	}
}

// D2: usage-endpoint refusal falls back to the header probe; the stored source is
// header_probe.
func TestProbeFallbackOnUsageRefusal(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{
		usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) },
		probe: func([]byte) (anthropic.Reading, error) { return reading(80, 40, anthropic.SourceHeaderProbe), nil },
	}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)

	e.Boot(context.Background())

	got, ok := st.got(u)
	if !ok {
		t.Fatal("expected a row from the probe fallback")
	}
	if got.Source.String != anthropic.SourceHeaderProbe {
		t.Errorf("source = %q, want header_probe", got.Source.String)
	}
	uc, pc := cl.calls()
	if uc != 1 || pc != 1 {
		t.Errorf("calls usage=%d probe=%d, want 1 and 1", uc, pc)
	}
}

// D2/D5: with the probe disabled, a usage refusal writes no row and arms the
// backoff; the next tick skips the user (usage is not called again) until the
// backoff expires.
func TestBackoffAfterRefusalProbeDisabled(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, httpErr(429) }}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, false /* probe disabled */)

	e.tickAll(context.Background())
	if st.count() != 0 {
		t.Fatal("no row should be written on refusal")
	}
	if uc, pc := cl.calls(); uc != 1 || pc != 0 {
		t.Fatalf("after 1st tick usage=%d probe=%d, want 1 and 0", uc, pc)
	}

	// Second tick, still inside the backoff window: the user is skipped.
	e.tickAll(context.Background())
	if uc, _ := cl.calls(); uc != 1 {
		t.Fatalf("usage called %d times, want still 1 (backed off)", uc)
	}

	// Advance past the backoff: the user is polled again.
	clk.advance(backoffDuration + time.Second)
	e.tickAll(context.Background())
	if uc, _ := cl.calls(); uc != 2 {
		t.Fatalf("usage called %d times after backoff expiry, want 2", uc)
	}
}

// D5: a malformed usage response is fail-closed — no row is written and NO backoff
// is set, so the very next tick retries.
func TestMalformedFailsClosedNoBackoff(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, malformed() }}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)

	e.tickAll(context.Background())
	e.tickAll(context.Background())

	if st.count() != 0 {
		t.Error("malformed reading must not write a row")
	}
	if uc, pc := cl.calls(); uc != 2 || pc != 0 {
		t.Errorf("usage=%d probe=%d, want 2 and 0 (no backoff, no probe)", uc, pc)
	}
}

// D5: a transport failure is transient — no row, no backoff, retried next tick.
func TestTransportNoBackoff(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return anthropic.Reading{}, transport() }}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)

	e.tickAll(context.Background())
	e.tickAll(context.Background())

	if uc, pc := cl.calls(); uc != 2 || pc != 0 {
		t.Errorf("usage=%d probe=%d, want 2 and 0", uc, pc)
	}
}

// D4: the row is overwritten each tick — a later reading replaces the earlier one.
func TestUpsertOverwrite(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	pct := 10
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(pct, pct, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)

	e.tickAll(context.Background())
	pct = 90
	e.tickAll(context.Background())

	got, _ := st.got(u)
	if got.FiveHourPct.Int16 != 90 {
		t.Errorf("five_hour_pct = %d, want 90 (overwritten)", got.FiveHourPct.Int16)
	}
	if st.count() != 1 {
		t.Errorf("row count = %d, want 1 (one row per user)", st.count())
	}
}

// A successful probe clears a prior backoff so the user resumes normal polling.
func TestSuccessClearsBackoff(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	fail := true
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		if fail {
			return anthropic.Reading{}, httpErr(429)
		}
		return reading(1, 1, anthropic.SourceUsageEndpoint), nil
	}}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, false)

	e.tickAll(context.Background()) // refusal -> backoff
	fail = false
	clk.advance(backoffDuration + time.Second)
	e.tickAll(context.Background()) // succeeds -> clears backoff
	e.tickAll(context.Background()) // immediately polls again (not backed off)

	if uc, _ := cl.calls(); uc != 3 {
		t.Fatalf("usage called %d times, want 3", uc)
	}
	if e.inBackoff(u) {
		t.Error("backoff should be cleared after a success")
	}
}

// D3b: the poke path ignores an existing backoff and polls the single user out of
// band (a just-saved token may work where the old one refused).
func TestPokeIgnoresBackoff(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	op := &fakeOpener{}
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return reading(5, 5, anthropic.SourceUsageEndpoint), nil }}
	e, _ := newEngine(t, st, op, cl, true)

	e.setBackoff(u) // pretend a prior refusal armed the backoff (secret id == user id here)
	// The real poke path: resolve the user's default token, then poll it ignoring backoff.
	e.pokeUser(context.Background(), u)

	if _, ok := st.got(u); !ok {
		t.Error("poke should poll despite the backoff")
	}
	if e.inBackoff(u) {
		t.Error("poke should clear the backoff")
	}
}

// Poke is non-blocking and never panics even when the buffer is saturated.
func TestPokeNonBlocking(t *testing.T) {
	e, _ := newEngine(t, newFakeStore(), &fakeOpener{}, &fakeClient{}, true)
	for i := 0; i < pokeBuffer+10; i++ {
		e.Poke(uuid.New()) // must not block or panic past the buffer
	}
}

// multiTokenStore models one user holding SEVERAL tokens with distinct secret ids,
// which the single-token fakeStore cannot express. It is the fixture for the one
// property M5's per-token repoint exists to provide.
type multiTokenStore struct {
	rows []store.ListAnthropicTokensToPollRow

	mu      sync.Mutex
	upserts map[uuid.UUID]store.UpsertRateLimitsParams // keyed by user_secret_id
	prev    map[uuid.UUID]store.AnthropicRateLimit     // keyed by user_secret_id
	notify  map[uuid.UUID]bool                         // keyed by user id
}

func (m *multiTokenStore) ListAnthropicTokensToPoll(context.Context) ([]store.ListAnthropicTokensToPollRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]store.ListAnthropicTokensToPollRow, len(m.rows))
	for i, r := range m.rows {
		r.NotifyEarlyLimitReset = m.notify[r.UserID]
		out[i] = r
	}
	return out, nil
}
func (m *multiTokenStore) GetDefaultUserSecretID(context.Context, store.GetDefaultUserSecretIDParams) (uuid.UUID, error) {
	return m.rows[0].ID, nil
}
func (m *multiTokenStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	return store.GetUserSecretCiphertextRow{Ciphertext: []byte("ct"), SealedWith: store.SealedWithDEK}, nil
}
func (m *multiTokenStore) UpsertRateLimits(_ context.Context, arg store.UpsertRateLimitsParams) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upserts[arg.UserSecretID] = arg
	if m.prev != nil {
		m.prev[arg.UserSecretID] = store.AnthropicRateLimit(arg)
	}
	return nil
}
func (m *multiTokenStore) GetRateLimitsForToken(_ context.Context, secretID uuid.UUID) (store.AnthropicRateLimit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.prev[secretID]; ok {
		return r, nil
	}
	return store.AnthropicRateLimit{}, pgx.ErrNoRows
}
func (m *multiTokenStore) GetUserByID(_ context.Context, id uuid.UUID) (store.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return store.User{ID: id, NotifyEarlyLimitReset: m.notify[id]}, nil
}
func (m *multiTokenStore) got(secretID uuid.UUID) (store.UpsertRateLimitsParams, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.upserts[secretID]
	return v, ok
}

// TestPerTokenPolledIndependently: a user with three tokens produces THREE gauge
// rows in one tick, each keyed by its own secret id and carrying its own reading.
// Under the pre-M5 per-user gauge this was impossible — three tokens raced one row.
func TestPerTokenPolledIndependently(t *testing.T) {
	user := uuid.New()
	tokA, tokB, tokC := uuid.New(), uuid.New(), uuid.New()
	st := &multiTokenStore{
		rows: []store.ListAnthropicTokensToPollRow{
			{ID: tokA, UserID: user, Ciphertext: []byte("a"), SealedWith: store.SealedWithMaster},
			{ID: tokB, UserID: user, Ciphertext: []byte("b"), SealedWith: store.SealedWithMaster},
			{ID: tokC, UserID: user, Ciphertext: []byte("c"), SealedWith: store.SealedWithMaster},
		},
		upserts: map[uuid.UUID]store.UpsertRateLimitsParams{},
	}
	// A distinct reading per token, so a row written against the wrong id would show.
	pctByCipher := map[string]int{"a": 11, "b": 22, "c": 33}
	cl := &fakeClient{usage: func(tok []byte) (anthropic.Reading, error) {
		p := pctByCipher[string(tok)]
		return reading(p, p, anthropic.SourceUsageEndpoint), nil
	}}
	// passthroughOpener returns the ciphertext as the token, so the client tells them apart.
	e := New(st, passthroughOpener{}, cl, time.Minute, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	e.Boot(context.Background())

	for id, want := range map[uuid.UUID]int{tokA: 11, tokB: 22, tokC: 33} {
		got, ok := st.got(id)
		if !ok {
			t.Fatalf("token %s got no gauge row — a user's tokens are not polled independently", id)
		}
		if int(got.FiveHourPct.Int16) != want {
			t.Errorf("token %s pct = %d, want %d (a reading landed on the wrong token's row)", id, got.FiveHourPct.Int16, want)
		}
		if got.UserID != user {
			t.Errorf("token %s row user_id = %v, want %v", id, got.UserID, user)
		}
	}
	if len(st.upserts) != 3 {
		t.Fatalf("wrote %d gauge rows, want 3 (one per token)", len(st.upserts))
	}
}

// TestPerTokenBackoffIsolation: one refusing credential must NOT silence its owner's
// other tokens. This is the exact case the feature exists for — a throttled
// subscription alongside a working console key — and under the pre-M5 per-USER
// backoff, backing off the user would have skipped every token they hold.
func TestPerTokenBackoffIsolation(t *testing.T) {
	user := uuid.New()
	bad, good := uuid.New(), uuid.New()
	st := &multiTokenStore{
		rows: []store.ListAnthropicTokensToPollRow{
			{ID: bad, UserID: user, Ciphertext: []byte("bad"), SealedWith: store.SealedWithMaster},
			{ID: good, UserID: user, Ciphertext: []byte("good"), SealedWith: store.SealedWithMaster},
		},
		upserts: map[uuid.UUID]store.UpsertRateLimitsParams{},
	}
	cl := &fakeClient{usage: func(tok []byte) (anthropic.Reading, error) {
		if string(tok) == "bad" {
			return anthropic.Reading{}, httpErr(429) // definitive refusal → backoff
		}
		return reading(7, 7, anthropic.SourceUsageEndpoint), nil
	}}
	e := New(st, passthroughOpener{}, cl, time.Minute, false, /* probe off → refusal arms backoff */
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	e.tickAll(context.Background())

	// The good token got its reading despite its sibling refusing.
	if _, ok := st.got(good); !ok {
		t.Fatal("the working token was not polled — a sibling's refusal silenced it")
	}
	if _, ok := st.got(bad); ok {
		t.Error("the refusing token must not have written a row")
	}
	// Backoff is per-token: the bad one is backed off, the good one is not.
	if !e.inBackoff(bad) {
		t.Error("the refusing token should be backed off")
	}
	if e.inBackoff(good) {
		t.Error("the working token must NOT be backed off — backoff is per-token, not per-user")
	}
}

// passthroughOpener returns the ciphertext verbatim as the token, so a per-token
// test's client can tell the tokens apart by their bytes.
type passthroughOpener struct{}

func (passthroughOpener) Open(_ context.Context, _ uuid.UUID, _ string) ([]byte, error) {
	return []byte("default"), nil
}
func (passthroughOpener) OpenSealed(_ uuid.UUID, _, _ string, ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}

// --- early-reset detection (PRD #1020 M2) ---

type earlyResetCall struct {
	userID             uuid.UUID
	expected, observed time.Time
}

// fakeNotifier captures NotifyEarlyReset calls so a test can assert fire/silence and
// the exact (expected, observed) times passed.
type fakeNotifier struct {
	mu    sync.Mutex
	calls []earlyResetCall
}

func (f *fakeNotifier) NotifyEarlyReset(_ context.Context, userID uuid.UUID, expected, observed time.Time) (store.Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, earlyResetCall{userID: userID, expected: expected, observed: observed})
	return store.Notification{}, nil
}
func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
func (f *fakeNotifier) last() (earlyResetCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return earlyResetCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// prevRow builds a stored gauge row for the 7-day window: resetsAt is the previously
// reported reset T, syncedAt when it was observed. A pending 7-day gauge row is prevRow
// with syncedAt < resetsAt. source/sevenPct are inputs the two arms consume (Arm B reads
// sevenPct), not a "was limiting" precondition — that gate was removed (PRD #1114).
func prevRow(userID, secretID uuid.UUID, sevenPct int16, source string, resetsAt, syncedAt time.Time) store.AnthropicRateLimit {
	return store.AnthropicRateLimit{
		UserSecretID:     secretID,
		UserID:           userID,
		SevenDayPct:      pgtype.Int2{Int16: sevenPct, Valid: true},
		SevenDayResetsAt: pgtype.Timestamptz{Time: resetsAt, Valid: true},
		Source:           pgtype.Text{String: source, Valid: true},
		SyncedAt:         pgtype.Timestamptz{Time: syncedAt, Valid: true},
	}
}

// readingWithReset is a fresh reading whose 7-day window reports resetsAt (a *time.Time,
// nil = unset — Arm A's boundary input; sevenPct feeds Arm B).
func readingWithReset(sevenPct int, source string, resetsAt *time.Time) anthropic.Reading {
	r := reading(0, sevenPct, source)
	r.SevenDay.ResetsAt = resetsAt
	return r
}

// TestEarlyResetDetection is the mutation-checked table over (prev, next, now). Each
// SILENT case is designed to FLIP to firing if the guard it targets were removed, and
// says which mutation it pins.
func TestEarlyResetDetection(t *testing.T) {
	// T is the previously advertised 7-day reset; the prior sync is well before it, so
	// the window was genuinely pending. `moved` is a later reset epoch the fresh reading
	// reports (the accepted "reset observed" signal).
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	synced := tReset.Add(-72 * time.Hour)
	moved := tReset.Add(72 * time.Hour)
	movedTiny := tReset.Add(30 * time.Minute)  // +30m < resetEpochMoveMargin(1h): Arm A must stay silent
	movedBack := tReset.Add(-30 * time.Minute) // boundary earlier than T: not an unmoved fixed grid -> Arm B must stay silent

	cases := []struct {
		name       string
		prevPct    int16
		prevSource string
		nextResets *time.Time
		nextPct    int
		nextSource string
		now        time.Time
		notify     bool
		wantFire   bool
		why        string
	}{
		{
			name: "fires_limit_report", prevPct: 99, prevSource: anthropic.SourceLimitReport,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: true,
			why: "epoch moved 72h > 1h margin -> Arm A; 10h > 8h early (no longer via the removed limiting gate)",
		},
		{
			name: "fires_high_pct", prevPct: 96, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: true,
			why: "epoch moved 72h > 1h margin -> Arm A; 10h early (no longer via the removed pct>=95 gate)",
		},
		{
			name: "silent_modestly_inside_8h", prevPct: 99, prevSource: anthropic.SourceLimitReport,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-6 * time.Hour), notify: true, wantFire: false,
			why: "6h < 8h threshold -> silent; WOULD fire if earlyResetThreshold were 0",
		},
		{
			name: "silent_on_time", prevPct: 99, prevSource: anthropic.SourceLimitReport,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset, notify: true, wantFire: false,
			why: "earliness ~0 -> silent",
		},
		{
			name: "silent_boundary_exactly_8h", prevPct: 99, prevSource: anthropic.SourceLimitReport,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-earlyResetThreshold), notify: true, wantFire: false,
			why: "now == T-8h; now.Before(T-8h) is strict, so exactly -8h is NOT early",
		},
		{
			name: "fires_arm_a_unconstrained", prevPct: 50, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: true,
			why: "unconstrained window (pct 50, usage_endpoint) whose boundary moved forward >8h early now fires via Arm A; the removed gate no longer suppresses it",
		},
		{
			name: "silent_setting_off", prevPct: 99, prevSource: anthropic.SourceLimitReport,
			nextResets: &moved, nextPct: 10, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: false, wantFire: false,
			why: "owner opted out -> no read, no fire, even with a fireable config",
		},
		{
			name: "silent_pct_jitter_epoch_unchanged", prevPct: 100, prevSource: anthropic.SourceLimitReport,
			nextResets: &tReset, nextPct: 99, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "epoch unchanged (== T, Arm A false) AND used% 100->99 not a zeroing (99 > pctResetCeil, Arm B false) -> silent",
		},
		{
			name: "fires_arm_b_util_zeroed", prevPct: 12, prevSource: anthropic.SourceHeaderProbe,
			nextResets: &tReset, nextPct: 0, nextSource: anthropic.SourceHeaderProbe,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: true,
			why: "fixed-grid early clear: used% 12->0 with an unmoved boundary fires via Arm B; the moved-epoch-only predicate missed this",
		},
		{
			name: "fires_arm_b_at_floor", prevPct: 5, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &tReset, nextPct: 0, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: true,
			why: "prev == pctResetFloor(5) and next <= pctResetCeil(1) fires Arm B; pins the floor boundary",
		},
		{
			name: "silent_epoch_move_below_margin", prevPct: 50, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &movedTiny, nextPct: 50, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "+30m < resetEpochMoveMargin(1h) so Arm A silent; pct 50->50 unchanged so Arm B false; WOULD fire if the margin were 0",
		},
		{
			name: "silent_pct_drop_below_floor", prevPct: 3, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &tReset, nextPct: 0, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "prev 3 < pctResetFloor(5) so Arm B silent (near-zero-usage clear indistinguishable from jitter, D5); WOULD fire if the floor were 0",
		},
		{
			name: "silent_pct_drop_above_ceil", prevPct: 40, prevSource: anthropic.SourceUsageEndpoint,
			nextResets: &tReset, nextPct: 20, nextSource: anthropic.SourceUsageEndpoint,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "next 20 > pctResetCeil(1) is a source-flip/partial delta, not a zeroing, so Arm B silent; WOULD fire if the ceil were raised",
		},
		{
			name: "silent_arm_b_boundary_nil", prevPct: 12, prevSource: anthropic.SourceHeaderProbe,
			nextResets: nil, nextPct: 0, nextSource: anthropic.SourceHeaderProbe,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "used% 12->0 but the fresh reading carries no reset boundary, so the fixed-grid model is unconfirmable -> Arm B silent; WOULD fire if Arm B dropped the ResetsAt!=nil && Equal(t) guard",
		},
		{
			name: "silent_arm_b_sub_margin_move", prevPct: 12, prevSource: anthropic.SourceHeaderProbe,
			nextResets: &movedTiny, nextPct: 0, nextSource: anthropic.SourceHeaderProbe,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "+30m < resetEpochMoveMargin(1h) so Arm A silent; boundary moved (!= T) so the grid is not unmoved -> Arm B silent even though used% 12->0; WOULD fire if Arm B ignored the boundary",
		},
		{
			name: "silent_arm_b_backward_move", prevPct: 12, prevSource: anthropic.SourceHeaderProbe,
			nextResets: &movedBack, nextPct: 0, nextSource: anthropic.SourceHeaderProbe,
			now: tReset.Add(-10 * time.Hour), notify: true, wantFire: false,
			why: "boundary earlier than T (moved backward) is not an unmoved fixed grid -> Arm B silent even though used% 12->0; WOULD fire if Arm B ignored the boundary",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := uuid.New()
			st := newFakeStore(u)
			st.setNotify(u, tc.notify)
			st.setPrev(u, prevRow(u, u, tc.prevPct, tc.prevSource, tReset, synced))
			next := readingWithReset(tc.nextPct, tc.nextSource, tc.nextResets)
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) { return next, nil }}
			notif := &fakeNotifier{}
			e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
			clk.set(tc.now)
			e.SetNotifier(notif)

			e.tickAll(context.Background())

			// The upsert must happen on EVERY reading, fire or not.
			if _, ok := st.got(u); !ok {
				t.Fatal("upsert must happen on every successful reading")
			}
			if fired := notif.count() > 0; fired != tc.wantFire {
				t.Fatalf("fired=%v, want %v (%s)", fired, tc.wantFire, tc.why)
			}
			if tc.wantFire {
				call, _ := notif.last()
				if !call.expected.Equal(tReset) {
					t.Errorf("expected reset = %v, want T=%v", call.expected, tReset)
				}
				if !call.observed.Equal(tc.now) {
					t.Errorf("observed = %v, want now=%v", call.observed, tc.now)
				}
				if call.userID != u {
					t.Errorf("notified user = %v, want %v", call.userID, u)
				}
			}
		})
	}
}

// TestEarlyResetUpsertBeforeNotify: the fresh reading is written to the gauge BEFORE
// the notify fires (D7 ordering), so the upserted row already reflects the moved epoch.
func TestEarlyResetUpsertBeforeNotify(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 99, anthropic.SourceLimitReport, tReset, tReset.Add(-72*time.Hour)))
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	if notif.count() != 1 {
		t.Fatalf("notify calls = %d, want 1", notif.count())
	}
	got, ok := st.got(u)
	if !ok {
		t.Fatal("expected an upserted row")
	}
	if !got.SevenDayResetsAt.Valid || !got.SevenDayResetsAt.Time.Equal(moved) {
		t.Errorf("upserted reset = %v, want the moved epoch %v (write must land before notify)", got.SevenDayResetsAt.Time, moved)
	}
}

// TestEarlyResetFailedUpsertDoesNotNotify: SC5. When the gauge write fails, the moved
// epoch is NOT persisted, so prev still holds the old boundary and the edge is not
// consumed. Notifying anyway would re-fire on every subsequent tick until the write
// lands, breaking at-most-once. A failed upsert must therefore stay silent — even
// though every fire precondition (constrained prior, moved epoch, ≥8h early) holds.
func TestEarlyResetFailedUpsertDoesNotNotify(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 99, anthropic.SourceLimitReport, tReset, tReset.Add(-72*time.Hour)))
	st.upsertErr = errors.New("boom") // the write fails on this tick
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour)) // 10h early → well past the 8h threshold
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	if notif.count() != 0 {
		t.Fatalf("notify calls = %d, want 0 (a failed upsert must not fire — SC5)", notif.count())
	}
	// The write did not land, so the edge was not consumed: a later successful tick
	// with the same reading can still fire exactly once.
	if _, ok := st.got(u); ok {
		t.Error("a failed upsert recorded a write; prev must stay at the old epoch")
	}
}

// TestEarlyResetOnceOnly: after firing, the upserted `next` becomes `prev`; a second
// identical tick does NOT re-fire because the epoch has not moved again (next.resets ==
// prev.resets). The moved-epoch guard is what consumes the edge — the second tick keeps
// a high pct + limit_report source, so only the unchanged epoch prevents the re-fire.
func TestEarlyResetOnceOnly(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 100, anthropic.SourceLimitReport, tReset, tReset.Add(-72*time.Hour)))
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(100, anthropic.SourceLimitReport, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background()) // fires
	e.tickAll(context.Background()) // identical epoch -> edge already consumed

	if notif.count() != 1 {
		t.Fatalf("notify calls = %d, want exactly 1 (the moved-epoch edge is consumed once)", notif.count())
	}
}

// TestEarlyResetPerTokenFanOut: N tokens each early-resetting produce N NotifyEarlyReset
// calls in one tick.
func TestEarlyResetPerTokenFanOut(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	synced := tReset.Add(-72 * time.Hour)
	user := uuid.New()
	tokA, tokB, tokC := uuid.New(), uuid.New(), uuid.New()
	st := &multiTokenStore{
		rows: []store.ListAnthropicTokensToPollRow{
			{ID: tokA, UserID: user, Ciphertext: []byte("a"), SealedWith: store.SealedWithMaster},
			{ID: tokB, UserID: user, Ciphertext: []byte("b"), SealedWith: store.SealedWithMaster},
			{ID: tokC, UserID: user, Ciphertext: []byte("c"), SealedWith: store.SealedWithMaster},
		},
		upserts: map[uuid.UUID]store.UpsertRateLimitsParams{},
		prev: map[uuid.UUID]store.AnthropicRateLimit{
			tokA: prevRow(user, tokA, 99, anthropic.SourceLimitReport, tReset, synced),
			tokB: prevRow(user, tokB, 99, anthropic.SourceLimitReport, tReset, synced),
			tokC: prevRow(user, tokC, 99, anthropic.SourceLimitReport, tReset, synced),
		},
		notify: map[uuid.UUID]bool{user: true},
	}
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, passthroughOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	if notif.count() != 3 {
		t.Fatalf("notify calls = %d, want 3 (one per early-resetting token)", notif.count())
	}
}

// TestEarlyResetSettingOffSuppressesAllTokens: with the owner opted out, NONE of their
// tokens fire even though every one is a fireable configuration.
func TestEarlyResetSettingOffSuppressesAllTokens(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	synced := tReset.Add(-72 * time.Hour)
	user := uuid.New()
	tokA, tokB := uuid.New(), uuid.New()
	st := &multiTokenStore{
		rows: []store.ListAnthropicTokensToPollRow{
			{ID: tokA, UserID: user, Ciphertext: []byte("a"), SealedWith: store.SealedWithMaster},
			{ID: tokB, UserID: user, Ciphertext: []byte("b"), SealedWith: store.SealedWithMaster},
		},
		upserts: map[uuid.UUID]store.UpsertRateLimitsParams{},
		prev: map[uuid.UUID]store.AnthropicRateLimit{
			tokA: prevRow(user, tokA, 99, anthropic.SourceLimitReport, tReset, synced),
			tokB: prevRow(user, tokB, 99, anthropic.SourceLimitReport, tReset, synced),
		},
		notify: map[uuid.UUID]bool{user: false}, // opted out
	}
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, passthroughOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	if notif.count() != 0 {
		t.Fatalf("notify calls = %d, want 0 (setting OFF suppresses every token)", notif.count())
	}
	// The gauges are still written — detection being off does not suppress the upsert.
	if _, ok := st.got(tokA); !ok {
		t.Error("token A gauge must still be upserted with the setting off")
	}
}

// TestEarlyResetNoPriorRow: with the setting on but no prior gauge row
// (GetRateLimitsForToken -> ErrNoRows), detection is silent and errors nothing.
func TestEarlyResetNoPriorRow(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true) // no setPrev: GetRateLimitsForToken returns ErrNoRows
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	if notif.count() != 0 {
		t.Fatalf("notify calls = %d, want 0 (no prior row -> no comparison basis)", notif.count())
	}
	if _, ok := st.got(u); !ok {
		t.Error("the first reading must still be upserted")
	}
}

// TestEarlyResetNilNotifier: with the setting on and a fireable config but NO notifier
// wired, detection runs without panic and still upserts. The default (unit) path.
func TestEarlyResetNilNotifier(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 99, anthropic.SourceLimitReport, tReset, tReset.Add(-72*time.Hour)))
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	// No SetNotifier: e.notifier stays nil.

	e.tickAll(context.Background()) // must not panic

	if _, ok := st.got(u); !ok {
		t.Error("detection with a nil notifier must still upsert")
	}
}

// TestEarlyResetOnPokePath: the poke/single-token path runs detection too, reading the
// owner opt-in via GetUserByID. A poke that skipped detection would upsert the moved
// epoch and silently consume the alert edge.
func TestEarlyResetOnPokePath(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 99, anthropic.SourceLimitReport, tReset, tReset.Add(-72*time.Hour)))
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.pokeUser(context.Background(), u)

	if notif.count() != 1 {
		t.Fatalf("notify calls = %d, want 1 (detection must run on the poke path)", notif.count())
	}
}
