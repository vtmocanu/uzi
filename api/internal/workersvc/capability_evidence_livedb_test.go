package workersvc

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCapabilityEvidenceStateIdentityLiveDB(t *testing.T) {
	for _, state := range []string{"awaiting_approval", "running"} {
		t.Run(state, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			wid := e.seedWorker(t, nil)
			rid := e.seedActiveRunOwnedBy(t, wid)
			e.exec(t, `UPDATE runs SET claim_generation=1,auto_approve=$2,hinted_capabilities='{}',ignored_inferred_capabilities='{jvm}' WHERE id=$1`, rid, state == "running")
			svc := New(e.q, nil, testParams())
			svc.SetTxBeginner(e.pool)
			worker := store.Worker{ID: wid, UserID: e.userID}
			plan, size := "plan", "s"
			caps := []string{"jvm"}
			id := uuid.New()
			metadata := func(p string) json.RawMessage {
				raw, err := json.Marshal(map[string]any{"jvm": apitypes.CapabilityInferenceObservation{Triggers: []apitypes.CapabilityTrigger{{Path: p, Reason: "jvm_manifest"}}}})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			req := StateRequest{State: state, ClaimGeneration: i64(1), PlanMd: &plan, SizeClass: &size, RequiredCapabilities: &caps, CapabilityInferenceEvidence: metadata("a/pom.xml")}
			if state == "awaiting_approval" {
				req.PresentationID = &id
			}
			_, applied, revision, err := svc.SetStateReport(e.ctx, worker, rid, req)
			if err != nil || !applied {
				t.Fatalf("first report: applied=%v err=%v", applied, err)
			}
			read := func(want string) store.Run {
				row, err := e.q.GetRunByID(e.ctx, rid)
				if err != nil {
					t.Fatal(err)
				}
				var evidence map[string]apitypes.CapabilityInferenceObservation
				if err := json.Unmarshal(row.CapabilityInferenceEvidence, &evidence); err != nil {
					t.Fatal(err)
				}
				if want == "" {
					if len(evidence) != 0 {
						t.Fatal(evidence)
					}
				} else if len(evidence["jvm"].Triggers) != 1 || evidence["jvm"].Triggers[0].Path != want {
					t.Fatalf("evidence=%+v want=%s", evidence, want)
				}
				if len(row.RequiredCapabilities) != 0 {
					t.Fatalf("advisory metadata restored ignored requirement: %v", row.RequiredCapabilities)
				}
				return row
			}
			first := read("a/pom.xml")
			digest := bytes.Clone(first.GatePayloadDigest)
			req.CapabilityInferenceEvidence = metadata("b/pom.xml")
			_, applied, next, err := svc.SetStateReport(e.ctx, worker, rid, req)
			if err != nil || !applied || next != revision {
				t.Fatalf("metadata regated: applied=%v revision=%d want=%d err=%v", applied, next, revision, err)
			}
			if !bytes.Equal(read("b/pom.xml").GatePayloadDigest, digest) {
				t.Fatal("metadata entered gate identity")
			}
			req.CapabilityInferenceEvidence = nil
			if _, applied, _, err := svc.SetStateReport(e.ctx, worker, rid, req); err != nil || !applied {
				t.Fatal(err)
			}
			read("b/pom.xml")
			bad := req
			bad.ClaimGeneration = i64(2)
			bad.CapabilityInferenceEvidence = metadata("stale/pom.xml")
			if _, applied, _, err := svc.SetStateReport(e.ctx, worker, rid, bad); !errors.Is(err, ErrStaleClaim) || applied {
				t.Fatalf("stale write: %v %v", applied, err)
			}
			read("b/pom.xml")
			changed := "different plan"
			bad = req
			bad.PlanMd = &changed
			bad.CapabilityInferenceEvidence = metadata("refused/pom.xml")
			if _, applied, _, err := svc.SetStateReport(e.ctx, worker, rid, bad); err == nil || applied {
				t.Fatalf("refused write accepted: %v %v", applied, err)
			}
			read("b/pom.xml")
			req.CapabilityInferenceEvidence = json.RawMessage(`{}`)
			if _, applied, _, err := svc.SetStateReport(e.ctx, worker, rid, req); err != nil || !applied {
				t.Fatal(err)
			}
			read("")
		})
	}
}

func TestCapabilityEvidenceSemanticIdentityLiveDB(t *testing.T) {
	f := newSemanticGateLive(t)
	req := gateReportRequest(t, f)
	req.CapabilityInferenceEvidence = json.RawMessage(`{"docker":{"triggers":[{"path":"Dockerfile","reason":"docker_marker"}],"truncated":false}}`)
	first := presentSemanticGate(t, f, req)
	digest := f.candidate.Digest
	req.CapabilityInferenceEvidence = json.RawMessage(`{"docker":{"triggers":[{"path":"ops/Dockerfile","reason":"docker_marker"}],"truncated":false}}`)
	second := presentSemanticGate(t, f, req)
	if !sameSemanticJSON(first, second) || f.candidate.Digest != digest {
		t.Fatal("advisory paths changed semantic identity")
	}
	row, err := f.e.q.GetRunByID(f.e.ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(row.CapabilityInferenceEvidence, []byte("ops/Dockerfile")) {
		t.Fatal("accepted evidence was not replaced")
	}
}
