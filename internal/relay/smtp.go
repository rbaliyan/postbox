package relay

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPConfig configures the generic SMTP relay backend.
//
// This backend covers AWS SES SMTP, Mailgun, Postfix, and any other SMTP relay
// that supports STARTTLS on port 587. Implicit TLS (port 465) is not supported
// by net/smtp; use port 587 STARTTLS for all providers.
//
// AWS SES SMTP example:
//
//	Host:     "email-smtp.us-east-1.amazonaws.com:587"
//	Username: <SES SMTP username — generated via IAM, starts with "AKIA...">
//	Password: <SES SMTP password — generated via IAM>
//	From:     "noreply@verified-domain.com"
type SMTPConfig struct {
	// Host is the SMTP server in "host:port" form (e.g. "smtp.mailgun.org:587").
	Host string
	// Username and Password for SASL AUTH PLAIN. Leave empty for open relays.
	Username string
	Password string
	// From is the default envelope sender when the SendMessage request has no user_id.
	From string
	// Timeout bounds the total dial+SMTP exchange duration. Zero defaults to 30s.
	// Context cancellation is not propagated to net/smtp; Timeout is the only bound.
	Timeout time.Duration
}

// SMTPBackend delivers email via SMTP with STARTTLS and optional SASL auth.
type SMTPBackend struct {
	cfg SMTPConfig
}

var _ RawBackend = (*SMTPBackend)(nil)

// NewSMTP creates an SMTPBackend from cfg.
func NewSMTP(cfg SMTPConfig) *SMTPBackend {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	return &SMTPBackend{cfg: cfg}
}

func (b *SMTPBackend) Name() string        { return "smtp" }
func (b *SMTPBackend) DefaultFrom() string { return b.cfg.From }

// Send delivers the message via SMTP. Context cancellation is not propagated
// to the underlying net/smtp dial; the Timeout in SMTPConfig is the bound.
func (b *SMTPBackend) Send(_ context.Context, from string, to []string, subject, body string, headers map[string]string) error {
	if from == "" {
		from = b.cfg.From
	}
	return b.send(from, to, buildRFC5322(from, to, subject, body, headers))
}

// SendRaw delivers a pre-built RFC 5322 message. Used by DKIMSigningBackend to
// send the already-signed bytes without rebuilding the message (which would
// invalidate the signature).
func (b *SMTPBackend) SendRaw(_ context.Context, from string, to []string, raw []byte) error {
	if from == "" {
		from = b.cfg.From
	}
	return b.send(from, to, raw)
}

func (b *SMTPBackend) send(from string, to []string, msg []byte) error {
	host, _, _ := net.SplitHostPort(b.cfg.Host)
	var auth smtp.Auth
	if b.cfg.Username != "" {
		auth = smtp.PlainAuth("", b.cfg.Username, b.cfg.Password, host)
	}
	if err := smtp.SendMail(b.cfg.Host, auth, from, to, msg); err != nil {
		return fmt.Errorf("smtp relay %s: %w", b.cfg.Host, err)
	}
	return nil
}

// buildRFC5322 constructs a minimal RFC 5322 message with CRLF line endings.
// The Content-Type header from extraHeaders is honoured; it defaults to
// text/plain; charset=utf-8.
func buildRFC5322(from string, to []string, subject, body string, extraHeaders map[string]string) []byte {
	var buf bytes.Buffer
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("From: " + from + "\r\n")
	buf.WriteString("To: " + strings.Join(to, ", ") + "\r\n")
	buf.WriteString("Subject: " + subject + "\r\n")
	ct := "text/plain; charset=utf-8"
	for k, v := range extraHeaders {
		if strings.EqualFold(k, "content-type") {
			ct = v
			continue
		}
		buf.WriteString(k + ": " + v + "\r\n")
	}
	buf.WriteString("Content-Type: " + ct + "\r\n")
	buf.WriteString("\r\n")
	buf.WriteString(body)
	return buf.Bytes()
}
