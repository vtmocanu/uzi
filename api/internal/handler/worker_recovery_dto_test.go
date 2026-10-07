package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestWorkerRecoveryRunDTO(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		limit                            int
		count, baseline, used, remaining int32
	}{
		{"zero allowance", 0, 7, 7, 0, 0},
		{"one allowance", 1, 8, 7, 1, 0},
		{"custom allowance", 5, 9, 7, 2, 3},
		{"over limit", 1, 10, 7, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dto := runToDTO(store.Run{WorkerRecoveryEpisode: 2, RequeueCount: tc.count,
				RequeueEpisodeBaseline: tc.baseline}, "normal", 0, 0, 0, dtoTestNow, tc.limit)
			wr := dto.WorkerRecovery
			if wr == nil || wr.Episode != 2 || wr.AutomaticRequeueLimit != tc.limit ||
				wr.EpisodeUsed != tc.used || wr.EpisodeRemaining != int(tc.remaining) || dto.RequeueCount != tc.count {
				t.Fatalf("recovery=%+v lifetime=%d", wr, dto.RequeueCount)
			}
			raw, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			var roundtrip apitypes.RunDTO
			if err := json.Unmarshal(raw, &roundtrip); err != nil {
				t.Fatal(err)
			}
			if roundtrip.WorkerRecovery == nil || *roundtrip.WorkerRecovery != *wr {
				t.Fatalf("roundtrip recovery=%+v want %+v", roundtrip.WorkerRecovery, wr)
			}
		})
	}
	t.Run("legacy omits recovery", func(t *testing.T) {
		dto := runToDTO(store.Run{RequeueCount: 7}, "normal", 0, 0, 0, dtoTestNow, 5)
		raw, err := json.Marshal(dto)
		if err != nil {
			t.Fatal(err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatal(err)
		}
		if _, present := keys["worker_recovery"]; present {
			t.Fatal("legacy recovery should be omitted")
		}
	})
}

func TestWorkerRecoveryHistoricEvidenceDTO(t *testing.T) {
	snapshot := []byte(`{"checkpoint_tip":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","available_capture":true,"publication_uncertain":false,"capture_uncertain":true,"custody_uncertain":false,"unknown":false,"recorded_at":"2026-01-02T03:04:05Z"}`)
	run := store.Run{WorkerRecoveryEpisode: 1, WorkerRecoveryEvidence: snapshot}
	before := runToDTO(run, "normal", 0, 0, 0, dtoTestNow, 1)
	after := runToDTO(run, "normal", 0, 0, 0, dtoTestNow.Add(24*time.Hour), 4)
	a, b := before.WorkerRecovery.Evidence, after.WorkerRecovery.Evidence
	if a == nil || b == nil || a.CheckpointTip == nil || *a.CheckpointTip != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		!a.AvailableCapture || !a.CaptureUncertain || a.Unknown || !a.RecordedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("historic evidence=%+v", a)
	}
	if *a.CheckpointTip != *b.CheckpointTip || a.RecordedAt != b.RecordedAt || a.AvailableCapture != b.AvailableCapture ||
		after.WorkerRecovery.AutomaticRequeueLimit != 4 {
		t.Fatal("historic evidence changed with current time/config")
	}
	for _, raw := range []string{"{broken", `{"available_capture":true,"recorded_at":"invalid"}`, "null", " null "} {
		t.Run(raw, func(t *testing.T) {
			run.WorkerRecoveryEvidence = []byte(raw)
			evidence := runToDTO(run, "normal", 0, 0, 0, dtoTestNow, 1).WorkerRecovery.Evidence
			if evidence == nil || !evidence.Unknown || !evidence.CaptureUncertain ||
				!evidence.PublicationUncertain || !evidence.CustodyUncertain || evidence.AvailableCapture || evidence.CheckpointTip != nil {
				t.Fatalf("malformed snapshot evidence=%+v", evidence)
			}
		})
	}
}
