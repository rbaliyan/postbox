// Package smtp provides an embedded SMTP listener that routes received
// messages into a mailbox service.
//
// Security model:
//   - AUTH requires a UserResolver; passwords are validated via an optional
//     CredentialValidator (pass nil to accept any password for the registered
//     user — only do this on private, trusted networks).
//   - MAIL FROM is rejected if AUTH is configured and the sender has not
//     authenticated. After authentication, the envelope sender is validated
//     against the authenticated identity to prevent spoofing.
//   - RCPT TO is validated against the UserResolver so unknown recipients are
//     rejected without revealing information about the user population.
//     When RelayEnabled is true, RCPT TO validation is skipped and any address
//     is accepted (open relay behaviour — only enable on trusted networks).
//   - Connection-level rate limiting is applied globally and per source IP.
//   - Hard connection concurrency cap via netutil.LimitListener.
//   - Internal headers (X-SMTP-Envelope-From, X-SMTP-Remote-Addr) are injected
//     into the draft before BeforeSend plugins run and are stripped before
//     passing to external consumers.
//
// Timeout guidance:
//   - ReadTimeout covers the time the server waits for a client to send data.
//     RFC 5321 recommends 5 minutes for the DATA body phase, which sets the
//     floor for slow clients or large messages. Default: 5 minutes.
//   - WriteTimeout covers how long the server waits to flush a response to a
//     slow client. Default: 60 seconds.
package smtp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/mail"
	"strings"
	"sync"
	"time"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
	"github.com/rbaliyan/mailbox"
	"golang.org/x/net/netutil"
	"golang.org/x/time/rate"
)

// ErrAlreadyRunning is returned by Start when the server is already running.
var ErrAlreadyRunning = errors.New("smtp: server already running")

// ErrNotRunning is returned by Stop when the server is not running.
var ErrNotRunning = errors.New("smtp: server not running")

// ThreadResolver maps an SMTP In-Reply-To value (SMTP Message-ID of the parent)
// to the mailbox UUID of that parent message, enabling SMTP reply chains to be
// linked inside the mailbox thread model.
//
// Implementations should return ("", store.ErrNotFound) when no match exists.
type ThreadResolver interface {
	ResolveReplyTo(ctx context.Context, recipientID, smtpMessageID string) (string, error)
}

// ThreadResolverFunc adapts a function to the ThreadResolver interface.
type ThreadResolverFunc func(ctx context.Context, recipientID, smtpMessageID string) (string, error)

// ResolveReplyTo implements ThreadResolver.
func (f ThreadResolverFunc) ResolveReplyTo(ctx context.Context, recipientID, smtpMessageID string) (string, error) {
	return f(ctx, recipientID, smtpMessageID)
}

// CredentialValidator checks whether the given username/password pair is valid.
// If nil, any non-empty username that resolves via UserResolver is accepted
// (useful for internal/trusted deployments only).
type CredentialValidator interface {
	ValidatePassword(ctx context.Context, username, password string) error
}

// AuthFailureHook is an optional callback invoked on every failed AUTH attempt.
// Implementations can track per-IP failure counts and signal lockout.
// SMTPSecurityPlugin satisfies this interface when its MaxAuthFailuresPerIP > 0.
type AuthFailureHook interface {
	// RecordAuthFailure increments the failure counter for remoteAddr and
	// returns true if the IP should now be locked out.
	RecordAuthFailure(remoteAddr string) bool
	// IsLockedOut reports whether remoteAddr is currently locked out.
	IsLockedOut(remoteAddr string) bool
}

// Config holds all SMTP server settings. Zero values are replaced by sane
// defaults when the server is constructed.
type Config struct {
	// Port is the TCP port to listen on. Default 2525.
	Port int
	// BindAddr is the interface address to bind to. Default "" (all interfaces).
	// Set to "127.0.0.1" to restrict to loopback only.
	BindAddr string
	// Domain is the SMTP banner domain. Default "localhost".
	Domain string

	// AllowInsecureAuth permits AUTH over a plain-text connection.
	// Defaults to false — do not enable on public networks.
	AllowInsecureAuth bool

	// MaxMessageBytes caps incoming message size. Default 10 MiB.
	MaxMessageBytes int64
	// MaxRecipients caps the RCPT TO count per message. Default 50.
	MaxRecipients int
	// MaxConnections is the maximum concurrent open connections. Default 1000.
	MaxConnections int

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	// RelayEnabled disables RCPT TO validation against the UserResolver,
	// accepting any recipient address. Only enable on trusted private networks.
	// Default false (closed relay).
	RelayEnabled bool

	// TLSCertFile and TLSKeyFile enable STARTTLS when both are set.
	// The server advertises STARTTLS in EHLO and upgrades on demand.
	// TLS 1.2 is the minimum version enforced.
	TLSCertFile string
	TLSKeyFile  string

	// AuthFailureHook is called on each failed AUTH attempt. When non-nil,
	// the server also checks IsLockedOut before accepting any auth attempt.
	// Wire an SMTPSecurityPlugin here to activate its IP-lockout feature.
	AuthFailureHook AuthFailureHook

	// MaxAuthFailuresPerSession disconnects with 421 after N failed AUTH
	// attempts in a single connection. 0 = unlimited (not recommended).
	// Default 5.
	MaxAuthFailuresPerSession int

	// CheckSPF enables SPF verification during MAIL FROM. The result is
	// injected as the X-SPF-Result internal header for plugin enforcement.
	CheckSPF bool

	// VerifyDKIM enables DKIM signature verification on inbound messages.
	// Results are injected as X-DKIM-Result for plugin enforcement.
	VerifyDKIM bool

	// Global token-bucket rate limiting on new connections.
	// MaxConnsPerSec is the sustained rate; 0 disables.
	MaxConnsPerSec float64
	// BurstConns is the burst capacity (defaults to 10 when limiting is on).
	BurstConns int

	// Per-IP rate limiting.
	// MaxConnsPerSecPerIP is the sustained rate per source IP; 0 disables.
	MaxConnsPerSecPerIP float64
	// BurstConnsPerIP is the burst capacity per source IP. Default 5.
	BurstConnsPerIP int
}

func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = 2525
	}
	if c.Domain == "" {
		c.Domain = "localhost"
	}
	if c.MaxMessageBytes == 0 {
		c.MaxMessageBytes = 10 << 20
	}
	if c.MaxRecipients == 0 {
		c.MaxRecipients = 50
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 1000
	}
	if c.ReadTimeout == 0 {
		c.ReadTimeout = 5 * time.Minute
	}
	if c.WriteTimeout == 0 {
		c.WriteTimeout = 60 * time.Second
	}
	if c.MaxConnsPerSec > 0 && c.BurstConns == 0 {
		c.BurstConns = 10
	}
	if c.MaxConnsPerSecPerIP > 0 && c.BurstConnsPerIP == 0 {
		c.BurstConnsPerIP = 5
	}
	if c.MaxAuthFailuresPerSession == 0 {
		c.MaxAuthFailuresPerSession = 5
	}
}

// Server wraps go-smtp and routes received messages into a mailbox service.
// Start and Stop are safe to call concurrently.
type Server struct {
	cfg       Config
	mailbox   mailbox.Service
	users     mailbox.UserResolver
	validator CredentialValidator
	threads   ThreadResolver
	limiter   *rate.Limiter

	// Per-IP rate limiters — each entry created lazily in NewSession.
	ipLimiters sync.Map // key: string IP → *rate.Limiter

	mu      sync.Mutex
	srv     *gosmtp.Server
	lis     net.Listener
	running bool
	// done is closed once the Serve goroutine returns, so Stop can wait for
	// in-flight accept loops to exit before returning.
	done chan struct{}
}

// New creates a Server. Call Start to begin accepting connections.
//   - users may be nil to disable AUTH (every attempt will be rejected).
//   - validator may be nil to skip password validation (trusted networks only).
//   - threads may be nil to disable SMTP reply-chain threading.
func New(cfg Config, mbx mailbox.Service, users mailbox.UserResolver, threads ThreadResolver) *Server {
	return NewWithValidator(cfg, mbx, users, nil, threads)
}

// NewWithValidator is like New but accepts a CredentialValidator for real
// password checking. Pass nil for validator to accept any password.
func NewWithValidator(cfg Config, mbx mailbox.Service, users mailbox.UserResolver, validator CredentialValidator, threads ThreadResolver) *Server {
	cfg.applyDefaults()

	var lim *rate.Limiter
	if cfg.MaxConnsPerSec > 0 {
		lim = rate.NewLimiter(rate.Limit(cfg.MaxConnsPerSec), cfg.BurstConns)
	}

	return &Server{
		cfg:       cfg,
		mailbox:   mbx,
		users:     users,
		validator: validator,
		threads:   threads,
		limiter:   lim,
	}
}

// Start binds the TCP listener and begins accepting SMTP connections in a
// background goroutine. It returns an error immediately if the port cannot be
// bound, so callers can distinguish a bad config from a runtime failure.
func (s *Server) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return ErrAlreadyRunning
	}

	srv := gosmtp.NewServer(s)
	srv.Addr = fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.Port)
	srv.Domain = s.cfg.Domain
	srv.AllowInsecureAuth = s.cfg.AllowInsecureAuth
	srv.MaxMessageBytes = s.cfg.MaxMessageBytes
	srv.MaxRecipients = s.cfg.MaxRecipients
	srv.ReadTimeout = s.cfg.ReadTimeout
	srv.WriteTimeout = s.cfg.WriteTimeout

	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		cert, err := tls.LoadX509KeyPair(s.cfg.TLSCertFile, s.cfg.TLSKeyFile)
		if err != nil {
			return fmt.Errorf("smtp: load TLS certificate: %w", err)
		}
		srv.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	lis, err := net.Listen("tcp", fmt.Sprintf("%s:%d", s.cfg.BindAddr, s.cfg.Port))
	if err != nil {
		return fmt.Errorf("smtp: listen %s:%d: %w", s.cfg.BindAddr, s.cfg.Port, err)
	}
	lis = netutil.LimitListener(lis, s.cfg.MaxConnections)

	s.srv = srv
	s.lis = lis
	s.running = true
	done := make(chan struct{})
	s.done = done

	go func() {
		defer func() {
			s.mu.Lock()
			s.running = false
			s.mu.Unlock()
			close(done)
		}()
		_ = srv.Serve(lis)
	}()

	return nil
}

// Stop closes the listener and waits for the accept loop to exit. In-flight
// sessions are aborted by go-smtp. After Stop returns, IsRunning is false.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.srv
	lis := s.lis
	done := s.done
	running := s.running
	s.mu.Unlock()

	if !running {
		return ErrNotRunning
	}
	// Close the listener first so Accept unblocks even if go-smtp.Serve has
	// not yet registered the listener internally (small race on startup).
	// Then call srv.Close to drop any open connections. gosmtp.Server.Close
	// also closes the listener it tracks, so a second close on the same fd
	// returning net.ErrClosed is expected and not surfaced.
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

// IsRunning reports whether the server is currently accepting connections.
func (s *Server) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Port returns the configured listen port.
func (s *Server) Port() int { return s.cfg.Port }

// Domain returns the configured banner domain.
func (s *Server) Domain() string { return s.cfg.Domain }

// NewSession implements gosmtp.Backend. Applies global and per-IP rate
// limiters before creating the session.
func (s *Server) NewSession(conn *gosmtp.Conn) (gosmtp.Session, error) {
	if s.limiter != nil && !s.limiter.Allow() {
		return nil, &gosmtp.SMTPError{
			Code:         421,
			EnhancedCode: gosmtp.EnhancedCode{4, 4, 5},
			Message:      "Too many connections — please try again later",
		}
	}

	var remoteAddr string
	if conn != nil {
		remoteAddr = conn.Conn().RemoteAddr().String()
		if s.cfg.MaxConnsPerSecPerIP > 0 {
			ip, _, _ := net.SplitHostPort(remoteAddr)
			if ip == "" {
				ip = remoteAddr
			}
			lim, _ := s.ipLimiters.LoadOrStore(ip,
				rate.NewLimiter(rate.Limit(s.cfg.MaxConnsPerSecPerIP), s.cfg.BurstConnsPerIP))
			if !lim.(*rate.Limiter).Allow() {
				return nil, &gosmtp.SMTPError{
					Code:         421,
					EnhancedCode: gosmtp.EnhancedCode{4, 4, 5},
					Message:      "Too many connections from your IP — please try again later",
				}
			}
		}
	}

	return &session{srv: s, remoteAddr: remoteAddr}, nil
}

// session holds per-connection SMTP state.
type session struct {
	srv          *Server
	remoteAddr   string // "IP:port" of the remote client
	authedUser   string // set on successful AUTH; empty if AUTH never completed
	from         string
	to           []string
	authFailures int    // failed AUTH attempts this connection
	spfResult    string // "pass", "fail", "softfail", "neutral", "none"; set in Mail()
}

// Ensure session satisfies both Session and AuthSession.
var _ gosmtp.Session = (*session)(nil)
var _ gosmtp.AuthSession = (*session)(nil)

// AuthMechanisms advertises PLAIN authentication.
func (s *session) AuthMechanisms() []string { return []string{sasl.Plain} }

// Auth validates the sender against the user registry using SASL PLAIN.
// If a CredentialValidator is configured, the password is also checked.
// Sets s.authedUser on success so Mail() can enforce sender identity.
func (s *session) Auth(mech string) (sasl.Server, error) {
	if mech != sasl.Plain {
		return nil, gosmtp.ErrAuthUnknownMechanism
	}
	if s.srv.users == nil {
		return nil, gosmtp.ErrAuthRequired
	}
	// Reject immediately if the source IP is already locked out.
	if hook := s.srv.cfg.AuthFailureHook; hook != nil && hook.IsLockedOut(s.remoteAddr) {
		return nil, &gosmtp.SMTPError{
			Code:         421,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 0},
			Message:      "Too many authentication failures — try again later",
		}
	}
	return sasl.NewPlainServer(func(identity, username, password string) error {
		email := username
		if identity != "" {
			email = identity
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.srv.users.ResolveUser(ctx, email); err != nil {
			return s.authFailed()
		}
		if s.srv.validator != nil {
			if err := s.srv.validator.ValidatePassword(ctx, email, password); err != nil {
				return s.authFailed()
			}
		}
		s.authedUser = email
		return nil
	}), nil
}

// authFailed records a failed AUTH attempt and returns the appropriate error.
// Returns a 421 (close connection) error once the per-session limit is reached.
func (s *session) authFailed() error {
	s.authFailures++
	if hook := s.srv.cfg.AuthFailureHook; hook != nil {
		hook.RecordAuthFailure(s.remoteAddr)
	}
	if s.srv.cfg.MaxAuthFailuresPerSession > 0 && s.authFailures >= s.srv.cfg.MaxAuthFailuresPerSession {
		return &gosmtp.SMTPError{
			Code:         421,
			EnhancedCode: gosmtp.EnhancedCode{4, 7, 0},
			Message:      "Too many authentication failures — connection closing",
		}
	}
	return gosmtp.ErrAuthRequired
}

// Mail implements gosmtp.Session. Requires authentication when a UserResolver
// is configured, and validates that the envelope sender matches the
// authenticated identity to prevent spoofing. Optionally checks SPF.
func (s *session) Mail(from string, _ *gosmtp.MailOptions) error {
	// Require AUTH before MAIL FROM when users are configured.
	if s.srv.users != nil && s.authedUser == "" {
		return &gosmtp.SMTPError{
			Code:         530,
			EnhancedCode: gosmtp.EnhancedCode{5, 7, 0},
			Message:      "Authentication required",
		}
	}
	// Prevent envelope spoofing: envelope sender must match authenticated user.
	if s.authedUser != "" {
		fromAddr := extractAddr(from)
		if !strings.EqualFold(fromAddr, s.authedUser) {
			return &gosmtp.SMTPError{
				Code:         553,
				EnhancedCode: gosmtp.EnhancedCode{5, 7, 1},
				Message:      "Sender address does not match authenticated user",
			}
		}
	}
	// SPF check: run asynchronously-safe synchronous DNS lookup.
	if s.srv.cfg.CheckSPF && s.remoteAddr != "" {
		if ipStr, _, err := net.SplitHostPort(s.remoteAddr); err == nil {
			if ip := net.ParseIP(ipStr); ip != nil {
				result, _ := spf.CheckHostWithSender(ip, senderDomain(extractAddr(from)), extractAddr(from))
				s.spfResult = string(result)
			}
		}
	}
	s.from = from
	return nil
}

// Rcpt implements gosmtp.Session. When RelayEnabled is false (default), validates
// the recipient against the UserResolver to prevent recipient harvesting.
func (s *session) Rcpt(to string, _ *gosmtp.RcptOptions) error {
	if s.srv.users != nil && !s.srv.cfg.RelayEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if _, err := s.srv.users.ResolveUser(ctx, extractAddr(to)); err != nil {
			// Return a generic 550 rather than 551 to avoid revealing whether
			// the user exists vs. is simply undeliverable.
			return &gosmtp.SMTPError{
				Code:         550,
				EnhancedCode: gosmtp.EnhancedCode{5, 1, 1},
				Message:      "Recipient not found",
			}
		}
	}
	s.to = append(s.to, to)
	return nil
}

// internalHeaders are injected by the SMTP layer for plugin use and stripped
// before messages are exposed to external consumers.
var internalHeaders = []string{
	"X-SMTP-Envelope-From",
	"X-SMTP-Remote-Addr",
	"X-SPF-Result",
	"X-DKIM-Result",
	"X-DKIM-Domain",
}

// Data implements gosmtp.Session. Reads and routes the message body.
func (s *session) Data(r io.Reader) error {
	// Create context before reading — the 30s window covers the full operation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Tee the reader so we can pass the raw bytes to DKIM verification after
	// mail.ReadMessage consumes the headers and msg.Body consumes the body.
	var rawBuf bytes.Buffer
	teeR := io.TeeReader(r, &rawBuf)

	msg, err := mail.ReadMessage(teeR)
	if err != nil {
		return fmt.Errorf("smtp: parse message: %w", err)
	}

	// Read body with a hard cap to prevent zip-bomb / expansion attacks.
	// go-smtp enforces MaxMessageBytes on the raw wire; this is a secondary
	// guard on the decoded body after MIME parsing.
	const bodyReadCap = 20 << 20 // 20 MiB — 2× the default MaxMessageBytes
	body, err := io.ReadAll(io.LimitReader(msg.Body, bodyReadCap))
	if err != nil {
		return fmt.Errorf("smtp: read body: %w", err)
	}
	if int64(len(body)) >= bodyReadCap {
		return &gosmtp.SMTPError{
			Code:         552,
			EnhancedCode: gosmtp.EnhancedCode{5, 3, 4},
			Message:      "Message body too large",
		}
	}

	// DKIM verification on the complete raw message (headers + body).
	dkimResult := "none"
	dkimDomain := ""
	if s.srv.cfg.VerifyDKIM && rawBuf.Len() > 0 {
		verifications, verifyErr := dkim.Verify(bytes.NewReader(rawBuf.Bytes()))
		if verifyErr == nil && len(verifications) > 0 {
			allPass := true
			for _, v := range verifications {
				if v.Err != nil {
					allPass = false
				} else if dkimDomain == "" {
					dkimDomain = v.Domain
				}
			}
			if allPass {
				dkimResult = "pass"
			} else {
				dkimResult = "fail"
			}
		}
	}

	subject := sanitiseHeaderValue(msg.Header.Get("Subject"), 998)
	smtpMessageID := sanitiseMessageID(msg.Header.Get("Message-ID"), 995)
	inReplyTo := sanitiseMessageID(msg.Header.Get("In-Reply-To"), 995)

	// Determine the effective sender: prefer authenticated identity, then
	// envelope sender, then From: header as last resort.
	from := s.authedUser
	if from == "" {
		from = s.from
	}
	if from == "" {
		from = extractAddr(msg.Header.Get("From"))
	}

	req := mailbox.SendRequest{
		RecipientIDs: s.to,
		Subject:      subject,
		Body:         strings.TrimRight(string(body), "\r\n"),
		ExternalID:   smtpMessageID,
		// Inject internal headers so BeforeSend plugins can access the
		// envelope sender, remote address, and email auth results.
		// These are stripped from internalHeaders before reaching external consumers.
		Headers: map[string]string{
			"X-SMTP-Envelope-From": s.from,
			"X-SMTP-Remote-Addr":   s.remoteAddr,
			"X-SPF-Result":         s.spfResult,
			"X-DKIM-Result":        dkimResult,
			"X-DKIM-Domain":        dkimDomain,
		},
	}

	// Resolve In-Reply-To → mailbox UUID to link thread chains.
	if inReplyTo != "" && s.srv.threads != nil && len(s.to) > 0 {
		if parentID, err := s.srv.threads.ResolveReplyTo(ctx, s.to[0], inReplyTo); err == nil {
			req.ReplyToID = parentID
		}
	}

	_, err = s.srv.mailbox.Client(from).SendMessage(ctx, req)
	return err
}

func (s *session) Reset() {
	s.from = ""
	s.to = nil
}

func (s *session) Logout() error { return nil }

// senderDomain extracts the lower-cased domain portion of an email address.
func senderDomain(email string) string {
	if at := strings.LastIndex(email, "@"); at >= 0 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}

// extractAddr parses "Display Name <email>" and returns just the address part.
func extractAddr(header string) string {
	addr, err := mail.ParseAddress(header)
	if err != nil {
		return header
	}
	return addr.Address
}

// sanitiseHeaderValue strips control characters and enforces a max byte length.
// RFC 5322 limits header field bodies to 998 characters per line.
func sanitiseHeaderValue(s string, maxBytes int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		// Allow printable ASCII, space, and tab; reject all control characters.
		if r == '\t' || (r >= 0x20 && r != 0x7f) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxBytes {
		return out[:maxBytes]
	}
	return out
}

// sanitiseMessageID applies sanitiseHeaderValue and additionally strips
// characters outside printable ASCII (no tabs, no high Unicode).
func sanitiseMessageID(s string, maxBytes int) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x21 && r <= 0x7e {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > maxBytes {
		return out[:maxBytes]
	}
	return out
}
