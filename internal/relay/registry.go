package relay

import (
	"context"
	"errors"

	"github.com/rbaliyan/postbox/internal/registry"
)

// FallbackRegistry wraps an inner registry and routes recipients that cannot
// be resolved (ErrNotFound) to a configured relay node ID. This enables
// outbound relay for email addresses not registered in the local store without
// enumerating every external domain.
type FallbackRegistry struct {
	inner       registry.Registry
	relayNodeID string
}

var _ registry.Registry = (*FallbackRegistry)(nil)

// NewFallbackRegistry returns a FallbackRegistry that sends all unresolvable
// recipients to relayNodeID.
func NewFallbackRegistry(inner registry.Registry, relayNodeID string) *FallbackRegistry {
	return &FallbackRegistry{inner: inner, relayNodeID: relayNodeID}
}

func (r *FallbackRegistry) Lookup(ctx context.Context, target string) (string, error) {
	nodeID, err := r.inner.Lookup(ctx, target)
	if errors.Is(err, registry.ErrNotFound) {
		return r.relayNodeID, nil
	}
	return nodeID, err
}

func (r *FallbackRegistry) Register(ctx context.Context, entity registry.Entity) error {
	return r.inner.Register(ctx, entity)
}

// StaticResolver resolves node IDs from a static in-memory map, delegating
// unknown IDs to an optional base resolver. Use it to register relay virtual
// nodes that have no Redis or other dynamic presence.
type StaticResolver struct {
	base    registry.NodeResolver // may be nil
	entries map[string]string     // nodeID → gRPC address
}

var _ registry.NodeResolver = (*StaticResolver)(nil)

// NewStaticResolver returns a StaticResolver that looks up entries from the
// given map and falls back to base (which may be nil) for unknown node IDs.
func NewStaticResolver(entries map[string]string, base registry.NodeResolver) *StaticResolver {
	return &StaticResolver{entries: entries, base: base}
}

func (r *StaticResolver) ResolveNode(ctx context.Context, nodeID string) (string, error) {
	if addr, ok := r.entries[nodeID]; ok {
		return addr, nil
	}
	if r.base != nil {
		return r.base.ResolveNode(ctx, nodeID)
	}
	return "", registry.ErrNotFound
}
