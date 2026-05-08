package webhook_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rbaliyan/mailbox"
	mailboxstore "github.com/rbaliyan/mailbox/store"

	"github.com/rbaliyan/postbox/internal/webhook"
)

// fakeMessage is the shared minimal mailbox.Message implementation used by
// both payload_test.go and dispatcher_test.go. dispatcher_test.go refers to
// this type directly (see the comment at the top of that file).
type fakeMessage struct {
	id           string
	ownerID      string
	senderID     string
	subject      string
	body         string
	recipientIDs []string
	threadID     string
	replyToID    string
	externalID   string
	metadata     map[string]any
}

// Ensure fakeMessage satisfies mailbox.Message at compile time.
var _ mailbox.Message = (*fakeMessage)(nil)

// --- store.MessageReader methods ---

func (m *fakeMessage) GetID() string                             { return m.id }
func (m *fakeMessage) GetOwnerID() string                        { return m.ownerID }
func (m *fakeMessage) GetSenderID() string                       { return m.senderID }
func (m *fakeMessage) GetSubject() string                        { return m.subject }
func (m *fakeMessage) GetBody() string                           { return m.body }
func (m *fakeMessage) GetRecipientIDs() []string                 { return m.recipientIDs }
func (m *fakeMessage) GetHeaders() map[string]string             { return nil }
func (m *fakeMessage) GetMetadata() map[string]any               { return m.metadata }
func (m *fakeMessage) GetAttachments() []mailboxstore.Attachment { return nil }
func (m *fakeMessage) GetCreatedAt() time.Time                   { return time.Time{} }
func (m *fakeMessage) GetUpdatedAt() time.Time                   { return time.Time{} }
func (m *fakeMessage) GetExpiresAt() *time.Time                  { return nil }
func (m *fakeMessage) GetAvailableAt() *time.Time                { return nil }

// --- store.Message extra methods ---

func (m *fakeMessage) GetStatus() mailboxstore.MessageStatus {
	return mailboxstore.MessageStatusDelivered
}
func (m *fakeMessage) GetIsRead() bool       { return false }
func (m *fakeMessage) GetReadAt() *time.Time { return nil }
func (m *fakeMessage) GetFolderID() string   { return mailboxstore.FolderInbox }
func (m *fakeMessage) GetTags() []string     { return nil }
func (m *fakeMessage) GetThreadID() string   { return m.threadID }
func (m *fakeMessage) GetReplyToID() string  { return m.replyToID }
func (m *fakeMessage) GetExternalID() string { return m.externalID }

// --- mailbox.MessageMutator methods (all no-ops) ---

func (m *fakeMessage) Update(_ context.Context, _ mailbox.Flags) error { return nil }
func (m *fakeMessage) Move(_ context.Context, _ string, _ ...mailboxstore.MoveOption) error {
	return nil
}
func (m *fakeMessage) Delete(_ context.Context) error              { return nil }
func (m *fakeMessage) Restore(_ context.Context) error             { return nil }
func (m *fakeMessage) PermanentlyDelete(_ context.Context) error   { return nil }
func (m *fakeMessage) AddTag(_ context.Context, _ string) error    { return nil }
func (m *fakeMessage) RemoveTag(_ context.Context, _ string) error { return nil }

// ---------------------------------------------------------------------------
// BuildPayload tests
// ---------------------------------------------------------------------------

func TestBuildPayload_Fields(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	msg := &fakeMessage{
		id:         "msg-1",
		senderID:   "sender@example.com",
		subject:    "Hello",
		body:       "World",
		threadID:   "thread-1",
		replyToID:  "msg-0",
		externalID: "ext-abc",
	}

	p := webhook.BuildPayload("recipient@example.com", msg, now)

	if p.EventType != "message.received" {
		t.Errorf("EventType: got %q, want %q", p.EventType, "message.received")
	}
	if p.MessageID != "msg-1" {
		t.Errorf("MessageID: got %q, want %q", p.MessageID, "msg-1")
	}
	if p.SenderID != "sender@example.com" {
		t.Errorf("SenderID: got %q", p.SenderID)
	}
	if p.RecipientID != "recipient@example.com" {
		t.Errorf("RecipientID: got %q", p.RecipientID)
	}
	if p.Subject != "Hello" {
		t.Errorf("Subject: got %q", p.Subject)
	}
	if p.Body != "World" {
		t.Errorf("Body: got %q", p.Body)
	}
	if p.ThreadID != "thread-1" {
		t.Errorf("ThreadID: got %q", p.ThreadID)
	}
	if p.ReplyToID != "msg-0" {
		t.Errorf("ReplyToID: got %q", p.ReplyToID)
	}
	if p.ExternalID != "ext-abc" {
		t.Errorf("ExternalID: got %q", p.ExternalID)
	}
	if !p.ReceivedAt.Equal(now) {
		t.Errorf("ReceivedAt: got %v, want %v", p.ReceivedAt, now)
	}
}

func TestBuildPayload_MetadataStringOnly(t *testing.T) {
	msg := &fakeMessage{
		id: "msg-meta",
		metadata: map[string]any{
			"key-str":  "value1",
			"key-int":  42,
			"key-bool": true,
			"key-nil":  nil,
		},
	}

	p := webhook.BuildPayload("r@example.com", msg, time.Now())

	if p.Metadata == nil {
		t.Fatal("expected non-nil metadata")
	}
	if v, ok := p.Metadata["key-str"]; !ok || v != "value1" {
		t.Errorf("expected key-str=value1; got %q", p.Metadata["key-str"])
	}
	if _, ok := p.Metadata["key-int"]; ok {
		t.Error("expected key-int to be dropped (non-string value)")
	}
	if _, ok := p.Metadata["key-bool"]; ok {
		t.Error("expected key-bool to be dropped (non-string value)")
	}
	if _, ok := p.Metadata["key-nil"]; ok {
		t.Error("expected key-nil to be dropped (non-string value)")
	}
}

func TestBuildPayload_NilMetadata(t *testing.T) {
	msg := &fakeMessage{id: "msg-nometa", metadata: nil}
	p := webhook.BuildPayload("r@example.com", msg, time.Now())
	if p.Metadata != nil {
		t.Errorf("expected nil metadata map for nil source; got %v", p.Metadata)
	}
}

// ---------------------------------------------------------------------------
// Payload.Marshal tests
// ---------------------------------------------------------------------------

func TestPayloadMarshal_ValidJSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	msg := &fakeMessage{
		id:       "msg-2",
		senderID: "alice@example.com",
		subject:  "Test",
		body:     "Body text",
	}

	p := webhook.BuildPayload("bob@example.com", msg, now)
	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}

	requiredKeys := []string{
		"event_type", "message_id", "sender_id",
		"recipient_id", "subject", "body", "received_at",
	}
	for _, k := range requiredKeys {
		if _, ok := parsed[k]; !ok {
			t.Errorf("JSON missing key %q", k)
		}
	}
	if parsed["event_type"] != "message.received" {
		t.Errorf("event_type: got %v", parsed["event_type"])
	}
}

func TestPayloadMarshal_RoundTrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	msg := &fakeMessage{
		id:       "rt-msg",
		senderID: "s@x.com",
		subject:  "Re: stuff",
		body:     "...",
		threadID: "t-1",
	}
	p := webhook.BuildPayload("recv@x.com", msg, now)

	data, err := p.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var p2 webhook.Payload
	if err := json.Unmarshal(data, &p2); err != nil {
		t.Fatalf("unmarshal into Payload: %v", err)
	}
	if p2.MessageID != p.MessageID {
		t.Errorf("MessageID mismatch: %q vs %q", p2.MessageID, p.MessageID)
	}
	if p2.ThreadID != p.ThreadID {
		t.Errorf("ThreadID mismatch: %q vs %q", p2.ThreadID, p.ThreadID)
	}
	if p2.RecipientID != p.RecipientID {
		t.Errorf("RecipientID mismatch: %q vs %q", p2.RecipientID, p.RecipientID)
	}
	if !p2.ReceivedAt.Equal(p.ReceivedAt) {
		t.Errorf("ReceivedAt mismatch: %v vs %v", p2.ReceivedAt, p.ReceivedAt)
	}
}
