package store_test

import (
	"testing"

	"github.com/rbaliyan/postbox/internal/store"
)

func TestUserMatchesFilters_EmptyFilters(t *testing.T) {
	u := store.User{
		Email: "alice@example.com",
		Type:  "human",
		Metadata: map[string]string{
			"region": "us-east-1",
		},
	}
	if !store.UserMatchesFilters(u, map[string]string{}) {
		t.Fatal("expected empty filters to match any user")
	}
	if !store.UserMatchesFilters(u, nil) {
		t.Fatal("expected nil filters to match any user")
	}
}

func TestUserMatchesFilters_TypeKey(t *testing.T) {
	u := store.User{Type: "human-agent"}

	// Exact match
	if !store.UserMatchesFilters(u, map[string]string{"type": "human-agent"}) {
		t.Fatal("expected exact type match")
	}
	// Partial/substring match
	if !store.UserMatchesFilters(u, map[string]string{"type": "agent"}) {
		t.Fatal("expected substring match for type")
	}
	if !store.UserMatchesFilters(u, map[string]string{"type": "human"}) {
		t.Fatal("expected substring match for type prefix")
	}
	// Non-matching
	if store.UserMatchesFilters(u, map[string]string{"type": "service"}) {
		t.Fatal("expected type 'service' not to match 'human-agent'")
	}
}

func TestUserMatchesFilters_MetadataKey(t *testing.T) {
	u := store.User{
		Type: "agent",
		Metadata: map[string]string{
			"skills": "web-search,image-gen",
			"region": "us-east-1",
		},
	}

	// Exact metadata match
	if !store.UserMatchesFilters(u, map[string]string{"skills": "web-search,image-gen"}) {
		t.Fatal("expected exact skills match")
	}
	// Partial metadata match
	if !store.UserMatchesFilters(u, map[string]string{"skills": "web-search"}) {
		t.Fatal("expected partial skills match")
	}
	if !store.UserMatchesFilters(u, map[string]string{"skills": "image-gen"}) {
		t.Fatal("expected partial skills match for image-gen")
	}
	// Multiple filters all match
	if !store.UserMatchesFilters(u, map[string]string{"skills": "web-search", "region": "us-east"}) {
		t.Fatal("expected multiple filters to all match")
	}
	// One of the filters does not match
	if store.UserMatchesFilters(u, map[string]string{"skills": "web-search", "region": "eu-west"}) {
		t.Fatal("expected mismatch on region filter")
	}
}

func TestUserMatchesFilters_NonMatchingFilter(t *testing.T) {
	u := store.User{
		Type: "agent",
		Metadata: map[string]string{
			"skills": "image-gen",
		},
	}
	if store.UserMatchesFilters(u, map[string]string{"skills": "web-search"}) {
		t.Fatal("expected filter not to match")
	}
}

func TestUserMatchesFilters_NilMetadataWithFilterKey(t *testing.T) {
	u := store.User{
		Type:     "agent",
		Metadata: nil,
	}
	// Requesting a metadata key when Metadata is nil should not match.
	if store.UserMatchesFilters(u, map[string]string{"skills": "web-search"}) {
		t.Fatal("expected nil metadata to not match a metadata filter")
	}
}

func TestIsValidMetaKey_Valid(t *testing.T) {
	validKeys := []string{
		"skills",
		"web-search",
		"region_1",
		"model-v2",
		"ABC",
		"a",
		"Z9",
	}
	for _, k := range validKeys {
		if !store.IsValidMetaKey(k) {
			t.Errorf("expected %q to be a valid meta key", k)
		}
	}
}

func TestIsValidMetaKey_Invalid(t *testing.T) {
	invalidKeys := []string{
		"",
		"key with space",
		"key.dot",
		"key/slash",
		"key$",
		"key@",
		"key!",
		"key#",
	}
	for _, k := range invalidKeys {
		if store.IsValidMetaKey(k) {
			t.Errorf("expected %q to be an invalid meta key", k)
		}
	}
}
