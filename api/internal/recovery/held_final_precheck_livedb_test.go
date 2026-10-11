package recovery

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"unicode/utf8"
)

// heldLogText bounds and neutralizes an attacker-controlled forge error before it is logged.
func TestHeldLogTextSanitizes(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2", 9)
	raw := "remote said: " + token + " ‮\x1b[31m\nforged line\r\n" + strings.Repeat("x", 1<<20+5)
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

func TestHeldFinalListingErrorIsSanitizedWhenLoggedLiveDB(t *testing.T) {
	e := newHeldEnv(t)
	token := "ghp_" + strings.Repeat("a1B2", 9)
	e.listErr = errors.New(token + " ‮\x1b[31m\nforged\n" + strings.Repeat("y", 1<<20))
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)
	res, err := e.release(e.w, e.request())
	e.wantRetained(res, err, HeldReleaseUnavailable)
	out := buf.String()
	if out == "" || strings.Contains(out, token) || strings.Contains(out, "‮") || strings.Contains(out, "\x1b") ||
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
