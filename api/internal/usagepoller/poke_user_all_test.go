package usagepoller

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/secretopen"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// ctOpener opens a row as its ciphertext (so a client can tell tokens apart), fails a
// ciphertext listed in errs, and records every ciphertext it was asked to open.
type ctOpener struct {
	errs map[string]error

	mu     sync.Mutex
	opened []string
}

func (o *ctOpener) OpenSealedAtEpoch(_ context.Context, _ uuid.UUID, _, _ string, ciphertext []byte, _ int64) ([]byte, error) {
	o.mu.Lock()
	o.opened = append(o.opened, string(ciphertext))
	o.mu.Unlock()
	if err, ok := o.errs[string(ciphertext)]; ok {
		return nil, err
	}
	return append([]byte(nil), ciphertext...), nil
}

func (o *ctOpener) openedSet() map[string]bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := map[string]bool{}
	for _, c := range o.opened {
		out[c] = true
	}
	return out
}

// ctxClient records the context deadline and token of every Usage call and delegates
// to fn, which receives the call's context.
type ctxClient struct {
	fn func(ctx context.Context, token []byte) (anthropic.Reading, error)

	mu        sync.Mutex
	tokens    []string
	deadlines []time.Time
	hadDL     []bool
}

func (c *ctxClient) Usage(ctx context.Context, token []byte) (anthropic.Reading, error) {
	dl, ok := ctx.Deadline()
	c.mu.Lock()
	c.tokens = append(c.tokens, string(token))
	c.deadlines = append(c.deadlines, dl)
	c.hadDL = append(c.hadDL, ok)
	c.mu.Unlock()
	return c.fn(ctx, token)
}

func (c *ctxClient) ProbeHeaders(context.Context, []byte) (anthropic.Reading, error) {
	return anthropic.Reading{}, httpErr(429)
}

func (c *ctxClient) polled() map[string]bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]bool{}
	for _, t := range c.tokens {
		out[t] = true
	}
	return out
}

func okUsage(context.Context, []byte) (anthropic.Reading, error) {
	return reading(7, 8, anthropic.SourceUsageEndpoint), nil
}

// addToken appends a token row for user with a distinct secret id; the ciphertext is
// name, which ctOpener hands back as the token.
func addToken(st *fakeStore, user uuid.UUID, name string) uuid.UUID {
	id := uuid.New()
	st.rows = append(st.rows, store.ListAnthropicTokensToPollRow{
		ID: id, UserID: user, Ciphertext: []byte(name), SealedWith: store.SealedWithDEK,
	})
	return id
}

// dispatchPoke takes the queued poke off the engine's channel and services it the way
// Run does.
func dispatchPoke(t *testing.T, e *Engine) {
	t.Helper()
	select {
	case p := <-e.poke:
		e.handlePoke(context.Background(), p)
	default:
		t.Fatal("no poke was queued")
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// #4406: the vault-ready poke polls every enabled token of the user, not just the
// default, and never touches another user's tokens.
func TestPokeUserAllPollsEveryEnabledToken(t *testing.T) {
	user, other := uuid.New(), uuid.New()
	st := newFakeStore()
	def := addToken(st, user, "default")
	spare := addToken(st, user, "spare")
	foreign := addToken(st, other, "foreign")
	op := &ctOpener{}
	cl := &ctxClient{fn: okUsage}
	e, _ := newEngine(t, st, op, cl, true)

	e.PokeUserAll(user)
	dispatchPoke(t, e)

	for name, id := range map[string]uuid.UUID{"default": def, "spare": spare} {
		if _, ok := st.got(id); !ok {
			t.Errorf("%s token got no gauge row from PokeUserAll", name)
		}
	}
	if _, ok := st.got(foreign); ok {
		t.Error("another user's token must not get a row")
	}
	if op.openedSet()["foreign"] || cl.polled()["foreign"] {
		t.Error("another user's token was opened or polled")
	}
}

func TestPokeUserAllSkipsDisabledToken(t *testing.T) {
	user := uuid.New()
	st := newFakeStore()
	def := addToken(st, user, "default")
	off := addToken(st, user, "disabled")
	st.transition(off, false)
	op := &ctOpener{}
	cl := &ctxClient{fn: okUsage}
	e, _ := newEngine(t, st, op, cl, true)

	e.PokeUserAll(user)
	dispatchPoke(t, e)

	if _, ok := st.got(def); !ok {
		t.Error("enabled token should be polled")
	}
	if _, ok := st.got(off); ok {
		t.Error("disabled token must not get a row")
	}
	if op.openedSet()["disabled"] || cl.polled()["disabled"] {
		t.Error("disabled token was opened or polled")
	}
}

// The poke runs under a deadline of one interval, so a hung provider call cannot hold
// the poke loop indefinitely.
func TestPokeUserAllHasIntervalDeadline(t *testing.T) {
	user := uuid.New()
	st := newFakeStore()
	addToken(st, user, "default")
	cl := &ctxClient{fn: func(ctx context.Context, _ []byte) (anthropic.Reading, error) {
		<-ctx.Done()
		return anthropic.Reading{}, ctx.Err()
	}}
	e, _ := newEngine(t, st, &ctOpener{}, cl, true)
	e.interval = 200 * time.Millisecond

	e.PokeUserAll(user)
	start := time.Now()
	done := make(chan struct{})
	go func() {
		dispatchPoke(t, e)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("PokeUserAll did not return; the interval deadline is not applied")
	}

	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.tokens) != 1 || !cl.hadDL[0] {
		t.Fatalf("Usage calls = %d, hadDeadline = %v; want one call with a deadline", len(cl.tokens), cl.hadDL)
	}
	if limit := start.Add(e.interval + 50*time.Millisecond); cl.deadlines[0].After(limit) {
		t.Errorf("deadline %v is later than poke start + interval (%v)", cl.deadlines[0], limit)
	}
}

// PokeUserAll honours backoff (unlock changes decryptability, not the credential), so
// a token in backoff is neither polled nor released from it; its sibling is polled.
func TestPokeUserAllHonoursBackoff(t *testing.T) {
	user := uuid.New()
	st := newFakeStore()
	def := addToken(st, user, "default")
	sib := addToken(st, user, "sibling")
	cl := &ctxClient{fn: okUsage}
	e, _ := newEngine(t, st, &ctOpener{}, cl, true)
	e.setBackoff(def)

	e.PokeUserAll(user)
	dispatchPoke(t, e)

	if _, ok := st.got(def); ok || cl.polled()["default"] {
		t.Error("token in backoff must not be polled")
	}
	if !e.inBackoff(def) {
		t.Error("PokeUserAll must not clear a token's backoff")
	}
	if _, ok := st.got(sib); !ok {
		t.Error("sibling token should be polled")
	}
}

// The single-token PokeSecret still ignores backoff and clears it, for a non-default
// named token.
func TestPokeSecretIgnoresBackoff(t *testing.T) {
	user := uuid.New()
	st := newFakeStore(user) // default token: id == user
	named := addToken(st, user, "named")
	cl := &ctxClient{fn: okUsage}
	e, _ := newEngine(t, st, &ctOpener{}, cl, true)
	e.setBackoff(named)

	e.PokeSecret(user, named)
	dispatchPoke(t, e)

	if _, ok := st.got(named); !ok {
		t.Error("PokeSecret should poll the named token despite its backoff")
	}
	if e.inBackoff(named) {
		t.Error("PokeSecret should clear the named token's backoff")
	}
}

// A locked vault on one token is skipped silently: no provider call, no backoff, no
// warn or error line, and the user's other token is still polled.
func TestPokeUserAllLockedVaultSkipsSilently(t *testing.T) {
	user := uuid.New()
	st := newFakeStore()
	locked := addToken(st, user, "locked")
	other := addToken(st, user, "other")
	op := &ctOpener{errs: map[string]error{"locked": secretopen.ErrVaultLocked}}
	cl := &ctxClient{fn: okUsage}
	e, _ := newEngine(t, st, op, cl, true)
	var logs syncBuf
	e.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	e.PokeUserAll(user)
	dispatchPoke(t, e)

	if cl.polled()["locked"] {
		t.Error("a locked token must not reach the provider")
	}
	if _, ok := st.got(locked); ok {
		t.Error("a locked token must not get a row")
	}
	if e.inBackoff(locked) {
		t.Error("a locked-vault skip must arm no backoff")
	}
	if _, ok := st.got(other); !ok {
		t.Error("the unlocked token should be polled")
	}
	out := logs.String()
	for _, bad := range []string{"level=WARN", "level=ERROR", "usage poller:", locked.String()} {
		if strings.Contains(out, bad) {
			t.Errorf("skip must be silent, but the log contains %q:\n%s", bad, out)
		}
	}
}
