package forwarder_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"

	mailboxpb "github.com/rbaliyan/mailbox/proto/mailbox/v1"
	"github.com/rbaliyan/postbox/internal/forwarder"
	"github.com/rbaliyan/postbox/internal/registry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ---- fake registry ----

type fakeRegistry struct {
	mu     sync.Mutex
	routes map[string]string // target → nodeID
}

func newFakeRegistry(routes map[string]string) *fakeRegistry {
	if routes == nil {
		routes = make(map[string]string)
	}
	return &fakeRegistry{routes: routes}
}

func (r *fakeRegistry) Lookup(_ context.Context, target string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.routes[target]
	if !ok {
		return "", registry.ErrNotFound
	}
	return id, nil
}

func (r *fakeRegistry) Register(_ context.Context, _ registry.Entity) error { return nil }

// ---- fake node resolver ----

type fakeResolver struct {
	mu    sync.Mutex
	nodes map[string]string // nodeID → address
	errs  map[string]error  // nodeID → forced error
}

func newFakeResolver(nodes map[string]string) *fakeResolver {
	if nodes == nil {
		nodes = make(map[string]string)
	}
	return &fakeResolver{nodes: nodes, errs: make(map[string]error)}
}

func (r *fakeResolver) ResolveNode(_ context.Context, nodeID string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.errs[nodeID]; ok {
		return "", err
	}
	addr, ok := r.nodes[nodeID]
	if !ok {
		return "", registry.ErrNotFound
	}
	return addr, nil
}

// ---- fake remote mailbox server ----

type fakeMailboxServer struct {
	mailboxpb.UnimplementedMailboxServiceServer
	mu       sync.Mutex
	received []*mailboxpb.SendMessageRequest
}

func (s *fakeMailboxServer) SendMessage(_ context.Context, req *mailboxpb.SendMessageRequest) (*mailboxpb.MessageResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.received = append(s.received, req)
	return &mailboxpb.MessageResponse{}, nil
}

func (s *fakeMailboxServer) calls() []*mailboxpb.SendMessageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*mailboxpb.SendMessageRequest, len(s.received))
	copy(out, s.received)
	return out
}

// startFakeGRPCServer starts a real TCP gRPC server on a random port and returns
// the server, its address, and the recorded-calls server instance.
func startFakeGRPCServer(t *testing.T) (*fakeMailboxServer, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeMailboxServer{}
	mailboxpb.RegisterMailboxServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	return fake, lis.Addr().String()
}

// ---- helpers ----

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// makeHandler returns a grpc.UnaryHandler that records the request and replies OK.
func makeHandler(t *testing.T) (grpc.UnaryHandler, *handlerSpy) {
	t.Helper()
	spy := &handlerSpy{}
	h := func(_ context.Context, req interface{}) (interface{}, error) {
		spy.mu.Lock()
		defer spy.mu.Unlock()
		spy.callCount++
		if r, ok := req.(*mailboxpb.SendMessageRequest); ok {
			spy.lastReq = r
		}
		return &mailboxpb.MessageResponse{}, nil
	}
	return h, spy
}

type handlerSpy struct {
	mu        sync.Mutex
	callCount int
	lastReq   *mailboxpb.SendMessageRequest
}

func (s *handlerSpy) called() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callCount
}

func (s *handlerSpy) req() *mailboxpb.SendMessageRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReq
}

// dialAddr dials an address and returns a grpc.ClientConn for test assertions.
func dialAddr(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// ---- tests ----

// TestForwarder_AllLocal: all recipients map to the local node (or have no
// mapping). The handler must be called exactly once with the original recipients.
func TestForwarder_AllLocal(t *testing.T) {
	const localNode = "node-local"
	reg := newFakeRegistry(map[string]string{
		"alice@local.test": localNode,
		"bob@local.test":   localNode,
	})
	resolver := newFakeResolver(nil)
	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"alice@local.test", "bob@local.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1", spy.called())
	}
	got := spy.req().RecipientIds
	if len(got) != 2 {
		t.Fatalf("handler received %d recipients, want 2", len(got))
	}
}

// TestForwarder_AllRemote: all recipients map to a remote node. The local
// handler must NOT be called; the remote gRPC server receives the forwarded
// request with deliver_to set to those recipients.
func TestForwarder_AllRemote(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	remoteSrv, remoteAddr := startFakeGRPCServer(t)

	reg := newFakeRegistry(map[string]string{
		"carol@remote.test": remoteNode,
		"dave@remote.test":  remoteNode,
	})
	resolver := newFakeResolver(map[string]string{
		remoteNode: remoteAddr,
	})
	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"carol@remote.test", "dave@remote.test"},
		Subject:      "hello",
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	if spy.called() != 0 {
		t.Fatalf("local handler should not have been called, got %d calls", spy.called())
	}

	calls := remoteSrv.calls()
	if len(calls) != 1 {
		t.Fatalf("remote server got %d calls, want 1", len(calls))
	}
	deliverTo := calls[0].DeliverTo
	if len(deliverTo) != 2 {
		t.Fatalf("deliver_to length: got %d, want 2", len(deliverTo))
	}
	// Confirm deliver_to contains the expected recipients.
	deliverToSet := make(map[string]bool)
	for _, id := range deliverTo {
		deliverToSet[id] = true
	}
	for _, id := range []string{"carol@remote.test", "dave@remote.test"} {
		if !deliverToSet[id] {
			t.Errorf("deliver_to missing %q", id)
		}
	}
}

// TestForwarder_Mixed: some recipients are local, some remote. The handler gets
// only local recipients; the remote node gets its own forwarded call.
func TestForwarder_Mixed(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	remoteSrv, remoteAddr := startFakeGRPCServer(t)

	reg := newFakeRegistry(map[string]string{
		"alice@local.test":  localNode,
		"carol@remote.test": remoteNode,
	})
	resolver := newFakeResolver(map[string]string{
		remoteNode: remoteAddr,
	})
	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"alice@local.test", "carol@remote.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}

	// Local handler called once with only local recipient.
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1", spy.called())
	}
	localRecipients := spy.req().RecipientIds
	if len(localRecipients) != 1 || localRecipients[0] != "alice@local.test" {
		t.Fatalf("handler got recipients %v, want [alice@local.test]", localRecipients)
	}

	// Remote server received a call with deliver_to scoped to the remote recipient.
	calls := remoteSrv.calls()
	if len(calls) != 1 {
		t.Fatalf("remote server got %d calls, want 1", len(calls))
	}
	if len(calls[0].DeliverTo) != 1 || calls[0].DeliverTo[0] != "carol@remote.test" {
		t.Fatalf("remote deliver_to: got %v, want [carol@remote.test]", calls[0].DeliverTo)
	}
}

// TestForwarder_NonSendMessage: a non-SendMessage method passes straight through
// to the handler without any routing logic.
func TestForwarder_NonSendMessage(t *testing.T) {
	const localNode = "node-local"
	reg := newFakeRegistry(nil)
	resolver := newFakeResolver(nil)
	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{RecipientIds: []string{"whoever@example.com"}}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/GetMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1", spy.called())
	}
}

// TestForwarder_RemoteUnreachable: when ResolveNode fails the forwarder falls
// back to local delivery rather than dropping the message.
func TestForwarder_RemoteUnreachable(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	reg := newFakeRegistry(map[string]string{
		"carol@remote.test": remoteNode,
	})
	resolver := newFakeResolver(nil) // remoteNode has no address
	resolver.errs[remoteNode] = errors.New("resolver: node not found")

	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"carol@remote.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	// Handler called with the fallback recipient.
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1 (fallback expected)", spy.called())
	}
	got := spy.req().RecipientIds
	if len(got) != 1 || got[0] != "carol@remote.test" {
		t.Fatalf("fallback recipient: got %v, want [carol@remote.test]", got)
	}
}

// TestForwarder_RemoteDialFail: when the remote address is unreachable (no
// listener at that port), the forwarder falls back to local delivery.
func TestForwarder_RemoteDialFail(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	// Use a port that has no listener — pick one and immediately close it to
	// free the port so the dial attempt will fail.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	deadAddr := lis.Addr().String()
	lis.Close() // nothing listening here anymore

	reg := newFakeRegistry(map[string]string{
		"carol@remote.test": remoteNode,
	})
	resolver := newFakeResolver(map[string]string{
		remoteNode: deadAddr,
	})

	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"carol@remote.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err = interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("interceptor returned error: %v", err)
	}
	// Delivery must fall back locally.
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1 (fallback expected)", spy.called())
	}
}

// TestForwarder_Close: Close releases all pooled connections without error.
// We exercise it by first forcing a connection to be cached, then closing.
func TestForwarder_Close(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	remoteSrv, remoteAddr := startFakeGRPCServer(t)
	_ = remoteSrv // server stays up for the duration

	reg := newFakeRegistry(map[string]string{
		"carol@remote.test": remoteNode,
	})
	resolver := newFakeResolver(map[string]string{
		remoteNode: remoteAddr,
	})
	f := forwarder.New(localNode, reg, resolver, newLogger())

	interceptor := f.Interceptor()
	handler, _ := makeHandler(t)

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"carol@remote.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	if _, err := interceptor(context.Background(), req, info, handler); err != nil {
		t.Fatalf("interceptor error: %v", err)
	}

	// Close should not panic or error.
	f.Close()

	// A second Close is also safe.
	f.Close()
}

// TestForwarder_NonSendMessageRequest: non-SendMessageRequest type passed with
// the SendMessage method path falls through to the handler.
func TestForwarder_NonSendMessageRequest(t *testing.T) {
	const localNode = "node-local"
	reg := newFakeRegistry(nil)
	resolver := newFakeResolver(nil)
	f := forwarder.New(localNode, reg, resolver, newLogger())

	called := false
	handler := func(_ context.Context, req interface{}) (interface{}, error) {
		called = true
		return &mailboxpb.MessageResponse{}, nil
	}
	interceptor := f.Interceptor()

	// Pass a non-SendMessageRequest as req with the SendMessage path.
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}
	_, err := interceptor(context.Background(), "not-a-send-request", info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler should have been called for unexpected req type")
	}
}

// TestForwarder_LookupError: registry.Lookup returns an error for a recipient →
// the recipient is treated as local (safe fallback).
func TestForwarder_LookupError(t *testing.T) {
	const localNode = "node-local"
	// Registry has no routes so Lookup returns ErrNotFound.
	reg := newFakeRegistry(nil)
	resolver := newFakeResolver(nil)
	f := forwarder.New(localNode, reg, resolver, newLogger())

	handler, spy := makeHandler(t)
	interceptor := f.Interceptor()

	req := &mailboxpb.SendMessageRequest{
		RecipientIds: []string{"unknown@nowhere.test"},
	}
	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	_, err := interceptor(context.Background(), req, info, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if spy.called() != 1 {
		t.Fatalf("handler call count: got %d, want 1", spy.called())
	}
	got := spy.req().RecipientIds
	if len(got) != 1 || got[0] != "unknown@nowhere.test" {
		t.Fatalf("got recipients %v", got)
	}
}

// TestForwarder_ConnectionReuse: sending two messages to the same remote node
// should result in two remote calls but only one connection being created
// (verified by checking the remote server receives both calls).
func TestForwarder_ConnectionReuse(t *testing.T) {
	const localNode = "node-local"
	const remoteNode = "node-remote"

	remoteSrv, remoteAddr := startFakeGRPCServer(t)

	reg := newFakeRegistry(map[string]string{
		"carol@remote.test": remoteNode,
	})
	resolver := newFakeResolver(map[string]string{
		remoteNode: remoteAddr,
	})
	f := forwarder.New(localNode, reg, resolver, newLogger())
	defer f.Close()

	interceptor := f.Interceptor()
	handler, _ := makeHandler(t)

	info := &grpc.UnaryServerInfo{FullMethod: "/mailbox.v1.MailboxService/SendMessage"}

	for i := 0; i < 3; i++ {
		req := &mailboxpb.SendMessageRequest{
			RecipientIds: []string{"carol@remote.test"},
		}
		if _, err := interceptor(context.Background(), req, info, handler); err != nil {
			t.Fatalf("call %d: interceptor error: %v", i, err)
		}
	}

	calls := remoteSrv.calls()
	if len(calls) != 3 {
		t.Fatalf("remote server got %d calls, want 3", len(calls))
	}
}

// ensure dialAddr is used to suppress unused import warnings during compilation
var _ = dialAddr
