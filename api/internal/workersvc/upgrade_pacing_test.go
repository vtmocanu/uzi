package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/store"
)

type pacingStore struct {
	Store
	calls []store.UpsertWorkerRollHealthParams
}

func (p *pacingStore) UpsertWorkerRollHealth(_ context.Context, arg store.UpsertWorkerRollHealthParams) (int64, error) {
	p.calls = append(p.calls, arg)
	return 1, nil
}
func TestRecordRollHealthPacingDirectValidation(t *testing.T) {
	reason := "bad\n\u001bpod"
	for _, p := range []*PacingReport{
		{State: "completed"},
		{State: "held", TargetImage: strings.Repeat("x", 513)},
		{State: "held", TargetVersion: strings.Repeat("x", 129)},
		{State: "held", CanaryID: ptrPacing("invalid")},
		{State: "held", BlockingWorkerID: ptrPacing("invalid")},
		{State: "held", BlockingReason: ptrPacing(strings.Repeat("x", 193))},
		{State: "held", RetainedImage: ptrPacing(strings.Repeat("x", 513))},
		{State: "held", RetainedVersion: ptrPacing(strings.Repeat("x", 129))},
	} {
		st := &pacingStore{}
		svc := &Service{q: st}
		if _, err := svc.RecordRollHealth(context.Background(), RollHealthReport{Kind: "fleet", Pacing: p}); err == nil || len(st.calls) != 0 {
			t.Fatalf("invalid direct call accepted: %+v", p)
		}
	}
	st := &pacingStore{}
	svc := &Service{q: st}
	if _, err := svc.RecordRollHealth(context.Background(), RollHealthReport{Kind: "fleet", Pacing: &PacingReport{State: "waiting_evidence", BlockingReason: &reason}, ObservedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if reason != "bad\n\u001bpod" || !strings.Contains(string(st.calls[0].Pacing), `"blocking_reason":"badpod"`) {
		t.Fatalf("sanitization mutated caller or failed: %s", st.calls[0].Pacing)
	}
	if _, err := svc.RecordRollHealth(context.Background(), RollHealthReport{Kind: "unknown"}); err == nil || len(st.calls) != 1 {
		t.Fatal("invalid kind accepted")
	}
}
func TestPacingDisplayFieldsStripControls(t *testing.T) {
	in := &PacingReport{State: "held", TargetImage: "repo:\u001b\u202ev2", TargetVersion: "2\n\u202c.0",
		RetainedImage: ptrPacing("repo:\t\u202eold"), RetainedVersion: ptrPacing("1\r\u202c.0")}
	out, err := NormalizePacingReport(in)
	if err != nil || out.TargetImage != "repo:v2" || out.TargetVersion != "2.0" ||
		*out.RetainedImage != "repo:old" || *out.RetainedVersion != "1.0" {
		t.Fatal("pacing display controls survived validation")
	}
	if in.TargetImage != "repo:\u001b\u202ev2" || *in.RetainedVersion != "1\r\u202c.0" {
		t.Fatal("normalization mutated the input")
	}
}
func ptrPacing(s string) *string { return &s }
