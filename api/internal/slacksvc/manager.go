package slacksvc

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Connection-state values surfaced on the admin settings DTO for the webui chip
// (PRD #25 M2). The error states carry a coarse class only — never the
// underlying error text (which could embed a token or the wss ticket URL).
const (
	StateDisabled   = "disabled"   // Slack off, or not both tokens present
	StateConnecting = "connecting" // socket opening / re-opening
	StateConnected  = "connected"  // live session

	StateErrorAuth       = "error:auth"       // token rejected by Slack
	StateErrorConnection = "error:connection" // socket dropped / network
)

// SettingsSource is the slice of the settings cache the manager reads: the enable
// flag and the two DECRYPTED tokens. *settings.Cache satisfies it (its
// SlackBotToken/SlackAppToken accessors apply the ENV-over-DB precedence, so the
// manager honors an env-set token exactly like a DB one).
type SettingsSource interface {
	SlackEnabled(ctx context.Context) (bool, error)
	SlackBotToken(ctx context.Context) (string, error)
	SlackAppToken(ctx context.Context) (string, error)
}

// DialFunc opens and runs ONE Slack Socket Mode connection with the given tokens
// until ctx is cancelled — in which case it returns nil — or the link fails or
// drops, in which case it returns the error (which drives the manager's
// backoff). It reports state via onConnecting (socket opening) and onConnected
// (session live). dialSocketMode is the production implementation (socket.go);
// tests inject a fake so the whole supervisor is exercised offline.
type DialFunc func(ctx context.Context, botToken, appToken string, onConnecting, onConnected func()) error

// desired is the effective connection target: the token pair to connect with. It
// is comparable so the watcher can diff it against the running connection's pair
// and hot-restart on any change.
type desired struct {
	bot string
	app string
}

// Manager supervises the single Slack Socket Mode connection (PRD #25 M2). It
// polls the settings cache; while Slack is enabled with both tokens present it
// keeps one connection up with exponential-backoff reconnect, and a token or
// enable change hot-restarts the socket with no api reboot. It never touches the
// run lifecycle — Slack is strictly best-effort, so a Slack outage never affects
// runs. The connection state is readable via State() for the admin DTO.
type Manager struct {
	settings   SettingsSource
	poll       time.Duration
	backoffMin time.Duration
	backoffMax time.Duration
	dial       DialFunc
	logger     *slog.Logger

	mu    sync.RWMutex
	state string
}

// Config are the Manager knobs; every zero value falls back to a sensible
// default, so NewManager(src, Config{}) is a valid production configuration.
type Config struct {
	// Poll is the settings re-read cadence (and the hot-reload latency floor).
	Poll time.Duration
	// BackoffMin/BackoffMax bound the reconnect backoff after a hard failure.
	BackoffMin time.Duration
	BackoffMax time.Duration
	// Dial overrides the socket dialer (tests inject a fake); nil uses the real
	// socketmode implementation.
	Dial DialFunc
	// HTTPTimeout bounds the socketmode HTTP handshake (apps.connections.open) so
	// a slow/unreachable Slack cannot wedge the connect path; zero uses 15s. It
	// does NOT bound the long-lived websocket itself. Ignored when Dial is set.
	HTTPTimeout time.Duration
	// Logger receives redacted connection-state warnings; nil uses slog.Default.
	Logger *slog.Logger
}

// NewManager builds a Manager. It does not connect; call Run in a goroutine.
func NewManager(s SettingsSource, cfg Config) *Manager {
	m := &Manager{
		settings:   s,
		poll:       orDuration(cfg.Poll, 5*time.Second),
		backoffMin: orDuration(cfg.BackoffMin, time.Second),
		backoffMax: orDuration(cfg.BackoffMax, time.Minute),
		dial:       cfg.Dial,
		logger:     cfg.Logger,
		state:      StateDisabled,
	}
	if m.dial == nil {
		m.dial = newSocketDialer(orDuration(cfg.HTTPTimeout, 15*time.Second))
	}
	if m.logger == nil {
		m.logger = slog.Default()
	}
	return m
}

// State returns the current connection state for the admin settings DTO. Safe
// for concurrent reads from HTTP handlers while Run mutates it.
func (m *Manager) State() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *Manager) setState(s string) {
	m.mu.Lock()
	m.state = s
	m.mu.Unlock()
}

// Run is the supervisor loop; it returns when ctx is cancelled. Wire it into the
// server's background WaitGroup like the poller/sweeper.
func (m *Manager) Run(ctx context.Context) {
	backoff := m.backoffMin
	for {
		if ctx.Err() != nil {
			return
		}
		want, ok := m.want(ctx)
		if !ok {
			// Not configured: idle as a strict no-op, re-checking each poll.
			m.setState(StateDisabled)
			backoff = m.backoffMin
			if !sleepCtx(ctx, m.poll) {
				return
			}
			continue
		}

		m.setState(StateConnecting)
		connected, err := m.serve(ctx, want)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// Clean stop: the watcher cancelled the connection because the config
			// changed (or was disabled). Re-evaluate immediately, no backoff.
			backoff = m.backoffMin
			continue
		}
		// A genuine failure/drop. Surface a coarse class and back off before
		// retrying; a session that HAD connected retries promptly (reset backoff).
		m.setState(classifyState(err))
		m.logger.Warn("slack: socket connection ended", "error", Redact(err.Error()))
		if connected {
			backoff = m.backoffMin
		}
		if !sleepCtx(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, m.backoffMax)
	}
}

// want reads the effective connection target. Slack must be enabled AND both
// tokens present; anything else (including a settings read error) means "off",
// so a transient DB blip idles rather than crashes.
func (m *Manager) want(ctx context.Context) (desired, bool) {
	enabled, err := m.settings.SlackEnabled(ctx)
	if err != nil || !enabled {
		return desired{}, false
	}
	bot, err := m.settings.SlackBotToken(ctx)
	if err != nil || bot == "" {
		return desired{}, false
	}
	app, err := m.settings.SlackAppToken(ctx)
	if err != nil || app == "" {
		return desired{}, false
	}
	return desired{bot: bot, app: app}, true
}

// serve runs one connection with the given tokens, cancelling it when a watcher
// detects the desired config changed. It returns whether the session ever became
// live (so the caller can reset backoff after a stable drop) and the dial error
// (nil when the watcher/parent cancelled — a hot restart, not a failure).
func (m *Manager) serve(ctx context.Context, want desired) (bool, error) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Watch settings; a token/enable change cancels this connection so Run
	// re-evaluates and reconnects with the new config — hot restart, no reboot.
	go m.watch(connCtx, cancel, want)

	// connected is atomic: the production dialer calls onConnected on this
	// goroutine, but a test fake may call it from another, so the flag must be
	// safe either way.
	var connected atomic.Bool
	onConnecting := func() { m.setState(StateConnecting) }
	onConnected := func() {
		connected.Store(true)
		m.setState(StateConnected)
	}
	err := m.dial(connCtx, want.bot, want.app, onConnecting, onConnected)
	return connected.Load(), err
}

// watch cancels the connection when the desired config no longer matches the
// running pair (tokens changed or Slack disabled), polling on the same cadence
// as Run's idle re-check.
func (m *Manager) watch(ctx context.Context, cancel context.CancelFunc, running desired) {
	t := time.NewTicker(m.poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if want, ok := m.want(ctx); !ok || want != running {
				cancel()
				return
			}
		}
	}
}

// classifyState maps a dial error to a coarse connection state. It inspects only
// well-known Slack auth markers; the raw error text is never surfaced (it can
// carry a token or the wss ticket). Anything unrecognized is a connection error.
func classifyState(err error) string {
	if err == nil {
		return StateConnected
	}
	e := strings.ToLower(err.Error())
	for _, marker := range []string{"invalid_auth", "not_authed", "account_inactive", "token_revoked", "no_permission"} {
		if strings.Contains(e, marker) {
			return StateErrorAuth
		}
	}
	return StateErrorConnection
}

func nextBackoff(cur, max time.Duration) time.Duration {
	n := cur * 2
	if n > max {
		return max
	}
	return n
}

// sleepCtx sleeps for d, returning false if ctx is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func orDuration(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
