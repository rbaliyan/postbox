// Package nodekey manages the Ed25519 key pair for a Postbox node.
// The private key is persisted in the store so it survives restarts.
package nodekey

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/rbaliyan/postbox/internal/store"
)

// EnsureKey checks whether node.PrivateKeyB64 is populated and, if not,
// generates a fresh Ed25519 key pair, persists it, and returns the updated node.
func EnsureKey(ctx context.Context, s store.Store, node store.Node) (store.Node, error) {
	if node.PrivateKeyB64 != "" {
		return node, nil
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return node, fmt.Errorf("nodekey: generate: %w", err)
	}
	node.PrivateKeyB64 = base64.StdEncoding.EncodeToString(priv)
	node.PublicKeyB64 = base64.StdEncoding.EncodeToString(pub)
	if err := s.SaveNode(ctx, node); err != nil {
		return node, fmt.Errorf("nodekey: persist: %w", err)
	}
	return node, nil
}

// DecodePrivate decodes a base64-encoded Ed25519 private key.
func DecodePrivate(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("nodekey: decode private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("nodekey: private key: got %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// DecodePublic decodes a base64-encoded Ed25519 public key.
func DecodePublic(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("nodekey: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("nodekey: public key: got %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// Generate creates a fresh key pair and returns (privateB64, publicB64).
// Used by the CLI keygen command to generate agent key pairs locally.
func Generate() (privateB64, publicB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("nodekey: generate: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv),
		base64.StdEncoding.EncodeToString(pub),
		nil
}
