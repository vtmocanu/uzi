package recovery

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// The held FINAL wire contract of issue #2545 step B, pinned by fixtures/api-contract/
// recovery_held_final.*. The worker (agent) side pins the same files, so a rename here or there
// fails one side by name. Fixtures sit outside this module: run with -count=1 after editing one.
const heldContractDir = "../../../fixtures/api-contract"

func readHeldContract(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(heldContractDir, name)) //nolint:gosec // G304: reads a fixed repo fixture path, never run data.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeStrict(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
}

// sameJSON compares two documents by value, so key order and whitespace are free.
func sameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("fixture drift:\nfixture: %s\nmarshal: %s", want, got)
	}
}

func TestHeldFinalContractRequest(t *testing.T) {
	raw := readHeldContract(t, "recovery_held_final.request.json")
	var req apitypes.RecoveryReleaseRequest
	decodeStrict(t, raw, &req)
	f := req.FinalDisposition
	if f == nil || f.Kind != FinalKindHeldPublication || f.CaptureID != "" || req.Generation == nil || *req.Generation <= 0 {
		t.Fatalf("request = %+v", req)
	}
	// The example passes the stateless shape gate of releaseHeldPublication.
	if id, err := uuid.Parse(f.PublicationID); err != nil || id.String() != f.PublicationID ||
		!heldShaRe.MatchString(f.SourceSha) || !validCoverageDigest(f.CoverageDigest) {
		t.Fatalf("the fixture's identity is malformed: %+v", f)
	}
	out, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, raw, out)
}

func TestHeldFinalContractResponses(t *testing.T) {
	var ack apitypes.RecoveryReleaseResponse
	raw := readHeldContract(t, "recovery_held_final.ack.json")
	decodeStrict(t, raw, &ack)
	if !ack.Released || ack.HoldsReleased != 1 || ack.Retained || ack.Reason != "" || ack.Generation == nil || *ack.Generation != 3 {
		t.Fatalf("ack = %+v", ack)
	}
	out, _ := json.Marshal(ack)
	sameJSON(t, raw, out)

	var retained apitypes.RecoveryReleaseResponse
	raw = readHeldContract(t, "recovery_held_final.retained.json")
	decodeStrict(t, raw, &retained)
	if retained.Released || retained.HoldsReleased != 0 || !retained.Retained || retained.Reason != HeldReleaseRefMissing {
		t.Fatalf("retained = %+v", retained)
	}
	out, _ = json.Marshal(retained)
	sameJSON(t, raw, out)
}

func TestHeldFinalContractReasons(t *testing.T) {
	var reasons map[string]struct {
		Definitive bool `json:"definitive"`
	}
	decodeStrict(t, readHeldContract(t, "recovery_held_final.reasons.json"), &reasons)
	want := map[string]bool{
		HeldReleaseUnsupported: false,
		HeldReleaseUnavailable: false,
		HeldReleaseRefMissing:  true,
		HeldReleaseRefMismatch: true,
		HeldReleaseNotEligible: true,
	}
	if len(reasons) != len(want) {
		t.Fatalf("reasons = %v, want exactly %v", reasons, want)
	}
	for reason, definitive := range want {
		got, ok := reasons[reason]
		if !ok || got.Definitive != definitive {
			t.Errorf("reason %q = %+v (present %v), want definitive=%v", reason, got, ok, definitive)
		}
	}
}
