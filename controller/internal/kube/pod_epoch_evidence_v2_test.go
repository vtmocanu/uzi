package kube

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPodEpochContractCorpusV2(t *testing.T) {
	for _, root := range []struct {
		dir   string
		valid bool
	}{{"valid", true}, {"invalid", false}} {
		files, err := filepath.Glob("../../../fixtures/pod-epoch/v2/" + root.dir + "/*.json")
		if err != nil || len(files) == 0 {
			t.Fatal("missing v2 corpus", err)
		}
		for _, file := range files {
			t.Run(root.dir+"/"+filepath.Base(file), func(t *testing.T) {
				b, err := os.ReadFile(file) //nolint:gosec // Paths come only from Glob in the fixed repository contract corpus.
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = DecodePodEpochDocument(b)
				if (err == nil) != root.valid {
					t.Fatalf("valid=%v err=%v", root.valid, err)
				}
			})
		}
	}
}

func TestPodEpochEvidenceV2Fields(t *testing.T) {
	b, err := os.ReadFile("../../../fixtures/pod-epoch/v2/positive.json")
	if err != nil {
		t.Fatal(err)
	}
	e, n, err := DecodePodEpochDocument(b)
	if err != nil || n != nil || e == nil {
		t.Fatal(e, n, err)
	}
	if e.Version != 2 {
		t.Fatal(e.Version)
	}
	want := []PodEpochSuccessorContainer{
		{Name: "seed", ContainerID: "containerd://seed2"},
		{Name: "worker", ContainerID: "containerd://worker2"},
	}
	if !reflect.DeepEqual(e.SuccessorContainers, want) {
		t.Fatal(e.SuccessorContainers)
	}
	if e.Attestation == nil || *e.Attestation != (PodEpochAttestation{Result: "consistent", Reason: "observations_consistent"}) {
		t.Fatal(e.Attestation)
	}
	// A valid v2 document survives an encode round trip byte for byte in meaning.
	out, err := EncodePodEpochEvidence(*e)
	if err != nil {
		t.Fatal(err)
	}
	again, _, err := DecodePodEpochDocument(out)
	if err != nil || !reflect.DeepEqual(again, e) {
		t.Fatal(again, err)
	}

	for _, change := range []struct {
		name string
		fn   func(*PodEpochEvidence)
	}{
		{"remove-successor-containers", func(e *PodEpochEvidence) { e.SuccessorContainers = nil }},
		{"remove-attestation", func(e *PodEpochEvidence) { e.Attestation = nil }},
		{"remove-both", func(e *PodEpochEvidence) { e.SuccessorContainers, e.Attestation = nil, nil }},
		{"version-1-with-fields", func(e *PodEpochEvidence) { e.Version = 1 }},
		{"duplicate-successor-name", func(e *PodEpochEvidence) {
			e.SuccessorContainers = append(e.SuccessorContainers, e.SuccessorContainers[0])
		}},
		{"blank-successor-id", func(e *PodEpochEvidence) {
			e.SuccessorContainers = []PodEpochSuccessorContainer{{Name: "worker"}}
		}},
		{"bad-result", func(e *PodEpochEvidence) { e.Attestation = &PodEpochAttestation{Result: "ok", Reason: "r"} }},
		{"blank-reason", func(e *PodEpochEvidence) {
			e.Attestation = &PodEpochAttestation{Result: "consistent", Reason: " \t"}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			copy := *e
			change.fn(&copy)
			if _, err := EncodePodEpochEvidence(copy); err == nil {
				t.Fatal("invalid producer document")
			}
		})
	}
}
