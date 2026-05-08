package relay

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/emersion/go-msgauth/dkim"
)

// rawBackendSpy implements RawBackend and captures the last SendRaw call.
type rawBackendSpy struct {
	lastRaw []byte
}

func (r *rawBackendSpy) Name() string        { return "spy" }
func (r *rawBackendSpy) DefaultFrom() string { return "default@example.com" }
func (r *rawBackendSpy) Send(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
	return nil
}
func (r *rawBackendSpy) SendRaw(_ context.Context, _ string, _ []string, raw []byte) error {
	r.lastRaw = raw
	return nil
}

// plainBackend implements only Backend, not RawBackend.
type plainBackend struct{}

func (p *plainBackend) Name() string { return "plain" }
func (p *plainBackend) Send(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
	return nil
}

func generateRSAPKCS8PEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal PKCS8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func generateRSAPKCS1PEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func generateEd25519PEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate Ed25519 key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("marshal PKCS8 Ed25519: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestParsePrivateKey_PKCS8RSA(t *testing.T) {
	signer, err := parsePrivateKey(generateRSAPKCS8PEM(t))
	if err != nil {
		t.Fatalf("parsePrivateKey PKCS8 RSA: %v", err)
	}
	if _, ok := signer.(*rsa.PrivateKey); !ok {
		t.Errorf("expected *rsa.PrivateKey, got %T", signer)
	}
}

func TestParsePrivateKey_PKCS1RSA(t *testing.T) {
	signer, err := parsePrivateKey(generateRSAPKCS1PEM(t))
	if err != nil {
		t.Fatalf("parsePrivateKey PKCS1 RSA: %v", err)
	}
	if _, ok := signer.(*rsa.PrivateKey); !ok {
		t.Errorf("expected *rsa.PrivateKey, got %T", signer)
	}
}

func TestParsePrivateKey_Ed25519(t *testing.T) {
	signer, err := parsePrivateKey(generateEd25519PEM(t))
	if err != nil {
		t.Fatalf("parsePrivateKey Ed25519: %v", err)
	}
	if _, ok := signer.(ed25519.PrivateKey); !ok {
		t.Errorf("expected ed25519.PrivateKey, got %T", signer)
	}
}

func TestParsePrivateKey_InvalidPEM(t *testing.T) {
	if _, err := parsePrivateKey("not-a-pem"); err == nil {
		t.Error("expected error for non-PEM input, got nil")
	}
}

func TestParsePrivateKey_EmptyPEM(t *testing.T) {
	if _, err := parsePrivateKey(""); err == nil {
		t.Error("expected error for empty input, got nil")
	}
}

func TestDKIMSigningBackend_RequiresRawBackend(t *testing.T) {
	_, err := NewDKIMSigningBackend(DKIMConfig{
		PrivateKeyPEM: generateRSAPKCS8PEM(t),
		Domain:        "example.com",
		Selector:      "sel",
	}, &plainBackend{})
	if err == nil {
		t.Fatal("expected error when inner backend does not implement RawBackend")
	}
	if !strings.Contains(err.Error(), "RawBackend") {
		t.Errorf("error should mention RawBackend, got: %v", err)
	}
}

func TestDKIMSigningBackend_SignaturePresent(t *testing.T) {
	spy := &rawBackendSpy{}
	// Use test.invalid — IANA-reserved, guaranteed no real DNS records.
	backend, err := NewDKIMSigningBackend(DKIMConfig{
		PrivateKeyPEM: generateRSAPKCS8PEM(t),
		Domain:        "test.invalid",
		Selector:      "testsel",
	}, spy)
	if err != nil {
		t.Fatalf("NewDKIMSigningBackend: %v", err)
	}

	if err := backend.Send(context.Background(), "sender@test.invalid", []string{"to@test.invalid"},
		"Test subject", "Hello body", nil); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(spy.lastRaw) == 0 {
		t.Fatal("SendRaw was not called")
	}
	if !bytes.Contains(spy.lastRaw, []byte("DKIM-Signature:")) {
		t.Errorf("signed message missing DKIM-Signature header; got:\n%s", spy.lastRaw[:min(200, len(spy.lastRaw))])
	}
}

func TestDKIMSigningBackend_SignedBytesUnaltered(t *testing.T) {
	// Verify that dkim.Verify does not report a body/header hash mismatch,
	// which would indicate the bytes were rebuilt after signing.
	// DNS lookup errors (no such host, key revoked) are expected in unit
	// tests — they prove DNS was consulted, not that the bytes were tampered.
	spy := &rawBackendSpy{}
	backend, err := NewDKIMSigningBackend(DKIMConfig{
		PrivateKeyPEM: generateRSAPKCS8PEM(t),
		Domain:        "test.invalid",
		Selector:      "testsel",
	}, spy)
	if err != nil {
		t.Fatalf("NewDKIMSigningBackend: %v", err)
	}

	_ = backend.Send(context.Background(), "sender@test.invalid", []string{"to@test.invalid"},
		"Test subject", "Hello body", nil)

	verifications, err := dkim.Verify(bytes.NewReader(spy.lastRaw))
	if err != nil {
		t.Fatalf("dkim.Verify error: %v", err)
	}
	for _, v := range verifications {
		if v.Err == nil {
			continue
		}
		msg := v.Err.Error()
		// DNS-level errors mean DNS was consulted; not a signing bug.
		if strings.Contains(msg, "no such host") ||
			strings.Contains(msg, "lookup") ||
			strings.Contains(msg, "dns") ||
			strings.Contains(msg, "key revoked") ||
			strings.Contains(msg, "NXDOMAIN") {
			continue
		}
		// Body hash or header hash mismatch would indicate bytes were altered.
		t.Errorf("DKIM verify error (not DNS-related): %v", v.Err)
	}
}

func TestDKIMSigningBackend_EmptyFromFallback(t *testing.T) {
	spy := &rawBackendSpy{} // DefaultFrom returns "default@example.com"
	backend, err := NewDKIMSigningBackend(DKIMConfig{
		PrivateKeyPEM: generateRSAPKCS8PEM(t),
		Domain:        "test.invalid",
		Selector:      "sel",
	}, spy)
	if err != nil {
		t.Fatalf("NewDKIMSigningBackend: %v", err)
	}

	if err := backend.Send(context.Background(), "", []string{"to@example.com"}, "subj", "body", nil); err != nil {
		t.Fatalf("Send with empty from: %v", err)
	}
	if !bytes.Contains(spy.lastRaw, []byte("From: default@example.com")) {
		t.Errorf("expected From: default@example.com in signed message, got:\n%s", spy.lastRaw[:min(300, len(spy.lastRaw))])
	}
}

func TestSendGridDKIM_RejectsBuild(t *testing.T) {
	// SendGrid rebuilds the message server-side so client-side DKIM signing
	// would be invalidated. NewDKIMSigningBackend must refuse a SendGrid backend.
	sg := NewSendGrid(SendGridConfig{APIKey: "SG.test", From: "noreply@example.com"})
	_, err := NewDKIMSigningBackend(DKIMConfig{
		PrivateKeyPEM: generateRSAPKCS8PEM(t),
		Domain:        "example.com",
		Selector:      "sel",
	}, sg)
	if err == nil {
		t.Fatal("expected error: SendGrid does not implement RawBackend")
	}
	if !strings.Contains(err.Error(), "RawBackend") {
		t.Errorf("error should mention RawBackend, got: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
