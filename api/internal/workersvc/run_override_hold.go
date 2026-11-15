package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// errRunOverrideUnavailable is the sentinel a claim-assembly refusal of a run_override model or
// effort matches (PRD #3250). It is its own non-terminal classification beside errCredentialDisabled:
// finishRunClaim parks the claim on HoldReasonRunOverrideUnavailable instead of failing or requeueing.
var errRunOverrideUnavailable = errors.New("run override unavailable")

// runOverrideUnavailableError carries the refused setting so the park's feed message can name it.
type runOverrideUnavailableError struct {
	Setting string // "model" or "effort"
	Value   string
	Harness Harness
}

func (e *runOverrideUnavailableError) Error() string {
	return fmt.Sprintf("%s: %s %q cannot be honored on the %s harness", errRunOverrideUnavailable, e.Setting, e.Value, e.Harness)
}

func (e *runOverrideUnavailableError) Is(target error) bool {
	return target == errRunOverrideUnavailable
}

// runOverrideUnavailableKind is the run_messages kind of the park's single feed line.
const runOverrideUnavailableKind = "run_override_unavailable"

// runOverrideUnavailableNote is the owner-facing sentence of the park message.
func runOverrideUnavailableNote(setting string) string {
	if setting == "effort" {
		return "the effort chosen for this run is not one of low, medium, high, xhigh, max, so the run was held and not started; cancel it and start a new run"
	}
	return "the model chosen for this run cannot be served by the run's harness, so the run was held and not started; cancel it and start a new run with a different model or harness"
}

// runOverrideUnavailablePayload is the nonsecret {setting, value, harness, note} feed payload.
func runOverrideUnavailablePayload(e *runOverrideUnavailableError) ([]byte, error) {
	return json.Marshal(struct {
		Setting string `json:"setting"`
		Value   string `json:"value"`
		Harness string `json:"harness"`
		Note    string `json:"note"`
	}{Setting: e.Setting, Value: e.Value, Harness: string(e.Harness), Note: runOverrideUnavailableNote(strings.TrimSpace(e.Setting))})
}
