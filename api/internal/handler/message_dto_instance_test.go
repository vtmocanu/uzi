package handler

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestMessageToDTOInstanceAndLabel pins the NULL half of the PRD #99 wire: a
// pre-migration or lead row (both columns NULL) must marshal as JSON null, never
// as "". The web's lane key is `agent_instance ?? agent ?? "lead"`, and "" is a
// truthy-enough key in that expression's sibling code paths — coercing NULL to ""
// would silently mint an empty-string lane that swallows every lead message.
func TestMessageToDTOInstanceAndLabel(t *testing.T) {
	nullRow := store.RunMessage{Seq: 1, Kind: "text", Agent: pgtype.Text{String: "lead", Valid: true}, Payload: []byte(`{}`)}
	dto := messageToDTO(nullRow)
	if dto.AgentInstance != nil || dto.AgentLabel != nil {
		t.Fatalf("NULL columns must map to nil pointers, got %v / %v", dto.AgentInstance, dto.AgentLabel)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"agent_instance", "agent_label"} {
		v, ok := wire[k]
		if !ok {
			t.Fatalf("%s must always be present on the wire (the CLI + web read it unconditionally)", k)
		}
		if string(v) != "null" {
			t.Fatalf("%s should be JSON null for a lead row, got %s", k, v)
		}
	}

	setRow := store.RunMessage{
		Seq: 2, Kind: "tool_use",
		Agent:         pgtype.Text{String: "coder", Valid: true},
		AgentInstance: pgtype.Text{String: "toolu_A", Valid: true},
		AgentLabel:    pgtype.Text{String: "web gate UX", Valid: true},
		Payload:       []byte(`{}`),
	}
	set := messageToDTO(setRow)
	if set.AgentInstance == nil || *set.AgentInstance != "toolu_A" {
		t.Fatalf("agent_instance should map through, got %v", set.AgentInstance)
	}
	if set.AgentLabel == nil || *set.AgentLabel != "web gate UX" {
		t.Fatalf("agent_label should map through, got %v", set.AgentLabel)
	}
}
