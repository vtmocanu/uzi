package workersvc

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/ciartifacts"
)

// Share the Go wire types, including aliases and omissions, with the worker's
// parser. The LiveDB composition separately exercises assembly from stored rows.
func TestCIArtifactsClaimWireContract(t *testing.T) {
	p := ciRepairClaimPayload(t, true)
	p.ClaimGeneration = 7
	p.Repo.ForgeType = "github"
	p.Repo.URL = "https://github.example/g/repo"
	p.Repo.CloneURL = p.Repo.URL + ".git"
	const id = "55555555-5555-5555-5555-555555555555"
	digest, name := ciartifacts.Identity([]byte("shared repair evidence\n"))
	file := ClaimCIRepairFile{ManifestID: id, Name: name, Size: 23, SHA256: digest}
	m, err := (ciartifacts.Manifest{Uninspected: true, Entries: []ciartifacts.Entry{
		{ArtifactID: "9", ArtifactName: "logs", EntryOrdinal: 0, Label: "test.txt", Status: "kept", ManifestID: id, Name: name, Size: file.Size, SHA256: digest},
		{ArtifactID: "10", ArtifactName: "logs-alias", EntryOrdinal: 0, Label: "same.txt", Status: "kept", ManifestID: id, Name: name, Size: file.Size, SHA256: digest},
		{ArtifactID: "10", EntryOrdinal: 1, Label: "image.bin", Status: "skipped", Reason: "binary"},
	}}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	p.CIRepair = &ClaimCIRepair{Manifest: m, InputFiles: []ClaimCIRepairFile{file}}
	got, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const fixture = "testdata/claim_ci_artifacts_wire.json"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(fixture, got, 0644); err != nil { //nolint:gosec // G306: fixed non-sensitive golden fixture.
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatal("artifact claim wire drift: regenerate the shared golden with UPDATE_GOLDEN=1")
	}
}
