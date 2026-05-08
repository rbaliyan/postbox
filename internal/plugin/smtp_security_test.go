package plugin_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/plugin"
)

func TestSMTPSecurity_NameInitClose(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		IPLookupTTL: time.Second,
	})
	if p.Name() != "smtp-sec" {
		t.Fatalf("Name()=%q", p.Name())
	}
	ctx := context.Background()
	if err := p.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// Second calls must not panic.
	if err := p.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := p.AfterSend(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestSMTPSecurity_CleanMessage_Passes(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		AllowedSenderDomains: []string{"example.com"},
		EnvelopeSpoofCheck:   true,
		MaxSubjectBytes:      998,
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		subject:  "Hello",
		headers: map[string]string{
			"X-SMTP-Envelope-From": "alice@example.com",
			"X-SMTP-Remote-Addr":   "10.0.0.1:50000",
		},
	}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// --- IP lockout ---

func TestSMTPSecurity_IPLockout_AfterThreshold(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxAuthFailuresPerIP: 3,
		LockoutDuration:      time.Hour,
		IPLookupTTL:          time.Hour,
	})
	for i := 0; i < 3; i++ {
		p.RecordAuthFailure("192.168.1.1:12345")
	}
	if !p.IsLockedOut("192.168.1.1:12345") {
		t.Fatal("expected IP to be locked out after 3 failures")
	}
}

func TestSMTPSecurity_IPLockout_RejectsBeforeSend(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxAuthFailuresPerIP: 1,
		LockoutDuration:      time.Hour,
		IPLookupTTL:          time.Hour,
	})
	p.RecordAuthFailure("192.168.1.2:9999")

	draft := &fakeDraft{
		senderID: "attacker@example.com",
		headers:  map[string]string{"X-SMTP-Remote-Addr": "192.168.1.2:9999"},
	}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for locked-out IP")
	}
}

func TestSMTPSecurity_IPLockout_NotLockedBeforeThreshold(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxAuthFailuresPerIP: 5,
		LockoutDuration:      time.Hour,
		IPLookupTTL:          time.Hour,
	})
	p.RecordAuthFailure("10.0.0.1:1234")
	p.RecordAuthFailure("10.0.0.1:1234")
	if p.IsLockedOut("10.0.0.1:1234") {
		t.Fatal("should not be locked out before threshold")
	}
}

func TestSMTPSecurity_IPLockout_Disabled_WhenZero(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxAuthFailuresPerIP: 0, // disabled
	})
	for i := 0; i < 100; i++ {
		p.RecordAuthFailure("10.0.0.1:1234")
	}
	if p.IsLockedOut("10.0.0.1:1234") {
		t.Fatal("lockout should be disabled when MaxAuthFailuresPerIP=0")
	}
}

// --- Sender domain checks ---

func TestSMTPSecurity_BlockedDomain_Rejects(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		BlockedSenderDomains: []string{"spam.com"},
	})
	draft := &fakeDraft{senderID: "bad@spam.com"}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for blocked domain")
	}
}

func TestSMTPSecurity_AllowedDomain_Passes(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		AllowedSenderDomains: []string{"example.com"},
	})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSMTPSecurity_AllowedDomain_BlocksOther(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		AllowedSenderDomains: []string{"example.com"},
	})
	draft := &fakeDraft{senderID: "bad@other.com"}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for non-allowlisted domain")
	}
}

// --- Envelope spoof check ---

func TestSMTPSecurity_EnvelopeSpoof_Rejects(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		EnvelopeSpoofCheck: true,
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-SMTP-Envelope-From": "mallory@evil.com"},
	}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for spoofed envelope sender")
	}
}

func TestSMTPSecurity_EnvelopeSpoof_Passes_WhenMatch(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		EnvelopeSpoofCheck: true,
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-SMTP-Envelope-From": "alice@example.com"},
	}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

func TestSMTPSecurity_EnvelopeSpoof_Passes_WhenDisplayName(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		EnvelopeSpoofCheck: true,
	})
	// Envelope header includes a display name.
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-SMTP-Envelope-From": `"Alice Smith" <alice@example.com>`},
	}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection for display-name envelope: %v", err)
	}
}

func TestSMTPSecurity_EnvelopeSpoof_Disabled_Passes(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		EnvelopeSpoofCheck: false,
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		headers:  map[string]string{"X-SMTP-Envelope-From": "other@evil.com"},
	}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("spoof check disabled but got rejection: %v", err)
	}
}

// --- Subject length ---

func TestSMTPSecurity_SubjectTooLong_Rejects(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxSubjectBytes: 10,
	})
	draft := &fakeDraft{
		senderID: "alice@example.com",
		subject:  "This subject is definitely longer than ten bytes",
	}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for oversized subject")
	}
}

func TestSMTPSecurity_SubjectWithinLimit_Passes(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxSubjectBytes: 100,
	})
	draft := &fakeDraft{senderID: "alice@example.com", subject: "Short"}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// --- Require authenticated sender ---

func TestSMTPSecurity_RequireAuth_EmptySender_Rejects(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		RequireAuthenticatedSender: true,
	})
	draft := &fakeDraft{senderID: ""}
	if !errors.Is(p.BeforeSend(context.Background(), "", draft), plugin.ErrRejected) {
		t.Fatal("expected rejection for unauthenticated sender")
	}
}

func TestSMTPSecurity_RequireAuth_WithSender_Passes(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		RequireAuthenticatedSender: true,
	})
	draft := &fakeDraft{senderID: "alice@example.com"}
	if err := p.BeforeSend(context.Background(), "", draft); err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
}

// TestSMTPSecurity_IsLockedOutRace runs concurrent RecordAuthFailure and
// IsLockedOut calls to verify the lockedUntil field is accessed safely.
// Run with: go test -race ./internal/plugin/...
func TestSMTPSecurity_IsLockedOutRace(t *testing.T) {
	p := plugin.NewSMTPSecurityPlugin("smtp-sec", plugin.SMTPSecurityConfig{
		MaxAuthFailuresPerIP: 3,
		LockoutDuration:      100 * time.Millisecond,
		IPLookupTTL:          time.Second,
	})

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			p.RecordAuthFailure("10.0.0.1:1234")
		}()
		go func() {
			defer wg.Done()
			p.IsLockedOut("10.0.0.1:1234")
		}()
	}
	wg.Wait()
}
