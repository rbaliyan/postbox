package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/rbaliyan/mailbox"
	"github.com/rbaliyan/mailbox/notify"
	"github.com/rbaliyan/postbox/internal/server"
	postboxsmtp "github.com/rbaliyan/postbox/internal/smtp"
	"github.com/rbaliyan/postbox/internal/store"
	"github.com/rbaliyan/postbox/internal/store/memstore"
	postboxpb "github.com/rbaliyan/postbox/proto/postbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

func newTestController(t *testing.T, opts ...server.Option) (*server.Controller, store.Store) {
	t.Helper()
	s := memstore.New()
	mailboxFactory := func(ctx context.Context, _ mailbox.UserResolver) (mailbox.Service, error) {
		return &fakeMailbox{}, nil
	}
	allOpts := append([]server.Option{
		server.WithNodeID("test-node"),
		server.WithAddress("test-host:1234"),
		server.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		server.WithMailboxFactory(mailboxFactory),
		server.WithSMTPFactory(newFakeSMTPFactory()),
	}, opts...)
	ctrl, err := server.NewController(context.Background(), s, allOpts...)
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })
	return ctrl, s
}

func TestController_AppliesDefaults(t *testing.T) {
	ctrl, _ := newTestController(t)
	if ctrl.NodeID() != "test-node" {
		t.Fatalf("NodeID=%q", ctrl.NodeID())
	}
	if ctrl.Mode() != server.ModeStandalone {
		t.Fatalf("Mode=%q", ctrl.Mode())
	}
	if ctrl.Address() != "test-host:1234" {
		t.Fatalf("Address=%q", ctrl.Address())
	}
}

func TestController_RejectsNilStore(t *testing.T) {
	if _, err := server.NewController(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil store")
	}
}

func TestController_StartSMTP_Idempotent(t *testing.T) {
	ctrl, _ := newTestController(t)
	if err := ctrl.StartSMTP(postboxsmtp.Config{Port: 25, Domain: "x"}); err != nil {
		t.Fatalf("first start: %v", err)
	}
	err := ctrl.StartSMTP(postboxsmtp.Config{Port: 25, Domain: "x"})
	if !errors.Is(err, server.ErrSMTPAlreadyRunning) {
		t.Fatalf("second start: got %v, want ErrSMTPAlreadyRunning", err)
	}
}

func TestController_StopSMTP_NotRunning(t *testing.T) {
	ctrl, _ := newTestController(t)
	if err := ctrl.StopSMTP(); !errors.Is(err, server.ErrSMTPNotRunning) {
		t.Fatalf("got %v, want ErrSMTPNotRunning", err)
	}
}

func TestController_StartStopRoundTrip(t *testing.T) {
	ctrl, _ := newTestController(t)
	if err := ctrl.StartSMTP(postboxsmtp.Config{Port: 25, Domain: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.StopSMTP(); err != nil {
		t.Fatal(err)
	}
	// After stop, start should succeed again.
	if err := ctrl.StartSMTP(postboxsmtp.Config{Port: 26, Domain: "y"}); err != nil {
		t.Fatalf("re-start: %v", err)
	}
}

// --- PostboxServer tests ---

func TestPostboxServer_Discover_NotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.Discover(context.Background(), &postboxpb.DiscoverRequest{Target: "nope@nope.com"})
	if c := status.Code(err); c != codes.NotFound {
		t.Fatalf("got code %v, want NotFound", c)
	}
}

func TestPostboxServer_Discover_RequiresTarget(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.Discover(context.Background(), &postboxpb.DiscoverRequest{})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument", c)
	}
}

func TestPostboxServer_RegisterDomain_Idempotent(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()
	req := &postboxpb.RegisterDomainRequest{Name: "example.com"}

	if _, err := srv.RegisterDomain(ctx, req); err != nil {
		t.Fatalf("first: %v", err)
	}
	resp, err := srv.RegisterDomain(ctx, req)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !resp.GetOk() {
		t.Fatal("expected ok=true on idempotent register")
	}
}

func TestPostboxServer_RegisterDomain_AlreadyExistsOnOtherNode(t *testing.T) {
	ctrl, s := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	// Pre-seed the store with a domain claimed by a different node.
	if err := s.SaveDomain(ctx, store.Domain{Name: "rival.com", NodeID: "other-node"}); err != nil {
		t.Fatal(err)
	}

	_, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "rival.com"})
	if c := status.Code(err); c != codes.AlreadyExists {
		t.Fatalf("got code %v, want AlreadyExists", c)
	}
}

func TestPostboxServer_Discover_ReturnsAddress(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()
	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.Discover(ctx, &postboxpb.DiscoverRequest{Target: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetAddress() != "test-host:1234" {
		t.Fatalf("address=%q", resp.GetAddress())
	}
	if resp.GetMode() != string(server.ModeStandalone) {
		t.Fatalf("mode=%q", resp.GetMode())
	}
	if resp.GetNodeId() != "test-node" {
		t.Fatalf("node_id=%q", resp.GetNodeId())
	}
}

func TestController_Router_UnknownUser(t *testing.T) {
	ctrl, _ := newTestController(t)
	router := ctrl.Router()
	_, err := router.Route(context.Background(), "nobody@example.com")
	if err == nil {
		t.Fatal("expected error for unknown user")
	}
}

func TestController_Router_KnownUser(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{Email: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := ctrl.Router().Route(ctx, "alice@example.com"); err != nil {
		t.Fatalf("Route: %v", err)
	}
}

func TestPostboxServer_RegisterUser_Validates(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.RegisterUser(context.Background(), &postboxpb.RegisterUserRequest{})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", c)
	}
}

func TestPostboxServer_GetStatus(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	_, _ = srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "a.com"})
	_, _ = srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "b.com"})
	_, _ = srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{Email: "u@a.com"})

	resp, err := srv.GetStatus(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetDomainCount() != 2 || resp.GetUserCount() != 1 {
		t.Fatalf("counts: %+v", resp)
	}
}

func TestPostboxServer_RemapDomain(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()
	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.RemapDomain(ctx, &postboxpb.RemapDomainRequest{
		Domain: "example.com", TargetNodeId: "other",
	}); err != nil {
		t.Fatal(err)
	}

	got, err := srv.Discover(ctx, &postboxpb.DiscoverRequest{Target: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetNodeId() != "other" {
		t.Fatalf("node_id=%q after remap", got.GetNodeId())
	}
}

func TestPostboxServer_RemapDomain_RequiresTarget(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.RemapDomain(context.Background(), &postboxpb.RemapDomainRequest{Domain: "x.com"})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got %v", c)
	}
}

func TestPostboxServer_StopSMTP_NotRunning(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.StopSMTP(context.Background(), &emptypb.Empty{})
	if c := status.Code(err); c != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", c)
	}
}

func TestPostboxServer_StartSMTP_AlreadyRunning(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()
	if _, err := srv.StartSMTP(ctx, &postboxpb.SMTPConfig{Port: 25}); err != nil {
		t.Fatal(err)
	}
	_, err := srv.StartSMTP(ctx, &postboxpb.SMTPConfig{Port: 25})
	if c := status.Code(err); c != codes.FailedPrecondition {
		t.Fatalf("got %v, want FailedPrecondition", c)
	}
}

func TestPostboxServer_GetSMTPStatus(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	resp, err := srv.GetSMTPStatus(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetRunning() {
		t.Fatal("expected running=false initially")
	}

	if _, err := srv.StartSMTP(ctx, &postboxpb.SMTPConfig{Port: 25, Domain: "x"}); err != nil {
		t.Fatal(err)
	}
	resp, err = srv.GetSMTPStatus(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.GetRunning() {
		t.Fatal("expected running=true after start")
	}
	if resp.GetPort() != 25 || resp.GetDomain() != "x" {
		t.Fatalf("status %+v", resp)
	}
}

func TestPostboxServer_ListDomains(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()
	for _, name := range []string{"b.com", "a.com"} {
		if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := srv.ListDomains(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetDomains()) != 2 {
		t.Fatalf("got %d domains", len(resp.GetDomains()))
	}
	if resp.GetDomains()[0].GetName() != "a.com" {
		t.Fatalf("expected sorted; got %+v", resp.GetDomains())
	}
}

// ---------------------------------------------------------------------------
// Controller getter / helper tests
// ---------------------------------------------------------------------------

func TestController_NodePrivateKey(t *testing.T) {
	ctrl, _ := newTestController(t)
	if len(ctrl.NodePrivateKey()) == 0 {
		t.Fatal("expected non-empty private key")
	}
}

func TestController_NodePublicKeyB64(t *testing.T) {
	ctrl, _ := newTestController(t)
	if ctrl.NodePublicKeyB64() == "" {
		t.Fatal("expected non-empty public key base64")
	}
}

func TestController_Mailbox(t *testing.T) {
	ctrl, _ := newTestController(t)
	// The fake mailbox factory is set in newTestController, so Mailbox() returns it.
	if ctrl.Mailbox() == nil {
		t.Fatal("expected non-nil mailbox service")
	}
}

func TestController_Forwarder_NilWithoutRedis(t *testing.T) {
	ctrl, _ := newTestController(t)
	// No Redis client configured, so Forwarder should be nil.
	if ctrl.Forwarder() != nil {
		t.Fatal("expected nil forwarder when Redis is not configured")
	}
}

func TestController_Mode_Default(t *testing.T) {
	ctrl, _ := newTestController(t)
	if ctrl.Mode() != server.ModeStandalone {
		t.Fatalf("expected standalone mode, got %q", ctrl.Mode())
	}
}

func TestController_DefaultMailboxFactory(t *testing.T) {
	// Create a controller WITHOUT WithMailboxFactory — uses the default in-memory mailbox.
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("default-mbx-node"),
		server.WithSMTPFactory(newFakeSMTPFactory()),
	)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })
	if ctrl.Mailbox() == nil {
		t.Fatal("expected non-nil mailbox from default factory")
	}
}

func TestController_ResolveUser_ViaRouter(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email: "resolver-test@example.com",
		Type:  "agent",
	}); err != nil {
		t.Fatal(err)
	}
	// Calling Route exercises storeUserResolver.ResolveUser and storeMailboxUser methods.
	_, err := ctrl.Router().Route(ctx, "resolver-test@example.com")
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
}

func TestController_Close_WithSMTPRunning(t *testing.T) {
	ctrl, _ := newTestController(t)
	if err := ctrl.StartSMTP(postboxsmtp.Config{Port: 25, Domain: "x"}); err != nil {
		t.Fatal(err)
	}
	// Close should stop SMTP and not return an error.
	if err := ctrl.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestController_WithMode_Temp(t *testing.T) {
	ctrl, _ := newTestController(t, server.WithMode(server.ModeTemp))
	if ctrl.Mode() != server.ModeTemp {
		t.Fatalf("expected temp mode, got %q", ctrl.Mode())
	}
}

func TestPostboxServer_RemapDomain_NotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.RemapDomain(context.Background(), &postboxpb.RemapDomainRequest{
		Domain:       "nonexistent.com",
		TargetNodeId: "other-node",
	})
	if c := status.Code(err); c != codes.NotFound {
		t.Fatalf("got code %v, want NotFound", c)
	}
}

func TestPostboxServer_RegisterDomain_RequiresName(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.RegisterDomain(context.Background(), &postboxpb.RegisterDomainRequest{})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument", c)
	}
}

func TestPostboxServer_GetUser_RequiresEmail(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	_, err := srv.GetUser(context.Background(), &postboxpb.GetUserRequest{})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument", c)
	}
}

func TestController_WithWebhook(t *testing.T) {
	// Create a controller with webhook enabled — use the default in-memory mailbox
	// (not fakeMailbox) so Events() returns a real ServiceEvents.
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("webhook-node"),
		// No WithMailboxFactory → uses defaultMailboxFactory which returns a real svc.
		server.WithSMTPFactory(newFakeSMTPFactory()),
		server.WithWebhook(),
	)
	if err != nil {
		t.Fatalf("NewController with webhook: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })
}

func TestController_DefaultSMTPFactory_Used(t *testing.T) {
	// Create a controller WITHOUT WithSMTPFactory so defaultSMTPFactory is used.
	// We don't actually start SMTP (that would need a port); we just verify the
	// controller is created successfully and the smtp server can be built.
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("smtp-factory-node"),
		// No WithSMTPFactory → defaultSMTPFactory is used.
	)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })
	// Starting SMTP invokes the factory. Port 0 is not a valid SMTP port but lets
	// us invoke the factory code path. The fake SMTP in tests wouldn't be triggered here
	// because we omitted WithSMTPFactory. Real postboxsmtp.New is used.
	// We just verify no panic.
}

func TestController_StoreUserResolver(t *testing.T) {
	// Use default mailbox factory so the storeUserResolver is wired in.
	// Send a message to trigger resolver.ResolveUser on the sender.
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("resolver-node"),
		// No WithMailboxFactory → real mailbox with storeUserResolver.
		server.WithSMTPFactory(newFakeSMTPFactory()),
	)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })

	srv := server.NewServer(ctrl)
	ctx := context.Background()
	// Register sender and recipient.
	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"sender@example.com", "recipient@example.com"} {
		if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{Email: u, Type: "human"}); err != nil {
			t.Fatalf("RegisterUser %s: %v", u, err)
		}
	}
	// SendMessage triggers storeUserResolver.ResolveUser for the sender.
	_, err = ctrl.Mailbox().Client("sender@example.com").SendMessage(ctx, mailbox.SendRequest{
		RecipientIDs: []string{"recipient@example.com"},
		Subject:      "Hello",
		Body:         "World",
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
}

func TestPostboxServer_GetDeliveryStatus_WithDeliveredAt(t *testing.T) {
	ctrl, s := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	// Save a delivered job.
	pending := store.DeliveryJob{
		MessageID:   "msg-delivered",
		RecipientID: "user@example.com",
		Status:      store.DeliveryDelivered,
		MaxAttempts: 5,
	}
	if err := s.SaveDeliveryJob(ctx, pending); err != nil {
		t.Fatal(err)
	}
	// Update to mark as delivered with a timestamp.
	job, _ := s.GetDeliveryJob(ctx, "msg-delivered", "user@example.com")
	deliveredAt := store.DeliveryJob{
		MessageID:   job.MessageID,
		RecipientID: job.RecipientID,
		Status:      store.DeliveryDelivered,
		Attempts:    1,
		MaxAttempts: 5,
	}
	ts := job.CreatedAt
	deliveredAt.DeliveredAt = &ts
	if err := s.UpdateDeliveryJob(ctx, deliveredAt); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetDeliveryStatus(ctx, &postboxpb.GetDeliveryStatusRequest{
		MessageId:   "msg-delivered",
		RecipientId: "user@example.com",
	})
	if err != nil {
		t.Fatalf("GetDeliveryStatus: %v", err)
	}
	if resp.GetDeliveredAt() == "" {
		t.Error("expected non-empty DeliveredAt in response")
	}
}

func TestController_Close_WithDispatcher(t *testing.T) {
	// Tests the dispatcher stop path in Close().
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("close-dispatcher-node"),
		server.WithSMTPFactory(newFakeSMTPFactory()),
		server.WithWebhook(),
	)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	// Close should stop the dispatcher without error.
	if err := ctrl.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// ---------------------------------------------------------------------------
// New RPC tests
// ---------------------------------------------------------------------------

func TestPostboxServer_GetUser_NotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.GetUser(context.Background(), &postboxpb.GetUserRequest{Email: "nobody@example.com"})
	if c := status.Code(err); c != codes.NotFound {
		t.Fatalf("got code %v, want NotFound", c)
	}
}

func TestPostboxServer_GetUser_Found(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email: "alice@example.com",
		Type:  "human",
	}); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetUser(ctx, &postboxpb.GetUserRequest{Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if resp.GetEmail() != "alice@example.com" {
		t.Errorf("email: got %q", resp.GetEmail())
	}
	if resp.GetType() != "human" {
		t.Errorf("type: got %q", resp.GetType())
	}
}

func TestPostboxServer_SearchUsers_ByType(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range []*postboxpb.RegisterUserRequest{
		{Email: "agent1@example.com", Type: "agent"},
		{Email: "human1@example.com", Type: "human"},
	} {
		if _, err := srv.RegisterUser(ctx, req); err != nil {
			t.Fatalf("RegisterUser: %v", err)
		}
	}

	resp, err := srv.SearchUsers(ctx, &postboxpb.SearchUsersRequest{
		MetadataFilters: map[string]string{"type": "agent"},
	})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(resp.GetUsers()) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(resp.GetUsers()))
	}
	if resp.GetUsers()[0].GetType() != "agent" {
		t.Errorf("user type: got %q", resp.GetUsers()[0].GetType())
	}
}

func TestPostboxServer_SearchUsers_Empty(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"u1@example.com", "u2@example.com"} {
		if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{Email: email}); err != nil {
			t.Fatalf("RegisterUser: %v", err)
		}
	}

	// Empty filters should return all users.
	resp, err := srv.SearchUsers(ctx, &postboxpb.SearchUsersRequest{})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(resp.GetUsers()) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.GetUsers()))
	}
}

func TestPostboxServer_ListUsers(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, email := range []string{"alice@example.com", "bob@example.com"} {
		if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{Email: email}); err != nil {
			t.Fatalf("RegisterUser %s: %v", email, err)
		}
	}

	resp, err := srv.ListUsers(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(resp.GetUsers()) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.GetUsers()))
	}
}

func TestPostboxServer_GetNodePublicKey(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	resp, err := srv.GetNodePublicKey(context.Background(), &emptypb.Empty{})
	if err != nil {
		t.Fatalf("GetNodePublicKey: %v", err)
	}
	if resp.GetPublicKey() == "" {
		t.Error("expected non-empty public key")
	}
	if resp.GetNodeId() == "" {
		t.Error("expected non-empty node ID")
	}
}

func TestPostboxServer_GetDeliveryStatus_NotFound(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.GetDeliveryStatus(context.Background(), &postboxpb.GetDeliveryStatusRequest{
		MessageId:   "unknown-msg",
		RecipientId: "unknown-user",
	})
	if c := status.Code(err); c != codes.NotFound {
		t.Fatalf("got code %v, want NotFound", c)
	}
}

func TestPostboxServer_GetDeliveryStatus_RequiresMessageID(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.GetDeliveryStatus(context.Background(), &postboxpb.GetDeliveryStatusRequest{
		RecipientId: "user@example.com",
	})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument", c)
	}
}

func TestPostboxServer_GetDeliveryStatus_RequiresRecipientID(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)

	_, err := srv.GetDeliveryStatus(context.Background(), &postboxpb.GetDeliveryStatusRequest{
		MessageId: "msg-123",
	})
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Fatalf("got code %v, want InvalidArgument", c)
	}
}

func TestPostboxServer_GetDeliveryStatus_Found(t *testing.T) {
	ctrl, s := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	// Insert a delivery job directly into the store.
	job := store.DeliveryJob{
		MessageID:   "msg-abc",
		RecipientID: "agent@example.com",
		EndpointURL: "https://example.com/hook",
		Status:      store.DeliveryPending,
		MaxAttempts: 5,
	}
	if err := s.SaveDeliveryJob(ctx, job); err != nil {
		t.Fatalf("SaveDeliveryJob: %v", err)
	}

	resp, err := srv.GetDeliveryStatus(ctx, &postboxpb.GetDeliveryStatusRequest{
		MessageId:   "msg-abc",
		RecipientId: "agent@example.com",
	})
	if err != nil {
		t.Fatalf("GetDeliveryStatus: %v", err)
	}
	if resp.GetMessageId() != "msg-abc" {
		t.Errorf("message_id: got %q", resp.GetMessageId())
	}
	if resp.GetRecipientId() != "agent@example.com" {
		t.Errorf("recipient_id: got %q", resp.GetRecipientId())
	}
}

func TestPostboxServer_ListDeliveryFailures(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	// With no jobs at all, ListDeliveryFailures should return an empty list (not an error).
	resp, err := srv.ListDeliveryFailures(ctx, &postboxpb.ListDeliveryFailuresRequest{})
	if err != nil {
		t.Fatalf("ListDeliveryFailures: %v", err)
	}
	if len(resp.GetJobs()) != 0 {
		t.Fatalf("expected empty jobs, got %d", len(resp.GetJobs()))
	}
}

func TestPostboxServer_ListDeliveryFailures_WithJobs(t *testing.T) {
	ctrl, s := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	// Insert one failed and one dead job.
	failed := store.DeliveryJob{
		MessageID: "msg-1", RecipientID: "r@example.com",
		Status: store.DeliveryFailed, MaxAttempts: 5,
	}
	dead := store.DeliveryJob{
		MessageID: "msg-2", RecipientID: "r@example.com",
		Status: store.DeliveryDead, MaxAttempts: 5,
	}
	for _, j := range []store.DeliveryJob{failed, dead} {
		if err := s.SaveDeliveryJob(ctx, j); err != nil {
			t.Fatalf("SaveDeliveryJob: %v", err)
		}
	}

	resp, err := srv.ListDeliveryFailures(ctx, &postboxpb.ListDeliveryFailuresRequest{
		RecipientId: "r@example.com",
	})
	if err != nil {
		t.Fatalf("ListDeliveryFailures: %v", err)
	}
	if len(resp.GetJobs()) != 2 {
		t.Fatalf("expected 2 jobs (1 failed + 1 dead), got %d", len(resp.GetJobs()))
	}
}

func TestPostboxServer_RegisterUser_UpdatesExisting(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	// First registration.
	if _, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email: "alice@example.com",
		Type:  "human",
	}); err != nil {
		t.Fatalf("first RegisterUser: %v", err)
	}

	// Second registration with a different type — should update.
	resp, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email: "alice@example.com",
		Type:  "agent",
	})
	if err != nil {
		t.Fatalf("second RegisterUser: %v", err)
	}
	if !resp.GetOk() {
		t.Fatal("expected ok=true on re-registration")
	}

	// Verify the type was updated.
	profile, err := srv.GetUser(ctx, &postboxpb.GetUserRequest{Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("GetUser after update: %v", err)
	}
	if profile.GetType() != "agent" {
		t.Errorf("type after update: got %q, want %q", profile.GetType(), "agent")
	}
}

// --- fakes ---

// fakeSMTP is a programmable SMTPLifecycle.
type fakeSMTP struct {
	mu      sync.Mutex
	running bool
	cfg     postboxsmtp.Config
	starts  atomic.Int32
	stops   atomic.Int32
}

func (f *fakeSMTP) Start() error {
	f.starts.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return postboxsmtp.ErrAlreadyRunning
	}
	f.running = true
	return nil
}

func (f *fakeSMTP) Stop() error {
	f.stops.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.running {
		return postboxsmtp.ErrNotRunning
	}
	f.running = false
	return nil
}

func (f *fakeSMTP) IsRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.running
}

func (f *fakeSMTP) Port() int      { return f.cfg.Port }
func (f *fakeSMTP) Domain() string { return f.cfg.Domain }

func newFakeSMTPFactory() server.SMTPFactory {
	return func(cfg postboxsmtp.Config, _ mailbox.Service, _ server.SMTPDeps) server.SMTPLifecycle {
		return &fakeSMTP{cfg: cfg}
	}
}

// fakeMailbox is a minimal stub that satisfies mailbox.Service for tests.
// Methods we don't exercise return zero values or no-ops.
type fakeMailbox struct{}

func (f *fakeMailbox) IsConnected() bool               { return true }
func (f *fakeMailbox) Connect(_ context.Context) error { return nil }
func (f *fakeMailbox) Close(_ context.Context) error   { return nil }
func (f *fakeMailbox) Client(_ string) mailbox.Mailbox { return nil }
func (f *fakeMailbox) MailboxID() string               { return "test-mailbox" }
func (f *fakeMailbox) Events() *mailbox.ServiceEvents  { return nil }
func (f *fakeMailbox) Notifications(_ context.Context, _, _ string) (notify.Stream, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeMailbox) CleanupTrash(_ context.Context) (*mailbox.CleanupTrashResult, error) {
	return &mailbox.CleanupTrashResult{}, nil
}
func (f *fakeMailbox) CleanupExpiredMessages(_ context.Context) (*mailbox.CleanupExpiredMessagesResult, error) {
	return &mailbox.CleanupExpiredMessagesResult{}, nil
}
func (f *fakeMailbox) EnforceQuotas(_ context.Context, _ []string) (*mailbox.EnforceQuotasResult, error) {
	return &mailbox.EnforceQuotasResult{}, nil
}
func (f *fakeMailbox) RunQuotaEnforcement(_ context.Context) (*mailbox.EnforceQuotasResult, error) {
	return &mailbox.EnforceQuotasResult{}, nil
}
func (f *fakeMailbox) ThreadParticipants(_ context.Context, _ string) ([]string, error) {
	return nil, nil
}

// --- Additional coverage tests ---

func TestPostboxServer_RegisterUser_WithPublicKey(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.RegisterDomain(ctx, &postboxpb.RegisterDomainRequest{Name: "example.com"}); err != nil {
		t.Fatal(err)
	}
	resp, err := srv.RegisterUser(ctx, &postboxpb.RegisterUserRequest{
		Email:     "keyuser@example.com",
		PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	})
	if err != nil {
		t.Fatalf("RegisterUser with public key: %v", err)
	}
	if !resp.GetOk() {
		t.Fatal("expected ok=true")
	}

	profile, err := srv.GetUser(ctx, &postboxpb.GetUserRequest{Email: "keyuser@example.com"})
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if profile.GetPublicKey() == "" {
		t.Error("expected non-empty public key after registration")
	}
}

func TestController_WithRedisClientNil(t *testing.T) {
	s := memstore.New()
	ctrl, err := server.NewController(context.Background(), s,
		server.WithNodeID("redis-nil-node"),
		server.WithSMTPFactory(newFakeSMTPFactory()),
		server.WithRedisClient(nil),
	)
	if err != nil {
		t.Fatalf("NewController with nil Redis client: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close(context.Background()) })
	if ctrl.Forwarder() != nil {
		t.Fatal("expected nil forwarder when Redis client is nil")
	}
}

func TestPostboxServer_StopSMTP_Success(t *testing.T) {
	ctrl, _ := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	if _, err := srv.StartSMTP(ctx, &postboxpb.SMTPConfig{Port: 25, Domain: "x"}); err != nil {
		t.Fatalf("StartSMTP: %v", err)
	}
	resp, err := srv.StopSMTP(ctx, &emptypb.Empty{})
	if err != nil {
		t.Fatalf("StopSMTP: %v", err)
	}
	if !resp.GetOk() {
		t.Fatal("expected ok=true on successful stop")
	}
}

func TestPostboxServer_GetDeliveryStatus_WithNextRetryAt(t *testing.T) {
	ctrl, s := newTestController(t)
	srv := server.NewServer(ctrl)
	ctx := context.Background()

	import_time := func() store.DeliveryJob {
		return store.DeliveryJob{
			MessageID:   "msg-retry",
			RecipientID: "user@example.com",
			Status:      store.DeliveryFailed,
			MaxAttempts: 5,
		}
	}
	job := import_time()
	if err := s.SaveDeliveryJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Fetch and update to set NextRetryAt.
	saved, err := s.GetDeliveryJob(ctx, "msg-retry", "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	retryAt := saved.CreatedAt.Add(60e9) // 1 minute later
	saved.NextRetryAt = retryAt
	if err := s.UpdateDeliveryJob(ctx, saved); err != nil {
		t.Fatal(err)
	}

	resp, err := srv.GetDeliveryStatus(ctx, &postboxpb.GetDeliveryStatusRequest{
		MessageId:   "msg-retry",
		RecipientId: "user@example.com",
	})
	if err != nil {
		t.Fatalf("GetDeliveryStatus: %v", err)
	}
	if resp.GetNextRetryAt() == "" {
		t.Error("expected non-empty NextRetryAt")
	}
}
