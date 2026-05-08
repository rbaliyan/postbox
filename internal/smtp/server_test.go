package smtp_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
)

func TestServer_StartStopRoundTrip(t *testing.T) {
	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{Port: port, Domain: "test"}, nil, nil, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !srv.IsRunning() {
		t.Fatal("expected running after start")
	}

	// Confirm the listener actually accepts a TCP connection.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("localhost:%d", port), time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()

	if err := srv.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if srv.IsRunning() {
		t.Fatal("still running after stop")
	}
}

func TestServer_StartTwiceFails(t *testing.T) {
	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{Port: port, Domain: "test"}, nil, nil, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("first start: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	if err := srv.Start(); !errors.Is(err, postboxsmtp.ErrAlreadyRunning) {
		t.Fatalf("got %v, want ErrAlreadyRunning", err)
	}
}

func TestServer_StartFailsOnTakenPort(t *testing.T) {
	// Hold the port on the same address family / wildcard interface that the
	// SMTP server uses so the bind genuinely conflicts.
	lis, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck
	port := lis.Addr().(*net.TCPAddr).Port

	srv := postboxsmtp.New(postboxsmtp.Config{Port: port, Domain: "test"}, nil, nil, nil)
	if err := srv.Start(); err == nil {
		_ = srv.Stop()
		t.Fatal("expected bind error on taken port")
	}
}

func TestServer_StopWhenNotRunning(t *testing.T) {
	srv := postboxsmtp.New(postboxsmtp.Config{Port: 25, Domain: "test"}, nil, nil, nil)
	if err := srv.Stop(); !errors.Is(err, postboxsmtp.ErrNotRunning) {
		t.Fatalf("got %v, want ErrNotRunning", err)
	}
}

func TestServer_PortAndDomain(t *testing.T) {
	srv := postboxsmtp.New(postboxsmtp.Config{Port: 4242, Domain: "mail.example.com"}, nil, nil, nil)
	if srv.Port() != 4242 {
		t.Fatalf("port=%d", srv.Port())
	}
	if srv.Domain() != "mail.example.com" {
		t.Fatalf("domain=%q", srv.Domain())
	}
}

func TestServer_AppliesDefaults(t *testing.T) {
	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{Port: port}, nil, nil, nil)
	if srv.Domain() != "localhost" {
		t.Fatalf("default domain=%q", srv.Domain())
	}
}

// freePort returns an OS-allocated TCP port.
func freePort(t *testing.T) int {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	if err := lis.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// Sanity: the package's error messages contain the expected prefix so that
// callers grepping logs don't break.
func TestErrorMessages(t *testing.T) {
	for _, e := range []error{postboxsmtp.ErrAlreadyRunning, postboxsmtp.ErrNotRunning} {
		if !strings.HasPrefix(e.Error(), "smtp: ") {
			t.Fatalf("error %q missing smtp: prefix", e)
		}
	}
}

func TestThreadResolverFunc_ResolveReplyTo(t *testing.T) {
	called := false
	fn := postboxsmtp.ThreadResolverFunc(func(_ context.Context, recipientID, smtpID string) (string, error) {
		called = true
		return "thread-uuid-" + smtpID, nil
	})
	_, err := fn.ResolveReplyTo(context.Background(), "user@example.com", "msg-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("expected the underlying func to be called")
	}
}

func TestServer_NewSession_RateLimited(t *testing.T) {
	// Burst=1 so the first call consumes the only token; the second must be rejected.
	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{
		Port:           port,
		Domain:         "test",
		MaxConnsPerSec: 0.0001, // very slow refill
		BurstConns:     1,
	}, nil, nil, nil)

	// Consume the single burst token.
	if _, err := srv.NewSession(nil); err != nil {
		t.Fatalf("first NewSession (should succeed): %v", err)
	}
	// Now the limiter is exhausted — next call must be rejected.
	if _, err := srv.NewSession(nil); err == nil {
		t.Fatal("expected rate-limit error on second NewSession")
	}
}

func TestExtractAddr(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"alice@example.com", "alice@example.com"},
		{`"Alice Smith" <alice@example.com>`, "alice@example.com"},
		{"not-an-email-at-all", "not-an-email-at-all"},
	}
	for _, tt := range tests {
		got := postboxsmtp.ExtractAddr(tt.input)
		if got != tt.want {
			t.Errorf("ExtractAddr(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}
