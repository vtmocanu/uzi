package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// claimResponseMaxBytes is the worker's run/chat claim read cap (agent/src/client.ts
// CLAIM_RESPONSE_MAX_BYTES). It is one figure pinned from both sides by
// fixtures/claims/claim-response-limit.json; to change it, edit the fixture and both constants
// together. See adr/3396-claim-size-contract.md.
//
// Budget derivation (issue #2306): Go encoding/json escapes <, > and & and controls at 6 wire
// bytes per input byte. Stock worst case: completion_pending 18,955,056 (TestSemanticCompletionPendingWireBound);
// plan_md 1 MiB request cap x6 = 6,291,456; skills at the default SKILLS_MAX_PER_RUN=32 x
// SKILL_MAX_BYTES=65,536 x6 = 12,582,912; completion_scope ~2,517,600; issue_description/job prompt
// 256 KiB x6 = 1,572,864; other bounded fields (comments, milestones, resume gate, pr_description,
// secrets, scalars) ~1.0 MB; builtin agent templates ~109 KB. Total ~43.3 MB (~41.3 MiB); 4x
// rounded up is 168 MiB. User/global agent templates (count and size, each only 1 MiB
// request-capped) and admin-raised SKILLS_MAX_PER_RUN / SKILL_MAX_BYTES are not otherwise bounded,
// hence refuseOversizeRunClaim.
const claimResponseMaxBytes = 168 << 20

// errClaimTooLarge marks a run-lane claim whose encoded body exceeds the worker's read cap. It is
// a terminal assembly failure (claimAssemblyOrigin: provisioning_failed).
var errClaimTooLarge = errors.New("claim too large")

// refuseOversizeRunClaim refuses a run-lane claim whose wire body would exceed the worker's cap.
// finishRunClaimTx calls it on the final payload, after the task checkpoint projection.
// cross_check claims are exempt: MarshalCrossCheckClaim already applies a tighter envelope. The
// reason carries sizes only, never claim content.
func (s *Service) refuseOversizeRunClaim(payload *ClaimPayload) error {
	if payload == nil || payload.Kind == runkind.CrossCheck {
		return nil
	}
	limit := claimResponseMaxBytes
	if s.claimSizeLimit > 0 {
		limit = s.claimSizeLimit
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	// httpx.WriteJSON's json.Encoder escapes HTML like json.Marshal and appends one newline.
	wire := len(raw) + 1
	if wire <= limit {
		return nil
	}
	return fmt.Errorf("%w: encoded claim is %d bytes, over the %d-byte worker claim limit; reduce agent templates, or the skill limits (SKILLS_MAX_PER_RUN, SKILL_MAX_BYTES) if raised, or the run's content", errClaimTooLarge, wire, limit)
}
