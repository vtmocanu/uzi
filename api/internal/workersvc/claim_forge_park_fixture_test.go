package workersvc

import (
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/privcheck"
)

// transientGuardBlock is the guard result of a claim whose only blocker is a forge read that failed
// transiently (issue #4689): one protection_unreadable block finding plus the scrubbed network class
// GuardRepo carries out of a failed VerifyToken. It is kept apart from the claim_forge_park_regress
// test so that file compiles against a tree that predates privcheck.GuardResult.ForgeRetryClass.
func transientGuardBlock() privcheck.GuardResult {
	return privcheck.GuardResult{
		Blocked: true,
		Findings: []privcheck.Finding{{
			Code: privcheck.CodeProtectionUnreadable, Severity: privcheck.SeverityBlock,
			Message: "could not verify the bot token against the forge",
		}},
		ForgeRetryClass: forge.ErrorClassNetwork,
	}
}
