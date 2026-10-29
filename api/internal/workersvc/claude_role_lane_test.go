package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type roleLaneStore struct{ *fakeStore }

func (f *roleLaneStore) ClearJobResultForRun(context.Context, uuid.UUID) error { return nil }
func (f *roleLaneStore) ListJobInputsForClaim(context.Context, uuid.UUID) ([]store.ListJobInputsForClaimRow, error) {
	return nil, nil
}
func (f *roleLaneStore) ListJobInputFilesForClaim(context.Context, pgtype.UUID) ([]store.ListJobInputFilesForClaimRow, error) {
	return nil, nil
}

func codexRoleFixture(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../fixtures/codex-models/models.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Role []string }
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Role) == 0 {
		t.Fatal("empty role fixture")
	}
	return fixture.Role
}

func TestClaimClaudeRoleLane(t *testing.T) {
	type laneCase struct {
		name         string
		lane, frozen pgtype.Text
		want         string
		dropped      bool
	}
	cases := []laneCase{
		{name: "unset"},
		{name: "custom", lane: pgconv.TextOrNull("claude-custom"), want: "claude-custom"},
		{name: "non-role", lane: pgconv.TextOrNull("gpt-custom-preview"), want: "gpt-custom-preview"},
		{name: "custom-verbatim", lane: pgconv.TextOrNull("  custom-preview  "), want: "  custom-preview  "},
	}
	for _, model := range codexRoleFixture(t) {
		for _, stored := range []string{model, " \t" + model + " \n"} {
			cases = append(cases, laneCase{name: stored, lane: pgconv.TextOrNull(stored), dropped: true})
		}
	}
	cases = append(cases, laneCase{name: "compatible-frozen-wins", lane: pgconv.TextOrNull("gpt-6-luna"), frozen: pgconv.TextOrNull("claude-frozen"), want: "claude-frozen", dropped: true})
	for _, path := range []string{"ordinary", "chat", "job"} {
		for _, tc := range cases {
			if path == "chat" && tc.frozen.Valid {
				continue
			} // Chat has no frozen-model override.
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				fs := scheduleModelStore(t, tc.frozen, tc.lane)
				fs.claimRun.Harness = harnessClaude
				fs.claimRun.UserID = uuid.New()
				fs.claimRun.Kind = runkind.Issue
				fs.claimRun.LastSeq = 17
				fs.claimRun.ClaimGeneration = 2
				if path == "job" {
					fs.claimRun.Kind = runkind.Job
				}
				fs.chatClaimRun = fs.claimRun
				fs.chatClaimRun.Kind = runkind.Chat
				var logs bytes.Buffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				t.Cleanup(func() { slog.SetDefault(old) })
				svc := newOffModeService(&roleLaneStore{fs}, newBox(t), testParams())
				var model *string
				var lastSeq int32
				var err error
				switch path {
				case "chat":
					var pl *ChatClaimPayload
					pl, err = svc.ClaimChat(context.Background(), worker())
					if pl != nil {
						model = pl.Config.DefaultModel
						lastSeq = pl.LastSeq
					}
				default:
					var pl *ClaimPayload
					pl, err = svc.Claim(context.Background(), worker(), nil)
					if pl != nil {
						model = pl.Config.DefaultModel
						lastSeq = pl.LastSeq
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if (model == nil) != (tc.want == "") || (model != nil && *model != tc.want) {
					t.Fatalf("default model = %v, want %q", model, tc.want)
				}
				if fs.defaultModel != tc.lane || fs.claimRun.Model != tc.frozen {
					t.Fatalf("stored lane changed: %+v", fs.defaultModel)
				}
				notes := 0
				for _, m := range fs.insertedMessages {
					if m.Kind != harnessModelFallbackKind {
						continue
					}
					notes++
					var note harnessModelFallbackPayload
					if err := json.Unmarshal(m.Payload, &note); err != nil {
						t.Fatal(err)
					}
					if note.Harness != "claude" || note.DroppedModel != strings.TrimSpace(tc.lane.String) || m.Seq != 18 || !m.ClaimGeneration.Valid || m.ClaimGeneration.Int64 != 2 {
						t.Fatalf("fallback note = %+v; message=%+v", note, m)
					}
				}
				if path == "ordinary" && tc.dropped {
					if notes != 1 || lastSeq != 18 || fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != 18 {
						t.Fatalf("fallback note/seq: notes=%d claim=%d stored=%v", notes, lastSeq, fs.lastSeqUpdated)
					}
				} else {
					if notes != 0 || lastSeq != 17 {
						t.Fatalf("unexpected notes/seq: %d/%d", notes, lastSeq)
					}
				}
				warnFound := false
				for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
					if line == "" {
						continue
					}
					var entry map[string]any
					if err := json.Unmarshal([]byte(line), &entry); err != nil {
						t.Fatal(err)
					}
					if entry["msg"] == path+" claim: dropped Codex role model from Claude lane" {
						warnFound = true
						if entry["level"] != "WARN" || entry["run_id"] != fs.claimRun.ID.String() || entry["model"] != strings.TrimSpace(tc.lane.String) {
							t.Fatalf("warning=%v", entry)
						}
					}
				}
				if warnFound != (tc.dropped && path != "ordinary") {
					t.Fatalf("warning found=%v logs=%s", warnFound, logs.String())
				}
			})
		}
	}
}

func TestClaimClaudeFrozenCodexRoleModel(t *testing.T) {
	for _, path := range []string{"ordinary", "job"} {
		for _, tc := range []struct {
			name string
			lane pgtype.Text
			want string
		}{
			{name: "valid lane", lane: pgconv.TextOrNull("claude-custom"), want: "claude-custom"},
			{name: "unset lane"},
			{name: "filtered lane", lane: pgconv.TextOrNull("gpt-6-sol")},
		} {
			for _, frozen := range []string{"gpt-6-luna", " \tgpt-6-luna \n"} {
				t.Run(path+"/"+tc.name+"/"+frozen, func(t *testing.T) {
					pin := pgconv.TextOrNull(frozen)
					fs := scheduleModelStore(t, pin, tc.lane)
					fs.claimRun.Harness = harnessClaude
					fs.claimRun.Kind = runkind.Issue
					fs.claimRun.LastSeq = 17
					fs.claimRun.ClaimGeneration = 2
					if path == "job" {
						fs.claimRun.Kind = runkind.Job
					}
					pl, err := newOffModeService(&roleLaneStore{fs}, newBox(t), testParams()).Claim(context.Background(), worker(), nil)
					if err != nil {
						t.Fatal(err)
					}
					if pl == nil {
						t.Fatal("missing claim")
					}
					model := pl.Config.DefaultModel
					if (model == nil) != (tc.want == "") || (model != nil && *model != tc.want) {
						t.Fatalf("frozen Codex role reached Claude claim: model=%v, want %q", model, tc.want)
					}
					if fs.defaultModel != tc.lane || fs.claimRun.Model != pin {
						t.Fatal("stored run or user model changed")
					}
					var notes []store.InsertRunMessageParams
					for _, message := range fs.insertedMessages {
						if message.Kind == harnessModelFallbackKind {
							notes = append(notes, message)
						}
					}
					var dropped []string
					if path == "ordinary" {
						if tc.name == "filtered lane" {
							dropped = append(dropped, "gpt-6-sol")
						}
						dropped = append(dropped, frozen)
					}
					if len(notes) != len(dropped) || int(pl.LastSeq) != 17+len(dropped) {
						t.Fatalf("fallback notes=%d last_seq=%d, want %d notes", len(notes), pl.LastSeq, len(dropped))
					}
					for i, want := range dropped {
						var note harnessModelFallbackPayload
						if err := json.Unmarshal(notes[i].Payload, &note); err != nil {
							t.Fatal(err)
						}
						if note.Harness != "claude" || note.DroppedModel != want || int(notes[i].Seq) != 18+i ||
							!notes[i].ClaimGeneration.Valid || notes[i].ClaimGeneration.Int64 != 2 {
							t.Fatalf("fallback note %d=%+v, message=%+v", i, note, notes[i])
						}
					}
					if path == "ordinary" && (fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != pl.LastSeq) {
						t.Fatal("stored fallback sequence differs from claim")
					}
				})
			}
		}
	}
}

func TestClaimClaudeRoleLaneFallbackSequence(t *testing.T) {
	fs := scheduleModelStore(t, pgconv.TextOrNull("gpt-6-astra"), pgconv.TextOrNull("gpt-6-luna"))
	fs.claimRun.Harness = harnessClaude
	fs.claimRun.LastSeq = 17
	pl, err := newOffModeService(fs, newBox(t), testParams()).Claim(context.Background(), worker(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pl == nil || pl.Config.DefaultModel != nil || pl.LastSeq != 19 || fs.lastSeqUpdated == nil || *fs.lastSeqUpdated != 19 {
		t.Fatalf("fallback sequence: payload=%+v stored=%v", pl, fs.lastSeqUpdated)
	}
	var notes []store.InsertRunMessageParams
	for _, m := range fs.insertedMessages {
		if m.Kind == harnessModelFallbackKind {
			notes = append(notes, m)
		}
	}
	if len(notes) != 2 {
		t.Fatalf("fallback messages=%+v", notes)
	}
	for i, want := range []string{"gpt-6-luna", "gpt-6-astra"} {
		var note harnessModelFallbackPayload
		if err := json.Unmarshal(notes[i].Payload, &note); err != nil {
			t.Fatal(err)
		}
		if int(notes[i].Seq) != 18+i || note.DroppedModel != want {
			t.Fatalf("note %d: %+v %+v", i, notes[i], note)
		}
	}
	if fs.defaultModel.String != "gpt-6-luna" || fs.claimRun.Model.String != "gpt-6-astra" {
		t.Fatal("stored models changed")
	}
}
