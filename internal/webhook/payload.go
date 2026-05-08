package webhook

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rbaliyan/mailbox"
)

// Payload is the JSON body POSTed to an agent's webhook endpoint.
type Payload struct {
	EventType   string            `json:"event_type"`
	MessageID   string            `json:"message_id"`
	ThreadID    string            `json:"thread_id,omitempty"`
	ReplyToID   string            `json:"reply_to_id,omitempty"`
	ExternalID  string            `json:"external_id,omitempty"`
	SenderID    string            `json:"sender_id"`
	RecipientID string            `json:"recipient_id"`
	Subject     string            `json:"subject"`
	Body        string            `json:"body"`
	ReceivedAt  time.Time         `json:"received_at"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// BuildPayload constructs the webhook payload from a received message.
func BuildPayload(recipientID string, msg mailbox.Message, receivedAt time.Time) Payload {
	// Convert map[string]any → map[string]string (string values only).
	var meta map[string]string
	if raw := msg.GetMetadata(); len(raw) > 0 {
		meta = make(map[string]string, len(raw))
		for k, v := range raw {
			if s, ok := v.(string); ok {
				meta[k] = s
			}
		}
	}
	return Payload{
		EventType:   "message.received",
		MessageID:   msg.GetID(),
		ThreadID:    msg.GetThreadID(),
		ReplyToID:   msg.GetReplyToID(),
		ExternalID:  msg.GetExternalID(),
		SenderID:    msg.GetSenderID(),
		RecipientID: recipientID,
		Subject:     msg.GetSubject(),
		Body:        msg.GetBody(),
		ReceivedAt:  receivedAt,
		Metadata:    meta,
	}
}

// Marshal serialises the payload to JSON.
func (p Payload) Marshal() ([]byte, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("webhook: marshal payload: %w", err)
	}
	return b, nil
}
