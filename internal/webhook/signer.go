package webhook

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"
)

// Signer signs outbound webhook payloads with the node's Ed25519 private key.
type Signer struct {
	privKey ed25519.PrivateKey
	keyID   string
}

// NewSigner constructs a Signer. privKey must be a valid 64-byte Ed25519 private key.
func NewSigner(privKey ed25519.PrivateKey, keyID string) (*Signer, error) {
	if len(privKey) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("webhook: signer: invalid private key length %d", len(privKey))
	}
	if keyID == "" {
		keyID = "v1"
	}
	return &Signer{privKey: privKey, keyID: keyID}, nil
}

// SignedHeaders returns the HTTP headers to attach to a webhook POST.
// body is the raw JSON payload bytes.
func (s *Signer) SignedHeaders(body []byte, ts time.Time) map[string]string {
	sig := ed25519.Sign(s.privKey, body)
	return map[string]string{
		"Content-Type":                "application/json",
		"X-Postbox-Signature-Ed25519": base64.StdEncoding.EncodeToString(sig),
		"X-Postbox-Timestamp":         strconv.FormatInt(ts.Unix(), 10),
		"X-Postbox-Key-Id":            s.keyID,
	}
}

// Verify checks a webhook signature. body is the raw JSON payload,
// sigB64 is the value of X-Postbox-Signature-Ed25519, ts is the time
// parsed from X-Postbox-Timestamp, and maxAge is the allowed clock skew
// (5 minutes is a sensible default).
func Verify(pubKey ed25519.PublicKey, body []byte, sigB64 string, ts time.Time, maxAge time.Duration) error {
	age := time.Since(ts)
	if age < 0 {
		age = -age
	}
	if age > maxAge {
		return fmt.Errorf("webhook: verify: timestamp too old (age=%v, max=%v)", age, maxAge)
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return fmt.Errorf("webhook: verify: decode signature: %w", err)
	}
	if !ed25519.Verify(pubKey, body, sig) {
		return fmt.Errorf("webhook: verify: invalid signature")
	}
	return nil
}
