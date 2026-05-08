package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// SpamCheckerConfig drives the SpamChecker plugin.
type SpamCheckerConfig struct {
	// Endpoint is the URL of the spam-scoring service (required).
	Endpoint string
	// Threshold is the score above which a message is considered spam.
	// 0 means any positive score triggers rejection (or tagging in TagOnly mode).
	Threshold float64
	// TagOnly, when true, annotates the draft with X-Spam-Score and
	// X-Spam-Status headers instead of rejecting the message outright.
	TagOnly bool
	// Timeout is the HTTP client timeout. Defaults to 5 s when zero.
	Timeout time.Duration
}

// spamRequest is the JSON body sent to the spam-scoring endpoint.
// Headers are intentionally omitted to avoid forwarding potentially sensitive
// internal header values to a third-party service.
type spamRequest struct {
	Sender     string   `json:"sender"`
	Recipients []string `json:"recipients"`
	Subject    string   `json:"subject"`
	Body       string   `json:"body"`
}

// spamResponse is the JSON body returned by the spam-scoring endpoint.
type spamResponse struct {
	Score  float64 `json:"score"`
	IsSpam bool    `json:"is_spam"`
	Reason string  `json:"reason"`
}

// SpamChecker is a mailbox.SendHook that calls an external HTTP spam-scoring
// service before allowing a message to be stored.
type SpamChecker struct {
	name   string
	cfg    SpamCheckerConfig
	client *http.Client
	logger *slog.Logger
}

var _ mailbox.Plugin = (*SpamChecker)(nil)
var _ mailbox.SendHook = (*SpamChecker)(nil)

// SpamCheckerOption configures a SpamChecker.
type SpamCheckerOption func(*SpamChecker)

// WithSpamCheckerLogger sets the structured logger.
func WithSpamCheckerLogger(l *slog.Logger) SpamCheckerOption {
	return func(s *SpamChecker) { s.logger = l }
}

// WithSpamCheckerHTTPClient replaces the default HTTP client.
func WithSpamCheckerHTTPClient(c *http.Client) SpamCheckerOption {
	return func(s *SpamChecker) { s.client = c }
}

// NewSpamChecker creates a SpamChecker plugin.
func NewSpamChecker(name string, cfg SpamCheckerConfig, opts ...SpamCheckerOption) *SpamChecker {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	s := &SpamChecker{
		name:   name,
		cfg:    cfg,
		client: &http.Client{Timeout: timeout},
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

func (s *SpamChecker) Name() string                  { return s.name }
func (s *SpamChecker) Init(_ context.Context) error  { return nil }
func (s *SpamChecker) Close(_ context.Context) error { return nil }
func (s *SpamChecker) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend calls the spam-scoring endpoint and either rejects the message or
// tags it with X-Spam-* headers depending on TagOnly mode.
func (s *SpamChecker) BeforeSend(ctx context.Context, _ string, draft mbxstore.DraftMessage) error {
	req := spamRequest{
		Sender:     draft.GetSenderID(),
		Recipients: draft.GetRecipientIDs(),
		Subject:    draft.GetSubject(),
		Body:       draft.GetBody(),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("spam checker: marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("spam checker: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(httpReq)
	if err != nil {
		s.logger.Warn("spam checker: endpoint unreachable, skipping check",
			"plugin", s.name, "error", err)
		return nil
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("spam checker: unexpected status, skipping check",
			"plugin", s.name, "status", resp.StatusCode)
		return nil
	}

	const maxResponseBytes = 1 << 20 // 1 MiB
	var sr spamResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&sr); err != nil {
		s.logger.Warn("spam checker: decode response failed, skipping check",
			"plugin", s.name, "error", err)
		return nil
	}

	isSpam := sr.IsSpam || sr.Score > s.cfg.Threshold
	if !isSpam {
		return nil
	}

	if s.cfg.TagOnly {
		draft.SetHeader("X-Spam-Score", fmt.Sprintf("%.2f", sr.Score))
		draft.SetHeader("X-Spam-Status", "Yes")
		if sr.Reason != "" {
			draft.SetHeader("X-Spam-Reason", sr.Reason)
		}
		return nil
	}

	reason := sr.Reason
	if reason == "" {
		reason = fmt.Sprintf("score %.2f exceeds threshold %.2f", sr.Score, s.cfg.Threshold)
	}
	return reject(s.name, reason)
}
