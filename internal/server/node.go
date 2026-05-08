// Package server wires together the store, registry, mailbox service, and
// optional SMTP listener into a single Controller, and exposes them as a
// PostboxServer gRPC implementation.
package server

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rbaliyan/mailbox"
	mailboxstore "github.com/rbaliyan/mailbox/store"
	"github.com/rbaliyan/postbox/internal/forwarder"
	"github.com/rbaliyan/postbox/internal/nodekey"
	"github.com/rbaliyan/postbox/internal/registry"
	localreg "github.com/rbaliyan/postbox/internal/registry/local"
	redisreg "github.com/rbaliyan/postbox/internal/registry/redis"
	"github.com/rbaliyan/postbox/internal/relay"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/webhook"
)

// NodeMode selects the storage backend.
//
//	ModeStandalone — persistent SQLite file (default).
//	ModeTemp       — in-memory SQLite; state is discarded on shutdown.
type NodeMode string

const (
	ModeStandalone NodeMode = "standalone"
	ModeTemp       NodeMode = "temp"
)

const defaultAddress = "localhost:50051"

// ErrSMTPAlreadyRunning is returned by StartSMTP when an SMTP listener is
// already active.
var ErrSMTPAlreadyRunning = errors.New("smtp: already running")

// ErrSMTPNotRunning is returned by StopSMTP when no SMTP listener is active.
var ErrSMTPNotRunning = errors.New("smtp: not running")

// ErrLMTPAlreadyRunning is returned by StartLMTP when an LMTP listener is
// already active.
var ErrLMTPAlreadyRunning = errors.New("lmtp: already running")

// ErrLMTPNotRunning is returned by StopLMTP when no LMTP listener is active.
var ErrLMTPNotRunning = errors.New("lmtp: not running")

// announcer is the minimal interface needed to refresh this node's address in
// the cluster registry. redisreg.Registry implements it.
type announcer interface {
	Announce(ctx context.Context) error
}

// Controller owns the lifecycle of the backing store, registry, mailbox
// service, and optional SMTP server for a single Postbox node.
//
// Controller is safe for concurrent use. Construct one with NewController
// and call Close on shutdown.
type Controller struct {
	nodeID        string
	mode          NodeMode
	address       string
	store         store.Store
	registry      registry.Registry
	mailbox       mailbox.Service
	logger        *slog.Logger
	nodePrivKey   ed25519.PrivateKey
	nodePubKeyB64 string
	dispatcher    *webhook.Dispatcher
	threads       postboxsmtp.ThreadResolver
	fwd           *forwarder.Forwarder // nil when Redis is not configured
	heartbeater   announcer            // non-nil when Redis is configured; stored before FallbackRegistry wrapping

	smtpFactory SMTPFactory
	relaySrv    *relay.RelayServer // nil when outbound relay is not configured

	smtpMu      sync.Mutex
	smtp        SMTPLifecycle
	lmtpMu      sync.Mutex
	lmtp        LMTPLifecycle
	heartbeatCh chan struct{} // closed to stop the heartbeat goroutine
}

// NewController connects the store, persists the node record, and starts the
// in-process mailbox service. Call Close when done.
//
// Configuration is supplied via Option values; sane defaults apply when an
// option is omitted.
func NewController(ctx context.Context, s store.Store, opts ...Option) (*Controller, error) {
	if s == nil {
		return nil, errors.New("controller: store is required")
	}

	cfg := options{
		mode:        ModeStandalone,
		address:     defaultAddress,
		logger:      slog.Default(),
		smtpFactory: defaultSMTPFactory,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.nodeID == "" {
		cfg.nodeID = uuid.New().String()
	}
	if cfg.mailboxFactory == nil {
		cfg.mailboxFactory = newMailboxFactory(cfg.mailboxPlugins)
	}

	if err := s.Connect(ctx); err != nil {
		return nil, fmt.Errorf("controller: connect store: %w", err)
	}

	node, err := nodekey.EnsureKey(ctx, s, store.Node{ID: cfg.nodeID})
	if err != nil {
		return nil, fmt.Errorf("controller: ensure node key: %w", err)
	}

	privKey, err := nodekey.DecodePrivate(node.PrivateKeyB64)
	if err != nil {
		return nil, fmt.Errorf("controller: decode node private key: %w", err)
	}

	resolver := storeUserResolver{s}
	mbx, err := cfg.mailboxFactory(ctx, resolver)
	if err != nil {
		return nil, fmt.Errorf("controller: create mailbox service: %w", err)
	}

	var reg registry.Registry
	var nodeResolver registry.NodeResolver // non-nil when Redis or relay is present
	var fwd *forwarder.Forwarder
	var heartbeatCh chan struct{}
	var heartbeater announcer // captured before FallbackRegistry wrapping

	if cfg.redisClient != nil {
		rr := redisreg.New(cfg.redisClient, cfg.nodeID, cfg.address, s)
		if err := rr.Announce(ctx); err != nil {
			return nil, fmt.Errorf("controller: announce node to Redis: %w", err)
		}
		reg = rr
		nodeResolver = rr
		heartbeatCh = make(chan struct{})
		heartbeater = rr // store before FallbackRegistry may wrap reg
	} else {
		reg = localreg.New(cfg.nodeID, s)
	}

	// Apply outbound relay: route unknown recipients to the relay's gRPC server.
	var relaySrv *relay.RelayServer
	if cfg.relayBackend != nil {
		relayOpts := []relay.Option{relay.WithRelayLogger(cfg.logger)}
		if cfg.relayQueueCfg != nil {
			relayOpts = append(relayOpts, relay.WithQueue(*cfg.relayQueueCfg))
		}
		relaySrv = relay.New(cfg.relayBackend, relayOpts...)
		if err := relaySrv.Start("127.0.0.1:0"); err != nil {
			return nil, fmt.Errorf("controller: start relay: %w", err)
		}
		reg = relay.NewFallbackRegistry(reg, cfg.relayNodeID)
		nodeResolver = relay.NewStaticResolver(
			map[string]string{cfg.relayNodeID: relaySrv.Addr()},
			nodeResolver, // base — nil in single-node mode, Redis registry otherwise
		)
	}

	// Create forwarder when there is a resolver: multi-node (Redis) or relay.
	if nodeResolver != nil {
		fwd = forwarder.New(cfg.nodeID, reg, nodeResolver, cfg.logger)
	}

	ctrl := &Controller{
		nodeID:        cfg.nodeID,
		mode:          cfg.mode,
		address:       cfg.address,
		store:         s,
		registry:      reg,
		mailbox:       mbx,
		logger:        cfg.logger,
		nodePrivKey:   privKey,
		nodePubKeyB64: node.PublicKeyB64,
		smtpFactory:   cfg.smtpFactory,
		relaySrv:      relaySrv,
		threads:       buildThreadResolver(mbx),
		fwd:           fwd,
		heartbeatCh:   heartbeatCh,
		heartbeater:   heartbeater,
	}

	if cfg.redisClient != nil {
		go ctrl.runHeartbeat()
	}

	if cfg.webhookEnabled {
		signer, err := webhook.NewSigner(privKey, "v1")
		if err != nil {
			return nil, fmt.Errorf("controller: create webhook signer: %w", err)
		}
		d := webhook.New(resolver, s, mbx, signer, cfg.webhookOpts...)
		if err := d.Start(ctx); err != nil {
			return nil, fmt.Errorf("controller: start webhook dispatcher: %w", err)
		}
		ctrl.dispatcher = d
	}

	return ctrl, nil
}

// defaultSMTPFactory builds the standard internal/smtp.Server.
func defaultSMTPFactory(cfg postboxsmtp.Config, mbx mailbox.Service, deps SMTPDeps) SMTPLifecycle {
	return postboxsmtp.New(cfg, mbx, deps.UserResolver, deps.Threads)
}

// NodePrivateKey returns the node's Ed25519 private key.
func (c *Controller) NodePrivateKey() ed25519.PrivateKey { return c.nodePrivKey }

// NodePublicKeyB64 returns the node's Ed25519 public key as a base64 string.
func (c *Controller) NodePublicKeyB64() string { return c.nodePubKeyB64 }

// NodeID returns the unique identifier for this node.
func (c *Controller) NodeID() string { return c.nodeID }

// Mode returns the operating mode of this node.
func (c *Controller) Mode() NodeMode { return c.mode }

// Address returns the publicly advertised gRPC address for this node.
func (c *Controller) Address() string { return c.address }

// Mailbox returns the connected in-process mailbox service.
// The caller must not call Connect or Close on the returned service.
func (c *Controller) Mailbox() mailbox.Service { return c.mailbox }

// StartSMTP creates and starts the SMTP listener with the given config.
// Returns ErrSMTPAlreadyRunning if an SMTP listener is already running.
func (c *Controller) StartSMTP(cfg postboxsmtp.Config) error {
	c.smtpMu.Lock()
	defer c.smtpMu.Unlock()
	if c.smtp != nil && c.smtp.IsRunning() {
		return fmt.Errorf("%w on port %d", ErrSMTPAlreadyRunning, c.smtp.Port())
	}
	srv := c.smtpFactory(cfg, c.mailbox, SMTPDeps{UserResolver: storeUserResolver{c.store}, Threads: c.threads})
	if err := srv.Start(); err != nil {
		return err
	}
	c.smtp = srv
	c.logger.Info("smtp listener started", "port", srv.Port(), "domain", srv.Domain())
	return nil
}

// StopSMTP stops the SMTP listener.
// Returns ErrSMTPNotRunning if no listener is currently active.
func (c *Controller) StopSMTP() error {
	c.smtpMu.Lock()
	srv := c.smtp
	c.smtpMu.Unlock()
	if srv == nil || !srv.IsRunning() {
		return ErrSMTPNotRunning
	}
	if err := srv.Stop(); err != nil {
		return err
	}
	c.logger.Info("smtp listener stopped")
	return nil
}

// smtpServer returns the current SMTP server, or nil if none is configured.
// Internal helper for PostboxServer.
func (c *Controller) smtpServer() SMTPLifecycle {
	c.smtpMu.Lock()
	defer c.smtpMu.Unlock()
	return c.smtp
}

// StartLMTP creates and starts the LMTP listener with the given config.
// Returns ErrLMTPAlreadyRunning if an LMTP listener is already running.
func (c *Controller) StartLMTP(cfg postboxsmtp.LMTPConfig) error {
	c.lmtpMu.Lock()
	defer c.lmtpMu.Unlock()
	if c.lmtp != nil && c.lmtp.IsRunning() {
		return fmt.Errorf("%w on port %d", ErrLMTPAlreadyRunning, c.lmtp.Port())
	}
	srv := postboxsmtp.NewLMTP(cfg, c.mailbox, c.threads)
	if err := srv.Start(); err != nil {
		return err
	}
	c.lmtp = srv
	if cfg.SocketPath != "" {
		c.logger.Info("lmtp listener started", "socket", cfg.SocketPath, "domain", srv.Domain())
	} else {
		c.logger.Info("lmtp listener started", "port", srv.Port(), "domain", srv.Domain())
	}
	return nil
}

// StopLMTP stops the LMTP listener.
// Returns ErrLMTPNotRunning if no listener is currently active.
func (c *Controller) StopLMTP() error {
	c.lmtpMu.Lock()
	srv := c.lmtp
	c.lmtpMu.Unlock()
	if srv == nil || !srv.IsRunning() {
		return ErrLMTPNotRunning
	}
	if err := srv.Stop(); err != nil {
		return err
	}
	c.logger.Info("lmtp listener stopped")
	return nil
}

// Close stops the SMTP listener (if running), the webhook dispatcher, the
// mailbox service, and the store. Errors are logged but do not abort the
// sequence so cleanup is best-effort.
func (c *Controller) Close(ctx context.Context) error {
	if c.heartbeatCh != nil {
		close(c.heartbeatCh)
	}
	if c.fwd != nil {
		c.fwd.Close()
	}
	if c.relaySrv != nil {
		c.relaySrv.Stop(ctx)
	}
	c.smtpMu.Lock()
	srv := c.smtp
	c.smtpMu.Unlock()
	if srv != nil && srv.IsRunning() {
		if err := srv.Stop(); err != nil {
			c.logger.Error("stop smtp", "error", err)
		}
	}
	c.lmtpMu.Lock()
	lsrv := c.lmtp
	c.lmtpMu.Unlock()
	if lsrv != nil && lsrv.IsRunning() {
		if err := lsrv.Stop(); err != nil {
			c.logger.Error("stop lmtp", "error", err)
		}
	}
	if c.dispatcher != nil {
		c.dispatcher.Stop()
	}
	if err := c.mailbox.Close(ctx); err != nil {
		c.logger.Error("close mailbox service", "error", err)
	}
	return c.store.Close(ctx)
}

// buildThreadResolver creates a ThreadResolver backed by the mailbox service.
// It looks up the parent message by SMTP Message-ID stored as ExternalID in
// the recipient's inbox and returns the corresponding mailbox UUID.
func buildThreadResolver(mbx mailbox.Service) postboxsmtp.ThreadResolver {
	return postboxsmtp.ThreadResolverFunc(func(ctx context.Context, recipientID, smtpMessageID string) (string, error) {
		iter, err := mbx.Client(recipientID).Stream(ctx,
			[]mailboxstore.Filter{mailboxstore.ExternalIDIs(smtpMessageID)},
			mailbox.StreamOptions{BatchSize: 1},
		)
		if err != nil {
			return "", err
		}
		hasNext, err := iter.Next(ctx)
		if err != nil {
			return "", err
		}
		if !hasNext {
			return "", store.ErrNotFound
		}
		msg, err := iter.Message()
		if err != nil {
			return "", err
		}
		return msg.GetID(), nil
	})
}

// Forwarder returns the message forwarder when Redis is configured, or nil in
// single-node mode. Use its Interceptor() to add cross-node delivery to the
// gRPC server.
func (c *Controller) Forwarder() *forwarder.Forwarder { return c.fwd }

// runHeartbeat periodically refreshes this node's address in Redis until the
// heartbeatCh channel is closed. It uses c.heartbeater, which is captured
// before any FallbackRegistry wrapping so the type assertion always succeeds.
func (c *Controller) runHeartbeat() {
	if c.heartbeater == nil {
		return
	}
	ticker := time.NewTicker(redisreg.HeartbeatTTL / 2)
	defer ticker.Stop()
	for {
		select {
		case <-c.heartbeatCh:
			return
		case <-ticker.C:
			if err := c.heartbeater.Announce(context.Background()); err != nil {
				c.logger.Error("heartbeat: announce failed", "error", err)
			}
		}
	}
}

// Router returns a mailbox.Router that resolves user IDs to their Mailbox on
// this node. The returned router verifies the user exists in the store before
// returning the mailbox client, so unknown users get a clear ErrNotFound.
func (c *Controller) Router() mailbox.Router {
	return &localRouter{store: c.store, svc: c.mailbox}
}

// localRouter implements mailbox.Router for a single-node deployment.
// Route verifies the user exists in the store then delegates to the in-process
// mailbox service. Swap for a Redis-backed implementation in Phase 3.
type localRouter struct {
	store store.Store
	svc   mailbox.Service
}

var _ mailbox.Router = (*localRouter)(nil)

func (r *localRouter) Route(ctx context.Context, userID string) (mailbox.Mailbox, error) {
	if _, err := r.store.GetUser(ctx, userID); err != nil {
		return nil, err
	}
	return r.svc.Client(userID), nil
}

// storeUserResolver adapts store.Store to mailbox.UserResolver.
type storeUserResolver struct{ s store.Store }

func (r storeUserResolver) ResolveUser(ctx context.Context, userID string) (mailbox.User, error) {
	u, err := r.s.GetUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	return storeMailboxUser{u}, nil
}

// storeMailboxUser bridges store.User to the mailbox.User interface.
type storeMailboxUser struct{ u store.User }

func (u storeMailboxUser) ID() string                      { return u.u.Email }
func (u storeMailboxUser) FirstName() string               { return "" }
func (u storeMailboxUser) LastName() string                { return "" }
func (u storeMailboxUser) Email() string                   { return u.u.Email }
func (u storeMailboxUser) Type() string                    { return u.u.Type }
func (u storeMailboxUser) PublicKey() string               { return u.u.PublicKeyB64 }
func (u storeMailboxUser) Capabilities() map[string]string { return u.u.Metadata }
