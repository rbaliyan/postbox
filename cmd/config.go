package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

// Config is the full configuration for the postbox server.
// It can be populated from a YAML file (--config), environment variables
// (POSTBOX_SERVER_PORT, POSTBOX_SMTP_DOMAIN, etc.), or CLI flags.
// Precedence from highest to lowest: CLI flag → env var → config file → default.
type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	SMTP          SMTPConfig          `mapstructure:"smtp"`
	LMTP          LMTPConfig          `mapstructure:"lmtp"`
	GRPC          GRPCConfig          `mapstructure:"grpc"`
	Webhook       WebhookConfig       `mapstructure:"webhook"`
	Cluster       ClusterConfig       `mapstructure:"cluster"`
	Plugins       PluginsConfig       `mapstructure:"plugins"`
	Outbound      OutboundConfig      `mapstructure:"outbound"`
	MailboxStore  MailboxStoreConfig  `mapstructure:"mailbox_store"`
	Observability ObservabilityConfig `mapstructure:"observability"`
}

// ObservabilityConfig controls the OTel metrics and gRPC health endpoints.
type ObservabilityConfig struct {
	// MetricsPort is the HTTP port for the /metrics Prometheus scrape endpoint.
	// 0 disables the endpoint. Default 9090.
	MetricsPort int `mapstructure:"metrics_port"`
	// HealthEnabled registers the standard gRPC health service on the main gRPC
	// listener. Default true.
	HealthEnabled bool `mapstructure:"health_enabled"`
}

// OutboundConfig enables outbound email relay via an external provider.
// Recipients not registered in the local store are delivered via the configured
// backend instead of being rejected.
//
// Backend must be one of: "smtp", "sendgrid". Leave empty to disable.
//
// SMTP backend covers AWS SES (use their SMTP endpoint), Mailgun, Postfix, and
// any provider that accepts SMTP with STARTTLS on port 587.
type OutboundConfig struct {
	// Backend selects the delivery provider: "smtp" or "sendgrid".
	// Empty string disables outbound relay.
	Backend string `mapstructure:"backend"`
	// NodeID is the virtual routing node identifier for relay recipients.
	// Defaults to "outbound" when empty.
	NodeID string `mapstructure:"node_id"`
	// From is the default envelope sender / From address when the message
	// sender (user_id) is not an email address.
	From string `mapstructure:"from"`

	// SMTP backend — also used for AWS SES SMTP endpoint.
	// AWS SES SMTP host: "email-smtp.<region>.amazonaws.com:587"
	SMTPHost        string `mapstructure:"smtp_host"`
	SMTPUsername    string `mapstructure:"smtp_username"`
	SMTPPassword    string `mapstructure:"smtp_password"`
	SMTPTimeoutSecs int    `mapstructure:"smtp_timeout_secs"`

	// SendGrid backend — HTTP v3 API.
	SendGridAPIKey      string `mapstructure:"sendgrid_api_key"`
	SendGridTimeoutSecs int    `mapstructure:"sendgrid_timeout_secs"`

	// DKIM signing — optional wrapper around the configured backend.
	// All three fields must be set to enable DKIM signing.
	DKIMPrivateKeyFile string `mapstructure:"dkim_private_key_file"`
	DKIMDomain         string `mapstructure:"dkim_domain"`
	DKIMSelector       string `mapstructure:"dkim_selector"`

	// Async delivery queue — enabled when QueueEnabled is true.
	QueueEnabled       bool `mapstructure:"queue_enabled"`
	QueueWorkers       int  `mapstructure:"queue_workers"`
	QueueMaxRetries    int  `mapstructure:"queue_max_retries"`
	QueueBaseDelaySecs int  `mapstructure:"queue_base_delay_secs"`
	QueueMaxDelaySecs  int  `mapstructure:"queue_max_delay_secs"`
}

// GRPCConfig holds gRPC server security settings.
type GRPCConfig struct {
	// AuthToken is a shared Bearer token required on every RPC call.
	// Empty string disables token auth (AllowAll — development only).
	// Set via --grpc-auth-token or POSTBOX_GRPC_AUTH_TOKEN.
	AuthToken string `mapstructure:"auth_token"`
}

// PluginsConfig holds optional mailbox plugin configurations.
// Each plugin has its own Enabled flag; all others are ignored when Enabled=false.
// Recommended evaluation order (cheapest first):
//
//	CrowdSec → DNSBL → AddressFilter → Attachment → SMTPSecurity → EmailAuth →
//	SpamChecker → AntiVirus → SecurityAgent
type PluginsConfig struct {
	CrowdSec      CrowdSecPluginConfig      `mapstructure:"crowdsec"`
	DNSBL         DNSBLPluginConfig         `mapstructure:"dnsbl"`
	AddressFilter AddressFilterPluginConfig `mapstructure:"address_filter"`
	Attachment    AttachmentPluginConfig    `mapstructure:"attachment"`
	SMTPSecurity  SMTPSecurityPluginConfig  `mapstructure:"smtp_security"`
	EmailAuth     EmailAuthPluginConfig     `mapstructure:"email_auth"`
	SpamChecker   SpamCheckerPluginConfig   `mapstructure:"spam_checker"`
	AntiVirus     AntiVirusPluginConfig     `mapstructure:"antivirus"`
	SecurityAgent SecurityAgentPluginConfig `mapstructure:"security_agent"`
}

// DNSBLPluginConfig configures the optional DNSBL/RBL IP reputation plugin.
type DNSBLPluginConfig struct {
	Enabled      bool     `mapstructure:"enabled"`
	Zones        []string `mapstructure:"zones"`
	CacheTTLSecs int      `mapstructure:"cache_ttl_secs"`
	FailOpen     bool     `mapstructure:"fail_open"`
}

// CrowdSecPluginConfig configures the optional CrowdSec bouncer plugin.
type CrowdSecPluginConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	Endpoint     string `mapstructure:"endpoint"`
	APIKey       string `mapstructure:"api_key"`
	TimeoutSecs  int    `mapstructure:"timeout_secs"`
	FailOpen     bool   `mapstructure:"fail_open"`
	CacheTTLSecs int    `mapstructure:"cache_ttl_secs"`
}

// EmailAuthPluginConfig configures the optional email authentication enforcement plugin.
type EmailAuthPluginConfig struct {
	Enabled     bool   `mapstructure:"enabled"`
	SPFPolicy   string `mapstructure:"spf_policy"`
	DKIMPolicy  string `mapstructure:"dkim_policy"`
	DMARCPolicy string `mapstructure:"dmarc_policy"`
}

// SMTPSecurityPluginConfig configures the optional SMTP-security plugin.
type SMTPSecurityPluginConfig struct {
	Enabled                    bool     `mapstructure:"enabled"`
	MaxAuthFailuresPerIP       int      `mapstructure:"max_auth_failures_per_ip"`
	LockoutMinutes             int      `mapstructure:"lockout_minutes"`
	EnvelopeSpoofCheck         bool     `mapstructure:"envelope_spoof_check"`
	AllowedSenderDomains       []string `mapstructure:"allowed_sender_domains"`
	BlockedSenderDomains       []string `mapstructure:"blocked_sender_domains"`
	MaxSubjectBytes            int      `mapstructure:"max_subject_bytes"`
	RequireAuthenticatedSender bool     `mapstructure:"require_authenticated_sender"`
	// RedisLockout uses the Redis client (from --redis-url) as the lockout store
	// instead of the default in-memory map. Has no effect when --redis-url is empty.
	RedisLockout bool `mapstructure:"redis_lockout"`
}

// AddressRuleConfig is a single address-filter rule loadable from config.
// Field must be "sender", "recipients", or "all" (default "all").
type AddressRuleConfig struct {
	ID      string `mapstructure:"id"`
	Pattern string `mapstructure:"pattern"`
	Field   string `mapstructure:"field"`
}

// AddressFilterPluginConfig configures the optional address-filter plugin.
// Mode must be "block" (default) or "allow".
// Rules seed the in-memory store at startup; further rules can be added at
// runtime via the AddressFilter API.
type AddressFilterPluginConfig struct {
	Enabled bool                `mapstructure:"enabled"`
	Mode    string              `mapstructure:"mode"`
	Rules   []AddressRuleConfig `mapstructure:"rules"`
}

// AttachmentPluginConfig configures the optional attachment-filter plugin.
// Zero values mean "no limit / no restriction".
// AllowedMIMEs and BlockedMIMEs accept comma-separated values on the CLI
// (--attachment-allowed-mimes "image/png,image/jpeg") and YAML lists in the
// config file.
type AttachmentPluginConfig struct {
	Enabled        bool     `mapstructure:"enabled"`
	MaxCount       int      `mapstructure:"max_count"`
	MaxSingleBytes int64    `mapstructure:"max_single_bytes"`
	MaxTotalBytes  int64    `mapstructure:"max_total_bytes"`
	AllowedMIMEs   []string `mapstructure:"allowed_mimes"`
	BlockedMIMEs   []string `mapstructure:"blocked_mimes"`
}

// SpamCheckerPluginConfig configures the optional spam-checking plugin.
type SpamCheckerPluginConfig struct {
	Enabled     bool    `mapstructure:"enabled"`
	Endpoint    string  `mapstructure:"endpoint"`
	Threshold   float64 `mapstructure:"threshold"`
	TagOnly     bool    `mapstructure:"tag_only"`
	TimeoutSecs int     `mapstructure:"timeout_secs"`
}

// AntiVirusPluginConfig configures the optional AV-scanning plugin.
type AntiVirusPluginConfig struct {
	Enabled     bool   `mapstructure:"enabled"`
	Endpoint    string `mapstructure:"endpoint"`
	TimeoutSecs int    `mapstructure:"timeout_secs"`
	FailOpen    bool   `mapstructure:"fail_open"`
}

// SecurityAgentPluginConfig configures the optional security-agent plugin.
type SecurityAgentPluginConfig struct {
	Enabled        bool    `mapstructure:"enabled"`
	MaxRecipients  int     `mapstructure:"max_recipients"`
	MaxBodyBytes   int     `mapstructure:"max_body_bytes"`
	RatePerSender  float64 `mapstructure:"rate_per_sender"`
	BurstPerSender int     `mapstructure:"burst_per_sender"`
}

// MailboxStoreConfig selects the durable mailbox message store backend.
// Backend values: "memory" (default), "postgres", "mongo".
// Memory is ephemeral and survives only for the process lifetime.
type MailboxStoreConfig struct {
	// Backend selects the store implementation: memory|postgres|mongo.
	Backend string `mapstructure:"backend"`
	// DSN is the connection string.
	// Postgres: "postgres://user:pass@host:5432/dbname?sslmode=disable"
	// Mongo:    "mongodb://localhost:27017"
	DSN string `mapstructure:"dsn"`
	// Database is the database/schema name. Required for mongo; ignored for postgres.
	Database string `mapstructure:"database"`
}

// ClusterConfig holds optional multi-node clustering settings.
type ClusterConfig struct {
	RedisURL string `mapstructure:"redis_url"`
}

// ServerConfig holds node and gRPC listener settings.
type ServerConfig struct {
	Port   string `mapstructure:"port"`
	Host   string `mapstructure:"host"`
	Mode   string `mapstructure:"mode"`
	DB     string `mapstructure:"db"`
	NodeID string `mapstructure:"node_id"`
}

// WebhookConfig holds optional webhook delivery dispatcher settings.
type WebhookConfig struct {
	Enabled     bool `mapstructure:"enabled"`
	Workers     int  `mapstructure:"workers"`
	MaxAttempts int  `mapstructure:"max_attempts"`
	TimeoutSecs int  `mapstructure:"timeout_secs"`
}

// SMTPConfig holds optional SMTP gateway settings.
type SMTPConfig struct {
	Enabled      bool   `mapstructure:"enabled"`
	Port         int    `mapstructure:"port"`
	BindAddr     string `mapstructure:"bind_addr"`
	Domain       string `mapstructure:"domain"`
	InsecureAuth bool   `mapstructure:"insecure_auth"`

	MaxMsgBytes    int64 `mapstructure:"max_msg_bytes"`
	MaxRecipients  int   `mapstructure:"max_recipients"`
	MaxConnections int   `mapstructure:"max_connections"`

	ReadTimeoutSecs  int `mapstructure:"read_timeout_secs"`
	WriteTimeoutSecs int `mapstructure:"write_timeout_secs"`

	RateConns      float64 `mapstructure:"rate_conns"`
	RateBurst      int     `mapstructure:"rate_burst"`
	RateConnsPerIP float64 `mapstructure:"rate_conns_per_ip"`
	RateBurstPerIP int     `mapstructure:"rate_burst_per_ip"`

	// RelayEnabled disables RCPT TO validation against the user registry,
	// accepting any recipient. Only enable on trusted private networks.
	RelayEnabled bool `mapstructure:"relay_enabled"`

	// TLS — both must be set to enable STARTTLS.
	TLSCertFile string `mapstructure:"tls_cert_file"`
	TLSKeyFile  string `mapstructure:"tls_key_file"`

	// MaxAuthFailuresPerSession disconnects after N failed AUTH attempts.
	// Default 5 when unset.
	MaxAuthFailuresPerSession int `mapstructure:"max_auth_failures_per_session"`

	// CheckSPF enables server-side SPF verification during MAIL FROM.
	CheckSPF bool `mapstructure:"check_spf"`
	// VerifyDKIM enables DKIM signature verification on inbound messages.
	VerifyDKIM bool `mapstructure:"verify_dkim"`
}

// LMTPConfig holds optional LMTP listener settings.
// LMTP (RFC 2033) is designed for local delivery from a trusted upstream MTA
// (e.g., Postfix) to this message store. The preferred transport is a Unix
// domain socket; TCP is available when the MTA runs on a different host.
type LMTPConfig struct {
	Enabled    bool   `mapstructure:"enabled"`
	SocketPath string `mapstructure:"socket_path"`
	Port       int    `mapstructure:"port"`
	BindAddr   string `mapstructure:"bind_addr"`
	Domain     string `mapstructure:"domain"`

	MaxMsgBytes   int64 `mapstructure:"max_msg_bytes"`
	MaxRecipients int   `mapstructure:"max_recipients"`

	ReadTimeoutSecs  int `mapstructure:"read_timeout_secs"`
	WriteTimeoutSecs int `mapstructure:"write_timeout_secs"`
}

var v = viper.New()

// loadConfig reads the config file at path (if non-empty), then overlays
// environment variables. It does not apply CLI flag values — those are bound
// separately via bindServeFlags and take priority automatically.
func loadConfig(path string) error {
	v.SetEnvPrefix("POSTBOX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	if path == "" {
		return nil
	}
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return fmt.Errorf("read config %q: %w", path, err)
	}
	return nil
}

// resolveConfig merges viper state into a typed Config. Call after loadConfig
// and after CLI flags have been bound via bindServeFlags.
func resolveConfig() (Config, error) {
	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config: %w", err)
	}
	return cfg, nil
}
