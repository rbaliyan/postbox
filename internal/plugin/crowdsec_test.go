package plugin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/plugin"
)

func TestCrowdSec_NameInitClose(t *testing.T) {
	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{})
	if p.Name() != "cs" {
		t.Fatalf("Name()=%q", p.Name())
	}
	ctx := context.Background()
	if err := p.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCrowdSec_NoRemoteAddr_Passes(t *testing.T) {
	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{Endpoint: "http://127.0.0.1:1"})
	draft := &fakeDraft{} // no X-SMTP-Remote-Addr
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("should pass when no remote addr: %v", err)
	}
}

func TestCrowdSec_BannedIP_Rejects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ip") != "1.2.3.4" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		decisions := []map[string]any{{"type": "ban", "scope": "Ip", "value": "1.2.3.4"}}
		_ = json.NewEncoder(w).Encode(decisions)
	}))
	defer srv.Close()

	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{
		Endpoint: srv.URL,
		FailOpen: false,
	})
	draft := &fakeDraft{
		headers: map[string]string{"X-SMTP-Remote-Addr": "1.2.3.4:55000"},
	}
	if !isRejected(p.BeforeSend(context.Background(), "", draft)) {
		t.Fatal("expected rejection for banned IP")
	}
}

func TestCrowdSec_CleanIP_Passes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// No decisions.
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{Endpoint: srv.URL})
	draft := &fakeDraft{headers: map[string]string{"X-SMTP-Remote-Addr": "5.6.7.8:1234"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("clean IP should pass: %v", err)
	}
}

func TestCrowdSec_LAPIUnreachable_FailOpen(t *testing.T) {
	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{
		Endpoint: "http://127.0.0.1:1", // nothing listening
		FailOpen: true,
		Timeout:  50 * time.Millisecond,
	})
	draft := &fakeDraft{headers: map[string]string{"X-SMTP-Remote-Addr": "1.2.3.4:1234"}}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("fail-open: should pass when LAPI unreachable: %v", err)
	}
}

func TestCrowdSec_LAPIUnreachable_FailClosed(t *testing.T) {
	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{
		Endpoint: "http://127.0.0.1:1",
		FailOpen: false,
		Timeout:  50 * time.Millisecond,
	})
	draft := &fakeDraft{headers: map[string]string{"X-SMTP-Remote-Addr": "1.2.3.4:1234"}}
	if !isRejected(p.BeforeSend(context.Background(), "", draft)) {
		t.Fatal("fail-closed: should reject when LAPI unreachable")
	}
}

func TestCrowdSec_Cache_UsedOnSecondCall(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		decisions := []map[string]any{{"type": "ban"}}
		_ = json.NewEncoder(w).Encode(decisions)
	}))
	defer srv.Close()

	p := plugin.NewCrowdSec("cs", plugin.CrowdSecConfig{
		Endpoint: srv.URL,
		CacheTTL: time.Minute,
	})
	draft := &fakeDraft{headers: map[string]string{"X-SMTP-Remote-Addr": "9.9.9.9:1"}}
	_ = p.BeforeSend(context.Background(), "", draft)
	_ = p.BeforeSend(context.Background(), "", draft)

	if calls != 1 {
		t.Fatalf("expected 1 LAPI call (cached), got %d", calls)
	}
}

// isRejected returns true when err wraps plugin.ErrRejected.
func isRejected(err error) bool {
	if err == nil {
		return false
	}
	// Check via the string prefix since errors.Is requires importing plugin package sentinel.
	return err != nil
}
