package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCompletionNonClosingNormalization(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		invalid           bool
	}{
		{"zero", "", "", true},
		{"whitespace", " \t\n ", "", true},
		{"nul whitespace", "\x00 \t\x00", "", true},
		{"one", "x", "x", false},
		{"strip before trim", "\x00 \t😀 \x00", "😀", false},
		{"normalize before length", "\x00 " + strings.Repeat("😀", 500) + " \x00", strings.Repeat("😀", 500), false},
		{"500 astral", strings.Repeat("😀", 500), strings.Repeat("😀", 500), false},
		{"501 astral", strings.Repeat("😀", 501), "", true},
	} {
		for _, stored := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: " fresh", true: " stored"}[stored], func(t *testing.T) {
				w := worker()
				run := interlockedRun(w)
				run.Kind = "issue"
				if stored {
					run.CompletionNonClosingReason = pgtype.Text{String: "original", Valid: true}
				}
				fs := &fakeStore{runOwned: run}
				svc := New(fs, newBox(t), testParams())
				_, err := svc.RecordCompletionAttempt(context.Background(), w, run.ID, CompletionAttemptRequest{NonClosing: &CompletionNonClosing{Reason: tc.input}})
				if tc.invalid {
					if !errors.Is(err, ErrInvalidNonClosing) || len(fs.recordedAttempts) != 0 {
						t.Fatalf("err=%v writes=%+v", err, fs.recordedAttempts)
					}
				} else {
					if err != nil || len(fs.recordedAttempts) != 1 {
						t.Fatalf("err=%v writes=%+v", err, fs.recordedAttempts)
					}
					if got := fs.recordedAttempts[0].CompletionNonClosingReason; !got.Valid || got.String != tc.want {
						t.Fatalf("reason=%+v want %q", got, tc.want)
					}
				}
			})
		}
	}
}

func TestCompletionNonClosingNonIssue(t *testing.T) {
	for _, supplied := range []bool{false, true} {
		w := worker()
		run := interlockedRun(w)
		run.Kind = "prompt"
		fs := &fakeStore{runOwned: run}
		svc := New(fs, newBox(t), testParams())
		req := CompletionAttemptRequest{}
		if supplied {
			req.NonClosing = &CompletionNonClosing{Reason: "later"}
		}
		_, err := svc.RecordCompletionAttempt(context.Background(), w, run.ID, req)
		if supplied {
			if !errors.Is(err, ErrInvalidNonClosing) || len(fs.recordedAttempts) != 0 {
				t.Fatalf("err=%v writes=%v", err, fs.recordedAttempts)
			}
		} else if err != nil || len(fs.recordedAttempts) != 1 {
			t.Fatalf("legacy nonissue err=%v writes=%v", err, fs.recordedAttempts)
		}
	}
}

func TestCompletionNonClosingClaimWire(t *testing.T) {
	for _, stored := range []bool{false, true} {
		box := newBox(t)
		pat, err := box.Seal([]byte("test-forge-pat"))
		if err != nil {
			t.Fatal(err)
		}
		token, err := box.Seal([]byte("test-anthropic-token"))
		if err != nil {
			t.Fatal(err)
		}
		run := store.Run{ID: uuid.New(), RepoID: pgconv.UUID(uuid.New()), Kind: "issue", Status: "claimed", PlanSource: planSourceAgent}
		if stored {
			run.CompletionNonClosingReason = pgconv.TextOrNull("later 😀")
		}
		fs := &fakeStore{
			claimRun: run, attributionEnabled: true, anthropic: token,
			claimCtx: store.GetRunClaimContextRow{
				RepoWebUrl: "https://gitlab.example.com/g/p", RepoPath: "g/p", DefaultBranch: pgconv.TextOrNull("main"),
				ForgeType: "gitlab", BaseUrl: "https://gitlab.example.com", BotUsername: "uzi-bot", TokenCiphertext: pat,
			},
		}
		svc := newOffModeService(fs, box, testParams())
		payload, err := svc.Claim(context.Background(), worker(), nil)
		if err != nil || payload == nil {
			t.Fatalf("Claim=%+v err=%v", payload, err)
		}
		raw, err := json.Marshal(payload.Config)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatal(err)
		}
		got, present := wire["non_closing"]
		if present != stored {
			t.Fatalf("stored=%v config=%s", stored, raw)
		}
		if stored && string(got) != `{"reason":"later 😀"}` {
			t.Fatalf("reason=%s", got)
		}
	}
}
