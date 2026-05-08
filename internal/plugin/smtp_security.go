package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// SMTPSecurityConfig drives the SMTPSecurityPlugin.
// All limits default to zero/false (permissive) so the plugin can be enabled
// incrementally; tighten the fields that matter for your deployment.
type SMTPSecurityConfig struct {
	// MaxAuthFailuresPerIP is the number of failed AUTH attempts from a single
	// IP before that IP is locked out for LockoutDuration. 0 = unlimited.
	MaxAuthFailuresPerIP int
	// LockoutDuration is how long a locked-out IP stays blocked. Default 15m.
	LockoutDuration time.Duration
	// IPLookupTTL controls how long idle per-IP counters are retained. Default 1h.
	IPLookupTTL time.Duration

	// EnvelopeSpoofCheck rejects messages where the X-SMTP-Envelope-From
	// internal header does not match the stored SenderID.
	EnvelopeSpoofCheck bool

	// AllowedSenderDomains is an exclusive allowlist of sender domains.
	// Empty means all domains are allowed.
	AllowedSenderDomains []string
	// BlockedSenderDomains is an additive blocklist of sender domains.
	BlockedSenderDomains []string

	// MaxSubjectBytes enforces a maximum subject header length.
	// Default 998 (RFC 5322 hard line limit).
	MaxSubjectBytes int

	// RequireAuthenticatedSender rejects messages where SenderID is empty,
	// which indicates the SMTP session was not authenticated.
	RequireAuthenticatedSender bool
}

// ipEntry tracks failed AUTH attempts and lockout state for one source IP.
type ipEntry struct {
	failures    atomic.Int32
	lockedUntil time.Time
	lastSeen    int64 // Unix seconds; for TTL eviction
}

// LockoutStore persists per-IP AUTH failure counters. The default in-memory
// implementation is replaced by RedisLockoutStore in clustered deployments so
// lockouts survive restarts and are shared across nodes.
type LockoutStore interface {
	// RecordFailure increments the failure counter for ip and locks it out if
	// the threshold is reached. Returns (true, nil) when the IP is now locked
	// out. A non-nil error means the store was unavailable; callers should
	// fail-open and log the error rather than silently ignoring it.
	RecordFailure(ctx context.Context, ip string, max int, window, lockout time.Duration) (bool, error)
	// IsLockedOut reports whether ip is currently under lockout. A non-nil
	// error means the store was unavailable; the returned bool is false.
	IsLockedOut(ctx context.Context, ip string) (bool, error)
}

// SMTPSecurityPlugin is a mailbox.SendHook that enforces SMTP-layer security
// controls not covered by SecurityAgent:
//
//  1. IP lockout on repeated AUTH failures
//  2. Sender domain allowlist/blocklist
//  3. Envelope-vs-From header spoof detection
//  4. Subject length limit
//  5. Requirement that the sender was authenticated
//
// The SMTP server layer is expected to store two internal headers on every
// incoming draft before plugins run:
//   - X-SMTP-Envelope-From: the raw MAIL FROM value
//   - X-SMTP-Remote-Addr: the "IP:port" of the connecting client
//
// These headers are strictly internal and should never be forwarded outside
// the server.
type SMTPSecurityPlugin struct {
	name   string
	cfg    SMTPSecurityConfig
	logger *slog.Logger

	allowedDomains map[string]struct{}
	blockedDomains map[string]struct{}

	// lockoutStore is used when a non-nil store is configured (e.g., Redis).
	// When nil, the plugin falls back to the in-process ipMap below.
	lockoutStore LockoutStore

	mu    sync.Mutex
	ipMap map[string]*ipEntry

	stopCh    chan struct{}
	initOnce  sync.Once
	closeOnce sync.Once
}

var _ mailbox.Plugin = (*SMTPSecurityPlugin)(nil)
var _ mailbox.SendHook = (*SMTPSecurityPlugin)(nil)

// SMTPSecurityOption configures an SMTPSecurityPlugin.
type SMTPSecurityOption func(*SMTPSecurityPlugin)

// WithSMTPSecurityLogger sets the structured logger.
func WithSMTPSecurityLogger(l *slog.Logger) SMTPSecurityOption {
	return func(p *SMTPSecurityPlugin) { p.logger = l }
}

// WithLockoutStore replaces the default in-memory lockout tracker with a
// persistent implementation (e.g., RedisLockoutStore). The store is used for
// all calls to RecordAuthFailure and IsLockedOut.
func WithLockoutStore(s LockoutStore) SMTPSecurityOption {
	return func(p *SMTPSecurityPlugin) { p.lockoutStore = s }
}

// NewSMTPSecurityPlugin creates an SMTPSecurityPlugin.
func NewSMTPSecurityPlugin(name string, cfg SMTPSecurityConfig, opts ...SMTPSecurityOption) *SMTPSecurityPlugin {
	if cfg.LockoutDuration == 0 {
		cfg.LockoutDuration = 15 * time.Minute
	}
	if cfg.IPLookupTTL == 0 {
		cfg.IPLookupTTL = time.Hour
	}
	if cfg.MaxSubjectBytes == 0 {
		cfg.MaxSubjectBytes = 998
	}

	p := &SMTPSecurityPlugin{
		name:           name,
		cfg:            cfg,
		logger:         slog.Default(),
		allowedDomains: toSet(cfg.AllowedSenderDomains),
		blockedDomains: toSet(cfg.BlockedSenderDomains),
		ipMap:          make(map[string]*ipEntry),
		stopCh:         make(chan struct{}),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (p *SMTPSecurityPlugin) Name() string { return p.name }

// Init starts the background eviction goroutine. Safe to call multiple times.
func (p *SMTPSecurityPlugin) Init(_ context.Context) error {
	p.initOnce.Do(func() { go p.evictLoop() })
	return nil
}

// Close stops the background eviction goroutine. Safe to call multiple times.
func (p *SMTPSecurityPlugin) Close(_ context.Context) error {
	p.closeOnce.Do(func() { close(p.stopCh) })
	return nil
}

func (p *SMTPSecurityPlugin) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// RecordAuthFailure records a failed AUTH attempt from remoteAddr ("IP:port"
// or plain IP). Returns true if the IP should now be locked out.
// Call this from the SMTP session's Auth callback on failure.
func (p *SMTPSecurityPlugin) RecordAuthFailure(remoteAddr string) bool {
	if p.cfg.MaxAuthFailuresPerIP == 0 {
		return false
	}
	ip := hostOnly(remoteAddr)

	if p.lockoutStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		locked, err := p.lockoutStore.RecordFailure(ctx, ip, p.cfg.MaxAuthFailuresPerIP, p.cfg.IPLookupTTL, p.cfg.LockoutDuration)
		if err != nil {
			p.logger.Error("smtp security: lockout store unavailable, failure not recorded",
				"plugin", p.name, "ip", ip, "error", err)
			return false
		}
		if locked {
			p.logger.Warn("smtp security: IP locked out after AUTH failures", "plugin", p.name, "ip", ip)
		}
		return locked
	}

	// In-memory fallback.
	now := time.Now()
	p.mu.Lock()
	e, ok := p.ipMap[ip]
	if !ok {
		e = &ipEntry{}
		p.ipMap[ip] = e
	}
	atomic.StoreInt64(&e.lastSeen, now.Unix())
	p.mu.Unlock()

	n := int(e.failures.Add(1))
	if n >= p.cfg.MaxAuthFailuresPerIP {
		p.mu.Lock()
		e.lockedUntil = now.Add(p.cfg.LockoutDuration)
		lockedUntil := e.lockedUntil // capture under lock; read outside is a race
		p.mu.Unlock()
		p.logger.Warn("smtp security: IP locked out after AUTH failures",
			"plugin", p.name, "ip", ip, "failures", n, "locked_until", lockedUntil)
		return true
	}
	return false
}

// IsLockedOut reports whether remoteAddr is currently under AUTH lockout.
func (p *SMTPSecurityPlugin) IsLockedOut(remoteAddr string) bool {
	if p.cfg.MaxAuthFailuresPerIP == 0 {
		return false
	}
	ip := hostOnly(remoteAddr)

	if p.lockoutStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		locked, err := p.lockoutStore.IsLockedOut(ctx, ip)
		if err != nil {
			p.logger.Error("smtp security: lockout store unavailable, allowing IP",
				"plugin", p.name, "ip", ip, "error", err)
			return false
		}
		return locked
	}

	// In-memory fallback — read lockedUntil while holding the mutex to avoid
	// a data race with RecordAuthFailure writing the same field.
	p.mu.Lock()
	e, ok := p.ipMap[ip]
	var lockedUntil time.Time
	if ok {
		lockedUntil = e.lockedUntil
	}
	p.mu.Unlock()
	return ok && time.Now().Before(lockedUntil)
}

// BeforeSend runs SMTP-specific security checks in order:
//  1. IP lockout (cheapest — checked first)
//  2. Sender domain allowlist/blocklist
//  3. Envelope-vs-SenderID spoof check
//  4. Subject length
//  5. Authenticated sender requirement
func (p *SMTPSecurityPlugin) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	headers := draft.GetHeaders()
	senderID := draft.GetSenderID()

	// 1. IP lockout.
	if remoteAddr := headers["X-SMTP-Remote-Addr"]; remoteAddr != "" {
		if p.IsLockedOut(remoteAddr) {
			return reject(p.name, fmt.Sprintf("connection from locked-out IP %s", hostOnly(remoteAddr)))
		}
	}

	// 2. Sender domain checks.
	if senderID != "" {
		domain := senderDomain(senderID)
		if _, blocked := p.blockedDomains[domain]; blocked {
			return reject(p.name, fmt.Sprintf("sender domain %q is blocked", domain))
		}
		if len(p.allowedDomains) > 0 {
			if _, ok := p.allowedDomains[domain]; !ok {
				return reject(p.name, fmt.Sprintf("sender domain %q is not in the allowlist", domain))
			}
		}
	}

	// 3. Envelope spoofing: X-SMTP-Envelope-From must agree with SenderID.
	if p.cfg.EnvelopeSpoofCheck {
		envelopeFrom := parseEmailAddr(headers["X-SMTP-Envelope-From"])
		if envelopeFrom != "" && senderID != "" {
			if !strings.EqualFold(envelopeFrom, senderID) {
				return reject(p.name, fmt.Sprintf(
					"envelope sender %q does not match message sender %q",
					envelopeFrom, senderID))
			}
		}
	}

	// 4. Subject length.
	if p.cfg.MaxSubjectBytes > 0 {
		if subj := draft.GetSubject(); len(subj) > p.cfg.MaxSubjectBytes {
			return reject(p.name, fmt.Sprintf(
				"subject length %d exceeds maximum of %d bytes",
				len(subj), p.cfg.MaxSubjectBytes))
		}
	}

	// 5. Require authenticated sender (SenderID non-empty means Auth succeeded).
	if p.cfg.RequireAuthenticatedSender && senderID == "" {
		return reject(p.name, "unauthenticated sender not permitted")
	}

	return nil
}

func (p *SMTPSecurityPlugin) evictLoop() {
	ticker := time.NewTicker(p.cfg.IPLookupTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.evict()
		}
	}
}

func (p *SMTPSecurityPlugin) evict() {
	cutoff := time.Now().Add(-p.cfg.IPLookupTTL).Unix()
	p.mu.Lock()
	for ip, e := range p.ipMap {
		if atomic.LoadInt64(&e.lastSeen) < cutoff && time.Now().After(e.lockedUntil) {
			delete(p.ipMap, ip)
		}
	}
	p.mu.Unlock()
}

// parseEmailAddr parses "Display Name <email>" and returns the address part.
func parseEmailAddr(header string) string {
	addr, err := mail.ParseAddress(header)
	if err != nil {
		return header
	}
	return addr.Address
}

// senderDomain extracts the lower-cased domain portion of an email address.
func senderDomain(email string) string {
	if at := strings.LastIndex(email, "@"); at >= 0 {
		return strings.ToLower(email[at+1:])
	}
	return ""
}

// hostOnly returns the IP part of "IP:port" or the original string if no port.
func hostOnly(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}
