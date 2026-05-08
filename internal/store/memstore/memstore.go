// Package memstore provides an in-memory implementation of store.Store
// suitable for unit tests. It is concurrent-safe.
package memstore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/rbaliyan/postbox/internal/store"
)

var _ store.Store = (*Store)(nil)

// Store is an in-memory store.Store. The zero value is not usable; call New.
type Store struct {
	mu           sync.RWMutex
	nodes        map[string]store.Node
	domains      map[string]store.Domain
	users        map[string]store.User
	deliveryJobs map[[2]string]store.DeliveryJob // key: [messageID, recipientID]
}

// New returns a fresh, ready-to-use in-memory store.
func New() *Store {
	return &Store{
		nodes:        make(map[string]store.Node),
		domains:      make(map[string]store.Domain),
		users:        make(map[string]store.User),
		deliveryJobs: make(map[[2]string]store.DeliveryJob),
	}
}

func (s *Store) Connect(_ context.Context) error { return nil }
func (s *Store) Close(_ context.Context) error   { return nil }

// ---- Node ----

func (s *Store) SaveNode(_ context.Context, n store.Node) error {
	if n.ID == "" {
		return errInvalid("node id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Preserve existing keys if not provided.
	if existing, ok := s.nodes[n.ID]; ok {
		if n.PrivateKeyB64 == "" {
			n.PrivateKeyB64 = existing.PrivateKeyB64
		}
		if n.PublicKeyB64 == "" {
			n.PublicKeyB64 = existing.PublicKeyB64
		}
	}
	s.nodes[n.ID] = n
	return nil
}

func (s *Store) GetNode(_ context.Context, id string) (store.Node, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n, ok := s.nodes[id]
	if !ok {
		return store.Node{}, store.ErrNotFound
	}
	return n, nil
}

// ---- Domain ----

func (s *Store) SaveDomain(_ context.Context, d store.Domain) error {
	if d.Name == "" {
		return errInvalid("domain name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if d.IsDefault {
		for k, existing := range s.domains {
			if existing.IsDefault && k != d.Name {
				existing.IsDefault = false
				s.domains[k] = existing
			}
		}
	}
	s.domains[d.Name] = d
	return nil
}

func (s *Store) GetDomain(_ context.Context, name string) (store.Domain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.domains[name]
	if !ok {
		return store.Domain{}, store.ErrNotFound
	}
	return d, nil
}

func (s *Store) GetDefaultDomain(_ context.Context) (store.Domain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.domains {
		if d.IsDefault {
			return d, nil
		}
	}
	return store.Domain{}, store.ErrNotFound
}

func (s *Store) ListDomains(_ context.Context) ([]store.Domain, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.Domain, 0, len(s.domains))
	for _, d := range s.domains {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) CountDomains(_ context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.domains)), nil
}

// ---- User ----

func (s *Store) SaveUser(_ context.Context, u store.User) error {
	if u.Email == "" {
		return errInvalid("user email is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.users[u.Email]; ok && !existing.CreatedAt.IsZero() {
		u.CreatedAt = existing.CreatedAt
	}
	s.users[u.Email] = u
	return nil
}

func (s *Store) GetUser(_ context.Context, email string) (store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[email]
	if !ok {
		return store.User{}, store.ErrNotFound
	}
	return u, nil
}

func (s *Store) ListUsers(_ context.Context) ([]store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (s *Store) SearchUsers(_ context.Context, filters map[string]string) ([]store.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]store.User, 0)
	for _, u := range s.users {
		if store.UserMatchesFilters(u, filters) {
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out, nil
}

func (s *Store) CountUsers(_ context.Context) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.users)), nil
}

// ---- Webhook delivery jobs ----

func (s *Store) SaveDeliveryJob(_ context.Context, job store.DeliveryJob) error {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	if job.MaxAttempts == 0 {
		job.MaxAttempts = 5
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{job.MessageID, job.RecipientID}
	if _, exists := s.deliveryJobs[key]; !exists {
		s.deliveryJobs[key] = job
	}
	return nil
}

func (s *Store) UpdateDeliveryJob(_ context.Context, job store.DeliveryJob) error {
	job.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	key := [2]string{job.MessageID, job.RecipientID}
	existing, ok := s.deliveryJobs[key]
	if !ok {
		return store.ErrNotFound
	}
	existing.Status = job.Status
	existing.Attempts = job.Attempts
	existing.LastError = job.LastError
	existing.NextRetryAt = job.NextRetryAt
	existing.DeliveredAt = job.DeliveredAt
	existing.UpdatedAt = job.UpdatedAt
	s.deliveryJobs[key] = existing
	return nil
}

func (s *Store) GetDeliveryJob(_ context.Context, messageID, recipientID string) (store.DeliveryJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	j, ok := s.deliveryJobs[[2]string{messageID, recipientID}]
	if !ok {
		return store.DeliveryJob{}, store.ErrNotFound
	}
	return j, nil
}

func (s *Store) ListPendingDeliveryJobs(_ context.Context, before time.Time, limit int) ([]store.DeliveryJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.DeliveryJob
	for _, j := range s.deliveryJobs {
		if (j.Status == store.DeliveryPending || j.Status == store.DeliveryFailed) &&
			!j.NextRetryAt.After(before) {
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].NextRetryAt.Before(out[k].NextRetryAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (s *Store) ListDeliveryJobsByRecipient(_ context.Context, recipientID string, status store.DeliveryStatus) ([]store.DeliveryJob, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []store.DeliveryJob
	for _, j := range s.deliveryJobs {
		if j.RecipientID != recipientID {
			continue
		}
		if status != "" && j.Status != status {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].CreatedAt.After(out[k].CreatedAt) })
	return out, nil
}

type invalidArgError string

func (e invalidArgError) Error() string { return "memstore: " + string(e) }

func errInvalid(s string) error { return invalidArgError(s) }
