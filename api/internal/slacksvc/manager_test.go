package slacksvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeSettings is a mutable, concurrency-safe SettingsSource the test drives to
// simulate an admin toggling Slack or rotating tokens.
type fakeSettings struct {
	mu         sync.Mutex
	enabled    bool
	bot, app   string
	enabledErr error
}

func (f *fakeSettings) set(enabled bool, bot, app string) {
	f.mu.Lock()
	f.enabled, f.bot, f.app = enabled, bot, app
	f.mu.Unlock()
}

func (f *fakeSettings) SlackEnabled(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.enabled, f.enabledErr
}

func (f *fakeSettings) SlackBotToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bot, nil
}

func (f *fakeSettings) SlackAppToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.app, nil
}

// dialCall records one invocation of the fake dialer and lets the test drive its
// lifecycle: call onConnecting/onConnected to simulate the session coming up, and
// send on ret (or cancel ctx) to make the dial return.
type dialCall struct {
	bot, app                  string
	ctx                       context.Context
	onConnecting, onConnected func()
	ret                       chan error
}

type fakeDialer struct {
	calls chan *dialCall
}

func newFakeDialer() *fakeDialer {
	return &fakeDialer{calls: make(chan *dialCall, 8)}
}

func (f *fakeDialer) dial(ctx context.Context, bot, app string, onConnecting, onConnected func()) error {
	c := &dialCall{bot: bot, app: app, ctx: ctx, onConnecting: onConnecting, onConnected: onConnected, ret: make(chan error, 1)}
	select {
	case f.calls <- c:
	case <-ctx.Done():
		return nil
	}
	select {
	case <-ctx.Done():
		return nil
	case err := <-c.ret:
		return err
	}
}

// await receives the next dial invocation, failing if none arrives in time.
func (f *fakeDialer) await(t *testing.T) *dialCall {
	t.Helper()
	select {
	case c := <-f.calls:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("expected a dial invocation, got none")
		return nil
	}
}

// expectNoDial fails if any dial happens within the window (used for the
// disabled/unconfigured cases).
func (f *fakeDialer) expectNoDial(t *testing.T, window time.Duration) {
	t.Helper()
	select {
	case <-f.calls:
		t.Fatal("dialed while Slack should be idle")
	case <-time.After(window):
	}
}

func waitState(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.State() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("state = %q, want %q", m.State(), want)
}

// fastManager wires a manager with short intervals for a snappy test.
func fastManager(fs *fakeSettings, fd *fakeDialer) *Manager {
	return NewManager(fs, Config{
		Poll:       5 * time.Millisecond,
		BackoffMin: 5 * time.Millisecond,
		BackoffMax: 20 * time.Millisecond,
		Dial:       fd.dial,
	})
}

func TestManagerIdleWhenDisabled(t *testing.T) {
	for name, fs := range map[string]*fakeSettings{
		"disabled":       {enabled: false, bot: "xoxb-1", app: "xapp-1"},
		"no bot token":   {enabled: true, bot: "", app: "xapp-1"},
		"no app token":   {enabled: true, bot: "xoxb-1", app: ""},
		"read error off": {enabled: true, bot: "xoxb-1", app: "xapp-1", enabledErr: errors.New("db down")},
	} {
		t.Run(name, func(t *testing.T) {
			fd := newFakeDialer()
			m := fastManager(fs, fd)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go m.Run(ctx)
			fd.expectNoDial(t, 40*time.Millisecond)
			if m.State() != StateDisabled {
				t.Fatalf("state = %q, want disabled", m.State())
			}
		})
	}
}

func TestManagerConnects(t *testing.T) {
	fs := &fakeSettings{enabled: true, bot: "xoxb-1", app: "xapp-1"}
	fd := newFakeDialer()
	m := fastManager(fs, fd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	c := fd.await(t)
	if c.bot != "xoxb-1" || c.app != "xapp-1" {
		t.Fatalf("dialed with %q/%q, want xoxb-1/xapp-1", c.bot, c.app)
	}
	waitState(t, m, StateConnecting)
	c.onConnected()
	waitState(t, m, StateConnected)
}

func TestManagerHotReloadOnTokenChange(t *testing.T) {
	fs := &fakeSettings{enabled: true, bot: "xoxb-1", app: "xapp-1"}
	fd := newFakeDialer()
	m := fastManager(fs, fd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	c1 := fd.await(t)
	c1.onConnected()
	waitState(t, m, StateConnected)

	// Rotate tokens: the watcher must cancel the live connection and reconnect
	// with the new pair — no reboot.
	fs.set(true, "xoxb-2", "xapp-2")
	c2 := fd.await(t)
	if c2.app != "xapp-2" || c2.bot != "xoxb-2" {
		t.Fatalf("reconnected with %q/%q, want xoxb-2/xapp-2", c2.bot, c2.app)
	}
	// The first connection's context was cancelled (hot restart).
	select {
	case <-c1.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("old connection was not cancelled on token change")
	}
}

func TestManagerDisableStopsConnection(t *testing.T) {
	fs := &fakeSettings{enabled: true, bot: "xoxb-1", app: "xapp-1"}
	fd := newFakeDialer()
	m := fastManager(fs, fd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	c := fd.await(t)
	c.onConnected()
	waitState(t, m, StateConnected)

	fs.set(false, "xoxb-1", "xapp-1") // admin turns Slack off
	waitState(t, m, StateDisabled)
	select {
	case <-c.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("connection not torn down on disable")
	}
}

func TestManagerReconnectsAfterDrop(t *testing.T) {
	fs := &fakeSettings{enabled: true, bot: "xoxb-1", app: "xapp-1"}
	fd := newFakeDialer()
	m := fastManager(fs, fd)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	c1 := fd.await(t)
	c1.onConnected()
	waitState(t, m, StateConnected)

	// Simulate a socket drop: the manager backs off then reconnects.
	c1.ret <- errors.New("websocket closed")
	c2 := fd.await(t)
	if c2.app != "xapp-1" {
		t.Fatalf("reconnected with app %q, want xapp-1", c2.app)
	}
	c2.onConnected()
	waitState(t, m, StateConnected)
}

func TestManagerAuthErrorState(t *testing.T) {
	fs := &fakeSettings{enabled: true, bot: "xoxb-1", app: "xapp-1"}
	fd := newFakeDialer()
	// Longer backoff so the error state lingers long enough to observe before the
	// reconnect attempt.
	m := NewManager(fs, Config{Poll: 5 * time.Millisecond, BackoffMin: 300 * time.Millisecond, BackoffMax: time.Second, Dial: fd.dial})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)

	c := fd.await(t)
	c.ret <- errors.New("slack: invalid_auth")
	waitState(t, m, StateErrorAuth)
}

func TestManagerStopsOnContextCancel(t *testing.T) {
	fs := &fakeSettings{enabled: false}
	fd := newFakeDialer()
	m := fastManager(fs, fd)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()

	waitState(t, m, StateDisabled)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}

// TestNewManagerBuildsBoundedDialer exercises the production path (no injected
// Dial): NewManager must build the real socket dialer from HTTPTimeout without
// panicking, and idle cleanly while unconfigured (it never dials).
func TestNewManagerBuildsBoundedDialer(t *testing.T) {
	m := NewManager(&fakeSettings{enabled: false}, Config{
		Poll:        5 * time.Millisecond,
		HTTPTimeout: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	waitState(t, m, StateDisabled)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestClassifyState(t *testing.T) {
	cases := map[string]string{
		"slack: invalid_auth":                  StateErrorAuth,
		"error: account_inactive":              StateErrorAuth,
		"token_revoked":                        StateErrorAuth,
		"websocket: close 1006 (abnormal)":     StateErrorConnection,
		"dial tcp: connection refused":         StateErrorConnection,
		"slack: socket mode connection closed": StateErrorConnection,
	}
	for in, want := range cases {
		if got := classifyState(errors.New(in)); got != want {
			t.Errorf("classifyState(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNextBackoff(t *testing.T) {
	if got := nextBackoff(time.Second, time.Minute); got != 2*time.Second {
		t.Errorf("nextBackoff doubled to %v, want 2s", got)
	}
	if got := nextBackoff(40*time.Second, time.Minute); got != time.Minute {
		t.Errorf("nextBackoff capped to %v, want 1m", got)
	}
}
