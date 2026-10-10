package healthsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/dbdiskfull"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
)

// The emergency path is exercised end to end through the REAL slacksvc.Notifier: the
// reconciler publishes, the notifier's drain resolves the Slack target and posts. Only the
// store and the Slack poster are fakes.

// emergencyNotifierStore implements the two NotifierStore methods the notification path
// calls; the embedded nil interface panics if the notifier ever reaches for another one.
type emergencyNotifierStore struct {
	slacksvc.NotifierStore

	mu        sync.Mutex
	optedOut  map[uuid.UUID]bool
	lookups   []uuid.UUID
	delivered []uuid.UUID
}

func (s *emergencyNotifierStore) GetSlackDeliveryForUser(_ context.Context, id uuid.UUID) (pgtype.Text, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups = append(s.lookups, id)
	if s.optedOut[id] {
		return pgtype.Text{}, pgx.ErrNoRows
	}
	return pgtype.Text{String: "U-" + id.String(), Valid: true}, nil
}

func (s *emergencyNotifierStore) MarkNotificationSlackDelivered(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delivered = append(s.delivered, id)
	return nil
}

type emergencyPost struct {
	channel  string
	fallback string
	blocks   string
}

type emergencyPoster struct {
	slacksvc.Poster

	mu    sync.Mutex
	posts []emergencyPost
}

func (p *emergencyPoster) OpenDM(_ context.Context, slackUserID string) (string, error) {
	return "D-" + slackUserID, nil
}

func (p *emergencyPoster) PostBlocks(_ context.Context, channel, _, fallback string, blocks []slack.Block) (string, error) {
	raw, _ := json.Marshal(blocks)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, emergencyPost{channel: channel, fallback: fallback, blocks: string(raw)})
	return "1.1", nil
}

func (p *emergencyPoster) postsTo(uid uuid.UUID) []emergencyPost {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []emergencyPost
	for _, po := range p.posts {
		if po.channel == "D-U-"+uid.String() {
			out = append(out, po)
		}
	}
	return out
}

type emergencyHarness struct {
	t      *testing.T
	clock  *time.Time
	sig    *dbdiskfull.Signal
	st     *fakeEpisodeStore
	nf     *fakeEpisodeNotifier
	se     *fakeEpisodeSettings
	ev     *mutEvaluator
	nstore *emergencyNotifierStore
	poster *emergencyPoster
	notif  *slacksvc.Notifier
	rec    *EpisodeReconciler
	admins []uuid.UUID
}

func pg53100() error {
	return fmt.Errorf("open health episode: %w", &pgconn.PgError{Code: dbdiskfull.CodeDiskFull, Message: "could not extend file: SECRET-RAW-TEXT"})
}

func newEmergencyHarness(t *testing.T, admins ...uuid.UUID) *emergencyHarness {
	t.Helper()
	clock := fixedNow
	h := &emergencyHarness{t: t, clock: &clock, admins: admins}
	h.sig = dbdiskfull.New(func() time.Time { return *h.clock })
	h.st = &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: admins}
	h.nf = &fakeEpisodeNotifier{}
	h.se = &fakeEpisodeSettings{enabled: true, baseURL: "https://uzi.example"}
	h.ev = &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{
		dangerCheckDTO("db", "Database", emergencyDiskFullLine),
	})}
	h.nstore = &emergencyNotifierStore{optedOut: map[uuid.UUID]bool{}}
	h.poster = &emergencyPoster{}
	h.notif = slacksvc.NewNotifier(h.nstore, h.poster,
		func(context.Context) (string, error) { return "", nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.notif.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	h.rec = NewEpisodeReconciler(h.ev, h.st, h.nf, h.se, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithEmergencySlack(h.notif, h.sig)
	h.rec.now = func() time.Time { return *h.clock }
	return h
}

// drain waits until the notifier has processed everything published so far: the queue is
// FIFO, so once a sentinel notification to an otherwise-unused user is posted, every earlier
// publish has been handled.
func (h *emergencyHarness) drain() {
	h.t.Helper()
	sentinel := uuid.New()
	h.notif.PublishNotification(sentinel, notifysvc.SlackRender{Title: "sentinel"})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.poster.postsTo(sentinel)) > 0 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatal("notifier did not drain within 5s")
}

func (h *emergencyHarness) posts(uid uuid.UUID) []emergencyPost {
	h.t.Helper()
	h.drain()
	return h.poster.postsTo(uid)
}

func (h *emergencyHarness) tick() { h.rec.Reconcile(context.Background()) }

func TestEmergency_OpenFails53100OnSecondConsecutiveTick(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	h := newEmergencyHarness(t, a1, a2)
	h.st.openRetErr = pg53100()

	h.tick()
	if n := len(h.posts(a1)) + len(h.posts(a2)); n != 0 {
		t.Fatalf("posts after the first failed open = %d, want 0 (debounce)", n)
	}
	h.tick()
	for _, a := range []uuid.UUID{a1, a2} {
		if got := h.posts(a); len(got) != 1 {
			t.Fatalf("posts to %s after the second failed open = %d, want 1", a, len(got))
		}
	}
	if len(h.nf.sent) != 0 || len(h.st.claims) != 0 {
		t.Fatalf("emergency path must not use the claim/persist path: sent=%d claims=%d", len(h.nf.sent), len(h.st.claims))
	}
}

func TestEmergency_StreakResets(t *testing.T) {
	a1 := uuid.New()
	for name, interrupt := range map[string]func(h *emergencyHarness){
		"non-blocking tick": func(h *emergencyHarness) {
			h.ev.doc = noticeDoc(sevOK, nil)
			h.tick()
			h.ev.doc = noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", emergencyDiskFullLine)})
		},
		"open fails with something else": func(h *emergencyHarness) {
			h.st.openRetErr = errors.New("connection reset")
			h.tick()
			h.st.openRetErr = pg53100()
		},
		"evaluate fails": func(h *emergencyHarness) {
			h.ev.err = errors.New("evaluate boom")
			h.tick()
			h.ev.err = nil
		},
		"open loses a unique race": func(h *emergencyHarness) {
			h.st.openRetErr = &pgconn.PgError{Code: "23505"}
			h.tick()
			h.st.openRetErr = pg53100()
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newEmergencyHarness(t, a1)
			h.st.openRetErr = pg53100()
			h.tick() // streak 1
			interrupt(h)
			h.tick() // streak 1 again, not 2
			if got := h.posts(a1); len(got) != 0 {
				t.Fatalf("posts = %d, want 0 (streak must restart)", len(got))
			}
			h.tick() // now 2 consecutive
			if got := h.posts(a1); len(got) != 1 {
				t.Fatalf("posts = %d, want 1", len(got))
			}
		})
	}
}

func TestEmergency_ClaimFails53100(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1)
	h.st.claimErr = pg53100()
	h.tick() // opener
	h.tick() // claim fails
	if got := h.posts(a1); len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}
}

func TestEmergency_NotifyFails53100(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1)
	h.nf.err = pg53100()
	h.tick()
	h.tick()
	if got := h.posts(a1); len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}
	if len(h.st.releases) != 1 {
		t.Fatalf("releases = %d, want 1 (the release attempt stays)", len(h.st.releases))
	}
}

func TestEmergency_OptedOutAdminGetsNothing(t *testing.T) {
	in, out := uuid.New(), uuid.New()
	h := newEmergencyHarness(t, in, out)
	h.nstore.optedOut[out] = true
	h.st.openRetErr = pg53100()
	h.tick()
	h.tick()
	if got := h.posts(in); len(got) != 1 {
		t.Fatalf("opted-in posts = %d, want 1", len(got))
	}
	if got := h.posts(out); len(got) != 0 {
		t.Fatalf("opted-out posts = %d, want 0", len(got))
	}
	h.nstore.mu.Lock()
	defer h.nstore.mu.Unlock()
	looked := false
	for _, id := range h.nstore.lookups {
		looked = looked || id == out
	}
	if !looked {
		t.Fatal("the opt-in lookup for the opted-out admin never ran")
	}
}

func TestEmergency_NoDeliveryIDMarked(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1)
	h.st.openRetErr = pg53100()
	h.tick()
	h.tick()
	if got := h.posts(a1); len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}
	h.nstore.mu.Lock()
	defer h.nstore.mu.Unlock()
	if len(h.nstore.delivered) != 0 {
		t.Fatalf("MarkNotificationSlackDelivered called %d times, want 0 (zero DeliveryID)", len(h.nstore.delivered))
	}
}

func TestEmergency_HealthDisabledSendsNothing(t *testing.T) {
	a1 := uuid.New()
	for name, setup := range map[string]func(h *emergencyHarness){
		"open":   func(h *emergencyHarness) { h.st.openRetErr = pg53100() },
		"claim":  func(h *emergencyHarness) { h.st.claimErr = pg53100() },
		"notify": func(h *emergencyHarness) { h.nf.err = pg53100() },
	} {
		t.Run(name, func(t *testing.T) {
			h := newEmergencyHarness(t, a1)
			h.se.enabled = false
			setup(h)
			h.tick()
			h.tick()
			h.tick()
			if got := h.posts(a1); len(got) != 0 {
				t.Fatalf("posts = %d, want 0 with health notifications disabled", len(got))
			}
		})
	}
}

func TestEmergency_CooldownAndGenerationRearm(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1)
	h.st.openRetErr = pg53100()
	h.tick()
	h.tick()
	if got := h.posts(a1); len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}

	// Same incident, still failing every minute inside the cooldown: no second DM.
	for i := 0; i < int(emergencyNoticeCooldown/time.Minute)-1; i++ {
		*h.clock = h.clock.Add(time.Minute)
		h.tick()
	}
	if got := h.posts(a1); len(got) != 1 {
		t.Fatalf("posts inside the cooldown = %d, want 1", len(got))
	}

	// Still the same generation (sightings never paused longer than the window) once the
	// cooldown expires: it is allowed again.
	gen0 := h.sig.Generation()
	*h.clock = h.clock.Add(time.Minute)
	h.tick()
	if h.sig.Generation() != gen0 {
		t.Fatalf("generation moved to %d inside one incident", h.sig.Generation())
	}
	if got := h.posts(a1); len(got) != 2 {
		t.Fatalf("posts after the cooldown = %d, want 2", len(got))
	}

	// A quiet gap longer than the signal window, then a fresh 53100: new generation re-arms
	// even though the cooldown (measured from the last send) has not elapsed.
	gen := h.sig.Generation()
	*h.clock = h.clock.Add(dbdiskfull.Window + time.Second)
	h.tick() // Observe sees 53100 after the gap: gen+1
	if h.sig.Generation() != gen+1 {
		t.Fatalf("generation = %d, want %d", h.sig.Generation(), gen+1)
	}
	if got := h.posts(a1); len(got) != 3 {
		t.Fatalf("posts after a new generation = %d, want 3", len(got))
	}
	h.rec.emergency.mu.Lock()
	for k := range h.rec.emergency.sent {
		if k.gen != h.sig.Generation() {
			t.Errorf("stale generation %d not pruned", k.gen)
		}
	}
	h.rec.emergency.mu.Unlock()
}

func TestEmergency_NonDiskFullErrorsSendNothing(t *testing.T) {
	a1 := uuid.New()
	for _, errv := range []error{
		&pgconn.PgError{Code: "53200"},
		fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: "53300"}),
		errors.New("generic"),
	} {
		for name, setup := range map[string]func(h *emergencyHarness){
			"open":   func(h *emergencyHarness) { h.st.openRetErr = errv },
			"claim":  func(h *emergencyHarness) { h.st.claimErr = errv },
			"notify": func(h *emergencyHarness) { h.nf.err = errv },
		} {
			t.Run(fmt.Sprintf("%s/%v", name, errv), func(t *testing.T) {
				h := newEmergencyHarness(t, a1)
				setup(h)
				h.tick()
				h.tick()
				h.tick()
				if got := h.posts(a1); len(got) != 0 {
					t.Fatalf("posts = %d, want 0", len(got))
				}
			})
		}
	}
}

func TestEmergency_PostedTextHasFixedLineAndNoRawError(t *testing.T) {
	a1 := uuid.New()
	// The evaluated doc lists no danger check at all: the fixed line must still be there.
	h := newEmergencyHarness(t, a1)
	h.ev.doc = Doc{Blocking: true}
	h.st.openRetErr = pg53100()
	h.tick()
	h.tick()
	got := h.posts(a1)
	if len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}
	for _, text := range []string{got[0].fallback, got[0].blocks} {
		if !strings.Contains(text, "Database writes are failing: disk full (53100).") {
			t.Errorf("posted text lacks the fixed disk-full line: %q", text)
		}
		if strings.Contains(text, "SECRET-RAW-TEXT") || strings.Contains(text, "could not extend") {
			t.Errorf("posted text leaks raw error text: %q", text)
		}
	}
	if strings.Count(got[0].blocks, "disk full (53100)") != 1 {
		t.Errorf("fixed line duplicated or missing in blocks: %q", got[0].blocks)
	}
}

func TestEmergency_NoDuplicateWhenDBDangerPresent(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1) // doc already carries the db disk-full summary
	h.st.openRetErr = pg53100()
	h.tick()
	h.tick()
	got := h.posts(a1)
	if len(got) != 1 {
		t.Fatalf("posts = %d, want 1", len(got))
	}
	if n := strings.Count(got[0].blocks, "disk full (53100)"); n != 1 {
		t.Errorf("disk-full line appears %d times, want 1", n)
	}
}

func TestEmergency_DisabledWithoutSlackOrSignal(t *testing.T) {
	a1 := uuid.New()
	h := newEmergencyHarness(t, a1)
	h.st.openRetErr = pg53100()
	h.rec.WithEmergencySlack(nil, h.sig)
	h.tick()
	h.tick()
	h.rec.WithEmergencySlack(h.notif, nil)
	h.tick()
	if got := h.posts(a1); len(got) != 0 {
		t.Fatalf("posts = %d, want 0 when the path is disabled", len(got))
	}
}
