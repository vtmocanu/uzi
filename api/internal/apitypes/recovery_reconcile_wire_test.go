package apitypes

import (
	"encoding/json"
	"testing"
)

func TestRecoveryReconcileWire(t *testing.T) {
	assertTags(t, "RecoveryReconcileRequest", RecoveryReconcileRequest{}, "generation", "source_sha", "coverage_digest", "checksum", "byte_size")
	assertTags(t, "RecoveryReconcileResponse", RecoveryReconcileResponse{}, "run_id", "generation", "capture_id", "outcome")
	assertTags(t, "RecoveryReconcileResponse(full)", RecoveryReconcileResponse{FinalReceipt: &RecoveryFinalDisposition{}, ReleaseEvidence: "publication", Reason: "capture_available"}, "run_id", "generation", "capture_id", "outcome", "final_receipt", "release_evidence", "reason")
	want := `{"run_id":"run","generation":2,"capture_id":"capture","outcome":"accepted","final_receipt":{"kind":"settled","coverage_digest":"empty"},"release_evidence":"publication"}`
	res := RecoveryReconcileResponse{RunID: "run", Generation: 2, CaptureID: "capture", Outcome: "accepted", FinalReceipt: &RecoveryFinalDisposition{Kind: "settled", CoverageDigest: "empty"}, ReleaseEvidence: "publication"}
	got, err := json.Marshal(res)
	if err != nil || string(got) != want {
		t.Fatalf("wire = %s, %v; want %s", got, err, want)
	}
	var roundtrip RecoveryReconcileResponse
	if err := json.Unmarshal(got, &roundtrip); err != nil || roundtrip.FinalReceipt.Kind != "settled" || roundtrip.Generation != 2 {
		t.Fatalf("roundtrip: %+v %v", roundtrip, err)
	}
}
