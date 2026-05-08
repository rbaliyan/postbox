package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"strings"

	"github.com/rbaliyan/mailbox"
	mbxstore "github.com/rbaliyan/mailbox/store"
	"golang.org/x/net/publicsuffix"
)

// EmailAuthConfig configures the EmailAuthPlugin.
//
// The plugin reads the X-SPF-Result, X-DKIM-Result, and X-DKIM-Domain internal
// headers injected by the SMTP server (when CheckSPF / VerifyDKIM are enabled)
// and enforces configurable policies. With those SMTP server features disabled
// all results are "none" and no rejections occur.
//
// Policy values for SPFPolicy, DKIMPolicy, and DMARCPolicy:
//
//	"off"     — disabled; header is ignored (default).
//	"warn"    — log a warning but always allow.
//	"reject"  — reject messages that fail the check.
//	"require" — (SPF/DKIM only) reject when result is none or fail.
type EmailAuthConfig struct {
	// SPFPolicy controls enforcement of the X-SPF-Result header.
	// Default "off".
	SPFPolicy string
	// DKIMPolicy controls enforcement of the X-DKIM-Result header.
	// Default "off".
	DKIMPolicy string
	// DMARCPolicy controls DMARC enforcement. The DNS _dmarc.<from-domain>
	// record is fetched and evaluated; the stricter of the DNS policy and this
	// setting applies (a DNS p=none always passes regardless of this field).
	// Default "off".
	DMARCPolicy string
}

// EmailAuthPlugin is a mailbox.SendHook that enforces SPF and DKIM policy
// based on the X-SPF-Result and X-DKIM-Result internal headers injected by
// the SMTP server. It does not perform verification itself — verification is
// done by the SMTP server and communicated via those headers.
type EmailAuthPlugin struct {
	name   string
	cfg    EmailAuthConfig
	logger *slog.Logger
}

// EmailAuthOption configures an EmailAuthPlugin.
type EmailAuthOption func(*EmailAuthPlugin)

// WithEmailAuthLogger sets the structured logger.
func WithEmailAuthLogger(l *slog.Logger) EmailAuthOption {
	return func(p *EmailAuthPlugin) { p.logger = l }
}

// NewEmailAuth creates an EmailAuthPlugin.
func NewEmailAuth(name string, cfg EmailAuthConfig, opts ...EmailAuthOption) *EmailAuthPlugin {
	p := &EmailAuthPlugin{name: name, cfg: cfg, logger: slog.Default()}
	for _, o := range opts {
		o(p)
	}
	return p
}

var _ mailbox.Plugin = (*EmailAuthPlugin)(nil)
var _ mailbox.SendHook = (*EmailAuthPlugin)(nil)

func (p *EmailAuthPlugin) Name() string                  { return p.name }
func (p *EmailAuthPlugin) Init(_ context.Context) error  { return nil }
func (p *EmailAuthPlugin) Close(_ context.Context) error { return nil }
func (p *EmailAuthPlugin) AfterSend(_ context.Context, _ string, _ mbxstore.Message) error {
	return nil
}

// BeforeSend enforces SPF, DKIM, and DMARC policies based on internal headers.
func (p *EmailAuthPlugin) BeforeSend(_ context.Context, _ string, draft mbxstore.DraftMessage) error {
	headers := draft.GetHeaders()

	if err := p.checkSPF(headers); err != nil {
		return err
	}
	if err := p.checkDKIM(headers); err != nil {
		return err
	}
	return p.checkDMARC(headers)
}

func (p *EmailAuthPlugin) checkSPF(headers map[string]string) error {
	policy := p.cfg.SPFPolicy
	if policy == "" || policy == "off" {
		return nil
	}
	result := headers["X-SPF-Result"]
	failed := result == "fail" || result == "softfail"
	missing := result == "" || result == "none"

	switch policy {
	case "warn":
		if failed || missing {
			p.logger.Warn("email-auth: SPF check did not pass",
				"plugin", p.name, "spf_result", result)
		}
	case "reject":
		if failed {
			return reject(p.name, fmt.Sprintf("SPF check failed: %s", result))
		}
	case "require":
		if missing || failed {
			return reject(p.name, fmt.Sprintf("SPF check required but result is %q", result))
		}
	}
	return nil
}

func (p *EmailAuthPlugin) checkDKIM(headers map[string]string) error {
	policy := p.cfg.DKIMPolicy
	if policy == "" || policy == "off" {
		return nil
	}
	result := headers["X-DKIM-Result"]
	failed := result == "fail"
	missing := result == "" || result == "none"

	switch policy {
	case "warn":
		if failed || missing {
			p.logger.Warn("email-auth: DKIM check did not pass",
				"plugin", p.name, "dkim_result", result)
		}
	case "reject":
		if failed {
			return reject(p.name, fmt.Sprintf("DKIM verification failed: %s", result))
		}
	case "require":
		if missing || failed {
			return reject(p.name, fmt.Sprintf("DKIM required but result is %q", result))
		}
	}
	return nil
}

// checkDMARC evaluates the sender's DMARC DNS record and enforces the policy.
// It passes when DMARC alignment succeeds via SPF or DKIM, or when the DNS
// record specifies p=none. DNS lookup failures are fail-open.
func (p *EmailAuthPlugin) checkDMARC(headers map[string]string) error {
	policy := p.cfg.DMARCPolicy
	if policy == "" || policy == "off" {
		return nil
	}

	// Extract RFC 5322 From domain.
	fromHeader := headers["From"]
	if fromHeader == "" {
		return nil
	}
	fromAddr, err := mail.ParseAddress(fromHeader)
	if err != nil {
		return nil
	}
	parts := strings.SplitN(fromAddr.Address, "@", 2)
	if len(parts) != 2 {
		return nil
	}
	fromDomain := strings.ToLower(parts[1])

	// Fetch _dmarc TXT record.
	txts, err := net.LookupTXT("_dmarc." + fromDomain)
	if err != nil {
		// Fail-open: no DMARC record or DNS error → pass.
		return nil
	}
	dnsPolicy, aspf, adkim := parseDMARCRecord(txts)
	if dnsPolicy == "" || dnsPolicy == "none" {
		// DNS says p=none → always pass regardless of plugin policy.
		return nil
	}

	// Evaluate alignment.
	spfResult := headers["X-SPF-Result"]
	envelopeDomain := domainOf(headers["X-SMTP-Envelope-From"])
	spfPass := spfResult == "pass" && domainMatch(envelopeDomain, fromDomain, aspf)

	dkimResult := headers["X-DKIM-Result"]
	dkimDomain := strings.ToLower(headers["X-DKIM-Domain"])
	dkimPass := dkimResult == "pass" && domainMatch(dkimDomain, fromDomain, adkim)

	if spfPass || dkimPass {
		return nil // DMARC alignment succeeded
	}

	msg := fmt.Sprintf("DMARC failed for %s (dns_policy=%s spf=%s dkim=%s)", fromDomain, dnsPolicy, spfResult, dkimResult)
	switch policy {
	case "warn":
		p.logger.Warn("email-auth: DMARC check failed", "plugin", p.name, "domain", fromDomain,
			"dns_policy", dnsPolicy, "spf", spfResult, "dkim", dkimResult)
	case "reject":
		return reject(p.name, msg)
	}
	return nil
}

// parseDMARCRecord parses a slice of TXT strings for a DMARC record.
// Returns (p, aspf, adkim) tags; aspf and adkim default to "r" (relaxed).
func parseDMARCRecord(txts []string) (p, aspf, adkim string) {
	aspf = "r"
	adkim = "r"
	for _, txt := range txts {
		if !strings.HasPrefix(txt, "v=DMARC1") {
			continue
		}
		for _, tag := range strings.Split(txt, ";") {
			tag = strings.TrimSpace(tag)
			switch {
			case strings.HasPrefix(tag, "p="):
				p = strings.ToLower(strings.TrimPrefix(tag, "p="))
			case strings.HasPrefix(tag, "aspf="):
				aspf = strings.ToLower(strings.TrimPrefix(tag, "aspf="))
			case strings.HasPrefix(tag, "adkim="):
				adkim = strings.ToLower(strings.TrimPrefix(tag, "adkim="))
			}
		}
		break
	}
	return
}

// domainOf extracts the domain portion from an email address.
func domainOf(addr string) string {
	parts := strings.SplitN(addr, "@", 2)
	if len(parts) == 2 {
		return strings.ToLower(parts[1])
	}
	return strings.ToLower(addr)
}

// domainMatch returns true if got aligns with want under the given DMARC mode.
// mode "s" (strict) requires an exact match; mode "r" (relaxed) accepts any
// subdomain of the organisational domain.
func domainMatch(got, want, mode string) bool {
	if got == "" || want == "" {
		return false
	}
	got = strings.ToLower(got)
	want = strings.ToLower(want)
	if strings.EqualFold(got, want) {
		return true
	}
	if mode == "s" {
		return false
	}
	// Relaxed: strip one label from the longer one and compare org domains.
	orgGot := orgDomain(got)
	orgWant := orgDomain(want)
	return orgGot == orgWant
}

// orgDomain returns the organisational (eTLD+1) domain using the public suffix
// list. This handles multi-label TLDs (e.g. co.uk, com.au) correctly.
// Falls back to the last two dot-labels when the PSL lookup fails.
func orgDomain(domain string) string {
	if etld1, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return etld1
	}
	parts := strings.Split(domain, ".")
	if len(parts) <= 2 {
		return domain
	}
	return strings.Join(parts[len(parts)-2:], ".")
}
