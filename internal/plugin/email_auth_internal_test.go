package plugin

import "testing"

func TestOrgDomain_SingleLabel(t *testing.T) {
	// A single-label domain (no dot) should be returned as-is.
	got := orgDomain("localhost")
	if got != "localhost" {
		t.Errorf("orgDomain(%q) = %q, want %q", "localhost", got, "localhost")
	}
}

func TestOrgDomain_PSL_co_uk(t *testing.T) {
	// PSL-aware lookup must treat "co.uk" as the public suffix, so the org
	// domain of "mail.example.co.uk" is "example.co.uk", not "co.uk".
	got := orgDomain("mail.example.co.uk")
	want := "example.co.uk"
	if got != want {
		t.Errorf("orgDomain(%q) = %q, want %q", "mail.example.co.uk", got, want)
	}
}

func TestOrgDomain_Subdomain(t *testing.T) {
	got := orgDomain("sub.example.com")
	if got != "example.com" {
		t.Errorf("orgDomain(%q) = %q, want %q", "sub.example.com", got, "example.com")
	}
}
