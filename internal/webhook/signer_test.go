package webhook_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/webhook"
)

func generateTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func TestNewSigner_ValidKey(t *testing.T) {
	_, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "mykey")
	if err != nil {
		t.Fatalf("expected no error; got %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil Signer")
	}
}

func TestNewSigner_WrongLength(t *testing.T) {
	_, err := webhook.NewSigner(ed25519.PrivateKey([]byte("tooshort")), "k1")
	if err == nil {
		t.Fatal("expected error for wrong-length private key")
	}
}

func TestNewSigner_EmptyKeyIDDefaultsToV1(t *testing.T) {
	_, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := []byte(`{"hello":"world"}`)
	headers := s.SignedHeaders(body, time.Now())
	if headers["X-Postbox-Key-Id"] != "v1" {
		t.Errorf("expected default key ID 'v1'; got %q", headers["X-Postbox-Key-Id"])
	}
}

func TestSignedHeaders_AllPresent(t *testing.T) {
	_, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "test-key")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"event":"test"}`)
	ts := time.Now()
	headers := s.SignedHeaders(body, ts)

	required := []string{
		"Content-Type",
		"X-Postbox-Signature-Ed25519",
		"X-Postbox-Timestamp",
		"X-Postbox-Key-Id",
	}
	for _, h := range required {
		v, ok := headers[h]
		if !ok {
			t.Errorf("missing header %q", h)
		}
		if v == "" {
			t.Errorf("header %q is empty", h)
		}
	}
	if headers["Content-Type"] != "application/json" {
		t.Errorf("unexpected Content-Type: %q", headers["Content-Type"])
	}
}

func TestVerify_ValidSignature(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"msg":"hello"}`)
	ts := time.Now()
	headers := s.SignedHeaders(body, ts)
	sigB64 := headers["X-Postbox-Signature-Ed25519"]

	if err := webhook.Verify(pub, body, sigB64, ts, 5*time.Minute); err != nil {
		t.Fatalf("Verify failed for valid signature: %v", err)
	}
}

func TestVerify_ExpiredTimestamp(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"msg":"hello"}`)
	// Timestamp is 10 minutes in the past.
	oldTS := time.Now().Add(-10 * time.Minute)
	headers := s.SignedHeaders(body, oldTS)
	sigB64 := headers["X-Postbox-Signature-Ed25519"]

	err = webhook.Verify(pub, body, sigB64, oldTS, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for expired timestamp")
	}
}

func TestVerify_TamperedBody(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	originalBody := []byte(`{"msg":"hello"}`)
	ts := time.Now()
	headers := s.SignedHeaders(originalBody, ts)
	sigB64 := headers["X-Postbox-Signature-Ed25519"]

	tamperedBody := []byte(`{"msg":"tampered"}`)
	err = webhook.Verify(pub, tamperedBody, sigB64, ts, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for tampered body")
	}
}

func TestVerify_InvalidBase64Signature(t *testing.T) {
	pub, _ := generateTestKeyPair(t)
	body := []byte(`{"msg":"hello"}`)
	ts := time.Now()

	err := webhook.Verify(pub, body, "not-valid-base64!!!", ts, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error for invalid base64 signature")
	}
}

func TestVerify_WrongKey(t *testing.T) {
	_, priv1 := generateTestKeyPair(t)
	pub2, _ := generateTestKeyPair(t) // different key pair

	s, err := webhook.NewSigner(priv1, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"msg":"hello"}`)
	ts := time.Now()
	headers := s.SignedHeaders(body, ts)
	sigB64 := headers["X-Postbox-Signature-Ed25519"]

	err = webhook.Verify(pub2, body, sigB64, ts, 5*time.Minute)
	if err == nil {
		t.Fatal("expected error when verifying with wrong public key")
	}
}

func TestVerify_FutureTimestampWithinMax(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"msg":"hello"}`)
	// Timestamp 30 seconds in the future — within a 5-minute window.
	futureTS := time.Now().Add(30 * time.Second)
	headers := s.SignedHeaders(body, futureTS)
	sigB64 := headers["X-Postbox-Signature-Ed25519"]

	if err := webhook.Verify(pub, body, sigB64, futureTS, 5*time.Minute); err != nil {
		t.Fatalf("Verify failed for slightly-future timestamp within max age: %v", err)
	}
}

// TestSignedHeaders_SignatureDecodesAndVerifiesManually ensures the signature
// is valid raw ed25519 over the body.
func TestSignedHeaders_SignatureDecodesAndVerifiesManually(t *testing.T) {
	pub, priv := generateTestKeyPair(t)
	s, err := webhook.NewSigner(priv, "k1")
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}

	body := []byte(`{"x":1}`)
	headers := s.SignedHeaders(body, time.Now())
	sigRaw, err := base64.StdEncoding.DecodeString(headers["X-Postbox-Signature-Ed25519"])
	if err != nil {
		t.Fatalf("decode signature: %v", err)
	}
	if !ed25519.Verify(pub, body, sigRaw) {
		t.Fatal("manual verification failed")
	}
}
