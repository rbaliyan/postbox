// Package relay provides outbound email relay backends that implement
// MailboxServiceServer so the forwarder can route external recipients to them
// exactly like any remote postbox node.
//
// Usage:
//
//	backend := relay.NewSendGrid(relay.SendGridConfig{APIKey: "SG.xxx", From: "noreply@example.com"})
//	srv := relay.New(backend)
//	_ = srv.Start("127.0.0.1:0")   // ephemeral loopback port
//
//	// Wrap registry so unknown recipients fall back to the relay node.
//	reg = relay.NewFallbackRegistry(reg, "outbound")
//	// Wrap resolver so the forwarder can dial the relay server.
//	res = relay.NewStaticResolver(map[string]string{"outbound": srv.Addr()}, res)
package relay

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Backend delivers email via an external provider.
// Implementations must be safe for concurrent use.
type Backend interface {
	// Send delivers an email. from and to are RFC 5321 addresses.
	// headers are additional RFC 5322 headers to include in the message.
	Send(ctx context.Context, from string, to []string, subject, body string, headers map[string]string) error
	// Name returns a short human-readable identifier used in log output.
	Name() string
}

// RawBackend is an optional extension of Backend for providers that accept a
// pre-built RFC 5322 message. DKIMSigningBackend requires this interface on its
// inner backend: DKIM signatures are computed over exact wire bytes, so the
// signed message must be sent verbatim rather than rebuilt by the inner backend.
type RawBackend interface {
	Backend
	// SendRaw delivers the pre-built RFC 5322 message bytes.
	SendRaw(ctx context.Context, from string, to []string, raw []byte) error
	// DefaultFrom returns the configured envelope sender address used when the
	// caller supplies an empty from. DKIMSigningBackend reads this before
	// signing so the signature covers the same From: header the inner backend
	// would produce.
	DefaultFrom() string
}

// Option configures a RelayServer.
type Option func(*RelayServer)

// WithRelayLogger sets the structured logger for the relay server.
func WithRelayLogger(l *slog.Logger) Option {
	return func(s *RelayServer) { s.logger = l }
}

// WithQueue enables async delivery with the given queue configuration.
// When set, SendMessage enqueues messages and returns immediately; workers
// retry failed deliveries with exponential backoff.
func WithQueue(cfg QueueConfig) Option {
	return func(s *RelayServer) { s.queueCfg = &cfg }
}

var _ mailboxpb.MailboxServiceServer = (*RelayServer)(nil)

// RelayServer wraps a Backend as a mailboxpb.MailboxServiceServer and manages
// its own gRPC listener so the forwarder can route external recipients to it
// identically to any remote postbox node. All methods except SendMessage return
// Unimplemented; this is a send-only virtual node.
type RelayServer struct {
	mailboxpb.UnimplementedMailboxServiceServer
	backend  Backend
	queue    *RelayQueue  // nil = synchronous delivery; immutable after New
	queueCfg *QueueConfig // held only during New; nil after construction

	logger    *slog.Logger
	startOnce sync.Once
	mu        sync.Mutex
	lis       net.Listener
	grpcSrv   *grpc.Server
}

// New creates a RelayServer backed by backend. The async queue (if configured
// via WithQueue) is started here so it is ready before Start is called.
func New(backend Backend, opts ...Option) *RelayServer {
	s := &RelayServer{backend: backend, logger: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	// Build the queue after options so WithRelayLogger is honoured.
	if s.queueCfg != nil {
		s.queue = NewRelayQueue(s.backend, *s.queueCfg, s.logger)
		s.queueCfg = nil
	}
	return s
}

// Start begins listening on addr. Use "127.0.0.1:0" for an ephemeral loopback
// port — call Addr() after Start to learn the actual address. Calling Start
// more than once returns an error without starting a second listener.
func (s *RelayServer) Start(addr string) error {
	var startErr error
	s.startOnce.Do(func() {
		lis, err := net.Listen("tcp", addr)
		if err != nil {
			startErr = fmt.Errorf("relay: listen %s: %w", addr, err)
			return
		}
		srv := grpc.NewServer()
		mailboxpb.RegisterMailboxServiceServer(srv, s)

		s.mu.Lock()
		s.lis = lis
		s.grpcSrv = srv
		s.mu.Unlock()

		go func() {
			if err := srv.Serve(lis); err != nil {
				s.logger.Error("relay: gRPC serve error", "backend", s.backend.Name(), "error", err)
			}
		}()
		s.logger.Info("relay: started", "backend", s.backend.Name(), "addr", lis.Addr())
	})
	if startErr != nil {
		return startErr
	}
	// If startOnce already ran without error, a second call is a no-op.
	return nil
}

// Stop gracefully drains in-flight RPCs, closes the listener, and waits for
// any async queue workers to finish. The caller's ctx bounds both gRPC drain
// and queue drain; a deadline ensures stuck sessions do not block forever.
func (s *RelayServer) Stop(ctx context.Context) {
	s.mu.Lock()
	srv := s.grpcSrv
	s.mu.Unlock()
	if srv != nil {
		// Enforce the caller's deadline: if GracefulStop blocks past ctx, force-stop.
		stopped := make(chan struct{})
		go func() { srv.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-ctx.Done():
			srv.Stop()
		}
	}
	if s.queue != nil {
		s.queue.Stop(ctx)
	}
}

// Addr returns the listening address after Start, or an empty string before.
func (s *RelayServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lis == nil {
		return ""
	}
	return s.lis.Addr().String()
}

// maxRelayBodyBytes caps the message body accepted by SendMessage to prevent
// a single large message from exhausting relay-node memory. 25 MiB matches the
// RFC 5321 recommended maximum message size.
const maxRelayBodyBytes = 25 << 20

// SendMessage implements MailboxServiceServer. The recipient_ids and deliver_to
// fields are treated as RFC 5321 email addresses; user_id is the sender address.
// The forwarder sets deliver_to when routing a subset of recipients, so we
// prefer that field when it is non-empty.
func (s *RelayServer) SendMessage(ctx context.Context, req *mailboxpb.SendMessageRequest) (*mailboxpb.MessageResponse, error) {
	if len(req.GetBody()) > maxRelayBodyBytes {
		return nil, status.Errorf(codes.InvalidArgument, "relay: message body exceeds %d bytes", maxRelayBodyBytes)
	}

	recipients := req.GetDeliverTo()
	if len(recipients) == 0 {
		recipients = req.GetRecipientIds()
	}
	if len(recipients) == 0 {
		return &mailboxpb.MessageResponse{}, nil
	}

	from := req.GetUserId()
	headers := make(map[string]string, len(req.GetHeaders()))
	for k, v := range req.GetHeaders() {
		headers[k] = v
	}

	if s.queue != nil {
		if err := s.queue.Enqueue(ctx, from, recipients, req.GetSubject(), req.GetBody(), headers); err != nil {
			return nil, status.Errorf(codes.ResourceExhausted, "relay queue full: %v", err)
		}
		return &mailboxpb.MessageResponse{}, nil
	}

	if err := s.backend.Send(ctx, from, recipients, req.GetSubject(), req.GetBody(), headers); err != nil {
		s.logger.Error("relay: send failed", "backend", s.backend.Name(), "from", from, "recipients", recipients, "error", err)
		return nil, status.Errorf(codes.Internal, "relay %s: %v", s.backend.Name(), err)
	}
	s.logger.Info("relay: delivered", "backend", s.backend.Name(), "from", from, "recipients", recipients)
	return &mailboxpb.MessageResponse{}, nil
}
