// Package store defines the persistence interface for Postbox node state:
// node identity, domain registrations, user records, and webhook delivery jobs.
package store

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when a requested record does not exist.
// Callers should test with errors.Is(err, store.ErrNotFound).
var ErrNotFound = errors.New("store: not found")

// Node records the identity of a Postbox node.
type Node struct {
	ID            string
	PrivateKeyB64 string // Ed25519 private key, base64-encoded; empty until first startup
	PublicKeyB64  string // Ed25519 public key, base64-encoded
}

// Domain maps a DNS domain name to the node that owns it.
// At most one Domain per Store may have IsDefault=true.
type Domain struct {
	Name      string
	NodeID    string
	IsDefault bool
}

// User holds registration data for any mailbox principal — human, AI agent,
// or service account.
//
// Type and PublicKeyB64 are structured fields; capability and routing
// properties (skills, region, model, endpoint, …) live in Metadata:
//
//	metadata["skills"]   = "web-search,image-gen"   // comma-separated
//	metadata["endpoint"] = "https://…/webhook"       // push-delivery URL
//	metadata["region"]   = "us-east-1"
type User struct {
	Email        string
	Type         string            // "human" | "agent" | "service" | "" (unset)
	PublicKeyB64 string            // Ed25519 public key, base64-encoded; empty for humans
	Metadata     map[string]string // capability / routing properties
	CreatedAt    time.Time
}

// DeliveryStatus is the lifecycle state of a webhook delivery attempt.
type DeliveryStatus string

const (
	DeliveryPending   DeliveryStatus = "pending"
	DeliveryDelivered DeliveryStatus = "delivered"
	DeliveryFailed    DeliveryStatus = "failed"
	DeliveryDead      DeliveryStatus = "dead" // max attempts exhausted
)

// DeliveryJob tracks webhook delivery state for one (message, recipient) pair.
type DeliveryJob struct {
	MessageID   string
	RecipientID string
	EndpointURL string
	Status      DeliveryStatus
	Attempts    int
	MaxAttempts int
	LastError   string
	NextRetryAt time.Time
	DeliveredAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Store is the persistence layer for node, domain, user, and webhook delivery records.
//
// Implementations must be safe for concurrent use after Connect has returned.
// All methods accept a context for cancellation and propagation.
type Store interface {
	// Connect opens the underlying connection and applies migrations.
	// It is idempotent and may be called only once per Store instance.
	Connect(ctx context.Context) error

	// Close releases the underlying connection.
	Close(ctx context.Context) error

	SaveNode(ctx context.Context, node Node) error
	GetNode(ctx context.Context, id string) (Node, error)

	SaveDomain(ctx context.Context, domain Domain) error
	GetDomain(ctx context.Context, name string) (Domain, error)
	GetDefaultDomain(ctx context.Context) (Domain, error)
	ListDomains(ctx context.Context) ([]Domain, error)
	CountDomains(ctx context.Context) (int64, error)

	SaveUser(ctx context.Context, user User) error
	GetUser(ctx context.Context, email string) (User, error)
	// ListUsers returns all users ordered by email.
	ListUsers(ctx context.Context) ([]User, error)
	// SearchUsers returns users whose metadata matches all supplied filters.
	// Each filter value is tested with strings.Contains against the
	// corresponding metadata value, so partial matches work (e.g. filtering
	// skills="web-search" matches "web-search,image-gen").
	// An empty filters map returns all users.
	SearchUsers(ctx context.Context, filters map[string]string) ([]User, error)
	CountUsers(ctx context.Context) (int64, error)

	// SaveDeliveryJob inserts a new webhook delivery job.
	SaveDeliveryJob(ctx context.Context, job DeliveryJob) error
	// UpdateDeliveryJob updates status, attempts, last_error, next_retry_at,
	// delivered_at, and updated_at on an existing job.
	UpdateDeliveryJob(ctx context.Context, job DeliveryJob) error
	// GetDeliveryJob retrieves the job for a (messageID, recipientID) pair.
	// Returns ErrNotFound when no job exists.
	GetDeliveryJob(ctx context.Context, messageID, recipientID string) (DeliveryJob, error)
	// ListPendingDeliveryJobs returns pending and failed jobs whose
	// next_retry_at is at or before `before`, ordered by next_retry_at.
	// limit caps the result set; 0 means no cap.
	ListPendingDeliveryJobs(ctx context.Context, before time.Time, limit int) ([]DeliveryJob, error)
	// ListDeliveryJobsByRecipient returns all jobs for a recipient email.
	// Pass an empty status to return all statuses.
	ListDeliveryJobsByRecipient(ctx context.Context, recipientID string, status DeliveryStatus) ([]DeliveryJob, error)
}

// UserMatchesFilters reports whether u satisfies all key-value filters.
// The "type" key matches the user's Type field directly; all other keys are
// matched with strings.Contains against the corresponding metadata value,
// allowing partial matches for comma-separated skill lists.
func UserMatchesFilters(u User, filters map[string]string) bool {
	for k, v := range filters {
		if k == "type" {
			if !strings.Contains(u.Type, v) {
				return false
			}
		} else if !strings.Contains(u.Metadata[k], v) {
			return false
		}
	}
	return true
}

// IsValidMetaKey reports whether k can be safely embedded in a SQL JSON path
// expression. Valid keys contain only ASCII letters, digits, underscores, and
// hyphens — the set used by all well-known Postbox metadata keys.
func IsValidMetaKey(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
