package agentsource

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/settings"
)

func TestRunnerDiscoveryAndStage(t *testing.T) {
	for _, mode := range []string{"success", "discovery error", "stage error", "discovery panic", "stage panic", "disabled", "enabled read error"} {
		t.Run(mode, func(t *testing.T) {
			st := newFakeStore()
			set := &fakeSettings{enabled: mode != "disabled", url: "https://ok.test/source.git", ref: "main"}
			if mode == "enabled read error" {
				set.enabledErr = errors.New("enabled unavailable")
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			rec := newRec(st, set, true)
			rec.logger = logger
			var order []string
			rec.lsRemote = func(context.Context, CloneOptions) (RemoteRefs, error) {
				order = append(order, "discovery")
				if mode == "discovery panic" {
					panic("discovery panic")
				}
				if mode == "discovery error" {
					return RemoteRefs{}, errors.New("discovery failed")
				}
				return RemoteRefs{HeadSHA: "tip", Branches: map[string]string{"main": "tip"}, Tags: map[string]string{"v1.10.0": "tagsha"}}, nil
			}
			rec.fetch = func(context.Context, CloneOptions) (string, []SourceFile, error) {
				order = append(order, "stage")
				if mode == "stage panic" {
					panic("stage panic")
				}
				if mode == "stage error" {
					return "", nil, errors.New("stage failed")
				}
				return "tip", []SourceFile{roleFile("coder", "body")}, nil
			}
			NewRunner(rec, set, logger).tick(context.Background())
			if mode == "disabled" || mode == "enabled read error" {
				if len(order) != 0 || len(st.appSet) != 0 || len(st.upserts) != 0 {
					t.Fatalf("disabled/read error performed work: %v", order)
				}
				return
			}
			if strings.Join(order, ",") != "discovery,stage" {
				t.Fatalf("operation order: %v", order)
			}
			if mode != "discovery error" && mode != "discovery panic" {
				if st.appSet[settings.KeyAgentSourceLatestRef] != "v1.10.0" || st.appSet[settings.KeyAgentSourceRemoteTipSHA] != "tip" || st.appSet[settings.KeyAgentSourceUpdateCheckedAt] == "" {
					t.Fatalf("missing discovery facts: %v", st.appSet)
				}
			} else if st.appSet[settings.KeyAgentSourceUpdateCheckedAt] != "" {
				t.Fatal("failed discovery persisted timestamp")
			}
			if mode != "stage error" && mode != "stage panic" && len(st.upserts) != 1 {
				t.Fatal("stage did not succeed")
			}
			if strings.HasSuffix(mode, "error") && !strings.Contains(logs.String(), "operation failed") {
				t.Fatalf("nil Go error status not logged: %s", logs.String())
			}
			if strings.HasSuffix(mode, "panic") && !strings.Contains(logs.String(), "panic recovered") {
				t.Fatalf("panic not logged: %s", logs.String())
			}
		})
	}
}
