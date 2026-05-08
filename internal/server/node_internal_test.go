package server

import (
	"context"
	"errors"
	"testing"

	"github.com/rbaliyan/mailbox"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
)

func TestDefaultSMTPFactory(t *testing.T) {
	lc := defaultSMTPFactory(postboxsmtp.Config{Port: 2525, Domain: "test"}, nil, SMTPDeps{})
	if lc == nil {
		t.Fatal("expected non-nil SMTPLifecycle")
	}
}

func TestStoreMailboxUser_Methods(t *testing.T) {
	u := storeMailboxUser{u: store.User{
		Email:        "alice@example.com",
		Type:         "agent",
		PublicKeyB64: "keypub123",
		Metadata:     map[string]string{"skill": "web-search"},
	}}

	if got := u.ID(); got != "alice@example.com" {
		t.Errorf("ID() = %q, want %q", got, "alice@example.com")
	}
	if got := u.Type(); got != "agent" {
		t.Errorf("Type() = %q, want %q", got, "agent")
	}
	if got := u.PublicKey(); got != "keypub123" {
		t.Errorf("PublicKey() = %q", got)
	}
	caps := u.Capabilities()
	if caps["skill"] != "web-search" {
		t.Errorf("Capabilities() = %v, want web-search", caps)
	}
}

func TestStoreUserResolver_NotFound(t *testing.T) {
	ctx := context.Background()
	s := memstore.New()
	if err := s.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Close(ctx) //nolint:errcheck
	r := storeUserResolver{s}
	_, err := r.ResolveUser(ctx, "nobody@example.com")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestBuildThreadResolver_NotFound(t *testing.T) {
	ctx := context.Background()
	mbx, err := newMailboxFactory(nil)(ctx, nil)
	if err != nil {
		t.Fatalf("newMailboxFactory(nil): %v", err)
	}
	defer mbx.Close(ctx) //nolint:errcheck

	resolver := buildThreadResolver(mbx)
	_, err = resolver.ResolveReplyTo(ctx, "nobody@example.com", "<missing-id@example.com>")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v, want store.ErrNotFound", err)
	}
}

func TestBuildThreadResolver_Found(t *testing.T) {
	ctx := context.Background()
	mbx, err := newMailboxFactory(nil)(ctx, nil)
	if err != nil {
		t.Fatalf("newMailboxFactory(nil): %v", err)
	}
	defer mbx.Close(ctx) //nolint:errcheck

	const externalID = "<msg-123@smtp.example.com>"
	if _, err := mbx.Client("sender@example.com").SendMessage(ctx, mailbox.SendRequest{
		RecipientIDs: []string{"recipient@example.com"},
		Subject:      "Thread starter",
		Body:         "Hello",
		ExternalID:   externalID,
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	resolver := buildThreadResolver(mbx)
	id, err := resolver.ResolveReplyTo(ctx, "recipient@example.com", externalID)
	if err != nil {
		t.Fatalf("ResolveReplyTo: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty message ID")
	}
}
