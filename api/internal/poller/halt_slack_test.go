package poller

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1650 D3: the two halt notifications the poller produces (ci_autofix_halted,
// mr_rework_halted) now carry a Slack render that links to where the owner acts. The
// untrusted values (the branch ref, the halt reason) go into Body RAW: the notifier's
// SlackMrkdwn owns their escaping, and pre-escaping here would double-escape them.

// hostileRef is a branch name an attacker controls on the forge: a broadcast mention,
// an ampersand and emphasis markers. It must reach the render verbatim.
const hostileRef = "feat/<!channel>&*x*"

// cfCappedStore is a store whose ref has spent the attempt budget (count 2 == cap 2),
// so the next fresh pipeline halts.
func cfCappedStore(cand store.ListCIAutofixCandidateRefsRow) *cfStore {
	return &cfStore{
		candidates: []store.ListCIAutofixCandidateRefsRow{cand},
		attempts: map[string]store.CiAutofixAttempt{
			cand.Ref.String: {Ref: cand.Ref.String, AttemptCount: 2, LastPipelineID: pgtype.Int8{Int64: 9001, Valid: true}},
		},
	}
}

func TestCIAutofixHaltPublishesSlackPipelineLink(t *testing.T) {
	cand := cfCand(9010)
	cand.Ref = pgtype.Text{String: hostileRef, Valid: true}
	st := cfCappedStore(cand)
	notifier := &cfNotifier{}
	f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

	newCF(st, &cfRuns{}, notifier).detect(context.Background(), cfRepoRow(), f)

	if len(notifier.calls) != 1 || notifier.calls[0].kind != "ci_autofix_halted" {
		t.Fatalf("expected one halted notification, got %+v", notifier.calls)
	}
	r := notifier.calls[0].slack
	if r == nil {
		t.Fatalf("ci_autofix_halted must carry a Slack render (PRD #1650 D3)")
	}
	if r.Emoji != "🛑" || r.Title != "CI auto-fix stopped" {
		t.Fatalf("emoji/title = %q/%q, want 🛑/%q", r.Emoji, r.Title, "CI auto-fix stopped")
	}
	if r.Link != cand.PipelineWebUrl {
		t.Fatalf("link = %q, want the pipeline URL %q", r.Link, cand.PipelineWebUrl)
	}
	if r.LinkLabel != "Open the pipeline" {
		t.Fatalf("link label = %q, want %q", r.LinkLabel, "Open the pipeline")
	}
	if !strings.Contains(r.Body, hostileRef) {
		t.Fatalf("body must carry the ref RAW (the notifier escapes it once): %q", r.Body)
	}
	if !strings.Contains(r.Body, "reached the 2-attempt limit") {
		t.Fatalf("body must carry the halt reason: %q", r.Body)
	}
	if len(r.Facts) != 0 {
		t.Fatalf("no untrusted value may ride in the trusted Facts: %v", r.Facts)
	}
}

// A forge-supplied pipeline URL that is not a clean absolute http(s) URL would break
// out of Slack's <url|label> link markup, so the render drops the link rather than
// emit it. The notification itself still goes out.
func TestCIAutofixHaltDropsUnsafePipelineLink(t *testing.T) {
	for _, bad := range []string{
		"https://forge/grp/proj/-/pipelines/1|spoofed label",
		"https://forge/grp/proj/-/pipelines/<1>",
		"https://forge/grp/proj/-/pipelines/1 2",
		"https://forge/grp/proj/-/pipelines/1\x07",
		"javascript:alert(1)",
		"/grp/proj/-/pipelines/1",
		"https:///grp/proj/-/pipelines/1",
		"",
	} {
		cand := cfCand(9010)
		cand.PipelineWebUrl = bad
		notifier := &cfNotifier{}
		f := &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"}

		newCF(cfCappedStore(cand), &cfRuns{}, notifier).detect(context.Background(), cfRepoRow(), f)

		if len(notifier.calls) != 1 || notifier.calls[0].slack == nil {
			t.Fatalf("url %q: expected one halted notification with a Slack render, got %+v", bad, notifier.calls)
		}
		if got := notifier.calls[0].slack.Link; got != "" {
			t.Fatalf("url %q: unsafe pipeline URL must yield no link, got %q", bad, got)
		}
	}
}

// The halt_notified latch keeps the DM to one per halt: a second halted tick (a fresh
// pipeline on the same stuck ref) records silently and publishes no second render.
func TestCIAutofixHaltSlackOncePerHalt(t *testing.T) {
	cand := cfCand(9010)
	st := cfCappedStore(cand)
	notifier := &cfNotifier{}
	d := newCF(st, &cfRuns{}, notifier)

	d.detect(context.Background(), cfRepoRow(), &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"})
	st.candidates = []store.ListCIAutofixCandidateRefsRow{cfCand(9011)}
	d.detect(context.Background(), cfRepoRow(), &cfForge{jobs: []forge.Job{cfJob()}, logTail: "boom"})

	renders := 0
	for _, c := range notifier.calls {
		if c.slack != nil {
			renders++
		}
	}
	if renders != 1 {
		t.Fatalf("Slack renders across two halted ticks = %d, want exactly 1 (calls %+v)", renders, notifier.calls)
	}
}

func TestMRReworkHaltPublishesSlackRunLink(t *testing.T) {
	cand := mrwCand("success")
	cand.Ref = pgtype.Text{String: hostileRef, Valid: true}
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{cand},
		ledgers:    map[string]store.MrReworkLedger{hostileRef: {Ref: hostileRef, AttemptCount: 5, HighWater: 120}},
	}
	notifier := &mrwNotifier{}
	f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

	newMRW(st, &mrwRuns{}, notifier, mrwSettings{enabled: true, capVal: 5, baseURL: "https://uzi.example.com/"}).
		detect(context.Background(), mrwRepoRow(), f)

	if len(notifier.calls) != 1 || notifier.calls[0].kind != "mr_rework_halted" {
		t.Fatalf("expected one halted notification, got %+v", notifier.calls)
	}
	r := notifier.calls[0].slack
	if r == nil {
		t.Fatalf("mr_rework_halted must carry a Slack render (PRD #1650 D3)")
	}
	if want := "https://uzi.example.com/runs/" + mrwSourceRunID.String(); r.Link != want {
		t.Fatalf("link = %q, want the source run page %q", r.Link, want)
	}
	if r.Emoji == "" || r.Title != "MR rework stopped" {
		t.Fatalf("emoji/title = %q/%q, want a glyph and %q", r.Emoji, r.Title, "MR rework stopped")
	}
	if !strings.Contains(r.Body, hostileRef) || !strings.Contains(r.Body, "Rework now") {
		t.Fatalf("body must carry the ref RAW and name Rework now: %q", r.Body)
	}
	if len(r.Facts) != 1 || r.Facts[0] != "`5`-cycle limit" {
		t.Fatalf("facts = %v, want only the int cap chip", r.Facts)
	}
}

// No public base URL (unset, or its read failed) means the DM goes out with no link.
func TestMRReworkHaltSlackNoBaseNoLink(t *testing.T) {
	for name, set := range map[string]mrwSettings{
		"empty base": {enabled: true, capVal: 5},
		"base error": {enabled: true, capVal: 5, baseURL: "https://uzi.example.com", baseErr: errors.New("settings down")},
	} {
		st := &mrwStore{
			candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
			ledgers:    map[string]store.MrReworkLedger{mrwRef: {Ref: mrwRef, AttemptCount: 5, HighWater: 120}},
		}
		notifier := &mrwNotifier{}
		f := landedForge(mrwComment(200, landed(), mrwHeadSHA))

		newMRW(st, &mrwRuns{}, notifier, set).detect(context.Background(), mrwRepoRow(), f)

		if len(notifier.calls) != 1 || notifier.calls[0].slack == nil {
			t.Fatalf("%s: expected one halted notification with a Slack render, got %+v", name, notifier.calls)
		}
		if got := notifier.calls[0].slack.Link; got != "" {
			t.Fatalf("%s: link = %q, want none", name, got)
		}
	}
}
