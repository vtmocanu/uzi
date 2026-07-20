package workersvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// instanceBroadcaster records the full PublishMessage argument list, unlike the
// seq-only fakeBroadcaster in service_test.go — the point here is what the LIVE
// frame carries, which is the only thing that lets the browser lane a message
// without a REST re-read (PRD #99 Decision 6).
type instanceBroadcaster struct {
	instances []string
	labels    []string
}

func (b *instanceBroadcaster) PublishMessage(_ uuid.UUID, _ int32, _, _, agentInstance, agentLabel string, _ []byte, _ time.Time) {
	b.instances = append(b.instances, agentInstance)
	b.labels = append(b.labels, agentLabel)
}
func (b *instanceBroadcaster) PublishState(uuid.UUID, string)                {}
func (b *instanceBroadcaster) PublishHealth(uuid.UUID, string, string, bool) {}
func (b *instanceBroadcaster) PublishInput(uuid.UUID)                        {}

// TestAppendMessagesCarriesInstanceAndLabel proves the two PRD #99 fields survive
// the worker→store hop with the empty-string→SQL-NULL contract intact: a lead
// frame (neither field) must insert pgtype.Text{Valid:false}, NOT a valid empty
// string, because every downstream consumer's role-name fallback keys off Valid.
func TestAppendMessagesCarriesInstanceAndLabel(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"t":"delegating"}`)},
		{Seq: 2, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_A", AgentLabel: "web gate UX", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 3, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_B", AgentLabel: "API wiring", Payload: json.RawMessage(`{"tool":"Edit"}`)},
		{Seq: 4, Kind: "text", Agent: "reviewer", AgentInstance: "toolu_C", Payload: json.RawMessage(`{"t":"no label"}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(fs.insertedMessages) != 4 {
		t.Fatalf("expected 4 inserts, got %d", len(fs.insertedMessages))
	}
	lead := fs.insertedMessages[0]
	if lead.AgentInstance.Valid || lead.AgentLabel.Valid {
		t.Fatalf("a lead frame must insert NULL for both columns, got instance=%+v label=%+v",
			lead.AgentInstance, lead.AgentLabel)
	}
	a, b := fs.insertedMessages[1], fs.insertedMessages[2]
	if a.AgentInstance.String != "toolu_A" || !a.AgentInstance.Valid ||
		b.AgentInstance.String != "toolu_B" || !b.AgentInstance.Valid {
		t.Fatalf("parallel same-role instances must persist distinctly, got %+v / %+v",
			a.AgentInstance, b.AgentInstance)
	}
	if a.AgentLabel.String != "web gate UX" || b.AgentLabel.String != "API wiring" {
		t.Fatalf("labels must persist verbatim, got %q / %q", a.AgentLabel.String, b.AgentLabel.String)
	}
	if noLabel := fs.insertedMessages[3]; !noLabel.AgentInstance.Valid || noLabel.AgentLabel.Valid {
		t.Fatalf("an instance frame with no task_description must persist instance-valid/label-NULL, got %+v / %+v",
			noLabel.AgentInstance, noLabel.AgentLabel)
	}
}

// TestAppendMessagesBroadcastsInstanceAndLabel proves the LIVE frame carries both
// fields. Persisting them is not enough: useRunStream builds its RunMessage from
// the WS frame with no REST re-read, so a live subagent message lands in the
// wrong lane if the broadcast drops the id.
func TestAppendMessagesBroadcastsInstanceAndLabel(t *testing.T) {
	w := worker()
	fs := &fakeStore{runOwned: store.Run{ID: uuid.New(), WorkerID: pgUUID(w.ID), LastSeq: 0}}
	svc := New(fs, newBox(t), testParams())
	b := &instanceBroadcaster{}
	svc.SetBroadcaster(b)

	msgs := []IncomingMessage{
		{Seq: 1, Kind: "text", Agent: "lead", Payload: json.RawMessage(`{"t":"hi"}`)},
		{Seq: 2, Kind: "tool_use", Agent: "coder", AgentInstance: "toolu_A", AgentLabel: "web gate UX", Payload: json.RawMessage(`{"tool":"Edit"}`)},
	}
	if err := svc.AppendMessages(context.Background(), w, fs.runOwned.ID, msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	if len(b.instances) != 2 {
		t.Fatalf("expected 2 broadcasts, got %d", len(b.instances))
	}
	if b.instances[0] != "" || b.labels[0] != "" {
		t.Fatalf("lead frame should broadcast empty instance/label, got %q / %q", b.instances[0], b.labels[0])
	}
	if b.instances[1] != "toolu_A" || b.labels[1] != "web gate UX" {
		t.Fatalf("subagent frame should broadcast its instance + label, got %q / %q", b.instances[1], b.labels[1])
	}
}
