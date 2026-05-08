package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
)

// DNSBLConfig configures the DNSBL plugin.
type DNSBLConfig struct {
	// Zones is the list of DNS block-list zones to query
	// (e.g., "zen.spamhaus.org", "bl.spamcop.net").
	Zones []string
	// CacheTTL is how long to cache per-IP decisions. Zero defaults to 5 minutes.
	CacheTTL time.Duration
	// FailOpen allows messages when a DNS query fails. Defaults to true.
	FailOpen bool
}

type dnsblEntry struct {
	banned  bool
	expires time.Time
}

// DNSBLPlugin rejects connections from IPs listed in DNS block-lists.
// It reads the X-SMTP-Remote-Addr internal header injected by the SMTP server.
type DNSBLPlugin struct {
	name   string
	cfg    DNSBLConfig
	logger *slog.Logger

	mu    sync.RWMutex
	cache map[string]dnsblEntry // key: "zone:ip"
}

// DNSBLOption configures a DNSBLPlugin.
type DNSBLOption func(*DNSBLPlugin)

// WithDNSBLLogger sets the structured logger.
func WithDNSBLLogger(l *slog.Logger) DNSBLOption {
	return func(p *DNSBLPlugin) { p.logger = l }
}

// NewDNSBL creates a DNSBLPlugin.
func NewDNSBL(name string, cfg DNSBLConfig, opts ...DNSBLOption) *DNSBLPlugin {
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = 5 * time.Minute
	}
	p := &DNSBLPlugin{
		name:   name,
		cfg:    cfg,
		logger: slog.Default(),
		cache:  make(map[string]dnsblEntry),
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

var _ mailbox.Plugin = (*DNSBLPlugin)(nil)
var _ mailbox.SendHook = (*DNSBLPlugin)(nil)

func (p *DNSBLPlugin) Name() string                  { return p.name }
func (p *DNSBLPlugin) Init(_ context.Context) error  { return nil }
func (p *DNSBLPlugin) Close(_ context.Context) error { return nil }
func (p *DNSBLPlugin) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend checks the connecting IP against all configured DNSBL zones.
func (p *DNSBLPlugin) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	if len(p.cfg.Zones) == 0 {
		return nil
	}
	headers := draft.GetHeaders()
	addr := headers["X-SMTP-Remote-Addr"]
	if addr == "" {
		return nil
	}
	ipStr, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.To4() == nil {
		return nil // only IPv4 for now
	}
	reversed := reverseIPv4(ip.To4())

	for _, zone := range p.cfg.Zones {
		banned, err := p.queryZone(reversed, zone)
		if err != nil {
			if !p.cfg.FailOpen {
				return reject(p.name, fmt.Sprintf("DNSBL lookup failed for %s: %v", ipStr, err))
			}
			p.logger.Warn("dnsbl: lookup error", "plugin", p.name, "ip", ipStr, "zone", zone, "error", err)
			continue
		}
		if banned {
			p.logger.Info("dnsbl: blocked", "plugin", p.name, "ip", ipStr, "zone", zone)
			return reject(p.name, fmt.Sprintf("IP %s is listed in %s", ipStr, zone))
		}
	}
	return nil
}

// queryZone returns true if ip (already reversed) is listed in zone, using the cache.
func (p *DNSBLPlugin) queryZone(reversed, zone string) (bool, error) {
	key := zone + ":" + reversed
	now := time.Now()

	p.mu.RLock()
	if e, ok := p.cache[key]; ok && now.Before(e.expires) {
		p.mu.RUnlock()
		return e.banned, nil
	}
	p.mu.RUnlock()

	host := reversed + "." + zone
	addrs, err := net.LookupHost(host)
	var banned bool
	if err != nil {
		// NXDOMAIN means not listed — treat as clean.
		if dnsErr, ok := err.(*net.DNSError); ok && dnsErr.IsNotFound {
			banned = false
			err = nil
		} else {
			return false, err
		}
	} else {
		banned = len(addrs) > 0
	}

	p.mu.Lock()
	p.cache[key] = dnsblEntry{banned: banned, expires: now.Add(p.cfg.CacheTTL)}
	p.mu.Unlock()
	return banned, nil
}

// reverseIPv4 returns the dotted-decimal octets in reverse order for DNSBL queries.
func reverseIPv4(ip net.IP) string {
	return fmt.Sprintf("%d.%d.%d.%d", ip[3], ip[2], ip[1], ip[0])
}
