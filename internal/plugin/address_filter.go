package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// FilterMode controls whether AddressFilter rules form an allowlist or a blocklist.
type FilterMode uint8

const (
	// ModeBlock (blacklist): reject the message if any monitored address
	// matches at least one rule pattern.
	ModeBlock FilterMode = iota
	// ModeAllow (whitelist): reject the message if any monitored address
	// does NOT match at least one rule pattern.
	ModeAllow
)

// AddressField is a bitmask selecting which address fields a rule applies to.
type AddressField uint8

const (
	FieldSender     AddressField = 1 << iota // the envelope sender
	FieldRecipients                          // all recipients
	// FieldAll applies to both sender and recipients.
	FieldAll = FieldSender | FieldRecipients
)

// AddressRule is a single regex-based address matching rule.
type AddressRule struct {
	// ID uniquely identifies the rule for runtime add/remove operations.
	ID string
	// Pattern is a Go regular expression matched against the full email address.
	Pattern string
	// Field controls which address field(s) this rule applies to.
	Field AddressField
}

// RuleStore persists AddressFilter rules so they survive restarts.
// Pass nil to keep rules in memory only.
type RuleStore interface {
	ListRules(ctx context.Context) ([]AddressRule, error)
	SaveRule(ctx context.Context, r AddressRule) error
	DeleteRule(ctx context.Context, id string) error
}

// compiledRule caches the precompiled regexp alongside the original rule.
type compiledRule struct {
	AddressRule
	re *regexp.Regexp
}

// addrEntry pairs an address with the field it was drawn from.
type addrEntry struct {
	addr  string
	field AddressField
}

// AddressFilter is a mailbox.SendHook that allows or blocks messages based on
// regex-matched sender and recipient addresses.
//
// Thread-safe: AddRule, RemoveRule and Reload may be called concurrently with
// ongoing message deliveries.
type AddressFilter struct {
	name   string
	mode   FilterMode
	store  RuleStore
	logger *slog.Logger

	mu       sync.RWMutex
	compiled []compiledRule
}

var _ mailbox.Plugin = (*AddressFilter)(nil)
var _ mailbox.SendHook = (*AddressFilter)(nil)

// AddressFilterOption configures an AddressFilter.
type AddressFilterOption func(*AddressFilter)

// WithAddressFilterLogger sets the structured logger.
func WithAddressFilterLogger(l *slog.Logger) AddressFilterOption {
	return func(f *AddressFilter) { f.logger = l }
}

// NewAddressFilter creates an AddressFilter.
//   - name:   plugin identifier returned by Name().
//   - mode:   ModeBlock (blacklist) or ModeAllow (whitelist).
//   - rs:     optional persistent store; nil → in-memory rules only.
func NewAddressFilter(name string, mode FilterMode, rs RuleStore, opts ...AddressFilterOption) *AddressFilter {
	f := &AddressFilter{
		name:   name,
		mode:   mode,
		store:  rs,
		logger: slog.Default(),
	}
	for _, o := range opts {
		o(f)
	}
	return f
}

func (f *AddressFilter) Name() string { return f.name }

// Init loads rules from the backing store (if any) and compiles their patterns.
func (f *AddressFilter) Init(ctx context.Context) error { return f.reload(ctx) }

// Close is a no-op; address filtering has no background resources.
func (f *AddressFilter) Close(_ context.Context) error { return nil }

// AddRule adds a rule at runtime and persists it when a store is configured.
// Returns an error immediately if Pattern does not compile.
func (f *AddressFilter) AddRule(ctx context.Context, r AddressRule) error {
	if _, err := regexp.Compile(r.Pattern); err != nil {
		return fmt.Errorf("address filter: invalid pattern %q: %w", r.Pattern, err)
	}
	if f.store != nil {
		if err := f.store.SaveRule(ctx, r); err != nil {
			return fmt.Errorf("address filter: save rule: %w", err)
		}
	} else {
		// In-memory path: append directly without going through the store.
		re := regexp.MustCompile(r.Pattern)
		f.mu.Lock()
		f.compiled = append(f.compiled, compiledRule{AddressRule: r, re: re})
		f.mu.Unlock()
		return nil
	}
	return f.reload(ctx)
}

// RemoveRule removes a rule by ID at runtime and persists the change when a
// store is configured.
func (f *AddressFilter) RemoveRule(ctx context.Context, id string) error {
	if f.store != nil {
		if err := f.store.DeleteRule(ctx, id); err != nil {
			return fmt.Errorf("address filter: delete rule: %w", err)
		}
		return f.reload(ctx)
	}
	// In-memory path: remove from slice.
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.compiled {
		if c.ID != id {
			f.compiled[n] = c
			n++
		}
	}
	f.compiled = f.compiled[:n]
	return nil
}

// Reload re-fetches rules from the store and recompiles. Safe to call concurrently.
func (f *AddressFilter) Reload(ctx context.Context) error { return f.reload(ctx) }

func (f *AddressFilter) reload(ctx context.Context) error {
	if f.store == nil {
		return nil
	}
	rules, err := f.store.ListRules(ctx)
	if err != nil {
		return fmt.Errorf("address filter: list rules: %w", err)
	}
	compiled := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			f.logger.Warn("address filter: skipping rule with invalid pattern",
				"id", r.ID, "pattern", r.Pattern, "error", err)
			continue
		}
		compiled = append(compiled, compiledRule{AddressRule: r, re: re})
	}
	f.mu.Lock()
	f.compiled = compiled
	f.mu.Unlock()
	return nil
}

// BeforeSend checks the sender and recipients against the compiled rules.
// Returns ErrRejected (via reject()) if the filter trips.
func (f *AddressFilter) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	f.mu.RLock()
	rules := f.compiled
	f.mu.RUnlock()

	if len(rules) == 0 {
		return nil
	}

	addrs := make([]addrEntry, 0, 1+len(draft.GetRecipientIDs()))
	addrs = append(addrs, addrEntry{draft.GetSenderID(), FieldSender})
	for _, r := range draft.GetRecipientIDs() {
		addrs = append(addrs, addrEntry{r, FieldRecipients})
	}

	switch f.mode {
	case ModeBlock:
		return f.checkBlock(addrs, rules)
	case ModeAllow:
		return f.checkAllow(addrs, rules)
	}
	return nil
}

func (f *AddressFilter) checkBlock(addrs []addrEntry, rules []compiledRule) error {
	for _, a := range addrs {
		for _, r := range rules {
			if r.Field&a.field == 0 {
				continue
			}
			if r.re.MatchString(a.addr) {
				return reject(f.name, fmt.Sprintf("address %q is blocked", a.addr))
			}
		}
	}
	return nil
}

func (f *AddressFilter) checkAllow(addrs []addrEntry, rules []compiledRule) error {
	for _, a := range addrs {
		applicable := false
		allowed := false
		for _, r := range rules {
			if r.Field&a.field == 0 {
				continue
			}
			applicable = true
			if r.re.MatchString(a.addr) {
				allowed = true
				break
			}
		}
		// If no rule covers this address field at all, skip the address rather
		// than defaulting to deny — a sender-only allowlist should not block recipients.
		if applicable && !allowed {
			return reject(f.name, fmt.Sprintf("address %q is not in the allowlist", a.addr))
		}
	}
	return nil
}

// AfterSend is a no-op; address filtering acts only before storage.
func (f *AddressFilter) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// MemRuleStore is a thread-safe in-memory RuleStore suitable for tests and
// deployments that do not require persistence across restarts.
type MemRuleStore struct {
	mu    sync.RWMutex
	rules map[string]AddressRule
}

// NewMemRuleStore creates a MemRuleStore optionally seeded with initial rules.
func NewMemRuleStore(initial ...AddressRule) *MemRuleStore {
	m := &MemRuleStore{rules: make(map[string]AddressRule, len(initial))}
	for _, r := range initial {
		m.rules[r.ID] = r
	}
	return m
}

func (m *MemRuleStore) ListRules(_ context.Context) ([]AddressRule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]AddressRule, 0, len(m.rules))
	for _, r := range m.rules {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemRuleStore) SaveRule(_ context.Context, r AddressRule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rules[r.ID] = r
	return nil
}

func (m *MemRuleStore) DeleteRule(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rules, id)
	return nil
}
