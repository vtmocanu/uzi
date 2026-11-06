package setupcmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// assertNoToken fails when the forge token appears on either stream.
func assertNoToken(t *testing.T, h *harness) {
	t.Helper()
	if strings.Contains(h.stdout.String(), testToken) {
		t.Errorf("the forge token reached stdout:\n%s", h.stdout.String())
	}
	if strings.Contains(h.stderr.String(), testToken) {
		t.Errorf("the forge token reached stderr:\n%s", h.stderr.String())
	}
}

// Report metadata that came from the forge or the database is untrusted text
// too: a stored bot username equal to the supplied token must not echo it
// (PR #3956 review, item 3). The join token, the one intended secret, stays.
func TestRunScrubsTheTokenFromReportMetadata(t *testing.T) {
	h := newHarness()
	h.st.conn = &store.ForgeConnection{ID: uuid.New(), ForgeType: "github", BaseUrl: testBase, BotUsername: testToken}
	code := h.run(context.Background(), baseArgs("--repos", "acme/app", "--worker-name", "w1")...)
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	rep := h.report(t)
	if rep.Forge == nil || rep.Forge.Status != StatusPresent {
		t.Fatalf("forge step = %+v, want present (the metadata path)", rep.Forge)
	}
	if rep.Forge.BotUsername != "[redacted]" {
		t.Errorf("bot_username = %q, want [redacted]", rep.Forge.BotUsername)
	}
	if rep.Worker == nil || rep.Worker.JoinToken != testJoinToken {
		t.Errorf("the join token must stay in the report, worker = %+v", rep.Worker)
	}
	assertNoToken(t, h)
}

// Log lines that the shared services write through slog go through the same
// scrubber as setup's own messages (PR #3956 review, item 7). provisionsvc logs
// a failed repo read with its error, which here carries the token.
func TestRunScrubsTheTokenFromServiceLogLines(t *testing.T) {
	h := newHarness()
	h.st.getRepoErr = errors.New("read failed near " + testToken)
	h.run(context.Background(), baseArgs("--repos", "acme/app")...)
	if !strings.Contains(h.stderr.String(), "get repo for enable guard") {
		t.Fatalf("the provisionsvc log line did not reach setup's stderr (positive control):\n%s", h.stderr.String())
	}
	assertNoToken(t, h)
}

// A database error can carry the password (pgx redacts malformed connection
// strings only best-effort), so setup prints a fixed message instead of the raw
// error on both streams (PR #3956 review, item 8).
func TestRunDatabaseOpenErrorPrintsAFixedMessage(t *testing.T) {
	const password = "s3cretDbPassw0rd"
	h := newHarness()
	h.openErr = errors.New("cannot parse `postgres://uzi:" + password + "@db/uzi`: failed")
	if code := h.run(context.Background(), baseArgs("--repos", "acme/app")...); code != ExitUsage {
		t.Fatalf("exit = %d, want 2", code)
	}
	rep := h.report(t)
	if rep.Error != dbStartupMessage {
		t.Errorf("report error = %q, want %q", rep.Error, dbStartupMessage)
	}
	for name, s := range map[string]string{"stdout": h.stdout.String(), "stderr": h.stderr.String()} {
		if strings.Contains(s, password) {
			t.Errorf("the database password reached %s:\n%s", name, s)
		}
	}
}
