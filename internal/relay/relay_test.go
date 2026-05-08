package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/rbaliyan/postbox/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// relayTestBackend is a configurable Backend stub for relay_test.go.
// Named distinctly to avoid conflict with stubBackend in queue_test.go.
type relayTestBackend struct {
	name    string
	sendErr error
	calls   int
}

func (b *relayTestBackend) Name() string { return b.name }
func (b *relayTestBackend) Send(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
	b.calls++
	return b.sendErr
}

// --- RelayServer ---

func TestRelayServer_StartAndAddr(t *testing.T) {
	s := New(&relayTestBackend{name: "test"})
	if err := s.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s.Addr() == "" {
		t.Fatal("Addr() should be non-empty after Start")
	}
}

func TestRelayServer_StartIdempotent(t *testing.T) {
	s := New(&relayTestBackend{name: "test"})
	if err := s.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	addr1 := s.Addr()
	if err := s.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if s.Addr() != addr1 {
		t.Errorf("second Start changed address: %s → %s", addr1, s.Addr())
	}
}

func TestRelayServer_AddrBeforeStart(t *testing.T) {
	s := New(&relayTestBackend{name: "test"})
	if addr := s.Addr(); addr != "" {
		t.Errorf("Addr before Start should be empty, got %q", addr)
	}
}

func TestRelayServer_StopBeforeStart(t *testing.T) {
	s := New(&relayTestBackend{name: "test"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s.Stop(ctx) // should not panic
}

func startRelayServer(t *testing.T, b Backend) (*RelayServer, *grpc.ClientConn, mailboxpb.MailboxServiceClient) {
	t.Helper()
	s := New(b)
	if err := s.Start("127.0.0.1:0"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		s.Stop(ctx)
	})
	conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return s, conn, mailboxpb.NewMailboxServiceClient(conn)
}

func TestRelayServer_SendMessage_Delivered(t *testing.T) {
	b := &relayTestBackend{name: "test"}
	_, _, client := startRelayServer(t, b)

	_, err := client.SendMessage(context.Background(), &mailboxpb.SendMessageRequest{
		UserId:       "alice@example.com",
		RecipientIds: []string{"bob@example.com"},
		Subject:      "Test",
		Body:         "Hello",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if b.calls != 1 {
		t.Errorf("backend.Send called %d times, want 1", b.calls)
	}
}

func TestRelayServer_SendMessage_PreferDeliverTo(t *testing.T) {
	b := &relayTestBackend{name: "test"}
	_, _, client := startRelayServer(t, b)

	_, err := client.SendMessage(context.Background(), &mailboxpb.SendMessageRequest{
		UserId:       "alice@example.com",
		RecipientIds: []string{"wrong@example.com"},
		DeliverTo:    []string{"correct@example.com"},
		Subject:      "Test",
		Body:         "Hello",
	})
	if err != nil {
		t.Fatalf("SendMessage with DeliverTo: %v", err)
	}
	if b.calls != 1 {
		t.Errorf("backend.Send called %d times, want 1", b.calls)
	}
}

func TestRelayServer_SendMessage_EmptyRecipients(t *testing.T) {
	b := &relayTestBackend{name: "test"}
	_, _, client := startRelayServer(t, b)

	_, err := client.SendMessage(context.Background(), &mailboxpb.SendMessageRequest{
		UserId:  "alice@example.com",
		Subject: "Test",
		Body:    "Hello",
	})
	if err != nil {
		t.Fatalf("no-recipients should succeed silently: %v", err)
	}
	if b.calls != 0 {
		t.Errorf("backend.Send should not be called with no recipients")
	}
}

func TestRelayServer_SendMessage_BodyTooLarge(t *testing.T) {
	// The gRPC transport rejects very large messages with ResourceExhausted
	// before the handler's InvalidArgument check (25 MiB) is reached, since
	// grpc.NewServer defaults to a 4 MiB max receive size.
	_, _, client := startRelayServer(t, &relayTestBackend{name: "test"})

	bigBody := make([]byte, maxRelayBodyBytes+1)
	_, err := client.SendMessage(context.Background(), &mailboxpb.SendMessageRequest{
		UserId:       "alice@example.com",
		RecipientIds: []string{"bob@example.com"},
		Body:         string(bigBody),
	})
	if err == nil {
		t.Fatal("expected error for oversized body")
	}
	code := status.Code(err)
	if code != codes.InvalidArgument && code != codes.ResourceExhausted {
		t.Errorf("expected InvalidArgument or ResourceExhausted, got %v", code)
	}
}

func TestRelayServer_SendMessage_BackendError(t *testing.T) {
	b := &relayTestBackend{name: "test", sendErr: errors.New("smtp down")}
	_, _, client := startRelayServer(t, b)

	_, err := client.SendMessage(context.Background(), &mailboxpb.SendMessageRequest{
		UserId:       "alice@example.com",
		RecipientIds: []string{"bob@example.com"},
		Subject:      "Test",
		Body:         "Hello",
	})
	if err == nil {
		t.Fatal("expected error when backend fails")
	}
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("expected Internal, got %v", code)
	}
}

// --- FallbackRegistry ---

type fakeRegistry struct {
	lookupResult string
	lookupErr    error
}

func (r *fakeRegistry) Lookup(_ context.Context, _ string) (string, error) {
	return r.lookupResult, r.lookupErr
}
func (r *fakeRegistry) Register(_ context.Context, _ registry.Entity) error { return nil }

func TestFallbackRegistry_LookupFound(t *testing.T) {
	inner := &fakeRegistry{lookupResult: "node-1"}
	fr := NewFallbackRegistry(inner, "relay")
	nodeID, err := fr.Lookup(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if nodeID != "node-1" {
		t.Errorf("got %q, want %q", nodeID, "node-1")
	}
}

func TestFallbackRegistry_LookupFallsBackOnNotFound(t *testing.T) {
	inner := &fakeRegistry{lookupErr: registry.ErrNotFound}
	fr := NewFallbackRegistry(inner, "relay")
	nodeID, err := fr.Lookup(context.Background(), "unknown@external.com")
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if nodeID != "relay" {
		t.Errorf("got %q, want %q", nodeID, "relay")
	}
}

func TestFallbackRegistry_LookupPropagatesOtherErrors(t *testing.T) {
	inner := &fakeRegistry{lookupErr: errors.New("db error")}
	fr := NewFallbackRegistry(inner, "relay")
	_, err := fr.Lookup(context.Background(), "alice@example.com")
	if err == nil {
		t.Fatal("expected error propagation, got nil")
	}
}

func TestFallbackRegistry_RegisterDelegates(t *testing.T) {
	inner := &fakeRegistry{}
	fr := NewFallbackRegistry(inner, "relay")
	if err := fr.Register(context.Background(), registry.Entity{Type: registry.EntityDomain, Name: "example.com"}); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

// --- StaticResolver ---

type fakeNodeResolver struct {
	result string
	err    error
}

func (r *fakeNodeResolver) ResolveNode(_ context.Context, _ string) (string, error) {
	return r.result, r.err
}

func TestStaticResolver_KnownNode(t *testing.T) {
	sr := NewStaticResolver(map[string]string{"relay": "127.0.0.1:9000"}, nil)
	addr, err := sr.ResolveNode(context.Background(), "relay")
	if err != nil {
		t.Fatalf("ResolveNode: %v", err)
	}
	if addr != "127.0.0.1:9000" {
		t.Errorf("got %q, want %q", addr, "127.0.0.1:9000")
	}
}

func TestStaticResolver_UnknownNode_NoBase(t *testing.T) {
	sr := NewStaticResolver(map[string]string{}, nil)
	_, err := sr.ResolveNode(context.Background(), "unknown")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestStaticResolver_UnknownNode_FallsBackToBase(t *testing.T) {
	base := &fakeNodeResolver{result: "192.168.1.1:50051"}
	sr := NewStaticResolver(map[string]string{}, base)
	addr, err := sr.ResolveNode(context.Background(), "node-2")
	if err != nil {
		t.Fatalf("ResolveNode: %v", err)
	}
	if addr != "192.168.1.1:50051" {
		t.Errorf("got %q, want %q", addr, "192.168.1.1:50051")
	}
}

func TestStaticResolver_UnknownNode_BaseError(t *testing.T) {
	base := &fakeNodeResolver{err: registry.ErrNotFound}
	sr := NewStaticResolver(map[string]string{}, base)
	_, err := sr.ResolveNode(context.Background(), "missing")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// --- SMTPBackend ---

func TestSMTPBackend_Name(t *testing.T) {
	b := NewSMTP(SMTPConfig{Host: "smtp.example.com:587"})
	if b.Name() != "smtp" {
		t.Errorf("Name() = %q, want %q", b.Name(), "smtp")
	}
}

func TestSMTPBackend_DefaultFrom(t *testing.T) {
	b := NewSMTP(SMTPConfig{From: "noreply@example.com"})
	if b.DefaultFrom() != "noreply@example.com" {
		t.Errorf("DefaultFrom() = %q, want %q", b.DefaultFrom(), "noreply@example.com")
	}
}

func TestSMTPBackend_DefaultTimeout(t *testing.T) {
	b := NewSMTP(SMTPConfig{})
	if b.cfg.Timeout != 30*time.Second {
		t.Errorf("default timeout = %v, want 30s", b.cfg.Timeout)
	}
}

// --- buildRFC5322 ---

func TestBuildRFC5322_ContainsHeaders(t *testing.T) {
	raw := buildRFC5322("alice@example.com", []string{"bob@example.com"}, "Test", "Hello", nil)
	msg := string(raw)
	for _, want := range []string{
		"From: alice@example.com",
		"To: bob@example.com",
		"Subject: Test",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !containsSubstr(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
}

func TestBuildRFC5322_CustomContentType(t *testing.T) {
	raw := buildRFC5322("a@b.com", []string{"c@d.com"}, "s", "b", map[string]string{
		"Content-Type": "text/html; charset=utf-8",
	})
	if !containsSubstr(string(raw), "Content-Type: text/html; charset=utf-8") {
		t.Errorf("custom Content-Type not honoured; got:\n%s", string(raw))
	}
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
