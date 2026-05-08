package relay

import (
	"bytes"
	"context"
	"crypto"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/emersion/go-msgauth/dkim"
)

// DKIMConfig holds the DKIM signing parameters for outbound messages.
type DKIMConfig struct {
	// PrivateKeyPEM is a PEM-encoded RSA or Ed25519 private key.
	PrivateKeyPEM string
	// Domain is the signing domain (e.g., "example.com").
	Domain string
	// Selector is the DNS selector label (e.g., "postbox").
	// The public key is expected at <selector>._domainkey.<domain>.
	Selector string
}

// DKIMSigningBackend wraps a RawBackend and DKIM-signs each outbound message
// before delivery. Because DKIM signatures are computed over exact wire bytes,
// the inner backend must implement RawBackend so the signed bytes are sent
// verbatim rather than being rebuilt (which would invalidate the signature).
//
// Note: HTTP-based providers like SendGrid rebuild the message server-side and
// are therefore incompatible with client-side DKIM signing. Use an SMTP backend
// with DKIMSigningBackend, or rely on the provider's own DKIM infrastructure.
type DKIMSigningBackend struct {
	inner   RawBackend
	options *dkim.SignOptions
}

var _ Backend = (*DKIMSigningBackend)(nil)

// NewDKIMSigningBackend parses cfg.PrivateKeyPEM and returns a wrapping backend.
// inner must implement RawBackend; returns an error otherwise since DKIM signing
// requires verbatim wire delivery.
func NewDKIMSigningBackend(cfg DKIMConfig, inner Backend) (*DKIMSigningBackend, error) {
	raw, ok := inner.(RawBackend)
	if !ok {
		return nil, fmt.Errorf("dkim: backend %q does not implement RawBackend; DKIM signing requires verbatim wire delivery (use SMTPBackend)", inner.Name())
	}
	signer, err := parsePrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("dkim: parse private key: %w", err)
	}
	opts := &dkim.SignOptions{
		Domain:   cfg.Domain,
		Selector: cfg.Selector,
		Signer:   signer,
	}
	return &DKIMSigningBackend{inner: raw, options: opts}, nil
}

func (b *DKIMSigningBackend) Name() string { return "dkim+" + b.inner.Name() }

// Send signs the RFC 5322 message bytes and delivers them verbatim via the
// inner backend's SendRaw method, preserving the exact bytes the signature
// was computed over.
func (b *DKIMSigningBackend) Send(ctx context.Context, from string, to []string, subject, body string, headers map[string]string) error {
	// Apply the inner backend's default sender before signing so the DKIM
	// signature covers the same From: header the recipient will see.
	if from == "" {
		from = b.inner.DefaultFrom()
	}
	raw := buildRFC5322(from, to, subject, body, headers)

	var signed bytes.Buffer
	if err := dkim.Sign(&signed, bytes.NewReader(raw), b.options); err != nil {
		return fmt.Errorf("dkim: sign: %w", err)
	}
	return b.inner.SendRaw(ctx, from, to, signed.Bytes())
}

// parsePrivateKey decodes a PEM block and returns the key as a crypto.Signer.
// Supports PKCS#8 and PKCS#1 (RSA only) formats.
func parsePrivateKey(pemData string) (crypto.Signer, error) {
	block, _ := pem.Decode([]byte(pemData))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Fall back to PKCS#1 RSA.
		rsa, rsaErr := x509.ParsePKCS1PrivateKey(block.Bytes)
		if rsaErr != nil {
			return nil, fmt.Errorf("PKCS8: %w; PKCS1: %v", err, rsaErr)
		}
		return rsa, nil
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("parsed key does not implement crypto.Signer")
	}
	return signer, nil
}
