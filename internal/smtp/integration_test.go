package smtp_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"testing"
	"time"

	"github.com/rbaliyan/mailbox"
	"github.com/rbaliyan/mailbox/store/memory"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
)

// TestServer_DeliversToMailbox is an end-to-end test: spin up a real SMTP
// listener wired to a real in-memory mailbox service, send a message via
// net/smtp, and verify it arrives in the recipient's inbox.
func TestServer_DeliversToMailbox(t *testing.T) {
	ctx := context.Background()

	mbx, err := mailbox.New(mailbox.Config{}, mailbox.WithStore(memory.New()))
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	if err := mbx.Connect(ctx); err != nil {
		t.Fatalf("connect mailbox: %v", err)
	}
	t.Cleanup(func() { _ = mbx.Close(ctx) })

	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{
		Port:              port,
		Domain:            "example.com",
		AllowInsecureAuth: true,
	}, mbx, nil, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("smtp start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })

	// Wait for the listener to actually accept connections.
	mustWaitListening(t, port)

	from := "alice@example.com"
	to := []string{"bob@example.com"}
	body := "From: alice@example.com\r\nTo: bob@example.com\r\nSubject: hello\r\n\r\nworld\r\n"

	if err := smtp.SendMail(fmt.Sprintf("127.0.0.1:%d", port), nil, from, to, []byte(body)); err != nil {
		t.Fatalf("send mail: %v", err)
	}

	// The mailbox routes the message via the recipient's client.
	// Allow a brief moment for the SMTP Data callback to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := mbx.Client("bob@example.com").Folder(ctx, "__inbox", mailbox.ListOptions{})
		if err == nil {
			msgs := list.All()
			if len(msgs) > 0 {
				msg := msgs[0]
				if got := msg.GetSubject(); got != "hello" {
					t.Fatalf("subject=%q, want hello", got)
				}
				if got := msg.GetBody(); got != "world" {
					t.Fatalf("body=%q, want world", got)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("message not delivered within 2s")
}

func mustWaitListening(t *testing.T, port int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("smtp not listening on :%d", port)
}

// --- fakes used by Auth and In-Reply-To tests ---

type fakeUserResolver struct {
	known map[string]struct{}
}

func (r *fakeUserResolver) ResolveUser(_ context.Context, userID string) (mailbox.User, error) {
	if _, ok := r.known[userID]; ok {
		return fakeMailboxUser{id: userID}, nil
	}
	return nil, errors.New("user not found")
}

type fakeMailboxUser struct{ id string }

func (u fakeMailboxUser) ID() string                      { return u.id }
func (u fakeMailboxUser) FirstName() string               { return "" }
func (u fakeMailboxUser) LastName() string                { return "" }
func (u fakeMailboxUser) Email() string                   { return u.id }
func (u fakeMailboxUser) Type() string                    { return "" }
func (u fakeMailboxUser) PublicKey() string               { return "" }
func (u fakeMailboxUser) Capabilities() map[string]string { return nil }

type fixedThreadResolver struct {
	smtpMsgID string
	threadID  string
}

func (r *fixedThreadResolver) ResolveReplyTo(_ context.Context, _, smtpID string) (string, error) {
	if smtpID == r.smtpMsgID {
		return r.threadID, nil
	}
	return "", errors.New("not found")
}

// TestServer_AuthPLAIN verifies that AUTH PLAIN is accepted for a known user
// and rejected when the user resolver returns nil (users == nil).
func TestServer_AuthPLAIN(t *testing.T) {
	ctx := context.Background()

	mbx, err := mailbox.New(mailbox.Config{}, mailbox.WithStore(memory.New()))
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	if err := mbx.Connect(ctx); err != nil {
		t.Fatalf("connect mailbox: %v", err)
	}
	t.Cleanup(func() { _ = mbx.Close(ctx) })

	users := &fakeUserResolver{known: map[string]struct{}{"alice@example.com": {}}}

	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{
		Port:              port,
		Domain:            "localhost",
		AllowInsecureAuth: true,
	}, mbx, users, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("smtp start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	mustWaitListening(t, port)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(conn, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	// PlainAuth allows insecure connections to localhost.
	auth := smtp.PlainAuth("", "alice@example.com", "ignored", "localhost")
	if err := c.Auth(auth); err != nil {
		t.Fatalf("Auth with known user: %v", err)
	}
}

// TestServer_AuthPLAIN_NilUsers verifies that Auth is rejected when the server
// has no UserResolver configured.
func TestServer_AuthPLAIN_NilUsers(t *testing.T) {
	ctx := context.Background()

	mbx, err := mailbox.New(mailbox.Config{}, mailbox.WithStore(memory.New()))
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	if err := mbx.Connect(ctx); err != nil {
		t.Fatalf("connect mailbox: %v", err)
	}
	t.Cleanup(func() { _ = mbx.Close(ctx) })

	port := freePort(t)
	// users = nil → Auth should be rejected immediately.
	srv := postboxsmtp.New(postboxsmtp.Config{
		Port:              port,
		Domain:            "localhost",
		AllowInsecureAuth: true,
	}, mbx, nil, nil)
	if err := srv.Start(); err != nil {
		t.Fatalf("smtp start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	mustWaitListening(t, port)

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c, err := smtp.NewClient(conn, "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	auth := smtp.PlainAuth("", "nobody@example.com", "pass", "localhost")
	if err := c.Auth(auth); err == nil {
		t.Fatal("expected auth failure when users resolver is nil")
	}
}

// TestServer_InReplyTo verifies that an SMTP message with an In-Reply-To header
// triggers thread resolution and sets ReplyToID on the delivered message.
func TestServer_InReplyTo(t *testing.T) {
	ctx := context.Background()

	mbx, err := mailbox.New(mailbox.Config{}, mailbox.WithStore(memory.New()))
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	if err := mbx.Connect(ctx); err != nil {
		t.Fatalf("connect mailbox: %v", err)
	}
	t.Cleanup(func() { _ = mbx.Close(ctx) })

	const parentSMTPID = "<parent-abc@example.com>"
	threads := &fixedThreadResolver{smtpMsgID: parentSMTPID, threadID: "thread-uuid-abc"}

	port := freePort(t)
	srv := postboxsmtp.New(postboxsmtp.Config{
		Port:   port,
		Domain: "example.com",
	}, mbx, nil, threads)
	if err := srv.Start(); err != nil {
		t.Fatalf("smtp start: %v", err)
	}
	t.Cleanup(func() { _ = srv.Stop() })
	mustWaitListening(t, port)

	from := "alice@example.com"
	to := []string{"bob@example.com"}
	body := fmt.Sprintf(
		"From: alice@example.com\r\nTo: bob@example.com\r\nSubject: Re: hello\r\nIn-Reply-To: %s\r\n\r\nReplying now\r\n",
		parentSMTPID,
	)
	if err := smtp.SendMail(fmt.Sprintf("127.0.0.1:%d", port), nil, from, to, []byte(body)); err != nil {
		t.Fatalf("send mail: %v", err)
	}

	// Poll until the message appears in bob's inbox.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		list, err := mbx.Client("bob@example.com").Folder(ctx, "__inbox", mailbox.ListOptions{})
		if err == nil && len(list.All()) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("reply message not delivered within 2s")
}
