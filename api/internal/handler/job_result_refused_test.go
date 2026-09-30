package handler

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1909 M4 rework: the optional bounded refused_outputs of the job-result body.

func TestValidateRefusedOutputs(t *testing.T) {
	req := workerJobResultRequest{Status: "completed", RefusedOutputs: []workerRefusedOutputBody{
		{DisplayName: "empty.csv", Reason: "worker_empty"},
		{DisplayName: "empty.csv", Reason: "worker_empty"}, // duplicate: kept once
		{DisplayName: "outputs/x/../huge.pdf", Reason: "worker_too_large"},
		{DisplayName: "ev\u202eil.txt", Reason: "worker_unreadable"},
		{DisplayName: "a.txt", Reason: "worker_upload_failed"},
		{DisplayName: "a.txt", Reason: "worker_busy"}, // same name, other reason: kept
	}}
	sub, err := validateAndScrubJobResult(req)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, r := range sub.RefusedOutputs {
		got[r.DisplayName+"|"+r.Reason]++
	}
	want := []string{"empty.csv|worker_empty", "huge.pdf|worker_too_large", "evil.txt|worker_unreadable", "a.txt|worker_upload_failed", "a.txt|worker_busy"}
	if len(sub.RefusedOutputs) != len(want) {
		t.Fatalf("refused outputs = %+v, want %v", sub.RefusedOutputs, want)
	}
	for _, w := range want {
		if got[w] != 1 {
			t.Errorf("missing %q in %+v", w, sub.RefusedOutputs)
		}
	}
}

func TestValidateRefusedOutputsRejects(t *testing.T) {
	for name, in := range map[string][]workerRefusedOutputBody{
		"unknown reason": {{DisplayName: "a.txt", Reason: "because"}},
		"empty reason":   {{DisplayName: "a.txt"}},
		"server reason":  {{DisplayName: "a.txt", Reason: "file_too_large"}}, // not a worker reason
		"huge name":      {{DisplayName: strings.Repeat("n", 5000), Reason: "worker_empty"}},
		"too many": func() []workerRefusedOutputBody {
			return make([]workerRefusedOutputBody, workersvc.JobResultMaxRefusedOutputs+1)
		}(),
	} {
		if _, err := validateRefusedOutputs(in); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestDecodeJobResultRefusedOutputsStream(t *testing.T) {
	ok, err := decodeJobResultBytes([]byte(`{"claim_generation":1,"status":"completed","report_md":"r","refused_outputs":[{"display_name":"a.txt","reason":"worker_empty"}]}`))
	if err != nil || len(ok.RefusedOutputs) != 1 || ok.RefusedOutputs[0].Reason != "worker_empty" {
		t.Fatalf("decode = %+v, %v", ok, err)
	}
	if _, err := decodeJobResultBytes([]byte(`{"refused_outputs":[{"display_name":"a","reason":"worker_empty","extra":1}]}`)); err == nil {
		t.Error("an unknown field inside a refused output was accepted")
	}
	if _, err := decodeJobResultBytes([]byte(`{"refused_outputs":[],"refused_outputs":[]}`)); err == nil {
		t.Error("a repeated refused_outputs key was accepted")
	}
	// The stream stops at the first element past the cap.
	var b strings.Builder
	b.WriteString(`{"refused_outputs":[`)
	for i := 0; i <= workersvc.JobResultMaxRefusedOutputs; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{}`)
	}
	b.WriteString(`]}`)
	if _, err := decodeJobResultBytes([]byte(b.String())); err == nil {
		t.Error("refused_outputs past the cap were accepted")
	}
}

// A refused_outputs entry naming a server-generated file (any case, or a path ending in one) is
// dropped: those two names are the server's own.
func TestValidateRefusedOutputsDropsReservedNames(t *testing.T) {
	sub, err := validateAndScrubJobResult(workerJobResultRequest{Status: "completed", RefusedOutputs: []workerRefusedOutputBody{
		{DisplayName: "report.md", Reason: "worker_empty"},
		{DisplayName: "outputs/Findings.JSON", Reason: "worker_unreadable"},
		{DisplayName: "keep.txt", Reason: "worker_busy"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.RefusedOutputs) != 1 || sub.RefusedOutputs[0].DisplayName != "keep.txt" {
		t.Fatalf("refused outputs = %+v, want only keep.txt", sub.RefusedOutputs)
	}
}
