package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// GHSA-2722 item 5: an MR-thread reply body is agent-authored text headed for a forge
// comment (possibly on a public repo), so it must get the same secret scrub and
// terminal/bidi strip as the other agent-to-forge sinks. The assertion is on the body
// the mock forge actually RECEIVES over HTTP, after the real gitlab driver.
func TestReplyMRThreadScrubsBodyBeforeDriver(t *testing.T) {
	var received string
	h := mrThreadMockHandler(t, "gitlab", mrThreadIID(),
		snapshotJSON(t, snapComment("disc-real", "disc-real")),
		map[string]http.HandlerFunc{
			"/api/v4/projects/4242/merge_requests/284/discussions/disc-real/notes": func(w http.ResponseWriter, r *http.Request) {
				var note struct {
					Body string `json:"body"`
				}
				if err := json.NewDecoder(r.Body).Decode(&note); err != nil {
					t.Errorf("decode forge request: %v", err)
				}
				received = note.Body
				_ = json.NewEncoder(w).Encode(map[string]any{"id": 9001})
			},
		})

	// Fake GitHub classic PAT, assembled at runtime so no complete provider-token
	// literal sits in source; never a real credential.
	token := "gh" + "p_" + strings.Repeat("Zq9x", 9)
	bidi := string(rune(0x202e)) // RIGHT-TO-LEFT OVERRIDE, built at runtime so no raw bidi rune sits in source
	body := "fixed in abc123\nleaked " + token + " here" + bidi + "\x1b[31m done"

	rec := httptest.NewRecorder()
	h.WorkerForgeReplyMRThread(rec, mrThreadReq("/x", true,
		apitypes.ForgeMRThreadReplyRequest{ReplyID: "disc-real", Body: body}))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(received, "fixed in abc123\nleaked ") {
		t.Fatalf("forge received %q, want the reply text (newline kept) to reach the driver", received)
	}
	if strings.Contains(received, "ghp_") || strings.Contains(received, "Zq9x") {
		t.Errorf("forge received an unredacted credential: %q", received)
	}
	if !strings.Contains(received, "[redacted]") {
		t.Errorf("forge received %q, want the credential replaced by [redacted]", received)
	}
	if strings.ContainsAny(received, bidi+"\x1b") {
		t.Errorf("forge received a bidi override or escape byte: %q", received)
	}
}
