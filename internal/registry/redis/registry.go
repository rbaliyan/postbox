// Package redis provides a Redis-backed registry.Registry implementation for
// multi-node Postbox deployments. All nodes in a cluster share the same Redis
// instance; domain registrations and node heartbeats are visible to every peer.
package redis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rbaliyan/postbox/internal/registry"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/redis/go-redis/v9"
)

const (
	domainKeyPrefix  = "postbox:route:domain:"
	defaultDomainKey = "postbox:default_domain"
	nodeKeyPrefix    = "postbox:node:"
	// HeartbeatTTL is how long a node's address entry lives in Redis before
	// expiring. Nodes must call Announce at least once per HeartbeatTTL.
	HeartbeatTTL = 30 * time.Second
)

// routeEntry stores the owning node for a domain.
type routeEntry struct {
	NodeID  string `json:"node_id"`
	Address string `json:"address"`
}

// Registry is a Redis-backed registry.Registry and registry.NodeResolver.
// Domain registrations are written to both Redis (for cluster-wide visibility)
// and a local store (for persistence across restarts).
type Registry struct {
	client  redis.UniversalClient
	nodeID  string
	address string
	local   store.Store
}

var _ registry.Registry = (*Registry)(nil)
var _ registry.NodeResolver = (*Registry)(nil)

// New creates a Registry. nodeID and address identify the local node; local is
// the persistent backing store that is also updated on Register calls.
// client accepts redis.UniversalClient to support standalone, Cluster, and
// Sentinel topologies.
func New(client redis.UniversalClient, nodeID, address string, local store.Store) *Registry {
	return &Registry{client: client, nodeID: nodeID, address: address, local: local}
}

// Lookup returns the node ID responsible for target using a three-tier search:
//  1. Domain extracted from target (or bare target if no "@").
//  2. Default domain fallback.
//
// Returns registry.ErrNotFound when no mapping exists.
func (r *Registry) Lookup(ctx context.Context, target string) (string, error) {
	domain := target
	if idx := strings.Index(target, "@"); idx >= 0 {
		domain = target[idx+1:]
	}

	if entry, err := r.getDomainRoute(ctx, domain); err == nil {
		return entry.NodeID, nil
	}

	defaultName, err := r.client.Get(ctx, defaultDomainKey).Result()
	if err != nil || defaultName == domain {
		return "", registry.ErrNotFound
	}
	entry, err := r.getDomainRoute(ctx, defaultName)
	if err != nil {
		return "", registry.ErrNotFound
	}
	return entry.NodeID, nil
}

// Register writes the entity to Redis and to the local persistent store.
func (r *Registry) Register(ctx context.Context, entity registry.Entity) error {
	switch entity.Type {
	case registry.EntityDomain:
		entry := routeEntry{NodeID: r.nodeID, Address: r.address}
		data, _ := json.Marshal(entry)
		if err := r.client.Set(ctx, domainKeyPrefix+entity.Name, data, 0).Err(); err != nil {
			return fmt.Errorf("redis registry: set domain route: %w", err)
		}
		if entity.IsDefault {
			if err := r.client.Set(ctx, defaultDomainKey, entity.Name, 0).Err(); err != nil {
				return fmt.Errorf("redis registry: set default domain: %w", err)
			}
		}
		return r.local.SaveDomain(ctx, store.Domain{
			Name:      entity.Name,
			NodeID:    r.nodeID,
			IsDefault: entity.IsDefault,
		})
	case registry.EntityUser:
		return r.local.SaveUser(ctx, store.User{
			Email:    entity.Name,
			Type:     entity.UserType,
			Metadata: entity.Metadata,
		})
	default:
		return fmt.Errorf("redis registry: unsupported entity type %q", entity.Type)
	}
}

// ResolveNode returns the gRPC address for the given node ID.
// For the local node it returns the configured address directly; for remote
// nodes it reads the heartbeat key set by Announce.
// Returns registry.ErrNotFound when the node has no active heartbeat.
func (r *Registry) ResolveNode(ctx context.Context, nodeID string) (string, error) {
	if nodeID == r.nodeID {
		return r.address, nil
	}
	addr, err := r.client.Get(ctx, nodeKeyPrefix+nodeID).Result()
	if errors.Is(err, redis.Nil) {
		return "", registry.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redis registry: resolve node %q: %w", nodeID, err)
	}
	return addr, nil
}

// Announce stores this node's gRPC address in Redis with a TTL of HeartbeatTTL.
// Call periodically (at most HeartbeatTTL/2) to keep the node visible to peers.
func (r *Registry) Announce(ctx context.Context) error {
	return r.client.Set(ctx, nodeKeyPrefix+r.nodeID, r.address, HeartbeatTTL).Err()
}

func (r *Registry) getDomainRoute(ctx context.Context, domain string) (routeEntry, error) {
	data, err := r.client.Get(ctx, domainKeyPrefix+domain).Bytes()
	if errors.Is(err, redis.Nil) {
		return routeEntry{}, registry.ErrNotFound
	}
	if err != nil {
		return routeEntry{}, err
	}
	var entry routeEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return routeEntry{}, fmt.Errorf("redis registry: unmarshal domain route: %w", err)
	}
	return entry, nil
}
