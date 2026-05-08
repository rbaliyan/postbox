package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SendGridConfig configures the SendGrid v3 HTTP API backend.
type SendGridConfig struct {
	// APIKey is the SendGrid API key (starts with "SG.").
	APIKey string
	// From is the verified sender address used when the SendMessage request
	// has no user_id.
	From string
	// Timeout is the per-request HTTP timeout. Zero defaults to 10 seconds.
	Timeout time.Duration
}

// SendGridBackend delivers email via the SendGrid v3 /mail/send HTTP API.
// It sends the message body as text/plain. To send HTML, set a Content-Type
// header in the SendMessage request headers map; the backend passes it through
// as a "content" block of the corresponding MIME type.
type SendGridBackend struct {
	cfg    SendGridConfig
	client *http.Client
}

var _ Backend = (*SendGridBackend)(nil)

// NewSendGrid creates a SendGridBackend from cfg.
func NewSendGrid(cfg SendGridConfig) *SendGridBackend {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}
	return &SendGridBackend{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}

func (b *SendGridBackend) Name() string { return "sendgrid" }

// Send POSTs a message to https://api.sendgrid.com/v3/mail/send.
// from falls back to SendGridConfig.From when the caller passes an empty string.
// Additional headers are forwarded as the SendGrid "headers" map field.
func (b *SendGridBackend) Send(ctx context.Context, from string, to []string, subject, body string, headers map[string]string) error {
	if from == "" {
		from = b.cfg.From
	}

	toAddrs := make([]map[string]string, 0, len(to))
	for _, addr := range to {
		toAddrs = append(toAddrs, map[string]string{"email": addr})
	}

	contentType := "text/plain"
	if ct, ok := headers["Content-Type"]; ok {
		contentType = ct
	}

	payload := map[string]any{
		"from": map[string]string{"email": from},
		"personalizations": []map[string]any{
			{"to": toAddrs},
		},
		"subject": subject,
		"content": []map[string]string{
			{"type": contentType, "value": body},
		},
	}
	// Pass additional headers through to SendGrid.
	if len(headers) > 0 {
		sg := make(map[string]string, len(headers))
		for k, v := range headers {
			if k != "Content-Type" {
				sg[k] = v
			}
		}
		if len(sg) > 0 {
			payload["headers"] = sg
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("sendgrid: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("sendgrid: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+b.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("sendgrid: http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("sendgrid: status %d: %s", resp.StatusCode, errBody)
}
