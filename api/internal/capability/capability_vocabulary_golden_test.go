package capability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The capability-vocabulary golden (PRD #512 M3).
//
// The v1 vocabulary {docker, jvm} lives in exactly one place — Vocabulary() in
// this package — and the web repo-capability picker mirrors it in
// web/src/lib/capabilityVocabulary.ts. This file is what joins the two: it is the
// producer half (Vocabulary() IS the golden), and web/src/lib/capabilityVocabulary.test.ts
// is the consumer half that `?raw`-imports this same JSON across the module boundary,
// exactly as workerSizes.test.ts pins WORKER_SIZES against hosted_sizes.json.
//
// Regenerate with `UPDATE_GOLDEN=1 go test ./internal/capability/...`.
//
// Like the size golden it catches DEV-TIME drift, never deployment skew: api and web
// are separately-built artifacts, so a build-time gate can only make our own mistake
// loud, not make a rollout mismatch impossible.
const vocabularyGoldenFixture = "testdata/vocabulary.json"

// vocabularyGolden is the fixture's shape. An object rather than a bare array so the
// file can gain a field later without breaking every parser that already reads it —
// the same reasoning as sizeGolden in the hostedsvc contract test.
type vocabularyGolden struct {
	Vocabulary []string `json:"vocabulary"`
}

func TestCapabilityVocabularyGolden(t *testing.T) {
	got, err := json.MarshalIndent(vocabularyGolden{Vocabulary: Vocabulary()}, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(vocabularyGoldenFixture), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(vocabularyGoldenFixture, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Log("golden file updated")
		return
	}

	want, err := os.ReadFile(vocabularyGoldenFixture)
	if err != nil {
		t.Fatalf("read golden (regenerate with UPDATE_GOLDEN=1 go test ./internal/capability/...): %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the capability vocabulary drifted from the cross-module golden.\n"+
			"If the vocabulary deliberately changed, regenerate this golden (UPDATE_GOLDEN=1 go test "+
			"./internal/capability/...) and update web/src/lib/capabilityVocabulary.ts to match.\n"+
			" got:\n%s\nwant:\n%s", got, want)
	}
}
