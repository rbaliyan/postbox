package server

import (
	"context"
	"log/slog"

	"github.com/rbaliyan/mailbox"
	mailboxmemory "github.com/rbaliyan/mailbox/store/memory"
	"github.com/rbaliyan/postbox/internal/relay"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/webhook"
	"github.com/redis/go-redis/v9"
)

// Option configures a Controller. Pass options to NewController.
type Option func(*options)

// MailboxFactory builds and connects a mailbox.Service for a Controller.
// The resolver is the postbox user registry adapter; pass it to
// mailbox.WithUserResolver so sent messages get sender identity metadata.
// The returned service must already be connected; Controller calls Close
// during shutdown.
type MailboxFactory func(ctx context.Context, resolver mailbox.UserResolver) (mailbox.Service, error)

// SMTPFactory builds an SMTPLifecycle for a Controller.
// It is invoked once per StartSMTP call and given the dependencies the
// listener needs to deliver inbound messages: the mailbox service, the user
// store (for AUTH lookups), and the validated SMTP config.
type SMTPFactory func(cfg postboxsmtp.Config, mbx mailbox.Service, deps SMTPDeps) SMTPLifecycle

// SMTPDeps bundles the dependencies an SMTP listener needs.
// Exposed as a struct so additional fields can be added in the future without
// breaking the SMTPFactory signature.
type SMTPDeps struct {
	UserResolver mailbox.UserResolver
	Threads      postboxsmtp.ThreadResolver
}

// SMTPLifecycle is the minimal contract a Controller needs from an SMTP server.
// The internal/smtp.Server satisfies this interface; tests can substitute a fake.
type SMTPLifecycle interface {
	Start() error
	Stop() error
	IsRunning() bool
	Port() int
	Domain() string
}

// LMTPLifecycle is the minimal contract a Controller needs from an LMTP server.
// The internal/smtp.LMTPServer satisfies this interface.
type LMTPLifecycle interface {
	Start() error
	Stop() error
	IsRunning() bool
	Port() int
	Domain() string
	SocketPath() string
}

type options struct {
	nodeID         string
	mode           NodeMode
	address        string
	logger         *slog.Logger
	mailboxFactory MailboxFactory
	smtpFactory    SMTPFactory
	webhookEnabled bool
	webhookOpts    []webhook.Option
	redisClient    redis.UniversalClient
	mailboxPlugins []mailbox.Plugin
	relayNodeID    string
	relayBackend   relay.Backend
	relayQueueCfg  *relay.QueueConfig
}

// WithNodeID sets the node identity. If unset, a UUID is generated.
func WithNodeID(id string) Option { return func(o *options) { o.nodeID = id } }

// WithMode sets the node operating mode. Defaults to ModeStandalone.
func WithMode(m NodeMode) Option { return func(o *options) { o.mode = m } }

// WithAddress sets the publicly reachable gRPC address returned by Discover.
// Defaults to "localhost:50051".
func WithAddress(addr string) Option { return func(o *options) { o.address = addr } }

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithMailboxFactory overrides how the mailbox service is created.
// Useful for tests; production callers can omit it to get the default
// in-memory mailbox.
func WithMailboxFactory(f MailboxFactory) Option {
	return func(o *options) { o.mailboxFactory = f }
}

// WithSMTPFactory overrides how the SMTP server is created.
// The default factory builds an internal/smtp.Server.
func WithSMTPFactory(f SMTPFactory) Option {
	return func(o *options) { o.smtpFactory = f }
}

// WithRedisClient enables the Redis-backed routing registry.
// When set, domain registrations are published to Redis so all nodes in the
// cluster can discover each other via Lookup and Discover RPCs. Node addresses
// are announced with a heartbeat TTL; the controller refreshes the heartbeat
// automatically in the background.
// Accepts redis.UniversalClient so callers may pass *redis.Client,
// *redis.ClusterClient, or *redis.Ring depending on their topology.
func WithRedisClient(client redis.UniversalClient) Option {
	return func(o *options) { o.redisClient = client }
}

// WithWebhook enables the webhook delivery dispatcher. Pass webhook.Option
// values to tune workers, retry behavior, and HTTP client settings.
// When omitted, webhook delivery is disabled.
func WithWebhook(opts ...webhook.Option) Option {
	return func(o *options) {
		o.webhookEnabled = true
		o.webhookOpts = opts
	}
}

// WithMailboxPlugins registers one or more plugins with the mailbox service.
// Plugins are initialised during Controller startup and run in the order given
// for every message that passes through the mailbox.
func WithMailboxPlugins(plugins ...mailbox.Plugin) Option {
	return func(o *options) { o.mailboxPlugins = append(o.mailboxPlugins, plugins...) }
}

// WithOutboundRelay enables outbound email relay for recipients not registered
// in the local store. nodeID is the virtual node identifier used in the routing
// registry (e.g., "outbound"); backend delivers the message to the external
// provider. The forwarder routes any recipient that returns registry.ErrNotFound
// to the relay backend's in-process gRPC server.
func WithOutboundRelay(nodeID string, backend relay.Backend) Option {
	return func(o *options) {
		o.relayNodeID = nodeID
		o.relayBackend = backend
	}
}

// WithOutboundRelayQueue enables async delivery for the outbound relay backend.
// When set, the relay server enqueues messages instead of delivering synchronously;
// workers retry with exponential backoff. Has no effect when WithOutboundRelay is
// not also configured.
func WithOutboundRelayQueue(cfg relay.QueueConfig) Option {
	return func(o *options) { o.relayQueueCfg = &cfg }
}

// newMailboxFactory builds the standard in-memory mailbox service, optionally
// registering plugins. Passing an empty slice is equivalent to no plugins.
func newMailboxFactory(plugins []mailbox.Plugin) MailboxFactory {
	return func(ctx context.Context, resolver mailbox.UserResolver) (mailbox.Service, error) {
		opts := []mailbox.Option{mailbox.WithStore(mailboxmemory.New())}
		if resolver != nil {
			opts = append(opts, mailbox.WithUserResolver(resolver))
		}
		if len(plugins) > 0 {
			opts = append(opts, mailbox.WithPlugins(plugins...))
		}
		svc, err := mailbox.New(mailbox.Config{}, opts...)
		if err != nil {
			return nil, err
		}
		if err := svc.Connect(ctx); err != nil {
			return nil, err
		}
		return svc, nil
	}
}
