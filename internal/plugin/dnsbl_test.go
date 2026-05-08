package plugin_test

import (
	"context"
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/plugin"
)

func TestDNSBL_NoZones(t *testing.T) {
	p := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{})
	err := p.BeforeSend(context.Background(), "", &fakeDraft{
		headers: map[string]string{"X-SMTP-Remote-Addr": "1.2.3.4:1234"},
	})
	if err != nil {
		t.Errorf("expected nil with no zones, got %v", err)
	}
}

func TestDNSBL_NoRemoteAddr(t *testing.T) {
	p := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{Zones: []string{"zen.spamhaus.org"}})
	err := p.BeforeSend(context.Background(), "", &fakeDraft{headers: map[string]string{}})
	if err != nil {
		t.Errorf("expected nil when X-SMTP-Remote-Addr absent, got %v", err)
	}
}

func TestDNSBL_IPv6Skipped(t *testing.T) {
	p := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{Zones: []string{"zen.spamhaus.org"}})
	err := p.BeforeSend(context.Background(), "", &fakeDraft{
		headers: map[string]string{"X-SMTP-Remote-Addr": "[::1]:25"},
	})
	if err != nil {
		t.Errorf("expected nil for IPv6 (not yet supported), got %v", err)
	}
}

func TestDNSBL_FailOpen_DNSError(t *testing.T) {
	// Non-existent zone guarantees a DNS error; FailOpen=true should allow.
	p := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{
		Zones:    []string{"nonexistent.invalid.zone.example.test"},
		CacheTTL: time.Second,
		FailOpen: true,
	})
	err := p.BeforeSend(context.Background(), "", &fakeDraft{
		headers: map[string]string{"X-SMTP-Remote-Addr": "1.2.3.4:25"},
	})
	if err != nil {
		t.Errorf("FailOpen=true should allow on DNS error, got %v", err)
	}
}

func TestDNSBL_PluginLifecycle(t *testing.T) {
	p := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{Zones: []string{"z.example"}})
	ctx := context.Background()
	if err := p.Init(ctx); err != nil {
		t.Errorf("Init: %v", err)
	}
	if p.Name() != "dnsbl" {
		t.Errorf("Name: got %q, want %q", p.Name(), "dnsbl")
	}
	if err := p.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := p.AfterSend(ctx, "", nil); err != nil {
		t.Errorf("AfterSend: %v", err)
	}
}
