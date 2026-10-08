package workersvc

import (
	"encoding/hex"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const crossCheckEnvelopeBytes = 2 * 1024 * 1024

func validateCrossCheckContext(title, body string) error {
	if len(title) > 4096 || len(body) > 262144 || !utf8.ValidString(title) || !utf8.ValidString(body) {
		return ErrCrossCheckRefused
	}
	return nil
}

// ValidateCrossCheckClaim validates the decoded kind, independently of transport
// markers. Ordinary claims retain their existing validation and encoding behavior.
func ValidateCrossCheckClaim(payload *ClaimPayload) error {
	if payload == nil {
		return ErrCrossCheckRefused
	}
	if payload.Kind != runkind.CrossCheck {
		return nil
	}
	if err := validateCrossCheckContext(payload.IssueTitle, payload.IssueDescription); err != nil {
		return err
	}
	input := payload.CrossCheck
	if input == nil {
		return ErrCrossCheckRefused
	}
	leadID, err := uuid.Parse(input.LeadRunID)
	if err != nil || leadID == uuid.Nil || input.LeadRunID == payload.RunID || !input.DeadlineAt.After(time.Time{}) {
		return ErrCrossCheckRefused
	}
	digest, err := hex.DecodeString(input.CandidateDigest)
	if err != nil || len(digest) != 32 {
		return ErrCrossCheckRefused
	}
	_, err = crossCheckCandidateInput(store.CrossCheck{
		Stage: input.Stage, Round: input.Round, LeadRunID: leadID,
		PlanMd: pgtype.Text{String: input.PlanMd, Valid: true}, Milestones: input.Milestones,
		RequiredCapabilities: input.RequiredCapabilities, RequiredTools: input.RequiredTools,
		SizeClass:       pgtype.Text{String: input.SizeClass, Valid: true},
		BaseCommit:      pgtype.Text{String: input.BaseCommit, Valid: true},
		PlanningDiff:    pgtype.Text{String: input.PlanningDiff, Valid: true},
		CandidateDigest: digest, DeadlineAt: pgtype.Timestamptz{Time: input.DeadlineAt, Valid: true},
	})
	return err
}

// MarshalCrossCheckClaim returns the exact JSON encoder representation, including
// its newline, for the handler to write unchanged. Errors never include payload
// bytes. The checker limit counts HTML escaping and applies without a marker.
func MarshalCrossCheckClaim(payload *ClaimPayload) ([]byte, error) {
	if err := ValidateCrossCheckClaim(payload); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		if payload.Kind == runkind.CrossCheck {
			return nil, ErrCrossCheckRefused
		}
		return nil, err
	}
	if payload.Kind == runkind.CrossCheck && len(raw) >= crossCheckEnvelopeBytes {
		return nil, ErrCrossCheckRefused
	}
	return append(raw, '\n'), nil
}

// Preserve a successful mint's identity when a later checker guard refuses the
// payload; finishRunClaim must settle that exact capability rather than the snapshot.
func crossCheckAssemblyError(payload *ClaimPayload, err error) error {
	if payload != nil && payload.Secrets.Codex != nil {
		epoch, secret, ok := parseCodexCapability(payload.Secrets.Codex.Capability)
		if ok {
			return &codexMintedClaimError{cause: err, epoch: epoch, hash: hashCodexCapability(secret)}
		}
	}
	return err
}
