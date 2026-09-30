package workersvc

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestReserveParamsValidateRejectsFormatCharacters: a display name may not carry Unicode format
// (Cf), line-separator (Zl) or paragraph-separator (Zp) characters: bidi overrides and isolates
// that spoof how a name reads, zero-width characters, the BOM, and the separators that break a
// line. Ordinary non-ASCII names stay valid. The escapes are runtime-built from code points so no
// invisible character sits in the source.
func TestReserveParamsValidateRejectsFormatCharacters(t *testing.T) {
	base := ReserveParams{UserID: uuid.New(), Direction: JobFileInput, DeclaredSize: 1}
	for _, r := range []rune{
		0x00AD, 0x061C, 0x200B, 0x200C, 0x200D, 0x200E, 0x200F,
		0x2028, 0x2029, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E,
		0x2060, 0x2064, 0x2066, 0x2067, 0x2068, 0x2069, 0xFEFF, 0xE0001,
	} {
		p := base
		p.DisplayName = "report" + string(r) + "gpj.exe"
		if err := p.validate(); err == nil {
			t.Errorf("U+%04X in a display name was accepted, want ErrJobFileInvalid", r)
		}
	}
	for _, name := range []string{"résumé.txt", "報告.md", "Ünïcödé notes.csv", "a b-c_d.json"} {
		p := base
		p.DisplayName = name
		if err := p.validate(); err != nil {
			t.Errorf("%q refused: %v", name, err)
		}
	}
}

// TestJobFailNoCapableWorkerNamesTheCause: the failure reason a job shows when its ephemeral worker
// can never serve it names both capabilities the claim clause can be missing, not just "jobs".
func TestJobFailNoCapableWorkerNamesTheCause(t *testing.T) {
	for _, want := range []string{"job_runner_v1", "job_files_v1"} {
		if !strings.Contains(jobFailNoCapableWorker, want) {
			t.Errorf("jobFailNoCapableWorker = %q, want it to name %s", jobFailNoCapableWorker, want)
		}
	}
}
