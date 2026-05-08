package registry

import (
	"context"
	"errors"
)

var ErrNotFound = errors.New("registry: not found")

type EntityType string

const (
	EntityDomain EntityType = "domain"
	EntityUser   EntityType = "user"
)

type Entity struct {
	Type      EntityType
	Name      string
	IsDefault bool              // relevant for EntityDomain
	UserType  string            // "human" | "agent" | "service" | "" — direct User.Type field
	Metadata  map[string]string // relevant for EntityUser
}

// Registry maps targets (domain names, user emails) to responsible node IDs.
// All routing decisions go through Registry.Lookup so the implementation can
// later be swapped for a Redis-backed consistent-hashing ring without touching
// any caller code.
type Registry interface {
	// Lookup returns the node ID responsible for target.
	// target may be a bare domain ("example.com") or a full email ("user@example.com").
	// Returns ErrNotFound if no mapping exists and no default domain is configured.
	Lookup(ctx context.Context, target string) (nodeID string, err error)

	// Register atomically claims target for the local node.
	Register(ctx context.Context, entity Entity) error
}

// NodeResolver maps node IDs to their publicly reachable gRPC addresses.
// Redis-backed registries implement this to enable Discover to return the real
// owning-node address rather than always returning the local address.
type NodeResolver interface {
	ResolveNode(ctx context.Context, nodeID string) (address string, err error)
}
