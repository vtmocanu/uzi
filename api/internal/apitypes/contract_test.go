package apitypes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes/apitypestest"
)

// The api ⇄ SPA JSON wire-contract (PRD #982). This is the GO HALF; the vitest
// half is web/src/lib/apiContract.test.ts. Neither reads the other: each side
// checks the SAME recorded fixtures with its OWN production definition (this Go
// struct, the TS type), so a failure names the side that drifted. The precedent
// is fixtures/run-usage — but that pins a fold; this pins the wire SHAPE.
//
// Per DTO two fixtures under fixtures/api-contract/:
//
//	<stem>.zero.json  == json.MarshalIndent(T{})            — nullability surface
//	<stem>.full.json  == json.MarshalIndent(populate(T{}))  — key set + value kinds
//
// The populator (apitypestest.Populate) sets every field non-zero so omitempty
// fields are present, which is what makes the key-set check on the TS side total.
//
// 🔴 THE FIXTURES ARE RECORDED, NOT AUTHORED, AND THERE IS NO -update FLAG. On a
// mismatch this test prints the FULL marshaled JSON so a deliberate wire change
// is a copy-paste into the fixture — a golden any run can rewrite is a snapshot,
// and a snapshot of a regression is green (the fixtures/run-usage house rule).
//
// 🔴 RUN THIS PACKAGE WITH -count=1 AFTER A FIXTURE-ONLY EDIT. fixtures/ sits
// ABOVE api/, so every byte of a fixture is outside this module and contributes
// NOTHING to this package's cache key: a fixture-only edit leaves `go test`
// printing "ok (cached)". The vitest half has no such cache. See the README.
const contractFixtureDir = "../../../fixtures/api-contract"

// contractCase is one DTO's contract row. zero/full are the recorded values;
// decode round-trips full.json back through the struct with DisallowUnknownFields,
// the request-body direction that catches the runtime-400 class the PRD names.
type contractCase struct {
	name   string
	zero   any
	full   any
	decode func([]byte) (any, error)
}

// newContractCase builds a case for DTO type T: the zero value, a fully populated
// value, and a strict decoder. Generic so adding a DTO is one line.
func newContractCase[T any](name string) contractCase {
	var zero T
	var full T
	apitypestest.Populate(&full)
	return contractCase{
		name: name,
		zero: zero,
		full: full,
		decode: func(b []byte) (any, error) {
			var v T
			dec := json.NewDecoder(bytes.NewReader(b))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			return v, nil
		},
	}
}

func contractCases() []contractCase {
	return []contractCase{
		newContractCase[RunDTO]("run"),
		newContractCase[RunListItemDTO]("run_list_item"),
		// M2 — the rest of the apitypes hot set.
		newContractCase[RepoDTO]("repo"),
		newContractCase[MessageDTO]("message"),
		newContractCase[ScheduleDTO]("schedule"),
		// ScheduleRequest is a REQUEST body: its full.json round-trips through
		// DisallowUnknownFields (the runtime-400 class the PRD names) like every other row.
		newContractCase[ScheduleRequest]("schedule_input"),
		newContractCase[WorkerDTO]("worker"),
		newContractCase[AdminWorkerDTO]("admin_worker"),
		newContractCase[UserDTO]("user"),
		newContractCase[AgentMemoryDTO]("agent_memory"),
		newContractCase[SecretDTO]("secret"),
		newContractCase[UsageDTO]("usage"),
		newContractCase[UserSettingsDTO]("user_settings"),
		newContractCase[CatalogEntryDTO]("catalog_entry"),
		newContractCase[AdminCLITokenDTO]("cli_token"),
	}
}

// readContractFixture is fatal on any failure, never a skip. A skipped contract
// check is indistinguishable from a passing one (the false-green shape this repo
// documents repeatedly), so a missing or unreadable fixture must FAIL loudly.
func readContractFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(contractFixtureDir, name)) //nolint:gosec // G304: test reads a fixture from the fixed contractFixtureDir path
	if err != nil {
		t.Fatalf("fixture unreadable: %s: %v -- this contract asserts nothing without it, "+
			"and skipping would look identical to passing", name, err)
	}
	return b
}

func mustMarshalIndent(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	return b
}

// assertFixtureEqual compares a freshly marshaled value against the recorded
// fixture byte-for-byte and, on mismatch, prints the full marshaled JSON so
// re-recording is a copy-paste.
func assertFixtureEqual(t *testing.T, name string, got []byte) {
	t.Helper()
	want := readContractFixture(t, name)
	if !bytes.Equal(bytes.TrimRight(want, "\n"), bytes.TrimRight(got, "\n")) {
		t.Errorf("fixture %s is stale -- re-record it from this exact output "+
			"(recorded, not authored; there is no -update flag):\n%s", name, got)
	}
}

// TestContractFixturesMatchMarshal is assertion (a): the zero and populated
// values must marshal byte-equal to the recorded fixtures.
func TestContractFixturesMatchMarshal(t *testing.T) {
	for _, c := range contractCases() {
		t.Run(c.name, func(t *testing.T) {
			assertFixtureEqual(t, c.name+".zero.json", mustMarshalIndent(t, c.zero))
			assertFixtureEqual(t, c.name+".full.json", mustMarshalIndent(t, c.full))
		})
	}
}

// TestContractFullFixtureDecodesStrict is assertion (b): full.json must decode
// into the struct with DisallowUnknownFields (a wire key the struct lacks is the
// runtime-400 class) AND re-marshal byte-equal to what it decoded.
func TestContractFullFixtureDecodesStrict(t *testing.T) {
	for _, c := range contractCases() {
		t.Run(c.name, func(t *testing.T) {
			raw := readContractFixture(t, c.name+".full.json")
			v, err := c.decode(raw)
			if err != nil {
				t.Fatalf("full.json for %s did not decode with DisallowUnknownFields: %v -- "+
					"a wire key the struct lacks is a runtime 400", c.name, err)
			}
			assertFixtureEqual(t, c.name+".full.json", mustMarshalIndent(t, v))
		})
	}
}
