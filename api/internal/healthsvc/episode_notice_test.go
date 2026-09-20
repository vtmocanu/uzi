package healthsvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
	return apitypes.HealthCheckDTO{ID: id, Group: groupWorkers, Title: title, Severity: sevDanger, Summary: summary}
}

func warnCheckDTO(id, title, summary string) apitypes.HealthCheckDTO {
	return apitypes.HealthCheckDTO{ID: id, Group: groupQueue, Title: title, Severity: sevWarn, Summary: summary}
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
	ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")}}}

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
	ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck")}}}
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
	ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")}}}
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
	ev.doc = Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")}}
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
			ev := &mutEvaluator{doc: Doc{Status: status, Checks: []apitypes.HealthCheckDTO{warnCheckDTO("queue.waiting", "Runs waiting", "oldest 12m")}}}
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
		ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")}}}
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
		ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{dangerCheckDTO("db", "Database", "ping fails")}}}
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
	ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{
		dangerCheckDTO("fleet.roll", "Worker image roll", "4 of 4 workers stuck rolling"),
		warnCheckDTO("queue.waiting", "Runs waiting for a worker", "oldest waiting 12m"),
		dangerCheckDTO("controller.report", "Controller reporting", "not reported for 6m"),
	}}}
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
	ev := &mutEvaluator{doc: Doc{Status: sevDanger, Checks: []apitypes.HealthCheckDTO{
		dangerCheckDTO("fleet.roll", "Worker image roll", evil+" stuck"),
	}}}
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
