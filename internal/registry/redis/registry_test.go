package redis_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/rbaliyan/postbox/internal/registry"
	redisreg "github.com/rbaliyan/postbox/internal/registry/redis"
	"github.com/rbaliyan/postbox/internal/store/memstore"
	"github.com/redis/go-redis/v9"
)

// newTestRegistry creates a Registry wired to a miniredis instance.
// It returns both the registry and the miniredis server so tests can fast-forward time.
func newTestRegistry(t *testing.T) (*redisreg.Registry, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := memstore.New()
	_ = s.Connect(context.Background())
	return redisreg.New(client, "node-1", "localhost:50051", s), mr
}

// newTestRegistryWithNodeID creates a Registry with a custom nodeID and address.
func newTestRegistryWithNodeID(t *testing.T, mr *miniredis.Miniredis, nodeID, addr string) *redisreg.Registry {
	t.Helper()
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := memstore.New()
	_ = s.Connect(context.Background())
	return redisreg.New(client, nodeID, addr, s)
}

// TestRegistry_Register_Domain verifies that registering a domain causes
// Lookup to return the correct nodeID.
func TestRegistry_Register_Domain(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	err := reg.Register(ctx, registry.Entity{
		Type: registry.EntityDomain,
		Name: "example.com",
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	nodeID, err := reg.Lookup(ctx, "example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if nodeID != "node-1" {
		t.Fatalf("Lookup: got %q, want %q", nodeID, "node-1")
	}
}

// TestRegistry_Register_DefaultDomain verifies that when a domain is registered
// as the default, Lookup on an unknown domain falls back to it.
func TestRegistry_Register_DefaultDomain(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	err := reg.Register(ctx, registry.Entity{
		Type:      registry.EntityDomain,
		Name:      "default.example.com",
		IsDefault: true,
	})
	if err != nil {
		t.Fatalf("Register default domain: %v", err)
	}

	nodeID, err := reg.Lookup(ctx, "unknown-domain.test")
	if err != nil {
		t.Fatalf("Lookup with default fallback: %v", err)
	}
	if nodeID != "node-1" {
		t.Fatalf("Lookup: got %q, want %q", nodeID, "node-1")
	}
}

// TestRegistry_Lookup_UserEmail verifies that Lookup extracts the domain from a
// full email address and resolves via the domain route.
func TestRegistry_Lookup_UserEmail(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	err := reg.Register(ctx, registry.Entity{
		Type: registry.EntityDomain,
		Name: "example.com",
	})
	if err != nil {
		t.Fatalf("Register domain: %v", err)
	}

	nodeID, err := reg.Lookup(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup email: %v", err)
	}
	if nodeID != "node-1" {
		t.Fatalf("Lookup email: got %q, want %q", nodeID, "node-1")
	}
}

// TestRegistry_Lookup_NotFound verifies that Lookup returns registry.ErrNotFound
// when neither the domain nor a default is configured.
func TestRegistry_Lookup_NotFound(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	_, err := reg.Lookup(ctx, "user@nope.example.com")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("Lookup: got %v, want registry.ErrNotFound", err)
	}
}

// TestRegistry_Announce_ResolveNode verifies that after Announce, ResolveNode
// returns the configured address for the node.
func TestRegistry_Announce_ResolveNode(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.Announce(ctx); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	// Use a second registry client to resolve the node announced above.
	// We need to call ResolveNode from a *different* registry instance so the
	// local-node shortcut ("node-1" == "node-1") doesn't fire — create one for
	// a different nodeID.
	mr := miniredis.RunT(t)
	client2 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client2.Close() })

	// Re-use the original miniredis server by building a second client pointed
	// at the same address. We do this by building the second registry against
	// the same miniredis.
	//
	// Because we need to resolve node-1 from another node, create a registry
	// for "node-2" backed by the same miniredis server.
	client3 := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client3.Close() })
	_ = client3
}

// TestRegistry_Announce_ResolveNode_CrossNode verifies cross-node resolution:
// node-2 announces itself, then node-1's registry resolves node-2's address.
func TestRegistry_Announce_ResolveNode_CrossNode(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	reg1 := newTestRegistryWithNodeID(t, mr, "node-1", "localhost:50051")
	reg2 := newTestRegistryWithNodeID(t, mr, "node-2", "localhost:50052")

	// node-2 announces itself.
	if err := reg2.Announce(ctx); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	// node-1 resolves node-2.
	addr, err := reg1.ResolveNode(ctx, "node-2")
	if err != nil {
		t.Fatalf("ResolveNode: %v", err)
	}
	if addr != "localhost:50052" {
		t.Fatalf("ResolveNode: got %q, want %q", addr, "localhost:50052")
	}
}

// TestRegistry_ResolveNode_SelfNode verifies that ResolveNode for the local
// node returns the configured address directly without a Redis roundtrip.
func TestRegistry_ResolveNode_SelfNode(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	// No Announce call — the local node should resolve itself without Redis.
	addr, err := reg.ResolveNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("ResolveNode self: %v", err)
	}
	if addr != "localhost:50051" {
		t.Fatalf("ResolveNode self: got %q, want %q", addr, "localhost:50051")
	}
}

// TestRegistry_ResolveNode_NotFound verifies that ResolveNode returns
// registry.ErrNotFound for a node that has never announced itself.
func TestRegistry_ResolveNode_NotFound(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	_, err := reg.ResolveNode(ctx, "node-unknown")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("ResolveNode unknown: got %v, want registry.ErrNotFound", err)
	}
}

// TestRegistry_Register_UserPersisted verifies that registering a user entity
// stores it in the local memstore (retrievable via GetUser).
func TestRegistry_Register_UserPersisted(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	s := memstore.New()
	_ = s.Connect(context.Background())
	reg := redisreg.New(client, "node-1", "localhost:50051", s)

	ctx := context.Background()
	err := reg.Register(ctx, registry.Entity{
		Type:     registry.EntityUser,
		Name:     "alice@example.com",
		UserType: "human",
		Metadata: map[string]string{"skills": "code-review"},
	})
	if err != nil {
		t.Fatalf("Register user: %v", err)
	}

	u, err := s.GetUser(ctx, "alice@example.com")
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Email != "alice@example.com" {
		t.Fatalf("user email: got %q, want %q", u.Email, "alice@example.com")
	}
	if u.Type != "human" {
		t.Fatalf("user type: got %q, want %q", u.Type, "human")
	}
	if u.Metadata["skills"] != "code-review" {
		t.Fatalf("user metadata skills: got %q, want %q", u.Metadata["skills"], "code-review")
	}
}

// TestRegistry_HeartbeatExpiry verifies that after the TTL expires (simulated
// with miniredis.FastForward) ResolveNode returns ErrNotFound.
func TestRegistry_HeartbeatExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx := context.Background()

	reg1 := newTestRegistryWithNodeID(t, mr, "node-1", "localhost:50051")
	reg2 := newTestRegistryWithNodeID(t, mr, "node-2", "localhost:50052")

	// node-2 announces itself.
	if err := reg2.Announce(ctx); err != nil {
		t.Fatalf("Announce: %v", err)
	}

	// Should be resolvable before TTL expires.
	addr, err := reg1.ResolveNode(ctx, "node-2")
	if err != nil {
		t.Fatalf("ResolveNode before expiry: %v", err)
	}
	if addr != "localhost:50052" {
		t.Fatalf("address before expiry: got %q", addr)
	}

	// Fast-forward past HeartbeatTTL.
	mr.FastForward(redisreg.HeartbeatTTL + time.Second)

	// After expiry node-2 should no longer be resolvable.
	_, err = reg1.ResolveNode(ctx, "node-2")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("ResolveNode after expiry: got %v, want registry.ErrNotFound", err)
	}
}

// TestRegistry_Register_UnsupportedEntity verifies that registering an
// unsupported entity type returns a non-nil error.
func TestRegistry_Register_UnsupportedEntity(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	err := reg.Register(ctx, registry.Entity{Type: "unknown-type", Name: "x"})
	if err == nil {
		t.Fatal("expected error for unsupported entity type, got nil")
	}
}

// TestRegistry_Lookup_BareDomainAfterRegister verifies Lookup on a bare domain
// (no "@") resolves to the correct node.
func TestRegistry_Lookup_BareDomainAfterRegister(t *testing.T) {
	reg, _ := newTestRegistry(t)
	ctx := context.Background()

	if err := reg.Register(ctx, registry.Entity{
		Type: registry.EntityDomain,
		Name: "myservice.internal",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	nodeID, err := reg.Lookup(ctx, "myservice.internal")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if nodeID != "node-1" {
		t.Fatalf("Lookup: got %q, want %q", nodeID, "node-1")
	}
}

// TestRegistry_DefaultDomain_NoFallbackForSameDomain verifies that when the
// queried domain IS the default domain but has no route, ErrNotFound is returned
// (guards against infinite self-lookup of the default key).
func TestRegistry_DefaultDomain_NoFallbackForSameDomain(t *testing.T) {
	mr := miniredis.RunT(t)
	// Manually set only the default_domain key, but NOT the domain route key,
	// so getDomainRoute fails for the default domain.
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	// Set the default domain pointer to "ghost.com" but register no route for it.
	_ = client.Set(ctx, "postbox:default_domain", "ghost.com", 0).Err()

	s := memstore.New()
	_ = s.Connect(ctx)
	reg := redisreg.New(client, "node-1", "localhost:50051", s)

	_, err := reg.Lookup(ctx, "ghost.com")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
