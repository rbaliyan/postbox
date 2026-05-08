package nodekey_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"testing"

	"github.com/rbaliyan/postbox/internal/nodekey"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
)

func TestGenerate_ReturnsNonEmpty(t *testing.T) {
	privB64, pubB64, err := nodekey.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}
	if privB64 == "" {
		t.Fatal("expected non-empty private key base64")
	}
	if pubB64 == "" {
		t.Fatal("expected non-empty public key base64")
	}
}

func TestGenerate_CorrectKeySizes(t *testing.T) {
	privB64, pubB64, err := nodekey.Generate()
	if err != nil {
		t.Fatalf("Generate() error: %v", err)
	}

	privRaw, err := base64.StdEncoding.DecodeString(privB64)
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	if len(privRaw) != ed25519.PrivateKeySize {
		t.Errorf("private key size: got %d, want %d", len(privRaw), ed25519.PrivateKeySize)
	}

	pubRaw, err := base64.StdEncoding.DecodeString(pubB64)
	if err != nil {
		t.Fatalf("decode public key: %v", err)
	}
	if len(pubRaw) != ed25519.PublicKeySize {
		t.Errorf("public key size: got %d, want %d", len(pubRaw), ed25519.PublicKeySize)
	}
}

func TestEnsureKey_AlreadySet_DoesNotSave(t *testing.T) {
	ctx := context.Background()

	// Use a counting store to detect unexpected SaveNode calls.
	ms := &saveCountStore{Store: memstore.New()}

	privB64, pubB64, err := nodekey.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	node := store.Node{
		ID:            "node-1",
		PrivateKeyB64: privB64,
		PublicKeyB64:  pubB64,
	}

	// Save the node first so it exists.
	if err := ms.SaveNode(ctx, node); err != nil {
		t.Fatalf("SaveNode: %v", err)
	}
	saveBefore := ms.saveCount

	result, err := nodekey.EnsureKey(ctx, ms, node)
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if result.PrivateKeyB64 != privB64 {
		t.Errorf("expected unchanged private key")
	}
	if ms.saveCount != saveBefore {
		t.Errorf("expected no additional SaveNode call; got %d extra saves", ms.saveCount-saveBefore)
	}
}

func TestEnsureKey_EmptyPrivateKey_GeneratesAndSaves(t *testing.T) {
	ctx := context.Background()
	ms := memstore.New()

	node := store.Node{ID: "node-2"}

	result, err := nodekey.EnsureKey(ctx, ms, node)
	if err != nil {
		t.Fatalf("EnsureKey: %v", err)
	}
	if result.PrivateKeyB64 == "" {
		t.Fatal("expected private key to be populated")
	}
	if result.PublicKeyB64 == "" {
		t.Fatal("expected public key to be populated")
	}

	// Verify persisted.
	saved, err := ms.GetNode(ctx, "node-2")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if saved.PrivateKeyB64 != result.PrivateKeyB64 {
		t.Errorf("persisted private key does not match returned key")
	}
}

func TestEnsureKey_Idempotent(t *testing.T) {
	ctx := context.Background()
	ms := memstore.New()

	node := store.Node{ID: "node-3"}

	first, err := nodekey.EnsureKey(ctx, ms, node)
	if err != nil {
		t.Fatalf("first EnsureKey: %v", err)
	}

	// Load what was stored and call EnsureKey again — it should not change the key.
	stored, err := ms.GetNode(ctx, "node-3")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}

	second, err := nodekey.EnsureKey(ctx, ms, stored)
	if err != nil {
		t.Fatalf("second EnsureKey: %v", err)
	}
	if second.PrivateKeyB64 != first.PrivateKeyB64 {
		t.Errorf("idempotency violated: private key changed on second call")
	}
}

func TestDecodePrivate_Valid(t *testing.T) {
	privB64, _, err := nodekey.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	key, err := nodekey.DecodePrivate(privB64)
	if err != nil {
		t.Fatalf("DecodePrivate: %v", err)
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Errorf("decoded private key length: got %d, want %d", len(key), ed25519.PrivateKeySize)
	}
}

func TestDecodePrivate_InvalidBase64(t *testing.T) {
	_, err := nodekey.DecodePrivate("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecodePrivate_WrongSize(t *testing.T) {
	// 10 bytes, not 64.
	short := base64.StdEncoding.EncodeToString([]byte("tooshort"))
	_, err := nodekey.DecodePrivate(short)
	if err == nil {
		t.Fatal("expected error for wrong-size private key bytes")
	}
}

func TestDecodePublic_Valid(t *testing.T) {
	_, pubB64, err := nodekey.Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	key, err := nodekey.DecodePublic(pubB64)
	if err != nil {
		t.Fatalf("DecodePublic: %v", err)
	}
	if len(key) != ed25519.PublicKeySize {
		t.Errorf("decoded public key length: got %d, want %d", len(key), ed25519.PublicKeySize)
	}
}

func TestDecodePublic_InvalidBase64(t *testing.T) {
	_, err := nodekey.DecodePublic("not-valid-base64!!!")
	if err == nil {
		t.Fatal("expected error for invalid base64")
	}
}

func TestDecodePublic_WrongSize(t *testing.T) {
	// 5 bytes, not 32.
	short := base64.StdEncoding.EncodeToString([]byte("hi"))
	_, err := nodekey.DecodePublic(short)
	if err == nil {
		t.Fatal("expected error for wrong-size public key bytes")
	}
}

// saveCountStore wraps memstore.Store to count SaveNode calls.
type saveCountStore struct {
	*memstore.Store
	saveCount int
}

func (s *saveCountStore) SaveNode(ctx context.Context, n store.Node) error {
	s.saveCount++
	return s.Store.SaveNode(ctx, n)
}
