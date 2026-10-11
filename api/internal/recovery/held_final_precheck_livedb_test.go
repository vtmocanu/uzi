package recovery

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
)

// heldLogText bounds and neutralizes an attacker-controlled forge error before it is logged.
func TestHeldLogTextSanitizes(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	raw := "remote said: " + token + " \u202e\x1b[31m\nforged line\r\n" + strings.Repeat("x", 1<<20+5)
	got := heldLogText(errors.New(raw))
	if strings.Contains(got, token) {
		t.Fatal("the token was not scrubbed")
	}
	for _, r := range got {
		if isUnsafeLabelRune(r) {
			t.Fatalf("unsafe rune %U in %q", r, got)
		}
	}
	if n := utf8.RuneCountInString(got); n > maxHeldLogLen {
		t.Fatalf("%d runes, want at most %d", n, maxHeldLogLen)
	}
	if !strings.HasPrefix(got, "remote said:") {
		t.Fatalf("got %q", got)
	}
}

// A token split by a separator must not survive the scrub: every separator is dropped BEFORE
// secretscrub runs, so the rejoined token is redacted whole.
func TestHeldLogTextRedactsSplitTokens(t *testing.T) {
	seps := map[string]string{
		"newline": "\n", "nul": "\x00", "esc": "\x1b", "zwnj": "\u200c", "zwj": "\u200d",
		"rlo": "\u202e", "ls": "\u2028", "bom": "\ufeff", "word joiner": "\u2060",
		"csi sequence": "\x1b[31m", "osc sequence": "\x1b]0;t\x07",
		"cgj": "\u034f", "vs16": "\ufe0f", "combining acute": "\u0301", "hangul filler": "\u3164",
	}
	body := strings.Repeat("a1B2", 9)
	for name, sep := range seps {
		for _, prefix := range []string{"gh" + "p_", "glpat" + "-"} {
			b := body
			if prefix == "glpat-" {
				b = body[:20]
			}
			joined := prefix + b
			got := heldLogText(errors.New("fail: " + prefix + sep + b + " end"))
			// The separator is dropped before the scrub, so the rejoined token is redacted
			// whole: neither the token nor its bare body may survive.
			if strings.Contains(got, joined) || strings.Contains(got, b) {
				t.Errorf("%s/%s: token or its body survived in %q", name, prefix, got)
			}
			for _, r := range got {
				if isUnsafeLabelRune(r) {
					t.Errorf("%s/%s: unsafe rune %U in %q", name, prefix, r, got)
				}
			}
		}
	}
}

func TestHeldFinalListingErrorIsSanitizedWhenLoggedLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	token := "ghp_" + strings.Repeat("a1B2", 9)
	e.listErr = errors.New(token + " \u202e\x1b[31m\nforged\n" + strings.Repeat("y", 1<<20))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	res, err := e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseUnavailable)
	out := buf.String()
	if out == "" || strings.Contains(out, token) || strings.Contains(out, "\u202e") || strings.Contains(out, "\x1b") ||
		strings.Count(out, "\n") != 1 || len(out) > 2000 {
		t.Fatalf("log output not sanitized/bounded (len %d): %.300q", len(out), out)
	}
}

func TestHeldFinalIneligibleRequestSpendsNoForgeCallLiveDB(t *testing.T) {
	t.Run("never published source sha", func(t *testing.T) {
		e := newHeldEnv(t)
		req := e.request()
		req.FinalDisposition.SourceSha = strings.Repeat("2", 40)
		res, err := e.release(e.w, req)
		e.wantRetained(res, err, HeldReleaseNotEligible)
		if e.lists() != 0 {
			t.Fatalf("%d forge listings for an ineligible request", e.lists())
		}
	})
	t.Run("run not failed", func(t *testing.T) {
		e := newHeldEnv(t)
		e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
		res, err := e.release(e.w, e.request())
		e.wantRetained(res, err, HeldReleaseNotEligible)
		if e.lists() != 0 {
			t.Fatalf("%d forge listings for an ineligible request", e.lists())
		}
	})
}

func TestHeldFinalPrecheckFailedThenRacedEligibleIsNotReleasedLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
	e.svc.heldPreLockHook = func() { e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run) }
	res, err := e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseUnavailable)
	if e.lists() != 0 {
		t.Fatalf("%d forge listings", e.lists())
	}
}

func TestHeldFinalPrecheckFailedThenConcurrentExactReleaseIsAckedLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	winner := e.newService()
	e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
	e.svc.heldPreLockHook = func() {
		e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)
		if res, err := winner.Release(e.ctx, e.w, e.run, e.request()); err != nil || !res.Released {
			t.Errorf("winner = %+v, %v", res, err)
		}
		e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
	}
	res, err := e.release(e.w, e.request())
	if err != nil {
		t.Fatal(err)
	}
	e.wantAck(res)
}

func TestHeldFinalRemoteGoneIsDefinitiveLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	e.remoteErr = ErrHeldRemoteGone
	res, err := e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseNotEligible)
}

func TestHeldFinalRemoteGoneThenConcurrentExactReleaseIsAckedLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	winner := e.newService()
	e.remoteErr = ErrHeldRemoteGone
	e.svc.heldPreLockHook = func() {
		e.mu.Lock()
		e.remoteErr = nil
		e.mu.Unlock()
		if res, err := winner.Release(e.ctx, e.w, e.run, e.request()); err != nil || !res.Released {
			t.Errorf("winner = %+v, %v", res, err)
		}
	}
	res, err := e.release(e.w, e.request())
	if err != nil {
		t.Fatal(err)
	}
	e.wantAck(res)
}

func TestHeldFinalListThrottleLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	allowed := false
	e.svc.SetHeldListAllow(func(id uuid.UUID) bool {
		if id != e.w.ID {
			t.Errorf("limiter keyed by %s, want the worker %s", id, e.w.ID)
		}
		return allowed
	})
	res, err := e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseUnavailable)
	if e.lists() != 0 {
		t.Fatalf("%d forge listings while throttled", e.lists())
	}

	// An ineligible request never consults the limiter.
	e.svc.SetHeldListAllow(func(uuid.UUID) bool { t.Error("limiter consulted for an ineligible request"); return false })
	e.exec("UPDATE runs SET status='running' WHERE id=$1", e.run)
	res, err = e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseNotEligible)
	e.exec("UPDATE runs SET status='failed' WHERE id=$1", e.run)

	// Once allowed it releases; an exact replay is then ACKed without consulting the limiter.
	e.svc.SetHeldListAllow(func(uuid.UUID) bool { return true })
	res, err = e.release(e.w, e.request())
	e.wantAck(res)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.SetHeldListAllow(func(uuid.UUID) bool { t.Error("limiter consulted for a replay"); return false })
	res, err = e.release(e.w, e.request())
	if err != nil {
		t.Fatal(err)
	}
	e.wantAck(res)
}
