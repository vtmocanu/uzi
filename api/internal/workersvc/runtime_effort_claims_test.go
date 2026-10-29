package workersvc

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// requireNoDefaultEffortKey marshals a claim config and fails when the wire carries a
// default_effort key at all (PRD #3247: Runtime default omits the key; omitempty drops nil).
func requireNoDefaultEffortKey(t *testing.T, name string, config any) {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatalf("%s: marshal: %v", name, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: unmarshal: %v", name, err)
	}
	if v, ok := m["default_effort"]; ok {
		t.Fatalf("%s: claim carries default_effort=%v for a runtime preference: %s", name, v, raw)
	}
}

// A stored `runtime` preference leaves the chat claim without default_effort.
func TestClaimChatRuntimeEffortOmitsDefaultEffort(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-chat-runtime-abcdef1234567890"))
	uid := uuid.New()
	fs := &fakeStore{
		chatClaimRun:  store.Run{ID: uuid.New(), UserID: uid, Kind: runkind.Chat, Status: "claimed", Title: pgtype.Text{String: "t", Valid: true}},
		anthropic:     sealedTok,
		defaultEffort: pgconv.TextOrNull("runtime"),
	}
	svc := newOffModeService(fs, box, testParams())
	payload, err := svc.ClaimChat(context.Background(), store.Worker{ID: uuid.New(), UserID: uid})
	if err != nil || payload == nil {
		t.Fatalf("ClaimChat: payload=%v err=%v", payload, err)
	}
	if payload.Config.DefaultEffort != nil {
		t.Fatalf("DefaultEffort = %q, want nil", *payload.Config.DefaultEffort)
	}
	requireNoDefaultEffortKey(t, "chat", payload.Config)
}

// A stored `runtime` preference leaves the Claude judge claim without default_effort.
func TestJudgeClaimRuntimeEffortOmitsDefaultEffort(t *testing.T) {
	box := newBox(t)
	sealedTok, _ := box.Seal([]byte("anthropic-judge-runtime-abcdef1234567890"))
	uid, target := uuid.New(), uuid.New()
	fs := &fakeStore{
		claimRun:      judgeRun(uid, target),
		anthropic:     sealedTok,
		defaultEffort: pgconv.TextOrNull("runtime"),
	}
	svc := newOffModeService(fs, box, testParams())
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku"})
	payload, err := svc.Claim(context.Background(), store.Worker{ID: uuid.New(), UserID: uid}, nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim: payload=%v err=%v", payload, err)
	}
	if payload.Config.DefaultEffort != nil {
		t.Fatalf("DefaultEffort = %q, want nil", *payload.Config.DefaultEffort)
	}
	requireNoDefaultEffortKey(t, "claude judge", payload.Config)
}

// The two ordinary pointer helpers return nil exactly for the runtime sentinel and never
// pass a stored string through verbatim.
func TestResolveEffortPtrsRuntimeAndCorrupt(t *testing.T) {
	for _, h := range []Harness{HarnessClaude, HarnessCodex} {
		for stored, want := range map[string]string{"runtime": "", "ultra": "medium", "HIGH": "medium", "xhigh": "xhigh"} {
			got := resolveHarnessEffortPtr(h, pgconv.TextOrNull(stored))
			if want == "" {
				if got != nil {
					t.Errorf("%s/%q: got %q, want nil", h, stored, *got)
				}
				continue
			}
			if got == nil || *got != want {
				t.Errorf("%s/%q: got %v, want %q", h, stored, got, want)
			}
		}
	}
	if got := resolveEffortPtr(pgconv.TextOrNull("runtime")); got != nil {
		t.Errorf("resolveEffortPtr(runtime) = %q, want nil", *got)
	}
	if got := resolveEffortPtr(pgtype.Text{}); got == nil || *got != "medium" {
		t.Errorf("resolveEffortPtr(NULL) = %v, want medium", got)
	}
}
