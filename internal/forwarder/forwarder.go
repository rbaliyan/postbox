// Package forwarder provides a gRPC unary interceptor that transparently
// forwards mailbox SendMessage calls to remote Postbox nodes when recipients
// live on a different node.
//
// How it works:
//  1. Each SendMessage call is intercepted before reaching the local handler.
//  2. For each recipient the routing registry is consulted to determine the
//     owning node ID.
//  3. Recipients owned by the local node are delivered via the normal handler.
//  4. Recipients on remote nodes are collected by node address and forwarded
//     with deliver_to scoped to only those recipients, preventing duplicate
//     delivery on the remote node.
//  5. Outbound gRPC connections to remote nodes are pooled and reused.
package forwarder

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/rbaliyan/postbox/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const sendMessageMethod = "/mailbox.v1.MailboxService/SendMessage"

// Forwarder splits SendMessage calls across nodes and forwards remote
// recipients to their owning nodes.
type Forwarder struct {
	nodeID   string
	reg      registry.Registry
	resolver registry.NodeResolver
	logger   *slog.Logger

	mu    sync.Mutex
	conns map[string]*grpc.ClientConn // keyed by remote address
}

// New creates a Forwarder. nodeID is this node's own ID; reg and resolver are
// used to determine which node owns each recipient.
func New(nodeID string, reg registry.Registry, resolver registry.NodeResolver, logger *slog.Logger) *Forwarder {
	return &Forwarder{
		nodeID:   nodeID,
		reg:      reg,
		resolver: resolver,
		logger:   logger,
		conns:    make(map[string]*grpc.ClientConn),
	}
}

// Interceptor returns a gRPC UnaryServerInterceptor. Add it to the gRPC server
// after the auth/logging interceptors:
//
//	grpc.ChainUnaryInterceptor(auth, logging, forwarder.Interceptor())
func (f *Forwarder) Interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if info.FullMethod != sendMessageMethod {
			return handler(ctx, req)
		}
		sendReq, ok := req.(*mailboxpb.SendMessageRequest)
		if !ok {
			return handler(ctx, req)
		}
		return f.handle(ctx, sendReq, handler)
	}
}

// handle classifies recipients, forwards remote ones, and invokes the local
// handler for the remainder.
func (f *Forwarder) handle(ctx context.Context, req *mailboxpb.SendMessageRequest, handler grpc.UnaryHandler) (interface{}, error) {
	// remoteByAddr maps remote node address → recipient IDs on that node.
	remoteByAddr := map[string][]string{}
	var localRecipients []string

	for _, recipID := range req.RecipientIds {
		nodeID, err := f.reg.Lookup(ctx, recipID)
		if err != nil || nodeID == f.nodeID {
			localRecipients = append(localRecipients, recipID)
			continue
		}
		addr, err := f.resolver.ResolveNode(ctx, nodeID)
		if err != nil {
			// Can't reach the remote node — fall back to local delivery so the
			// message isn't silently dropped.
			f.logger.Warn("forwarder: cannot resolve remote node, delivering locally",
				"recipient", recipID, "node", nodeID, "error", err)
			localRecipients = append(localRecipients, recipID)
			continue
		}
		remoteByAddr[addr] = append(remoteByAddr[addr], recipID)
	}

	// Forward to each remote node.
	for addr, recipients := range remoteByAddr {
		if err := f.forward(ctx, req, addr, recipients); err != nil {
			f.logger.Error("forwarder: remote delivery failed, falling back to local",
				"addr", addr, "recipients", recipients, "error", err)
			localRecipients = append(localRecipients, recipients...)
		}
	}

	// Deliver local recipients (may be all, some, or none).
	if len(localRecipients) == 0 {
		// All recipients were forwarded — return a synthetic response.
		return &mailboxpb.MessageResponse{}, nil
	}
	localReq := cloneReq(req)
	localReq.RecipientIds = localRecipients
	return handler(ctx, localReq)
}

// forward sends req to the remote node at addr, restricting delivery to
// recipients via the deliver_to field so the remote node doesn't double-deliver
// to any local recipients that appear in recipient_ids.
func (f *Forwarder) forward(ctx context.Context, req *mailboxpb.SendMessageRequest, addr string, recipients []string) error {
	conn, err := f.dial(addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	client := mailboxpb.NewMailboxServiceClient(conn)

	fwd := cloneReq(req)
	fwd.DeliverTo = recipients // restrict delivery to only these recipients

	_, err = client.SendMessage(ctx, fwd)
	if err != nil {
		if status.Code(err) == codes.Unavailable {
			// Evict the cached connection so the next attempt re-dials.
			f.evict(addr)
		}
		return fmt.Errorf("forward to %s: %w", addr, err)
	}
	f.logger.Info("forwarder: forwarded message", "addr", addr, "recipients", recipients)
	return nil
}

// dial returns a cached connection to addr, creating one if needed.
func (f *Forwarder) dial(addr string) (*grpc.ClientConn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if conn, ok := f.conns[addr]; ok {
		return conn, nil
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	f.conns[addr] = conn
	return conn, nil
}

func (f *Forwarder) evict(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if conn, ok := f.conns[addr]; ok {
		_ = conn.Close()
		delete(f.conns, addr)
	}
}

// Close releases all pooled outbound connections.
func (f *Forwarder) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for addr, conn := range f.conns {
		_ = conn.Close()
		delete(f.conns, addr)
	}
}

// cloneReq returns a deep clone of req so the caller can mutate fields without
// affecting the original.
func cloneReq(req *mailboxpb.SendMessageRequest) *mailboxpb.SendMessageRequest {
	return proto.Clone(req).(*mailboxpb.SendMessageRequest)
}
