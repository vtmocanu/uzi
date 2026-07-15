package usagepoller

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/anthropic"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretopen"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// --- fakes ---

type fakeStore struct {
	rows []store.ListUsersWithAnthropicTokenRow

	mu      sync.Mutex
	upserts map[uuid.UUID]store.UpsertRateLimitsParams
}

func newFakeStore(users ...uuid.UUID) *fakeStore {
	rows := make([]store.ListUsersWithAnthropicTokenRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, store.ListUsersWithAnthropicTokenRow{UserID: u, Ciphertext: []byte("ct"), SealedWith: store.SealedWithDEK})
	}
	return &fakeStore{rows: rows, upserts: map[uuid.UUID]store.UpsertRateLimitsParams{}}
}
func (f *fakeStore) ListUsersWithAnthropicToken(context.Context) ([]store.ListUsersWithAnthropicTokenRow, error) {
	return f.rows, nil
}
func (f *fakeStore) GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error) {
	return store.GetUserSecretCiphertextRow{Ciphertext: []byte("ct"), SealedWith: store.SealedWithDEK}, nil
}
func (f *fakeStore) UpsertRateLimits(_ context.Context, arg store.UpsertRateLimitsParams) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts[arg.UserID] = arg
	return nil
}
func (f *fakeStore) got(u uuid.UUID) (store.UpsertRateLimitsParams, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.upserts[u]
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

	e.setBackoff(u) // pretend a prior refusal armed the backoff
	// Mirror the poke path: ignoreBackoff + a single-user lookup-open.
	e.pollUser(context.Background(), u, true, func() ([]byte, error) {
		return op.Open(context.Background(), u, store.KindAnthropicToken)
	})

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
