package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestUsageTailGeoCapableNotAvailableRemainsUnpriced(t *testing.T) {
	for _, model := range []string{"claude-opus-4-8", "claude-opus-5", "claude-sonnet-5-5"} {
		t.Run(model, func(t *testing.T) {
			dto := buildUsageTail(nil, []store.ListRunUsageTailMessagesRow{{
				Model: model, InputTokens: 100, OutputFinal: true,
				InferenceGeo: pgconv.TextOrNull("not_available"),
			}}, false)
			if dto.CostStatus != usageTailCostUnpriced || dto.CostUSD != nil ||
				len(dto.Models) != 1 || dto.Models[0].CostUSD != nil || dto.InputTokens != 100 {
				t.Fatalf("geo-capable tail must retain tokens with unknown cost: %+v", dto)
			}
		})
	}
}
