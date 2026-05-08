package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// AntiVirusConfig drives the AntiVirus plugin.
type AntiVirusConfig struct {
	// Endpoint is the URL of the AV scanning service (required).
	Endpoint string
	// Timeout is the HTTP client timeout. Defaults to 30 s when zero.
	// Attachment scanning can be slow; use a generous value.
	Timeout time.Duration
	// FailOpen, when true, allows messages through when the AV endpoint is
	// unreachable or returns an unexpected status. Default (false) rejects them.
	FailOpen bool
}

// avAttachment is an attachment descriptor sent to the AV endpoint.
type avAttachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URI         string `json:"uri"`
}

// avRequest is the JSON body sent to the AV endpoint.
type avRequest struct {
	Sender      string         `json:"sender"`
	Subject     string         `json:"subject"`
	Body        string         `json:"body"`
	Attachments []avAttachment `json:"attachments,omitempty"`
}

// avThreat describes a single detected threat.
type avThreat struct {
	AttachmentID string `json:"attachment_id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
}

// avResponse is the JSON body returned by the AV endpoint.
type avResponse struct {
	Clean   bool       `json:"clean"`
	Threats []avThreat `json:"threats,omitempty"`
}

// AntiVirus is a mailbox.SendHook that calls an external AV scanning service
// before allowing a message to be stored.
type AntiVirus struct {
	name   string
	cfg    AntiVirusConfig
	client *http.Client
	logger *slog.Logger
}

var _ mailbox.Plugin = (*AntiVirus)(nil)
var _ mailbox.SendHook = (*AntiVirus)(nil)

// AntiVirusOption configures an AntiVirus.
type AntiVirusOption func(*AntiVirus)

// WithAntiVirusLogger sets the structured logger.
func WithAntiVirusLogger(l *slog.Logger) AntiVirusOption {
	return func(av *AntiVirus) { av.logger = l }
}

// WithAntiVirusHTTPClient replaces the default HTTP client.
func WithAntiVirusHTTPClient(c *http.Client) AntiVirusOption {
	return func(av *AntiVirus) { av.client = c }
}

// NewAntiVirus creates an AntiVirus plugin.
func NewAntiVirus(name string, cfg AntiVirusConfig, opts ...AntiVirusOption) *AntiVirus {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	av := &AntiVirus{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(av)
	}
	return av
}

func (av *AntiVirus) Name() string                  { return av.name }
func (av *AntiVirus) Init(_ context.Context) error  { return nil }
func (av *AntiVirus) Close(_ context.Context) error { return nil }
func (av *AntiVirus) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend calls the AV scanning endpoint. On threat detection the message
// is rejected with a summary of the detected threats.
func (av *AntiVirus) BeforeSend(ctx context.Context, _ string, draft mbxstore.DraftMessage) error {
	atts := draft.GetAttachments()
	avAtts := make([]avAttachment, 0, len(atts))
	for _, a := range atts {
		avAtts = append(avAtts, avAttachment{
			ID:          a.GetID(),
			Filename:    a.GetFilename(),
			ContentType: a.GetContentType(),
			Size:        a.GetSize(),
			URI:         a.GetURI(),
		})
	}

	req := avRequest{
		Sender:      draft.GetSenderID(),
		Subject:     draft.GetSubject(),
		Body:        draft.GetBody(),
		Attachments: avAtts,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("antivirus: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, av.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("antivirus: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := av.client.Do(httpReq)
	if err != nil {
		av.logger.Warn("antivirus: endpoint unreachable",
			"plugin", av.name, "error", err)
		if av.cfg.FailOpen {
			return nil
		}
		return reject(av.name, "AV scan unavailable")
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		av.logger.Warn("antivirus: unexpected status",
			"plugin", av.name, "status", resp.StatusCode)
		if av.cfg.FailOpen {
			return nil
		}
		return reject(av.name, fmt.Sprintf("AV scan returned status %d", resp.StatusCode))
	}

	const maxResponseBytes = 1 << 20 // 1 MiB
	var avr avResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&avr); err != nil {
		av.logger.Warn("antivirus: decode response failed",
			"plugin", av.name, "error", err)
		if av.cfg.FailOpen {
			return nil
		}
		return reject(av.name, "AV scan response unreadable")
	}

	if avr.Clean {
		return nil
	}

	names := make([]string, 0, len(avr.Threats))
	for _, t := range avr.Threats {
		names = append(names, t.Name)
	}
	return reject(av.name, fmt.Sprintf("threats detected: %s", strings.Join(names, ", ")))
}
