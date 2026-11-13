package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub/scrubtest"
)

func TestScrubKnownTokensMatchesEveryOffsetReference(t *testing.T) {
	scrubtest.AssertMatchesReference(t, ScrubKnownTokens, snapshotSecretPatterns, "[REDACTED]")
}
