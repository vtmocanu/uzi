package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerManualCordonRepeatQuietHelp(t *testing.T) {
	at := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	label, actor, reason := "\u202e", "owner-uuid", "first reason"
	holds := 1
	w := apitypes.WorkerDTO{ID: "updated", Status: "offline", ManualCordonedAt: &at,
		ManualCordonedByLabel: &label, ManualCordonedBy: &actor, ManualCordonReason: &reason,
		CustodyDecisionsNeeded: &holds}
	fc := &uzicli.FakeClient{CordonWorkerResult: w, UncordonWorkerResult: apitypes.WorkerDTO{ID: w.ID}}
	first, stderr, code := runCLI(t, fakeEnv(fc), "worker", "cordon", "id", "-m", reason)
	if code != 0 || !strings.Contains(first, "actor=owner-uuid") {
		t.Fatalf("sanitized label fallback: code=%d out=%q err=%q", code, first, stderr)
	}
	second, _, code := runCLI(t, fakeEnv(fc), "worker", "cordon", "id", "-m", "replacement reason")
	if code != 0 || first != second || fc.CordonWorkerCalls != 2 {
		t.Fatal("repeat output overwrote authoritative first attribution")
	}
	for _, verb := range []string{"cordon", "uncordon"} {
		out, stderr, code := runCLI(t, fakeEnv(fc), "worker", verb, "id", "--quiet")
		if code != 0 || out != "" || stderr != "" {
			t.Fatalf("quiet %s: %d %q %q", verb, code, out, stderr)
		}
		before := fc.CordonWorkerCalls + fc.UncordonWorkerCalls
		out, _, code = runCLI(t, fakeEnv(fc), "worker", verb, "--help")
		if code != 0 || !strings.Contains(out, "Held work continues") || fc.CordonWorkerCalls+fc.UncordonWorkerCalls != before {
			t.Fatalf("help %s invoked mutation or lost continuation guidance: %q", verb, out)
		}
	}
}

func TestWorkerManualCordonCommands(t *testing.T) {
	now := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	reason := strings.Repeat("維", 198) + "😀" + strings.Repeat("尾", 301) + "\n\u202e\u001b]0;evil\a"
	actor := "owner\n\u202e\u001b[31m😀"
	w := apitypes.WorkerDTO{ID: "returned-id", Status: "offline", ManualCordonedAt: &now, ManualCordonedByLabel: &actor, ManualCordonReason: &reason, DrainingSince: &now, ActiveRuns: 1}
	for _, flag := range []string{"-m", "--reason", ""} {
		for _, asJSON := range []bool{false, true} {
			fc := &uzicli.FakeClient{CordonWorkerResult: w}
			args := []string{"worker", "cordon", "requested-id"}
			if flag != "" {
				args = append(args, flag, reason)
			}
			if asJSON {
				args = append(args, "--json")
			}
			out, stderr, code := runCLI(t, fakeEnv(fc), args...)
			if code != 0 {
				t.Fatalf("%d %s", code, stderr)
			}
			wantReason := reason
			if flag == "" {
				wantReason = ""
			}
			if fc.CordonWorkerCalls != 1 || fc.LastCordonWorkerID != "requested-id" || fc.LastCordonReason != wantReason {
				t.Fatalf("capture %+v", fc)
			}
			if asJSON {
				var got apitypes.WorkerDTO
				if err := json.Unmarshal([]byte(out), &got); err != nil || got.ID != w.ID || got.ManualCordonReason == nil || *got.ManualCordonReason != reason {
					t.Fatalf("json %q %v", out, err)
				}
			} else {
				for _, want := range []string{"returned-id", "offline · cordoned (manual) (draining)", "actor=owner 😀", "2026-10-23T01:02:03Z", strings.Repeat("維", 198) + "😀…", "Held work continues", "other fences can remain"} {
					if !strings.Contains(out, want) {
						t.Errorf("missing %q in %q", want, out)
					}
				}
				if !utf8.ValidString(out) || strings.ContainsAny(out, "\u001b\u202e\a") || strings.Contains(out, "尾") {
					t.Fatalf("unsafe/unbounded %q", out)
				}
			}
		}
	}
	// Returned no-op state is authoritative, even when other intake fences remain.
	fc := &uzicli.FakeClient{UncordonWorkerResult: apitypes.WorkerDTO{ID: "updated", Status: "offline", EffectiveCordoned: true, DrainingSince: &now}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "worker", "uncordon", "id")
	if code != 0 || fc.UncordonWorkerCalls != 1 || fc.LastUncordonWorkerID != "id" || !strings.Contains(out, "offline (cordoned)") || !strings.Contains(out, "Manual cordon cleared") {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "worker", "uncordon", "id", "--json")
	var got apitypes.WorkerDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || !got.EffectiveCordoned || got.ManualCordonedAt != nil {
		t.Fatalf("uncordon json %q %v", out, err)
	}
}

func TestWorkerManualCordonFailureAndUsage(t *testing.T) {
	for _, verb := range []string{"cordon", "uncordon"} {
		fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitConflict, "manual cordon is only supported for persistent workers")}
		out, stderr, code := runCLI(t, fakeEnv(fc), "worker", verb, "id")
		if code != uzicli.ExitConflict || out != "" || !strings.Contains(stderr, "manual cordon is only supported for persistent workers") {
			t.Fatalf("%d %q %q", code, out, stderr)
		}
		for _, tail := range [][]string{{}, {"a", "b"}} {
			f := &uzicli.FakeClient{}
			_, _, code = runCLI(t, fakeEnv(f), append([]string{"worker", verb}, tail...)...)
			if code == 0 || f.CordonWorkerCalls+f.UncordonWorkerCalls != 0 {
				t.Fatal("invalid args called client")
			}
		}
	}
}

func TestWorkerManualCordonList(t *testing.T) {
	now := time.Now()
	reason := "manual maintenance"
	w := apitypes.WorkerDTO{ID: "id", Name: "name", Status: "offline", ManualCordonedAt: &now, ManualCordonReason: &reason, DrainingSince: &now, ActiveRuns: 1, MemoryAuthorityLostAt: &now, ResidueQuarantinedAt: &now}
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{w, {ID: "legacy", Status: "online"}}}
	out, stderr, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != 0 {
		t.Fatalf("%d %s", code, stderr)
	}
	for _, want := range []string{"offline · cordoned (manual) (draining)", "(quarantined)", "(memory authority lost)", "Manual reason: manual maintenance", "legacy"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
}
