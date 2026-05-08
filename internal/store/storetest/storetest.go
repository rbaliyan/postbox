// Package storetest exposes a backend-agnostic conformance suite for the
// store.Store interface. It is intended to be invoked from each backend's
// _test.go to avoid duplicating test logic.
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/store"
)

// Run runs the full conformance suite against the supplied factory.
// The factory is called once per sub-test and must return a fresh, empty
// Store that can be Connected and Closed by the suite.
func Run(t *testing.T, factory func(t *testing.T) store.Store) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(t *testing.T, s store.Store)
	}{
		{"NodeRoundTrip", testNodeRoundTrip},
		{"DomainRoundTrip", testDomainRoundTrip},
		{"DomainNotFound", testDomainNotFound},
		{"DefaultDomain", testDefaultDomain},
		{"ListDomains", testListDomains},
		{"DomainCount", testDomainCount},
		{"UserRoundTrip", testUserRoundTrip},
		{"UserPublicKeyRoundTrip", testUserPublicKeyRoundTrip},
		{"UserTypeRoundTrip", testUserTypeRoundTrip},
		{"UserMetadataRoundTrip", testUserMetadataRoundTrip},
		{"UserNotFound", testUserNotFound},
		{"UserCount", testUserCount},
		{"UserList", testUserList},
		{"UserSearchByType", testUserSearchByType},
		{"UserSearchByMetadata", testUserSearchByMetadata},
		{"UserSearchSkillsContains", testUserSearchSkillsContains},
		{"UserSearchEmpty", testUserSearchEmpty},
		{"SaveNodeIdempotent", testSaveNodeIdempotent},
		{"SaveNodeRequiresID", testSaveNodeRequiresID},
		{"DeliveryJobRoundTrip", testDeliveryJobRoundTrip},
		{"DeliveryJobUpdate", testDeliveryJobUpdate},
		{"DeliveryJobNotFound", testDeliveryJobNotFound},
		{"DeliveryJobListPending", testDeliveryJobListPending},
		{"DeliveryJobListByRecipient", testDeliveryJobListByRecipient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := factory(t)
			ctx := context.Background()
			if err := s.Connect(ctx); err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(func() { _ = s.Close(ctx) })
			tc.fn(t, s)
		})
	}
}

func testNodeRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	if err := s.SaveNode(ctx, store.Node{ID: "node-1"}); err != nil {
		t.Fatalf("save node: %v", err)
	}
	got, err := s.GetNode(ctx, "node-1")
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	if got.ID != "node-1" {
		t.Fatalf("got id %q, want node-1", got.ID)
	}
	if _, err := s.GetNode(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing node: got %v, want ErrNotFound", err)
	}
}

func testDomainRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	d := store.Domain{Name: "example.com", NodeID: "node-1"}
	if err := s.SaveDomain(ctx, d); err != nil {
		t.Fatalf("save domain: %v", err)
	}
	got, err := s.GetDomain(ctx, "example.com")
	if err != nil {
		t.Fatalf("get domain: %v", err)
	}
	if got != d {
		t.Fatalf("got %+v, want %+v", got, d)
	}
}

func testDomainNotFound(t *testing.T, s store.Store) {
	if _, err := s.GetDomain(context.Background(), "nope.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing domain: got %v, want ErrNotFound", err)
	}
}

func testDefaultDomain(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetDefaultDomain(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no default: got %v, want ErrNotFound", err)
	}
	if err := s.SaveDomain(ctx, store.Domain{Name: "alpha.com", NodeID: "n", IsDefault: true}); err != nil {
		t.Fatalf("save default: %v", err)
	}
	got, err := s.GetDefaultDomain(ctx)
	if err != nil {
		t.Fatalf("get default: %v", err)
	}
	if got.Name != "alpha.com" || !got.IsDefault {
		t.Fatalf("got %+v", got)
	}
}

func testListDomains(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, name := range []string{"b.com", "a.com", "c.com"} {
		if err := s.SaveDomain(ctx, store.Domain{Name: name, NodeID: "n"}); err != nil {
			t.Fatalf("save %s: %v", name, err)
		}
	}
	domains, err := s.ListDomains(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(domains) != 3 {
		t.Fatalf("got %d domains, want 3", len(domains))
	}
	if domains[0].Name != "a.com" || domains[2].Name != "c.com" {
		t.Fatalf("expected sorted order, got %v", domains)
	}
}

func testDomainCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	if got, err := s.CountDomains(ctx); err != nil || got != 0 {
		t.Fatalf("count empty: got=%d err=%v", got, err)
	}
	if err := s.SaveDomain(ctx, store.Domain{Name: "x.com", NodeID: "n"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err := s.CountDomains(ctx); err != nil || got != 1 {
		t.Fatalf("count one: got=%d err=%v", got, err)
	}
}

func testUserRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	u := store.User{Email: "alice@x.com", CreatedAt: created}
	if err := s.SaveUser(ctx, u); err != nil {
		t.Fatalf("save user: %v", err)
	}
	got, err := s.GetUser(ctx, "alice@x.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Email != u.Email {
		t.Fatalf("got email %q, want %q", got.Email, u.Email)
	}
	if !got.CreatedAt.Equal(created) {
		t.Fatalf("got created_at %v, want %v", got.CreatedAt, created)
	}
}

func testUserPublicKeyRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	u := store.User{Email: "bot@x.com", PublicKeyB64: "dGVzdGtleQ=="}
	if err := s.SaveUser(ctx, u); err != nil {
		t.Fatalf("save user: %v", err)
	}
	got, err := s.GetUser(ctx, "bot@x.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.PublicKeyB64 != "dGVzdGtleQ==" {
		t.Fatalf("got public_key %q, want dGVzdGtleQ==", got.PublicKeyB64)
	}
}

func testUserTypeRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	u := store.User{Email: "agent@x.com", Type: "agent"}
	if err := s.SaveUser(ctx, u); err != nil {
		t.Fatalf("save user: %v", err)
	}
	got, err := s.GetUser(ctx, "agent@x.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Type != "agent" {
		t.Fatalf("got type %q, want agent", got.Type)
	}
}

func testUserMetadataRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	meta := map[string]string{"role": "admin", "team": "ops"}
	if err := s.SaveUser(ctx, store.User{Email: "bob@x.com", Metadata: meta}); err != nil {
		t.Fatalf("save user: %v", err)
	}
	got, err := s.GetUser(ctx, "bob@x.com")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Metadata["role"] != "admin" || got.Metadata["team"] != "ops" {
		t.Fatalf("got metadata %+v", got.Metadata)
	}
}

func testUserNotFound(t *testing.T, s store.Store) {
	if _, err := s.GetUser(context.Background(), "ghost@x.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing user: got %v, want ErrNotFound", err)
	}
}

func testUserCount(t *testing.T, s store.Store) {
	ctx := context.Background()
	if got, err := s.CountUsers(ctx); err != nil || got != 0 {
		t.Fatalf("count empty: got=%d err=%v", got, err)
	}
	if err := s.SaveUser(ctx, store.User{Email: "u@x.com"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if got, err := s.CountUsers(ctx); err != nil || got != 1 {
		t.Fatalf("count one: got=%d err=%v", got, err)
	}
}

func testUserList(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, email := range []string{"c@x.com", "a@x.com", "b@x.com"} {
		if err := s.SaveUser(ctx, store.User{Email: email}); err != nil {
			t.Fatalf("save %s: %v", email, err)
		}
	}
	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("got %d users, want 3", len(users))
	}
	if users[0].Email != "a@x.com" || users[2].Email != "c@x.com" {
		t.Fatalf("expected sorted order, got %v", users)
	}
}

func testUserSearchByType(t *testing.T, s store.Store) {
	ctx := context.Background()
	users := []store.User{
		{Email: "a@x.com", Type: "agent"},
		{Email: "b@x.com", Type: "agent"},
		{Email: "c@x.com", Type: "human"},
	}
	for _, u := range users {
		if err := s.SaveUser(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.Email, err)
		}
	}

	got, err := s.SearchUsers(ctx, map[string]string{"type": "agent"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d users, want 2", len(got))
	}

	got2, err := s.SearchUsers(ctx, map[string]string{"type": "human"})
	if err != nil {
		t.Fatalf("search human: %v", err)
	}
	if len(got2) != 1 || got2[0].Email != "c@x.com" {
		t.Fatalf("human filter: got %v", got2)
	}
}

func testUserSearchByMetadata(t *testing.T, s store.Store) {
	ctx := context.Background()
	users := []store.User{
		{Email: "a@x.com", Type: "agent", Metadata: map[string]string{"model": "claude"}},
		{Email: "b@x.com", Type: "agent", Metadata: map[string]string{"model": "gpt"}},
		{Email: "c@x.com", Type: "human"},
	}
	for _, u := range users {
		if err := s.SaveUser(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.Email, err)
		}
	}

	// Filter by model=claude — should match only a.
	got, err := s.SearchUsers(ctx, map[string]string{"model": "claude"})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(got) != 1 || got[0].Email != "a@x.com" {
		t.Fatalf("model-filter: got %v", got)
	}

	// Combine type and metadata filter — type=agent, model=gpt should match only b.
	got2, err := s.SearchUsers(ctx, map[string]string{"type": "agent", "model": "gpt"})
	if err != nil {
		t.Fatalf("search multi: %v", err)
	}
	if len(got2) != 1 || got2[0].Email != "b@x.com" {
		t.Fatalf("multi-filter: got %v", got2)
	}
}

func testUserSearchSkillsContains(t *testing.T, s store.Store) {
	ctx := context.Background()
	users := []store.User{
		{Email: "a@x.com", Metadata: map[string]string{"skills": "web-search,image-gen"}},
		{Email: "b@x.com", Metadata: map[string]string{"skills": "image-gen"}},
		{Email: "c@x.com", Metadata: map[string]string{"skills": "code-review"}},
	}
	for _, u := range users {
		if err := s.SaveUser(ctx, u); err != nil {
			t.Fatalf("save %s: %v", u.Email, err)
		}
	}

	// Substring match: "image-gen" matches both a and b.
	got, err := s.SearchUsers(ctx, map[string]string{"skills": "image-gen"})
	if err != nil {
		t.Fatalf("search skills: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}

	// "web-search" matches only a.
	got2, err := s.SearchUsers(ctx, map[string]string{"skills": "web-search"})
	if err != nil {
		t.Fatalf("search web-search: %v", err)
	}
	if len(got2) != 1 || got2[0].Email != "a@x.com" {
		t.Fatalf("web-search: got %v", got2)
	}
}

func testUserSearchEmpty(t *testing.T, s store.Store) {
	ctx := context.Background()
	for _, email := range []string{"a@x.com", "b@x.com"} {
		if err := s.SaveUser(ctx, store.User{Email: email}); err != nil {
			t.Fatalf("save %s: %v", email, err)
		}
	}
	got, err := s.SearchUsers(ctx, nil)
	if err != nil {
		t.Fatalf("search empty: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func testSaveNodeIdempotent(t *testing.T, s store.Store) {
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.SaveNode(ctx, store.Node{ID: "node-id"}); err != nil {
			t.Fatalf("save iter=%d: %v", i, err)
		}
	}
}

func testSaveNodeRequiresID(t *testing.T, s store.Store) {
	if err := s.SaveNode(context.Background(), store.Node{}); err == nil {
		t.Fatal("expected error for empty node id")
	}
}

func testDeliveryJobRoundTrip(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	job := store.DeliveryJob{
		MessageID:   "msg-1",
		RecipientID: "alice@example.com",
		EndpointURL: "https://example.com/webhook",
		Status:      store.DeliveryPending,
		MaxAttempts: 5,
		NextRetryAt: now,
		CreatedAt:   now,
	}
	if err := s.SaveDeliveryJob(ctx, job); err != nil {
		t.Fatalf("save delivery job: %v", err)
	}
	got, err := s.GetDeliveryJob(ctx, "msg-1", "alice@example.com")
	if err != nil {
		t.Fatalf("get delivery job: %v", err)
	}
	if got.MessageID != job.MessageID {
		t.Fatalf("got message_id %q, want %q", got.MessageID, job.MessageID)
	}
	if got.RecipientID != job.RecipientID {
		t.Fatalf("got recipient_id %q, want %q", got.RecipientID, job.RecipientID)
	}
	if got.EndpointURL != job.EndpointURL {
		t.Fatalf("got endpoint_url %q, want %q", got.EndpointURL, job.EndpointURL)
	}
	if got.Status != store.DeliveryPending {
		t.Fatalf("got status %q, want %q", got.Status, store.DeliveryPending)
	}
}

func testDeliveryJobUpdate(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	job := store.DeliveryJob{
		MessageID:   "msg-2",
		RecipientID: "bob@example.com",
		EndpointURL: "https://example.com/webhook",
		Status:      store.DeliveryPending,
		MaxAttempts: 3,
		NextRetryAt: now,
		CreatedAt:   now,
	}
	if err := s.SaveDeliveryJob(ctx, job); err != nil {
		t.Fatalf("save delivery job: %v", err)
	}

	deliveredAt := now.Add(time.Minute)
	updated := store.DeliveryJob{
		MessageID:   "msg-2",
		RecipientID: "bob@example.com",
		Status:      store.DeliveryDelivered,
		Attempts:    1,
		DeliveredAt: &deliveredAt,
	}
	if err := s.UpdateDeliveryJob(ctx, updated); err != nil {
		t.Fatalf("update delivery job: %v", err)
	}

	got, err := s.GetDeliveryJob(ctx, "msg-2", "bob@example.com")
	if err != nil {
		t.Fatalf("get delivery job after update: %v", err)
	}
	if got.Status != store.DeliveryDelivered {
		t.Fatalf("got status %q, want %q", got.Status, store.DeliveryDelivered)
	}
	if got.Attempts != 1 {
		t.Fatalf("got attempts %d, want 1", got.Attempts)
	}
	if got.DeliveredAt == nil {
		t.Fatal("expected delivered_at to be set")
	}
}

func testDeliveryJobNotFound(t *testing.T, s store.Store) {
	ctx := context.Background()
	if _, err := s.GetDeliveryJob(ctx, "nonexistent-msg", "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing job: got %v, want ErrNotFound", err)
	}
}

func testDeliveryJobListPending(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	jobs := []store.DeliveryJob{
		{
			MessageID:   "msg-a",
			RecipientID: "r@example.com",
			Status:      store.DeliveryPending,
			NextRetryAt: past,
			CreatedAt:   now,
		},
		{
			MessageID:   "msg-b",
			RecipientID: "r@example.com",
			Status:      store.DeliveryFailed,
			NextRetryAt: past.Add(30 * time.Minute),
			CreatedAt:   now,
		},
		{
			MessageID:   "msg-c",
			RecipientID: "r@example.com",
			Status:      store.DeliveryPending,
			NextRetryAt: future,
			CreatedAt:   now,
		},
	}
	for _, j := range jobs {
		if err := s.SaveDeliveryJob(ctx, j); err != nil {
			t.Fatalf("save job %s: %v", j.MessageID, j.MessageID)
		}
	}

	// Both past-due jobs (msg-a and msg-b) should be returned.
	got, err := s.ListPendingDeliveryJobs(ctx, now, 10)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d pending jobs, want 2", len(got))
	}
	// Results should be ordered by NextRetryAt ascending.
	if !got[0].NextRetryAt.Before(got[1].NextRetryAt) && got[0].NextRetryAt != got[1].NextRetryAt {
		t.Fatalf("expected ascending order by next_retry_at, got %v then %v", got[0].NextRetryAt, got[1].NextRetryAt)
	}

	// Limit=1 should return only one job.
	limited, err := s.ListPendingDeliveryJobs(ctx, now, 1)
	if err != nil {
		t.Fatalf("list pending limit=1: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("got %d jobs with limit=1, want 1", len(limited))
	}
}

func testDeliveryJobListByRecipient(t *testing.T, s store.Store) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	jobs := []store.DeliveryJob{
		{
			MessageID:   "msg-1",
			RecipientID: "alice@example.com",
			Status:      store.DeliveryPending,
			NextRetryAt: now,
			CreatedAt:   now,
		},
		{
			MessageID:   "msg-2",
			RecipientID: "alice@example.com",
			Status:      store.DeliveryPending,
			NextRetryAt: now,
			CreatedAt:   now.Add(-time.Minute),
		},
		{
			MessageID:   "msg-3",
			RecipientID: "bob@example.com",
			Status:      store.DeliveryPending,
			NextRetryAt: now,
			CreatedAt:   now,
		},
	}
	for _, j := range jobs {
		if err := s.SaveDeliveryJob(ctx, j); err != nil {
			t.Fatalf("save job %s: %v", j.MessageID, err)
		}
	}

	// Only alice's jobs should be returned.
	aliceJobs, err := s.ListDeliveryJobsByRecipient(ctx, "alice@example.com", "")
	if err != nil {
		t.Fatalf("list by recipient alice: %v", err)
	}
	if len(aliceJobs) != 2 {
		t.Fatalf("got %d jobs for alice, want 2", len(aliceJobs))
	}
	for _, j := range aliceJobs {
		if j.RecipientID != "alice@example.com" {
			t.Fatalf("expected only alice's jobs, got recipient %q", j.RecipientID)
		}
	}

	// Filtering by status=Delivered should return nothing (all are Pending).
	delivered, err := s.ListDeliveryJobsByRecipient(ctx, "alice@example.com", store.DeliveryDelivered)
	if err != nil {
		t.Fatalf("list delivered for alice: %v", err)
	}
	if len(delivered) != 0 {
		t.Fatalf("got %d delivered jobs, want 0", len(delivered))
	}

	// Empty status string should return all jobs for the recipient.
	all, err := s.ListDeliveryJobsByRecipient(ctx, "alice@example.com", "")
	if err != nil {
		t.Fatalf("list all for alice: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d jobs for alice with empty status, want 2", len(all))
	}
}
