package healthsvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// errBoom is a sentinel for injecting a settings/store read failure into the fakes.
var errBoom = errors.New("boom")

// M6 danger-notice fan-out tests: pure decision logic with a fake claim, fake admin lister,
// fake notifier, fake enablement gate and an injected clock — NO database. The claim query's
// atomicity is proven for real in the store package (TestClaimHealthEpisodeNoticeAtomicLiveDB,
// the custody precedent). These prove: no notice on the opener tick; exactly one per admin on
// the next still-danger tick (idempotent on replays); a fresh notice after a re-arm; nothing
// for warn/unknown; nothing when the gate is off; and a server-authored notice body.

// mutEvaluator is a settable evaluator so a single reconciler can be driven across ticks with
// different overall statuses / check sets (fakeEvaluator is immutable).
type mutEvaluator struct {
	doc Doc
	err error
}

func (m *mutEvaluator) Evaluate(context.Context) (Doc, error) { return m.doc, m.err }

func dangerCheckDTO(id, title, summary string) apitypes.HealthCheckDTO {
	c := (&Service{}).base(id)
	c.Title, c.Severity, c.Summary = title, sevDanger, summary
	return c
}

func warnCheckDTO(id, title, summary string) apitypes.HealthCheckDTO {
	c := dangerCheckDTO(id, title, summary)
	c.Severity = sevWarn
	return c
}

func newNoticeReconciler(ev *mutEvaluator, st *fakeEpisodeStore, nf *fakeEpisodeNotifier, se *fakeEpisodeSettings) *EpisodeReconciler {
	r := NewEpisodeReconciler(ev, st, nf, se, nil)
	r.now = func() time.Time { return fixedNow }
	return r
}

// (a) The OPENER tick sends NO notice — the debounce. Danger with no episode open opens one
// and returns without claiming or notifying anyone.
func TestEpisodeNotice_OpenerTickNoNotice(t *testing.T) {
	a1 := uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep, admins: []uuid.UUID{a1}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")})}

	newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true}).Reconcile(context.Background())

	if st.opened != 1 {
		t.Fatalf("opened = %d, want 1 (the opener tick opens the episode)", st.opened)
	}
	if len(st.claims) != 0 {
		t.Fatalf("claims = %d, want 0 (the opener tick must not claim)", len(st.claims))
	}
	if len(nf.sent) != 0 {
		t.Fatalf("notices sent = %d, want 0 (the opener tick sends NO notice — the debounce)", len(nf.sent))
	}
}

// (b) Exactly one notice per admin on the NEXT still-danger tick, and no duplicate on further
// still-danger ticks (the claim is idempotent).
func TestEpisodeNotice_ExactlyOncePerAdmin(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep, admins: []uuid.UUID{a1, a2}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})

	r.Reconcile(context.Background()) // tick 1: opens, no notice
	if len(nf.sent) != 0 {
		t.Fatalf("after opener tick: notices = %d, want 0", len(nf.sent))
	}

	r.Reconcile(context.Background()) // tick 2: episode already open ⇒ fan out
	if len(nf.sent) != 2 {
		t.Fatalf("after first still-danger tick: notices = %d, want 2 (one per admin)", len(nf.sent))
	}
	got := map[uuid.UUID]bool{}
	for _, n := range nf.sent {
		got[n.UserID] = true
	}
	if !got[a1] || !got[a2] {
		t.Fatalf("notices went to %v, want exactly the two admins %s / %s", got, a1, a2)
	}

	r.Reconcile(context.Background()) // tick 3: still danger, still open ⇒ claim no-ops, no new notice
	r.Reconcile(context.Background()) // tick 4: same
	if len(nf.sent) != 2 {
		t.Fatalf("after repeated still-danger ticks: notices = %d, want 2 (the claim is idempotent)", len(nf.sent))
	}
}

// (c) A fresh notice after the episode CLOSES and a new one opens (the re-arm): the second
// episode's claim key is distinct, so each admin is notified afresh.
func TestEpisodeNotice_FreshNoticeAfterReArm(t *testing.T) {
	a1 := uuid.New()
	ep1, ep2 := uuid.New(), uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep1, admins: []uuid.UUID{a1}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})

	r.Reconcile(context.Background()) // opens ep1
	r.Reconcile(context.Background()) // notify a1 for ep1
	if len(nf.sent) != 1 {
		t.Fatalf("after ep1 notify: notices = %d, want 1", len(nf.sent))
	}

	// Recover: a not-danger tick closes ep1 (the re-arm).
	ev.doc = Doc{Status: sevOK}
	r.Reconcile(context.Background())
	if len(st.closed) != 1 || st.closed[0] != ep1 {
		t.Fatalf("closed = %v, want [ep1 %s]", st.closed, ep1)
	}

	// A new danger opens ep2 (distinct id) and notifies afresh.
	st.openReturn = ep2
	ev.doc = noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")})
	r.Reconcile(context.Background()) // opens ep2, no notice (opener)
	if len(nf.sent) != 1 {
		t.Fatalf("after ep2 opener tick: notices = %d, want still 1 (opener sends none)", len(nf.sent))
	}
	r.Reconcile(context.Background()) // notify a1 for ep2
	if len(nf.sent) != 2 {
		t.Fatalf("after ep2 notify: notices = %d, want 2 (a fresh notice for the re-armed episode)", len(nf.sent))
	}
	if nf.sent[1].UserID != a1 {
		t.Fatalf("ep2 notice went to %s, want the admin %s", nf.sent[1].UserID, a1)
	}
}

// (d) warn and unknown NEVER notify (and never open an episode): only danger drives the fan-out.
func TestEpisodeNotice_NoNoticeForWarnOrUnknown(t *testing.T) {
	for _, status := range []string{sevWarn, sevUnknown, sevOK, sevNA} {
		t.Run(status, func(t *testing.T) {
			a1 := uuid.New()
			st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: []uuid.UUID{a1}}
			nf := &fakeEpisodeNotifier{}
			ev := &mutEvaluator{doc: noticeDoc(status, []apitypes.HealthCheckDTO{warnCheckDTO("queue.waiting", "Runs waiting", "oldest 12m")})}
			r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})

			r.Reconcile(context.Background())
			r.Reconcile(context.Background())
			if st.opened != 0 {
				t.Fatalf("status %q opened %d episodes, want 0", status, st.opened)
			}
			if len(nf.sent) != 0 {
				t.Fatalf("status %q sent %d notices, want 0 (only danger notifies)", status, len(nf.sent))
			}
		})
	}
}

// (e) NOTHING when the enablement gate is OFF: the episode still opens (so the banner/snooze
// work), but no notice fans out. A gate READ ERROR suppresses the notice too.
func TestEpisodeNotice_GateOffSuppressesNotice(t *testing.T) {
	t.Run("gate off", func(t *testing.T) {
		a1 := uuid.New()
		ep := uuid.New()
		st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep, admins: []uuid.UUID{a1}}
		nf := &fakeEpisodeNotifier{}
		ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")})}
		r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: false})

		r.Reconcile(context.Background()) // opens
		r.Reconcile(context.Background()) // still danger, open, but gate off ⇒ no notice
		if st.opened != 1 {
			t.Fatalf("opened = %d, want 1 (the episode opens regardless of the notice gate)", st.opened)
		}
		if len(st.claims) != 0 {
			t.Fatalf("claims = %d, want 0 (gate off ⇒ no claim, no notice)", len(st.claims))
		}
		if len(nf.sent) != 0 {
			t.Fatalf("notices = %d, want 0 (the notice gate is off)", len(nf.sent))
		}
	})

	t.Run("gate read error", func(t *testing.T) {
		a1 := uuid.New()
		ep := uuid.New()
		st := &fakeEpisodeStore{open: store.GetOpenHealthEpisodeRow{ID: ep}, admins: []uuid.UUID{a1}}
		nf := &fakeEpisodeNotifier{}
		ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")})}
		r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true, enabledErr: errBoom})

		r.Reconcile(context.Background()) // episode already open, gate read fails ⇒ no notice
		if len(nf.sent) != 0 {
			t.Fatalf("notices = %d, want 0 (a gate read error must suppress the notice, never notify blind)", len(nf.sent))
		}
	})
}

// (f) The notice title/body is composed ONLY from server-authored text + the danger checks'
// already-sanitized titles/summaries — no warn/ok check text, no raw free text.
func TestEpisodeNotice_ServerAuthoredBody(t *testing.T) {
	a1 := uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{open: store.GetOpenHealthEpisodeRow{ID: ep}, admins: []uuid.UUID{a1}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{
		dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck rolling"),
		warnCheckDTO("queue.waiting", "Runs waiting for a worker", "oldest waiting 12m"),
		dangerCheckDTO("controller.report", "Controller reporting", "not reported for 6m"),
	})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true, baseURL: "https://uzi.example.com/"})

	r.Reconcile(context.Background()) // episode already open ⇒ fan out
	if len(nf.sent) != 1 {
		t.Fatalf("notices = %d, want 1", len(nf.sent))
	}
	n := nf.sent[0]

	if n.Kind != KindHealthEpisode {
		t.Errorf("kind = %q, want %q", n.Kind, KindHealthEpisode)
	}
	payload, ok := n.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", n.Payload)
	}
	if payload["title"] != healthEpisodeTitle {
		t.Errorf("payload title = %q, want the fixed server template %q", payload["title"], healthEpisodeTitle)
	}
	body, _ := payload["body"].(string)
	if !strings.HasPrefix(body, healthEpisodeBody) {
		t.Errorf("body does not begin with the fixed server template; got %q", body)
	}
	// The two DANGER checks' server-authored titles + summaries are present...
	for _, want := range []string{"Worker image roll", "4 of 4 workers stuck rolling", "Controller reporting", "not reported for 6m"} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing danger-check text %q; got %q", want, body)
		}
	}
	// ...and the WARN check's text is NOT (only danger checks are listed).
	for _, absent := range []string{"Runs waiting for a worker", "oldest waiting 12m"} {
		if strings.Contains(body, absent) {
			t.Errorf("body contains warn-check text %q; only danger checks may appear", absent)
		}
	}

	if n.Slack == nil {
		t.Fatal("Slack render is nil; a linked admin gets no DM")
	}
	// The check titles/summaries ride in the Slack BODY (the notifier mrkdwn-escapes it), NOT in
	// the Facts. The Slack body must contain the two DANGER checks' server-authored text...
	for _, want := range []string{"Worker image roll", "4 of 4 workers stuck rolling", "Controller reporting", "not reported for 6m"} {
		if !strings.Contains(n.Slack.Body, want) {
			t.Errorf("Slack body missing danger-check text %q; got %q", want, n.Slack.Body)
		}
	}
	// ...and NOT the WARN check's text (only danger checks are listed).
	for _, absent := range []string{"Runs waiting for a worker", "oldest waiting 12m"} {
		if strings.Contains(n.Slack.Body, absent) {
			t.Errorf("Slack body contains warn-check text %q; only danger checks may appear", absent)
		}
	}
	// Facts carry ONLY the CLOSED server count now (never the untrusted-carrying check text) —
	// one fact reading the number of danger checks (2 here), matching custody_episode.
	if len(n.Slack.Facts) != 1 {
		t.Fatalf("Slack facts = %d, want 1 (a single CLOSED danger-check count)", len(n.Slack.Facts))
	}
	if !strings.Contains(n.Slack.Facts[0], "2") {
		t.Errorf("Slack count fact = %q, want it to report the 2 danger checks", n.Slack.Facts[0])
	}
	for _, leaked := range []string{"Worker image roll", "Controller reporting"} {
		if strings.Contains(n.Slack.Facts[0], leaked) {
			t.Errorf("Slack fact %q leaks check text %q; Facts must be CLOSED server values only", n.Slack.Facts[0], leaked)
		}
	}
	if n.Slack.Link != "https://uzi.example.com/admin/health" {
		t.Errorf("Slack link = %q, want the server-built /admin/health deep link", n.Slack.Link)
	}
	if n.UserID != a1 {
		t.Errorf("notice UserID = %s, want the admin %s", n.UserID, a1)
	}
}

// (g) SECURITY (M6 audit): an owner-supplied worker name carrying Slack markup that reaches a
// danger-check summary must render INERT in the Slack DM — no live <url|text> phishing link, no
// bold. Worker names are owner-supplied and termsafe.Validate permits these printable ASCII markup
// chars; safe() strips control/bidi but NOT Slack markup. The fix routes the check text through
// the notice BODY (which the notifier renders via SlackMrkdwn — the injection-safe &<>-escaper)
// and keeps the Facts slot (scrubbed but NOT mrkdwn-escaped) to CLOSED server values only. This
// asserts the untrusted text left the Facts slot AND that the real SlackMrkdwn render of the body
// neutralizes the phishing link. It FAILS against the pre-fix code, which put the raw summary in a
// Fact and left the Slack body free of it.
func TestEpisodeNotice_SlackMarkupInSummaryIsInert(t *testing.T) {
	a1 := uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{open: store.GetOpenHealthEpisodeRow{ID: ep}, admins: []uuid.UUID{a1}}
	nf := &fakeEpisodeNotifier{}
	// A worker an owner named with Slack markup flows into the check summary (PRD D7): a live-link
	// attempt, bold, and a backtick code chip.
	const evil = "worker <https://evil.example|click> *urgent* `code`"
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{
		dangerCheckDTO("fleet.roll", "Worker image roll", evil+" stuck"),
	})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})

	r.Reconcile(context.Background()) // episode already open ⇒ fan out
	if len(nf.sent) != 1 {
		t.Fatalf("notices = %d, want 1", len(nf.sent))
	}
	slackR := nf.sent[0].Slack
	if slackR == nil {
		t.Fatal("Slack render is nil; a linked admin gets no DM")
	}

	// The untrusted-carrying check text must NOT sit in a Fact: the notifier scrubs but does NOT
	// mrkdwn-escape Facts, so a raw <url|text> there would render as a live link. (Pre-fix, the
	// fact was "*Worker image roll*: worker <https://evil.example|click> …" — this catches a revert.)
	for _, f := range slackR.Facts {
		if strings.Contains(f, "evil.example") {
			t.Fatalf("Slack Fact carries the untrusted summary %q; check text must ride in Body, not the un-escaped Facts", f)
		}
	}

	// The check text rides in the Slack Body, so it reaches the notifier's SlackMrkdwn render — the
	// exact escaper the notifier applies to the body. Render it here and assert it is INERT.
	if !strings.Contains(slackR.Body, "evil.example") {
		t.Fatalf("Slack Body does not carry the danger-check summary at all; got %q", slackR.Body)
	}
	rendered := slacksvc.SlackMrkdwn(slackR.Body)
	// The hostname survives as inert text (not silently dropped)...
	if !strings.Contains(rendered, "evil.example") {
		t.Fatalf("SlackMrkdwn render dropped the summary entirely; got %q", rendered)
	}
	// ...but the live <url|text> link markup is neutralized: the raw <https://evil.example… must be
	// gone (SlackMrkdwn escapes the angle brackets / degrades the autolink to inert text), so no
	// clickable phishing link reaches the admin's DM.
	if strings.Contains(rendered, "<https://evil.example") {
		t.Errorf("SlackMrkdwn render kept a RAW <url|text> link (live phishing markup); want it neutralized. got %q", rendered)
	}
}

// M2 read counters distinguish skipping the notice from a gate hiding it.
type m2EpisodeStore struct {
	*fakeEpisodeStore
	adminReads int
}

func (s *m2EpisodeStore) ListAdmins(ctx context.Context) ([]uuid.UUID, error) {
	s.adminReads++
	return s.fakeEpisodeStore.ListAdmins(ctx)
}

type m2Settings struct {
	enabledReads, baseReads int
}

func (s *m2Settings) HealthEnabled(context.Context) (bool, error) {
	s.enabledReads++
	return true, nil
}
func (s *m2Settings) PublicBaseURL(context.Context) (string, error) {
	s.baseReads++
	return "https://uzi.example.com", nil
}

func TestEpisodeNoticeM2Allowed(t *testing.T) {
	for _, id := range []string{"db", "controller.report", "loops", "fleet.roll"} {
		t.Run(id, func(t *testing.T) {
			a, b := uuid.New(), uuid.New()
			st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: []uuid.UUID{a, b}}
			nf := &fakeEpisodeNotifier{}
			ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO(id, id, "danger")})}
			r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})
			r.Reconcile(context.Background())
			if len(st.claims) != 0 || len(nf.sent) != 0 {
				t.Fatal("opener notified")
			}
			r.Reconcile(context.Background())
			r.Reconcile(context.Background())
			if countDeliveredTo(nf, a) != 1 || countDeliveredTo(nf, b) != 1 || len(nf.sent) != 2 {
				t.Fatalf("notices = %+v, want once per admin", nf.sent)
			}
			for _, n := range nf.sent {
				if got := n.Payload.(map[string]any)["checks"]; !reflect.DeepEqual(got, []dangerCheck{{ID: id, Title: id, Summary: "danger"}}) {
					t.Fatalf("checks = %+v", got)
				}
			}
		})
	}
}

func TestEpisodeNoticeM2ExcludedAndSeverities(t *testing.T) {
	for _, id := range []string{"queue.waiting", "fleet.capacity", "queue.undispatched", "fleet.disk", "fleet.rundisk", "forge.sync", "forge.ciwatch", "slack.socket", "schedules.paused", "board.drift", "custody.holds", "release.check", "future.check"} {
		for _, severity := range []string{sevWarn, sevUnknown, sevOK} {
			t.Run(id+"/"+severity, func(t *testing.T) {
				st := &m2EpisodeStore{fakeEpisodeStore: &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: []uuid.UUID{uuid.New()}}}
				se, nf := &m2Settings{}, &fakeEpisodeNotifier{}
				checks := []apitypes.HealthCheckDTO{dangerCheckDTO(id, "owner danger", "excluded")}
				for _, allowed := range []string{"db", "controller.report", "loops", "fleet.roll"} {
					c := dangerCheckDTO(allowed, allowed, "not danger")
					c.Severity = severity
					checks = append(checks, c)
				}
				ev := &mutEvaluator{doc: noticeDoc(sevDanger, checks)}
				r := NewEpisodeReconciler(ev, st, nf, se, nil)
				for range 3 {
					r.Reconcile(context.Background())
				}
				if st.opened != 0 || len(st.closed) != 0 {
					t.Fatal("owner danger opened an episode")
				}
				if se.enabledReads != 0 || se.baseReads != 0 || st.adminReads != 0 || len(st.claims) != 0 || len(nf.sent) != 0 {
					t.Fatalf("reads enabled/base/admin=%d/%d/%d claims=%d notices=%d", se.enabledReads, se.baseReads, st.adminReads, len(st.claims), len(nf.sent))
				}
			})
		}
	}
}

func TestEpisodeNoticeM2MixedPayload(t *testing.T) {
	for _, severity := range []string{sevWarn, sevUnknown, sevOK} {
		t.Run(severity, func(t *testing.T) {
			st := &fakeEpisodeStore{open: store.GetOpenHealthEpisodeRow{ID: uuid.New()}, admins: []uuid.UUID{uuid.New()}}
			nf := &fakeEpisodeNotifier{}
			checks := []apitypes.HealthCheckDTO{
				dangerCheckDTO("queue.waiting", "owner waiting", "owner-only waiting"),
				dangerCheckDTO("fleet.roll", "roll title", "roll summary"),
				{ID: "db", Scope: "instance", Title: "db nondanger", Summary: "db-only nondanger", Severity: severity},
				dangerCheckDTO("fleet.disk", "owner disk", "owner-only disk"),
				dangerCheckDTO("loops", "loops title", "loops summary"),
				dangerCheckDTO("controller.report", "controller title", "controller summary"),
			}
			ev := &mutEvaluator{doc: noticeDoc(sevDanger, checks)}
			newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true}).Reconcile(context.Background())
			if len(nf.sent) != 1 {
				t.Fatalf("notices=%d", len(nf.sent))
			}
			n := nf.sent[0]
			want := []dangerCheck{{"fleet.roll", "roll title", "roll summary"}, {"loops", "loops title", "loops summary"}, {"controller.report", "controller title", "controller summary"}}
			p := n.Payload.(map[string]any)
			if !reflect.DeepEqual(p["checks"], want) {
				t.Fatalf("checks=%+v want %+v", p["checks"], want)
			}
			body := p["body"].(string)
			if body != n.Slack.Body {
				t.Fatal("payload and Slack body differ")
			}
			last := -1
			for _, c := range want {
				pos := strings.Index(body, c.Title+": "+c.Summary)
				if pos <= last {
					t.Fatalf("body order/content=%q", body)
				}
				last = pos
			}
			for _, absent := range []string{"owner waiting", "owner disk", "db nondanger", "owner-only", "db-only"} {
				if strings.Contains(body, absent) {
					t.Fatalf("excluded text in body: %q", body)
				}
			}
			if !reflect.DeepEqual(n.Slack.Facts, []string{"*3* danger checks"}) {
				t.Fatalf("facts=%v", n.Slack.Facts)
			}
		})
	}
}

func TestEpisodeNoticeInstanceOwnerRearm(t *testing.T) {
	admin, first, second := uuid.New(), uuid.New(), uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: first, admins: []uuid.UUID{admin}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("db", "db", "unavailable")})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})
	ctx := context.Background()
	r.Reconcile(ctx)
	if st.open.ID != first || len(st.claims) != 0 || len(nf.sent) != 0 {
		t.Fatal("first opener claimed or notified")
	}
	r.Reconcile(ctx)
	if len(st.claims) != 1 || countDeliveredTo(nf, admin) != 1 {
		t.Fatal("first episode did not notify")
	}
	snooze := fixedNow.Add(time.Hour).Format(time.RFC3339)
	ev.doc = noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("queue.waiting", "owner", "waiting")})
	ev.doc.EpisodeID, ev.doc.SnoozedUntil = strPtr(first.String()), &snooze
	r.Reconcile(ctx)
	r.Reconcile(ctx)
	if !reflect.DeepEqual(st.closed, []uuid.UUID{first}) || st.open.ID != uuid.Nil || st.opened != 1 || len(st.claims) != 1 || len(nf.sent) != 1 {
		t.Fatal("owner-only danger did not close without claiming/notifying")
	}
	st.openReturn = second
	ev.doc = noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "roll", "stuck")})
	r.Reconcile(ctx)
	if st.open.ID != second || st.opened != 2 || len(st.claims) != 1 || len(nf.sent) != 1 {
		t.Fatal("fresh instance opener inherited notice state")
	}
	r.Reconcile(ctx)
	if len(st.claims) != 2 || st.claims[1].EpisodeID != second || countDeliveredTo(nf, admin) != 2 || nf.sent[1].Payload.(map[string]any)["episode_id"] != second.String() {
		t.Fatal("fresh episode did not notify on second tick")
	}
}

// These spies prove persistence and PublishNotification calls, not queue drain or link resolution.
type m2NotificationStore struct {
	notifysvc.Store
	rows   []store.InsertNotificationParams
	pruned []uuid.UUID
}

func (s *m2NotificationStore) InsertNotification(_ context.Context, p store.InsertNotificationParams) (store.Notification, error) {
	s.rows = append(s.rows, p)
	return store.Notification{ID: uuid.New(), UserID: p.UserID, Kind: p.Kind, Payload: p.Payload}, nil
}
func (s *m2NotificationStore) PruneNotificationsForUser(_ context.Context, p store.PruneNotificationsForUserParams) (int64, error) {
	s.pruned = append(s.pruned, p.UserID)
	return 0, nil
}

type m2Slacker struct {
	t       *testing.T
	store   *m2NotificationStore
	users   []uuid.UUID
	renders []notifysvc.SlackRender
}

func (s *m2Slacker) PublishNotification(uid uuid.UUID, r notifysvc.SlackRender) {
	if len(s.store.rows) != len(s.users)+1 || s.store.rows[len(s.users)].UserID != uid {
		s.t.Fatal("publish before matching persistence")
	}
	s.users = append(s.users, uid)
	s.renders = append(s.renders, r)
}
func TestEpisodeNoticeM2RealNotifyAddressing(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	for _, membership := range []struct {
		name   string
		admins []uuid.UUID
	}{{"none", nil}, {"A", []uuid.UUID{a}}, {"B", []uuid.UUID{b}}, {"both", []uuid.UUID{a, b}}} {
		for _, id := range []string{"db", "controller.report", "loops", "fleet.roll", "queue.waiting", "fleet.capacity"} {
			// Owner labels are informational: these health checks aggregate owners and carry no owner UUID.
			owners := []string{""}
			ownerOnly := id == "queue.waiting" || id == "fleet.capacity"
			if ownerOnly {
				owners = []string{"A", "B"}
			}
			for _, owner := range owners {
				t.Run(membership.name+"/"+id+"/owner"+owner, func(t *testing.T) {
					ns := &m2NotificationStore{}
					sl := &m2Slacker{t: t, store: ns}
					st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: uuid.New(), admins: membership.admins}
					ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO(id, id, "danger")})}
					r := NewEpisodeReconciler(ev, st, notifysvc.New(ns, sl, 0, nil), &fakeEpisodeSettings{enabled: true}, nil)
					r.Reconcile(context.Background())
					if len(ns.rows) != 0 || len(sl.users) != 0 || len(ns.pruned) != 0 {
						t.Fatal("opener persisted/published/pruned")
					}
					r.Reconcile(context.Background())
					r.Reconcile(context.Background())
					want := membership.admins
					if ownerOnly {
						want = nil
					}
					if len(ns.rows) != len(want) || !reflect.DeepEqual(sl.users, want) || !reflect.DeepEqual(ns.pruned, want) {
						t.Fatalf("rows=%v publish=%v prune=%v want admins=%v", ns.rows, sl.users, ns.pruned, want)
					}
					for i, uid := range want {
						row := ns.rows[i]
						if row.UserID != uid || row.Kind != KindHealthEpisode || row.RunID.Valid || row.ReviewID.Valid {
							t.Fatalf("row=%+v", row)
						}
						var payload struct {
							Checks    []dangerCheck `json:"checks"`
							Body      string        `json:"body"`
							EpisodeID string        `json:"episode_id"`
						}
						if err := json.Unmarshal(row.Payload, &payload); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(payload.Checks, []dangerCheck{{id, id, "danger"}}) || payload.Body != sl.renders[i].Body || payload.EpisodeID != st.open.ID.String() {
							t.Fatalf("payload=%+v", payload)
						}
						if sl.renders[i].DeliveryID != uuid.Nil {
							t.Fatal("unexpected durable delivery")
						}
					}
				})
			}
		}
	}
}

// countDeliveredTo counts the successful sends addressed to uid.
func countDeliveredTo(nf *fakeEpisodeNotifier, uid uuid.UUID) int {
	n := 0
	for _, d := range nf.delivered {
		if d.UserID == uid {
			n++
		}
	}
	return n
}

// (issue #1499) A failed Notify releases the claimed slot, so the next still-danger tick
// re-claims it and retries: the admin is notified exactly once after the failure clears, and
// the admin whose send succeeded is never re-notified.
func TestEpisodeNotice_FailedNotifyIsRetried(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep, admins: []uuid.UUID{a1, a2}}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})
	ctx := context.Background()

	r.Reconcile(ctx) // tick 1: opens the episode, no notice (debounce)
	if len(nf.sent) != 0 {
		t.Fatalf("tick 1 sent = %d, want 0 (opener tick)", len(nf.sent))
	}

	// Tick 2: a1's send fails, a2's succeeds.
	nf.errFor = map[uuid.UUID]error{a1: errBoom}
	r.Reconcile(ctx)
	if len(st.releases) != 1 {
		t.Fatalf("tick 2 releases = %d, want 1 (only the failed send is released)", len(st.releases))
	}
	if got := st.releases[0]; got.EpisodeID != ep || got.UserID != a1 {
		t.Fatalf("tick 2 released %+v, want (episode %s, user %s)", got, ep, a1)
	}
	if got := countDeliveredTo(nf, a1); got != 0 {
		t.Fatalf("tick 2 a1 delivered = %d, want 0 (its send failed)", got)
	}
	if got := countDeliveredTo(nf, a2); got != 1 {
		t.Fatalf("tick 2 a2 delivered = %d, want 1", got)
	}

	// Tick 3: the failure clears; a1 is re-claimed and notified exactly once, a2 is not re-sent.
	nf.errFor = nil
	r.Reconcile(ctx)
	if got := countDeliveredTo(nf, a1); got != 1 {
		t.Fatalf("tick 3 a1 delivered = %d, want 1 (the released slot is retried)", got)
	}
	if got := countDeliveredTo(nf, a2); got != 1 {
		t.Fatalf("tick 3 a2 delivered = %d, want 1 (no re-notify of a delivered admin)", got)
	}

	// Tick 4: both slots stay claimed; no further deliveries or releases.
	r.Reconcile(ctx)
	if got := countDeliveredTo(nf, a1); got != 1 {
		t.Fatalf("tick 4 a1 delivered = %d, want 1 (exactly once)", got)
	}
	if got := countDeliveredTo(nf, a2); got != 1 {
		t.Fatalf("tick 4 a2 delivered = %d, want 1 (exactly once)", got)
	}
	if len(nf.delivered) != 2 {
		t.Fatalf("total delivered = %d, want 2", len(nf.delivered))
	}
	if len(st.releases) != 1 {
		t.Fatalf("total releases = %d, want 1", len(st.releases))
	}
}

// (issue #1499) A failing release is tolerated: it is logged, never panics or aborts the
// fan-out, and the slot stays claimed, so that admin is not retried (suppressed, as logged).
func TestEpisodeNotice_FailedReleaseIsTolerated(t *testing.T) {
	a1, a2 := uuid.New(), uuid.New()
	ep := uuid.New()
	st := &fakeEpisodeStore{openErr: pgx.ErrNoRows, openReturn: ep, admins: []uuid.UUID{a1, a2}, releaseErr: errBoom}
	nf := &fakeEpisodeNotifier{}
	ev := &mutEvaluator{doc: noticeDoc(sevDanger, []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")})}
	r := newNoticeReconciler(ev, st, nf, &fakeEpisodeSettings{enabled: true})
	ctx := context.Background()

	r.Reconcile(ctx) // opener
	nf.errFor = map[uuid.UUID]error{a1: errBoom}
	r.Reconcile(ctx) // a1 fails, release fails; a2 still delivered
	if len(st.releases) != 1 {
		t.Fatalf("releases = %d, want 1 (the failed send's release was attempted)", len(st.releases))
	}
	if got := countDeliveredTo(nf, a2); got != 1 {
		t.Fatalf("a2 delivered = %d, want 1 (a failed release must not abort the fan-out)", got)
	}

	nf.errFor = nil
	r.Reconcile(ctx) // a1's slot is still claimed: no retry
	if got := countDeliveredTo(nf, a1); got != 0 {
		t.Fatalf("a1 delivered = %d, want 0 (the unreleased slot stays claimed)", got)
	}
	if len(st.releases) != 1 {
		t.Fatalf("releases = %d, want 1 (no further release attempts)", len(st.releases))
	}
}

// noticeDoc supplies internally consistent test documents without changing evaluator behavior.
func noticeDoc(status string, checks []apitypes.HealthCheckDTO) Doc {
	d := Doc{Status: status, Counts: tally(checks), Checks: checks}
	for _, c := range checks {
		if c.Scope == "instance" && c.Severity == sevDanger {
			d.Blocking = true
		}
	}
	return d
}
