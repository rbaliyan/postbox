package smtp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"os"
	"strings"
	"sync"
	"time"

	gosmtp "github.com/emersion/go-smtp"
	"github.com/rbaliyan/mailbox"
)

// LMTPConfig holds LMTP server settings.
//
// LMTP (RFC 2033) is designed for last-mile delivery from a trusted upstream
// MTA (e.g., Postfix, Exim) to a message store. The preferred transport is a
// Unix domain socket; TCP is available when the MTA is on a different host.
type LMTPConfig struct {
	// SocketPath is a Unix domain socket path. When non-empty, the server
	// listens on a Unix socket instead of TCP. Mutually exclusive with Port.
	// Recommended for co-located MTA deployments.
	SocketPath string
	// Port is the TCP port. Used only when SocketPath is empty. Default 2424.
	Port int
	// BindAddr is the TCP bind address. Default "" (all interfaces).
	// Set to "127.0.0.1" to restrict to loopback only (recommended for TCP mode).
	BindAddr string
	// Domain is the LMTP banner domain. Default "localhost".
	Domain string
	// MaxMessageBytes caps incoming message size. Default 10 MiB.
	MaxMessageBytes int64
	// MaxRecipients caps the RCPT TO count per message. Default 100.
	MaxRecipients int
	ReadTimeout   time.Duration
	WriteTimeout  time.Duration
}

func (c *LMTPConfig) applyDefaults() {
	if c.Port == 0 && c.SocketPath == "" {
		c.Port = 2424
	}
	if c.Domain == "" {
		c.Domain = "localhost"
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = 10 << 20
	}
	if c.MaxRecipients == 0 {
		c.MaxRecipients = 100
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 5 * time.Minute
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 60 * time.Second
	}
}

// LMTPServer is an LMTP listener (RFC 2033) that delivers messages directly
// into the mailbox service. It is intended for use with an upstream MTA that
// has already performed spam filtering, DKIM validation, and recipient
// verification. No AUTH is required — LMTP is a local, trusted protocol.
//
// Per-recipient delivery status is reported via StatusCollector so the upstream
// MTA can handle partial delivery and bounce generation correctly.
type LMTPServer struct {
	cfg     LMTPConfig
	mailbox mailbox.Service
	threads ThreadResolver

	mu         sync.Mutex
	srv        *gosmtp.Server
	lis        net.Listener
	running    bool
	done       chan struct{}
	socketPath string // tracks the active socket path for cleanup on stop
}

// ErrLMTPAlreadyRunning is returned by LMTPServer.Start when already running.
var ErrLMTPAlreadyRunning = errors.New("lmtp: server already running")

// ErrLMTPNotRunning is returned by LMTPServer.Stop when not running.
var ErrLMTPNotRunning = errors.New("lmtp: server not running")

// NewLMTP creates an LMTPServer. Call Start to begin accepting connections.
func NewLMTP(cfg LMTPConfig, mbx mailbox.Service, threads ThreadResolver) *LMTPServer {
	cfg.applyDefaults()
	return &LMTPServer{cfg: cfg, mailbox: mbx, threads: threads}
}

// Start binds the listener and begins accepting connections in a background
// goroutine. When using a Unix socket, any stale socket file is removed first.
func (s *LMTPServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return ErrLMTPAlreadyRunning
	}

	srv := gosmtp.NewServer(s)
	srv.LMTP = true
	srv.Domain = s.cfg.Domain
	srv.MaxMessageBytes = s.cfg.MaxMessageBytes
	srv.MaxRecipients = s.cfg.MaxRecipients
	srv.ReadTimeout = s.cfg.ReadTimeout
	srv.WriteTimeout = s.cfg.WriteTimeout

	var (
		lis        net.Listener
		err        error
		socketPath string
	)
	if s.cfg.SocketPath != "" {
		_ = os.Remove(s.cfg.SocketPath) // remove stale socket
		lis, err = net.Listen("unix", s.cfg.SocketPath)
		if err != nil {
			return fmt.Errorf("lmtp: listen unix %s: %w", s.cfg.SocketPath, err)
		}
		if err := os.Chmod(s.cfg.SocketPath, 0600); err != nil {
			_ = lis.Close()
			return fmt.Errorf("lmtp: set socket permissions %s: %w", s.cfg.SocketPath, err)
		}
		socketPath = s.cfg.SocketPath
	} else {
		addr := fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.Port)
		lis, err = net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("lmtp: listen %s: %w", addr, err)
		}
	}

	s.srv = srv
	s.lis = lis
	s.socketPath = socketPath
	s.running = true
	done := make(chan struct{})
	s.done = done

	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			if s.socketPath != "" {
				_ = os.Remove(s.socketPath)
				s.socketPath = ""
			}
			s.mu.Unlock()
			close(done)
		}()
		_ = srv.Serve(lis)
	}()
	return nil
}

// Stop closes the listener and waits for the accept loop to exit.
func (s *LMTPServer) Stop() error {
	s.mu.Lock()
	srv := s.srv
	lis := s.lis
	done := s.done
	running := s.running
	s.mu.Unlock()

	if !running {
		return ErrLMTPNotRunning
	}
	var firstErr error
	if lis != nil {
		if err := lis.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			firstErr = err
		}
	}
	if err := srv.Close(); err != nil && firstErr == nil && !errors.Is(err, net.ErrClosed) {
		firstErr = err
	}
	if done != nil {
		<-done
	}
	return firstErr
}

// IsRunning reports whether the server is accepting connections.
func (s *LMTPServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Port returns the configured TCP port (0 when using a Unix socket).
func (s *LMTPServer) Port() int { return s.cfg.Port }

// Domain returns the configured LMTP banner domain.
func (s *LMTPServer) Domain() string { return s.cfg.Domain }

// SocketPath returns the configured Unix socket path (empty when using TCP).
func (s *LMTPServer) SocketPath() string { return s.cfg.SocketPath }

// NewSession implements gosmtp.Backend.
func (s *LMTPServer) NewSession(conn *gosmtp.Conn) (gosmtp.Session, error) {
	var remoteAddr string
	if conn != nil {
		remoteAddr = conn.Conn().RemoteAddr().String()
	}
	return &lmtpSession{srv: s, remoteAddr: remoteAddr}, nil
}

// lmtpSession holds per-connection LMTP state. It implements both
// gosmtp.Session and gosmtp.LMTPSession for per-recipient delivery status.
type lmtpSession struct {
	srv        *LMTPServer
	remoteAddr string
	from       string
	to         []string
}

var _ gosmtp.Session = (*lmtpSession)(nil)
var _ gosmtp.LMTPSession = (*lmtpSession)(nil)

// Mail implements gosmtp.Session. Accepts any envelope sender.
func (s *lmtpSession) Mail(from string, _ *gosmtp.MailOptions) error {
	s.from = from
	return nil
}

// Rcpt implements gosmtp.Session. Accepts any recipient; the upstream MTA is
// responsible for confirming the address is local before delivery.
func (s *lmtpSession) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

// LMTPData implements gosmtp.LMTPSession. Delivers the message and sets a
// per-recipient status via the StatusCollector so the upstream MTA can handle
// partial delivery and bounce generation correctly.
func (s *lmtpSession) LMTPData(r io.Reader, status gosmtp.StatusCollector) error {
	err := s.deliverMessage(r)
	for _, rcpt := range s.to {
		status.SetStatus(rcpt, err)
	}
	return nil
}

// Data implements gosmtp.Session as a fallback; normally LMTPData is called.
func (s *lmtpSession) Data(r io.Reader) error {
	return s.deliverMessage(r)
}

func (s *lmtpSession) deliverMessage(r io.Reader) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg, err := mail.ReadMessage(r)
	if err != nil {
		return fmt.Errorf("lmtp: parse message: %w", err)
	}

	const bodyReadCap = 20 << 20
	body, err := io.ReadAll(io.LimitReader(msg.Body, bodyReadCap))
	if err != nil {
		return fmt.Errorf("lmtp: read body: %w", err)
	}
	if int64(len(body)) >= bodyReadCap {
		return &gosmtp.SMTPError{
			Code:         552,
			EnhancedCode: gosmtp.EnhancedCode{5, 3, 4},
			Message:      "Message body too large",
		}
	}

	subject := sanitiseHeaderValue(msg.Header.Get("Subject"), 998)
	smtpMessageID := sanitiseMessageID(msg.Header.Get("Message-ID"), 995)
	inReplyTo := sanitiseMessageID(msg.Header.Get("In-Reply-To"), 995)

	from := s.from
	if from == "" {
		from = extractAddr(msg.Header.Get("From"))
	}

	req := mailbox.SendRequest{
		RecipientIDs: s.to,
		Subject:      subject,
		Body:         strings.TrimRight(string(body), "\r\n"),
		ExternalID:   smtpMessageID,
		Headers: map[string]string{
			"X-SMTP-Envelope-From": s.from,
			"X-LMTP-Remote-Addr":   s.remoteAddr,
		},
	}

	if inReplyTo != "" && s.srv.threads != nil && len(s.to) > 0 {
		if parentID, err := s.srv.threads.ResolveReplyTo(ctx, s.to[0], inReplyTo); err == nil {
			req.ReplyToID = parentID
		}
	}

	_, err = s.srv.mailbox.Client(from).SendMessage(ctx, req)
	return err
}

func (s *lmtpSession) Reset() {
	s.from = ""
	s.to = nil
}

func (s *lmtpSession) Logout() error { return nil }
