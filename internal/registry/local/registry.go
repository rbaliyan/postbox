// Package local provides a single-node Registry backed by a Store.
// All Lookup calls resolve to the local node; the Store is the source of truth
// for which domains and users have been registered.
// Swap this implementation for a Redis consistent-hash ring to enable sharding.
package local

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/rbaliyan/postbox/internal/registry"
	"github.com/rbaliyan/postbox/internal/store"
)

var _ registry.Registry = (*Registry)(nil)

type Registry struct {
	nodeID string
	store  store.Store
}

func New(nodeID string, s store.Store) *Registry {
	return &Registry{nodeID: nodeID, store: s}
}

// Lookup implements the three-tier discovery:
//  1. If target contains "@", attempt a user lookup first.
//  2. Extract the domain portion and look it up directly.
//  3. Fall back to the default domain if configured.
func (r *Registry) Lookup(ctx context.Context, target string) (string, error) {
	// Step 1: user-level lookup (only when a full email is given).
	if strings.Contains(target, "@") {
		if _, err := r.store.GetUser(ctx, target); err == nil {
			// User is known — resolve via their domain.
			parts := strings.SplitN(target, "@", 2)
			if len(parts) == 2 {
				if d, err := r.store.GetDomain(ctx, parts[1]); err == nil {
					return d.NodeID, nil
				}
			}
		}
	}

	// Step 2: domain lookup.
	domainName := target
	if idx := strings.Index(target, "@"); idx >= 0 {
		domainName = target[idx+1:]
	}
	if d, err := r.store.GetDomain(ctx, domainName); err == nil {
		return d.NodeID, nil
	}

	// Step 3: default domain fallback.
	def, err := r.store.GetDefaultDomain(ctx)
	if err != nil {
		return "", registry.ErrNotFound
	}
	return def.NodeID, nil
}

func (r *Registry) Register(ctx context.Context, entity registry.Entity) error {
	switch entity.Type {
	case registry.EntityDomain:
		return r.store.SaveDomain(ctx, store.Domain{
			Name:      entity.Name,
			NodeID:    r.nodeID,
			IsDefault: entity.IsDefault,
		})
	case registry.EntityUser:
		return r.store.SaveUser(ctx, store.User{
			Email:     entity.Name,
			Type:      entity.UserType,
			Metadata:  entity.Metadata,
			CreatedAt: time.Now().UTC(),
		})
	default:
		return fmt.Errorf("registry: unsupported entity type %q", entity.Type)
	}
}
