package local_test

import (
	"context"
	"errors"
	"testing"

	"github.com/rbaliyan/postbox/internal/registry"
	"github.com/rbaliyan/postbox/internal/registry/local"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
)

func newRegistry(t *testing.T) (*local.Registry, store.Store) {
	t.Helper()
	s := memstore.New()
	return local.New("node-A", s), s
}

func TestLookup_DomainOnly(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	if err := s.SaveDomain(ctx, store.Domain{Name: "example.com", NodeID: "node-A"}); err != nil {
		t.Fatal(err)
	}

	got, err := r.Lookup(ctx, "example.com")
	if err != nil || got != "node-A" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestLookup_EmailWithRegisteredUser(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	_ = s.SaveDomain(ctx, store.Domain{Name: "example.com", NodeID: "node-A"})
	_ = s.SaveUser(ctx, store.User{Email: "alice@example.com"})

	got, err := r.Lookup(ctx, "alice@example.com")
	if err != nil || got != "node-A" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestLookup_EmailFallsBackToDomain(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	_ = s.SaveDomain(ctx, store.Domain{Name: "example.com", NodeID: "node-A"})

	// User isn't registered, but the domain is.
	got, err := r.Lookup(ctx, "stranger@example.com")
	if err != nil || got != "node-A" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestLookup_DefaultDomainFallback(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	_ = s.SaveDomain(ctx, store.Domain{Name: "fallback.com", NodeID: "node-A", IsDefault: true})

	got, err := r.Lookup(ctx, "user@unknown.com")
	if err != nil || got != "node-A" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestLookup_NotFound(t *testing.T) {
	r, _ := newRegistry(t)
	if _, err := r.Lookup(context.Background(), "nope@nope.com"); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("got %v, want registry.ErrNotFound", err)
	}
}

func TestRegister_Domain(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	if err := r.Register(ctx, registry.Entity{
		Type: registry.EntityDomain, Name: "example.com", IsDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetDomain(ctx, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.NodeID != "node-A" || !d.IsDefault {
		t.Fatalf("got %+v", d)
	}
}

func TestRegister_User(t *testing.T) {
	r, s := newRegistry(t)
	ctx := context.Background()
	if err := r.Register(ctx, registry.Entity{
		Type: registry.EntityUser, Name: "alice@x.com",
		Metadata: map[string]string{"role": "admin"},
	}); err != nil {
		t.Fatal(err)
	}
	u, err := s.GetUser(ctx, "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if u.Metadata["role"] != "admin" {
		t.Fatalf("metadata not persisted: %+v", u.Metadata)
	}
	if u.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be populated")
	}
}

func TestRegister_UnsupportedType(t *testing.T) {
	r, _ := newRegistry(t)
	err := r.Register(context.Background(), registry.Entity{Type: "garbage", Name: "x"})
	if err == nil {
		t.Fatal("expected error for unsupported entity type")
	}
}
