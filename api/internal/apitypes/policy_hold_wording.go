package apitypes

// Owner-facing wording for a Codex policy hold (issue #3607). The tables are copied verbatim from
// fixtures/policy-hold/wording.json, the single source the web copy is pinned to as well;
// TestPolicyHoldWordingMatchesFixture fails when either side drifts.

// Policy resume modes accepted by POST /api/runs/{id}/resume-now for a policy hold.
const (
	PolicyResumePreserve           = "preserve"
	PolicyResumeFreshFromSavedWork = "fresh_from_saved_work"
	PolicyResumeFreshOneClaim      = "fresh_one_claim"
)

// PolicyResumeModes is the closed set of resume modes, in presentation order.
var PolicyResumeModes = []string{PolicyResumePreserve, PolicyResumeFreshFromSavedWork, PolicyResumeFreshOneClaim}

const policyHoldUnknownCauseText = "held (cause not recorded)"

var policyHoldCauseText = map[string]string{
	"refused":                  "The provider refused a request under its usage policy.",
	"attempt_path":             "This pickup cannot continue the old conversation on this lane; continuing needs a fresh conversation from the saved work.",
	"saved_work_missing":       "The required saved work was not found where this pickup ran; nothing was started.",
	"session_missing":          "The saved conversation is not available where this pickup ran; nothing was started.",
	"transcript_unknown":       "The saved conversation's transcript could not be confirmed where this pickup ran; nothing was started.",
	"provider_resume_rejected": "The provider rejected resuming the saved conversation.",
	"pending_conversion":       "An earlier refusal recorded on the worker was converted into this hold.",
	"fresh_start_uncertain":    "A one-claim fresh start was used but never confirmed; a provider conversation may already exist.",
}

var policyResumeModeLabel = map[string]string{
	PolicyResumePreserve:           "Resume the preserved conversation",
	PolicyResumeFreshFromSavedWork: "Fresh conversation from the saved work",
	PolicyResumeFreshOneClaim:      "Fresh conversation for one pickup",
}

var policyResumeModeConsequence = map[string]string{
	PolicyResumePreserve:           "Continues the same conversation with your guidance. If the conversation is missing or rejected, the run is held again; it never starts a new conversation.",
	PolicyResumeFreshFromSavedWork: "Starts a fresh conversation from the saved work and resolves this refusal. Any earlier requirement to preserve the old conversation ends for the rest of the run, until a new refusal.",
	PolicyResumeFreshOneClaim:      "Lets exactly the next pickup start one new conversation from the saved work. Later pickups preserve that new conversation again; a fresh start that is never confirmed is held, not repeated.",
}

// PolicyHoldCauseText returns the owner-facing sentence for a hold_cause, or the unknown-cause
// text for nil and for any value this build does not know.
func PolicyHoldCauseText(cause *string) string {
	if cause != nil {
		if s, ok := policyHoldCauseText[*cause]; ok {
			return s
		}
	}
	return policyHoldUnknownCauseText
}

// PolicyResumeModeLabel returns the owner-facing label for a resume mode, "" when unknown.
func PolicyResumeModeLabel(mode string) string { return policyResumeModeLabel[mode] }

// PolicyResumeModeConsequence returns what choosing a resume mode does, "" when unknown.
func PolicyResumeModeConsequence(mode string) string { return policyResumeModeConsequence[mode] }
