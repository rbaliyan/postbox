package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rbaliyan/mailbox"
	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	mbxserver "github.com/rbaliyan/mailbox/server"
	"github.com/rbaliyan/postbox/internal/guard"
	"github.com/rbaliyan/postbox/internal/mbxstore"
	"github.com/rbaliyan/postbox/internal/observability"
	"github.com/rbaliyan/postbox/internal/plugin"
	"github.com/rbaliyan/postbox/internal/relay"
	"github.com/rbaliyan/postbox/internal/server"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/store/sqlite"
	"github.com/rbaliyan/postbox/internal/webhook"
	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"github.com/redis/go-redis/v9"
	"github.com/spf13/cobra"
	"go.opentelemetry.io/otel"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
)

// shutdownTimeout caps how long graceful shutdown is allowed to take when the
// server receives SIGINT/SIGTERM. Once exceeded, the deferred Close is forced.
const shutdownTimeout = 10 * time.Second

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the Postbox gRPC server",
	RunE:  runServe,
}

func init() {
	serveCmd.Flags().String("port", "50051", "gRPC listen port")
	serveCmd.Flags().String("mode", "standalone", "node mode: standalone | temp")
	serveCmd.Flags().String("db", "postbox.db", "SQLite database path (ignored when --mode=temp)")
	serveCmd.Flags().String("node-id", "", "node ID (auto-generated if empty)")
	serveCmd.Flags().String("host", "localhost", "publicly reachable hostname for Discover responses")

	// SMTP flags — only active when --smtp-enabled is set.
	serveCmd.Flags().Bool("smtp-enabled", false, "start SMTP listener on startup")
	serveCmd.Flags().Int("smtp-port", 2525, "SMTP listen port")
	serveCmd.Flags().String("smtp-bind-addr", "", "SMTP bind address (default all interfaces; use 127.0.0.1 to restrict)")
	serveCmd.Flags().String("smtp-domain", "localhost", "SMTP banner domain")
	// --smtp-insecure-auth defaults to false. Enable only on private networks.
	serveCmd.Flags().Bool("smtp-insecure-auth", false, "allow AUTH over plain-text connections (unsafe)")
	serveCmd.Flags().Int64("smtp-max-msg-bytes", 0, "max message size in bytes (0 = 10 MiB default)")
	serveCmd.Flags().Int("smtp-max-recipients", 0, "max recipients per message (0 = 50 default)")
	serveCmd.Flags().Int("smtp-max-connections", 0, "max concurrent SMTP connections (0 = 1000 default)")
	serveCmd.Flags().Int("smtp-read-timeout-secs", 0, "SMTP read timeout in seconds (0 = 60s default)")
	serveCmd.Flags().Int("smtp-write-timeout-secs", 0, "SMTP write timeout in seconds (0 = 60s default)")
	serveCmd.Flags().Float64("smtp-rate-conns", 0, "max new connections per second globally (0 = unlimited)")
	serveCmd.Flags().Int("smtp-rate-burst", 0, "global connection rate burst capacity (0 = default 10)")
	serveCmd.Flags().Float64("smtp-rate-conns-per-ip", 0, "max new connections per second per source IP (0 = unlimited)")
	serveCmd.Flags().Int("smtp-rate-burst-per-ip", 0, "per-IP connection rate burst capacity (0 = default 5)")
	serveCmd.Flags().Bool("smtp-relay-enabled", false, "accept RCPT TO for any address without user validation (open relay — trusted networks only)")

	// LMTP flags — for local delivery from an upstream MTA (e.g. Postfix).
	// Prefer --lmtp-socket-path over TCP for co-located deployments.
	serveCmd.Flags().Bool("lmtp-enabled", false, "start LMTP listener for local MTA delivery (RFC 2033)")
	serveCmd.Flags().String("lmtp-socket-path", "", "Unix socket path for LMTP (recommended; mutually exclusive with --lmtp-port)")
	serveCmd.Flags().Int("lmtp-port", 2424, "LMTP TCP listen port (used when --lmtp-socket-path is empty)")
	serveCmd.Flags().String("lmtp-bind-addr", "127.0.0.1", "LMTP TCP bind address (ignored when --lmtp-socket-path is set)")
	serveCmd.Flags().String("lmtp-domain", "localhost", "LMTP banner domain")
	serveCmd.Flags().Int64("lmtp-max-msg-bytes", 0, "max message size in bytes (0 = 10 MiB default)")
	serveCmd.Flags().Int("lmtp-max-recipients", 0, "max recipients per LMTP message (0 = 100 default)")
	serveCmd.Flags().Int("lmtp-read-timeout-secs", 0, "LMTP read timeout in seconds (0 = 5m default)")
	serveCmd.Flags().Int("lmtp-write-timeout-secs", 0, "LMTP write timeout in seconds (0 = 60s default)")

	// Cluster / multi-node flags.
	serveCmd.Flags().String("redis-url", "", "Redis URL for multi-node routing (e.g. redis://localhost:6379)")

	// Webhook delivery dispatcher flags.
	serveCmd.Flags().Bool("webhook-enabled", false, "enable async webhook delivery to agent endpoints")
	serveCmd.Flags().Int("webhook-workers", 4, "number of concurrent webhook delivery workers")
	serveCmd.Flags().Int("webhook-max-attempts", 5, "max delivery attempts before marking dead")
	serveCmd.Flags().Int("webhook-timeout-secs", 10, "HTTP timeout per webhook POST in seconds")

	// gRPC security flag.
	serveCmd.Flags().String("grpc-auth-token", "", "bearer token required on every gRPC call (empty = allow all — development only)")

	// SMTP TLS flags.
	serveCmd.Flags().String("smtp-tls-cert", "", "path to TLS certificate file for STARTTLS")
	serveCmd.Flags().String("smtp-tls-key", "", "path to TLS private key file for STARTTLS")
	serveCmd.Flags().Int("smtp-max-auth-failures", 0, "max AUTH failures per session before disconnect (0 = default 5)")
	serveCmd.Flags().Bool("smtp-check-spf", false, "verify SPF during MAIL FROM and inject X-SPF-Result")
	serveCmd.Flags().Bool("smtp-verify-dkim", false, "verify DKIM signatures and inject X-DKIM-Result")

	// Plugin flags — CrowdSec.
	serveCmd.Flags().Bool("crowdsec-enabled", false, "enable CrowdSec bouncer plugin")
	serveCmd.Flags().String("crowdsec-endpoint", "http://localhost:8080", "CrowdSec LAPI base URL")
	serveCmd.Flags().String("crowdsec-api-key", "", "CrowdSec bouncer API key")
	serveCmd.Flags().Int("crowdsec-timeout-secs", 2, "CrowdSec LAPI HTTP timeout in seconds")
	serveCmd.Flags().Bool("crowdsec-fail-open", true, "allow messages when CrowdSec LAPI is unreachable")
	serveCmd.Flags().Int("crowdsec-cache-ttl-secs", 30, "seconds to cache CrowdSec decisions")

	// Plugin flags — DNSBL/RBL IP reputation.
	serveCmd.Flags().Bool("dnsbl-enabled", false, "enable DNSBL/RBL IP reputation plugin")
	serveCmd.Flags().StringSlice("dnsbl-zones", nil, "DNSBL zones to query (e.g. zen.spamhaus.org)")
	serveCmd.Flags().Int("dnsbl-cache-ttl-secs", 300, "DNSBL decision cache TTL in seconds")
	serveCmd.Flags().Bool("dnsbl-fail-open", true, "allow messages when DNSBL lookup fails")

	// Plugin flags — email auth enforcement.
	serveCmd.Flags().Bool("email-auth-enabled", false, "enable email authentication enforcement plugin")
	serveCmd.Flags().String("email-auth-spf-policy", "off", "SPF enforcement policy: off|warn|reject|require")
	serveCmd.Flags().String("email-auth-dkim-policy", "off", "DKIM enforcement policy: off|warn|reject|require")
	serveCmd.Flags().String("email-auth-dmarc-policy", "off", "DMARC enforcement policy: off|warn|reject")

	// Plugin flags — SMTP security.
	serveCmd.Flags().Bool("smtp-security-enabled", false, "enable SMTP security plugin")
	serveCmd.Flags().Int("smtp-security-max-auth-failures", 5, "max AUTH failures per IP before lockout (0 = unlimited)")
	serveCmd.Flags().Int("smtp-security-lockout-minutes", 15, "lockout duration in minutes after AUTH failure threshold")
	serveCmd.Flags().Bool("smtp-security-spoof-check", false, "reject messages where envelope sender doesn't match From")
	serveCmd.Flags().Bool("smtp-security-require-auth", false, "reject messages from unauthenticated senders")
	serveCmd.Flags().StringSlice("smtp-security-allowed-domains", nil, "sender domain allowlist (empty = all allowed)")
	serveCmd.Flags().StringSlice("smtp-security-blocked-domains", nil, "sender domain blocklist")
	serveCmd.Flags().Bool("smtp-security-redis-lockout", false, "use Redis-backed lockout store (requires --redis-url)")

	// Plugin flags — address filter.
	serveCmd.Flags().Bool("address-filter-enabled", false, "enable address-filter plugin")
	serveCmd.Flags().String("address-filter-mode", "block", "address filter mode: block or allow")

	// Plugin flags — attachment filter.
	serveCmd.Flags().Bool("attachment-enabled", false, "enable attachment-filter plugin")
	serveCmd.Flags().Int("attachment-max-count", 0, "max number of attachments per message (0 = unlimited)")
	serveCmd.Flags().Int64("attachment-max-single-bytes", 0, "max size of a single attachment in bytes (0 = unlimited)")
	serveCmd.Flags().Int64("attachment-max-total-bytes", 0, "max total attachment size per message in bytes (0 = unlimited)")
	serveCmd.Flags().StringSlice("attachment-allowed-mimes", nil, "MIME type allowlist, comma-separated (empty = all allowed)")
	serveCmd.Flags().StringSlice("attachment-blocked-mimes", nil, "MIME type blocklist, comma-separated")

	// Plugin flags — spam checker.
	serveCmd.Flags().Bool("spam-enabled", false, "enable spam-checker plugin")
	serveCmd.Flags().String("spam-endpoint", "", "spam-checker HTTP endpoint URL")
	serveCmd.Flags().Float64("spam-threshold", 5.0, "spam score threshold (0 = any positive score)")
	serveCmd.Flags().Bool("spam-tag-only", false, "tag spam headers instead of rejecting")
	serveCmd.Flags().Int("spam-timeout-secs", 5, "spam-checker HTTP timeout in seconds")

	// Plugin flags — antivirus.
	serveCmd.Flags().Bool("av-enabled", false, "enable antivirus plugin")
	serveCmd.Flags().String("av-endpoint", "", "antivirus HTTP endpoint URL")
	serveCmd.Flags().Bool("av-fail-open", false, "allow messages through when AV is unreachable")
	serveCmd.Flags().Int("av-timeout-secs", 30, "antivirus HTTP timeout in seconds")

	// Plugin flags — security agent.
	serveCmd.Flags().Bool("security-enabled", false, "enable security-agent plugin")
	serveCmd.Flags().Int("security-max-recipients", 0, "max recipients per message (0 = unlimited)")
	serveCmd.Flags().Int("security-max-body-bytes", 0, "max body size in bytes (0 = unlimited)")
	serveCmd.Flags().Float64("security-rate-per-sender", 0, "per-sender rate limit (msgs/sec, 0 = unlimited)")
	serveCmd.Flags().Int("security-burst-per-sender", 1, "per-sender rate-limit burst capacity")

	// Outbound relay flags — route external recipients to SES, SendGrid, or an SMTP relay.
	serveCmd.Flags().String("outbound-backend", "", "outbound relay backend: smtp|sendgrid (empty = disabled)")
	serveCmd.Flags().String("outbound-from", "", "outbound envelope sender / From address")
	serveCmd.Flags().String("outbound-node-id", "outbound", "virtual routing node ID for relay recipients")
	serveCmd.Flags().String("outbound-smtp-host", "", "SMTP relay host:port (e.g. email-smtp.us-east-1.amazonaws.com:587)")
	serveCmd.Flags().String("outbound-smtp-username", "", "SMTP relay username (SES IAM SMTP credential)")
	serveCmd.Flags().String("outbound-smtp-password", "", "SMTP relay password (SES IAM SMTP credential)")
	serveCmd.Flags().Int("outbound-smtp-timeout-secs", 30, "SMTP relay connection timeout in seconds")
	serveCmd.Flags().String("outbound-sendgrid-api-key", "", "SendGrid v3 API key (starts with SG.)")
	serveCmd.Flags().Int("outbound-sendgrid-timeout-secs", 10, "SendGrid HTTP request timeout in seconds")
	serveCmd.Flags().String("outbound-dkim-private-key-file", "", "path to PEM-encoded DKIM private key for outbound signing")
	serveCmd.Flags().String("outbound-dkim-domain", "", "DKIM signing domain (e.g. example.com)")
	serveCmd.Flags().String("outbound-dkim-selector", "", "DKIM DNS selector label (e.g. postbox)")
	serveCmd.Flags().Bool("outbound-queue", false, "enable async outbound delivery queue")
	serveCmd.Flags().Int("outbound-queue-workers", 4, "number of concurrent outbound delivery workers")
	serveCmd.Flags().Int("outbound-queue-max-retries", 7, "max delivery attempts before discarding")
	serveCmd.Flags().Int("outbound-queue-base-delay-secs", 5, "initial backoff duration in seconds")
	serveCmd.Flags().Int("outbound-queue-max-delay-secs", 14400, "maximum backoff duration in seconds (default 4h)")

	// Mailbox message store backend flags.
	serveCmd.Flags().String("mailbox-backend", "memory", "mailbox message store backend: memory|postgres|mongo")
	serveCmd.Flags().String("mailbox-dsn", "", "mailbox store connection string (postgres DSN or mongo URI)")
	serveCmd.Flags().String("mailbox-db", "", "mailbox database/schema name (mongo only; default \"mailbox\")")

	// Observability flags.
	serveCmd.Flags().Int("metrics-port", 9090, "HTTP port for OTel /metrics Prometheus scrape endpoint (0 = disabled)")
	serveCmd.Flags().Bool("health-enabled", true, "register gRPC health service on the main listener")

	// Bind cobra flags to viper keys so that config-file values are overridden
	// by any flag the user explicitly sets on the command line.
	_ = v.BindPFlag("server.port", serveCmd.Flags().Lookup("port"))
	_ = v.BindPFlag("server.mode", serveCmd.Flags().Lookup("mode"))
	_ = v.BindPFlag("server.db", serveCmd.Flags().Lookup("db"))
	_ = v.BindPFlag("server.node_id", serveCmd.Flags().Lookup("node-id"))
	_ = v.BindPFlag("server.host", serveCmd.Flags().Lookup("host"))
	_ = v.BindPFlag("smtp.enabled", serveCmd.Flags().Lookup("smtp-enabled"))
	_ = v.BindPFlag("smtp.port", serveCmd.Flags().Lookup("smtp-port"))
	_ = v.BindPFlag("smtp.bind_addr", serveCmd.Flags().Lookup("smtp-bind-addr"))
	_ = v.BindPFlag("smtp.domain", serveCmd.Flags().Lookup("smtp-domain"))
	_ = v.BindPFlag("smtp.insecure_auth", serveCmd.Flags().Lookup("smtp-insecure-auth"))
	_ = v.BindPFlag("smtp.max_msg_bytes", serveCmd.Flags().Lookup("smtp-max-msg-bytes"))
	_ = v.BindPFlag("smtp.max_recipients", serveCmd.Flags().Lookup("smtp-max-recipients"))
	_ = v.BindPFlag("smtp.max_connections", serveCmd.Flags().Lookup("smtp-max-connections"))
	_ = v.BindPFlag("smtp.read_timeout_secs", serveCmd.Flags().Lookup("smtp-read-timeout-secs"))
	_ = v.BindPFlag("smtp.write_timeout_secs", serveCmd.Flags().Lookup("smtp-write-timeout-secs"))
	_ = v.BindPFlag("smtp.rate_conns", serveCmd.Flags().Lookup("smtp-rate-conns"))
	_ = v.BindPFlag("smtp.rate_burst", serveCmd.Flags().Lookup("smtp-rate-burst"))
	_ = v.BindPFlag("smtp.rate_conns_per_ip", serveCmd.Flags().Lookup("smtp-rate-conns-per-ip"))
	_ = v.BindPFlag("smtp.rate_burst_per_ip", serveCmd.Flags().Lookup("smtp-rate-burst-per-ip"))
	_ = v.BindPFlag("smtp.relay_enabled", serveCmd.Flags().Lookup("smtp-relay-enabled"))
	_ = v.BindPFlag("smtp.tls_cert_file", serveCmd.Flags().Lookup("smtp-tls-cert"))
	_ = v.BindPFlag("smtp.tls_key_file", serveCmd.Flags().Lookup("smtp-tls-key"))
	_ = v.BindPFlag("smtp.max_auth_failures_per_session", serveCmd.Flags().Lookup("smtp-max-auth-failures"))
	_ = v.BindPFlag("smtp.check_spf", serveCmd.Flags().Lookup("smtp-check-spf"))
	_ = v.BindPFlag("smtp.verify_dkim", serveCmd.Flags().Lookup("smtp-verify-dkim"))
	_ = v.BindPFlag("grpc.auth_token", serveCmd.Flags().Lookup("grpc-auth-token"))
	_ = v.BindPFlag("plugins.crowdsec.enabled", serveCmd.Flags().Lookup("crowdsec-enabled"))
	_ = v.BindPFlag("plugins.crowdsec.endpoint", serveCmd.Flags().Lookup("crowdsec-endpoint"))
	_ = v.BindPFlag("plugins.crowdsec.api_key", serveCmd.Flags().Lookup("crowdsec-api-key"))
	_ = v.BindPFlag("plugins.crowdsec.timeout_secs", serveCmd.Flags().Lookup("crowdsec-timeout-secs"))
	_ = v.BindPFlag("plugins.crowdsec.fail_open", serveCmd.Flags().Lookup("crowdsec-fail-open"))
	_ = v.BindPFlag("plugins.crowdsec.cache_ttl_secs", serveCmd.Flags().Lookup("crowdsec-cache-ttl-secs"))
	_ = v.BindPFlag("plugins.dnsbl.enabled", serveCmd.Flags().Lookup("dnsbl-enabled"))
	_ = v.BindPFlag("plugins.dnsbl.zones", serveCmd.Flags().Lookup("dnsbl-zones"))
	_ = v.BindPFlag("plugins.dnsbl.cache_ttl_secs", serveCmd.Flags().Lookup("dnsbl-cache-ttl-secs"))
	_ = v.BindPFlag("plugins.dnsbl.fail_open", serveCmd.Flags().Lookup("dnsbl-fail-open"))
	_ = v.BindPFlag("plugins.email_auth.enabled", serveCmd.Flags().Lookup("email-auth-enabled"))
	_ = v.BindPFlag("plugins.email_auth.spf_policy", serveCmd.Flags().Lookup("email-auth-spf-policy"))
	_ = v.BindPFlag("plugins.email_auth.dkim_policy", serveCmd.Flags().Lookup("email-auth-dkim-policy"))
	_ = v.BindPFlag("plugins.email_auth.dmarc_policy", serveCmd.Flags().Lookup("email-auth-dmarc-policy"))
	_ = v.BindPFlag("lmtp.enabled", serveCmd.Flags().Lookup("lmtp-enabled"))
	_ = v.BindPFlag("lmtp.socket_path", serveCmd.Flags().Lookup("lmtp-socket-path"))
	_ = v.BindPFlag("lmtp.port", serveCmd.Flags().Lookup("lmtp-port"))
	_ = v.BindPFlag("lmtp.bind_addr", serveCmd.Flags().Lookup("lmtp-bind-addr"))
	_ = v.BindPFlag("lmtp.domain", serveCmd.Flags().Lookup("lmtp-domain"))
	_ = v.BindPFlag("lmtp.max_msg_bytes", serveCmd.Flags().Lookup("lmtp-max-msg-bytes"))
	_ = v.BindPFlag("lmtp.max_recipients", serveCmd.Flags().Lookup("lmtp-max-recipients"))
	_ = v.BindPFlag("lmtp.read_timeout_secs", serveCmd.Flags().Lookup("lmtp-read-timeout-secs"))
	_ = v.BindPFlag("lmtp.write_timeout_secs", serveCmd.Flags().Lookup("lmtp-write-timeout-secs"))
	_ = v.BindPFlag("cluster.redis_url", serveCmd.Flags().Lookup("redis-url"))
	_ = v.BindPFlag("webhook.enabled", serveCmd.Flags().Lookup("webhook-enabled"))
	_ = v.BindPFlag("webhook.workers", serveCmd.Flags().Lookup("webhook-workers"))
	_ = v.BindPFlag("webhook.max_attempts", serveCmd.Flags().Lookup("webhook-max-attempts"))
	_ = v.BindPFlag("webhook.timeout_secs", serveCmd.Flags().Lookup("webhook-timeout-secs"))
	_ = v.BindPFlag("plugins.smtp_security.enabled", serveCmd.Flags().Lookup("smtp-security-enabled"))
	_ = v.BindPFlag("plugins.smtp_security.max_auth_failures_per_ip", serveCmd.Flags().Lookup("smtp-security-max-auth-failures"))
	_ = v.BindPFlag("plugins.smtp_security.lockout_minutes", serveCmd.Flags().Lookup("smtp-security-lockout-minutes"))
	_ = v.BindPFlag("plugins.smtp_security.envelope_spoof_check", serveCmd.Flags().Lookup("smtp-security-spoof-check"))
	_ = v.BindPFlag("plugins.smtp_security.require_authenticated_sender", serveCmd.Flags().Lookup("smtp-security-require-auth"))
	_ = v.BindPFlag("plugins.smtp_security.allowed_sender_domains", serveCmd.Flags().Lookup("smtp-security-allowed-domains"))
	_ = v.BindPFlag("plugins.smtp_security.blocked_sender_domains", serveCmd.Flags().Lookup("smtp-security-blocked-domains"))
	_ = v.BindPFlag("plugins.smtp_security.redis_lockout", serveCmd.Flags().Lookup("smtp-security-redis-lockout"))
	_ = v.BindPFlag("plugins.address_filter.enabled", serveCmd.Flags().Lookup("address-filter-enabled"))
	_ = v.BindPFlag("plugins.address_filter.mode", serveCmd.Flags().Lookup("address-filter-mode"))
	_ = v.BindPFlag("plugins.attachment.enabled", serveCmd.Flags().Lookup("attachment-enabled"))
	_ = v.BindPFlag("plugins.attachment.max_count", serveCmd.Flags().Lookup("attachment-max-count"))
	_ = v.BindPFlag("plugins.attachment.max_single_bytes", serveCmd.Flags().Lookup("attachment-max-single-bytes"))
	_ = v.BindPFlag("plugins.attachment.max_total_bytes", serveCmd.Flags().Lookup("attachment-max-total-bytes"))
	_ = v.BindPFlag("plugins.attachment.allowed_mimes", serveCmd.Flags().Lookup("attachment-allowed-mimes"))
	_ = v.BindPFlag("plugins.attachment.blocked_mimes", serveCmd.Flags().Lookup("attachment-blocked-mimes"))
	_ = v.BindPFlag("plugins.spam_checker.enabled", serveCmd.Flags().Lookup("spam-enabled"))
	_ = v.BindPFlag("plugins.spam_checker.endpoint", serveCmd.Flags().Lookup("spam-endpoint"))
	_ = v.BindPFlag("plugins.spam_checker.threshold", serveCmd.Flags().Lookup("spam-threshold"))
	_ = v.BindPFlag("plugins.spam_checker.tag_only", serveCmd.Flags().Lookup("spam-tag-only"))
	_ = v.BindPFlag("plugins.spam_checker.timeout_secs", serveCmd.Flags().Lookup("spam-timeout-secs"))
	_ = v.BindPFlag("plugins.antivirus.enabled", serveCmd.Flags().Lookup("av-enabled"))
	_ = v.BindPFlag("plugins.antivirus.endpoint", serveCmd.Flags().Lookup("av-endpoint"))
	_ = v.BindPFlag("plugins.antivirus.fail_open", serveCmd.Flags().Lookup("av-fail-open"))
	_ = v.BindPFlag("plugins.antivirus.timeout_secs", serveCmd.Flags().Lookup("av-timeout-secs"))
	_ = v.BindPFlag("plugins.security_agent.enabled", serveCmd.Flags().Lookup("security-enabled"))
	_ = v.BindPFlag("plugins.security_agent.max_recipients", serveCmd.Flags().Lookup("security-max-recipients"))
	_ = v.BindPFlag("plugins.security_agent.max_body_bytes", serveCmd.Flags().Lookup("security-max-body-bytes"))
	_ = v.BindPFlag("plugins.security_agent.rate_per_sender", serveCmd.Flags().Lookup("security-rate-per-sender"))
	_ = v.BindPFlag("plugins.security_agent.burst_per_sender", serveCmd.Flags().Lookup("security-burst-per-sender"))
	_ = v.BindPFlag("outbound.backend", serveCmd.Flags().Lookup("outbound-backend"))
	_ = v.BindPFlag("outbound.from", serveCmd.Flags().Lookup("outbound-from"))
	_ = v.BindPFlag("outbound.node_id", serveCmd.Flags().Lookup("outbound-node-id"))
	_ = v.BindPFlag("outbound.smtp_host", serveCmd.Flags().Lookup("outbound-smtp-host"))
	_ = v.BindPFlag("outbound.smtp_username", serveCmd.Flags().Lookup("outbound-smtp-username"))
	_ = v.BindPFlag("outbound.smtp_password", serveCmd.Flags().Lookup("outbound-smtp-password"))
	_ = v.BindPFlag("outbound.smtp_timeout_secs", serveCmd.Flags().Lookup("outbound-smtp-timeout-secs"))
	_ = v.BindPFlag("outbound.sendgrid_api_key", serveCmd.Flags().Lookup("outbound-sendgrid-api-key"))
	_ = v.BindPFlag("outbound.sendgrid_timeout_secs", serveCmd.Flags().Lookup("outbound-sendgrid-timeout-secs"))
	_ = v.BindPFlag("outbound.dkim_private_key_file", serveCmd.Flags().Lookup("outbound-dkim-private-key-file"))
	_ = v.BindPFlag("outbound.dkim_domain", serveCmd.Flags().Lookup("outbound-dkim-domain"))
	_ = v.BindPFlag("outbound.dkim_selector", serveCmd.Flags().Lookup("outbound-dkim-selector"))
	_ = v.BindPFlag("outbound.queue_enabled", serveCmd.Flags().Lookup("outbound-queue"))
	_ = v.BindPFlag("outbound.queue_workers", serveCmd.Flags().Lookup("outbound-queue-workers"))
	_ = v.BindPFlag("outbound.queue_max_retries", serveCmd.Flags().Lookup("outbound-queue-max-retries"))
	_ = v.BindPFlag("outbound.queue_base_delay_secs", serveCmd.Flags().Lookup("outbound-queue-base-delay-secs"))
	_ = v.BindPFlag("outbound.queue_max_delay_secs", serveCmd.Flags().Lookup("outbound-queue-max-delay-secs"))
	_ = v.BindPFlag("mailbox_store.backend", serveCmd.Flags().Lookup("mailbox-backend"))
	_ = v.BindPFlag("mailbox_store.dsn", serveCmd.Flags().Lookup("mailbox-dsn"))
	_ = v.BindPFlag("mailbox_store.database", serveCmd.Flags().Lookup("mailbox-db"))
	_ = v.BindPFlag("observability.metrics_port", serveCmd.Flags().Lookup("metrics-port"))
	_ = v.BindPFlag("observability.health_enabled", serveCmd.Flags().Lookup("health-enabled"))
}

// buildPlugins constructs the enabled plugins from config in the recommended
// evaluation order, and returns the SMTPSecurityPlugin separately so it can
// also be wired into the SMTP server as an AuthFailureHook.
// redisClient may be nil; it is used for the Redis-backed lockout store when
// cfg.SMTPSecurity.RedisLockout is true.
func buildPlugins(cfg PluginsConfig, logger *slog.Logger, redisClient redis.UniversalClient) ([]mailbox.Plugin, postboxsmtp.AuthFailureHook) {
	var plugins []mailbox.Plugin
	var authHook postboxsmtp.AuthFailureHook

	if cfg.CrowdSec.Enabled {
		cs := plugin.NewCrowdSec("crowdsec", plugin.CrowdSecConfig{
			Endpoint: cfg.CrowdSec.Endpoint,
			APIKey:   cfg.CrowdSec.APIKey,
			Timeout:  time.Duration(cfg.CrowdSec.TimeoutSecs) * time.Second,
			FailOpen: cfg.CrowdSec.FailOpen,
			CacheTTL: time.Duration(cfg.CrowdSec.CacheTTLSecs) * time.Second,
		}, plugin.WithCrowdSecLogger(logger))
		plugins = append(plugins, cs)
	}

	if cfg.DNSBL.Enabled && len(cfg.DNSBL.Zones) > 0 {
		dnsbl := plugin.NewDNSBL("dnsbl", plugin.DNSBLConfig{
			Zones:    cfg.DNSBL.Zones,
			CacheTTL: time.Duration(cfg.DNSBL.CacheTTLSecs) * time.Second,
			FailOpen: cfg.DNSBL.FailOpen,
		}, plugin.WithDNSBLLogger(logger))
		plugins = append(plugins, dnsbl)
	}

	if cfg.AddressFilter.Enabled {
		mode := plugin.ModeBlock
		if cfg.AddressFilter.Mode == "allow" {
			mode = plugin.ModeAllow
		}
		store := plugin.NewMemRuleStore(addressRulesFromConfig(cfg.AddressFilter.Rules)...)
		af := plugin.NewAddressFilter("address-filter", mode, store,
			plugin.WithAddressFilterLogger(logger))
		plugins = append(plugins, af)
	}

	if cfg.Attachment.Enabled {
		att := plugin.NewAttachmentFilter("attachment-filter", plugin.AttachmentFilterConfig{
			MaxCount:       cfg.Attachment.MaxCount,
			MaxSingleBytes: cfg.Attachment.MaxSingleBytes,
			MaxTotalBytes:  cfg.Attachment.MaxTotalBytes,
			AllowedMIMEs:   cfg.Attachment.AllowedMIMEs,
			BlockedMIMEs:   cfg.Attachment.BlockedMIMEs,
		}, plugin.WithAttachmentFilterLogger(logger))
		plugins = append(plugins, att)
	}

	if cfg.SMTPSecurity.Enabled {
		ssOpts := []plugin.SMTPSecurityOption{plugin.WithSMTPSecurityLogger(logger)}
		if cfg.SMTPSecurity.RedisLockout && redisClient != nil {
			store := plugin.NewRedisLockoutStore(plugin.RedisLockoutConfig{Client: redisClient})
			ssOpts = append(ssOpts, plugin.WithLockoutStore(store))
		}
		ss := plugin.NewSMTPSecurityPlugin("smtp-security", plugin.SMTPSecurityConfig{
			MaxAuthFailuresPerIP:       cfg.SMTPSecurity.MaxAuthFailuresPerIP,
			LockoutDuration:            time.Duration(cfg.SMTPSecurity.LockoutMinutes) * time.Minute,
			EnvelopeSpoofCheck:         cfg.SMTPSecurity.EnvelopeSpoofCheck,
			AllowedSenderDomains:       cfg.SMTPSecurity.AllowedSenderDomains,
			BlockedSenderDomains:       cfg.SMTPSecurity.BlockedSenderDomains,
			MaxSubjectBytes:            cfg.SMTPSecurity.MaxSubjectBytes,
			RequireAuthenticatedSender: cfg.SMTPSecurity.RequireAuthenticatedSender,
		}, ssOpts...)
		plugins = append(plugins, ss)
		authHook = ss // wire as AuthFailureHook for the SMTP server
	}

	if cfg.EmailAuth.Enabled {
		ea := plugin.NewEmailAuth("email-auth", plugin.EmailAuthConfig{
			SPFPolicy:   cfg.EmailAuth.SPFPolicy,
			DKIMPolicy:  cfg.EmailAuth.DKIMPolicy,
			DMARCPolicy: cfg.EmailAuth.DMARCPolicy,
		}, plugin.WithEmailAuthLogger(logger))
		plugins = append(plugins, ea)
	}

	if cfg.SpamChecker.Enabled && cfg.SpamChecker.Endpoint != "" {
		timeout := time.Duration(cfg.SpamChecker.TimeoutSecs) * time.Second
		sc := plugin.NewSpamChecker("spam-checker", plugin.SpamCheckerConfig{
			Endpoint:  cfg.SpamChecker.Endpoint,
			Threshold: cfg.SpamChecker.Threshold,
			TagOnly:   cfg.SpamChecker.TagOnly,
			Timeout:   timeout,
		}, plugin.WithSpamCheckerLogger(logger))
		plugins = append(plugins, sc)
	}

	if cfg.AntiVirus.Enabled && cfg.AntiVirus.Endpoint != "" {
		timeout := time.Duration(cfg.AntiVirus.TimeoutSecs) * time.Second
		av := plugin.NewAntiVirus("antivirus", plugin.AntiVirusConfig{
			Endpoint: cfg.AntiVirus.Endpoint,
			Timeout:  timeout,
			FailOpen: cfg.AntiVirus.FailOpen,
		}, plugin.WithAntiVirusLogger(logger))
		plugins = append(plugins, av)
	}

	if cfg.SecurityAgent.Enabled {
		sa := plugin.NewSecurityAgent("security-agent", plugin.SecurityAgentConfig{
			MaxRecipients:  cfg.SecurityAgent.MaxRecipients,
			MaxBodyBytes:   cfg.SecurityAgent.MaxBodyBytes,
			RatePerSender:  cfg.SecurityAgent.RatePerSender,
			BurstPerSender: cfg.SecurityAgent.BurstPerSender,
		}, plugin.WithSecurityAgentLogger(logger))
		plugins = append(plugins, sa)
	}

	return plugins, authHook
}

// buildRelayBackend constructs the outbound relay backend from config.
// It optionally wraps the base backend with DKIM signing and/or an async queue.
func buildRelayBackend(cfg OutboundConfig) (relay.Backend, error) {
	var base relay.Backend
	switch cfg.Backend {
	case "sendgrid":
		base = relay.NewSendGrid(relay.SendGridConfig{
			APIKey:  cfg.SendGridAPIKey,
			From:    cfg.From,
			Timeout: time.Duration(cfg.SendGridTimeoutSecs) * time.Second,
		})
	case "smtp":
		base = relay.NewSMTP(relay.SMTPConfig{
			Host:     cfg.SMTPHost,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.From,
			Timeout:  time.Duration(cfg.SMTPTimeoutSecs) * time.Second,
		})
	default:
		return nil, fmt.Errorf("unknown outbound backend %q; want smtp|sendgrid", cfg.Backend)
	}

	// Wrap with DKIM signing if all three DKIM fields are configured.
	if cfg.DKIMPrivateKeyFile != "" && cfg.DKIMDomain != "" && cfg.DKIMSelector != "" {
		pem, err := os.ReadFile(cfg.DKIMPrivateKeyFile)
		if err != nil {
			return nil, fmt.Errorf("read dkim private key %q: %w", cfg.DKIMPrivateKeyFile, err)
		}
		base, err = relay.NewDKIMSigningBackend(relay.DKIMConfig{
			PrivateKeyPEM: string(pem),
			Domain:        cfg.DKIMDomain,
			Selector:      cfg.DKIMSelector,
		}, base)
		if err != nil {
			return nil, fmt.Errorf("dkim signing backend: %w", err)
		}
	}

	return base, nil
}

// addressRulesFromConfig converts the config-file rule format to AddressRule values.
func addressRulesFromConfig(rules []AddressRuleConfig) []plugin.AddressRule {
	out := make([]plugin.AddressRule, 0, len(rules))
	for _, r := range rules {
		var field plugin.AddressField
		switch r.Field {
		case "sender":
			field = plugin.FieldSender
		case "recipients":
			field = plugin.FieldRecipients
		default:
			field = plugin.FieldAll
		}
		out = append(out, plugin.AddressRule{ID: r.ID, Pattern: r.Pattern, Field: field})
	}
	return out
}

func runServe(cmd *cobra.Command, _ []string) error {
	cfg, err := resolveConfig()
	if err != nil {
		return err
	}

	logger := slog.Default()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	dbPath := cfg.Server.DB
	nodeMode := server.NodeMode(cfg.Server.Mode)
	if nodeMode == server.ModeTemp {
		dbPath = ":memory:"
	}

	s := sqlite.New(dbPath)

	// Build Redis client early so it can be shared by cluster routing and the
	// optional Redis-backed lockout store.
	var redisClient redis.UniversalClient
	if cfg.Cluster.RedisURL != "" {
		opt, err := redis.ParseURL(cfg.Cluster.RedisURL)
		if err != nil {
			return fmt.Errorf("parse redis-url: %w", err)
		}
		rc := redis.NewClient(opt)
		defer rc.Close() //nolint:errcheck
		redisClient = rc
	}

	ctrlOpts := []server.Option{
		server.WithNodeID(cfg.Server.NodeID),
		server.WithMode(nodeMode),
		server.WithAddress(cfg.Server.Host + ":" + cfg.Server.Port),
		server.WithLogger(logger),
	}
	if redisClient != nil {
		ctrlOpts = append(ctrlOpts, server.WithRedisClient(redisClient))
	}
	plugins, authHook := buildPlugins(cfg.Plugins, logger, redisClient)

	// Always register plugins first so they apply regardless of which mailbox
	// store backend is selected. The factory path also passes plugins directly,
	// but WithMailboxPlugins ensures they are registered even for the in-memory
	// (default) backend without duplicating the branching logic.
	if len(plugins) > 0 {
		ctrlOpts = append(ctrlOpts, server.WithMailboxPlugins(plugins...))
	}
	// Wire the durable mailbox store factory (replaces the default in-memory store).
	if cfg.MailboxStore.Backend != "" && cfg.MailboxStore.Backend != "memory" {
		ctrlOpts = append(ctrlOpts, server.WithMailboxFactory(
			mbxstore.NewMailboxFactory(mbxstore.Config{
				Backend:  cfg.MailboxStore.Backend,
				DSN:      cfg.MailboxStore.DSN,
				Database: cfg.MailboxStore.Database,
			}, plugins),
		))
	}
	if cfg.Outbound.Backend != "" {
		outbound, err := buildRelayBackend(cfg.Outbound)
		if err != nil {
			return err
		}
		nodeID := cfg.Outbound.NodeID
		if nodeID == "" {
			nodeID = "outbound"
		}
		ctrlOpts = append(ctrlOpts, server.WithOutboundRelay(nodeID, outbound))
		if cfg.Outbound.QueueEnabled {
			ctrlOpts = append(ctrlOpts, server.WithOutboundRelayQueue(relay.QueueConfig{
				Workers:    cfg.Outbound.QueueWorkers,
				MaxRetries: cfg.Outbound.QueueMaxRetries,
				BaseDelay:  time.Duration(cfg.Outbound.QueueBaseDelaySecs) * time.Second,
				MaxDelay:   time.Duration(cfg.Outbound.QueueMaxDelaySecs) * time.Second,
			}))
		}
	}
	if cfg.Webhook.Enabled {
		wOpts := []webhook.Option{webhook.WithLogger(logger)}
		if cfg.Webhook.Workers > 0 {
			wOpts = append(wOpts, webhook.WithWorkers(cfg.Webhook.Workers))
		}
		if cfg.Webhook.MaxAttempts > 0 {
			wOpts = append(wOpts, webhook.WithMaxAttempts(cfg.Webhook.MaxAttempts))
		}
		if cfg.Webhook.TimeoutSecs > 0 {
			wOpts = append(wOpts, webhook.WithHTTPClient(&http.Client{
				Timeout: time.Duration(cfg.Webhook.TimeoutSecs) * time.Second,
			}))
		}
		ctrlOpts = append(ctrlOpts, server.WithWebhook(wOpts...))
	}

	ctrl, err := server.NewController(ctx, s, ctrlOpts...)
	if err != nil {
		return fmt.Errorf("init controller: %w", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer shutdownCancel()
		if err := ctrl.Close(shutdownCtx); err != nil {
			logger.Error("controller close", "error", err)
		}
	}()

	postboxSrv := server.NewServer(ctrl)

	var secGuard mbxserver.SecurityGuard
	if cfg.GRPC.AuthToken != "" {
		secGuard = guard.NewBearer(cfg.GRPC.AuthToken)
	} else {
		logger.Warn("grpc: no auth token configured — all callers are allowed (set --grpc-auth-token for production)")
		secGuard = mbxserver.AllowAll()
	}
	mbxSrv, err := mbxserver.New(ctrl.Mailbox(),
		mbxserver.WithSecurityGuard(secGuard),
		mbxserver.WithLogger(logger),
	)
	if err != nil {
		return fmt.Errorf("init mailbox server: %w", err)
	}

	unaryInterceptors := []grpc.UnaryServerInterceptor{
		mbxserver.AuthInterceptor(secGuard),
		mbxserver.LoggingInterceptor(logger),
		mbxserver.RecoveryInterceptor(logger),
	}
	if fwd := ctrl.Forwarder(); fwd != nil {
		unaryInterceptors = append(unaryInterceptors, fwd.Interceptor())
	}
	// Start OTel metrics server before any gRPC work so instrumentation is active.
	if cfg.Observability.MetricsPort > 0 {
		metricsAddr := fmt.Sprintf(":%d", cfg.Observability.MetricsPort)
		metricsSrv, err := observability.NewMetricsServer(metricsAddr, logger)
		if err != nil {
			return fmt.Errorf("init metrics server: %w", err)
		}
		otel.SetMeterProvider(metricsSrv.Provider())
		if err := metricsSrv.Start(); err != nil {
			return fmt.Errorf("start metrics server: %w", err)
		}
		defer func() {
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			metricsSrv.Stop(stopCtx)
		}()
	}

	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unaryInterceptors...),
		grpc.ChainStreamInterceptor(
			mbxserver.StreamAuthInterceptor(secGuard),
			mbxserver.StreamLoggingInterceptor(logger),
			mbxserver.StreamRecoveryInterceptor(logger),
		),
	)
	postboxpb.RegisterPostboxServiceServer(grpcSrv, postboxSrv)
	mailboxpb.RegisterMailboxServiceServer(grpcSrv, mbxSrv)
	if cfg.Observability.HealthEnabled {
		healthSrv := health.NewServer()
		grpc_health_v1.RegisterHealthServer(grpcSrv, healthSrv)
		healthSrv.SetServingStatus("", grpc_health_v1.HealthCheckResponse_SERVING)
	}

	lis, err := net.Listen("tcp", ":"+cfg.Server.Port)
	if err != nil {
		return fmt.Errorf("listen :%s: %w", cfg.Server.Port, err)
	}

	logger.Info("postbox started", "port", cfg.Server.Port, "mode", cfg.Server.Mode, "node_id", ctrl.NodeID())

	if cfg.SMTP.Enabled {
		smtpCfg := postboxsmtp.Config{
			Port:                      cfg.SMTP.Port,
			BindAddr:                  cfg.SMTP.BindAddr,
			Domain:                    cfg.SMTP.Domain,
			AllowInsecureAuth:         cfg.SMTP.InsecureAuth,
			MaxMessageBytes:           cfg.SMTP.MaxMsgBytes,
			MaxRecipients:             cfg.SMTP.MaxRecipients,
			MaxConnections:            cfg.SMTP.MaxConnections,
			ReadTimeout:               time.Duration(cfg.SMTP.ReadTimeoutSecs) * time.Second,
			WriteTimeout:              time.Duration(cfg.SMTP.WriteTimeoutSecs) * time.Second,
			MaxConnsPerSec:            cfg.SMTP.RateConns,
			BurstConns:                cfg.SMTP.RateBurst,
			MaxConnsPerSecPerIP:       cfg.SMTP.RateConnsPerIP,
			BurstConnsPerIP:           cfg.SMTP.RateBurstPerIP,
			RelayEnabled:              cfg.SMTP.RelayEnabled,
			TLSCertFile:               cfg.SMTP.TLSCertFile,
			TLSKeyFile:                cfg.SMTP.TLSKeyFile,
			MaxAuthFailuresPerSession: cfg.SMTP.MaxAuthFailuresPerSession,
			CheckSPF:                  cfg.SMTP.CheckSPF,
			VerifyDKIM:                cfg.SMTP.VerifyDKIM,
			AuthFailureHook:           authHook,
		}
		if err := ctrl.StartSMTP(smtpCfg); err != nil {
			return fmt.Errorf("start smtp: %w", err)
		}
	}

	if cfg.LMTP.Enabled {
		if err := ctrl.StartLMTP(postboxsmtp.LMTPConfig{
			SocketPath:      cfg.LMTP.SocketPath,
			Port:            cfg.LMTP.Port,
			BindAddr:        cfg.LMTP.BindAddr,
			Domain:          cfg.LMTP.Domain,
			MaxMessageBytes: cfg.LMTP.MaxMsgBytes,
			MaxRecipients:   cfg.LMTP.MaxRecipients,
			ReadTimeout:     time.Duration(cfg.LMTP.ReadTimeoutSecs) * time.Second,
			WriteTimeout:    time.Duration(cfg.LMTP.WriteTimeoutSecs) * time.Second,
		}); err != nil {
			return fmt.Errorf("start lmtp: %w", err)
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- grpcSrv.Serve(lis) }()

	select {
	case <-ctx.Done():
		logger.Info("shutting down gracefully")
		grpcSrv.GracefulStop()
		return nil
	case err := <-errCh:
		return err
	}
}
