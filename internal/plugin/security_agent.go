package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// SecurityAgentConfig drives the SecurityAgent plugin.
type SecurityAgentConfig struct {
	// MaxRecipients is the maximum number of recipients per message. 0 = unlimited.
	MaxRecipients int
	// MaxBodyBytes is the maximum plain-text body length in bytes. 0 = unlimited.
	MaxBodyBytes int
	// RatePerSender is the sustained per-sender send rate (messages per second).
	// 0 disables per-sender rate limiting.
	RatePerSender float64
	// BurstPerSender is the burst capacity for the per-sender limiter. Defaults to 1.
	BurstPerSender int
	// SenderTTL is how long an idle sender's limiter entry is kept before eviction.
	// Defaults to 10 minutes when zero.
	SenderTTL time.Duration
	// BlockedExtensions is the list of file extensions (lower-cased, with leading
	// dot) that are not allowed in attachment filenames.
	// Defaults to a built-in dangerous-extensions list when nil.
	BlockedExtensions []string
}

var defaultBlockedExtensions = []string{
	".exe", ".bat", ".cmd", ".sh", ".ps1", ".vbs", ".js",
	".jar", ".msi", ".dll", ".scr", ".pif", ".com",
}

// senderEntry tracks the rate limiter and last-seen timestamp for one sender.
type senderEntry struct {
	lim     *rate.Limiter
	lastSec int64 // Unix seconds of last access
}

// SecurityAgent is a mailbox.SendHook that enforces a battery of security
// checks before a message is stored:
//
//   - per-sender rate limiting
//   - recipient count limit
//   - body size limit
//   - header-injection prevention
//   - dangerous attachment extension blocking
type SecurityAgent struct {
	name   string
	cfg    SecurityAgentConfig
	logger *slog.Logger

	blockedExt map[string]struct{}

	mu      sync.Mutex
	senders map[string]*senderEntry

	stopCh    chan struct{}
	initOnce  sync.Once
	closeOnce sync.Once
}

var _ mailbox.Plugin = (*SecurityAgent)(nil)
var _ mailbox.SendHook = (*SecurityAgent)(nil)

// SecurityAgentOption configures a SecurityAgent.
type SecurityAgentOption func(*SecurityAgent)

// WithSecurityAgentLogger sets the structured logger.
func WithSecurityAgentLogger(l *slog.Logger) SecurityAgentOption {
	return func(sa *SecurityAgent) { sa.logger = l }
}

// NewSecurityAgent creates a SecurityAgent plugin.
// Call Init to start the background eviction goroutine.
func NewSecurityAgent(name string, cfg SecurityAgentConfig, opts ...SecurityAgentOption) *SecurityAgent {
	exts := cfg.BlockedExtensions
	if exts == nil {
		exts = defaultBlockedExtensions
	}
	extSet := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		extSet[strings.ToLower(e)] = struct{}{}
	}

	burst := cfg.BurstPerSender
	if burst < 1 {
		burst = 1
	}

	ttl := cfg.SenderTTL
	if ttl == 0 {
		ttl = 10 * time.Minute
	}
	cfg.SenderTTL = ttl
	cfg.BurstPerSender = burst

	sa := &SecurityAgent{
		name:       name,
		cfg:        cfg,
		logger:     slog.Default(),
		blockedExt: extSet,
		senders:    make(map[string]*senderEntry),
		stopCh:     make(chan struct{}),
	}
	for _, o := range opts {
		o(sa)
	}
	return sa
}

func (sa *SecurityAgent) Name() string { return sa.name }

// Init starts the background eviction goroutine that removes idle sender entries.
// Safe to call multiple times — only the first call starts the goroutine.
func (sa *SecurityAgent) Init(_ context.Context) error {
	sa.initOnce.Do(func() { go sa.evictLoop() })
	return nil
}

// Close stops the background eviction goroutine.
// Safe to call multiple times — only the first call closes the channel.
func (sa *SecurityAgent) Close(_ context.Context) error {
	sa.closeOnce.Do(func() { close(sa.stopCh) })
	return nil
}

func (sa *SecurityAgent) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend runs all security checks in order:
//  1. per-sender rate limit
//  2. recipient count
//  3. body size
//  4. header injection
//  5. dangerous attachment extensions
func (sa *SecurityAgent) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	sender := draft.GetSenderID()

	if sa.cfg.RatePerSender > 0 {
		if !sa.limiterFor(sender).Allow() {
			return reject(sa.name, fmt.Sprintf("sender %q is sending too fast", sender))
		}
	}

	if sa.cfg.MaxRecipients > 0 && len(draft.GetRecipientIDs()) > sa.cfg.MaxRecipients {
		return reject(sa.name, fmt.Sprintf(
			"too many recipients: %d > max %d",
			len(draft.GetRecipientIDs()), sa.cfg.MaxRecipients))
	}

	if sa.cfg.MaxBodyBytes > 0 && len(draft.GetBody()) > sa.cfg.MaxBodyBytes {
		return reject(sa.name, fmt.Sprintf(
			"body size %d exceeds limit of %d bytes",
			len(draft.GetBody()), sa.cfg.MaxBodyBytes))
	}

	for k, v := range draft.GetHeaders() {
		if containsNewline(k) || containsNewline(v) {
			return reject(sa.name, fmt.Sprintf(
				"header %q contains illegal newline characters", k))
		}
	}

	for _, a := range draft.GetAttachments() {
		if ext := blockedExtension(a.GetFilename(), sa.blockedExt); ext != "" {
			return reject(sa.name, fmt.Sprintf(
				"attachment %q has blocked extension %q", a.GetFilename(), ext))
		}
	}

	return nil
}

// limiterFor returns (and lazily creates) the per-sender rate limiter.
func (sa *SecurityAgent) limiterFor(sender string) *rate.Limiter {
	now := time.Now().Unix()
	sa.mu.Lock()
	e, ok := sa.senders[sender]
	if !ok {
		e = &senderEntry{
			lim:     rate.NewLimiter(rate.Limit(sa.cfg.RatePerSender), sa.cfg.BurstPerSender),
			lastSec: now,
		}
		sa.senders[sender] = e
	} else {
		e.lastSec = now
	}
	sa.mu.Unlock()
	return e.lim
}

// evictLoop removes sender entries that have been idle for longer than SenderTTL.
func (sa *SecurityAgent) evictLoop() {
	ticker := time.NewTicker(sa.cfg.SenderTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-sa.stopCh:
			return
		case <-ticker.C:
			sa.evict()
		}
	}
}

func (sa *SecurityAgent) evict() {
	cutoff := time.Now().Add(-sa.cfg.SenderTTL).Unix()
	sa.mu.Lock()
	for k, e := range sa.senders {
		if e.lastSec < cutoff {
			delete(sa.senders, k)
		}
	}
	sa.mu.Unlock()
}

// containsNewline reports whether s contains CR or LF, which enables
// header-injection attacks.
func containsNewline(s string) bool {
	return strings.ContainsAny(s, "\r\n")
}

// blockedExtension scans all dot-separated segments of filename from right
// to left and returns the first blocked extension found, or "" if none.
// Scanning all segments prevents double-extension bypasses like "evil.exe.pdf".
func blockedExtension(filename string, blocked map[string]struct{}) string {
	lower := strings.ToLower(filename)
	for {
		idx := strings.LastIndexByte(lower, '.')
		if idx < 0 {
			return ""
		}
		ext := lower[idx:]
		if _, ok := blocked[ext]; ok {
			return ext
		}
		lower = lower[:idx]
	}
}
